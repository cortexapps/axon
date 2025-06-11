package cmd

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
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

		config := config.NewAgentEnvConfig()

		if id, _ := cmd.Flags().GetString("alias"); id != "" {
			config.IntegrationAlias = id
		}

		config.Print()

		info := common.IntegrationInfo{
			Integration: common.IntegrationCustom,
			Alias:       config.IntegrationAlias,
		}

		stack := fx.Options(
			initStack(cmd, config, info),
			AgentModule,
			http.Module,
			snykbroker.Module,
		)

		startAgent(stack)
		fmt.Println("Server stopped")
	},
}

func init() {
	serveCmd.Flags().Bool("dry-run", false, "Dry run mode")
	serveCmd.Flags().BoolP("verbose", "v", false, "Verbose mode")
	serveCmd.Flags().StringP("alias", "a", "customer-agent", "Alias (identifier) for this agent type")
}
