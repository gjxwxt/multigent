package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	controldb "github.com/multigent/multigent/internal/db"
)

// newSecretsCmd is the intranet security baseline entry point: inventory the
// secret storage surfaces and re-encrypt plaintext records once
// MULTIGENT_CONNECTION_ENCRYPTION_KEY is configured. It never prints secret
// material — only record identities and key-version markers.
func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Inspect and migrate connection/provider secret encryption",
	}
	cmd.AddCommand(newSecretsAuditCmd(), newSecretsMigrateCmd())
	return cmd
}

func newSecretsAuditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit",
		Short: "Inventory plaintext vs encrypted secret records",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := controldb.OpenDefault()
			if err != nil {
				return err
			}
			defer db.Close()
			report, err := db.AuditSecrets()
			if err != nil {
				return err
			}
			if err := printJSON(report); err != nil {
				return err
			}
			if len(report.Plaintext) > 0 {
				fmt.Fprintf(os.Stderr, "\n%d plaintext record(s). Set MULTIGENT_CONNECTION_ENCRYPTION_KEY, back up the control DB, then run `multigent secrets migrate --apply`.\n", len(report.Plaintext))
				os.Exit(2)
			}
			return nil
		},
	}
}

func newSecretsMigrateCmd() *cobra.Command {
	var apply bool
	var backupPath string
	var skipBackup bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Re-encrypt plaintext secret records with the configured key",
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY") == "" {
				return fmt.Errorf("MULTIGENT_CONNECTION_ENCRYPTION_KEY must be set before migrating; generate one with: openssl rand -hex 32")
			}
			db, err := controldb.OpenDefault()
			if err != nil {
				return err
			}
			defer db.Close()
			before, err := db.AuditSecrets()
			if err != nil {
				return err
			}
			if len(before.Plaintext) == 0 {
				fmt.Println("nothing to migrate: no plaintext secret records")
				return nil
			}
			report := controldb.SecretsMigrateReport{}
			if apply {
				if !skipBackup {
					path, err := resolveSecretsBackupPath(backupPath)
					if err != nil {
						return err
					}
					// VACUUM INTO produces a single-file consistent snapshot
					// of the live (possibly WAL-mode) database — a bare
					// sequential copy of db+wal+shm files is NOT a snapshot
					// while the server is running. Runs against the live DB
					// through the same SQLite connection, so it works with
					// the service up; the maintenance-window recommendation
					// in the docs is about the key cutover, not the backup.
					if err := db.BackupConsistent(path); err != nil {
						return fmt.Errorf("create control DB backup: %w", err)
					}
					if err := controldb.VerifyBackup(path); err != nil {
						return fmt.Errorf("backup failed integrity check: %w", err)
					}
					report.BackupPath = path
					fmt.Printf("consistent backup written and verified: %s\n", path)
				}
				result, err := db.MigrateSecrets()
				if err != nil {
					return err
				}
				report = *result
			} else {
				// Dry run: surface what WOULD change without writing.
				for _, rec := range before.Plaintext {
					report.Details = append(report.Details, controldb.SecretsMigrationResult{
						Table: rec.Table, ID: rec.ID, WorkspaceID: rec.WorkspaceID, From: rec.KeyVersion, To: "encrypted (dry run)",
					})
				}
			}
			if err := printJSON(report); err != nil {
				return err
			}
			if !apply {
				fmt.Fprintln(os.Stderr, "\ndry run only; re-run with --apply to write (a control DB backup is created automatically)")
			} else if len(report.Failed) > 0 {
				fmt.Fprintf(os.Stderr, "\n%d record(s) failed; inspect the failed[] entries. Re-run after fixing; successful records are not re-touched.\n", len(report.Failed))
				os.Exit(3)
			} else {
				fmt.Fprintf(os.Stderr, "\nre-encrypted %d record(s). Restart the server so all readers pick up the key, then verify `multigent secrets audit` reports zero plaintext.\n", report.ReEncrypted)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write the re-encrypted records (default is a dry run)")
	cmd.Flags().StringVar(&backupPath, "backup", "", "control DB backup path for --apply (default: sibling of the live DB)")
	cmd.Flags().BoolVar(&skipBackup, "skip-backup", false, "skip the automatic control DB backup before --apply")
	return cmd
}

func defaultSecretsBackupPath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return filepath.Clean(explicit), nil
	}
	dbPath, err := controldb.DefaultPath()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.pre-encrypt-%s", dbPath, time.Now().UTC().Format("20060102T150405Z")), nil
}

// resolveSecretsBackupPath finalizes the backup destination and refuses
// obviously wrong targets: an existing file (O_EXCL semantics — never
// overwrite a previous snapshot) and the live control DB itself.
func resolveSecretsBackupPath(explicit string) (string, error) {
	path, err := defaultSecretsBackupPath(explicit)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if live, err := controldb.DefaultPath(); err == nil {
		if liveAbs, err := filepath.Abs(live); err == nil && liveAbs == abs {
			return "", fmt.Errorf("backup path must not be the live control DB itself")
		}
	}
	if _, err := os.Stat(abs); err == nil {
		return "", fmt.Errorf("backup path %s already exists; refusing to overwrite a previous snapshot", abs)
	}
	return abs, nil
}
