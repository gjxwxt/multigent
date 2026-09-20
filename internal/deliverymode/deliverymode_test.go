package deliverymode

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		project  *entity.Project
		bound    bool
		env      string
		want     Mode
	}{
		{"a verified binding always wins", &entity.Project{RemotePipelineRequired: "required"}, true, "true", ModeBound},
		{"unbound without a requirement is local branch", &entity.Project{}, false, "", ModeLocalBranch},
		{"project-declared requirement blocks instead of degrading", &entity.Project{RemotePipelineRequired: "required"}, false, "", ModeRequiredUnbound},
		{"project-local declaration outranks a strict server default", &entity.Project{RemotePipelineRequired: "local"}, false, "true", ModeLocalBranch},
		{"server default fills an undeclared project", &entity.Project{}, false, "1", ModeRequiredUnbound},
		{"nil project falls back to the server default", nil, false, "required", ModeRequiredUnbound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(RemotePipelineRequiredEnv, tc.env)
			if got := Resolve(tc.project, tc.bound); got != tc.want {
				t.Fatalf("Resolve(declared=%q bound=%v env=%q) = %q, want %q",
					declaredOf(tc.project), tc.bound, tc.env, got, tc.want)
			}
		})
	}
}

// ModeUnknown is not reachable from Resolve: it belongs to the caller that could
// not read the inputs. If it ever becomes reachable, a consumer that defaults to
// "safe" would start telling agents to stop pushing on a healthy project.
func TestResolveNeverReturnsUnknown(t *testing.T) {
	for _, bound := range []bool{true, false} {
		for _, declared := range []string{"", "local", "required", "bogus"} {
			if got := Resolve(&entity.Project{RemotePipelineRequired: declared}, bound); got == ModeUnknown {
				t.Fatalf("Resolve(bound=%v declared=%q) leaked ModeUnknown", bound, declared)
			}
		}
	}
}

func declaredOf(p *entity.Project) string {
	if p == nil {
		return "<nil>"
	}
	return p.RemotePipelineRequired
}
