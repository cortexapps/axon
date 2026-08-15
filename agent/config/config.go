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

// RelayReflectorMode controls how the reflector proxy routes traffic.
// The reflector intercepts HTTP requests to handle CA certs and add custom headers.
const (
	RelayReflectorDisabled         RelayReflectorMode = iota // No reflector - all traffic goes direct
	RelayReflectorRegistrationOnly                           // Only broker server URL goes through reflector
	RelayReflectorAllTraffic                                 // Both broker URL and accept file origins go through reflector
	RelayReflectorTrafficOnly                                // Only accept file origins go through reflector (DEFAULT)
)

func (m RelayReflectorMode) String() string {
	switch m {
	case RelayReflectorDisabled:
		return "Disabled"
	case RelayReflectorRegistrationOnly:
		return "RegistrationOnly"
	case RelayReflectorAllTraffic:
		return "AllTraffic"
	case RelayReflectorTrafficOnly:
		return "TrafficOnly"
	default:
		return "Unknown"
	}
}

// ReflectsRegistration returns true if this mode routes the broker server URL through the reflector.
// Modes: registration, all
func (m RelayReflectorMode) ReflectsRegistration() bool {
	return m == RelayReflectorRegistrationOnly || m == RelayReflectorAllTraffic
}

// ReflectsTraffic returns true if this mode routes accept file origins through the reflector.
// Modes: traffic, all
func (m RelayReflectorMode) ReflectsTraffic() bool {
	return m == RelayReflectorTrafficOnly || m == RelayReflectorAllTraffic
}

// IsEnabled returns true if this mode enables the reflector at all.
// Modes: registration, traffic, all
func (m RelayReflectorMode) IsEnabled() bool {
	return m != RelayReflectorDisabled
}

type RelayMode string

const (
	RelayModeSnykBroker RelayMode = "snyk-broker"
	RelayModeGrpcTunnel RelayMode = "grpc-tunnel"
)

// TunnelConnMode selects how tunnel streams map onto TCP connections.
type TunnelConnMode string

const (
	// TunnelConnModePool is the adaptive watermark pool: one connection per
	// stream, count varying between MinTunnelSlots and MaxTunnelSlots.
	TunnelConnModePool TunnelConnMode = "pool"
	// TunnelConnModeConns keeps a fixed TunnelConns connections, one stream each.
	TunnelConnModeConns TunnelConnMode = "conns"
	// TunnelConnModeMux keeps a fixed TunnelConns connections and multiplexes
	// TunnelStreamsPerConn streams over each.
	TunnelConnModeMux TunnelConnMode = "mux"
	// TunnelConnModeDirect keeps a fixed TunnelConns connections — a static
	// resilience decision, load-balanced round-robin across server instances
	// — and opens streams on demand, keeping TunnelIdleStreams of them
	// waiting so a burst never pays for a stream open. One call per stream,
	// up to TunnelMaxStreams.
	//
	// There is deliberately no traffic shaping here. Concurrency is whatever
	// the agent can actually serve: when it falls behind, idle streams run
	// out, the server's dispatch blocks briefly, and the delay propagates to
	// the caller as latency. That stream cap is what makes the backpressure
	// real rather than a queue hidden in the agent's heap.
	TunnelConnModeDirect TunnelConnMode = "direct"
)

// IsFixed reports whether the mode uses a fixed number of streams — i.e. one
// that neither grows on demand nor retires on idle.
func (m TunnelConnMode) IsFixed() bool {
	return m == TunnelConnModeConns || m == TunnelConnModeMux
}

