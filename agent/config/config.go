package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/pflag"
)

const DefaultGrpcPort = 50051
const DefaultHttpPort = 80
const WebhookServerPort = 8081

type RelayReflectorMode int

const (
	RelayReflectorDisabled RelayReflectorMode = iota
	RelayReflectorRegistrationOnly
	RelayReflectorAllTraffic
)

func (m RelayReflectorMode) String() string {
	switch m {
	case RelayReflectorDisabled:
		return "Disabled"
	case RelayReflectorRegistrationOnly:
		return "RegistrationOnly"
	case RelayReflectorAllTraffic:
		return "AllTraffic"
	default:
		return "Unknown"
	}
}

type AgentConfig struct {
	GrpcPort              int
	CortexApiBaseUrl      string
	CortexApiToken        string
	DryRun                bool
	DequeueWaitTime       time.Duration
	InstanceId            string
	Integration           string
	IntegrationAlias      string
	HttpServerPort        int
	WebhookServerPort     int
	SnykBrokerPort        int
	EnableApiProxy        bool
	FailWaitTime          time.Duration
	AutoRegisterFrequency time.Duration
	VerboseOutput         bool
	PluginDirs            []string

	HandlerHistoryPath         string
	HandlerHistoryMaxAge       time.Duration
	HandlerHistoryMaxSizeBytes int64

	HttpDisableTLS         bool
	HttpCaCertFilePath     string
	HttpRelayReflectorMode RelayReflectorMode
}

func (ac AgentConfig) HttpBaseUrl() string {
	return fmt.Sprintf("http://localhost:%d", ac.HttpServerPort)
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

	if id := os.Getenv("HOSTNAME"); id != "" && id != "localhost" {
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

	snykBrokerPort := 0
	if snykBrokerPortStr := os.Getenv("SNYK_BROKER_PORT"); snykBrokerPortStr != "" {
		p, err := strconv.Atoi(snykBrokerPortStr)
		if err != nil {
			panic(err)
		}
		snykBrokerPort = p
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
	if historyPathEnv := os.Getenv("HANDLER_HISTORY_PATH"); historyPathEnv != "" {
		historyPath = historyPathEnv
	}

	handlerHistoryMaxAge := time.Hour * 24 * 7
	if handlerHistoryMaxAgeEnv := os.Getenv("HANDLER_HISTORY_MAX_AGE"); handlerHistoryMaxAgeEnv != "" {
		hma, err := time.ParseDuration(handlerHistoryMaxAgeEnv)
		if err != nil {
			panic(err)
		}
		handlerHistoryMaxAge = hma
	}
	handlerHistoryMaxSizeBytes := int64(1024 * 1024 * 1024) // 1GB
	if handlerHistoryMaxSizeBytesEnv := os.Getenv("HANDLER_HISTORY_MAX_SIZE_BYTES"); handlerHistoryMaxSizeBytesEnv != "" {
		hma, err := strconv.ParseInt(handlerHistoryMaxSizeBytesEnv, 10, 64)
		if err != nil {
			panic(err)
		}
		handlerHistoryMaxSizeBytes = hma
	}

	identifier := os.Getenv("INTEGRATION_ALIAS")
	if identifier == "" {
		identifier = "custom-agent"
	}

	token := os.Getenv("CORTEX_API_TOKEN")

	if dryRun {
		token = "dry-run"
	}

	reregisterFrequency := time.Minute * 5
	if reregisterFrequencyEnv := os.Getenv("AUTO_REGISTER_FREQUENCY"); reregisterFrequencyEnv != "" {
		var err error
		reregisterFrequency, err = time.ParseDuration(reregisterFrequencyEnv)
		if err != nil {
			panic(err)
		}
	}

	cfg := AgentConfig{
		GrpcPort:                   port,
		CortexApiBaseUrl:           baseUrl,
		CortexApiToken:             token,
		DryRun:                     dryRun,
		DequeueWaitTime:            dequeueWaitTime,
		InstanceId:                 getInstanceId(),
		IntegrationAlias:           identifier,
		HttpServerPort:             httpPort,
		WebhookServerPort:          WebhookServerPort,
		SnykBrokerPort:             snykBrokerPort,
		EnableApiProxy:             true,
		FailWaitTime:               time.Second * 2,
		PluginDirs:                 []string{"./plugins"},
		AutoRegisterFrequency:      reregisterFrequency,
		HandlerHistoryPath:         historyPath,
		HandlerHistoryMaxAge:       handlerHistoryMaxAge,
		HandlerHistoryMaxSizeBytes: handlerHistoryMaxSizeBytes,
	}

	if builtinPluginDir := os.Getenv("BUILTIN_PLUGIN_DIR"); builtinPluginDir != "" {
		cfg.PluginDirs = append(cfg.PluginDirs, filepath.Clean(builtinPluginDir))
	}

	if pluginDirsEnv := os.Getenv("PLUGIN_DIRS"); pluginDirsEnv != "" {

		pluginDirs := filepath.SplitList(pluginDirsEnv)

		for _, dir := range pluginDirs {
			cfg.PluginDirs = append(cfg.PluginDirs, filepath.Clean(dir))
		}
	}

	if DisableTLS := os.Getenv("DISABLE_TLS"); DisableTLS == "true" {
		cfg.HttpDisableTLS = true
	}

	if caCertFilePath := os.Getenv("CA_CERT_PATH"); caCertFilePath != "" {
		cfg.HttpCaCertFilePath = caCertFilePath
		cfg.HttpCaCertFilePath = filepath.Clean(cfg.HttpCaCertFilePath)
	}

	cfg.HttpRelayReflectorMode = RelayReflectorAllTraffic
	if relayReflector := os.Getenv("ENABLE_RELAY_REFLECTOR"); relayReflector != "" {
		switch relayReflector {
		case "false", "disabled":
			cfg.HttpRelayReflectorMode = RelayReflectorDisabled
		case "registration":
			cfg.HttpRelayReflectorMode = RelayReflectorRegistrationOnly
		case "true", "all":
			cfg.HttpRelayReflectorMode = RelayReflectorAllTraffic
		}
	}

	return cfg
}

func (ac AgentConfig) ApplyFlags(flags *pflag.FlagSet) AgentConfig {
	if enabled, _ := flags.GetBool("verbose"); enabled {
		ac.VerboseOutput = true
	}
	return ac
}
