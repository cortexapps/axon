package snykbroker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// connTap records every byte the agent writes to an upstream connection, so a
// test can assert on what actually left the process rather than on what a
// handler happened to observe.
type connTap struct {
	mu      sync.Mutex
	written bytes.Buffer
}

func (tap *connTap) wrap(conn net.Conn) net.Conn {
	return &tappedConn{Conn: conn, tap: tap}
}

func (tap *connTap) bytesWritten() []byte {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	return append([]byte(nil), tap.written.Bytes()...)
}

type tappedConn struct {
	net.Conn
	tap *connTap
}

func (c *tappedConn) Write(p []byte) (int, error) {
	c.tap.mu.Lock()
	c.tap.written.Write(p)
	c.tap.mu.Unlock()
	return c.Conn.Write(p)
}

// newCapturingLogger returns a logger and a reader over everything it emitted.
// Like newTestLogger it stays safe once the test finishes, because request
// goroutines outlive the test that started them.
func newCapturingLogger(t *testing.T) (*zap.Logger, func() string) {
	t.Helper()
	w := &syncBuffer{}
	logger := zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		zapcore.AddSync(w),
		zapcore.DebugLevel,
	))
	return logger, w.contents
}

// syncBuffer is a bytes.Buffer safe to read while writers are still going.
// Its writers outlive the test that started them — the supervisor's line
// pumps keep draining after Wait() returns, and request goroutines keep
// logging after the reflector test finishes — so an unguarded buffer races
// with any assertion on its contents.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) contents() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newReflectorWith(t *testing.T, logger *zap.Logger, transport *http.Transport) *RegistrationReflector {
	t.Helper()
	rr := newReflectorWithDrain(t, RegistrationReflectorParams{
		Logger:    logger,
		Registry:  prometheus.NewRegistry(),
		Transport: transport,
		Config:    config.AgentConfig{ReflectorWebSocketUpgrade: true},
	})
	t.Cleanup(func() { rr.Stop() })
	return rr
}

// The WebSocket path bypasses the Director and writes the caller's header set
// verbatim, so the single removal in ServeHTTP is the only thing standing
// between an internal routing value and a third-party upstream.
//
// The origin is "localhost" rather than a synthetic name because the tunnel
// dials with a plain net.Dialer: it ignores the transport's DialContext, so
// nothing else here can stand in for DNS. It must also be a name and not
// httptest's 127.0.0.1, since an IP literal is never an accepted authority.
func TestTargetHostHeaderNeverReachesAWebSocketUpstream(t *testing.T) {
	env := newTestReflectorEnv(t)

	var mu sync.Mutex
	var upstream http.Header
	env.Router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstream = r.Header.Clone()
		mu.Unlock()

		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, message); err != nil {
				return
			}
		}
	})

	origin := "http://localhost:" + portOf(t, env.Server.URL)
	proxyURI := env.Reflector.ProxyURI(origin,
		WithHeaders(map[string]string{"Authorization": "Bearer minted"}),
	)

	header := http.Header{}
	header.Set(HeaderTargetHost, "localhost")
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+proxyURI[len("http"):]+"/ws", header)
	require.NoError(t, err)
	defer conn.Close()
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("ping")))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "ping", string(message))

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, upstream, "the upstream never received the upgrade")
	require.Empty(t, upstream.Values(HeaderTargetHost),
		"the routing header reached the upstream through the tunnel")
}

