package httpfake

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func get(t *testing.T, client *http.Client, ctx context.Context) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://example.com/", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

func TestBody_RecordsEOFAndClose(t *testing.T) {
	client, tr := Client(Response(http.StatusOK, "hello"))
	resp := get(t, client, context.Background())

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(b))
	require.True(t, tr.Bodies[0].EOF)
	require.False(t, tr.Bodies[0].Closed)

	require.NoError(t, resp.Body.Close())
	require.True(t, tr.Bodies[0].Closed)
}

func TestBody_FailsReadAfterRequestContextIsDone(t *testing.T) {
	client, _ := Client(Response(http.StatusOK, "hello"))
	ctx, cancel := context.WithCancel(context.Background())
	resp := get(t, client, ctx)
	cancel()

	_, err := io.ReadAll(resp.Body)
	require.ErrorIs(t, err, context.Canceled)
}

func TestBody_FailsReadAfterClose(t *testing.T) {
	client, _ := Client(Response(http.StatusOK, "hello"))
	resp := get(t, client, context.Background())
	require.NoError(t, resp.Body.Close())

	_, err := io.ReadAll(resp.Body)
	require.ErrorIs(t, err, http.ErrBodyReadAfterClose)
}

func TestTransport_ServesResponsesInOrderThenFails(t *testing.T) {
	client, tr := Client(Response(http.StatusTooManyRequests, ""), Response(http.StatusOK, "ok"))

	first := get(t, client, context.Background())
	require.Equal(t, http.StatusTooManyRequests, first.StatusCode)
	second := get(t, client, context.Background())
	require.Equal(t, http.StatusOK, second.StatusCode)
	require.Len(t, tr.Bodies, 2)

	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	_, err := client.Do(req)
	require.ErrorContains(t, err, "no canned response for request 3")
}

func TestTransport_DoesNotMutateCannedResponse(t *testing.T) {
	canned := Response(http.StatusOK, "ok")
	client, _ := Client(canned, canned)

	for i := 0; i < 2; i++ {
		resp := get(t, client, context.Background())
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "ok", string(b), "request %d", i+1)
	}
	require.Nil(t, canned.Request)
}
