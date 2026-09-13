package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/imbridge"
)

// Q0 PR-3: infra failure backoff, cap, human-owner notification, and the
// explicit unblock gate. Plan D4: counting relies ONLY on server-controlled
// infra error codes; agent self-reported business markers do not exist and
// would hand the counter to the counted party anyway.

// runtimeInfraFailureBackoff is how long a task waits after infra failure 1-2
// before the scheduler may dispatch it again.
const runtimeInfraFailureBackoff = 5 * time.Minute

// runtimeInfraFailureBlockedThreshold is the consecutive-infra-failure count
// at which the task is parked as blocked pending human unblock.
const runtimeInfraFailureBlockedThreshold = 3

// runtimeInfraErrorCodes is the CLOSED set of server-controlled error codes
// that count as infrastructure failures (GPT fix 6: only PROVABLY
// platform-side infra codes advance the backoff). agent_run_failed is
// deliberately excluded — an agent-level run failure may be a business
// outcome, and the counted party must never hold the counter. Workflow step
// failures (workflow_step_not_completed etc.) are likewise excluded — they go
// through the workflow rework path. agent_prepare_failed stays in the set: it
// is emitted by the runtime node's own prepare stage (cmd/multigent
// runtime_node.go) before any agent business logic runs, so it is provably
// platform-side.
var runtimeInfraErrorCodes = map[string]struct{}{
	"spec_fetch_failed":        {},
	"workspace_prepare_failed": {},
	"agent_prepare_failed":     {},
	"executor_failed":          {},
	"lease_expired":            {},
}

// isRuntimeInfraFailureCode reports whether code is a server-controlled infra
// failure that advances the task's failure streak.
func isRuntimeInfraFailureCode(code string) bool {
	_, ok := runtimeInfraErrorCodes[strings.ToLower(strings.TrimSpace(code))]
	return ok
}

// applyInfraFailureBackoff is the standalone (non-fenced) entry point, kept
// for callers that transition a task outside the runtime fence. It mutates in
// place and persists via PersistTask.
func (s *Server) applyInfraFailureBackoff(workspaceID, project, agent string, task *entity.Task, errorCode string) {
	if !applyInfraFailureBackoffMutation(task, errorCode) {
		return
	}
	if s == nil || s.ts == nil {
		return
	}
	persistErr := s.ts.PersistTask(project, agent, task)
	if task.Status == entity.TaskStatusBlocked {
		if persistErr != nil {
			slog.Warn("infra failure cap: persisting blocked task failed", "task", task.ID, "error", persistErr)
		}
		s.notifyTaskHumanOwners(workspaceID, project, agent, task, errorCode, task.InfraFailureStreak)
		return
	}
	if persistErr != nil {
		slog.Warn("infra failure backoff: persisting task failed", "task", task.ID, "error", persistErr)
	}
}

// notifyInfraBlockedOwner drives the human-owner notification for a task the
// fenced transition just parked in blocked (Q0 PR-3: blocked is never
// silent — comment/IM/audit side effects). Called AFTER the fence released:
// notification is not task state and must not hold the token mutex.
func (s *Server) notifyInfraBlockedOwner(workspaceID, project, agent, taskID, errorCode string) {
	if s == nil || s.ts == nil {
		return
	}
	task, err := s.ts.GetTask(project, agent, taskID)
	if err != nil || task == nil || !taskBlockedByInfraFailures(task) {
		return
	}
	s.notifyTaskHumanOwners(workspaceID, project, agent, task, errorCode, task.InfraFailureStreak)
}

