package tunnel

import (
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
	// Send sends a TunnelServerMessage to the client through this stream.
	Send func(msg *pb.TunnelServerMessage) error
	// Cancel closes this stream.
	Cancel func()
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
	Identity              ClientIdentity
	Token                 broker.Token
	Streams               map[string]*StreamHandle // streamID -> handle
	BrokerServerRegistered atomic.Bool
	roundRobin            atomic.Uint64
}

// ClientRegistry is a thread-safe registry of connected clients,
// keyed by hashed broker token.
type ClientRegistry struct {
	mu      sync.RWMutex
	entries map[string]*clientEntry // hashed token -> entry
	logger  *zap.Logger
}

// NewClientRegistry creates a new client registry.
func NewClientRegistry(logger *zap.Logger) *ClientRegistry {
	return &ClientRegistry{
		entries: make(map[string]*clientEntry),
		logger:  logger,
	}
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

// PickStream returns a stream handle for dispatching via round-robin.
// Returns nil if no streams are available for the token.
func (r *ClientRegistry) PickStream(token broker.Token) *StreamHandle {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[token.Hashed()]
	if !ok || len(entry.Streams) == 0 {
		return nil
	}

	// Collect stream handles into a slice for round-robin
	streams := make([]*StreamHandle, 0, len(entry.Streams))
	for _, s := range entry.Streams {
		streams = append(streams, s)
	}

	idx := entry.roundRobin.Add(1) - 1
	return streams[idx%uint64(len(streams))]
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
