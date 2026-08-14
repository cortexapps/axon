package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

const (
	defaultMaxAttempts  = 4
	defaultRetryBackoff = 500 * time.Millisecond
	maxRetryBackoff     = 5 * time.Second
)

const dispatcherAPIVersion = "2022-12-02~experimental"

// Client wraps all BROKER_SERVER (dispatcher) HTTP API interactions.
// Paths match the snyk-broker dispatcher API:
//
//	POST   /internal/brokerservers/{serverId}                              — server starting
//	DELETE /internal/brokerservers/{serverId}                              — server stopping
//	POST   /internal/brokerservers/{serverId}/connections/{hashedToken}    — client connected
//	DELETE /internal/brokerservers/{serverId}/connections/{hashedToken}    — client disconnected
type Client struct {
	baseURL    string
	serverID   string
	httpClient *http.Client
	logger     *zap.Logger

	// Retry policy for transient failures (network errors, 429, 5xx).
	maxAttempts  int
	retryBackoff time.Duration
}

// NewClient creates a new BROKER_SERVER client.
// If baseURL is empty, all operations are no-ops (for testing/dev).
func NewClient(baseURL string, serverID string, logger *zap.Logger) *Client {
	return &Client{
		baseURL:  baseURL,
		serverID: serverID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger:       logger,
		maxAttempts:  defaultMaxAttempts,
		retryBackoff: defaultRetryBackoff,
	}
}

// SetRetryPolicy overrides the transient-failure retry policy (mainly for
// tests). maxAttempts includes the first try; backoff doubles per retry up
// to maxRetryBackoff.
func (c *Client) SetRetryPolicy(maxAttempts int, backoff time.Duration) {
	if maxAttempts > 0 {
		c.maxAttempts = maxAttempts
	}
	if backoff > 0 {
		c.retryBackoff = backoff
	}
}

// statusError marks a non-success HTTP response so isRetryable can
// distinguish transient (429, 5xx) from permanent (other 4xx) failures.
type statusError struct {
	method, path string
	code         int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("broker-server %s %s: status %d", e.method, e.path, e.code)
}

// isRetryable reports whether an attempt failure is worth retrying:
// network-level errors and 429/5xx responses are transient; other HTTP
// statuses are permanent.
func isRetryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return true // network-level failure
}

// IsConfigured returns true if a BROKER_SERVER URL is set.
func (c *Client) IsConfigured() bool {
	return c.baseURL != ""
}

// Token encapsulates a raw broker token and its SHA-256 hash.
// Create via NewToken (from raw) or TokenFromHash (from pre-hashed).
type Token struct {
	raw    string
	hashed string
}

// NewToken creates a Token from a raw broker token, computing the SHA-256 hash.
func NewToken(raw string) Token {
	h := sha256.Sum256([]byte(raw))
	return Token{
		raw:    raw,
		hashed: fmt.Sprintf("%x", h[:]),
	}
}

// TokenFromHash creates a Token from an already-hashed value (no raw token available).
func TokenFromHash(hashed string) Token {
	return Token{hashed: hashed}
}

// Raw returns the original unhashed token. May be empty if created via TokenFromHash.
func (t Token) Raw() string { return t.raw }

// Hashed returns the SHA-256 hex hash of the token.
func (t Token) Hashed() string { return t.hashed }

// String returns a safe representation of the token for logging,
// showing only the first 12 characters of the hash to prevent accidental
// raw token exposure via %v or %s.
func (t Token) String() string {
	h := t.hashed
	if len(h) > 12 {
		h = h[:12]
	}
	return fmt.Sprintf("Token{hash=%s...}", h)
}

// jsonAPIBody wraps request bodies in the JSONAPI envelope expected by the dispatcher.
type jsonAPIBody struct {
	Data jsonAPIData `json:"data"`
}

type jsonAPIData struct {
	Attributes map[string]string `json:"attributes"`
}

