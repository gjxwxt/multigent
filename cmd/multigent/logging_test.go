package main

import (
	"testing"

	"github.com/multigent/multigent/internal/appconfig"
)

// The log size env contract: MULTIGENT_LOG_MAX_SIZE_MB (MB) is the canonical
// key; MULTIGENT_LOG_MAX_SIZE (bytes, written by the daemon systemd/launchd
// installers) is the legacy compatibility key read only when the canonical
// one is absent. Both keys exist by design — this test locks the precedence
// so a refactor cannot silently flip it (migration-notes §6 item 2 resolved
// by documenting rather than unifying: the byte-form writers are installed
// units in the field).
func TestResolveServiceLogOptionsLogSizeKeys(t *testing.T) {
	flagUnchanged := func(string) bool { return false }

	t.Run("canonical MB key wins", func(t *testing.T) {
		t.Setenv("MULTIGENT_LOG_MAX_SIZE_MB", "25")
		t.Setenv("MULTIGENT_LOG_MAX_SIZE", "999999999") // would clamp to 953MB if wrongly preferred
		opts := resolveServiceLogOptions(nil, "", "", "", 0, flagUnchanged)
		if opts.MaxSizeMB != 25 {
			t.Fatalf("MaxSizeMB=%d, want 25 (canonical key must win)", opts.MaxSizeMB)
		}
	})

	t.Run("byte key is the fallback", func(t *testing.T) {
		t.Setenv("MULTIGENT_LOG_MAX_SIZE_MB", "")
		t.Setenv("MULTIGENT_LOG_MAX_SIZE", "20971520") // 20 MB
		opts := resolveServiceLogOptions(nil, "", "", "", 0, flagUnchanged)
		if opts.MaxSizeMB != 20 {
			t.Fatalf("MaxSizeMB=%d, want 20 (bytes/1024/1024)", opts.MaxSizeMB)
		}
	})

	t.Run("config file value used when no env", func(t *testing.T) {
		t.Setenv("MULTIGENT_LOG_MAX_SIZE_MB", "")
		t.Setenv("MULTIGENT_LOG_MAX_SIZE", "")
		cfg := &appconfig.Config{}
		cfg.Logging.MaxSizeMB = 7
		opts := resolveServiceLogOptions(cfg, "", "", "", 0, flagUnchanged)
		if opts.MaxSizeMB != 7 {
			t.Fatalf("MaxSizeMB=%d, want 7 (config file)", opts.MaxSizeMB)
		}
	})
}
