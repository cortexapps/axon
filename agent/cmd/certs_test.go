package cmd

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLoadCerts(t *testing.T) {
	// This test is a placeholder for the actual implementation.
	// It should load certificates and verify their correctness.
	transport := createHttpTransport(config.AgentConfig{
		HttpCaCertFilePath: "../test/certs",
	}, zap.NewNop())
	require.NotNil(t, transport)
}
