package broker

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
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
		logger: logger,
	}
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

// jsonAPIBody wraps request bodies in the JSONAPI envelope expected by the dispatcher.
type jsonAPIBody struct {
	Data jsonAPIData `json:"data"`
}

type jsonAPIData struct {
	Attributes map[string]string `json:"attributes"`
}

// ClientConnected notifies the BROKER_SERVER that a client has connected.
// POST /internal/brokerservers/{serverId}/connections/{hashedToken}?broker_client_id=...&request_type=client-connected&version=...
func (c *Client) ClientConnected(token Token, clientID string, metadata map[string]string) error {
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

	return c.doRequest(http.MethodPost, path, params, body)
}

// ClientDisconnected notifies the BROKER_SERVER that a client has disconnected.
// DELETE /internal/brokerservers/{serverId}/connections/{hashedToken}?broker_client_id=...&version=...
func (c *Client) ClientDisconnected(token Token, clientID string) error {
	if !c.IsConfigured() {
		return nil
	}

	path := fmt.Sprintf("/internal/brokerservers/%s/connections/%s", c.serverID, token.Hashed())

	params := url.Values{}
	if clientID != "" {
		params.Set("broker_client_id", clientID)
	}

	return c.doRequest(http.MethodDelete, path, params, nil)
}

// ServerStarting notifies the BROKER_SERVER that this server instance has started.
// POST /internal/brokerservers/{serverId}?version=...
func (c *Client) ServerStarting(hostname string) error {
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

	return c.doRequest(http.MethodPost, path, nil, body)
}

// ServerStopping notifies the BROKER_SERVER that this server instance is shutting down.
// DELETE /internal/brokerservers/{serverId}?version=...
func (c *Client) ServerStopping() error {
	if !c.IsConfigured() {
		return nil
	}

	path := fmt.Sprintf("/internal/brokerservers/%s", c.serverID)
	return c.doRequest(http.MethodDelete, path, nil, nil)
}

// doRequest sends a request to the dispatcher API with the required version param and content type.
func (c *Client) doRequest(method, path string, params url.Values, body any) error {
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

	var reqBody *bytes.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	var httpReq *http.Request
	if reqBody != nil {
		httpReq, err = http.NewRequest(method, u.String(), reqBody)
	} else {
		httpReq, err = http.NewRequest(method, u.String(), nil)
	}
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/vnd.api+json")
	httpReq.Header.Set("Connection", "Keep-Alive")
	httpReq.Header.Set("Keep-Alive", "timeout=60, max=10")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.logger.Warn("BROKER_SERVER request failed",
			zap.String("method", method),
			zap.String("path", path),
			zap.Error(err),
		)
		return fmt.Errorf("broker-server %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		c.logger.Warn("BROKER_SERVER returned non-success status",
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("status", resp.StatusCode),
		)
		return fmt.Errorf("broker-server %s %s: status %d", method, path, resp.StatusCode)
	}

	return nil
}
