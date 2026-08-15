package tunnel

import (
	"fmt"
	"sync"
	"testing"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func idleTestRegistry(t *testing.T) (*ClientRegistry, broker.Token) {
	t.Helper()
	return NewClientRegistry(zap.NewNop()), broker.NewToken("tok-idle")
}

func registerN(t *testing.T, r *ClientRegistry, token broker.Token, n int) []*StreamHandle {
	t.Helper()
	handles := make([]*StreamHandle, 0, n)
	for i := 0; i < n; i++ {
		h := &StreamHandle{
			StreamID: fmt.Sprintf("s-%02d", i),
			Send:     func(*pb.ServerFrame) error { return nil },
			Cancel:   func() {},
		}
		require.NoError(t, r.Register(token, ClientIdentity{TenantID: "t"}, h))
		handles = append(handles, h)
	}
	return handles
}

func TestIdleStack_AcquireReleaseRoundTrip(t *testing.T) {
	r, token := idleTestRegistry(t)
	registerN(t, r, token, 3)

	first, allBusy := r.AcquireIdleStream(token)
	require.NotNil(t, first)
	assert.False(t, allBusy)
	assert.True(t, first.Busy())

	first.Release()
	assert.False(t, first.Busy())

	// Returned to the stack, so it can be handed out again.
	again, _ := r.AcquireIdleStream(token)
	require.NotNil(t, again)
	assert.Equal(t, first.StreamID, again.StreamID, "LIFO hands back the most recently released stream")
}

func TestIdleStack_ExhaustsThenReportsAllBusy(t *testing.T) {
	r, token := idleTestRegistry(t)
	registerN(t, r, token, 4)

	for i := 0; i < 4; i++ {
		h, allBusy := r.AcquireIdleStream(token)
		require.NotNil(t, h, "stream %d should be available", i)
		require.False(t, allBusy)
	}

	h, allBusy := r.AcquireIdleStream(token)
	assert.Nil(t, h)
	assert.True(t, allBusy, "all busy is the signal dispatch turns into a wait")
}

func TestIdleStack_UnknownTokenIsNotAllBusy(t *testing.T) {
	r, _ := idleTestRegistry(t)

	h, allBusy := r.AcquireIdleStream(broker.NewToken("nobody"))
	assert.Nil(t, h)
	assert.False(t, allBusy, "no streams at all must stay distinguishable from all busy")
}

func TestIdleStack_DoubleReleaseDoesNotDuplicate(t *testing.T) {
	r, token := idleTestRegistry(t)
	registerN(t, r, token, 1)

	h, _ := r.AcquireIdleStream(token)
	require.NotNil(t, h)
	h.Release()
	h.Release() // e.g. a call that both errored and then finished

	got, _ := r.AcquireIdleStream(token)
	require.NotNil(t, got)

	// If the double release had queued the handle twice, this second
	// acquisition would succeed against an already-busy stream.
	dup, allBusy := r.AcquireIdleStream(token)
	assert.Nil(t, dup)
	assert.True(t, allBusy)
}

func TestIdleStack_UnregisterWhileIdleRemovesFromStack(t *testing.T) {
	r, token := idleTestRegistry(t)
	handles := registerN(t, r, token, 2)

	r.Unregister(token, handles[0].StreamID)

	// Only the surviving stream may be handed out, however many times we ask.
	for i := 0; i < 3; i++ {
		h, _ := r.AcquireIdleStream(token)
		if h == nil {
			break
		}
		assert.Equal(t, handles[1].StreamID, h.StreamID)
		h.Release()
	}
}

func TestIdleStack_ReleaseAfterUnregisterIsDropped(t *testing.T) {
	r, token := idleTestRegistry(t)
	handles := registerN(t, r, token, 2)

	h, _ := r.AcquireIdleStream(token)
	require.NotNil(t, h)

	// The stream dies mid-call; the dispatcher still releases it afterwards.
	r.Unregister(token, h.StreamID)
	h.Release()

	// The dead stream must not come back out of the stack.
	for i := 0; i < 3; i++ {
		got, _ := r.AcquireIdleStream(token)
		if got == nil {
			break
		}
		assert.NotEqual(t, h.StreamID, got.StreamID, "unregistered stream was handed out")
		got.Release()
	}
	_ = handles
}

// Concurrent acquisition must never hand the same stream to two callers —
// that would put two calls on one stream and silently break the one-call-per-
// stream rule the whole design rests on.
func TestIdleStack_ConcurrentAcquireIsExclusive(t *testing.T) {
	r, token := idleTestRegistry(t)
	const streams = 32
	registerN(t, r, token, streams)

	var (
		mu      sync.Mutex
		got     = map[string]int{}
		wg      sync.WaitGroup
		nilHits int
	)
	for i := 0; i < streams*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, _ := r.AcquireIdleStream(token)
			mu.Lock()
			defer mu.Unlock()
			if h == nil {
				nilHits++
				return
			}
			got[h.StreamID]++
		}()
	}
	wg.Wait()

	assert.Equal(t, streams, len(got), "every stream handed out exactly once")
	for id, n := range got {
		assert.Equal(t, 1, n, "stream %s handed out %d times", id, n)
	}
	assert.Equal(t, streams, nilHits)
}

func TestIdleStack_StreamCapStillEnforced(t *testing.T) {
	r, token := idleTestRegistry(t)
	r.SetMaxStreamsPerToken(2)
	registerN(t, r, token, 2)

	err := r.Register(token, ClientIdentity{TenantID: "t"}, &StreamHandle{
		StreamID: "s-over", Send: func(*pb.ServerFrame) error { return nil }, Cancel: func() {},
	})
	require.ErrorIs(t, err, ErrTokenStreamCap)
}
