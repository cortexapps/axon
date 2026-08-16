package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultGrpcPort           = 50052
	DefaultHttpPort           = 8080
	DefaultHeartbeatInterval  = 30 * time.Second
	DefaultMaxFrameBytes      = 1 << 20 // 1 MiB
	DefaultMaxStreamsPerToken = 64
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
}

func (c Config) Print() {
	fmt.Println("Server Configuration:")
	fmt.Printf("\tgRPC Port: %d\n", c.GrpcPort)
	fmt.Printf("\tHTTP Port: %d\n", c.HttpPort)
	fmt.Printf("\tBroker Server URL: %s\n", c.BrokerServerURL)
	fmt.Printf("\tServer ID: %s\n", c.ServerID)
	fmt.Printf("\tHeartbeat Interval: %v\n", c.HeartbeatInterval)
	fmt.Printf("\tDispatch Timeout: %v\n", c.DispatchTimeout)
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
	}

	if v := os.Getenv("MAX_FRAME_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Errorf("invalid MAX_FRAME_BYTES: %w", err))
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

func getServerID() string {
	if h := os.Getenv("HOSTNAME"); h != "" && h != "localhost" {
		return h
	}
	return uuid.New().String()
}
