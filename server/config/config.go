package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultGrpcPort          = 50052
	DefaultHttpPort          = 8080
	DefaultHeartbeatInterval = 30 * time.Second
)

type Config struct {
	// GrpcPort is the port the gRPC tunnel server listens on.
	GrpcPort int
	// HttpPort is the port the HTTP dispatch server listens on.
	HttpPort int
	// BrokerServerURL is the base URL of the BROKER_SERVER HTTP API
	// for client-connected/deleted and server-connected/deleted notifications.
	BrokerServerURL string
	// JWTPublicKeyPath is the path to a PEM-encoded public key for
	// validating JWT tokens in ClientHello. Empty disables JWT validation.
	JWTPublicKeyPath string
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
}

func (c Config) Print() {
	fmt.Println("Server Configuration:")
	fmt.Printf("\tgRPC Port: %d\n", c.GrpcPort)
	fmt.Printf("\tHTTP Port: %d\n", c.HttpPort)
	fmt.Printf("\tBroker Server URL: %s\n", c.BrokerServerURL)
	fmt.Printf("\tServer ID: %s\n", c.ServerID)
	fmt.Printf("\tHeartbeat Interval: %v\n", c.HeartbeatInterval)
	fmt.Printf("\tDispatch Timeout: %v\n", c.DispatchTimeout)
	if c.JWTPublicKeyPath != "" {
		fmt.Printf("\tJWT Public Key: %s\n", c.JWTPublicKeyPath)
	} else {
		fmt.Println("\tJWT Validation: Disabled")
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

	cfg.BrokerServerURL = os.Getenv("BROKER_SERVER_URL")
	cfg.JWTPublicKeyPath = os.Getenv("JWT_PUBLIC_KEY_PATH")

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
