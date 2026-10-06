// Transient credential injection for the delivery-evidence gate (defect ⑯,
// final form — fu91es QA, 2026-10-06).
//
// The gate's ls-remote runs in the runner process. The run's materialized
// git config does not reliably carry host-usable credentials there: the
// helper's host list is built from the connection's base URL spellings and
// can miss the origin URL's host alias, and [user]-only configs carry no
// credentials at all — every such run failed closed with "could not read
// Username" even though the agent had genuinely pushed (the container push
// path injects credentials transiently via the connections manifest).
//
// The fix mirrors the API-side gate (remote_binding_service
// deliveryEvidenceEnv): resolve the GitLab connection whose secret base URL
// host MATCHES the worktree origin's host, decrypt its token, and inject an
// http.extraHeader for this one read-only command. The host match IS the
// exfiltration guard — a worktree pointing at a foreign origin never
// receives a credential, exactly like the API-side rule. Nothing is written
// to disk, repo config, or the remote URL.
package runner

import (
	"encoding/base64"
	"net/url"
	"os/exec"
	"strings"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// evidenceEnvForTask resolves the transient credential env for one gate run:
// the GitLab connection whose secret base-URL host equals the task
// worktree's origin host. nil = no match / no usable connection / no origin
// — the gate then behaves exactly as before and fails closed.
func (r *Runner) evidenceEnvForTask(task *entity.Task) []string {
	if r == nil || task == nil {
		return nil
	}
	dir := strings.TrimSpace(task.WorktreeDir)
	if dir == "" {
		return nil
	}
	originHost := gitOriginHost(dir)
	if originHost == "" {
		// ssh/local origins: the platform's connection model is HTTP(S);
		// injecting an HTTP header would do nothing — keep fail-closed.
		return nil
	}
	controlDB, err := controldb.OpenDefault()
	if err != nil {
		return nil
	}
	defer func() { _ = controlDB.Close() }()
	connections, err := controlDB.ListConnections(controldb.ConnectionFilter{Provider: "gitlab"})
	if err != nil {
		return nil
	}
	resolver, closeResolver := newRuntimeConnectionSecretResolver()
	if resolver == nil {
		return nil
	}
	defer closeResolver()
	for _, conn := range connections {
		if conn.Status != "" && conn.Status != "active" {
			continue
		}
		values, ok, err := resolver(conn.ID)
		if err != nil || !ok {
			continue
		}
		token := firstNonEmpty(values["apiKey"], values["accessToken"], values["token"])
		if token == "" {
			continue
		}
		baseHost := connectionBaseHost(values["baseUrl"])
		if baseHost == "" || !sameGitHostPort(baseHost, originHost) {
			// The exfiltration guard: this connection's credential is
			// scoped to its own host; a foreign origin gets nothing.
			continue
		}
		return gitlabHeaderEvidenceEnv(token)
	}
	return nil
}

// gitlabHeaderEvidenceEnv builds the transient env: an Authorization header
// injected as git config via the GIT_CONFIG_* environment mechanism (the
// same shape the API-side clone/evidence path uses). Nothing touches disk.
func gitlabHeaderEvidenceEnv(token string) []string {
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

// gitOriginHost returns the host[:port] of the worktree's origin remote for
// http(s) URLs. Empty for ssh/scp-style, file://, local paths, or errors —
// the injection only applies to HTTP(S) remotes.
func gitOriginHost(worktreeDir string) string {
	out, err := exec.Command("git", "-C", worktreeDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.Host
}

// connectionBaseHost extracts host[:port] from a connection's base URL.
func connectionBaseHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// sameGitHostPort compares host:port pairs case-insensitively. Both sides
// come from url.Parse of full URLs, so the port is present exactly when the
// URL spelled one out — the same spelling on both sides matches, and a
// mismatched spelling fails closed (no credential) rather than guessing.
func sameGitHostPort(a, b string) bool {
	return strings.ToLower(a) == strings.ToLower(b)
}