// applyInfraFailureBackoffMutation is the pure in-memory backoff decision used
// inside the fenced transition (GPT 收口 2/4d): streak+1 < 3 → pending with
// NotBefore = now+5m; streak+1 == 3 → blocked. It writes NOTHING — persisting
// is the fenced helper's job, so a persist failure there leaves the task
// byte-identical for the retrying pass. Reports whether the task state
// changed. Terminal done_failed transitions are NOT overridden — infra
// counting only applies when the task is being returned to pending/blocked.
func applyInfraFailureBackoffMutation(task *entity.Task, errorCode string) bool {
	if task == nil || !isRuntimeInfraFailureCode(errorCode) {
		return false
	}
	streak := task.InfraFailureStreak + 1
	task.InfraFailureStreak = streak
	now := time.Now().UTC()
	task.UpdatedAt = now
	prev := task.Status
	if streak >= runtimeInfraFailureBlockedThreshold {
		task.Status = entity.TaskStatusBlocked
		task.NotBefore = nil
	} else {
		// Backoff: return the task to pending with a future NotBefore so the
		// scheduler skips it until the window passes (it stays visible).
		notBefore := now.Add(runtimeInfraFailureBackoff)
		task.Status = entity.TaskStatusPending
		task.NotBefore = &notBefore
	}
	entity.ApplyStatusTimestamps(task, prev, now)
	return true
}

// resetInfraFailureStreak clears the consecutive-failure counter on success
// (or any non-infra terminal outcome such as cancellation).
func resetInfraFailureStreak(task *entity.Task) {
	if task != nil {
		task.InfraFailureStreak = 0
	}
}

// taskBlockedByInfraFailures reports whether the task is parked in blocked
// status and must be skipped by every dispatch path until unblocked.
func taskBlockedByInfraFailures(task *entity.Task) bool {
	return task != nil && task.Status == entity.TaskStatusBlocked
}

// unblockInfraBlockedTask returns a task to pending with the streak cleared.
// Only the explicit unblock endpoint (RBAC-gated, audited) may call this —
// editing the task has no side effect on the streak by design.
func unblockInfraBlockedTask(task *entity.Task, now time.Time) {
	if task == nil {
		return
	}
	prev := task.Status
	task.Status = entity.TaskStatusPending
	task.InfraFailureStreak = 0
	task.NotBefore = nil
	task.UpdatedAt = now
	entity.ApplyStatusTimestamps(task, prev, now)
}

// notifyTaskHumanOwners resolves the human owners of a task (creator if human
// ∪ project managers) and notifies each one through their mapped identity on
// the project's IM instance; a recipient without a mapping degrades to a task
// comment mention, and when nobody can be reached the audit trail carries the
// notification. Agent attention and workspace-admin broadcasts are
// explicitly NOT used (GPT v3 ruling 5).
func (s *Server) notifyTaskHumanOwners(workspaceID, project, agent string, task *entity.Task, errorCode string, streak int) {
	if s == nil || task == nil {
		return
	}
	recipients := s.taskHumanOwnerUsernames(workspaceID, project, task)
	if len(recipients) == 0 {
		// No human recipient resolvable (creator was an agent and the project
		// has no manager users). Comment + audit so this is never silent —
		// but do NOT broaden to workspace admins.
		s.addTaskOwnerNotificationComment(project, agent, task, recipients, errorCode, streak)
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			Action:       "task.blocked",
			ResourceType: "task",
			ResourceID:   task.ID,
			Summary:      fmt.Sprintf("Task blocked after %d consecutive infra failures (last error: %s); no human recipient resolved", streak, errorCode),
			After: map[string]any{
				"project":       project,
				"agent":         agent,
				"taskId":        task.ID,
				"errorCode":     errorCode,
				"streak":        streak,
				"recipientDropped": true,
			},
		})
		return
	}
	delivered := s.deliverTaskOwnerNotifications(workspaceID, project, agent, task, recipients, errorCode, streak)
	commented := s.addTaskOwnerNotificationComment(project, agent, task, recipientsNot(delivered, recipients), errorCode, streak)
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "task.blocked",
		ResourceType: "task",
		ResourceID:   task.ID,
		Summary:      fmt.Sprintf("Task blocked after %d consecutive infra failures (last error: %s); notified %d human owner(s)", streak, errorCode, len(delivered)),
		After: map[string]any{
			"project":          project,
			"agent":            agent,
			"taskId":           task.ID,
			"errorCode":        errorCode,
			"streak":           streak,
			"notified":         delivered,
			"degradedToComment": commented,
		},
	})
}

