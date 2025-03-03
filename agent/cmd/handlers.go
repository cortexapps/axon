package cmd

import (
	"fmt"
	"log"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/util"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// handlers represents the handlers command which lists handlers
//
// usage
// axon handlers list
// axon handlers history <handler-name>

var handlersRootCmd = &cobra.Command{
	Use:   "handlers",
	Short: "Interact with handlers",
}

var handlersList = &cobra.Command{
	Use:   "list",
	Short: "list handlers",
	Run: func(cmd *cobra.Command, args []string) {
		client := buildClient(config.DefaultGrpcPort)

		if len(args) == 0 {

			handlers, err := client.ListHandlers(cmd.Context(), &pb.ListHandlersRequest{})
			if err != nil {
				panic(err)
			}
			for _, handler := range handlers.Handlers {

				fmt.Printf("Name: %s\n", handler.Name)
				fmt.Printf("  ID: %s\n", handler.Id)
				fmt.Printf("  Dispatch ID: %s\n", handler.DispatchId)
				fmt.Printf("  Options: %v\n", handler.Options)
				fmt.Printf("  Active: %v\n", handler.IsActive)
				if handler.LastInvokedClientTimestamp != nil {
					fmt.Printf("  Last Invoked: %s\n\n", util.TimeToString(handler.LastInvokedClientTimestamp.AsTime()))

				}
				fmt.Println()
			}
			return
		}
	},
}

var handlersHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "get handler history",
	Run: func(cmd *cobra.Command, args []string) {

		if len(args) == 0 || args[0] == "" {
			log.Fatalf("handler name is required")
		}
		handlerName := args[0]

		client := buildClient(config.DefaultGrpcPort)
		logs := false
		if ok, _ := cmd.Flags().GetBool("logs"); ok {
			logs = true
		}

		tail, _ := cmd.Flags().GetInt("tail")

		if tail < 0 {
			log.Fatalf("tail must be a positive integer")
		}

		history, err := client.GetHandlerHistory(cmd.Context(), &pb.GetHandlerHistoryRequest{
			HandlerName: handlerName,
			IncludeLogs: logs,
			Tail:        int32(tail),
		})
		if err != nil {
			panic(err)
		}
		for _, execution := range history.History {

			fmt.Printf("Handler: %s\n", execution.HandlerName)
			fmt.Printf("  Dispatch ID: %s\n", execution.DispatchId)
			fmt.Printf("  Invocation ID: %s\n", execution.InvocationId)
			fmt.Printf("  Timestamp: %s\n", execution.StartClientTimestamp.AsTime().Format(time.RFC3339))
			fmt.Printf("  Duration: %dms\n", execution.DurationMs)
			if execution.Error != nil {
				fmt.Printf("  Error: %+v\n\n", execution.Error)
			}
			if logs {
				for _, logLine := range execution.Logs {
					fmt.Printf("  %s\t%s\t%s\n", logLine.Timestamp.AsTime().Format(time.RFC3339), logLine.Level, logLine.Message)
				}
			}
		}

	},
}

var handlersLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "get handler logs",
	Run: func(cmd *cobra.Command, args []string) {

		if len(args) == 0 || args[0] == "" {
			log.Fatalf("handler name is required")
		}
		handlerName := args[0]

		client := buildClient(config.DefaultGrpcPort)

		tail, _ := cmd.Flags().GetInt("tail")

		if tail < 0 {
			log.Fatalf("tail must be a positive integer")
		}

		history, err := client.GetHandlerHistory(cmd.Context(), &pb.GetHandlerHistoryRequest{
			HandlerName: handlerName,
			IncludeLogs: true,
			Tail:        int32(tail),
		})
		if err != nil {
			panic(err)
		}
		for _, execution := range history.History {
			for _, logLine := range execution.Logs {
				fmt.Printf("%s\t%s\t%s\n", logLine.Timestamp.AsTime().Format(time.RFC3339), logLine.Level, logLine.Message)
			}
		}
	},
}

func init() {
	handlersRootCmd.AddCommand(handlersList)

	handlersRootCmd.AddCommand(handlersHistoryCmd)
	handlersHistoryCmd.Flags().IntP("tail", "t", 0, "Show last N executions")
	handlersHistoryCmd.Flags().BoolP("logs", "l", false, "Include logs")

	handlersRootCmd.AddCommand(handlersLogsCmd)
	handlersLogsCmd.Flags().IntP("tail", "t", 0, "Show last N executions")

}

func buildClient(port int) pb.AxonAgentClient {

	conn, err := grpc.NewClient(
		fmt.Sprintf("localhost:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()))

	if err != nil {
		panic(err)
	}

	stub := pb.NewAxonAgentClient(conn)
	return stub
}
