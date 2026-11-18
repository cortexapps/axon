package tunnel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeNotifier stands in for BROKER_SERVER so delivery failures can be driven
// without a server. onAttempt runs before the verdict, for tests that need the
// world to change mid-retry.
type fakeNotifier struct {
	mu        sync.Mutex
	attempts  int
	failFirst int
	onAttempt func()
}

func (f *fakeNotifier) ClientConnected(ctx context.Context, token broker.Token, clientID string, metadata map[string]string) error {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.mu.Unlock()
	if f.onAttempt != nil {
		f.onAttempt()
	}
	if n <= f.failFirst {
		return errors.New("broker server unavailable")
	}
	return nil
}

func (f *fakeNotifier) ClientDisconnected(ctx context.Context, token broker.Token, clientID string) error {
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func newNotifyService(t *testing.T, notifier brokerNotifier) (*Service, *ClientRegistry) {
	t.Helper()
	registry := NewClientRegistry(zap.NewNop())
	svc := NewService(config.Config{ServerID: "test-server"}, zap.NewNop(), registry, notifier, metrics.New("test-server"))
	// Real backoff starts at 5s; the retry behaviour is what is under test, not
	// the wall-clock spacing.
	svc.notifyBackoff = time.Millisecond
	svc.notifyMaxBackoff = 2 * time.Millisecond
	return svc, registry
}

// The notification has to survive a BROKER_SERVER that is briefly down. It is
// now sent on the 0->1 connection edge, so there is no later stream to carry a
// retry: the loop itself must persist until the registration actually lands.
func TestNotifyClientConnected_LoopsUntilDelivered(t *testing.T) {
	notifier := &fakeNotifier{failFirst: 2}
	svc, registry := newNotifyService(t, notifier)
	token := broker.NewToken("token-abc")

	_, err := registry.Register(token, testIdentity("tenant-1"), testStream("s1"))
	require.NoError(t, err)

	svc.notifyClientConnected(token, "agent-7", "v1.2.3")

	require.Equal(t, 3, notifier.count(), "should retry until the delivery succeeds")
	require.True(t, registry.IsBrokerServerRegistered(token))
}

// "Registration already complete" is an exit condition, so a redundant call
// costs nothing.
func TestNotifyClientConnected_ReturnsWhenAlreadyRegistered(t *testing.T) {
	notifier := &fakeNotifier{}
	svc, registry := newNotifyService(t, notifier)
	token := broker.NewToken("token-abc")

	_, err := registry.Register(token, testIdentity("tenant-1"), testStream("s1"))
	require.NoError(t, err)
	registry.SetBrokerServerRegistered(token)

	svc.notifyClientConnected(token, "agent-7", "v1.2.3")

	require.Equal(t, 0, notifier.count(), "an already-registered token needs no notification")
}

// If the agent goes away entirely mid-retry there is nothing left to announce,
// and the loop must not spin forever against a token with no connections.
func TestNotifyClientConnected_StopsWhenTokenFullyDisconnected(t *testing.T) {
	notifier := &fakeNotifier{failFirst: 1000}
	svc, registry := newNotifyService(t, notifier)
	token := broker.NewToken("token-abc")

	_, err := registry.Register(token, testIdentity("tenant-1"), testStream("s1"))
	require.NoError(t, err)
	// The agent disconnects while the first attempt is in flight.
	notifier.onAttempt = func() { registry.Unregister(token, "s1") }

	done := make(chan struct{})
	go func() { svc.notifyClientConnected(token, "agent-7", "v1.2.3"); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notify did not stop after the token lost its last connection")
	}
	require.Equal(t, 1, notifier.count())
	require.False(t, registry.IsBrokerServerRegistered(token))
}

// An unknown token has nothing to announce.
func TestNotifyClientConnected_UnknownTokenDoesNothing(t *testing.T) {
	notifier := &fakeNotifier{}
	svc, _ := newNotifyService(t, notifier)
	svc.notifyClientConnected(broker.NewToken("never-registered"), "agent-7", "v1.2.3")
	require.Equal(t, 0, notifier.count())
}
