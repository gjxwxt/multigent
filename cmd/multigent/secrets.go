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
					path, err := defaultSecretsBackupPath(backupPath)
					if err != nil {
						return err
					}
					if err := copyControlDBBackup(path); err != nil {
						return fmt.Errorf("create control DB backup: %w", err)
					}
					report.BackupPath = path
					fmt.Printf("backup written: %s\n", path)
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

// copyControlDBBackup copies the live control DB (and WAL/SHM siblings) to
// dest. Checkpoint-free copy is safe here because the server should be
// stopped or idle during migration; the copies keep the snapshot consistent
// either way.
func copyControlDBBackup(dest string) error {
	dbPath, err := controldb.DefaultPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := dbPath + suffix
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) && suffix != "" {
				continue
			}
			if os.IsNotExist(err) {
				return fmt.Errorf("control DB %s not found", dbPath)
			}
			return err
		}
		if err := os.WriteFile(dest+suffix, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