// IsDirect reports whether the mode keeps a fixed connection set with
// on-demand streams over it.
func (m TunnelConnMode) IsDirect() bool {
	return m == TunnelConnModeDirect
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
	EnablePprof           bool
	FailWaitTime          time.Duration
	AutoRegisterFrequency time.Duration
	VerboseOutput         bool
	PluginDirs            []string

	HandlerHistoryPath         string
	HandlerHistoryMaxAge       time.Duration
	HandlerHistoryMaxSizeBytes int64

	HttpDisableTLS            bool
	HttpCaCertFilePath        string
	HttpRelayReflectorMode    RelayReflectorMode
	ReflectorWebSocketUpgrade bool
	RelayIdleTimeout          time.Duration

	// RelayMode selects the tunnel implementation: "snyk-broker" or "grpc-tunnel".
	RelayMode string
	// MinTunnelSlots is the number of tunnel streams an idle agent keeps
	// open (grpc-tunnel mode only). The pool grows toward MaxTunnelSlots
	// under load (watermark model) and shrinks back to this floor.
	MinTunnelSlots int
	// MaxTunnelSlots is the ceiling on concurrent tunnel streams. Clamped
	// at runtime to the server-announced per-token cap.
	MaxTunnelSlots int
	// SlotIdleTimeout is how long a slot above MinTunnelSlots must sit
	// without carrying a call before it retires.
	SlotIdleTimeout time.Duration
	// MaxStreamsPerServer caps how many streams may land on the same server_id.
	// Default 2; 0 means unlimited. Replaces strict server-id dedup so a small
	// server pool still gets independent-TCP redundancy. In "mux" conn mode the
	// cap counts connections rather than streams, since streams share them.
	MaxStreamsPerServer int
	// TunnelConnMode selects how tunnel streams map onto TCP connections:
	//
	//	pool  (default) adaptive watermark pool, one connection per stream
	//	conns fixed TunnelConns connections, one stream each
	//	mux   fixed TunnelConns connections, TunnelStreamsPerConn streams
	//	      multiplexed over each by HTTP/2
	//
	// "conns" and "mux" are fixed-size: they neither grow under load nor
	// retire on idle, which is what makes them comparable under load test.
	TunnelConnMode TunnelConnMode
	// TunnelConns is the connection count for the "conns" and "mux" modes.
	TunnelConns int
	// UpstreamMaxConnsPerHost bounds concurrent connections the agent opens
	// to any single upstream host, and sets the idle-connection pool size to
	// match so concurrent calls reuse connections instead of handshaking.
	UpstreamMaxConnsPerHost int
	// TunnelIdleStreams is how many idle streams the "direct" mode keeps
	// waiting for work. It is a latency knob, not a capacity one: it sets how
	// large a burst is absorbed without waiting on a stream open.
	TunnelIdleStreams int
	// TunnelMaxStreams caps concurrent streams in "direct" mode, which caps
	// concurrent calls. This is the safety backstop, and also the mechanism
	// by which an overloaded agent pushes back instead of accepting work it
	// cannot do.
	TunnelMaxStreams int
	// TunnelStreamsPerConn is how many streams share each connection in "mux"
	// mode. Ignored by the other modes.
	TunnelStreamsPerConn int
	// MaxInflightRequests caps concurrent in-flight requests dispatched into the
	// agent across all streams. Requests over the cap queue until capacity
	// frees or their deadline expires (then fail with a 503-coded cancel).
	MaxInflightRequests int
	// MaxRequestTimeout is the absolute ceiling on any single request, even when
	// the server provides TimeoutMs=0. Prevents goroutine accumulation during
	// slow-loris downstream incidents.
	MaxRequestTimeout time.Duration
	// RegistrationRefreshInterval controls how often the agent re-registers with
	// the Cortex API to pick up rotated tokens. 0 disables periodic refresh.
	RegistrationRefreshInterval time.Duration
	// GrpcInsecure disables TLS on the gRPC tunnel connection (separate from HttpDisableTLS).
	GrpcInsecure bool
	// GrpcTunnelServer is the address of the gRPC tunnel server (host:port).
	GrpcTunnelServer string
}

func (ac AgentConfig) HttpBaseUrl() string {
	return fmt.Sprintf("http://localhost:%d", ac.HttpServerPort)
}

