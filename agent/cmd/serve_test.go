package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/cortexapps/axon/util"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
)

func buildServeStack(cmd *cobra.Command, config config.AgentConfig) fx.Option {

	return fx.Options(
		initStack(cmd, config, common.IntegrationInfo{}),
		AgentModule,
		cortexHttp.Module,
		snykbroker.Module,
	)
}

func TestBuildServeStack(t *testing.T) {
	oldEnv := util.SaveEnv(false)
	defer util.RestoreEnv(oldEnv)
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

	// create a fake server that serves http://localhost:xxx/relay/register
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))
	defer server.Close()

	os.Setenv("CORTEX_API_TOKEN", "xxxyyyzzz")
	os.Setenv("CORTEX_API_BASE_URL", server.URL)
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

	oldEnv := util.SaveEnv(false)
	defer util.RestoreEnv(oldEnv)

	envVars := map[string]string{
		"DRYRUN":            "true",
		"PORT":              "0",
		"BROKER_SERVER_URL": "http://broker.cortex.io",
		"BROKER_TOKEN":      "abcd1234",
		"SNYK_BROKER_PATH":  "bash",
		"ACCEPT_FILE_DIR":   "../server/snykbroker/accept_files",
		"GITHUB_TOKEN":      "the-token",
		"GITHUB_API":        "api.github.com",
		"GITHUB_GRAPHQL":    "api.github.com/graphql",
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
