package sandbox

import (
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// EnvJVMToolOptions is the JVM-standard env var picked up by every java
// process, so it reaches Gradle daemons, wrappers, and bootRun without any
// per-project files (no gradle.properties is committed to templates).
const EnvJVMToolOptions = "JAVA_TOOL_OPTIONS"

// JVM proxy system properties are set through JAVA_TOOL_OPTIONS because the
// JVM ignores HTTPS_PROXY/HTTP_PROXY environment variables — without this,
// `gradlew` wrapper downloads and dependency resolution hang in proxied
// environments even though curl/npm (which honor the env vars) work.
//
// The values derive at container-create time from the same typed network
// configuration (server config [network] section bridged into the process
// env) that governs every other toolchain; nothing environment-specific is
// committed into project templates. TLS-intercepting proxies that need a
// custom truststore are NOT expressible in the current typed config — that
// requires a dedicated config field, not a workaround here.

// jvmProxyHostPort parses a proxy URL into host/port. It only accepts http(s)
// schemes; anything else (or malformed input) yields no property pair rather
// than a broken one.
func jvmProxyHostPort(raw string) (host, port string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "", "", false
	}
	host = u.Hostname()
	if host == "" {
		return "", "", false
	}
	port = u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	if net.ParseIP(host) == nil && strings.Contains(host, ":") {
		// IPv6 literals need brackets in -DproxyHost? The JVM expects the bare
		// host; brackets break it. Skip them defensively.
		host = strings.Trim(host, "[]")
	}
	return host, port, true
}

// jvmNonProxyHosts converts a NO_PROXY comma list into the JVM's
// http.nonProxyHosts pipe list. Domain suffixes gain a "*." prefix so both
// "example.com" and "sub.example.com" bypass. localhost and loopback are
// always included: the JVM does not honor NO_PROXY's implicit local bypass.
func jvmNonProxyHosts(noProxy string) string {
	seen := map[string]bool{}
	var parts []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		if strings.HasPrefix(v, ".") {
			v = "*" + v
		}
		parts = append(parts, v)
	}
	add("localhost")
	add("127.0.0.1")
	for _, entry := range strings.Split(noProxy, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "*" {
			return "*"
		}
		add(entry)
	}
	return strings.Join(parts, "|")
}

// JVMToolchainProxyEnv returns the value for JAVA_TOOL_OPTIONS derived from
// the server's transport env (HTTPS_PROXY/HTTP_PROXY/NO_PROXY), or "" when no
// proxy is configured — the property must then stay unset so direct
// connections are not broken.
func JVMToolchainProxyEnv() string {
	var props []string
	if host, port, ok := jvmProxyHostPort(os.Getenv("HTTPS_PROXY")); ok {
		props = append(props,
			"-Dhttps.proxyHost="+host,
			"-Dhttps.proxyPort="+port,
		)
	}
	if host, port, ok := jvmProxyHostPort(os.Getenv("HTTP_PROXY")); ok {
		props = append(props,
			"-Dhttp.proxyHost="+host,
			"-Dhttp.proxyPort="+port,
		)
	}
	if len(props) == 0 {
		return ""
	}
	// https.proxyHost alone is not enough: gradle/java reads
	// http.nonProxyHosts for both schemes, and an HTTPS-only proxy setup
	// still needs the nonProxyHosts safety net.
	props = append(props, "-Dhttp.nonProxyHosts="+jvmNonProxyHosts(os.Getenv("NO_PROXY")))
	return strings.Join(props, " ")
}

// ProfileDockerArgs returns profile-dependent "-e KEY=VALUE" docker args for
// agent sandbox containers. It mirrors the preview engine's profilePreviewEnv:
// the jvm21 profile (canonical or declared via a supported alias — normalized
// here so jdk21/java21 behave identically; unknown profiles fail closed)
// receives JVM proxy properties via JAVA_TOOL_OPTIONS so Gradle wrapper
// downloads and dependency resolution inherit the platform's typed network
// configuration. Every entry carries its own "-e" flag — docker parses the
// first bare KEY=VALUE argument as the image reference.
func ProfileDockerArgs(cfg *entity.DockerSandboxConfig) []string {
	if cfg == nil {
		return nil
	}
	profile, err := NormalizeProfile(cfg.Profile)
	if err != nil || profile != ProfileJVM21 {
		return nil
	}
	if value := JVMToolchainProxyEnv(); value != "" {
		return []string{"-e", EnvJVMToolOptions + "=" + value}
	}
	return nil
}
