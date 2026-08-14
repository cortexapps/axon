package tunnel

import (
	"errors"
	"fmt"
	"sort"
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

	// busy is true while a call is in flight on this stream. In the slot-pool
	// model each stream carries at most one call at a time; the dispatcher
	// acquires the stream before sending CallStart and releases it when the
	// call fully terminates.
	busy atomic.Bool
}

// TryAcquire marks the stream busy. Returns false if it was already busy.
func (s *StreamHandle) TryAcquire() bool {
	return s.busy.CompareAndSwap(false, true)
}

// Release marks the stream idle again.
func (s *StreamHandle) Release() {
	s.busy.Store(false)
}

// Busy reports whether a call is currently in flight on this stream.
// Used to relax heartbeat-timeout enforcement while a call is active.
func (s *StreamHandle) Busy() bool {
	return s.busy.Load()
}

// ClientIdentity holds the identity metadata for a connected client.
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

// Register adds a new stream for a broker token.
// If the token already exists, it validates the identity matches (same tenant)
// and adds the stream. Returns an error on identity collision.
func (r *ClientRegistry) Register(token broker.Token, identity ClientIdentity, stream *StreamHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := token.Hashed()
	if existing, ok := r.entries[key]; ok {
		// Same tenant is allowed (reconnect or new instance)
		if existing.Identity.TenantID != identity.TenantID {
			return fmt.Errorf("token collision: different tenant_id for token (existing=%s, new=%s)",
				existing.Identity.TenantID, identity.TenantID)
		}
		if r.maxStreamsPerToken > 0 && len(existing.Streams) >= r.maxStreamsPerToken {
			return fmt.Errorf("%w (%d)", ErrTokenStreamCap, r.maxStreamsPerToken)
		}
		existing.Streams[stream.StreamID] = stream
		r.logger.Info("Added stream to existing client entry",
			zap.String("tenantId", identity.TenantID),
			zap.String("instanceId", identity.InstanceID),
			zap.String("streamId", stream.StreamID),
			zap.Int("totalStreams", len(existing.Streams)),
		)
		return nil
	}

	r.entries[key] = &clientEntry{
		Identity: identity,
		Token:    token,
		Streams:  map[string]*StreamHandle{stream.StreamID: stream},
	}

	r.logger.Info("Registered new client",
		zap.String("tenantId", identity.TenantID),
		zap.String("integration", identity.Integration),
		zap.String("alias", identity.Alias),
		zap.String("instanceId", identity.InstanceID),
		zap.String("streamId", stream.StreamID),
	)
	return nil
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

	if len(entry.Streams) == 0 {
		delete(r.entries, key)
		r.logger.Info("Removed client entry (last stream closed)",
			zap.String("tenantId", entry.Identity.TenantID),
			zap.String("streamId", streamID),
		)
		return true
	}

	r.logger.Info("Removed stream from client entry",
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
// busy, preferring the idle stream with the most recent successful response.
// Idle streams that have never recorded a success are tried in round-robin
// order. Returns (nil, false) when the token has no streams at all, and
// (nil, true) when streams exist but all are busy.
//
// The caller owns the returned stream's busy flag and must call Release()
// when the call fully terminates.
func (r *ClientRegistry) AcquireIdleStream(token broker.Token) (handle *StreamHandle, allBusy bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[token.Hashed()]
	if !ok || len(entry.Streams) == 0 {
		return nil, false
	}

	// Collect stream handles into a slice with deterministic ordering
	// (map iteration order is randomized).
	streams := make([]*StreamHandle, 0, len(entry.Streams))
	for _, s := range entry.Streams {
		streams = append(streams, s)
	}
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].StreamID < streams[j].StreamID
	})

	// Prefer idle streams with the most recent successful response.
	sort.SliceStable(streams, func(i, j int) bool {
		return streams[i].LastSuccessAt.Load() > streams[j].LastSuccessAt.Load()
	})
	for _, s := range streams {
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
