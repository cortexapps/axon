package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The server consumes a certificate rather than producing one: where it comes
// from is a deployment decision. These pin the only rule it enforces — that
// the pair is all-or-nothing.

func TestGrpcTLS_OffByDefault(t *testing.T) {
	cfg := NewConfigFromEnv()

	assert.Empty(t, cfg.GrpcTLSCertFile)
	assert.Empty(t, cfg.GrpcTLSKeyFile)
}

func TestGrpcTLS_EnabledWhenBothSet(t *testing.T) {
	t.Setenv("GRPC_TLS_CERT_FILE", "/tls/tls.crt")
	t.Setenv("GRPC_TLS_KEY_FILE", "/tls/tls.key")

	cfg := NewConfigFromEnv()

	assert.Equal(t, "/tls/tls.crt", cfg.GrpcTLSCertFile)
	assert.Equal(t, "/tls/tls.key", cfg.GrpcTLSKeyFile)
}

// Half-configured TLS means someone intended encryption and would not get it.
// Failing loudly beats silently serving plaintext on a listener the operator
// believes is encrypted.
func TestGrpcTLS_PanicsOnHalfConfiguration(t *testing.T) {
	t.Run("cert without key", func(t *testing.T) {
		t.Setenv("GRPC_TLS_CERT_FILE", "/tls/tls.crt")
		assert.Panics(t, func() { NewConfigFromEnv() })
	})

	t.Run("key without cert", func(t *testing.T) {
		t.Setenv("GRPC_TLS_KEY_FILE", "/tls/tls.key")
		assert.Panics(t, func() { NewConfigFromEnv() })
	})
}