// ClientConnected notifies the BROKER_SERVER that a client has connected.
// POST /internal/brokerservers/{serverId}/connections/{hashedToken}?broker_client_id=...&request_type=client-connected&version=...
func (c *Client) ClientConnected(ctx context.Context, token Token, clientID string, metadata map[string]string) error {
	if !c.IsConfigured() {
		return nil
	}

	path := fmt.Sprintf("/internal/brokerservers/%s/connections/%s", c.serverID, token.Hashed())

	params := url.Values{}
	if clientID != "" {
		params.Set("broker_client_id", clientID)
	}
	params.Set("request_type", "client-connected")

	body := jsonAPIBody{
		Data: jsonAPIData{
			Attributes: map[string]string{
				"health_check_link": fmt.Sprintf("http://%s/healthcheck", c.serverID),
			},
		},
	}

	// Merge any additional metadata into attributes.
	if metadata != nil {
		for k, v := range metadata {
			body.Data.Attributes[k] = v
		}
	}

	return c.doRequest(ctx, http.MethodPost, path, params, body)
}

// ClientDisconnected notifies the BROKER_SERVER that a client has disconnected.
// DELETE /internal/brokerservers/{serverId}/connections/{hashedToken}?broker_client_id=...&version=...
func (c *Client) ClientDisconnected(ctx context.Context, token Token, clientID string) error {
	if !c.IsConfigured() {
		return nil
	}

	path := fmt.Sprintf("/internal/brokerservers/%s/connections/%s", c.serverID, token.Hashed())

	params := url.Values{}
	if clientID != "" {
		params.Set("broker_client_id", clientID)
	}

	return c.doRequest(ctx, http.MethodDelete, path, params, nil)
}

// ServerStarting notifies the BROKER_SERVER that this server instance has started.
// POST /internal/brokerservers/{serverId}?version=...
func (c *Client) ServerStarting(ctx context.Context, hostname string) error {
	if !c.IsConfigured() {
		return nil
	}

	path := fmt.Sprintf("/internal/brokerservers/%s", c.serverID)

	body := jsonAPIBody{
		Data: jsonAPIData{
			Attributes: map[string]string{
				"health_check_link": fmt.Sprintf("http://%s/healthcheck", hostname),
			},
		},
	}

	return c.doRequest(ctx, http.MethodPost, path, nil, body)
}

// ServerStopping notifies the BROKER_SERVER that this server instance is shutting down.
// DELETE /internal/brokerservers/{serverId}?version=...
func (c *Client) ServerStopping(ctx context.Context) error {
	if !c.IsConfigured() {
		return nil
	}

	path := fmt.Sprintf("/internal/brokerservers/%s", c.serverID)
	return c.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// doRequest sends a request to the dispatcher API with the required version
// param and content type, retrying transient failures (network errors, 429,
// 5xx) with jittered exponential backoff, bounded by ctx and the client's
// retry policy. Permanent failures (other 4xx) return immediately.
func (c *Client) doRequest(ctx context.Context, method, path string, params url.Values, body any) error {
	if ctx == nil {
		ctx = context.Background()
	}

	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	// Merge params and always add version.
	q := u.Query()
	if params != nil {
		for k, vs := range params {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
	}
	q.Set("version", dispatcherAPIVersion)
	u.RawQuery = q.Encode()

	// Marshal once; each attempt gets a fresh reader.
	var jsonBody []byte
	if body != nil {
		jsonBody, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
	}

	backoff := c.retryBackoff
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			// Jittered backoff so a fleet of servers doesn't retry in sync.
			jitter := time.Duration(rand.Int64N(int64(backoff) / 2))
			select {
			case <-ctx.Done():
				return fmt.Errorf("broker-server %s %s: %w (last error: %w)", method, path, ctx.Err(), lastErr)
			case <-time.After(backoff/2 + jitter):
			}
			if backoff *= 2; backoff > maxRetryBackoff {
				backoff = maxRetryBackoff
			}
		}

		lastErr = c.attempt(ctx, method, u.String(), path, jsonBody)
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
		c.logger.Warn("BROKER_SERVER request failed; will retry",
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("attempt", attempt),
			zap.Int("maxAttempts", c.maxAttempts),
			zap.Error(lastErr),
		)
	}
	return fmt.Errorf("broker-server %s %s failed after %d attempts: %w", method, path, c.maxAttempts, lastErr)
}

// attempt performs a single request.
func (c *Client) attempt(ctx context.Context, method, fullURL, path string, jsonBody []byte) error {
	var reader io.Reader
	if jsonBody != nil {
		reader = bytes.NewReader(jsonBody)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/vnd.api+json")
	httpReq.Header.Set("Connection", "Keep-Alive")
	httpReq.Header.Set("Keep-Alive", "timeout=60, max=10")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("broker-server %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return &statusError{method: method, path: path, code: resp.StatusCode}
	}
	return nil
}