// Validation and removal both look up the canonical header key. Go canonicalises
// during parsing, so every spelling arrives under the same key - but only over a
// real socket, which is why this test writes request bytes rather than building
// a request in process.
//
// A missed spelling would not merely skip validation: the value would also
// survive removal and be forwarded. The two outcomes are distinguishable, so
// this asserts the forwarded case never happens.
func TestTargetHostHeaderIsValidatedInEverySpelling(t *testing.T) {
	for _, spelling := range []string{
		"X-Cortex-Target-Host",
		"x-cortex-target-host",
		"X-CORTEX-TARGET-HOST",
		"x-Cortex-target-host",
	} {
		t.Run(spelling, func(t *testing.T) {
			env := newTestReflectorEnv(t)
			backend := newRecordingBackend(t, "reached")

			origin := "http://localhost:" + portOf(t, backend.server.URL)
			proxyURI := env.Reflector.ProxyURI(origin,
				WithHeaders(map[string]string{"Authorization": "Bearer minted"}),
			)

			status := rawRequest(t, env.Reflector.server.Port(),
				proxyPath(t, proxyURI)+"/v1/things",
				spelling+": attacker-chosen.example.com")

			require.Equal(t, http.StatusForbidden, status,
				"a value disagreeing with the declared origin must be rejected")
			hits, _, _ := backend.snapshot()
			require.Equal(t, 0, hits, "the rejected request reached the upstream")
		})
	}
}

// A hostile resolver is the case certificate verification exists to catch: DNS
// answers for an authorized name with a host that cannot prove it.
//
// The transport verifies against the backend's own certificate pool, so the
// name mismatch is the only reason the handshake can fail. Asserting on the
// bytes the agent wrote is what makes this prove the credential was never sent
// rather than that the request failed: the handshake aborts before any
// application data, so a bearer token appearing in that capture would be the
// leak.
func TestHostileResolverReceivesNoAuthorizationHeader(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	pool := x509.NewCertPool()
	pool.AddCert(backend.server.Certificate())

	tap := &connTap{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if host != "alpha.axon.example.com" {
				return nil, fmt.Errorf("no backend registered for %q", host)
			}
			conn, err := net.Dial(network, backend.hostPort(t))
			if err != nil {
				return nil, err
			}
			return tap.wrap(conn), nil
		},
		// Verification on, and the backend's own root: a mismatched name is
		// then the only way the handshake can fail.
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}

	rr := newReflectorWith(t, newTestLogger(t), transport)
	headers := acceptfile.NewResolverMapFromMap(map[string]string{"Authorization": "Bearer minted"})
	proxyURI := rr.ProxyURI("https://*.axon.example.com", WithHeadersResolver(headers))

	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "alpha.axon.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits, "the request was served despite the certificate mismatch")

	written := tap.bytesWritten()
	require.NotEmpty(t, written, "nothing was dialed, so this proves nothing")
	require.NotContains(t, string(written), "Authorization")
	require.NotContains(t, string(written), "Bearer minted")
}

// A redirect is a new destination decision, and the caller owns it. The relay
// returns the 3xx so that the next request re-enters authority capture.
//
// This holds today only because httputil.ReverseProxy dials through an
// http.Transport, which does not follow redirects. A change to a
// redirect-following client would carry the injected credential to whatever
// Location named, out of policy or not, and nothing else would fail.
func TestUpstreamRedirectIsReturnedRatherThanFollowed(t *testing.T) {
	redirectTarget := newRecordingTLSBackend(t, "redirect target")
	authorized := &recordingBackend{}
	authorized.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized.mu.Lock()
		authorized.hits++
		authorized.header = r.Header.Clone()
		authorized.mu.Unlock()
		w.Header().Set("Location", "https://evil.example.com/v1/things")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(authorized.server.Close)

	backends := map[string]*recordingBackend{
		"a.axon.example.com": authorized,
		// Registered so a followed redirect would connect. It must not.
		"evil.example.com": redirectTarget,
	}
	transport := routedTransport(t, backends)
	rr := newReflectorWith(t, newTestLogger(t), transport)

	// Reach the redirect target through the same dialer first, so the zero
	// below is the transport declining to follow rather than a dead route.
	probe := &http.Client{Transport: transport}
	resp, err := probe.Get("https://evil.example.com/v1/things")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	reachable, _, _ := redirectTarget.snapshot()
	require.Equal(t, 1, reachable)

	headers := acceptfile.NewResolverMapFromMap(map[string]string{"Authorization": "Bearer minted"})
	proxyURI := rr.ProxyURI("https://*.axon.example.com", WithHeadersResolver(headers))

	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "a.axon.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "the redirect was consumed rather than returned")
	require.Equal(t, "https://evil.example.com/v1/things", rec.Header().Get("Location"))

	authorizedHits, _, authorizedHeader := authorized.snapshot()
	require.Equal(t, 1, authorizedHits)
	require.Equal(t, "Bearer minted", authorizedHeader.Get("Authorization"))

	hits, _, header := redirectTarget.snapshot()
	require.Equal(t, reachable, hits, "the redirect was followed to a destination nobody authorized")
	require.Empty(t, header.Get("Authorization"),
		"the injected credential was carried to the redirect target")
}

