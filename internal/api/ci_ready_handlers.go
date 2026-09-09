package api

import (
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
)

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
	repo := strings.TrimSpace(project.Repo)
	if repo == "" {
		s.jsonError(w, http.StatusBadRequest, "project has no repository path; run project initialization first")
		return
	}
	report, err := ciready.Ensure(repo)
	if err != nil {
		s.serverError(w, err)
		return
	}
	response := ciReadyResponse{Report: report}

	waitSeconds := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_seconds")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 0 || parsed > 600 {
			s.jsonError(w, http.StatusBadRequest, "wait_seconds must be between 0 and 600")
			return
		}
		waitSeconds = parsed
	}
	if waitSeconds > 0 {
		if project.RemoteProvider != "gitlab" || strings.TrimSpace(project.RemoteProjectID) == "" {
			response.PipelineError = "project is not bound to a GitLab remote; pipeline evidence skipped"
		} else {
			evidence, evidenceErr := s.ciReadyPipelineEvidence(r, project, waitSeconds)
			if evidenceErr != nil {
				response.PipelineError = evidenceErr.Error()
			} else {
				response.Pipeline = evidence
			}
		}
	}
	_ = json.NewEncoder(w).Encode(response)
}

// ciReadyPipelineEvidence polls the GitLab pipeline for the repository HEAD
// until it reaches a terminal state or the bounded wait elapses. Polling
// failures are surfaced as errors, never silently swallowed.
func (s *Server) ciReadyPipelineEvidence(r *http.Request, project *entity.Project, waitSeconds int) (*ciReadyPipelineEvidence, error) {
	gitDir := strings.TrimSpace(project.Repo)
	if gitDir == "" || !dirHasGit(gitDir) {
		if resolved := s.resolveProjectGitRoot(project.Name); resolved != "" && dirHasGit(resolved) {
			gitDir = resolved
		}
	}
	shaOut, err := exec.CommandContext(r.Context(), "git", "-C", gitDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git -C %s rev-parse HEAD: %w", gitDir, err)
	}
	sha := strings.TrimSpace(string(shaOut))
	if sha == "" {
		return nil, errNoCommits
	}
	host, _, err := s.pinnedGitLabHost(r.Context(), project.Name, project)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	var last *ciReadyPipelineEvidence
	for {
		pipelines, listErr := host.PipelinesForSHA(r.Context(), project.RemoteProjectID, sha)
		if listErr != nil {
			return nil, listErr
		}
		if len(pipelines) > 0 {
			pipe := pipelines[0]
			last = &ciReadyPipelineEvidence{SHA: sha, ID: pipe.ID, Status: pipe.Status, WebURL: pipe.WebURL}
			if isPipelineTerminal(pipe.Status) {
				if jobs, jobsErr := host.PipelineJobs(r.Context(), project.RemoteProjectID, pipe.ID); jobsErr == nil {
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
		case <-r.Context().Done():
			if last != nil {
				return last, nil
			}
			return nil, r.Context().Err()
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
