package main

import (
	"fmt"
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
