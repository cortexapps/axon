package cmd

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/grpctunnel"
	"github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/spf13/cobra"
	"go.uber.org/fx"
)

//go:embed banner.txt
var banner string

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Starts the server",
	Run: func(cmd *cobra.Command, args []string) {

		// we instantiate the agent configuration outside of the
		// stack so we can pick up args if needed
		if ok, _ := cmd.Flags().GetBool("dry-run"); ok {
			os.Setenv("DRYRUN", "true")
		}

		config := config.NewAgentEnvConfig().ApplyFlags(cmd.Flags())

		if id, _ := cmd.Flags().GetString("alias"); id != "" {
			config.IntegrationAlias = id
		}

		config.RelayIdleTimeout = serveRelayIdleTimeout(config.RelayIdleTimeout)

		config.Print()

		info := common.IntegrationInfo{
			Integration: common.IntegrationCustom,
			Alias:       config.IntegrationAlias,
		}

		relayModule := snykbroker.Module
		if config.IsGRPCTunnel() {
			relayModule = grpctunnel.Module
		}

		stack := fx.Options(
			initStack(cmd, config, info),
			AgentModule,
			http.Module,
			relayModule,
		)

		startAgent(stack)
		fmt.Println("Server stopped")
	},
}

// serveRelayIdleTimeout disables the relay idle watchdog for serve mode
// unless RELAY_IDLE_TIMEOUT is set explicitly. A handler agent's tunnel
// carries only what relay-dispatcher sends it, and the broker server
// delivers that to just the newest client per token, so the other
// replicas would look idle and be restarted in rotation forever.
func serveRelayIdleTimeout(configured time.Duration) time.Duration {
	if _, set := os.LookupEnv("RELAY_IDLE_TIMEOUT"); set {
		return configured
	}
	return 0
}

func init() {
	serveCmd.Flags().Bool("dry-run", false, "Dry run mode")
	serveCmd.Flags().StringP("alias", "a", "customer-agent", "Alias (identifier) for this agent type")
}
