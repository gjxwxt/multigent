package appconfig

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Workspace  WorkspaceConfig
	Server     ServerConfig
	Auth       AuthConfig
	SMTP       SMTPConfig
	Logging    LoggingConfig
	Runtime    RuntimeConfig
	Sandbox    SandboxConfig
	Playbooks  PlaybooksConfig
	Registries RegistriesConfig
	Network    NetworkConfig
}

type WorkspaceConfig struct {
	Dir string
}

type ServerConfig struct {
	Addr string
}

type AuthConfig struct {
	APIKey string
}

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
	TLS      string
}

type LoggingConfig struct {
	File      string
	Level     string
	Format    string
	MaxSizeMB int
	Stderr    *bool
}

type RuntimeConfig struct {
	Image       string
	Region      string
	NPMRegistry string
}

type SandboxConfig struct {
	AllowDirectHost *bool
	E2B             E2BConfig
}

type PlaybooksConfig struct {
	RegistryURLs []string
}

type E2BConfig struct {
	APIURL string
}

type RegistriesConfig struct {
	NPM       string
	PIP       string
	Go        string
	GoSumDB   string
	GoPrivate string
}

type NetworkConfig struct {
	HTTPSProxy string
	HTTPProxy  string
	NoProxy    string
}

func isValidSection(section string) bool {
	switch section {
	case "workspace", "server", "auth", "smtp", "logging", "runtime", "sandbox", "sandbox.e2b", "playbooks", "registries", "network":
		return true
	default:
		return false
	}
}