// taskHumanOwnerUsernames returns the union of the task creator (only when it
// is a human username) and the project's manager users, deduplicated.
func (s *Server) taskHumanOwnerUsernames(workspaceID, project string, task *entity.Task) []string {
	seen := map[string]struct{}{}
	out := []string{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if task != nil {
		creator := strings.TrimSpace(task.CreatedBy)
		// Agent/heartbeat creators are not humans ("agent:...", "heartbeat:..."
		// style) — only plain usernames count as human creators.
		if creator != "" && !strings.Contains(creator, ":") && s.userExists(creator) {
			add(creator)
		}
	}
	for _, manager := range s.projectManagerUsernames(project) {
		add(manager)
	}
	return out
}

func recipientsNot(delivered []string, all []string) []string {
	set := map[string]struct{}{}
	for _, name := range delivered {
		set[name] = struct{}{}
	}
	out := []string{}
	for _, name := range all {
		if _, ok := set[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// deliverTaskOwnerNotifications sends the per-person IM notification to every
// recipient that has a mapped identity on the project's IM instance. It
// returns the usernames actually delivered.
func (s *Server) deliverTaskOwnerNotifications(workspaceID, project, agent string, task *entity.Task, recipients []string, errorCode string, streak int) []string {
	delivered := []string{}
	for _, username := range recipients {
		if s.sendTaskBlockedIM(workspaceID, project, agent, task, username, errorCode, streak) {
			delivered = append(delivered, username)
		}
	}
	return delivered
}

// sendTaskBlockedIM resolves one user's external identity on the project's
// bound IM instance and DMs the blocked notice. Best-effort per person.
func (s *Server) sendTaskBlockedIM(workspaceID, project, agent string, task *entity.Task, username, errorCode string, streak int) bool {
	message := taskBlockedNoticeText(project, agent, task, errorCode, streak)
	if s.sendUserIMDirectMessage(workspaceID, project, username, message) {
		return true
	}
	taskID := ""
	if task != nil {
		taskID = task.ID
	}
	slog.Warn("task blocked IM delivery unavailable; degrading to comment", "task", taskID, "user", username)
	return false
}

// addTaskOwnerNotificationComment posts the blocked notice as a task comment
// mentioning the recipients that could not be reached over IM. No recipients
// at all → plain notice, still commented and audited by the caller.
func (s *Server) addTaskOwnerNotificationComment(project, agent string, task *entity.Task, mention []string, errorCode string, streak int) bool {
	if s == nil || s.ts == nil || task == nil {
		return false
	}
	text := taskBlockedNoticeText(project, agent, task, errorCode, streak)
	if len(mention) > 0 {
		text = "@" + strings.Join(mention, " @") + "\n" + text
	}
	return s.addTaskSystemComment(project, agent, task, "task blocked after repeated infra failures", text)
}

func taskBlockedNoticeText(project, agent string, task *entity.Task, errorCode string, streak int) string {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = task.ID
	}
	lastError := strings.TrimSpace(task.LastError)
	var b strings.Builder
	fmt.Fprintf(&b, "任务已被暂停（blocked）：基础设施连续失败 %d 次，最近错误码 %s。", streak, errorCode)
	if lastError != "" {
		fmt.Fprintf(&b, "详情：%s。", lastError)
	}
	fmt.Fprintf(&b, "任务 %s（%s/%s）。请人工检查后，通过任务操作里的\"解除封锁\"（unblock）恢复派发；编辑任务不会解除封锁。", title, project, agent)
	return b.String()
}

// ── helper resolvers ─────────────────────────────────────────────────────────

// userExists reports whether username is a real (human) user.
func (s *Server) userExists(username string) bool {
	if s == nil || s.users == nil {
		return false
	}
	return s.users.GetUser(strings.TrimSpace(username)) != nil
}

// projectManagerUsernames returns the workspace usernames holding a manager
// (or admin) role on the given project — user-type memberships only; agent
// worker memberships are never humans.
func (s *Server) projectManagerUsernames(project string) []string {
	if s == nil || s.users == nil {
		return nil
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return nil
	}
	memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
	})
	if err != nil {
		return nil
	}
	out := []string{}
	for _, m := range memberships {
		if !strings.EqualFold(strings.TrimSpace(m.MemberType), "user") {
			continue
		}
		role := strings.TrimSpace(m.Role)
		if projectRoleLevel(role) >= projectRoleLevel(ProjectRoleManager) {
			out = append(out, strings.TrimSpace(m.MemberID))
		}
	}
	return out
}

// sendUserIMDirectMessage delivers one DM through the project's connected
// human-collaboration channel binding, resolving the recipient's mapped
// external identity STRICTLY within that binding (GPT fix 5): with two
// Mattermost instances connected, an identity from the other instance must
// never be picked. Filtering on the binding ID (which pins connectionId and
// imInstanceId) makes the lookup instance-exact; a user without an identity
// under this binding returns false and the caller degrades to a task comment.
func (s *Server) sendUserIMDirectMessage(workspaceID, project, username, message string) bool {
	if s == nil || s.controlDB == nil || strings.TrimSpace(username) == "" || strings.TrimSpace(message) == "" {
		return false
	}
	binding, found, err := s.projectHumanChannelBinding(workspaceID, project)
	if err != nil || !found {
		return false
	}
	identities, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:      workspaceID,
		UserID:           username,
		Provider:         binding.Provider,
		ChannelBindingID: binding.ID,
	})
	if err != nil || len(identities) == 0 {
		return false
	}
	identity := identities[0]
	target := imbridge.OutgoingTarget{}
	if externalID := strings.TrimSpace(identity.ExternalUserID); externalID != "" {
		target = imbridge.OutgoingTarget{ReceiveID: externalID, ReceiveIDType: "open_id", ChatID: strings.TrimSpace(identity.ExternalChatID)}
	} else if chatID := strings.TrimSpace(identity.ExternalChatID); chatID != "" {
		target = imbridge.OutgoingTarget{ReceiveID: chatID, ReceiveIDType: "chat_id", ChatID: chatID}
	} else {
		return false
	}
	provider, ok := imbridge.LookupProvider(binding.Provider)
	if !ok {
		return false
	}
	secret, ok, err := s.controlDB.ConnectionSecret(binding.ConnectionID)
	if err != nil || !ok {
		return false
	}
	secrets, err := openConnectionSecret(secret)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	msg := imbridge.OutgoingMessage{Text: trimForIM(message, 3500), Format: "markdown"}
	if err := provider.SendMessage(ctx, secrets, target, msg); err != nil {
		slog.Warn("task blocked IM delivery failed", "user", username, "error", err)
		return false
	}
	return true
}

// projectHumanChannelBinding finds the project agent's connected
// human-collaboration channel binding (agent_channel_events-style), i.e. an
// active binding whose provider is a real IM platform.
func (s *Server) projectHumanChannelBinding(workspaceID, project string) (controldb.AgentChannelBinding, bool, error) {
	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
	})
	if err != nil {
		return controldb.AgentChannelBinding{}, false, err
	}
	for _, b := range bindings {
		if strings.TrimSpace(b.Provider) == "" {
			continue
		}
		if _, ok := imbridge.LookupProvider(b.Provider); !ok {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(b.Status))
		if status != "" && status != "active" && status != "connected" {
			continue
		}
		return b, true, nil
	}
	return controldb.AgentChannelBinding{}, false, nil
}

// addTaskSystemComment posts a system-authored comment on the task.
func (s *Server) addTaskSystemComment(project, agent string, task *entity.Task, subject, body string) bool {
	if s == nil || s.ts == nil || task == nil {
		return false
	}
	text := strings.TrimSpace(body)
	if subject != "" {
		text = subject + "\n" + text
	}
	return s.ts.AddComment(project, agent, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    task.ID,
		Author:    "system",
		Body:      text,
		CreatedAt: time.Now().UTC(),
	}) == nil
}
