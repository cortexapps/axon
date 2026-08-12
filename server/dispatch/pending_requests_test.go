package dispatch

import (
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingRequests_BasicFlow(t *testing.T) {
	pr := NewPendingRequests(5 * time.Second)

	ch := pr.Add("req-1", "stream-1")
	assert.Equal(t, 1, pr.Count())

	err := pr.Deliver(&pb.HttpResponse{
		RequestId:  "req-1",
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       []byte(`{"ok":true}`),
		ChunkIndex: 0,
		IsFinal:    true,
	})
	require.NoError(t, err)

	resp := <-ch
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, `{"ok":true}`, string(resp.Body))
	assert.Equal(t, "application/json", resp.Headers["Content-Type"])
	assert.Equal(t, 0, pr.Count())
}

func TestPendingRequests_ChunkedResponse(t *testing.T) {
	pr := NewPendingRequests(5 * time.Second)

	ch := pr.Add("req-1", "stream-1")

	// Chunk 0 - headers + partial body.
	err := pr.Deliver(&pb.HttpResponse{
		RequestId:  "req-1",
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "text/plain"},
		Body:       []byte("Hello "),
		ChunkIndex: 0,
		IsFinal:    false,
	})
	require.NoError(t, err)

	// Chunk 1 - final body.
	err = pr.Deliver(&pb.HttpResponse{
		RequestId:  "req-1",
		Body:       []byte("World!"),
		ChunkIndex: 1,
		IsFinal:    true,
	})
	require.NoError(t, err)

	resp := <-ch
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "Hello World!", string(resp.Body))
}

func TestPendingRequests_Timeout(t *testing.T) {
	pr := NewPendingRequests(100 * time.Millisecond)

	ch := pr.Add("req-1", "stream-1")

	// Wait for timeout.
	resp, ok := <-ch
	assert.False(t, ok)
	assert.Nil(t, resp)
	assert.Equal(t, 0, pr.Count())
}

func TestPendingRequests_FailStream(t *testing.T) {
	pr := NewPendingRequests(5 * time.Second)

	ch1 := pr.Add("req-1", "stream-1")
	ch2 := pr.Add("req-2", "stream-1")
	ch3 := pr.Add("req-3", "stream-2")

	pr.FailStream("stream-1")

	// Requests on stream-1 should fail.
	_, ok := <-ch1
	assert.False(t, ok)
	_, ok = <-ch2
	assert.False(t, ok)

	// Request on stream-2 should still be pending.
	assert.Equal(t, 1, pr.Count())

	// Deliver to stream-2 request normally.
	pr.Deliver(&pb.HttpResponse{
		RequestId:  "req-3",
		StatusCode: 200,
		Body:       []byte("ok"),
		IsFinal:    true,
	})

	resp := <-ch3
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestPendingRequests_UnknownRequestID(t *testing.T) {
	pr := NewPendingRequests(5 * time.Second)

	err := pr.Deliver(&pb.HttpResponse{
		RequestId: "unknown",
		IsFinal:   true,
	})
	assert.Error(t, err)
}
