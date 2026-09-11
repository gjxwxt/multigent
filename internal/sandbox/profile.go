package sandbox

import (
	"fmt"
	"os"
	"strings"
)

// Runtime profile names identify managed runtime image families. Profiles are
// declared explicitly (project sandbox config, workflow steps, or the preview
// engine); the sandbox never sniffs project files to pick one.
const (
	// ProfileBase is the default profile: Node + Python + Go (dev tools), no JVM.
	ProfileBase = "base"
	// ProfileJVM21 adds a pinned Temurin JDK 21 on top of ProfileBase.
	ProfileJVM21 = "jvm21"
)

// RuntimeJVM21Version is the versioned default tag of the managed
// runtime-jvm21 image family (plan §3 P1: no floating `latest` in production;
// prepare records the pulled digest for traceability). Operators may still
// override the reference via MULTIGENT_RUNTIME_IMAGE — such overrides must
// honor the jvm21 image contract (JDK layout, trust/env variables, toolchain)
// and own digest pinning for the substituted image.
const RuntimeJVM21Version = "2026.9.1"

// JVM21ImageBase is the published runtime-jvm21 image (intranet/enterprise
// installs should override via MULTIGENT_RUNTIME_IMAGE or per-project image).
const JVM21ImageBase = imagePrefix + "/runtime-jvm21:" + RuntimeJVM21Version

// ChinaJVM21Image is the official mainland China mirror of JVM21ImageBase.
const ChinaJVM21Image = ChinaImagePrefix + "/runtime-jvm21:" + RuntimeJVM21Version

// LocalJVM21Image is the tag produced by local builds of the jvm21 profile.
// Build with: docker buildx build --platform linux/<arch> -t
// multigent/runtime-jvm21:2026.9.1 docker/runtime-jvm21
const LocalJVM21Image = "multigent/runtime-jvm21:" + RuntimeJVM21Version

// EnvRuntimeProfile selects the managed runtime profile for this server
// process ("base", "jvm21"). Bridged from [runtime] profile in the config
// file; individual projects may still override per sandbox config.
const EnvRuntimeProfile = "MULTIGENT_RUNTIME_PROFILE"

// NormalizeProfile validates a user-supplied profile name. Unknown profiles
// fail closed: a typo like "jvm" must not silently fall back to base.
func NormalizeProfile(profile string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "":
		return ProfileBase, nil
	case ProfileBase:
		return ProfileBase, nil
	case ProfileJVM21, "jdk21", "java21":
		return ProfileJVM21, nil
	default:
		return "", fmt.Errorf("sandbox: unknown runtime profile %q (supported: base, jvm21)", profile)
	}
}

// RuntimeSelection is the resolved runtime for one execution context (agent
// sandbox or preview container): the normalized profile and the concrete
// image reference to run.
type RuntimeSelection struct {
	Profile  string
	ImageRef string
}

// RuntimeRequest carries the authorities that participate in runtime
// resolution. Higher layers pass what they know; the decision order is
// documented on ResolveRuntime.
type RuntimeRequest struct {
	// ExplicitImage is a pinned image reference; it always wins.
	ExplicitImage string
	// AgentProfile is the individual agent's sandbox profile preference. It
	// only applies in projects that declare no runtime profile of their own.
	AgentProfile string
	// ProjectProfile is the project-level runtime profile authority. It is
	// authoritative for everything the project delivers: agent sandboxes and
	// preview containers resolve through the same value, so a jvm21 project
	// never runs one member's workload on base while its preview has a JDK.
	ProjectProfile string
}

