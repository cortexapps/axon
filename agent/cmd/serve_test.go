package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	config.WebhookServerPort = 0
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
		rr := snykbroker.RegistrationInfoResponse{
			ServerUri: "http://localhost:12345",
			Token:     "test-broker-token",
		}
		json, err := json.Marshal(rr)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(json)
	}))
	defer server.Close()

	os.Setenv("CORTEX_API_TOKEN", "xxxyyyzzz")
	os.Setenv("CORTEX_API_BASE_URL", server.URL)
	os.Setenv("PORT", "0")
	config := config.NewAgentEnvConfig()
	config.HttpServerPort = common.GetRandomPort()
	config.WebhookServerPort = common.GetRandomPort()
	stack := buildServeStack(&cobra.Command{}, config)

	require.NotNil(t, stack)

	app := fx.New(
		stack,
	)

	err := app.Start(context.Background())
	require.NoError(t, err)

	testAxonHealthcheck(t, config.HttpServerPort)
	testWebhook(t, config.WebhookServerPort)

	app.Stop(context.Background())

}

func testAxonHealthcheck(t *testing.T, port int) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/__axon/healthcheck", port))
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "{\"OK\":true}", string(body))
}

func testWebhook(t *testing.T, port int) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/webhook/12345", port))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
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
		"ACCEPTFILE_DIR":    "../server/snykbroker/accept_files",
		"GITHUB_TOKEN":      "the-token",
		"GITHUB_API":        "api.github.com",
		"GITHUB_GRAPHQL":    "api.github.com/graphql",
	}

	common.ApplyEnv(envVars)

	config := config.NewAgentEnvConfig()
	config.FailWaitTime = time.Millisecond
	config.HttpServerPort = common.GetRandomPort()
	config.WebhookServerPort = common.GetRandomPort()
	// Set a random port for the HTTP server
	stack := buildRelayStack(&cobra.Command{}, config, common.IntegrationInfo{
		Integration: common.IntegrationGithub,
	})

	require.NotNil(t, stack)

	app := fx.New(
		stack,
	)

	err := app.Start(context.Background())
	require.NoError(t, err)

	testAxonHealthcheck(t, config.HttpServerPort)
	app.Stop(context.Background())
}
