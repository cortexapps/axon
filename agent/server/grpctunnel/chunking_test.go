package grpctunnel

import (
	"testing"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
)

func TestRequestAssembler_SingleChunk(t *testing.T) {
	ra := newRequestAssembler()

	// A single-chunk request (chunk_index=0, is_final=true) should pass through directly.
	req := &pb.HttpRequest{
		RequestId: "req-1",
		Method:    "GET",
		Path:      "/api/v1/repos",
		Headers:   map[string]string{"Authorization": "Bearer tok"},
		Body:      []byte("hello"),
		ChunkIndex: 0,
		IsFinal:    true,
		TimeoutMs:  5000,
	}

	result := ra.handleChunk(req)
	if result == nil {
		t.Fatal("expected non-nil result for single-chunk request")
	}
	// Single-chunk should return the exact same pointer (fast path).
	if result != req {
		t.Fatal("expected single-chunk to return the original request pointer")
	}
	if result.RequestId != "req-1" {
		t.Errorf("expected RequestId=req-1, got %s", result.RequestId)
	}
	if result.Method != "GET" {
		t.Errorf("expected Method=GET, got %s", result.Method)
	}
	if string(result.Body) != "hello" {
		t.Errorf("expected body=hello, got %s", string(result.Body))
	}

	// No pending requests should remain.
	ra.mu.Lock()
	pendingCount := len(ra.pending)
	ra.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("expected 0 pending requests, got %d", pendingCount)
	}
}

func TestRequestAssembler_MultiChunk(t *testing.T) {
	ra := newRequestAssembler()

	// Chunk 0: first chunk with metadata.
	chunk0 := &pb.HttpRequest{
		RequestId:  "req-2",
		Method:     "POST",
		Path:       "/api/v1/upload",
		Headers:    map[string]string{"Content-Type": "application/octet-stream"},
		Body:       []byte("chunk0-"),
		ChunkIndex: 0,
		IsFinal:    false,
		TimeoutMs:  10000,
	}
	result := ra.handleChunk(chunk0)
	if result != nil {
		t.Fatal("expected nil result for non-final first chunk")
	}

	// Chunk 1: continuation chunk (only has body).
	chunk1 := &pb.HttpRequest{
		RequestId:  "req-2",
		Body:       []byte("chunk1-"),
		ChunkIndex: 1,
		IsFinal:    false,
	}
	result = ra.handleChunk(chunk1)
	if result != nil {
		t.Fatal("expected nil result for non-final continuation chunk")
	}

	// Chunk 2: final chunk.
	chunk2 := &pb.HttpRequest{
		RequestId:  "req-2",
		Body:       []byte("chunk2"),
		ChunkIndex: 2,
		IsFinal:    true,
	}
	result = ra.handleChunk(chunk2)
	if result == nil {
		t.Fatal("expected non-nil result for final chunk")
	}

	// Verify assembled request carries metadata from chunk 0.
	if result.RequestId != "req-2" {
		t.Errorf("expected RequestId=req-2, got %s", result.RequestId)
	}
	if result.Method != "POST" {
		t.Errorf("expected Method=POST, got %s", result.Method)
	}
	if result.Path != "/api/v1/upload" {
		t.Errorf("expected Path=/api/v1/upload, got %s", result.Path)
	}
	if result.Headers["Content-Type"] != "application/octet-stream" {
		t.Errorf("expected Content-Type header, got %v", result.Headers)
	}
	if result.TimeoutMs != 10000 {
		t.Errorf("expected TimeoutMs=10000, got %d", result.TimeoutMs)
	}
	if !result.IsFinal {
		t.Error("expected IsFinal=true on assembled request")
	}

	// Verify body is concatenated in order.
	expectedBody := "chunk0-chunk1-chunk2"
	if string(result.Body) != expectedBody {
		t.Errorf("expected body=%q, got %q", expectedBody, string(result.Body))
	}

	// No pending requests should remain after assembly.
	ra.mu.Lock()
	pendingCount := len(ra.pending)
	ra.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("expected 0 pending requests after assembly, got %d", pendingCount)
	}
}

