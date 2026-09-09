package telemetry

import (
	"strings"
	"testing"
)

func TestRedactSensitiveOutput(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		contains []string
		omits    []string
	}{
		{
			name:     "jwt token redaction",
			input:    "Agent token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c in session",
			contains: []string{"eyJ...<redacted>"},
			omits:    []string{"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ"},
		},
		{
			name:     "multigent agent token env",
			input:    "export MULTIGENT_AGENT_TOKEN=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc.def\nMULTIGENT_RUN_ID=123",
			contains: []string{"MULTIGENT_AGENT_TOKEN=<redacted>", "MULTIGENT_RUN_ID=123"},
			omits:    []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc.def"},
		},
		{
			name:     "anthropic api key",
			input:    "ANTHROPIC_API_KEY=\"sk-ant-api03-abcdef1234567890\"\nANTHROPIC_AUTH_TOKEN='sk-auth-secret'",
			contains: []string{"ANTHROPIC_API_KEY=<redacted>", "ANTHROPIC_AUTH_TOKEN=<redacted>"},
			omits:    []string{"sk-ant-api03-abcdef1234567890", "sk-auth-secret"},
		},
		{
			name:     "bearer token",
			input:    "Authorization: Bearer my-secret-bearer-token-1234567890",
			contains: []string{"Bearer <redacted>"},
			omits:    []string{"my-secret-bearer-token-1234567890"},
		},
		{
			name:     "private key",
			input:    "Key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0\n-----END RSA PRIVATE KEY-----\nDone",
			contains: []string{"[REDACTED PRIVATE KEY]"},
			omits:    []string{"MIIEowIBAAKCAQEA0"},
		},
		{
			name:     "format exec command redacts token",
			input:    FormatExecCommand("docker", []string{"run", "-e", "MULTIGENT_AGENT_TOKEN=secret_jwt", "image"}),
			contains: []string{"MULTIGENT_AGENT_TOKEN=<redacted>"},
			omits:    []string{"secret_jwt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSensitiveOutput(tc.input)
			for _, exp := range tc.contains {
				if !strings.Contains(got, exp) {
					t.Errorf("expected %q to contain %q, got %q", tc.input, exp, got)
				}
			}
			for _, omit := range tc.omits {
				if strings.Contains(got, omit) {
					t.Errorf("expected %q to omit %q, got %q", tc.input, omit, got)
				}
			}
		})
	}
}
