package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/multigent/multigent/internal/imbridge"
	"github.com/spf13/cobra"
)

func newMattermostBridgeCmd() *cobra.Command {
	var (
		multigentURL string
		pollInterval time.Duration
		statusAddr   string
	)

	cmd := &cobra.Command{
		Use:   "mattermost-bridge",
		Short: "Run Mattermost WebSocket bridge for Multigent",
		Long: `mattermost-bridge maintains WebSocket connections to Mattermost for all
connected agents, catches up on missed messages after reconnects, and forwards
incoming messages to Multigent over loopback with HMAC authentication.`,
		Example: `  multigent mattermost-bridge
  multigent mattermost-bridge --multigent-url http://127.0.0.1:27892
  multigent mattermost-bridge --status-addr 127.0.0.1:27894`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, _ := resolveRoot()
			db, err := openControlDBForRoot(root)
			if err != nil {
				return fmt.Errorf("open control database: %w", err)
			}
			defer db.Close()

			bridgeCfg := imbridge.DefaultBridgeConfig()
			if multigentURL != "" {
				bridgeCfg.MultigentURL = multigentURL
			}
			if pollInterval > 0 {
				bridgeCfg.PollInterval = pollInterval
			}
			bridgeCfg.StatusAddr = statusAddr

			bridge := imbridge.NewMattermostBridge(bridgeCfg, db)

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			log.Printf("Starting Mattermost WebSocket Bridge (multigent=%s)", bridgeCfg.MultigentURL)
			if err := bridge.Start(ctx); err != nil && err != context.Canceled {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&multigentURL, "multigent-url", "http://127.0.0.1:27892", "Target Multigent server URL for forwarding events")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 30*time.Second, "Interval to check for new/updated agent channel bindings")
	cmd.Flags().StringVar(&statusAddr, "status-addr", "127.0.0.1:27894", "Loopback address for bridge health status endpoint")

	return cmd
}
