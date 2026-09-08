package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	DefaultGrpcPort          = 50052
	DefaultHttpPort          = 8080
	DefaultHeartbeatInterval = 30 * time.Second
	DefaultMaxFrameBytes     = 1 << 20 // 1 MiB
	// MaxAllowedFrameBytes bounds MAX_FRAME_BYTES. Agents allocate a buffer of
	// this size per in-flight call, so the value sets their memory ceiling;
	// it must also fit in the int32 the hello carries.
	MaxAllowedFrameBytes      = 4 << 20 // 4 MiB
	DefaultMaxStreamsPerToken = 64
	// DefaultBuildVersion is what an unstamped binary reports. The release
	// image sets AXON_BUILD_VERSION at build time; anything without it — a
	// local `go run`, a hand-built image — is honestly labeled "dev" rather
	// than claiming a version it does not have.
	DefaultBuildVersion = "dev"
)

type Config struct {
	// GrpcPort is the port the gRPC tunnel server listens on.
	GrpcPort int
	// GrpcTLSCertFile and GrpcTLSKeyFile enable TLS on the gRPC listener.
	// Both must be set, or the listener serves plaintext h2c.
	//
	// The server does not produce a certificate, only consume one — where it
	// comes from is a deployment decision. Behind a Google load balancer that
	// can be a throwaway self-signed cert, because the balancer requires TLS
	// to speak HTTP/2 to a backend but accepts any certificate an in-GCP
	// backend presents. Anywhere agents connect directly, it should be a real
	// one. Neither case needs different code here.
	GrpcTLSCertFile string
	GrpcTLSKeyFile  string
	// HttpPort is the port the HTTP dispatch server listens on.
	HttpPort int
	// BrokerServerURL is the base URL of the BROKER_SERVER HTTP API
	// for client-connected/deleted and server-connected/deleted notifications.
	BrokerServerURL string
	// HeartbeatInterval is how often the server sends heartbeat messages.
	// Clients must respond within 2x this interval.
	HeartbeatInterval time.Duration
	// DispatchTimeout is the maximum time to wait for a client response
	// to a dispatched HTTP request.
	DispatchTimeout time.Duration
	// ServerID identifies this server instance. Used in metrics and
	// returned to clients in ServerHello for dedup.
	ServerID string
	// ReRegistrationInterval is how often the server re-sends
	// client-connected notifications to BROKER_SERVER as a TTL refresh.
	ReRegistrationInterval time.Duration
	// MaxFrameBytes is the maximum CallData payload size, announced to
	// clients in ServerHello.
	MaxFrameBytes int
	// MaxStreamsPerToken caps concurrent tunnel streams per broker token
	// (defensive: a runaway agent can't stampede). Announced to clients in
	// ServerHello.max_streams; the (N+1)th stream is rejected with
	// ResourceExhausted. 0 means unlimited.
	MaxStreamsPerToken int
	// BuildVersion identifies the build this server is running, from
	// AXON_BUILD_VERSION (stamped into the image at build time), or "dev".
	// It is logged at startup and served from /healthcheck so a deployed
	// server can be matched to a release without shelling into the pod --
	// the same AXON_BUILD_VERSION contract the agent reports from
	// /api/v1/info.
	BuildVersion string
}

// Fields renders the configuration for logging. It goes through the logger
// rather than fmt so the startup config obeys the configured encoder — printing
// it directly would put plain text in the log stream of a server whose every
// other line is JSON.
func (c Config) Fields() []zap.Field {
	return []zap.Field{
		zap.Int("grpc_port", c.GrpcPort),
		zap.Int("http_port", c.HttpPort),
		zap.String("broker_server_url", c.BrokerServerURL),
		zap.String("server_id", c.ServerID),
		zap.Duration("heartbeat_interval", c.HeartbeatInterval),
		zap.Duration("dispatch_timeout", c.DispatchTimeout),
		zap.Duration("re_registration_interval", c.ReRegistrationInterval),
		zap.Int("max_frame_bytes", c.MaxFrameBytes),
		zap.Int("max_streams_per_token", c.MaxStreamsPerToken),
		zap.String("build_version", c.BuildVersion),
		// Whether the gRPC listener serves TLS decides whether GCLB can reach
		// it at all, so it belongs in the line you check first.
		zap.Bool("grpc_tls_enabled", c.GrpcTLSCertFile != ""),
		zap.String("grpc_tls_cert_file", c.GrpcTLSCertFile),
		zap.String("grpc_tls_key_file", c.GrpcTLSKeyFile),
	}
}

