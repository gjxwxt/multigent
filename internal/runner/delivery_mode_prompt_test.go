package runner

import (
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/deliverymode"
	"github.com/multigent/multigent/internal/entity"
)

// pushDenials are phrasings that would teach an agent to stop pushing. That is
// a worse outcome than the placeholder pr_url this section exists to prevent:
// push authenticates from the environment at the push boundary and never
// consults the verified binding, so a run that skips pushing has destroyed the
// delivery instead of degrading it.
var pushDenials = []string{
	"cannot push", "can not push", "pushing is unavailable",
	"do not push", "no remote to push to", "cannot be pushed",
}

func assertSection(t *testing.T, name, section string, mustContain []string, mustNotContain []string) {
	t.Helper()
	lower := strings.ToLower(section)
	for _, want := range mustContain {
		if !strings.Contains(section, want) {
			t.Fatalf("%s: missing %q in:\n%s", name, want, section)
		}
	}
	for _, bad := range mustNotContain {
		if strings.Contains(lower, strings.ToLower(bad)) {
			t.Fatalf("%s: must not contain %q in:\n%s", name, bad, section)
		}
	}
}

func TestDeliveryModeSectionWording(t *testing.T) {
	bound := deliveryModeSection(deliverymode.ModeBound, "gao/react-components")
	assertSection(t, "bound", bound,
		[]string{"Mode: `remote_bound`", "gao/react-components", "Report the real merge request URL"},
		[]string{"branch:<branch_name>", "NO verified remote binding"})

	local := deliveryModeSection(deliverymode.ModeLocalBranch, "")
	assertSection(t, "local-branch", local,
		[]string{
			"Mode: `local_branch`",
			"Git push and the task branch DO work",
			"do not attempt to create a merge request",
			"branch:<branch_name>",
		},
		append([]string{"Report the real merge request URL"}, pushDenials...))

	required := deliveryModeSection(deliverymode.ModeRequiredUnbound, "")
	assertSection(t, "required-unbound", required,
		[]string{"Mode: `required_unbound`", "cause=environment", "do not substitute placeholder values"},
		append([]string{"branch:<branch_name>", "Report the real merge request URL"}, pushDenials...))

	unknown := deliveryModeSection(deliverymode.ModeUnknown, "")
	assertSection(t, "unknown", unknown,
		[]string{"Mode: `unknown`", "could not determine", "Do not assume either way"},
		// Unknown must not hand out the placeholder as an escape hatch, and must
		// not talk the agent out of pushing either.
		append([]string{"branch:<branch_name>"}, pushDenials...))
}

func TestBuildTaskPromptCarriesVerifiedBindingState(t *testing.T) {
	db, runner, projectName, taskID := setupReviewerTaskEnvironment(t, "en")
	workspaceID := resolveRuntimeWorkspaceID(runner.root, db)
	if err := runner.agentStore.SaveProject(projectName, &entity.Project{
		Name: projectName,
		Repo: t.TempDir(),
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// The display fields are deliberately populated with a lie: a project can
	// claim "gitlab" while holding no verified binding, and the resolver must
	// still report local_branch. This is the exact data source the replaced
	// design read from.
	prompt := runner.BuildTaskPrompt(projectName, "reviewer-agent", &entity.Task{ID: taskID, Prompt: "execute the step"})
	if !strings.Contains(prompt, "Mode: `local_branch`") {
		t.Fatalf("unbound project must render local_branch:\n%s", promptSection(prompt))
	}
	if !strings.Contains(prompt, "Git push and the task branch DO work") {
		t.Fatal("local_branch prompt must affirm that pushing still works")
	}

	if err := db.UpsertVerifiedRemoteBinding(controldb.VerifiedRemoteBinding{
		WorkspaceID:       workspaceID,
		ProjectID:         projectName,
		Provider:          "gitlab",
		ConnectionID:      "conn-gitlab-1",
		RemoteProjectID:   "42",
		PathWithNamespace: "gao/react-components",
		VerifiedAt:        "2026-09-20T00:00:00Z",
		Source:            controldb.BindingSourceExplicitVerify,
	}); err != nil {
		t.Fatalf("upsert verified binding: %v", err)
	}
	boundPrompt := runner.BuildTaskPrompt(projectName, "reviewer-agent", &entity.Task{ID: taskID, Prompt: "execute the step"})
	if !strings.Contains(boundPrompt, "Mode: `remote_bound`") || !strings.Contains(boundPrompt, "gao/react-components") {
		t.Fatalf("verified binding must render remote_bound with the namespace:\n%s", promptSection(boundPrompt))
	}
	if strings.Contains(boundPrompt, "branch:<branch_name>") {
		t.Fatal("a bound project must never be offered the placeholder value")
	}

	if err := db.DeleteVerifiedRemoteBinding(workspaceID, projectName); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if err := runner.agentStore.SaveProject(projectName, &entity.Project{
		Name:                   projectName,
		Repo:                   t.TempDir(),
		RemotePipelineRequired: "required",
	}); err != nil {
		t.Fatalf("save required project: %v", err)
	}
	requiredPrompt := runner.BuildTaskPrompt(projectName, "reviewer-agent", &entity.Task{ID: taskID, Prompt: "execute the step"})
	if !strings.Contains(requiredPrompt, "Mode: `required_unbound`") || !strings.Contains(requiredPrompt, "cause=environment") {
		t.Fatalf("remote-required + unbound must tell the agent to escalate, not retry:\n%s", promptSection(requiredPrompt))
	}
}

// promptSection keeps a failing assertion readable instead of dumping a 400-line
// prompt into the test log.
func promptSection(prompt string) string {
	idx := strings.Index(prompt, "## Delivery mode")
	if idx < 0 {
		return "<no Delivery mode section>\n" + firstLines(prompt, 20)
	}
	end := strings.Index(prompt[idx:], "\n\n")
	if end < 0 {
		return prompt[idx:]
	}
	return prompt[idx : idx+end]
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
