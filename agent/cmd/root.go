package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "cortex-axon",
	Short: "Cortex Axon is an agent for Cortex",
}

func init() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(handlersRootCmd)
	rootCmd.AddCommand(RelayCommand)

	rootCmd.Flags().BoolP("verbose", "v", false, "Verbose mode")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
