package tunnel

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"go.uber.org/zap"
)

// StreamHandle represents a single tunnel stream to a client.
type StreamHandle struct {
	StreamID string
	// Send sends a ServerFrame to the client through this stream.
	Send func(msg *pb.ServerFrame) error
	// Cancel closes this stream.
	Cancel func()
	// LastSuccessAt holds the UnixNano timestamp of the most recent successful
	// response received on this stream. Used by AcquireIdleStream to prefer
	// healthy streams.
	LastSuccessAt atomic.Int64

	// busy is true while a call is in flight on this stream. Each stream
	// carries at most one call at a time; the dispatcher acquires the stream
	// before sending CallStart and releases it when the call fully
	// terminates. That one-call-per-stream rule is what makes a busy stream
	// genuinely unavailable, which is in turn what gives dispatch its
	// backpressure: an agent that falls behind stops offering idle streams.
	busy atomic.Bool

	// onRelease returns the stream to its entry's idle stack. Set by the
	// registry at Register time; nil for handles that were never registered.
	onRelease func()
}

// TryAcquire marks the stream busy. Returns false if it was already busy.
func (s *StreamHandle) TryAcquire() bool {
	return s.busy.CompareAndSwap(false, true)
}

// Release marks the stream idle again and returns it to the idle stack. The
// compare-and-swap makes a double Release a no-op, so a call that both fails
// and then finishes cannot enqueue the stream twice.
func (s *StreamHandle) Release() {
	if !s.busy.CompareAndSwap(true, false) {
		return
	}
	if s.onRelease != nil {
		s.onRelease()
	}
}

// Busy reports whether a call is currently in flight on this stream.
// Used to relax heartbeat-timeout enforcement while a call is active.
func (s *StreamHandle) Busy() bool {
	return s.busy.Load()
}

// ClientIdentity holds client-supplied identity metadata for a connected
// client. Informational only — it feeds logs, metrics, and BROKER_SERVER
// notifications, never authorization or routing decisions; the broker
// token is the sole credential and dispatch key.
type ClientIdentity struct {
	TenantID    string
	Integration string
	Alias       string
	InstanceID  string
}

// clientEntry represents all connections for a single broker token.
type clientEntry struct {
	Identity               ClientIdentity
	Token                  broker.Token
	Streams                map[string]*StreamHandle // streamID -> handle
	BrokerServerRegistered atomic.Bool

	// idle is a stack of streams believed to be idle, so dispatch can take
	// one in O(1) instead of scanning and sorting every stream on the token.
	// It is a hint, not the truth — busy is: a handle may sit here while
	// concurrently acquired elsewhere, so acquisition still confirms with
	// TryAcquire and skips losers. LIFO is deliberate: the most recently
	// released stream is the one most recently proven healthy, which is the
	// preference the old LastSuccessAt sort was reaching for.
	idle []*StreamHandle
}

// pushIdle returns a stream to the idle stack. Callers hold r.mu.
func (e *clientEntry) pushIdle(s *StreamHandle) {
	e.idle = append(e.idle, s)
}

// popIdle takes the newest idle stream, dropping entries that are stale
// (already busy, or no longer registered). Callers hold r.mu.
func (e *clientEntry) popIdle() *StreamHandle {
	for len(e.idle) > 0 {
		s := e.idle[len(e.idle)-1]
		e.idle = e.idle[:len(e.idle)-1]
		if e.Streams[s.StreamID] != s {
			continue // unregistered while queued
		}
		if s.TryAcquire() {
			return s
		}
	}
	return nil
}

// removeIdle drops a stream from the idle stack. Callers hold r.mu.
func (e *clientEntry) removeIdle(streamID string) {
	for i, s := range e.idle {
		if s.StreamID == streamID {
			e.idle = append(e.idle[:i], e.idle[i+1:]...)
			return
		}
	}
}

// ErrTokenStreamCap is returned by Register when the token already holds
// the maximum allowed number of streams.
var ErrTokenStreamCap = errors.New("token is at its stream cap")

// ClientRegistry is a thread-safe registry of connected clients,
// keyed by hashed broker token.
type ClientRegistry struct {
	mu      sync.RWMutex
	entries map[string]*clientEntry // hashed token -> entry
	logger  *zap.Logger

	// maxStreamsPerToken caps streams per token; 0 means unlimited.
	maxStreamsPerToken int
}

// NewClientRegistry creates a new client registry.
func NewClientRegistry(logger *zap.Logger) *ClientRegistry {
	return &ClientRegistry{
		entries: make(map[string]*clientEntry),
		logger:  logger,
	}
}

// SetMaxStreamsPerToken sets the per-token stream cap (0 = unlimited).
func (r *ClientRegistry) SetMaxStreamsPerToken(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxStreamsPerToken = n
}

