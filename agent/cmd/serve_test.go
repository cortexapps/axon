package cmd

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
)

func TestBuildServeStack(t *testing.T) {

	os.Setenv("DRYRUN", "true")
	os.Setenv("PORT", "0")
	config := config.NewAgentEnvConfig()
	config.HttpServerPort = 0
	stack := buildServeStack(&cobra.Command{}, config)

	require.NotNil(t, stack)

	app := fx.New(
		stack,
	)

	err := app.Start(context.Background())
	require.NoError(t, err)
	app.Stop(context.Background())
}

func TestBuildServeStackLive(t *testing.T) {

	os.Setenv("CORTEX_API_TOKEN", "xxxyyyzzz")
	os.Setenv("PORT", "0")
	config := config.NewAgentEnvConfig()
	config.HttpServerPort = 0
	stack := buildServeStack(&cobra.Command{}, config)

	require.NotNil(t, stack)

	app := fx.New(
		stack,
	)

	err := app.Start(context.Background())
	require.NoError(t, err)
	app.Stop(context.Background())
}

func TestBuildRelayStack(t *testing.T) {

	envVars := map[string]string{
		"DRYRUN":              "true",
		"PORT":                "0",
		"BROKER_SERVER_URL":   "http://broker.cortex.io",
		"BROKER_TOKEN":        "abcd1234",
		"SNYK_BROKER_PATH":    "bash",
		"ACCEPT_FILE_DIR":     "../server/snykbroker/accept_files",
		"GITHUB_TOKEN":        "the-token",
		"GITHUB_API_ROOT":     "api.github.com",
		"GITHUB_GRAPHQL_ROOT": "api.github.com/graphql",
	}

	common.ApplyEnv(envVars)

	config := config.NewAgentEnvConfig()
	config.FailWaitTime = time.Millisecond
	config.HttpServerPort = 0
	stack := buildRelayStack(&cobra.Command{}, config, common.IntegrationInfo{
		Integration: common.IntegrationGithub,
	})

	require.NotNil(t, stack)

	app := fx.New(
		stack,
	)

	err := app.Start(context.Background())
	require.NoError(t, err)
	app.Stop(context.Background())
}
