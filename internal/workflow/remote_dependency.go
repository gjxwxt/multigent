package workflow

import (
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// StepConfigRequiresRemote marks a step that cannot complete its purpose without
// a verified remote binding. It answers "what does this step need", which is a
// property of the workflow definition; whether the project has it is a separate
// question answered from the verified binding table.
const StepConfigRequiresRemote = "requires_remote"

// StepRequiresRemote reports whether a step declares a verified remote binding.
//
// Declaration only, with no step-ID fallback. An ID whitelist (the shape
// ciReadyGateStepIDs uses) would silently lose protection the moment a custom
// workflow renames a step, which manufactures false green — strictly worse than
// no signal. A definition that predates this marker therefore reports "not
// declared", and every consumer must render that as *uncertain*, never as ready.
// Granting an old definition the protection is done by re-instantiating it, not
// by guessing its shape.
func StepRequiresRemote(step entity.WorkflowStep) bool {
	return strings.EqualFold(strings.TrimSpace(step.Config[StepConfigRequiresRemote]), "true")
}

// RequiresRemoteStepIDs lists the IDs of steps that declare a verified remote
// binding, in template order. It is the payload a UI renders verbatim, so the
// rule for "which steps are remote-dependent" stays in this package only.
func RequiresRemoteStepIDs(steps []entity.WorkflowStep) []string {
	var ids []string
	for _, step := range steps {
		if StepRequiresRemote(step) {
			ids = append(ids, step.ID)
		}
	}
	return ids
}

// withStepConfig adds Config entries to a template step without widening
// tmplStep's argument list across every step declaration.
func withStepConfig(step entity.WorkflowStep, cfg map[string]string) entity.WorkflowStep {
	if step.Config == nil {
		step.Config = map[string]string{}
	}
	for k, v := range cfg {
		step.Config[k] = v
	}
	return step
}