func NewConfigFromEnv() Config {
	cfg := Config{
		GrpcPort:               DefaultGrpcPort,
		HttpPort:               DefaultHttpPort,
		HeartbeatInterval:      DefaultHeartbeatInterval,
		DispatchTimeout:        60 * time.Second,
		ServerID:               getServerID(),
		ReRegistrationInterval: 5 * time.Minute,
		MaxFrameBytes:          DefaultMaxFrameBytes,
		MaxStreamsPerToken:     DefaultMaxStreamsPerToken,
		BuildVersion:           getBuildVersion(),
	}

	if v := os.Getenv("MAX_FRAME_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Errorf("invalid MAX_FRAME_BYTES: %w", err))
		}
		// The value is announced to every connected agent, which sizes a
		// per-call buffer from it, so a typo here is a fleet-wide memory
		// event rather than a local one. It also crosses the wire as an
		// int32, where anything larger would arrive wrapped and negative.
		if n <= 0 || n > MaxAllowedFrameBytes {
			panic(fmt.Errorf("MAX_FRAME_BYTES must be between 1 and %d, got %d", MaxAllowedFrameBytes, n))
		}
		cfg.MaxFrameBytes = n
	}

	if v := os.Getenv("MAX_STREAMS_PER_TOKEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Errorf("invalid MAX_STREAMS_PER_TOKEN: %w", err))
		}
		cfg.MaxStreamsPerToken = n
	}

	if v := os.Getenv("GRPC_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Errorf("invalid GRPC_PORT: %w", err))
		}
		cfg.GrpcPort = p
	}

	if v := os.Getenv("HTTP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Errorf("invalid HTTP_PORT: %w", err))
		}
		cfg.HttpPort = p
	}

	cfg.GrpcTLSCertFile = os.Getenv("GRPC_TLS_CERT_FILE")
	cfg.GrpcTLSKeyFile = os.Getenv("GRPC_TLS_KEY_FILE")
	if (cfg.GrpcTLSCertFile == "") != (cfg.GrpcTLSKeyFile == "") {
		// Half-configured TLS means someone intended encryption and will not
		// get it. Failing here beats silently serving plaintext.
		panic("GRPC_TLS_CERT_FILE and GRPC_TLS_KEY_FILE must be set together")
	}

	cfg.BrokerServerURL = os.Getenv("BROKER_SERVER_URL")

	if v := os.Getenv("HEARTBEAT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic(fmt.Errorf("invalid HEARTBEAT_INTERVAL: %w", err))
		}
		cfg.HeartbeatInterval = d
	}

	if v := os.Getenv("DISPATCH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic(fmt.Errorf("invalid DISPATCH_TIMEOUT: %w", err))
		}
		cfg.DispatchTimeout = d
	}

	if v := os.Getenv("RE_REGISTRATION_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic(fmt.Errorf("invalid RE_REGISTRATION_INTERVAL: %w", err))
		}
		cfg.ReRegistrationInterval = d
	}

	return cfg
}

// getBuildVersion mirrors the agent's read of the same variable
// (agent/server/http/axon_handler.go): an empty or unset value is "dev", so
// the field is never blank in a log line or a health response.
func getBuildVersion() string {
	if v := os.Getenv("AXON_BUILD_VERSION"); v != "" {
		return v
	}
	return DefaultBuildVersion
}

func getServerID() string {
	if h := os.Getenv("HOSTNAME"); h != "" && h != "localhost" {
		return h
	}
	return uuid.New().String()
}
