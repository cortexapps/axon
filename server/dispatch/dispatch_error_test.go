package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// A caller hanging up mid-upload fails the request-body read before the agent
// has answered. That path used to cancel the call without delivering an
// error, so Dispatch read nil from the closed errC and returned (nil, nil),
// and the HTTP adapter then dereferenced the nil response.
func TestDispatch_RequestBodyFailureBeforeStartIsAnError(t *testing.T) {
	d, _, token, _, _ := newTestDispatcher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	readErr := errors.New("caller hung up")
	resp, err := d.Dispatch(ctx, token, &Request{
		PseudoHeaders: map[string]string{":method": "POST", ":path": "/upload"},
		Body:          failingReader{err: readErr},
	})
	require.Nil(t, resp)
	var dispErr *DispatchError
	require.ErrorAs(t, err, &dispErr, "Dispatch must return *DispatchError, got %v", err)
	assert.Equal(t, FailureRequestBody, dispErr.Kind)
	assert.ErrorIs(t, err, readErr)
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}
