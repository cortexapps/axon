package cmd

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
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

		config := config.NewAgentEnvConfig()

		if id, _ := cmd.Flags().GetString("alias"); id != "" {
			config.IntegrationAlias = id
		}

		config.Print()
		startAgent(buildServeStack(cmd, config))
		fmt.Println("Server stopped")
	},
}

func init() {
	serveCmd.Flags().Bool("dry-run", false, "Dry run mode")
	serveCmd.Flags().BoolP("verbose", "v", false, "Verbose mode")
	serveCmd.Flags().StringP("alias", "a", "customer-agent", "Alias (identifier) for this agent type")
}

// buildServeStack builds the fx dependency injection stack for the agent
func buildServeStack(cmd *cobra.Command, cfg config.AgentConfig) fx.Option {

	opts := []fx.Option{
		buildCoreAgentStack(cmd, cfg),
		fx.Supply(common.IntegrationInfo{
			Integration: common.IntegrationCustom,
			Alias:       cfg.IntegrationAlias,
		}),
		fx.Provide(createHttpServer),
		fx.Provide(cron.New),
		fx.Invoke(createWebhookHttpServer),
	}

	if !cfg.DryRun || os.Getenv("BROKER_TOKEN") != "" {
		// we only start the broker if we have a token
		opts = append(opts,
			fx.Invoke(snykbroker.NewRelayInstanceManager),
		)
	}

	return fx.Options(
		opts...,
	)
}
