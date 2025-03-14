package cmd

import (
	"fmt"
	"os"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/spf13/cobra"
	"go.uber.org/fx"
)

// relayCmd configures the relay command to support broker relaying
// of local API calls
var RelayCommand = &cobra.Command{
	Use:   "relay",
	Short: "Allows relaying calls from Cortex to the local environment",
	Run: func(cmd *cobra.Command, args []string) {

		config := config.NewAgentEnvConfig()
		if config.CortexApiToken == "" {
			fmt.Println("Cortex API token (CORTEX_API_TOKEN) must be provided")
			os.Exit(1)
		}

		acceptFile, _ := cmd.Flags().GetString("accept-file")
		integration, _ := cmd.Flags().GetString("integration")
		alias, _ := cmd.Flags().GetString("alias")
		subtype, _ := cmd.Flags().GetString("subtype")

		if acceptFile == "" && integration == "" {
			fmt.Println("Either accept-file or integration must be provided")
			os.Exit(1)
		} else if acceptFile != "" {
			stat, err := os.Stat(acceptFile)
			if err != nil || stat.IsDir() {
				fmt.Printf("Accept file %s does not exist or is a directory\n", acceptFile)
				os.Exit(1)
			}
		}

		i, err := common.ParseIntegration(integration)
		if acceptFile == "" && err != nil {
			fmt.Printf("invalid integration: %v", integration)
			os.Exit(1)
		}
		config.Integration = i.String()
		config.IntegrationAlias = alias

		info := common.IntegrationInfo{
			Integration:    i,
			Alias:          alias,
			Subtype:        subtype,
			AcceptFilePath: acceptFile,
		}

		if subtype != "" {

			_, err := info.ValidateSubtype()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}

		fmt.Println("Starting agent")
		startAgent(buildRelayStack(cmd, config, info))
	},
}

func init() {
	RelayCommand.Flags().StringP("accept-file", "f", "", "Accept.json file detailing which APIs are allowed to be relayed")
	RelayCommand.Flags().StringP("integration", "i", "", fmt.Sprintf("Integration to use for relaying, allowed values are: %v", common.ValidIntegrations()))
	RelayCommand.Flags().StringP("subtype", "s", "", "Integation subtype, integration dependent")
	RelayCommand.Flags().BoolP("verbose", "v", false, "Verbose mode")
	RelayCommand.Flags().StringP("alias", "a", "", "The alias to use for the integration")
	RelayCommand.MarkFlagRequired("integration")
	RelayCommand.MarkFlagRequired("alias")
}

// buildRelayStack builds the fx dependency injection stack for the agent
func buildRelayStack(cmd *cobra.Command, cfg config.AgentConfig, integrationInfo common.IntegrationInfo) fx.Option {
	cfg.EnableApiProxy = false
	return fx.Options(
		fx.Supply(integrationInfo),
		fx.Provide(cron.NewNoopCron),
		buildCoreAgentStack(cmd, cfg),
		fx.Provide(createHttpServer),
		fx.Invoke(snykbroker.NewRelayInstanceManager),
	)
}