func Load(path string) (*Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return &Config{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &Config{}
	section := ""
	sc := bufio.NewScanner(f)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := stripComment(strings.TrimSpace(sc.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sec := strings.TrimSpace(strings.Trim(line, "[]"))
			if !isValidSection(sec) {
				return nil, fmt.Errorf("%s:%d: unknown section [%s]", path, lineNo, sec)
			}
			section = sec
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, lineNo)
		}
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "[") && !strings.Contains(raw, "]") {
			raw = collectMultilineArray(sc, &lineNo, raw)
		}
		if err := setValue(cfg, section, strings.TrimSpace(key), raw); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func collectMultilineArray(sc *bufio.Scanner, lineNo *int, firstLine string) string {
	var b strings.Builder
	b.WriteString(firstLine)
	for sc.Scan() {
		(*lineNo)++
		line := stripComment(strings.TrimSpace(sc.Text()))
		if line == "" {
			continue
		}
		b.WriteString(" ")
		b.WriteString(line)
		if strings.Contains(line, "]") {
			break
		}
	}
	return b.String()
}

func setValue(cfg *Config, section, key, raw string) error {
	switch section {
	case "workspace":
		if key == "dir" {
			cfg.Workspace.Dir = stringValue(raw)
			return nil
		}
		return fmt.Errorf("unknown key %q in section [%s]", key, section)
	case "server":
		if key == "addr" {
			cfg.Server.Addr = stringValue(raw)
			return nil
		}
		return fmt.Errorf("unknown key %q in section [%s]", key, section)
	case "auth":
		if key == "api_key" {
			cfg.Auth.APIKey = stringValue(raw)
			return nil
		}
		return fmt.Errorf("unknown key %q in section [%s]", key, section)
	case "smtp":
		switch key {
		case "host":
			cfg.SMTP.Host = stringValue(raw)
		case "port":
			cfg.SMTP.Port = intValue(raw)
		case "username":
			cfg.SMTP.Username = stringValue(raw)
		case "password":
			cfg.SMTP.Password = stringValue(raw)
		case "from":
			cfg.SMTP.From = stringValue(raw)
		case "from_name":
			cfg.SMTP.FromName = stringValue(raw)
		case "tls":
			cfg.SMTP.TLS = stringValue(raw)
		default:
			return fmt.Errorf("unknown key %q in section [%s]", key, section)
		}
		return nil
	case "logging":
		switch key {
		case "file":
			cfg.Logging.File = stringValue(raw)
		case "level":
			cfg.Logging.Level = stringValue(raw)
		case "format":
			cfg.Logging.Format = stringValue(raw)
		case "max_size_mb":
			cfg.Logging.MaxSizeMB = intValue(raw)
		case "stderr":
			v := boolValue(raw)
			cfg.Logging.Stderr = &v
		default:
			return fmt.Errorf("unknown key %q in section [%s]", key, section)
		}
		return nil
	case "runtime":
		switch key {
		case "image":
			cfg.Runtime.Image = stringValue(raw)
		case "region":
			cfg.Runtime.Region = stringValue(raw)
		case "npm_registry":
			cfg.Runtime.NPMRegistry = stringValue(raw)
		default:
			return fmt.Errorf("unknown key %q in section [%s]", key, section)
		}
		return nil
	case "sandbox":
		if key == "allow_direct_host" {
			v := boolValue(raw)
			cfg.Sandbox.AllowDirectHost = &v
			return nil
		}
		return fmt.Errorf("unknown key %q in section [%s]", key, section)
	case "sandbox.e2b":
		if key == "api_url" {
			cfg.Sandbox.E2B.APIURL = stringValue(raw)
			return nil
		}
		return fmt.Errorf("unknown key %q in section [%s]", key, section)
	case "playbooks":
		if key == "registry_urls" {
			cfg.Playbooks.RegistryURLs = stringSliceValue(raw)
			return nil
		}
		return fmt.Errorf("unknown key %q in section [%s]", key, section)
	case "registries":
		switch key {
		case "npm":
			cfg.Registries.NPM = stringValue(raw)
		case "pip":
			cfg.Registries.PIP = stringValue(raw)
		case "go":
			cfg.Registries.Go = stringValue(raw)
		case "go_sumdb":
			cfg.Registries.GoSumDB = stringValue(raw)
		case "go_private":
			cfg.Registries.GoPrivate = stringValue(raw)
		default:
			return fmt.Errorf("unknown key %q in section [%s]", key, section)
		}
		return nil
	case "network":
		switch key {
		case "https_proxy":
			cfg.Network.HTTPSProxy = stringValue(raw)
		case "http_proxy":
			cfg.Network.HTTPProxy = stringValue(raw)
		case "no_proxy":
			cfg.Network.NoProxy = stringValue(raw)
		default:
			return fmt.Errorf("unknown key %q in section [%s]", key, section)
		}
		return nil
	default:
		return fmt.Errorf("unknown section [%s]", section)
	}
}

func validateConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	if err := validateRegistryURL("registries.npm", cfg.Registries.NPM); err != nil {
		return err
	}
	if err := validateRegistryURL("registries.pip", cfg.Registries.PIP); err != nil {
		return err
	}
	if cfg.Registries.Go != "" {
		tokens := strings.FieldsFunc(cfg.Registries.Go, func(r rune) bool {
			return r == ',' || r == '|'
		})
		if len(tokens) == 0 {
			return fmt.Errorf("invalid registries.go: empty proxy configuration")
		}
		for _, token := range tokens {
			t := strings.TrimSpace(token)
			if t == "" {
				return fmt.Errorf("invalid registries.go: contains empty proxy token")
			}
			if strings.EqualFold(t, "direct") {
				return fmt.Errorf("invalid registries.go: enterprise proxy must not specify direct fallback")
			}
			if strings.EqualFold(t, "off") {
				return fmt.Errorf("invalid registries.go: enterprise proxy must not be off")
			}
			if err := validateRegistryURL("registries.go", t); err != nil {
				return err
			}
		}
		sumdb := strings.TrimSpace(cfg.Registries.GoSumDB)
		if sumdb == "" {
			return fmt.Errorf("registries.go_sumdb is required when registries.go is set")
		}
	}
	if cfg.Registries.GoSumDB != "" {
		sumdb := strings.TrimSpace(cfg.Registries.GoSumDB)
		if strings.EqualFold(sumdb, "off") {
			return fmt.Errorf("registries.go_sumdb must not be off")
		}
		if strings.ContainsAny(sumdb, "\r\n\t") {
			return fmt.Errorf("invalid registries.go_sumdb: contains invalid whitespace")
		}
	}
	if err := validateProxyURL("network.https_proxy", cfg.Network.HTTPSProxy); err != nil {
		return err
	}
	if err := validateProxyURL("network.http_proxy", cfg.Network.HTTPProxy); err != nil {
		return err
	}
	return nil
}

func validateRegistryURL(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("invalid %s: must be an absolute https:// URL with host", field)
	}
	if u.User != nil {
		return fmt.Errorf("invalid %s: must not contain userinfo", field)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid %s: must not contain query or fragment", field)
	}
	return nil
}

func validateProxyURL(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") || strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("invalid %s: must be an absolute URL (http, https, or socks5) with host", field)
	}
	if u.User != nil {
		return fmt.Errorf("invalid %s: must not contain userinfo", field)
	}
	return nil
}

func stripComment(s string) string {
	inString := false
	for i, r := range s {
		if r == '"' {
			inString = !inString
			continue
		}
		if r == '#' && !inString {
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

func stringValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		if v, err := strconv.Unquote(raw); err == nil {
			return v
		}
		return strings.Trim(raw, `"`)
	}
	return raw
}

func stringSliceValue(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"))
		if raw == "" {
			return nil
		}
		parts := splitCSV(raw)
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			v := stringValue(strings.TrimSpace(part))
			if v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	v := stringValue(raw)
	if v == "" {
		return nil
	}
	return []string{v}
}

func splitCSV(raw string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	escaped := false
	for _, r := range raw {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && inString {
			cur.WriteRune(r)
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			cur.WriteRune(r)
			continue
		}
		if r == ',' && !inString {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	out = append(out, cur.String())
	return out
}

func intValue(raw string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(raw))
	return v
}

func boolValue(raw string) bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(raw))
	return v
}
