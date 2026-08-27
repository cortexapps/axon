package snykbroker

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// HeaderTargetHost is routing input, never authorization: the deployed origin
// stays the policy, and this value is only ever checked against it.
const HeaderTargetHost = "x-cortex-target-host"

// HeaderFailureClass names the component that failed, so a caller can branch on
// it without reading a body an upstream could also have written. Only components
// on this side of the boundary set it; an upstream copy is stripped.
const HeaderFailureClass = "x-cortex-failure-class"

const ErrClassDestinationRejected = "AXON_DESTINATION_REJECTED"

// The rule's credential provider produced no value. Distinct from an upstream
// 401 or 403, which says the credential was produced and then refused.
const ErrClassCredentialFailure = "AXON_CREDENTIAL_FAILURE"

// This agent's own DNS, connection, or TLS failed, so the upstream never
// answered and its status must not be invented on its behalf.
const ErrClassNetworkFailure = "AXON_NETWORK_FAILURE"

// With verification off, anything answering the connection can claim the
// authorized name, so a host family authorizes nothing. A concrete origin at
// least names one host the operator chose.
var ErrWildcardOriginRequiresTLSVerification = errors.New(
	"a wildcard origin requires TLS verification, but it is disabled")

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

// parseOrigin returns the URL to dial and, for a wildcard origin, the family it
// authorizes. Callers run it at startup so a malformed policy fails there
// rather than per request.
func parseOrigin(origin string) (*url.URL, *wildcardOrigin, error) {
	asURL, err := url.Parse(origin)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid target URI: %w", err)
	}

	if !strings.Contains(asURL.Host, "*") {
		// Deliberately not port-checked. A concrete origin names one host the
		// operator picked, and the agent's own origin carries port 0 until its
		// listener binds.
		return asURL, nil, nil
	}

	// Certificate verification is the destination control, so a family cannot
	// be served over plaintext.
	if asURL.Scheme != "https" {
		return nil, nil, fmt.Errorf("wildcard origin %q must use https", origin)
	}
	// A wildcard origin's port is dialed against a host the operator never
	// wrote down, so an unusable one has to stop the agent rather than surface
	// as a per-request failure.
	if err := validatePort(asURL.Port()); err != nil {
		return nil, nil, fmt.Errorf("wildcard origin %q: %w", origin, err)
	}

	host := asURL.Hostname()
	suffix, found := strings.CutPrefix(host, "*.")
	if !found || strings.Contains(suffix, "*") {
		return nil, nil, fmt.Errorf("wildcard origin %q must have the form https://*.example.com", origin)
	}
	// "https://*." alone would authorize every host, and an empty label would
	// misalign the match against the dot this stores.
	if suffix == "" || strings.HasPrefix(suffix, ".") ||
		strings.HasSuffix(suffix, ".") || strings.Contains(suffix, "..") {
		return nil, nil, fmt.Errorf("wildcard origin %q has an empty label", origin)
	}
	// ICANN division only. The private section holds ordinary registrable
	// domains added for cookie scoping, and rejecting those would rule out
	// legitimate families.
	if publicSuffix, icann := publicsuffix.PublicSuffix(suffix); icann && publicSuffix == suffix {
		return nil, nil, fmt.Errorf("wildcard origin %q must contain a registrable domain", origin)
	}

	return asURL, &wildcardOrigin{suffix: "." + suffix}, nil
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
	// A comma-joined value used to pass the suffix test whole and come back as
	// the host to dial, so this one guards the match rather than tidying input.
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
