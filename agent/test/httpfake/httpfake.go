// Package httpfake serves canned HTTP responses through a body that enforces
// the parts of the net/http response body contract that callers get wrong.
//
// A real response body is backed by a pooled connection, so:
//
//   - it must be read to EOF and closed before the connection goes back to
//     the pool, otherwise the connection is held for the life of the process;
//   - reads fail once the request's context is done, because the transport
//     tears the connection down. net/http still returns whatever bytes it had
//     buffered, so small bodies survive a cancel by accident. Body models the
//     rule strictly, failing any read after cancel, because the buffered
//     threshold is not something a test can control;
//   - reads fail after Close.
//
// Tests that build an http.Client around a Transport get all three rules on
// every request without opting in.
package httpfake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Body is a response body bound to the context of the request it answered.
type Body struct {
	io.Reader
	ctx context.Context

	// EOF records whether Read returned io.EOF.
	EOF bool
	// Closed records whether Close was called.
	Closed bool
}

// NewBody returns a Body bound to ctx, for fakes that sit above the HTTP
// client but still hand a response body to code that must drain and close it.
func NewBody(ctx context.Context, r io.Reader) *Body {
	return &Body{Reader: r, ctx: ctx}
}

func (b *Body) Read(p []byte) (int, error) {
	if b.Closed {
		return 0, http.ErrBodyReadAfterClose
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.EOF = true
	}
	return n, err
}

func (b *Body) Close() error {
	b.Closed = true
	return nil
}

// Transport answers each request with the next entry in Responses, wrapping
// its body in a Body bound to that request's context. Bodies holds the Body
// served to each request, in order, so a test can check what the caller did
// with it. Running out of Responses is an error, not a panic, so the failure
// surfaces in the code under test the way a transport error would.
type Transport struct {
	Responses []*http.Response
	Bodies    []*Body

	// canned caches each response's body bytes so the same *http.Response
	// can be served more than once, each time from the start.
	canned map[*http.Response][]byte
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	i := len(t.Bodies)
	if i >= len(t.Responses) {
		return nil, fmt.Errorf("httpfake: no canned response for request %d (%s %s)", i+1, r.Method, r.URL)
	}
	data, err := t.bodyBytes(t.Responses[i])
	if err != nil {
		return nil, fmt.Errorf("httpfake: reading canned response %d: %w", i+1, err)
	}
	resp := *t.Responses[i]
	body := NewBody(r.Context(), bytes.NewReader(data))
	resp.Body = body
	resp.Request = r
	t.Bodies = append(t.Bodies, body)
	return &resp, nil
}

func (t *Transport) bodyBytes(resp *http.Response) ([]byte, error) {
	if data, ok := t.canned[resp]; ok {
		return data, nil
	}
	var data []byte
	if resp.Body != nil {
		var err error
		if data, err = io.ReadAll(resp.Body); err != nil {
			return nil, err
		}
		resp.Body.Close()
	}
	if t.canned == nil {
		t.canned = map[*http.Response][]byte{}
	}
	t.canned[resp] = data
	return data, nil
}

// Response builds a canned response with the given status and body text.
// Set headers on the result before handing it to a Transport.
func Response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Client returns an http.Client that serves the given responses in order,
// and the Transport behind it for inspecting the bodies afterwards.
func Client(responses ...*http.Response) (*http.Client, *Transport) {
	tr := &Transport{Responses: responses}
	return &http.Client{Transport: tr}, tr
}
