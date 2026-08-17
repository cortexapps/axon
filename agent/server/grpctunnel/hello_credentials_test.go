package grpctunnel

import (
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The handshake carries two credentials: broker_token, which authorizes the
// stream today, and cortex_api_token, which is there so the server can
// eventually authenticate the agent itself and drop the registration
// round-trip. Neither may ever reach a log.

// captureHello returns the ClientHello the agent actually sent.
func captureHello(t *testing.T, cfg config.AgentConfig, token string) *pb.ClientHello {
	t.Helper()

	var (
		mu   sync.Mutex
		seen *pb.ClientHello
	)
	svc := &fakeTunnelService{behavior: serverBehavior{
		serverID: "hello-server",
		onStream: func(stream pb.TunnelService_TunnelServer, hello *pb.ClientHello) error {
			mu.Lock()
			if seen == nil {
				seen = hello
			}
			mu.Unlock()
			for {
				if _, err := stream.Recv(); err != nil {
					return err
				}
			}
		},
	}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{token}})
	startClientWithEnv(t, tc, addr, token)
	defer tc.Close()

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen != nil
	})

	mu.Lock()
	defer mu.Unlock()
	return seen
}

func TestHello_CarriesCortexApiToken(t *testing.T) {
	hello := captureHello(t, config.AgentConfig{
		CortexApiToken:   "cortex-api-token-value",
		InstanceId:       "agent-1",
		IntegrationAlias: "test-alias",
	}, "broker-token-value")

	require.NotNil(t, hello)
	assert.Equal(t, "cortex-api-token-value", hello.CortexApiToken,
		"the server needs this to authenticate the agent directly later")
	assert.Equal(t, "broker-token-value", hello.BrokerToken,
		"and the broker token still authorizes the stream today")
}

// An agent handed a broker token directly never registers and has no API
// token. That must be an empty field, not a failure to connect.
func TestHello_OmitsCortexApiTokenWhenUnset(t *testing.T) {
	hello := captureHello(t, config.AgentConfig{InstanceId: "agent-2"}, "broker-token-value")

	require.NotNil(t, hello)
	assert.Empty(t, hello.CortexApiToken)
	assert.Equal(t, "broker-token-value", hello.BrokerToken)
}

// Both are live credentials. A log line carrying either turns every log sink
// into a place they leak, so this asserts on everything the client wrote while
// connecting rather than on any single call site.
func TestHello_CredentialsNeverReachTheLog(t *testing.T) {
	const (
		apiToken    = "SECRET-cortex-api-token"
		brokerToken = "SECRET-broker-token"
	)

	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "log-server"}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	tc, _ := newTestClient(t, config.AgentConfig{
		CortexApiToken: apiToken,
		InstanceId:     "agent-3",
	}, &fakeRegistration{serverURI: addr, tokens: []string{brokerToken}})

	core, logs := observer.New(zapcore.DebugLevel)
	tc.logger = zap.New(core)

	startClientWithEnv(t, tc, addr, brokerToken)
	defer tc.Close()

	waitFor(t, 5*time.Second, func() bool {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		return len(tc.streams) > 0
	})

	for _, entry := range logs.All() {
		line := entry.Message
		for k, v := range entry.ContextMap() {
			line += fmt.Sprintf(" %s=%v", k, v)
		}
		assert.NotContains(t, line, apiToken, "cortex_api_token leaked into a log line")
		assert.NotContains(t, line, brokerToken, "broker_token leaked into a log line")
	}
	assert.NotEmpty(t, logs.All(), "no log output captured, so the check proved nothing")
}
