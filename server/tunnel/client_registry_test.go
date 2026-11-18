package tunnel

import (
	"fmt"
	"testing"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func testIdentity(tenantID string) ClientIdentity {
	return ClientIdentity{
		TenantID:    tenantID,
		Integration: "github",
		Alias:       "my-github",
		InstanceID:  "instance-1",
	}
}

func testStream(streamID string) *StreamHandle {
	return &StreamHandle{
		StreamID: streamID,
		Send:     func(msg *pb.ServerFrame) error { return nil },
		Cancel:   func() {},
	}
}

func TestRegisterAndLookup(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	stream := testStream("stream-1")

	_, err := registry.Register(token, identity, stream)
	require.NoError(t, err)

	assert.Equal(t, 1, registry.Count())
	assert.Equal(t, 1, registry.StreamCount())

	got := registry.GetIdentity(token)
	require.NotNil(t, got)
	assert.Equal(t, "tenant-1", got.TenantID)
	assert.Equal(t, "github", got.Integration)
	assert.Equal(t, "my-github", got.Alias)
}

func TestRegisterMultipleStreams(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")

	_, err := registry.Register(token, identity, testStream("stream-1"))
	require.NoError(t, err)

	// Same tenant, different instance — allowed.
	identity2 := identity
	identity2.InstanceID = "instance-2"
	_, err = registry.Register(token, identity2, testStream("stream-2"))
	require.NoError(t, err)

	assert.Equal(t, 1, registry.Count())
	assert.Equal(t, 2, registry.StreamCount())
}

func TestRegisterTenantMismatchIsInformational(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")

	_, err := registry.Register(token, testIdentity("tenant-1"), testStream("stream-1"))
	require.NoError(t, err)

	// A different claimed tenant on the same token is accepted: identity is
	// client-supplied, informational metadata — the token is the credential.
	// (It logs a warning; likely agent misconfiguration.)
	_, err = registry.Register(token, testIdentity("tenant-2"), testStream("stream-2"))
	require.NoError(t, err)
	assert.Equal(t, 2, registry.StreamCount())

	// First-seen identity remains the entry's display identity.
	got := registry.GetIdentity(token)
	require.NotNil(t, got)
	assert.Equal(t, "tenant-1", got.TenantID)
}

func TestUnregisterStream(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	registry.Register(token, identity, testStream("stream-1"))
	registry.Register(token, identity, testStream("stream-2"))

	// Remove one stream — entry should remain.
	removed := registry.Unregister(token, "stream-1")
	assert.False(t, removed)
	assert.Equal(t, 1, registry.Count())
	assert.Equal(t, 1, registry.StreamCount())

	// Remove last stream — entry should be removed.
	removed = registry.Unregister(token, "stream-2")
	assert.True(t, removed)
	assert.Equal(t, 0, registry.Count())
	assert.Equal(t, 0, registry.StreamCount())
	assert.Nil(t, registry.GetIdentity(token))
}

func TestUnregisterNonexistent(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	removed := registry.Unregister(broker.TokenFromHash("no-such-hash"), "stream-1")
	assert.False(t, removed)
}

func TestAcquireIdleStream(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	registry.Register(token, identity, testStream("stream-1"))
	registry.Register(token, identity, testStream("stream-2"))

	// Acquire both; each acquisition marks the stream busy so the second
	// call must return the other stream.
	s1, allBusy := registry.AcquireIdleStream(token)
	require.NotNil(t, s1)
	assert.False(t, allBusy)
	s2, allBusy := registry.AcquireIdleStream(token)
	require.NotNil(t, s2)
	assert.False(t, allBusy)
	assert.NotEqual(t, s1.StreamID, s2.StreamID)

	// All busy now.
	s3, allBusy := registry.AcquireIdleStream(token)
	assert.Nil(t, s3)
	assert.True(t, allBusy)

	// Release one and it becomes acquirable again.
	s1.Release()
	s4, allBusy := registry.AcquireIdleStream(token)
	require.NotNil(t, s4)
	assert.False(t, allBusy)
	assert.Equal(t, s1.StreamID, s4.StreamID)
}

func TestAcquireIdleStreamPrefersLastSuccess(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	sA := testStream("stream-a")
	sB := testStream("stream-b")
	registry.Register(token, identity, sA)
	registry.Register(token, identity, sB)

	// stream-b has the most recent success; it should be picked first.
	sB.LastSuccessAt.Store(100)
	got, _ := registry.AcquireIdleStream(token)
	require.NotNil(t, got)
	assert.Equal(t, "stream-b", got.StreamID)

	// stream-b is now busy; the next acquire falls back to stream-a.
	got2, _ := registry.AcquireIdleStream(token)
	require.NotNil(t, got2)
	assert.Equal(t, "stream-a", got2.StreamID)
}

func TestAcquireIdleStreamNoEntry(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	s, allBusy := registry.AcquireIdleStream(broker.TokenFromHash("no-such-hash"))
	assert.Nil(t, s)
	assert.False(t, allBusy)
}

func TestBrokerServerRegistered(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	registry.Register(token, identity, testStream("stream-1"))

	// Not registered initially.
	registry.SetBrokerServerRegistered(token)

	// Verify no panic on non-existent entry.
	registry.SetBrokerServerRegistered(broker.TokenFromHash("no-such-hash"))
}

func TestForEach(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	registry.Register(broker.NewToken("token-1"), testIdentity("tenant-1"), testStream("s1"))
	registry.Register(broker.NewToken("token-2"), testIdentity("tenant-2"), testStream("s2"))

	var entries []string
	registry.ForEach(func(token broker.Token, identity ClientIdentity) {
		entries = append(entries, identity.TenantID)
	})
	assert.Len(t, entries, 2)
	assert.Contains(t, entries, "tenant-1")
	assert.Contains(t, entries, "tenant-2")
}

func TestRegisterTokenStreamCap(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)
	registry.SetMaxStreamsPerToken(2)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")

	mustRegister(t, registry, token, identity, testStream("s1"))
	mustRegister(t, registry, token, identity, testStream("s2"))

	// Third stream is rejected at the cap.
	_, err := registry.Register(token, identity, testStream("s3"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTokenStreamCap)

	// Unregistering frees a slot at the cap.
	registry.Unregister(token, "s1")
	mustRegister(t, registry, token, identity, testStream("s3"))

	// Cap of zero means unlimited.
	registry.SetMaxStreamsPerToken(0)
	mustRegister(t, registry, token, identity, testStream("s4"))
}

// BROKER_SERVER cares whether a token has any connection at all, not how many
// streams it has: the edges that matter are 0->1 and 1->0. Unregister already
// reports the 1->0 edge and the client-disconnected notification is gated on
// it; Register computed the same signal for 0->1 and discarded it, so
// client-connected fired once per stream instead. A reconnecting agent opens a
// stream per unit of concurrent work — up to 256, against a server cap of 64 —
// which in staging became a sustained 12-32 POST/s against the dispatcher for
// 16 seconds, subsiding only when the stream cap engaged.
func TestRegister_ReportsTheZeroToOneEdge(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)
	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")

	created, err := registry.Register(token, identity, testStream("s1"))
	require.NoError(t, err)
	require.True(t, created, "the first stream takes the token from 0 to 1 connections")

	for i := 2; i <= 64; i++ {
		created, err := registry.Register(token, identity, testStream(fmt.Sprintf("s%d", i)))
		require.NoError(t, err)
		require.False(t, created, "stream %d is not a connectivity change for the token", i)
	}

	// Closing every stream is the 1->0 edge, which Unregister already reports.
	for i := 2; i <= 64; i++ {
		require.False(t, registry.Unregister(token, fmt.Sprintf("s%d", i)))
	}
	require.True(t, registry.Unregister(token, "s1"), "last stream closing is the 1->0 edge")

	// Reconnecting is a fresh 0->1 edge and must be announced again.
	created, err = registry.Register(token, identity, testStream("s-new"))
	require.NoError(t, err)
	require.True(t, created)
}

