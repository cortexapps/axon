package cmd

import (
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The shared transport serves the Cortex API proxy, registration and the
// reflector. It must never cap connections per host (a cap queues instead of
// failing, so one leaking caller starves every other user of the host) and it
// must bound every phase of a request so a hung upstream cannot hold a
// connection forever.
func TestCreateHttpTransport_TimeoutsAndNoCap(t *testing.T) {
	tr := createHttpTransport(config.AgentConfig{UpstreamMaxConnsPerHost: 128}, zap.NewNop())

	require.Equal(t, 0, tr.MaxConnsPerHost, "shared transport must not cap connections per host")
	require.Equal(t, 16, tr.MaxIdleConnsPerHost)
	require.NotNil(t, tr.DialContext, "dial must have a timeout")
	require.Equal(t, 30*time.Second, tr.TLSHandshakeTimeout)
	require.Equal(t, 5*time.Minute, tr.ResponseHeaderTimeout)
	require.Equal(t, time.Second, tr.ExpectContinueTimeout)
	require.Equal(t, 90*time.Second, tr.IdleConnTimeout)
}
