package cmd

import (
	"log"

	"github.com/cortexapps/axon/scaffold"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initializes a project",
	Run: func(cmd *cobra.Command, args []string) {
		language, _ := cmd.Flags().GetString("language")
		name, _ := cmd.Flags().GetString("name")
		path, _ := cmd.Flags().GetString("path")
		err := scaffold.Init(language, name, path)
		if err != nil {
			log.Fatalf("Failed to generate: %v", err)
		}
	},
}

func init() {
	initCmd.Flags().StringP("path", "p", "/src", "Root path for writing")
	initCmd.Flags().StringP("language", "l", "", "Programming language (required)")
	initCmd.MarkFlagRequired("language")
	initCmd.Flags().StringP("name", "n", "", "Project name (required)")
	initCmd.MarkFlagRequired("name")
}
