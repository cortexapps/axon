package cmd

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLoadCerts(t *testing.T) {

	transport := createHttpTransport(config.AgentConfig{
		HttpCaCertFilePath: "../test/certs",
	}, zap.NewNop())
	require.NotNil(t, transport)
	require.NotNil(t, transport.TLSClientConfig.RootCAs)
}
