package acceptfile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These mirror the reflector's wildcard-origin suite
// (server/snykbroker/reflector_dynamic_target_test.go) case for case, against
// the Router's own implementation. The Router is the authoritative accept-file
// engine; the reflector keeps its copy until it is rerouted through this one,
// and until then the two suites are how we know they agree.

func TestParseOriginAcceptsWildcardFamilies(t *testing.T) {
	for _, origin := range []string{
		"https://*.api.example.net",
		"https://*.internal.api.example.net",
		"https://*.axon.example.com",
		"https://*.api.example.net:8443",
		"https://*.something.com.internal:8443",
		"https://*.googleapis.com",
	} {
		parsed, err := parseOrigin(origin)
		require.NoError(t, err, "origin=%q", origin)
		require.True(t, parsed.isWildcard(), "origin=%q", origin)
	}
}

func TestParseOriginRejectsUnusableWildcards(t *testing.T) {
	cases := map[string]string{
		"plaintext":            "http://*.api.example.net",
		"bare wildcard":        "https://*",
		"partial label":        "https://a*.api.example.net",
		"non-leftmost":         "https://foo.*.api.example.net",
		"two wildcards":        "https://*.*.api.example.net",
		"public suffix":        "https://*.com",
		"multipart public sfx": "https://*.co.uk",
		"empty suffix":         "https://*.",
		"port zero":            "https://*.api.example.net:0",
		"port out of range":    "https://*.api.example.net:70000",
	}
	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseOrigin(origin)
			require.Error(t, err)
		})
	}
}

func TestParseOriginLeavesConcreteOriginsAlone(t *testing.T) {
	for _, origin := range []string{
		"https://beta.api.example.net",
		"http://127.0.0.1:8080",
		"https://api.github.com/v3",
	} {
		parsed, err := parseOrigin(origin)
		require.NoError(t, err, "origin=%q", origin)
		require.False(t, parsed.isWildcard(), "origin=%q", origin)
		require.NotNil(t, parsed.url)
	}
}

// A wildcard certificate covers a single label, so a multi-label match would
// authorize names that verification then refuses. If this fails, the fix is
// not to relax it.
func TestWildcardMatchesExactlyOneLabel(t *testing.T) {
	parsed, err := parseOrigin("https://*.api.example.net")
	require.NoError(t, err)
	wildcard := parsed.wildcard

	for _, host := range []string{
		"alpha.api.example.net",
		"eu-west1-compute.api.example.net",
	} {
		require.True(t, wildcard.matches(host), "host=%q", host)
	}

	for _, host := range []string{
		"a.b.api.example.net",
		"svc.internal.api.example.net",
		"api.example.net",
		"evilapi.example.net",
		"notapi.example.net",
		"alpha.api.example.net.evil.com",
		"evil.com",
	} {
		require.False(t, wildcard.matches(host), "host=%q", host)
	}
}

// parseTargetHost is not a hostname validator, so this covers only what it
// still claims: the shapes that would confuse the origin match or dial
// something other than a host. Everything else is the match's job, and
// TestPolicyRefusesValuesOutsideTheFamily covers that.
func TestParseTargetHostNormalizesAndRejects(t *testing.T) {
	host, err := parseTargetHost("ALPHA.API.Example.NET")
	require.NoError(t, err)
	require.Equal(t, "alpha.api.example.net", host)

	cases := map[string]string{
		"empty":         "",
		"comma joined":  "alpha.api.example.net,beta.api.example.net",
		"leading space": " alpha.api.example.net",
		"inner space":   "alpha api.example.net",
		"tab":           "alpha.api.example.net\t",
		"carriage":      "alpha.api.example.net\r\nX-Evil: y",
		"default port":  "alpha.api.example.net:443",
		"other port":    "alpha.api.example.net:8443",
		"ipv4":          "127.0.0.1",
		"ipv6":          "::1",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseTargetHost(value)
			require.Error(t, err)
		})
	}
}

// Ordinary hostnames must reach the dial. The double-hyphen cases are here on
// purpose: they are valid DNS but idna.Lookup rejects a hyphen in label
// positions 3-4, which IDNA reserves for "xn--". If a strict validator ever
// arrives, this is what catches it refusing a real destination.
func TestPolicyAdmitsOrdinaryHostnames(t *testing.T) {
	parsed, err := parseOrigin("https://*.example.net")
	require.NoError(t, err)
	wildcard := parsed.wildcard

	for _, value := range []string{
		"alpha.example.net",
		"eu-west1-compute.example.net",
		"my-service-01.example.net",
		"ab--cd.example.net",
		"x1--y.example.net",
		"a--b.example.net",
		"1.example.net",
		"xn--e1afmkfd.example.net",
		strings.Repeat("a", 63) + ".example.net",
	} {
		host, err := parseTargetHost(value)
		require.NoError(t, err, "value=%q", value)
		require.True(t, wildcard.matches(host), "value=%q", value)
	}
}

// The origin match is the destination control, not parseTargetHost, so these
// have to be refused by the policy however well-formed they look.
func TestPolicyRefusesValuesOutsideTheFamily(t *testing.T) {
	parsed, err := parseOrigin("https://*.api.example.net")
	require.NoError(t, err)
	wildcard := parsed.wildcard

	cases := map[string]string{
		"other family":      "alpha.evil.example.net",
		"suffix as prefix":  "alpha.api.example.net.evil.com",
		"parent of family":  "api.example.net",
		"partial label":     "evilapi.example.net",
		"two labels":        "a.b.api.example.net",
		"nested family":     "svc.internal.api.example.net",
		"bare suffix":       ".api.example.net",
		"empty inner label": "alpha..api.example.net",
		"trailing dot":      "alpha.api.example.net.",
		"path appended":     "alpha.api.example.net/v1",
		"url":               "https://alpha.api.example.net",
		"percent encoded":   "alpha%2eapi.example.net",
		"unicode label":     "alpha.api.examplı.net",
		"over-long label":   strings.Repeat("a", 64) + ".example.net",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			host, err := parseTargetHost(value)
			if err != nil {
				return // rejected before the policy ever saw it
			}
			require.False(t, wildcard.matches(host), "value=%q must not match the family", value)
		})
	}
}