func TestRequestAssembler_FirstChunkStoresMetadata(t *testing.T) {
	ra := newRequestAssembler()

	// First chunk carries all metadata.
	chunk0 := &pb.HttpRequest{
		RequestId:  "req-3",
		Method:     "PUT",
		Path:       "/api/v1/data",
		Headers:    map[string]string{"X-Custom": "value1", "Accept": "application/json"},
		Body:       []byte("part1"),
		ChunkIndex: 0,
		IsFinal:    false,
		TimeoutMs:  30000,
	}
	ra.handleChunk(chunk0)

	// Subsequent chunk has different method/path/headers on the proto message,
	// but the assembler should use only the first chunk's metadata.
	chunk1 := &pb.HttpRequest{
		RequestId:  "req-3",
		Method:     "DELETE",          // should be ignored
		Path:       "/wrong/path",     // should be ignored
		Headers:    map[string]string{"X-Wrong": "ignored"}, // should be ignored
		Body:       []byte("part2"),
		ChunkIndex: 1,
		IsFinal:    true,
		TimeoutMs:  99999,             // should be ignored
	}
	result := ra.handleChunk(chunk1)
	if result == nil {
		t.Fatal("expected non-nil result for final chunk")
	}

	// Verify metadata comes from chunk 0, not chunk 1.
	if result.Method != "PUT" {
		t.Errorf("expected Method=PUT from first chunk, got %s", result.Method)
	}
	if result.Path != "/api/v1/data" {
		t.Errorf("expected Path=/api/v1/data from first chunk, got %s", result.Path)
	}
	if result.TimeoutMs != 30000 {
		t.Errorf("expected TimeoutMs=30000 from first chunk, got %d", result.TimeoutMs)
	}
	if result.Headers["X-Custom"] != "value1" {
		t.Errorf("expected X-Custom=value1 from first chunk, got %v", result.Headers)
	}
	if _, ok := result.Headers["X-Wrong"]; ok {
		t.Error("expected X-Wrong header from continuation chunk to be ignored")
	}

	// Body should be concatenated.
	if string(result.Body) != "part1part2" {
		t.Errorf("expected body=part1part2, got %q", string(result.Body))
	}
}

func TestRequestAssembler_IncompleteRequestDiscarded(t *testing.T) {
	ra := newRequestAssembler()

	// Start a multi-chunk request but never send the final chunk.
	chunk0 := &pb.HttpRequest{
		RequestId:  "req-4",
		Method:     "POST",
		Path:       "/api/v1/big",
		Headers:    map[string]string{"Content-Type": "text/plain"},
		Body:       []byte("partial-"),
		ChunkIndex: 0,
		IsFinal:    false,
		TimeoutMs:  5000,
	}
	ra.handleChunk(chunk0)

	chunk1 := &pb.HttpRequest{
		RequestId:  "req-4",
		Body:       []byte("data"),
		ChunkIndex: 1,
		IsFinal:    false,
	}
	ra.handleChunk(chunk1)

	// Verify there is a pending request.
	ra.mu.Lock()
	pendingCount := len(ra.pending)
	ra.mu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("expected 1 pending request, got %d", pendingCount)
	}

	// Simulate stream close: discardAll should remove incomplete requests.
	ra.discardAll()

	ra.mu.Lock()
	pendingCount = len(ra.pending)
	ra.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("expected 0 pending requests after discardAll, got %d", pendingCount)
	}
}

