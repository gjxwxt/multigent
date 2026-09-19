package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/ciready"
	"github.com/multigent/multigent/internal/codehost"
	"github.com/multigent/multigent/internal/entity"
)

var (
	errNoCommits        = errors.New("repository has no commits yet; push the initialization commit first")
	errNoPipelineForSHA = errors.New("no pipeline observed for the current HEAD yet; retry with a longer wait")
	errRemoteRequired   = errors.New("remote pipeline required but the project is not bound to a GitLab remote")
)

// ciRemotePipelineRequiredEnv is the server-level DEFAULT for projects that
// do not declare RemotePipelineRequired themselves. Any of "1", "true",
// "yes", "required" (case-insensitive) turns the gate strict workspace-wide.
// It is a default, not a floor: a project may still declare "local" to opt
// out, because there is no organization-enforcement deployment today — the
// name deliberately keeps "REQUIRED" (not "DEFAULT") since the value it
// selects is the strict gate, and if an org-enforced floor is ever needed it
// should be a separate setting that ignores project-level "local".
const ciRemotePipelineRequiredEnv = "MULTIGENT_CI_REMOTE_PIPELINE_REQUIRED"

// ciRemotePipelineRequired resolves the effective gate semantics: explicit
// project declaration first, then the server env default, then local-only
// (the historical behavior). The declared- vs-bound-inference ambiguity
// flagged in review 2026-09-14 is resolved by making both sides explicit.
// Precedence (review round 3, C): project declaration > env default >
// fallback local. The env is a default only — "local" at project level
// always wins until an explicit org-floor setting exists.
func ciRemotePipelineRequired(p *entity.Project) bool {
	if p != nil {
		switch strings.ToLower(strings.TrimSpace(p.RemotePipelineRequired)) {
		case "required":
			return true
		case "local":
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ciRemotePipelineRequiredEnv))) {
	case "1", "true", "yes", "required":
		return true
	}
	return false
}

// ciReadyOptions selects what an evaluation does. seed and gatherEvidence are
// separate on purpose: the init endpoint seeds and only looks for CI when asked
// to wait, while the gate looks for CI on a snapshot and never writes.
type ciReadyOptions struct {
	project        *entity.Project
	seed           bool
	gatherEvidence bool
	waitSeconds    int
}

// ciReadyResponse wraps the deterministic report with optional GitLab
// pipeline evidence collected after the first push.
type ciReadyResponse struct {
	ciready.Report
	Pipeline      *ciReadyPipelineEvidence `json:"pipeline,omitempty"`
	PipelineError string                   `json:"pipelineError,omitempty"`
}

type ciReadyPipelineEvidence struct {
	SHA    string                     `json:"sha"`
	ID     int64                      `json:"id"`
	Status string                     `json:"status"`
	WebURL string                     `json:"webUrl,omitempty"`
	Jobs   []codehost.PipelineJobInfo `json:"jobs,omitempty"`
}

