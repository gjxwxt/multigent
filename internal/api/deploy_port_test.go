package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestEnsureDeployPortAllocatesPoolOrder(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	sample, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	changed, err := s.ensureDeployPort(sample)
	if err != nil || !changed {
		t.Fatalf("first allocation: changed=%v err=%v", changed, err)
	}
	if sample.DeployPort != defaultDeployPortMin {
		t.Fatalf("first port=%d, want %d", sample.DeployPort, defaultDeployPortMin)
	}
	if err := s.st.SaveProject("sample", sample); err != nil {
		t.Fatalf("save sample: %v", err)
	}

	other := &entity.Project{Name: "other"}
	if _, err := s.ensureDeployPort(other); err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	if other.DeployPort != defaultDeployPortMin+1 {
		t.Fatalf("second port=%d, want %d (must skip in-use)", other.DeployPort, defaultDeployPortMin+1)
	}

	// An existing allocation is never re-assigned.
	other.DeployPort = 12345
	changed, err = s.ensureDeployPort(other)
	if changed || err != nil || other.DeployPort != 12345 {
		t.Fatalf("existing port must be kept: changed=%v err=%v port=%d", changed, err, other.DeployPort)
	}
}

func TestEnsureDeployPortRespectsRangeEnvAndExhaustion(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	t.Setenv(deployPortRangeEnv, "29000-29000")

	sample, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if _, err := s.ensureDeployPort(sample); err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if sample.DeployPort != 29000 {
		t.Fatalf("port=%d, want 29000 from env range", sample.DeployPort)
	}
	if err := s.st.SaveProject("sample", sample); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := s.ensureDeployPort(&entity.Project{Name: "other"}); err == nil {
		t.Fatal("expected exhaustion error for single-port pool")
	}
}

func TestDeployPortPoolRangeInvalidFallsBack(t *testing.T) {
	for _, raw := range []string{"not-a-range", "88000", "99999-88000", "80-70000", "abc-def"} {
		t.Setenv(deployPortRangeEnv, raw)
		min, max := deployPortPoolRange()
		if min != defaultDeployPortMin || max != defaultDeployPortMax {
			t.Fatalf("range(%q)=%d-%d, want fallback %d-%d", raw, min, max, defaultDeployPortMin, defaultDeployPortMax)
		}
	}
	t.Setenv(deployPortRangeEnv, "60000-60010")
	if min, max := deployPortPoolRange(); min != 60000 || max != 60010 {
		t.Fatalf("custom range parsed as %d-%d", min, max)
	}
}
