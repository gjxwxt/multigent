// In-clone verification (round-18 P0-3): the design contract (§4.1 v6,
// unique ordering) requires the clone to pass ALL verification — including
// source-writing commands — AFTER clone-apply and BEFORE the main-worktree
// apply. The engine accepts a Validator hook; the API layer wires a
// container-executed validator so project-provided commands never run on
// the host (security red line).
package changerun

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// VerificationResult is one command's outcome inside the clone.
type VerificationResult struct {
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	Output   string `json:"output,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// Validator executes verification commands against a directory (the
// validation clone). Implementations MUST NOT run project argv on the host —
// the API layer's implementation shells into a disposable container.
type Validator interface {
	// VerifyDir runs the project's verification suite in dir. Returning an
	// error fails the apply (proposal parks in verification_failed; worktree
	// untouched). The returned map lands on the proposal's Verification
	// record (stage "clone_verify").
	VerifyDir(ctx context.Context, dir string) ([]VerificationResult, error)
}

// ValidatorFunc adapts a function to Validator.
type ValidatorFunc func(ctx context.Context, dir string) ([]VerificationResult, error)

// VerifyDir implements Validator.
func (f ValidatorFunc) VerifyDir(ctx context.Context, dir string) ([]VerificationResult, error) {
	return f(ctx, dir)
}

// ContainerVerifyOptions configures ContainerValidator.
type ContainerVerifyOptions struct {
	// Commands are the shell lines to run inside the container, in order.
	// First failure stops the sequence. Empty = no verification (V1 projects
	// without a declared suite still record an explicit "none" result so the
	// trail distinguishes "not run" from "passed").
	Commands []string
	// Image is the container image; empty = no container execution (host
	// mode is FORBIDDEN for project argv, so an empty image with commands
	// set is a configuration error at wire time).
	Image string
	// Timeout per command; default 10 minutes.
	Timeout time.Duration
	// WorkdirRel is the in-container working directory (default /workspace).
	WorkdirRel string
}

// ContainerValidator runs each command in a disposable container with dir
// mounted at /workspace — the same execution shape as the fixturesandbox
// generator: project argv never touches the host.
type ContainerValidator struct {
	Opts ContainerVerifyOptions
	// RunContainer is the docker-exec seam (injected so tests don't need
	// Docker). It must execute argv inside a container mounting dir.
	RunContainer func(ctx context.Context, dir, image, workdir, argv string, timeout time.Duration) (string, error)
}

// VerifyDir runs the configured commands, stopping at first failure.
func (v *ContainerValidator) VerifyDir(ctx context.Context, dir string) ([]VerificationResult, error) {
	cmds := v.Opts.Commands
	if len(cmds) == 0 {
		return []VerificationResult{{Command: "(none configured)", OK: true}}, nil
	}
	if v.Opts.Image == "" {
		return nil, fmt.Errorf("container verifier configured with commands but no image — refusing host execution of project commands")
	}
	timeout := v.Opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	workdir := v.Opts.WorkdirRel
	if workdir == "" {
		workdir = "/workspace"
	}
	var results []VerificationResult
	for _, argv := range cmds {
		start := time.Now()
		out, err := v.RunContainer(ctx, dir, v.Opts.Image, workdir, argv, timeout)
		res := VerificationResult{
			Command:  argv,
			Output:   tail(out, 4000),
			Duration: time.Since(start).Round(time.Millisecond).String(),
		}
		if err != nil {
			res.OK = false
			results = append(results, res)
			return results, fmt.Errorf("verification command failed: %s — %w", argv, err)
		}
		res.OK = true
		results = append(results, res)
	}
	return results, nil
}

// verificationSummary flattens results into the proposal record map.
func verificationSummary(results []VerificationResult) map[string]string {
	out := map[string]string{}
	for i, r := range results {
		key := fmt.Sprintf("cmd_%d", i)
		status := "pass"
		if !r.OK {
			status = "fail"
		}
		out[key] = status + " " + r.Command
		if r.Output != "" {
			out[key+"_output_tail"] = r.Output
		}
	}
	if len(results) == 0 {
		out["cmd_none"] = "no verification configured"
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
