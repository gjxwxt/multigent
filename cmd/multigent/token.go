package main

import (
	"fmt"
	"os"
	"time"

	"github.com/multigent/multigent/internal/api"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/spf13/cobra"
)

// adminTokenCmd issues a local JWT for the admin console API without a
// browser login flow. It exists for operators on the deployment host: health
// checks after upgrades, smoke-task creation, ChatOps card debugging — all of
// which previously required hand-rolling the custom-alphabet JWT from the
// jwt_secret setting (a procedure nobody should have to reverse-engineer
// under pressure).
func newAdminTokenCmd() *cobra.Command {
	var (
		username string
		duration time.Duration
	)
	cmd := &cobra.Command{
		Use:   "admin-token",
		Short: "Issue a local console API token for an admin operator task",
		Long: `Issue a short-lived Bearer token for the local console API.

The token signs with the same jwt_secret the running server validates against,
so it works immediately against http://127.0.0.1:<addr>/api/v1 while the
console is up. Default subject is the first admin account; default lifetime is
30 minutes. Run this on the deployment host with access to the data directory.
The control DB is resolved exactly like the server's (MULTIGENT_DATA_DIR /
MULTIGENT_CONTROL_DATA_DIR / $HOME/.multigent) — pass the same environment the
service runs with, e.g.:

  sudo systemctl show multigent --property=Environment   # find MULTIGENT_DATA_DIR
  sudo env MULTIGENT_DATA_DIR=/opt/multigent/data multigent admin-token

  multigent admin-token --user admin --ttl 15m

Use the token as "Authorization: Bearer <token>". Treat it as a credential:
it grants the named account's full API access until it expires.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warnIfControlDBLooksFresh()
			db, err := controldb.OpenDefault()
			if err != nil {
				return fmt.Errorf("open control DB: %w", err)
			}
			defer db.Close()

			users := api.NewUserStore(db)
			name := username
			if name == "" {
				admin := firstAdminUsername(users)
				if admin == "" {
					return fmt.Errorf("no admin account found; create one first or pass --user")
				}
				name = admin
			}
			user := users.GetUser(name)
			if user == nil {
				return fmt.Errorf("user %q not found", name)
			}
			if user.Disabled {
				return fmt.Errorf("user %q is disabled", name)
			}
			fmt.Println(users.IssueToken(user.Username, duration))
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "user", "", "target username (default: first admin account)")
	cmd.Flags().DurationVar(&duration, "ttl", 30*time.Minute, "token lifetime (e.g. 15m, 1h); server rejects expired tokens")
	return cmd
}

func firstAdminUsername(users *api.UserStore) string {
	for _, u := range users.ListUsers() {
		if u.Role == "admin" && !u.Disabled {
			return u.Username
		}
	}
	return ""
}

// warnIfControlDBLooksFresh flags the case where the resolved control DB does
// not exist yet: opening it would silently CREATE an empty store with a
// brand-new jwt_secret, and every token issued against it is rejected by the
// running server (which validates against the real data directory's secret —
// the production footgun where `sudo multigent admin-token` without
// MULTIGENT_DATA_DIR produced a 401). A missing DB is never legitimate for
// this command: there are no accounts in a fresh store to mint tokens for.
func warnIfControlDBLooksFresh() {
	path, err := controldb.DefaultPath()
	if err != nil {
		return
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return
	}
	envSet := os.Getenv("MULTIGENT_CONTROL_DATA_DIR") != "" || os.Getenv("MULTIGENT_DATA_DIR") != ""
	fmt.Fprintf(os.Stderr, "warning: control DB %s does not exist and will be CREATED EMPTY with a new jwt_secret\n", path)
	if !envSet {
		fmt.Fprintf(os.Stderr, "warning: MULTIGENT_DATA_DIR / MULTIGENT_CONTROL_DATA_DIR are unset, so this resolved to $HOME/.multigent — under sudo that is root's home, not the service's data dir\n")
	}
	fmt.Fprintf(os.Stderr, "warning: a token from a fresh DB will be REJECTED by the running server. Pass the service's environment, e.g.: sudo env MULTIGENT_DATA_DIR=<service data dir> multigent admin-token\n")
}
