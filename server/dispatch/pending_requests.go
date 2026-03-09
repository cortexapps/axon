package dispatch

import (
	"fmt"
	"sync"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
)

// PendingRequests tracks inflight HTTP requests dispatched through tunnels,
// correlating request IDs to response channels. It supports chunked responses.
type PendingRequests struct {
	mu       sync.RWMutex
	pending  map[string]*pendingEntry
	timeout  time.Duration
}

type pendingEntry struct {
	ch       chan *assembledResponse
	streamID string
	chunks   []*pb.HttpResponse
	timer    *time.Timer
}

type assembledResponse struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
}

// NewPendingRequests creates a new pending requests tracker.
func NewPendingRequests(timeout time.Duration) *PendingRequests {
	return &PendingRequests{
		pending: make(map[string]*pendingEntry),
		timeout: timeout,
	}
}

// Add registers a new pending request and returns a channel to await the response.
func (pr *PendingRequests) Add(requestID, streamID string) <-chan *assembledResponse {
	ch := make(chan *assembledResponse, 1)

	pr.mu.Lock()
	defer pr.mu.Unlock()

	timer := time.AfterFunc(pr.timeout, func() {
		pr.Timeout(requestID)
	})

	pr.pending[requestID] = &pendingEntry{
		ch:       ch,
		streamID: streamID,
		timer:    timer,
	}
	return ch
}

// Deliver processes an incoming HttpResponse chunk. When the final chunk is
// received (is_final=true), the assembled response is sent to the waiting channel.
func (pr *PendingRequests) Deliver(response *pb.HttpResponse) error {
	pr.mu.Lock()
	entry, ok := pr.pending[response.RequestId]
	if !ok {
		pr.mu.Unlock()
		return fmt.Errorf("no pending request for ID %s", response.RequestId)
	}

	entry.chunks = append(entry.chunks, response)

	if !response.IsFinal {
		pr.mu.Unlock()
		return nil
	}

	// Final chunk received — assemble and deliver.
	entry.timer.Stop()
	delete(pr.pending, response.RequestId)
	pr.mu.Unlock()

	assembled := assembleChunks(entry.chunks)
	entry.ch <- assembled
	return nil
}

// Timeout fails a pending request with a timeout error.
func (pr *PendingRequests) Timeout(requestID string) {
	pr.mu.Lock()
	entry, ok := pr.pending[requestID]
	if !ok {
		pr.mu.Unlock()
		return
	}
	delete(pr.pending, requestID)
	pr.mu.Unlock()

	// Send nil to indicate timeout.
	close(entry.ch)
}

// FailStream fails all pending requests that were dispatched on a given stream.
func (pr *PendingRequests) FailStream(streamID string) {
	pr.mu.Lock()
	var toFail []*pendingEntry
	var toDelete []string

	for reqID, entry := range pr.pending {
		if entry.streamID == streamID {
			toFail = append(toFail, entry)
			toDelete = append(toDelete, reqID)
		}
	}
	for _, reqID := range toDelete {
		delete(pr.pending, reqID)
	}
	pr.mu.Unlock()

	for _, entry := range toFail {
		entry.timer.Stop()
		close(entry.ch)
	}
}

// Count returns the number of inflight requests.
func (pr *PendingRequests) Count() int {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return len(pr.pending)
}

// assembleChunks reconstructs a full response from ordered chunks.
func assembleChunks(chunks []*pb.HttpResponse) *assembledResponse {
	if len(chunks) == 0 {
		return &assembledResponse{}
	}

	// First chunk has status code and headers.
	first := chunks[0]
	resp := &assembledResponse{
		StatusCode: int(first.StatusCode),
		Headers:    first.Headers,
	}

	// Concatenate all body chunks.
	var totalLen int
	for _, c := range chunks {
		totalLen += len(c.Body)
	}
	body := make([]byte, 0, totalLen)
	for _, c := range chunks {
		body = append(body, c.Body...)
	}
	resp.Body = body

	return resp
}