// The rejection reason is safe to record; the requested authority is not. An
// operator reading logs, or anything shipping them onward, must not be handed
// the value a caller asked for.
func TestRoutingMetadataReachesNeitherTheResponseNorTheLogs(t *testing.T) {
	const rejected = "attacker-chosen.example.com"
	const accepted = "a.axon.example.com"

	backend := newRecordingTLSBackend(t, "a")
	logger, logged := newCapturingLogger(t)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, backend.hostPort(t))
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	rr := newReflectorWith(t, logger, transport)

	headers := acceptfile.NewResolverMapFromMap(map[string]string{"Authorization": "Bearer minted"})
	proxyURI := rr.ProxyURI("https://*.axon.example.com", WithHeadersResolver(headers))
	path := proxyPath(t, proxyURI) + "/v1/things"

	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set(HeaderTargetHost, rejected)
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), ErrClassDestinationRejected)
	require.Empty(t, rec.Header().Values(HeaderTargetHost))

	req = httptest.NewRequest("GET", path, nil)
	req.Header.Set(HeaderTargetHost, accepted)
	rec = httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Values(HeaderTargetHost),
		"the routing header was echoed back to the caller")

	output := logged()
	require.NotContains(t, output, rejected, "a rejected authority was written to the log")
	require.NotContains(t, output, accepted, "a routed authority was written to the log")
	require.Contains(t, output, ErrClassDestinationRejected,
		"the rejection itself must still be recorded")
}

// A rule header value is a credential. Registration logs the rule, so it must
// log the names and not the map.
//
// The map was safe to log only by accident: zap falls back to json.Marshal,
// which fails on the resolver's func field and drops the whole field. Making
// ValueResolver serializable would have written every accept-file header value
// to the log at INFO, with nothing to catch it.
func TestRegistrationLogsHeaderNamesRatherThanValues(t *testing.T) {
	const secret = "Bearer sk-literal-value"

	logger, logged := newCapturingLogger(t)
	rr := newReflectorWith(t, logger, nil)
	backend := newRecordingBackend(t, "a")

	rr.ProxyURI(backend.server.URL, WithHeaders(map[string]string{
		"Authorization":        secret,
		"X-GitHub-Api-Version": "2022-11-28",
	}))

	output := logged()
	require.NotContains(t, output, secret, "a rule credential was written to the log")
	require.NotContains(t, output, "sk-literal-value")
	require.Contains(t, output, "Authorization", "the header names are still worth recording")
	require.Contains(t, output, "X-GitHub-Api-Version")
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Port()
}

// rawRequest writes request bytes to the reflector's own listener and returns
// the status code, so header names arrive exactly as spelled here.
func rawRequest(t *testing.T, port int, path string, headerLines ...string) int {
	t.Helper()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	request := "GET " + path + " HTTP/1.1\r\nHost: 127.0.0.1\r\n"
	for _, line := range headerLines {
		request += line + "\r\n"
	}
	request += "Connection: close\r\n\r\n"
	_, err = conn.Write([]byte(request))
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}
