package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	team := strings.TrimPrefix(r.PathValue("teamPath"), "/")
	if team == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "missing team path")
		return
	}
	if _, err := s.st.Team(team); err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeTeamNotFound, "team not found")
			return
		}
		s.serverError(w, err)
		return
	}
	if err := s.st.DeleteTeam(team); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		Action:       "team.delete",
		ResourceType: "team",
		ResourceID:   team,
		Summary:      "Team deleted",
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	team := strings.TrimSpace(r.PathValue("team"))
	role := strings.TrimSpace(r.PathValue("role"))
	if team == "" || role == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "team and role are required")
		return
	}
	if _, err := s.st.Role(team, role); err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "role not found")
			return
		}
		s.serverError(w, err)
		return
	}
	if err := s.st.DeleteRole(team, role); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		Action:       "role.delete",
		ResourceType: "role",
		ResourceID:   team + "/" + role,
		Summary:      "Role deleted",
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "missing project name")
		return
	}
	if _, err := s.st.Project(project); err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}
	// Destroy physical artifacts BEFORE touching any control-plane record: a
	// deletion that reports success must not leave project code, worktrees, or
	// preview containers on disk (the p16 soak found ~200MB of worktrees plus
	// historical orphans surviving a 200 response). destroyProjectArtifacts is
	// purely physical — it never mutates the DB — so when it fails the project
	// record and all agent metadata survive intact with no restore dance, and
	// the caller can retry or inspect instead of losing the only pointer to
	// orphaned data.
	if err := s.destroyProjectArtifacts(project); err != nil {
		log.Printf("[project:delete] %s: physical teardown failed, keeping records: %v", project, err)
		s.jsonErrorCode(w, http.StatusInternalServerError, ErrCodeConflict, fmt.Sprintf("project teardown failed, records kept: %v", err))
		return
	}
	workspaceID, _ := s.currentWorkspaceID()
	if s.controlDB != nil {
		if workspaceID != "" {
			if err := s.controlDB.DeleteProjectMembershipsByProject(workspaceID, project); err != nil {
				log.Printf("[project:delete] failed to delete memberships for project %s: %v", project, err)
				s.serverError(w, err)
				return
			}
		}
		if err := s.controlDB.DeleteProjectChannelLinks(workspaceID, project); err != nil {
			log.Printf("[project:delete] failed to delete channel links for project %s: %v", project, err)
			s.serverError(w, err)
			return
		}
		if err := s.controlDB.DeleteAgentChannelBindingsByProject(workspaceID, project); err != nil {
			log.Printf("[project:delete] failed to delete agent channel bindings for project %s: %v", project, err)
			s.serverError(w, err)
			return
		}
	}

	if err := s.st.DeleteProject(project); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		Action:       "project.delete",
		ResourceType: "project",
		ResourceID:   project,
		Summary:      "Project deleted",
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// destroyProjectArtifacts tears down everything the project put on disk or in
// Docker before any control-plane record disappears: preview containers (via
// label, so stopped/renamed containers are caught too), per-task worktrees,
// then the project directory itself. Purely physical — it must never touch
// the DB, so a failure here leaves the project record and all agent metadata
// exactly as they were. Preview container removal is strict: a container that
// survives means the caller must not drop the records that name it.
func (s *Server) destroyProjectArtifacts(project string) error {
	if err := s.stopProjectPreviewContainers(project); err != nil {
		return err
	}
	s.cleanupProjectWorktrees(project)

	projectDir := s.st.ProjectDir(project)
	if dirExists(projectDir) {
		cmd := exec.Command("chmod", "-R", "u+rwX", projectDir)
		_ = cmd.Run()
	}
	// Physical removal only: the store-level DeleteProject also drops the kv
	// record (and agent records) before deleting files, which would lose the
	// record before the outcome is known. Physical deletion goes through
	// fsStore.DeleteProject semantics here without any record mutation.
	if err := s.deleteProjectFiles(projectDir); err != nil {
		return err
	}
	if dirExists(projectDir) {
		return fmt.Errorf("project directory %s still exists after deletion (root-owned files?); records kept", projectDir)
	}
	return nil
}

// deleteProjectFiles removes the project directory with the store's removal
// semantics (chmod walk, RemoveAll, rm -rf fallback) without touching any
// record.
func (s *Server) deleteProjectFiles(projectDir string) error {
	if _, err := os.Stat(projectDir); os.IsNotExist(err) {
		return nil
	}
	if err := os.RemoveAll(projectDir); err != nil && !os.IsNotExist(err) {
		cmd := exec.Command("rm", "-rf", projectDir)
		_ = cmd.Run()
		if _, statErr := os.Stat(projectDir); statErr == nil {
			// Root-owned files from sandbox containers are the production
			// failure mode; escalate instead of reporting a clean delete with
			// project bytes still on disk (p16 soak orphan evidence).
			return fmt.Errorf("remove project dir %q failed after rm -rf fallback (root-owned files?)", projectDir)
		}
	}
	return nil
}

