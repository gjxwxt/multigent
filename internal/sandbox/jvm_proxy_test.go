package sandbox

import (
	"os"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestJVMToolchainProxyEnvFromTransport(t *testing.T) {
	origHTTPS, origHTTP, origNO := os.Getenv("HTTPS_PROXY"), os.Getenv("HTTP_PROXY"), os.Getenv("NO_PROXY")
	defer func() {
		setEnvForTest("HTTPS_PROXY", origHTTPS)
		setEnvForTest("HTTP_PROXY", origHTTP)
		setEnvForTest("NO_PROXY", origNO)
	}()

	t.Run("derives JAVA_TOOL_OPTIONS from proxy env", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://proxy.internal:17890")
		os.Unsetenv("HTTP_PROXY")
		t.Setenv("NO_PROXY", "localhost,127.0.0.1,.internal.example")

		got := JVMToolchainProxyEnv()
		if got == "" {
			t.Fatalf("expected JAVA_TOOL_OPTIONS value, got empty")
		}
		for _, want := range []string{
			"-Dhttps.proxyHost=proxy.internal",
			"-Dhttps.proxyPort=17890",
			"-Dhttp.nonProxyHosts=",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("JAVA_TOOL_OPTIONS %q missing %q", got, want)
			}
		}
		// http proxy unset -> http.proxyHost must not claim a proxy.
		if strings.Contains(got, "-Dhttp.proxyHost") {
			t.Fatalf("JAVA_TOOL_OPTIONS %q must not set http.proxyHost without HTTP_PROXY", got)
		}
		// nonProxyHosts must translate the separator and keep mandatory localhost.
		if !strings.Contains(got, "localhost|127.0.0.1|*.internal.example") {
			t.Fatalf("JAVA_TOOL_OPTIONS %q missing translated nonProxyHosts", got)
		}
	})

	t.Run("both proxies set", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://10.0.0.9:3128")
		t.Setenv("HTTP_PROXY", "http://10.0.0.9:3128")
		os.Unsetenv("NO_PROXY")

		got := JVMToolchainProxyEnv()
		if !strings.Contains(got, "-Dhttps.proxyHost=10.0.0.9") || !strings.Contains(got, "-Dhttp.proxyHost=10.0.0.9") {
			t.Fatalf("JAVA_TOOL_OPTIONS %q missing both proxy hosts", got)
		}
		// Empty NO_PROXY still yields the JVM-mandatory localhost entries.
		if !strings.Contains(got, "-Dhttp.nonProxyHosts=localhost|127.0.0.1") {
			t.Fatalf("JAVA_TOOL_OPTIONS %q missing default nonProxyHosts", got)
		}
	})

	t.Run("no proxy env -> empty", func(t *testing.T) {
		os.Unsetenv("HTTPS_PROXY")
		os.Unsetenv("HTTP_PROXY")
		if got := JVMToolchainProxyEnv(); got != "" {
			t.Fatalf("expected empty JAVA_TOOL_OPTIONS without proxy env, got %q", got)
		}
	})

	t.Run("malformed proxy url degrades gracefully", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "::::not-a-url")
		os.Unsetenv("HTTP_PROXY")
		os.Unsetenv("NO_PROXY")
		// Malformed value must not produce a broken host/port pair; the
		// scheme is simply skipped.
		if got := JVMToolchainProxyEnv(); strings.Contains(got, "-Dhttps.proxyHost=::") {
			t.Fatalf("JAVA_TOOL_OPTIONS %q contains malformed host", got)
		}
	})
}

func TestProfileDockerArgsJVMProxy(t *testing.T) {
	origHTTPS := os.Getenv("HTTPS_PROXY")
	defer setEnvForTest("HTTPS_PROXY", origHTTPS)

	t.Setenv("HTTPS_PROXY", "http://proxy.internal:17890")

	base := &entity.DockerSandboxConfig{}
	jvm := &entity.DockerSandboxConfig{Profile: ProfileJVM21}

	if got := ProfileDockerArgs(base); containsEnvPair(got, "JAVA_TOOL_OPTIONS") {
		t.Fatalf("base profile must not receive JAVA_TOOL_OPTIONS: %v", got)
	}
	jvmArgs := ProfileDockerArgs(jvm)
	if !containsEnvPair(jvmArgs, "JAVA_TOOL_OPTIONS") {
		t.Fatalf("jvm21 profile must receive JAVA_TOOL_OPTIONS: %v", jvmArgs)
	}
	// Every env entry must carry its own -e flag (docker image-reference bug).
	for i, arg := range jvmArgs {
		if strings.Contains(arg, "=") && !strings.HasPrefix(arg, "-") {
			if i == 0 || jvmArgs[i-1] != "-e" {
				t.Fatalf("env value %q at %d must be preceded by -e, got %v", arg, i, jvmArgs)
			}
		}
	}
}

func containsEnvPair(args []string, key string) bool {
	for i, arg := range args {
		if i > 0 && args[i-1] == "-e" && strings.HasPrefix(arg, key+"=") {
			return true
		}
	}
	return false
}

func setEnvForTest(key, value string) {
	if value == "" {
		os.Unsetenv(key)
		return
	}
	_ = os.Setenv(key, value)
}

// jdk21/java21 aliases must behave exactly like jvm21: the JVM proxy
// properties key off the normalized profile, not a string comparison.
func TestProfileDockerArgsNormalizesAliases(t *testing.T) {
	origHTTPS := os.Getenv("HTTPS_PROXY")
	defer setEnvForTest("HTTPS_PROXY", origHTTPS)
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:17890")

	for _, alias := range []string{"jvm21", "jdk21", "java21", "JVM21", " jvm21 "} {
		args := ProfileDockerArgs(&entity.DockerSandboxConfig{Profile: alias})
		if !containsEnvPair(args, "JAVA_TOOL_OPTIONS") {
			t.Fatalf("alias %q must receive JAVA_TOOL_OPTIONS, got %v", alias, args)
		}
	}
	// Unknown profiles fail closed: no JVM properties for an undeclared value.
	if args := ProfileDockerArgs(&entity.DockerSandboxConfig{Profile: "node22"}); len(args) != 0 {
		t.Fatalf("unknown profile must produce no JVM args, got %v", args)
	}
}

// Project jvm21 + agent-pinned explicit image: the recorded project profile
// must still deliver JVM network properties to the AGENT sandbox path
// (BuildArgs), symmetric with the preview path, while the image selection
// itself stays the pinned reference.
func TestBuildArgsPinnedImageKeepsProjectProfileJVMArgs(t *testing.T) {
	origHTTPS := os.Getenv("HTTPS_PROXY")
	defer setEnvForTest("HTTPS_PROXY", origHTTPS)
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:17890")

	cfg := &entity.DockerSandboxConfig{
		Profile: ProfileJVM21,
		Image:   "registry.example/team/jdk-stack:21",
	}
	args, err := BuildArgs("/tmp/agent-dir-nonexistent", "codex", cfg, []string{"echo", "hi"})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !containsEnvPair(args, "JAVA_TOOL_OPTIONS") {
		t.Fatalf("pinned-image sandbox with project jvm21 must still get JVM proxy args, args=%v", args)
	}
	found := false
	for _, a := range args {
		if a == "registry.example/team/jdk-stack:21" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pinned image must be the executed image, args=%v", args)
	}
}
