package tunnel

import (
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
		Send:     func(msg *pb.TunnelServerMessage) error { return nil },
		Cancel:   func() {},
	}
}

func TestRegisterAndLookup(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	stream := testStream("stream-1")

	err := registry.Register(token, identity, stream)
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

	err := registry.Register(token, identity, testStream("stream-1"))
	require.NoError(t, err)

	// Same tenant, different instance — allowed.
	identity2 := identity
	identity2.InstanceID = "instance-2"
	err = registry.Register(token, identity2, testStream("stream-2"))
	require.NoError(t, err)

	assert.Equal(t, 1, registry.Count())
	assert.Equal(t, 2, registry.StreamCount())
}

func TestRegisterTokenCollision(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")

	err := registry.Register(token, testIdentity("tenant-1"), testStream("stream-1"))
	require.NoError(t, err)

	// Different tenant with same token hash — rejected.
	err = registry.Register(token, testIdentity("tenant-2"), testStream("stream-2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token collision")
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

func TestPickStreamRoundRobin(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	token := broker.NewToken("token-abc")
	identity := testIdentity("tenant-1")
	registry.Register(token, identity, testStream("stream-1"))
	registry.Register(token, identity, testStream("stream-2"))

	// Pick multiple times and verify we get both streams.
	seen := map[string]bool{}
	for range 10 {
		s := registry.PickStream(token)
		require.NotNil(t, s)
		seen[s.StreamID] = true
	}
	assert.True(t, seen["stream-1"], "should pick stream-1")
	assert.True(t, seen["stream-2"], "should pick stream-2")
}

func TestPickStreamNoEntry(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := NewClientRegistry(logger)

	s := registry.PickStream(broker.TokenFromHash("no-such-hash"))
	assert.Nil(t, s)
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
