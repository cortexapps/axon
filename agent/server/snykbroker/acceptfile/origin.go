package acceptfile

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
)

// HeaderTargetHost is routing input, never authorization: the origin declared
// in the accept file stays the policy, and this value is only ever checked
// against it.
const HeaderTargetHost = "x-cortex-target-host"

// ErrDestinationRejected marks a request that matched a rule but named a
// destination the rule does not authorize. Transports map it to 403.
var ErrDestinationRejected = errors.New("destination rejected")

// wildcardOrigin is the family of hosts a wildcard origin authorizes.
type wildcardOrigin struct {
	// Keeps the leading dot, so matching is label-aligned: ".example.com".
	suffix string
}

// matches reports whether host sits exactly one label under the wildcard.
//
// One label, not one-or-more: a wildcard certificate covers a single label, so
// a multi-label match would authorize names that verification then refuses. Do
// not relax this to a plain suffix test.
func (w wildcardOrigin) matches(host string) bool {
	if !strings.HasSuffix(host, w.suffix) {
		return false
	}
	label := host[:len(host)-len(w.suffix)]
	return label != "" && !strings.Contains(label, ".")
}

// parsedOrigin is a rule's origin after parsing: the URL to dial, and the
// family it authorizes when the operator wrote a wildcard. The two always
// travel together — every destination decision needs both, since a nil wildcard
// is what makes an origin concrete.
type parsedOrigin struct {
	url      *url.URL
	wildcard *wildcardOrigin
}

// isWildcard reports whether the origin authorizes a family rather than naming
// one host.
func (p parsedOrigin) isWildcard() bool { return p.wildcard != nil }

// parsedOrigins caches parseOrigin by origin string. A rotating pool resolves to
// a handful of distinct values, so this keeps the per-request cost to a map
// lookup rather than a URL parse plus a public-suffix walk.
var parsedOrigins sync.Map

type originCacheEntry struct {
	parsed parsedOrigin
	err    error
}

// parseOrigin returns the URL to dial and, for a wildcard origin, the family it
// authorizes. Routers run it at construction so a malformed policy fails there
// rather than per request.
func parseOrigin(origin string) (parsedOrigin, error) {
	if cached, ok := parsedOrigins.Load(origin); ok {
		entry := cached.(originCacheEntry)
		return entry.parsed, entry.err
	}
	parsed, err := parseOriginUncached(origin)
	parsedOrigins.Store(origin, originCacheEntry{parsed: parsed, err: err})
	return parsed, err
}

func parseOriginUncached(origin string) (parsedOrigin, error) {
	asURL, err := url.Parse(origin)
	if err != nil {
		return parsedOrigin{}, fmt.Errorf("invalid origin %q: %w", origin, err)
	}

	if !strings.Contains(asURL.Host, "*") {
		// Deliberately not port-checked. A concrete origin names one host the
		// operator picked, and the agent's own origin carries port 0 until its
		// listener binds.
		return parsedOrigin{url: asURL}, nil
	}

	// Certificate verification is the destination control, so a family cannot
	// be served over plaintext.
	if asURL.Scheme != "https" {
		return parsedOrigin{}, fmt.Errorf("wildcard origin %q must use https", origin)
	}
	// A wildcard origin's port is dialed against a host the operator never
	// wrote down, so an unusable one has to stop the agent rather than surface
	// as a per-request failure.
	if err := validatePort(asURL.Port()); err != nil {
		return parsedOrigin{}, fmt.Errorf("wildcard origin %q: %w", origin, err)
	}

	host := asURL.Hostname()
	suffix, found := strings.CutPrefix(host, "*.")
	if !found || strings.Contains(suffix, "*") {
		return parsedOrigin{}, fmt.Errorf("wildcard origin %q must have the form https://*.example.com", origin)
	}
	// "https://*." alone would authorize every host, and an empty label would
	// misalign the match against the dot this stores.
	if suffix == "" || strings.HasPrefix(suffix, ".") ||
		strings.HasSuffix(suffix, ".") || strings.Contains(suffix, "..") {
		return parsedOrigin{}, fmt.Errorf("wildcard origin %q has an empty label", origin)
	}
	// ICANN division only. The private section holds ordinary registrable
	// domains added for cookie scoping, and rejecting those would rule out
	// legitimate families.
	if publicSuffix, icann := publicsuffix.PublicSuffix(suffix); icann && publicSuffix == suffix {
		return parsedOrigin{}, fmt.Errorf("wildcard origin %q must contain a registrable domain", origin)
	}

	return parsedOrigin{url: asURL, wildcard: &wildcardOrigin{suffix: "." + suffix}}, nil
}

// parseTargetHost normalizes a header value into a host to match against the
// origin, or reports why it is unusable.
//
// Deliberately not a hostname validator. The destination controls are
// wildcardOrigin.matches, which confines the value to the family whatever it
// contains, and the certificate check behind it, which no made-up name can
// pass. What is left here is the handful of shapes that would confuse those two
// or dial something other than a host in the family.
//
// The value names a host and nothing else. The origin decides the port, so a
// value carrying one is either restating it or trying to change it, and there
// is no reason to tell those apart.
func parseTargetHost(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("empty target host")
	}
	// A comma-joined value would otherwise pass the suffix test whole and come
	// back as the host to dial, so this guards the match rather than tidying
	// input.
	if strings.ContainsAny(value, ",") {
		return "", fmt.Errorf("target host carries multiple values")
	}
	// This becomes a Host header, so CR and LF do not get to travel.
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("target host contains whitespace")
	}
	if _, _, err := net.SplitHostPort(value); err == nil {
		return "", fmt.Errorf("target host declares a port")
	}

	// Lowercased because matches and the concrete-origin comparison are both
	// byte comparisons against a policy the operator wrote in some other case.
	host := strings.ToLower(value)
	// An address cannot sit under a DNS family, so one here means the caller is
	// trying to leave the policy rather than move within it.
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("target host is an IP address")
	}
	return host, nil
}

// validatePort accepts an empty port, meaning the scheme's default. url.Parse
// already rejects a non-numeric port, but not 0 or an out-of-range number.
func validatePort(port string) error {
	if port == "" {
		return nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not in 1-65535", port)
	}
	return nil
}

// resolveTargetHost returns the host to retarget to, or "" to dial the origin
// as declared. Fail closed both ways: a family never falls back to a declared
// host, and a concrete origin never ignores a value that disagrees with it.
func (p parsedOrigin) resolveTargetHost(values []string) (string, error) {
	if len(values) > 1 {
		return "", fmt.Errorf("%w: duplicate target host", ErrDestinationRejected)
	}

	if len(values) == 0 {
		if p.isWildcard() {
			return "", fmt.Errorf("%w: wildcard origin requires a target host", ErrDestinationRejected)
		}
		return "", nil
	}

	host, err := parseTargetHost(values[0])
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrDestinationRejected, err)
	}

	if p.isWildcard() {
		if !p.wildcard.matches(host) {
			return "", fmt.Errorf("%w: target host is outside the origin policy", ErrDestinationRejected)
		}
		return host, nil
	}

	// A concrete origin routes on itself, but a value that names somewhere else
	// is a request to go there — refuse rather than silently ignore it.
	if host != strings.ToLower(p.url.Hostname()) {
		return "", fmt.Errorf("%w: target host disagrees with the origin", ErrDestinationRejected)
	}
	return "", nil
}