func TestRequestAssembler_OrphanChunkIgnored(t *testing.T) {
	ra := newRequestAssembler()

	// Send a continuation chunk without a preceding first chunk.
	orphan := &pb.HttpRequest{
		RequestId:  "req-orphan",
		Body:       []byte("orphan-data"),
		ChunkIndex: 2,
		IsFinal:    true,
	}
	result := ra.handleChunk(orphan)
	if result != nil {
		t.Error("expected nil result for orphan chunk with no matching first chunk")
	}

	// No pending requests should exist.
	ra.mu.Lock()
	pendingCount := len(ra.pending)
	ra.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("expected 0 pending requests, got %d", pendingCount)
	}
}

func TestRequestAssembler_MultipleConcurrentRequests(t *testing.T) {
	ra := newRequestAssembler()

	// Start two multi-chunk requests interleaved.
	ra.handleChunk(&pb.HttpRequest{
		RequestId: "req-a", Method: "GET", Path: "/a",
		Headers: map[string]string{"X-Req": "a"},
		Body: []byte("a0-"), ChunkIndex: 0, IsFinal: false, TimeoutMs: 1000,
	})
	ra.handleChunk(&pb.HttpRequest{
		RequestId: "req-b", Method: "POST", Path: "/b",
		Headers: map[string]string{"X-Req": "b"},
		Body: []byte("b0-"), ChunkIndex: 0, IsFinal: false, TimeoutMs: 2000,
	})

	// Continue both.
	ra.handleChunk(&pb.HttpRequest{
		RequestId: "req-a", Body: []byte("a1"), ChunkIndex: 1, IsFinal: true,
	})
	resultA := ra.handleChunk(&pb.HttpRequest{
		RequestId: "req-a", Body: []byte("a1"), ChunkIndex: 1, IsFinal: true,
	})

	ra.handleChunk(&pb.HttpRequest{
		RequestId: "req-b", Body: []byte("b1"), ChunkIndex: 1, IsFinal: true,
	})

	// req-a was already completed, so a second final chunk for it is an orphan.
	// Let's re-test properly: we need to restart.
	ra2 := newRequestAssembler()
	ra2.handleChunk(&pb.HttpRequest{
		RequestId: "req-a", Method: "GET", Path: "/a",
		Headers: map[string]string{"X-Req": "a"},
		Body: []byte("a0-"), ChunkIndex: 0, IsFinal: false, TimeoutMs: 1000,
	})
	ra2.handleChunk(&pb.HttpRequest{
		RequestId: "req-b", Method: "POST", Path: "/b",
		Headers: map[string]string{"X-Req": "b"},
		Body: []byte("b0-"), ChunkIndex: 0, IsFinal: false, TimeoutMs: 2000,
	})

	// Finalize req-b first.
	resultB := ra2.handleChunk(&pb.HttpRequest{
		RequestId: "req-b", Body: []byte("b1"), ChunkIndex: 1, IsFinal: true,
	})
	if resultB == nil {
		t.Fatal("expected non-nil result for req-b")
	}
	if resultB.Method != "POST" || resultB.Path != "/b" {
		t.Errorf("req-b metadata wrong: method=%s path=%s", resultB.Method, resultB.Path)
	}
	if string(resultB.Body) != "b0-b1" {
		t.Errorf("req-b body wrong: got %q", string(resultB.Body))
	}

	// req-a should still be pending.
	ra2.mu.Lock()
	if _, ok := ra2.pending["req-a"]; !ok {
		t.Error("expected req-a to still be pending")
	}
	ra2.mu.Unlock()

	// Finalize req-a.
	resultA = ra2.handleChunk(&pb.HttpRequest{
		RequestId: "req-a", Body: []byte("a1"), ChunkIndex: 1, IsFinal: true,
	})
	if resultA == nil {
		t.Fatal("expected non-nil result for req-a")
	}
	if resultA.Method != "GET" || resultA.Path != "/a" {
		t.Errorf("req-a metadata wrong: method=%s path=%s", resultA.Method, resultA.Path)
	}
	if string(resultA.Body) != "a0-a1" {
		t.Errorf("req-a body wrong: got %q", string(resultA.Body))
	}
}