// IsGRPCTunnel returns true if the relay mode is grpc-tunnel.
func (ac AgentConfig) IsGRPCTunnel() bool {
	return ac.RelayMode == "grpc-tunnel"
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
	if ac.EnablePprof {
		fmt.Println("\tpprof: Enabled at /pprof")
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

// envFirst returns the first non-empty value among the given environment
// variable names.
func envFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
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

	enablePprof := false
	if pprofEnv := os.Getenv("ENABLE_PPROF"); pprofEnv != "" {
		enablePprof = pprofEnv == "true" || pprofEnv == "1"
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
		EnablePprof:                enablePprof,
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

	cfg.HttpRelayReflectorMode = RelayReflectorAllTraffic // Default to "all"
	if relayReflector := os.Getenv("ENABLE_RELAY_REFLECTOR"); relayReflector != "" {
		switch relayReflector {
		case "false", "disabled":
			cfg.HttpRelayReflectorMode = RelayReflectorDisabled
		case "registration":
			cfg.HttpRelayReflectorMode = RelayReflectorRegistrationOnly
		case "true", "all":
			cfg.HttpRelayReflectorMode = RelayReflectorAllTraffic
		case "traffic":
			cfg.HttpRelayReflectorMode = RelayReflectorTrafficOnly
		}
	}

	// Default to true - WebSocket upgrade support in reflector
	cfg.ReflectorWebSocketUpgrade = true
	if wsUpgrade := os.Getenv("REFLECTOR_WEBSOCKET_UPGRADE"); wsUpgrade == "false" {
		cfg.ReflectorWebSocketUpgrade = false
	}

	cfg.RelayIdleTimeout = 10 * time.Minute
	if relayIdleTimeout := os.Getenv("RELAY_IDLE_TIMEOUT"); relayIdleTimeout != "" {
		rit, err := time.ParseDuration(relayIdleTimeout)
		if err != nil {
			panic(err)
		}
		cfg.RelayIdleTimeout = rit
	}

	// AXON_RELAY_TRANSPORT is the documented name (design doc §10);
	// RELAY_MODE is accepted as an alias.
	cfg.RelayMode = "snyk-broker"
	if relayMode := envFirst("AXON_RELAY_TRANSPORT", "RELAY_MODE"); relayMode != "" {
		cfg.RelayMode = relayMode
	}

	cfg.MinTunnelSlots = 4
	if v := os.Getenv("AXON_GRPC_TUNNEL_MIN_SLOTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		cfg.MinTunnelSlots = n
	}

	cfg.MaxTunnelSlots = 32
	if v := os.Getenv("AXON_GRPC_TUNNEL_MAX_SLOTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		cfg.MaxTunnelSlots = n
	}
	if cfg.MaxTunnelSlots < cfg.MinTunnelSlots {
		cfg.MaxTunnelSlots = cfg.MinTunnelSlots
	}

	cfg.SlotIdleTimeout = 10 * time.Minute
	if v := os.Getenv("AXON_GRPC_TUNNEL_SLOT_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic(err)
		}
		cfg.SlotIdleTimeout = d
	}

	if grpcInsecure := envFirst("AXON_GRPC_TUNNEL_INSECURE", "GRPC_INSECURE"); grpcInsecure == "true" {
		cfg.GrpcInsecure = true
	}

	cfg.GrpcTunnelServer = os.Getenv("GRPC_TUNNEL_SERVER")

	cfg.MaxStreamsPerServer = 2
	if maxStreamsPerServer := os.Getenv("MAX_STREAMS_PER_SERVER"); maxStreamsPerServer != "" {
		v, err := strconv.Atoi(maxStreamsPerServer)
		if err != nil {
			panic(err)
		}
		cfg.MaxStreamsPerServer = v
	}

	cfg.TunnelConnMode = TunnelConnModePool
	if v := os.Getenv("AXON_GRPC_TUNNEL_CONN_MODE"); v != "" {
		mode := TunnelConnMode(v)
		switch mode {
		case TunnelConnModePool, TunnelConnModeConns, TunnelConnModeMux, TunnelConnModeDirect:
			cfg.TunnelConnMode = mode
		default:
			panic(fmt.Sprintf("invalid AXON_GRPC_TUNNEL_CONN_MODE %q (want pool, conns, mux or direct)", v))
		}
	}

	cfg.UpstreamMaxConnsPerHost = 128
	if v := os.Getenv("AXON_UPSTREAM_MAX_CONNS_PER_HOST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		if n < 1 {
			panic("AXON_UPSTREAM_MAX_CONNS_PER_HOST must be >= 1")
		}
		cfg.UpstreamMaxConnsPerHost = n
	}

	cfg.TunnelIdleStreams = 4
	if v := os.Getenv("AXON_GRPC_TUNNEL_IDLE_STREAMS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		if n < 1 {
			panic("AXON_GRPC_TUNNEL_IDLE_STREAMS must be >= 1")
		}
		cfg.TunnelIdleStreams = n
	}

	cfg.TunnelMaxStreams = 256
	if v := os.Getenv("AXON_GRPC_TUNNEL_MAX_STREAMS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		if n < 1 {
			panic("AXON_GRPC_TUNNEL_MAX_STREAMS must be >= 1")
		}
		cfg.TunnelMaxStreams = n
	}
	if cfg.TunnelMaxStreams < cfg.TunnelIdleStreams {
		cfg.TunnelMaxStreams = cfg.TunnelIdleStreams
	}

	cfg.TunnelConns = 8
	if v := os.Getenv("AXON_GRPC_TUNNEL_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		if n < 1 {
			panic("AXON_GRPC_TUNNEL_CONNS must be >= 1")
		}
		cfg.TunnelConns = n
	}

	cfg.TunnelStreamsPerConn = 8
	if v := os.Getenv("AXON_GRPC_TUNNEL_STREAMS_PER_CONN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		if n < 1 {
			panic("AXON_GRPC_TUNNEL_STREAMS_PER_CONN must be >= 1")
		}
		cfg.TunnelStreamsPerConn = n
	}

	cfg.MaxInflightRequests = 256
	if maxInflight := os.Getenv("MAX_INFLIGHT_REQUESTS"); maxInflight != "" {
		v, err := strconv.Atoi(maxInflight)
		if err != nil {
			panic(err)
		}
		cfg.MaxInflightRequests = v
	}

	cfg.MaxRequestTimeout = 5 * time.Minute
	if maxReqTimeout := envFirst("AXON_GRPC_TUNNEL_MAX_REQUEST_TIMEOUT", "MAX_REQUEST_TIMEOUT"); maxReqTimeout != "" {
		v, err := time.ParseDuration(maxReqTimeout)
		if err != nil {
			panic(err)
		}
		cfg.MaxRequestTimeout = v
	}

	cfg.RegistrationRefreshInterval = 12 * time.Hour
	if refresh := os.Getenv("REGISTRATION_REFRESH_INTERVAL"); refresh != "" {
		v, err := time.ParseDuration(refresh)
		if err != nil {
			panic(err)
		}
		cfg.RegistrationRefreshInterval = v
	}

	return cfg
}

func (ac AgentConfig) ApplyFlags(flags *pflag.FlagSet) AgentConfig {
	if enabled, _ := flags.GetBool("verbose"); enabled {
		ac.VerboseOutput = true
	}
	return ac
}
