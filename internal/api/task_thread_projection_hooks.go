package api

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/imbridge"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func (s *Server) notifyTaskThreadStarted(workspaceID, project string, t *entity.Task, workflowID string) {
	if s == nil || s.threadProjections == nil || t == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[task-thread-proj] panic in notifyTaskThreadStarted: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var channelID string
		if s.st != nil {
			if p, err := s.st.Project(project); err == nil && p != nil {
				channelID = p.DefaultIMChannelID
			}
		}

		_, err := s.threadProjections.EnsureTaskRootPost(ctx, imbridge.TaskRootPostRequest{
			WorkspaceID: workspaceID,
			ProjectID:   project,
			TaskID:      t.ID,
			TaskTitle:   t.Title,
			TaskSummary: t.Summary,
			PipelineID:  workflowID,
			CreatedBy:   t.CreatedBy,
			ChannelID:   channelID,
		})
		if err != nil {
			log.Printf("[task-thread-proj] ensure root post failed for %s/%s: %v", project, t.ID, err)
			return
		}

		// If initial workflow step is human_review, post human review card immediately
		if s.controlDB != nil {
			wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
			if run, ok, err := wfStore.RunForTask(project, t.ID); err == nil && ok {
				if def, ok, err := wfStore.RunDefinition(run); err == nil && ok && len(def.Steps) > 0 {
					firstStep := def.Steps[0]
					if strings.TrimSpace(firstStep.Type) == "human_review" {
						preview, err := wfStore.GetReviewResolutionPreview(project, t, firstStep.ID)
						if err == nil {
							_, postErr := s.threadProjections.PostHumanReviewCard(ctx, imbridge.HumanReviewPostRequest{
								WorkspaceID: workspaceID,
								ProjectID:   project,
								TaskID:      t.ID,
								StepID:      firstStep.ID,
								StepTitle:   firstStep.Title,
								Assignee:    firstStep.Title,
								Preview:     preview,
							})
							if postErr != nil {
								log.Printf("[task-thread-proj] post initial human review card failed for %s/%s: %v", project, t.ID, postErr)
							}
						}
					}
				}
			}
		}
	}()
}

func (s *Server) notifyTaskThreadStepTransition(workspaceID, project string, t *entity.Task, transition workflowstore.TransitionResult, outputs map[string]string) {
	if s == nil || s.threadProjections == nil || t == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[task-thread-proj] panic in notifyTaskThreadStepTransition: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var nextAssignee string
		if transition.Next != nil {
			nextAssignee = transition.Next.Title
		}

		summary := strings.TrimSpace(transition.Current.Summary)
		if summary == "" && t != nil {
			summary = strings.TrimSpace(t.Summary)
		}

		stepTitle := transition.Current.StepID
		if transition.Next != nil && transition.Next.ID == transition.Current.StepID {
			stepTitle = transition.Next.Title
		}

		_ = s.threadProjections.PostStepTransition(ctx, imbridge.StepTransitionPostRequest{
			WorkspaceID:  workspaceID,
			ProjectID:    project,
			TaskID:       t.ID,
			StepID:       transition.Current.StepID,
			StepTitle:    stepTitle,
			StepStatus:   transition.Current.Status,
			Assignee:     transition.Current.ActorID,
			NextAssignee: nextAssignee,
			Summary:      summary,
			Outputs:      outputs,
		})

		if transition.Done {
			finalSummary := summary
			if t != nil && strings.TrimSpace(t.Summary) != "" {
				finalSummary = t.Summary
			}
			_ = s.threadProjections.CloseTaskThread(ctx, workspaceID, project, t.ID, finalSummary)
		} else if transition.Next != nil && strings.TrimSpace(transition.Next.Type) == "human_review" {
			// Human review gate reached! Dispatch interactive review card to thread.
			if s.controlDB != nil {
				wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
				preview, err := wfStore.GetReviewResolutionPreview(project, t, transition.Next.ID)
				if err == nil {
					_, postErr := s.threadProjections.PostHumanReviewCard(ctx, imbridge.HumanReviewPostRequest{
						WorkspaceID: workspaceID,
						ProjectID:   project,
						TaskID:      t.ID,
						StepID:      transition.Next.ID,
						StepTitle:   transition.Next.Title,
						Assignee:    transition.Next.Title,
						Preview:     preview,
					})
					if postErr != nil {
						log.Printf("[task-thread-proj] post human review card error for %s/%s: %v", project, t.ID, postErr)
					}
				} else {
					log.Printf("[task-thread-proj] failed to get review preview for %s/%s step %s: %v", project, t.ID, transition.Next.ID, err)
				}
			}
		}
	}()
}