// A stream rejected at the cap must not be reported as a connectivity change.
func TestRegister_CapRejectionIsNotAnEdge(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)
	registry.SetMaxStreamsPerToken(1)
	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")

	created, err := registry.Register(token, identity, testStream("s1"))
	require.NoError(t, err)
	require.True(t, created)

	created, err = registry.Register(token, identity, testStream("s2"))
	require.ErrorIs(t, err, ErrTokenStreamCap)
	require.False(t, created)
}

func TestIsBrokerServerRegistered(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)
	token := broker.NewToken("token-abc")

	require.False(t, registry.IsBrokerServerRegistered(token), "unknown token is not registered")

	_, err := registry.Register(token, testIdentity("tenant-1"), testStream("s1"))
	require.NoError(t, err)
	require.False(t, registry.IsBrokerServerRegistered(token))

	registry.SetBrokerServerRegistered(token)
	require.True(t, registry.IsBrokerServerRegistered(token))

	// The flag lives on the entry, so a full disconnect clears it and the
	// reconnect is announced again rather than assumed known.
	require.True(t, registry.Unregister(token, "s1"))
	_, err = registry.Register(token, testIdentity("tenant-1"), testStream("s2"))
	require.NoError(t, err)
	require.False(t, registry.IsBrokerServerRegistered(token))
}

// mustRegister registers a stream and drops Register's connection-edge result,
// for the call sites that only care that registration succeeded.
func mustRegister(t *testing.T, r *ClientRegistry, token broker.Token, id ClientIdentity, h *StreamHandle) {
	t.Helper()
	_, err := r.Register(token, id, h)
	require.NoError(t, err)
}