// handleRuntimeCIReady runs the deterministic CI/CD readiness gate against
// the project repository: seed missing CI baseline files, then run every
// check. With wait_seconds > 0 and a bound GitLab remote it additionally
// polls the pipeline built for the current HEAD as delivery evidence.
func (s *Server) handleRuntimeCIReady(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	project, err := s.st.Project(principal.Project)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}
	if strings.TrimSpace(project.Repo) == "" {
		s.jsonError(w, http.StatusBadRequest, "project has no repository path; run project initialization first")
		return
	}
	waitSeconds := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_seconds")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 0 || parsed > 600 {
			s.jsonError(w, http.StatusBadRequest, "wait_seconds must be between 0 and 600")
			return
		}
		waitSeconds = parsed
	}
	// Asking for evidence is what wait_seconds has always meant on this
	// endpoint; the gate path asks separately. This endpoint seeds: it is the
	// documented init-time "补种 + 校验" contract.
	response, err := s.ciReadyEvaluation(r.Context(), ciReadyOptions{
		project:        project,
		gatherEvidence: waitSeconds > 0,
		waitSeconds:    waitSeconds,
		seed:           true,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

// ciReadyEvaluation runs the deterministic CI/CD readiness gate and, when asked,
// attaches the GitLab pipeline built for the repository HEAD as delivery
// evidence.
//
// gatherEvidence=false keeps the historical endpoint behavior for callers that do
// not ask about CI at all. The gate path always gathers it with waitSeconds=0, so
// a bound remote is judged on a snapshot rather than skipped: a report that reads
// "ready" only because nobody looked is the exact failure the p15 canary §8.4
// finding was about.
func (s *Server) ciReadyEvaluation(ctx context.Context, opts ciReadyOptions) (ciReadyResponse, error) {
	project := opts.project
	repo := strings.TrimSpace(project.Repo)
	var report ciready.Report
	if opts.seed {
		seeded, err := ciready.Ensure(repo)
		if err != nil {
			return ciReadyResponse{}, err
		}
		report = seeded
	} else {
		// Judge only. Writing a CI baseline from the gate would put the platform
		// process inside a repository working tree, which is the boundary the
		// brownfield contract materialization step deliberately left to the sandbox.
		report = ciready.Verify(repo)
	}
	response := ciReadyResponse{Report: report}
	if !opts.gatherEvidence {
		return response, nil
	}
	waitSeconds := opts.waitSeconds
	remoteRequired := ciRemotePipelineRequired(project)
	// P0.6: "bound" means a verified remote binding exists — display fields on
	// the project record are client-writable and prove nothing.
	_, _, remoteBound, bindErr := s.verifiedBinding(project.Name)
	if bindErr != nil {
		return ciReadyResponse{}, bindErr
	}
	switch {
	case !remoteBound && !remoteRequired:
		// Local-only delivery is explicitly acceptable: report the skip but keep
		// it out of the checks (nothing failed).
		response.PipelineError = "project is not bound to a GitLab remote; pipeline evidence skipped (local-only semantics)"
	case !remoteBound:
		response.Pipeline = nil
		response.PipelineError = errRemoteRequired.Error()
	default:
		evidence, evidenceErr := s.ciReadyPipelineEvidence(ctx, project, waitSeconds)
		if evidenceErr != nil {
			response.PipelineError = evidenceErr.Error()
		} else {
			response.Pipeline = evidence
		}
	}
	// Fail-closed: once a GitLab remote is bound, the pipeline IS the delivery
	// evidence. 13/13 deterministic checks with no successful pipeline must not
	// read as "ready" — that green masked a permanently pending pipeline when the
	// runner binding was missing (p15 canary §8.4). Under remote-required
	// semantics an UNBOUND remote fails the same way (explicit declaration, not
	// bound-inference); under local-only semantics no check is appended at all.
	localOnlySkip := !remoteBound && !remoteRequired
	if response.Pipeline == nil && !localOnlySkip {
		response.Checks = append(response.Checks, ciready.Check{
			Name:   "pipeline_evidence",
			Status: ciready.StatusFail,
			Detail: response.PipelineError,
		})
		response.Overall = ciready.OverallNotReady
	} else if response.Pipeline != nil && response.Pipeline.Status != "success" {
		response.Checks = append(response.Checks, ciready.Check{
			Name:   "pipeline_evidence",
			Status: ciready.StatusFail,
			Detail: fmt.Sprintf("pipeline %d for HEAD is %s (terminal status required: success)", response.Pipeline.ID, response.Pipeline.Status),
		})
		response.Overall = ciready.OverallNotReady
	} else if response.Pipeline != nil {
		response.Checks = append(response.Checks, ciready.Check{
			Name:   "pipeline_evidence",
			Status: ciready.StatusPass,
			Detail: fmt.Sprintf("pipeline %d succeeded for HEAD %s", response.Pipeline.ID, response.Pipeline.SHA),
		})
	}
	return response, nil
}

// ciReadyPipelineEvidence polls the GitLab pipeline for the repository HEAD
// until it reaches a terminal state or the bounded wait elapses. Polling
// failures are surfaced as errors, never silently swallowed.
func (s *Server) ciReadyPipelineEvidence(ctx context.Context, project *entity.Project, waitSeconds int) (*ciReadyPipelineEvidence, error) {
	gitDir := strings.TrimSpace(project.Repo)
	if gitDir == "" || !dirHasGit(gitDir) {
		if resolved := s.resolveProjectGitRoot(project.Name); resolved != "" && dirHasGit(resolved) {
			gitDir = resolved
		}
	}
	shaOut, err := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git -C %s rev-parse HEAD: %w", gitDir, err)
	}
	sha := strings.TrimSpace(string(shaOut))
	if sha == "" {
		return nil, errNoCommits
	}
	// P0.6: pipeline evidence reads the verified remote binding — an
	// unverified project (or a forged display-field remote id) yields no
	// evidence rather than a lookup against an attacker-chosen project.
	host, binding, err := s.verifiedGitLabHost(ctx, project.Name)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	var last *ciReadyPipelineEvidence
	for {
		pipelines, listErr := host.PipelinesForSHA(ctx, binding.RemoteProjectID, sha)
		if listErr != nil {
			return nil, listErr
		}
		if len(pipelines) > 0 {
			pipe := pipelines[0]
			last = &ciReadyPipelineEvidence{SHA: sha, ID: pipe.ID, Status: pipe.Status, WebURL: pipe.WebURL}
			if isPipelineTerminal(pipe.Status) {
				if jobs, jobsErr := host.PipelineJobs(ctx, binding.RemoteProjectID, pipe.ID); jobsErr == nil {
					last.Jobs = jobs
				}
				return last, nil
			}
		}
		if time.Now().After(deadline) {
			if last != nil {
				return last, nil
			}
			return nil, errNoPipelineForSHA
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return last, nil
			}
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func isPipelineTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case "success", "failed", "canceled", "skipped":
		return true
	default:
		return false
	}
}

func dirHasGit(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
