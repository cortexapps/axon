package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const DefaultGrpcPort = 50051
const DefaultHttpPort = 80
const WebhookServerPort = 8081

type AgentConfig struct {
	GrpcPort          int
	CortexApiBaseUrl  string
	CortexApiToken    string
	DryRun            bool
	DequeueWaitTime   time.Duration
	HistoryPath       string
	InstanceId        string
	Integration       string
	IntegrationAlias  string
	HttpServerPort    int
	WebhookServerPort int
	EnableApiProxy    bool
	FailWaitTime      time.Duration
	VerboseOutput     bool
}

func (ac AgentConfig) Print() {
	fmt.Println("Agent Configuration:")
	if ac.GrpcPort != DefaultGrpcPort {
		fmt.Println("\tGrpcPort: ", ac.GrpcPort)
	}
	fmt.Println("\tCortex API Base URL: ", ac.CortexApiBaseUrl)
	if ac.CortexApiToken != "" {
		fmt.Printf("\tCortex API Token: %s...%s\n", ac.CortexApiToken[0:5], ac.CortexApiToken[len(ac.CortexApiToken)-5:])
	}
	if ac.DryRun {
		fmt.Println("\tDry Run: Enabled")
	}
	if ac.EnableApiProxy {
		fmt.Println("\tAPI Port: ", ac.HttpServerPort)
	} else {
		fmt.Println("\tAPI Proxy: Disabled")
	}

	fmt.Printf("\tFast fail time: %v\n", ac.FailWaitTime)
}

var instancePath = filepath.Join(os.TempDir(), "instance-id")

func getInstanceId() string {

	if id := os.Getenv("CORTEX_INSTANCE_ID"); id != "" {
		return id
	}

	if _, err := os.Stat(instancePath); os.IsNotExist(err) {
		id := uuid.New().String()
		err := os.WriteFile(instancePath, []byte(id), 0644)
		if err != nil {
			panic(fmt.Errorf("error writing instance id: %v", err))
		}
	}

	id, err := os.ReadFile(instancePath)

	if err != nil {
		panic(fmt.Errorf("error reading instance id: %v", err))
	}
	return string(id)
}

func NewAgentEnvConfig() AgentConfig {

	baseUrl := os.Getenv("CORTEX_API_BASE_URL")
	if baseUrl == "" {
		baseUrl = "https://api.getcortexapp.com"
	}

	port := DefaultGrpcPort
	if portStr := os.Getenv("PORT"); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			panic(err)
		}
		port = p
	}

	httpPort := DefaultHttpPort
	if httpPortStr := os.Getenv("HTTP_PORT"); httpPortStr != "" {
		p, err := strconv.Atoi(httpPortStr)
		if err != nil {
			panic(err)
		}
		httpPort = p
	}

	dryRun := false
	if dryRunEnv := os.Getenv("DRYRUN"); dryRunEnv != "" {
		dryRun = dryRunEnv == "true" || dryRunEnv == "1"
	}

	dequeueWaitTime := 1 * time.Second
	if dequeueWaitTimeEnv := os.Getenv("DEQUEUE_WAIT_TIME"); dequeueWaitTimeEnv != "" {
		dwt, err := time.ParseDuration(dequeueWaitTimeEnv)
		if err != nil {
			panic(err)
		}
		dequeueWaitTime = dwt
	}

	historyPath := "/tmp/axon-agent/history"
	if historyPathEnv := os.Getenv("HISTORY_PATH"); historyPathEnv != "" {
		historyPath = historyPathEnv
	}

	identifier := os.Getenv("INTEGRATION_ALIAS")
	if identifier == "" {
		identifier = "custom-agent"
	}

	token := os.Getenv("CORTEX_API_TOKEN")

	if dryRun {
		token = "dry-run"
	}

	cfg := AgentConfig{
		GrpcPort:          port,
		CortexApiBaseUrl:  baseUrl,
		CortexApiToken:    token,
		DryRun:            dryRun,
		DequeueWaitTime:   dequeueWaitTime,
		HistoryPath:       historyPath,
		InstanceId:        getInstanceId(),
		IntegrationAlias:  identifier,
		HttpServerPort:    httpPort,
		WebhookServerPort: WebhookServerPort,
		EnableApiProxy:    true,
		FailWaitTime:      time.Second * 2,
	}
	return cfg
}
