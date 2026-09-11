package sandbox

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestNormalizeProfile(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", ProfileBase, false},
		{"base", ProfileBase, false},
		{"jvm21", ProfileJVM21, false},
		{"JVM21", ProfileJVM21, false},
		{"jdk21", ProfileJVM21, false},
		{"java21", ProfileJVM21, false},
		{" jvm21 ", ProfileJVM21, false},
		{"jvm", "", true},
		{"node20", "", true},
		{"jvm21; rm -rf /", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeProfile(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeProfile(%q) expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeProfile(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeProfile(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProfileFromImage(t *testing.T) {
	cases := map[string]string{
		BaseImage:                 ProfileBase,
		ChinaBaseImage:            ProfileBase,
		LocalBaseImage:            ProfileBase,
		JVM21ImageBase:            ProfileJVM21,
		ChinaJVM21Image:           ProfileJVM21,
		LocalJVM21Image:           ProfileJVM21,
		"harbor.corp/x/custom:v1": ProfileBase,
	}
	for image, want := range cases {
		if got := ProfileFromImage(image); got != want {
			t.Errorf("ProfileFromImage(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestResolveImageProfileSelection(t *testing.T) {
	// Explicit image always wins over profile.
	img, err := EffectiveImage(entity.ModelClaudeCode, &entity.DockerSandboxConfig{
		Image:   "harbor.corp/custom:v1",
		Profile: "jvm21",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img != "harbor.corp/custom:v1" {
		t.Errorf("explicit image should win over profile, got %q", img)
	}

	// Profile=jvm21 without image selects a jvm21 image family member.
	img, err = EffectiveImage(entity.ModelClaudeCode, &entity.DockerSandboxConfig{Profile: "jvm21"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(img, "runtime-jvm21") {
		t.Errorf("profile jvm21 should select jvm21 image, got %q", img)
	}

	// Unknown profile fails closed: an error, never a silent base image.
	if _, err := EffectiveImage(entity.ModelClaudeCode, &entity.DockerSandboxConfig{Profile: "bogus"}); err == nil {
		t.Errorf("unknown profile should be rejected, got image with no error")
	}

	// Empty profile keeps existing behavior exactly.
	img, err = EffectiveImage(entity.ModelClaudeCode, &entity.DockerSandboxConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img != normalizeDefaultImage(DefaultBaseImage()) {
		t.Errorf("empty profile should keep default resolution, got %q", img)
	}
}

func TestImageForProfileBaseMatchesDefault(t *testing.T) {
	if ImageForProfile(ProfileBase) != DefaultBaseImage() {
		t.Errorf("ImageForProfile(base) should equal DefaultBaseImage()")
	}
}

func TestResolveRuntimeServerEnvDefault(t *testing.T) {
	t.Setenv(EnvRuntimeProfile, "")
	t.Setenv(EnvRuntimeImage, "")
	sel, err := ResolveRuntime(RuntimeRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != ProfileBase || !strings.Contains(sel.ImageRef, "runtime-base") {
		t.Errorf("empty env should resolve base + managed base image, got %+v", sel)
	}

	// Env profile selects the managed family image.
	t.Setenv(EnvRuntimeProfile, ProfileJVM21)
	sel, err = ResolveRuntime(RuntimeRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != ProfileJVM21 || !strings.Contains(sel.ImageRef, "runtime-jvm21") {
		t.Errorf("env jvm21 should select jvm21 image, got %+v", sel)
	}

	// Invalid env profile degrades to base (startup config already rejects it).
	t.Setenv(EnvRuntimeProfile, "node20")
	sel, err = ResolveRuntime(RuntimeRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != ProfileBase || !strings.Contains(sel.ImageRef, "runtime-base") {
		t.Errorf("invalid env profile should degrade to base, got %+v", sel)
	}
}

func TestResolveRuntimePriority(t *testing.T) {
	// Explicit image wins over everything; the recorded profile is the
	// declared one (never inferred from the image name).
	sel, err := ResolveRuntime(RuntimeRequest{
		ExplicitImage:  "harbor.corp/custom:v1",
		AgentProfile:   ProfileJVM21,
		ProjectProfile: ProfileJVM21,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.ImageRef != "harbor.corp/custom:v1" {
		t.Errorf("explicit image should win, got %+v", sel)
	}
	if sel.Profile != ProfileJVM21 {
		t.Errorf("declared profile should be recorded with explicit image, got %+v", sel)
	}

	// PROJECT profile is authoritative for delivery: it overrides an agent's
	// personal profile preference so agent sandboxes and previews agree.
	sel, err = ResolveRuntime(RuntimeRequest{
		AgentProfile:   ProfileBase,
		ProjectProfile: ProfileJVM21,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != ProfileJVM21 {
		t.Errorf("project profile must override agent preference, got %+v", sel)
	}

	// Agent profile applies only when the project declares none.
	sel, err = ResolveRuntime(RuntimeRequest{AgentProfile: ProfileJVM21})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != ProfileJVM21 || !strings.Contains(sel.ImageRef, "runtime-jvm21") {
		t.Errorf("agent profile should drive selection in undeclared projects, got %+v", sel)
	}

	// Project profile applies when agent has no preference either.
	sel, err = ResolveRuntime(RuntimeRequest{ProjectProfile: ProfileJVM21})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != ProfileJVM21 || !strings.Contains(sel.ImageRef, "runtime-jvm21") {
		t.Errorf("project profile should drive selection, got %+v", sel)
	}

	// Unknown agent profile fails closed with a descriptive error.
	_, err = ResolveRuntime(RuntimeRequest{AgentProfile: "node20"})
	if err == nil || !strings.Contains(err.Error(), "agent sandbox profile") {
		t.Errorf("unknown agent profile should fail closed, got err=%v", err)
	}

	// Unknown project profile fails closed with a descriptive error.
	_, err = ResolveRuntime(RuntimeRequest{ProjectProfile: "java-17"})
	if err == nil || !strings.Contains(err.Error(), "project runtime profile") {
		t.Errorf("unknown project profile should fail closed, got err=%v", err)
	}
}
