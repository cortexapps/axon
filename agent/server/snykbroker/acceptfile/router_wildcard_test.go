package acceptfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func wildcardRules(origin string) string {
	return fmt.Sprintf(`{"private":[{"method":"any","path":"/*","origin":%q}]}`, origin)
}

// ---------------------------------------------------------------------------
// Construction: a malformed policy fails at startup, not per request.
// ---------------------------------------------------------------------------

func TestRouterRejectsMalformedWildcardOriginAtConstruction(t *testing.T) {
	for name, origin := range map[string]string{
		"plaintext":     "http://*.api.example.net",
		"bare wildcard": "https://*",
		"non-leftmost":  "https://foo.*.api.example.net",
		"public suffix": "https://*.com",
		"port zero":     "https://*.api.example.net:0",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newTestRouterErr(t, wildcardRules(origin))
			require.Error(t, err, "origin=%q must be refused before any request", origin)
		})
	}
}

func TestRouterAcceptsWellFormedWildcardOriginAtConstruction(t *testing.T) {
	_, err := newTestRouterErr(t, wildcardRules("https://*.axon.example.com"))
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Wildcard origins: the authority arrives per request and is checked first.
// ---------------------------------------------------------------------------

func TestRouterWildcardRetargetsAcrossHosts(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

	for _, host := range []string{"a.axon.example.com", "b.axon.example.com"} {
		routed, err := rt.Route("GET", "/v1/things", map[string]string{HeaderTargetHost: host})
		require.NoError(t, err, "host=%q", host)
		require.Equal(t, "https://"+host+"/v1/things", routed.URL.String())
		require.Empty(t, routed.Header.Values(HeaderTargetHost),
			"routing metadata must not survive the hop")
	}
}

// The header names a host; the origin keeps deciding the port.
func TestRouterWildcardOriginPortReachesTheURL(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.something.com.internal:8443"))

	routed, err := rt.Route("GET", "/v1/things", map[string]string{
		HeaderTargetHost: "alpha.something.com.internal",
	})
	require.NoError(t, err)
	require.Equal(t, "alpha.something.com.internal:8443", routed.URL.Host)
}

// A wildcard origin never falls back to a declared host: there isn't one.
func TestRouterWildcardWithoutTargetHostIsRejected(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

	_, err := rt.Route("GET", "/v1/things", nil)
	require.ErrorIs(t, err, ErrDestinationRejected)
}

func TestRouterWildcardOutsidePolicyIsRejected(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

	_, err := rt.Route("GET", "/v1/things", map[string]string{
		HeaderTargetHost: "evil.example.com",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)
}

// A tunnel carries no per-request routing, so a family has no authority to
// upgrade against.
func TestRouterWildcardRefusesWebSocketUpgrade(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

	_, err := rt.Route("GET", "/socket", map[string]string{
		HeaderTargetHost: "a.axon.example.com",
		"Connection":     "Upgrade",
		"Upgrade":        "websocket",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)
}

func TestRouterConcreteOriginAcceptsWebSocketUpgrade(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://a.axon.example.com"))

	routed, err := rt.Route("GET", "/socket", map[string]string{
		"Connection": "Upgrade",
		"Upgrade":    "websocket",
	})
	require.NoError(t, err)
	require.Equal(t, "https://a.axon.example.com/socket", routed.URL.String())
}

// ---------------------------------------------------------------------------
// Concrete origins: never route on the header, but must still police and strip
// it, or the value reaches a third-party upstream.
// ---------------------------------------------------------------------------

func TestRouterConcreteOriginStripsTargetHostAndRejectsDisagreement(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://a.axon.example.com"))

	routed, err := rt.Route("GET", "/v1/things", map[string]string{
		HeaderTargetHost: "a.axon.example.com",
	})
	require.NoError(t, err)
	require.Equal(t, "https://a.axon.example.com/v1/things", routed.URL.String())
	require.Empty(t, routed.Header.Values(HeaderTargetHost))

	// Agreeing in a different case is still agreement.
	routed, err = rt.Route("GET", "/v1/things", map[string]string{
		HeaderTargetHost: "A.Axon.Example.COM",
	})
	require.NoError(t, err)
	require.Equal(t, "https://a.axon.example.com/v1/things", routed.URL.String())

	_, err = rt.Route("GET", "/v1/things", map[string]string{
		HeaderTargetHost: "alpha.api.example.net",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)
}

func TestRouterConcreteOriginWithoutTargetHostIsUntouched(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://a.axon.example.com"))

	routed, err := rt.Route("GET", "/v1/things", map[string]string{"x-caller": "kept"})
	require.NoError(t, err)
	require.Equal(t, "https://a.axon.example.com/v1/things", routed.URL.String())
	require.Equal(t, "kept", routed.Header.Get("x-caller"))
	require.Empty(t, routed.Header.Values(HeaderTargetHost))
}

// The tunnel hands the Router a header map, so a duplicate arrives as two keys
// that differ only in case. Fail closed rather than picking one.
func TestRouterDuplicateTargetHostIsRejected(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

	_, err := rt.Route("GET", "/v1/things", map[string]string{
		"X-Cortex-Target-Host": "a.axon.example.com",
		"x-cortex-target-host": "b.axon.example.com",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)

	// Even when both spellings agree: two values is a shape we do not accept.
	_, err = rt.Route("GET", "/v1/things", map[string]string{
		"X-Cortex-Target-Host": "a.axon.example.com",
		"x-cortex-target-host": "a.axon.example.com",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)
}

// However the caller spells it, the value is policed and removed.
func TestRouterTargetHostIsValidatedInEverySpelling(t *testing.T) {
	for _, spelling := range []string{
		"x-cortex-target-host",
		"X-Cortex-Target-Host",
		"X-CORTEX-TARGET-HOST",
		"x-Cortex-Target-host",
	} {
		t.Run(spelling, func(t *testing.T) {
			rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

			_, err := rt.Route("GET", "/v1/things", map[string]string{spelling: "evil.example.com"})
			require.ErrorIs(t, err, ErrDestinationRejected, "spelling must not bypass the policy")

			routed, err := rt.Route("GET", "/v1/things", map[string]string{spelling: "a.axon.example.com"})
			require.NoError(t, err)
			require.Empty(t, routed.Header.Values(HeaderTargetHost))
			require.Empty(t, routed.Header.Values(spelling))
		})
	}
}

func TestRouterWildcardConcurrentRetargets(t *testing.T) {
	rt := newTestRouter(t, wildcardRules("https://*.axon.example.com"))

	const workers = 16
	const perWorker = 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				host := "a.axon.example.com"
				if (w+i)%2 == 1 {
					host = "b.axon.example.com"
				}
				routed, err := rt.Route("GET", "/v1/things", map[string]string{HeaderTargetHost: host})
				if err != nil {
					errs <- fmt.Errorf("worker %d req %d: %w", w, i, err)
					return
				}
				if got := routed.URL.Host; got != host {
					errs <- fmt.Errorf("worker %d req %d: host=%q want %q", w, i, got, host)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// ---------------------------------------------------------------------------
// The shipped Google template, end to end through the Router.
// ---------------------------------------------------------------------------

func TestGoogleTemplateRoutesThroughTheWildcardFamily(t *testing.T) {
	pluginDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(pluginDir, "google-adc"),
		[]byte("#!/bin/sh\nprintf 'Bearer stub-token'\n"), 0700))

	content, err := os.ReadFile(filepath.Join("..", "accept_files", "accept.google.json"))
	require.NoError(t, err)

	rt := newTestRouter(t, string(content), pluginDir)

	routed, err := rt.Route("GET", "/storage/v1/b/my-bucket", map[string]string{
		HeaderTargetHost: "storage.googleapis.com",
	})
	require.NoError(t, err)
	require.Equal(t, "https://storage.googleapis.com/storage/v1/b/my-bucket", routed.URL.String())
	require.Equal(t, "Bearer stub-token", routed.Header.Get("authorization"))
	require.Empty(t, routed.Header.Values(HeaderTargetHost))

	// A host outside the family is refused even though the rule path matches.
	_, err = rt.Route("GET", "/storage/v1/b/my-bucket", map[string]string{
		HeaderTargetHost: "evil.example.com",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)

	// And the family is not a free pass to any depth under it.
	_, err = rt.Route("GET", "/storage/v1/b", map[string]string{
		HeaderTargetHost: "a.b.googleapis.com",
	})
	require.ErrorIs(t, err, ErrDestinationRejected)
}

// Routing metadata is taken off the request before rules are consulted, so it
// cannot be used to reach a rule the caller was not meant to reach.
func TestRouterTargetHostCannotSelectARule(t *testing.T) {
	rt := newTestRouter(t, `{"private":[
		{"method":"any","path":"/*","origin":"https://gated.example.com",
		 "valid":[{"header":"x-cortex-target-host","values":["alpha.axon.example.com"]}]},
		{"method":"any","path":"/*","origin":"https://*.axon.example.com"}
	]}`)

	routed, err := rt.Route("GET", "/v1/things", map[string]string{
		HeaderTargetHost: "alpha.axon.example.com",
	})
	require.NoError(t, err)
	require.Equal(t, "https://alpha.axon.example.com/v1/things", routed.URL.String(),
		"the gated rule must not be reachable through routing metadata")
}

// An origin that resolves to nothing dialable warns at construction and fails
// the requests that match it, rather than stopping the agent. The snyk-broker
// path behaves the same way — it registers the rule and fails per request — so
// a deployment carrying one still starts on the tunnel.
func TestRouterWarnsOnUndialableOriginAndFailsPerRequest(t *testing.T) {
	// A scheme-less origin carrying a port parses as an opaque URL with no
	// host, which nothing can dial.
	t.Setenv("NOT_A_URL", "github.com:8080")
	rt, err := newTestRouterErr(t, `{"private":[
		{"method":"any","path":"/*","origin":"${NOT_A_URL}"}]}`)
	require.NoError(t, err, "a bad origin must not stop the agent")

	_, err = rt.Route("GET", "/x", nil)
	require.Error(t, err, "but the request it would have carried has to fail")

	// A rule with no origin at all is the same story.
	rt, err = newTestRouterErr(t, `{"private":[{"method":"any","path":"/*"}]}`)
	require.NoError(t, err)
	_, err = rt.Route("GET", "/x", nil)
	require.Error(t, err)

	// So is an origin that will not parse at all. Only a malformed *wildcard*
	// stops the agent; a bad concrete origin is caught before parseOrigin sees
	// it and left to fail per request.
	t.Setenv("UNPARSEABLE", "http://%zz")
	rt, err = newTestRouterErr(t, `{"private":[
		{"method":"any","path":"/*","origin":"${UNPARSEABLE}"}]}`)
	require.NoError(t, err)
	_, err = rt.Route("GET", "/x", nil)
	require.Error(t, err)
}
