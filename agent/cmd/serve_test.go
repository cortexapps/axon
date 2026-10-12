package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	config.HttpServerPort = common.GetRandomPort()
	config.WebhookServerPort = 0
	stack := buildServeStack(&cobra.Command{}, config)

	require.NotNil(t, stack)

	app := fx.New(
		stack,
	)

	err := app.Start(context.Background())
	require.NoError(t, err)

	// Without REMOTE_LOGS_VISIBLE the logs endpoint does not exist as far
	// as the backend is concerned: a 404, not an empty page.
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/__axon/logs", config.HttpServerPort))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	app.Stop(context.Background())
}

// With REMOTE_LOGS_VISIBLE set, the logger built by the fx stack tees into
// the store, so the startup lines fx itself logs are what the endpoint
// pages through. This runs without ENV=production, so stderr is the console
// encoder; the store must still hand back JSON lines.
func TestBuildServeStackRemoteLogs(t *testing.T) {
	oldEnv := util.SaveEnv(false)
	defer util.RestoreEnv(oldEnv)
	logDir := t.TempDir()
	os.Setenv("DRYRUN", "true")
	os.Setenv("PORT", "0")
	os.Setenv("REMOTE_LOGS_VISIBLE", "*")
	os.Setenv("REMOTE_LOGS_PATH", logDir)
	os.Unsetenv("ENV")
	config := config.NewAgentEnvConfig()
	config.HttpServerPort = common.GetRandomPort()
	config.WebhookServerPort = 0
	config.GrpcPort = common.GetRandomPort()

	app := fx.New(buildServeStack(&cobra.Command{}, config))
	require.NoError(t, app.Start(context.Background()))

	// now is one second ahead so the lines written during startup, in the
	// current second, fall inside the cutoff.
	url := fmt.Sprintf("http://localhost:%d/__axon/logs?size=5&now=%d", config.HttpServerPort, time.Now().Unix()+1)
	resp, err := http.Get(url)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var page struct {
		Count   int      `json:"count"`
		Lines   []string `json:"lines"`
		HasMore bool     `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(body, &page), string(body))
	require.Greater(t, page.Count, 0, "fx logs its own startup, so the store cannot be empty")
	require.Len(t, page.Lines, page.Count)
	for _, line := range page.Lines {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "not a JSON line: %s", line)
		require.Contains(t, entry, "ts")
		require.Contains(t, entry, "msg")
	}

	resp, err = http.Get(fmt.Sprintf("http://localhost:%d/__axon/info", config.HttpServerPort))
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var info map[string]any
	require.NoError(t, json.Unmarshal(body, &info), string(body))
	require.Equal(t, true, info["remote_logs"])

	app.Stop(context.Background())

	files, err := filepath.Glob(filepath.Join(logDir, "axon-*.log"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "segments must be under REMOTE_LOGS_PATH")
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

	// Parsed rather than string-compared: the body carries build_version
	// alongside OK, and pinning the exact bytes made adding a field a test
	// failure rather than a decision.
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed), "health body is not valid JSON: %s", string(body))
	require.Equal(t, true, parsed["OK"])
	require.NotEmpty(t, parsed["build_version"])
}

func testWebhook(t *testing.T, port int) {
	resp, err := http.Post(fmt.Sprintf("http://localhost:%d/webhook/12345", port), "application/json", nil)
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
	config.RelayIdleTimeout = 0 // Disable idle timeout to prevent restart during test
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