// stopProjectPreviewContainers removes every preview container labelled for
// this project. Label-based so containers survive even when the in-memory
// engine map lost them (restart, reaper already dropped the entry). Returns
// an error when a container could not be removed — the caller must then keep
// all records.
func (s *Server) stopProjectPreviewContainers(project string) error {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.multigent.preview.project="+project).Output()
	if err != nil {
		// Listing requires docker; without it nothing is known about
		// leftover containers, so fail rather than silently proceed.
		return fmt.Errorf("list preview containers: %w", err)
	}
	ids := strings.Fields(string(out))
	for _, id := range ids {
		if out, err := exec.Command("docker", "rm", "-f", id).CombinedOutput(); err != nil {
			return fmt.Errorf("remove preview container %s: %w (%s)", id, err, strings.TrimSpace(string(out)))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// Re-check: zero surviving containers is the gate for proceeding.
	if out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.multigent.preview.project="+project).Output(); err != nil {
		return fmt.Errorf("re-check preview containers: %w", err)
	} else if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("preview containers survived removal for project %s; records kept", project)
	}
	return nil
}

// cleanupProjectWorktrees removes each task worktree through the manager (so
// git metadata is pruned) and falls back to a raw delete for orphans whose
// `.git` is a real directory — those are invisible to `git worktree prune`.
func (s *Server) cleanupProjectWorktrees(project string) {
	gitRoot := s.resolveProjectGitRoot(project)
	wtRoot := filepath.Join(gitRoot, ".multigent", "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		return // no worktrees dir: nothing to clean
	}
	for _, e := range entries {
		taskID := e.Name()
		if s.worktreeMgr != nil {
			if err := s.worktreeMgr.CleanupWorktree(gitRoot, taskID); err != nil {
				log.Printf("[project:delete] %s: worktree cleanup for %s: %v", project, taskID, err)
				continue
			}
		} else {
			_ = os.RemoveAll(filepath.Join(wtRoot, taskID))
		}
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// handleProjectOrphans reports project directories on disk that no longer
// correspond to a project record. These accumulate when historical deletes
// failed silently (root-owned files defeated RemoveAll before the gate
// existed) or were interrupted. Deletion of the orphans is a separate
// explicit admin action, never automatic: the scan only names them.
func (s *Server) handleProjectOrphans(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	orphans, err := s.scanOrphanProjectDirs()
	if err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"orphans": orphans})
}

// handleProjectOrphansDelete removes the named orphan directory after
// re-verifying it still matches no project record. Name (not path) is the
// input to keep the operation rooted at the projects dir.
func (s *Server) handleProjectOrphansDelete(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "missing project name")
		return
	}
	if err := validateWorkspaceObjectName("project", name); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}
	if _, err := s.st.Project(name); err == nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "project record exists; use project deletion instead")
		return
	} else if !isNotFoundErr(err) {
		s.serverError(w, err)
		return
	}
	dir := s.st.ProjectDir(name)
	if !dirExists(dir) {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "no such directory")
		return
	}
	// Orphans commonly contain sandbox-written root-owned files (exactly why
	// the historical deletes failed); repair ownership the same way task
	// worktree cleanup does, then remove.
	_ = exec.Command("chmod", "-R", "u+rwX", dir).Run()
	if err := os.RemoveAll(dir); err != nil {
		_ = exec.Command("rm", "-rf", dir).Run()
	}
	if dirExists(dir) {
		// Truthful failure mode: files written by root inside the sandbox
		// survive RemoveAll and rm -rf for a non-privileged service process.
		// Removing them needs elevated permissions on the host — say so
		// instead of claiming the service can fix ownership. Name only: no
		// absolute host paths in API responses.
		s.serverError(w, fmt.Errorf("orphan %s could not be fully removed (root-owned files?); remove manually with elevated permissions on the host", name))
		return
	}
	s.auditLog(auditLogInput{
		Action:       "project.orphans_delete",
		ResourceType: "project",
		ResourceID:   name,
		Summary:      "Orphan project directory removed",
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// scanOrphanProjectDirs lists subdirectories of the projects root that have
// no project record. A directory counts as an orphan only when it holds no
// project.yaml (historical deletes always removed the record first) — the
// flag distinguishes "record missing" from "record present but file gone",
// both of which make the directory unreachable through the API.
func (s *Server) scanOrphanProjectDirs() ([]map[string]any, error) {
	projectsRoot := s.st.ProjectDir(".")
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var orphans []map[string]any
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, err := s.st.Project(name); err == nil {
			continue // live project
		}
		dir := filepath.Join(projectsRoot, name)
		if _, statErr := os.Stat(dir); statErr != nil {
			continue
		}
		orphans = append(orphans, map[string]any{
			// Name/size/state only — never absolute host paths in API responses.
			"name": name,
			"hasProjectYaml": func() bool {
				_, err := os.Stat(filepath.Join(dir, "project.yaml"))
				return err == nil
			}(),
			"sizeBytes": dirSize(dir),
		})
	}
	return orphans, nil
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