// Register adds a new stream for a broker token, enforcing the per-token
// stream cap. Identity is client-supplied and informational: a tenant
// mismatch across streams of the same token is logged as likely agent
// misconfiguration but never rejects the stream — the token, not the
// claimed identity, is the credential (the first-seen identity remains
// the entry's display identity).
func (r *ClientRegistry) Register(token broker.Token, identity ClientIdentity, stream *StreamHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := token.Hashed()
	if existing, ok := r.entries[key]; ok {
		if existing.Identity.TenantID != identity.TenantID {
			r.logger.Warn("Streams on the same token claim different tenant_ids; likely agent misconfiguration (identity is informational only)",
				zap.String("existingTenantId", existing.Identity.TenantID),
				zap.String("newTenantId", identity.TenantID),
				zap.String("streamId", stream.StreamID),
			)
		}
		if r.maxStreamsPerToken > 0 && len(existing.Streams) >= r.maxStreamsPerToken {
			return fmt.Errorf("%w (%d)", ErrTokenStreamCap, r.maxStreamsPerToken)
		}
		existing.Streams[stream.StreamID] = stream
		stream.onRelease = r.makeOnRelease(key, stream)
		existing.pushIdle(stream)
		// Debug: the entry already existed, so the token's reachability here
		// has not changed — only its stream count. Registered new client
		// below is the transition, and stays at Info.
		r.logger.Debug("Added stream to existing client entry",
			zap.String("tenantId", identity.TenantID),
			zap.String("instanceId", identity.InstanceID),
			zap.String("streamId", stream.StreamID),
			zap.Int("totalStreams", len(existing.Streams)),
		)
		return nil
	}

	entry := &clientEntry{
		Identity: identity,
		Token:    token,
		Streams:  map[string]*StreamHandle{stream.StreamID: stream},
	}
	stream.onRelease = r.makeOnRelease(key, stream)
	entry.pushIdle(stream)
	r.entries[key] = entry

	r.logger.Info("Registered new client",
		zap.String("tenantId", identity.TenantID),
		zap.String("integration", identity.Integration),
		zap.String("alias", identity.Alias),
		zap.String("instanceId", identity.InstanceID),
		zap.String("streamId", stream.StreamID),
	)
	return nil
}

// makeOnRelease builds the callback that returns a stream to its entry's
// idle stack when its call finishes. It re-checks registration because a
// stream can die mid-call: the dispatcher still releases it, and by then the
// entry may be gone or the handle replaced.
func (r *ClientRegistry) makeOnRelease(key string, s *StreamHandle) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		entry, ok := r.entries[key]
		if !ok || entry.Streams[s.StreamID] != s {
			return
		}
		entry.pushIdle(s)
	}
}

// Unregister removes a specific stream for a token.
// If it was the last stream, the entire entry is removed.
// Returns true if the entire entry was removed.
func (r *ClientRegistry) Unregister(token broker.Token, streamID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := token.Hashed()
	entry, ok := r.entries[key]
	if !ok {
		return false
	}

	delete(entry.Streams, streamID)
	entry.removeIdle(streamID)

	if len(entry.Streams) == 0 {
		delete(r.entries, key)
		r.logger.Info("Removed client entry (last stream closed)",
			zap.String("tenantId", entry.Identity.TenantID),
			zap.String("streamId", streamID),
		)
		return true
	}

	// Debug: streams remain, so the token is still reachable here. Losing the
	// last one is the event that matters, and it is logged at Info above.
	r.logger.Debug("Removed stream from client entry",
		zap.String("tenantId", entry.Identity.TenantID),
		zap.String("streamId", streamID),
		zap.Int("remainingStreams", len(entry.Streams)),
	)
	return false
}

// GetIdentity returns the identity for a token, or nil if not found.
func (r *ClientRegistry) GetIdentity(token broker.Token) *ClientIdentity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[token.Hashed()]
	if !ok {
		return nil
	}
	id := entry.Identity
	return &id
}

// AcquireIdleStream returns an idle stream handle for dispatching, marked
// busy. Returns (nil, false) when the token has no streams at all, and
// (nil, true) when streams exist but all are busy — the signal the dispatcher
// turns into a brief wait, and thus into backpressure toward the caller.
//
// This is the hot path: one acquisition per dispatched call. It pops the idle
// stack rather than ranking every stream on the token, so cost does not grow
// with the agent's stream count.
//
// The caller owns the returned stream's busy flag and must call Release()
// when the call fully terminates.
func (r *ClientRegistry) AcquireIdleStream(token broker.Token) (handle *StreamHandle, allBusy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[token.Hashed()]
	if !ok || len(entry.Streams) == 0 {
		return nil, false
	}

	if s := entry.popIdle(); s != nil {
		return s, false
	}

	// The stack is a hint and can drift empty while a stream is in fact
	// idle (a handle released after being unregistered, say). Falling back
	// to a scan keeps "all busy" honest — reporting a false all-busy would
	// stall dispatch until the caller's deadline for no reason.
	for _, s := range entry.Streams {
		if s.TryAcquire() {
			return s, false
		}
	}

	return nil, true
}

// SetBrokerServerRegistered marks a token as successfully registered with BROKER_SERVER.
func (r *ClientRegistry) SetBrokerServerRegistered(token broker.Token) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.entries[token.Hashed()]; ok {
		entry.BrokerServerRegistered.Store(true)
	}
}

// ForEach calls fn for each registered client entry.
// Used for periodic re-registration.
func (r *ClientRegistry) ForEach(fn func(token broker.Token, identity ClientIdentity)) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, entry := range r.entries {
		fn(entry.Token, entry.Identity)
	}
}

// Count returns the number of registered client entries.
func (r *ClientRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// StreamCount returns the total number of active streams across all clients.
func (r *ClientRegistry) StreamCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := 0
	for _, entry := range r.entries {
		total += len(entry.Streams)
	}
	return total
}