// ResolveRuntime resolves the runtime for one execution context (agent
// sandbox or preview container): the normalized profile and the concrete
// image reference to run. Decision order:
//
//  1. ExplicitImage — a pinned image reference always wins.
//  2. ProjectProfile — the project-declared runtime profile (written by
//     templates and admins) is authoritative for project delivery.
//  3. AgentProfile — the individual agent's profile preference, honored only
//     when the project declares none (ad-hoc experiments in base projects).
//  4. Server environment default (MULTIGENT_RUNTIME_PROFILE / region).
//
// Declared profiles are validated: an unknown profile fails closed with an
// error instead of silently degrading to base. Only the server environment
// degrades to base on an invalid value — appconfig already rejects it at
// startup, so refusing every run over a stale env var would be worse.
func ResolveRuntime(req RuntimeRequest) (RuntimeSelection, error) {
	agentProfile := ""
	if strings.TrimSpace(req.AgentProfile) != "" {
		p, err := NormalizeProfile(req.AgentProfile)
		if err != nil {
			return RuntimeSelection{}, fmt.Errorf("agent sandbox profile: %w", err)
		}
		agentProfile = p
	}
	projectProfile := ""
	if strings.TrimSpace(req.ProjectProfile) != "" {
		p, err := NormalizeProfile(req.ProjectProfile)
		if err != nil {
			return RuntimeSelection{}, fmt.Errorf("project runtime profile: %w", err)
		}
		projectProfile = p
	}
	if image := strings.TrimSpace(req.ExplicitImage); image != "" {
		// A pinned image wins over any declared profile; the profile recorded
		// in the selection is the declared one (informational only — the image
		// choice itself never depends on image-name inference).
		return RuntimeSelection{
			Profile:  firstNonEmpty(projectProfile, agentProfile, ProfileBase),
			ImageRef: normalizeDefaultImage(image),
		}, nil
	}
	profile := firstNonEmpty(projectProfile, agentProfile)
	if profile == "" {
		profile, _ = NormalizeProfile(os.Getenv(EnvRuntimeProfile))
	}
	if profile == ProfileJVM21 {
		return RuntimeSelection{Profile: profile, ImageRef: normalizeProfileImage(ImageForProfile(profile))}, nil
	}
	return RuntimeSelection{Profile: ProfileBase, ImageRef: normalizeDefaultImage(DefaultBaseImage())}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// localImageExists reports whether the image reference exists in the local
// Docker store — without platform compatibility filtering. A locally built
// image is always preferred over a registry pull even when its platform does
// not match the host: launching it surfaces a clear platform error, while a
// silent fallback would pull from the public registry in environments with no
// egress (the exact situation local builds exist for).
func localImageExists(image string) bool {
	if strings.TrimSpace(image) == "" {
		return false
	}
	return DockerCommand("image", "inspect", image).Run() == nil
}

// ImageForProfile returns the managed image for a profile, honoring the same
// region/local-build resolution rules as DefaultBaseImage. An explicit
// MULTIGENT_RUNTIME_IMAGE is an operator override and is returned as-is —
// image capability is never inferred from the image's name (enterprise
// registries rename images; the name says nothing about the contents).
func ImageForProfile(profile string) string {
	if profile == ProfileJVM21 {
		if localImageExists(LocalJVM21Image) {
			return LocalJVM21Image
		}
		if image := strings.TrimSpace(os.Getenv(EnvRuntimeImage)); image != "" {
			return image
		}
		switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvRuntimeRegion))) {
		case "cn", "china", "zh-cn", "mainland", "mainland-china":
			return ChinaJVM21Image
		default:
			return JVM21ImageBase
		}
	}
	return DefaultBaseImage()
}

// ProfileFromImage reverse-maps an image reference to its profile family.
//
// Deprecated: image-name inference no longer participates in any resolution
// or env decision — runtime capability comes from declared profiles
// (RuntimeRequest / RuntimeSelection), because enterprise registries rename
// images and the name says nothing about the contents. Kept only for
// diagnostics and legacy callers.
func ProfileFromImage(image string) string {
	switch {
	case strings.Contains(image, "runtime-jvm21"):
		return ProfileJVM21
	default:
		return ProfileBase
	}
}
