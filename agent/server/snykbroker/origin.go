package snykbroker

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// HeaderTargetHost carries the concrete destination authority for a rule whose
// origin is a wildcard. It is routing input, never authorization: the deployed
// origin remains the policy, and this value is only ever checked against it.
//
// Axon validates it once, moves the result into private request context, and
// removes every copy from the request on all paths - concrete-origin rules and
// WebSocket upgrades included - so it never reaches an upstream, a response, a
// log, or a trace attribute.
const HeaderTargetHost = "x-cortex-target-host"

// ErrClassDestinationRejected is the stable machine-readable class returned
// when the target is missing, malformed, duplicated, or outside the origin
// policy. The class names the component that failed, never the provider
// behind it.
const ErrClassDestinationRejected = "AXON_DESTINATION_REJECTED"

// wildcardOrigin is an origin whose leftmost DNS label is a wildcard, such as
// "https://*.googleapis.com". It authorizes a hostname family and a scheme;
// the concrete authority arrives per request.
type wildcardOrigin struct {
	// suffix is the part after the "*", with the dot kept so matching is
	// label-aligned: ".googleapis.com".
	suffix string
}

// matches reports whether host sits exactly one label under the wildcard.
//
// One label, not one-or-more: every authority Cortex dials is a single label
// under googleapis.com, and multi-label matching would silently admit the
// "<service>.mtls.googleapis.com" family, which is a deliberate non-target.
func (w wildcardOrigin) matches(host string) bool {
	if !strings.HasSuffix(host, w.suffix) {
		return false
	}
	label := host[:len(host)-len(w.suffix)]
	return label != "" && !strings.Contains(label, ".")
}

// parseOrigin splits a rule origin into the URL to dial and, when the origin
// is a wildcard, the family it authorizes. A concrete origin returns a nil
// wildcard and keeps its existing behavior.
//
// Wildcard origins are validated here, at startup, so a malformed policy fails
// the agent rather than surfacing as a per-request rejection later.
func parseOrigin(origin string) (*url.URL, *wildcardOrigin, error) {
	asURL, err := url.Parse(origin)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid target URI: %w", err)
	}

	if !strings.Contains(asURL.Host, "*") {
		return asURL, nil, nil
	}

	// Certificate verification is the destination control, so a wildcard
	// family cannot be served over plaintext.
	if asURL.Scheme != "https" {
		return nil, nil, fmt.Errorf("wildcard origin %q must use https", origin)
	}
	if asURL.Port() != "" {
		return nil, nil, fmt.Errorf("wildcard origin %q cannot declare a port", origin)
	}

	host := asURL.Hostname()
	// Exactly one leading "*." and no other wildcard anywhere: this rejects
	// "*", "a*.com", and "foo.*.com".
	suffix, found := strings.CutPrefix(host, "*.")
	if !found || strings.Contains(suffix, "*") {
		return nil, nil, fmt.Errorf("wildcard origin %q must have the form https://*.example.com", origin)
	}
	if err := validateDNSName(suffix); err != nil {
		return nil, nil, fmt.Errorf("wildcard origin %q: %w", origin, err)
	}
	// A public suffix authorizes an entire registry, so require at least one
	// registrable label beneath it.
	//
	// Only the ICANN division counts. The list's private section contains
	// registrable domains that operators added for cookie scoping -
	// googleapis.com among them - and treating those as public would reject
	// the very families this exists to express.
	if publicSuffix, icann := publicsuffix.PublicSuffix(suffix); icann && publicSuffix == suffix {
		return nil, nil, fmt.Errorf("wildcard origin %q must contain a registrable domain", origin)
	}

	return asURL, &wildcardOrigin{suffix: "." + suffix}, nil
}

// parseTargetHost normalizes a header value into a bare ASCII DNS hostname, or
// reports why it is unusable. URLs, IP addresses, ports, Unicode names,
// wildcards, user information, whitespace, and comma-joined values are all
// rejected rather than repaired.
func parseTargetHost(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("empty target host")
	}
	// A comma-joined value is two claims in one header, so there is no single
	// authority to authorize.
	if strings.ContainsAny(value, ",") {
		return "", fmt.Errorf("target host carries multiple values")
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("target host contains whitespace")
	}

	host := strings.ToLower(value)
	if err := validateDNSName(host); err != nil {
		return "", err
	}
	// A literal address bypasses the hostname policy and the certificate name
	// check that policy depends on.
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("target host is an IP address")
	}
	return host, nil
}

// validateDNSName accepts only a bare, ASCII, dot-separated hostname. It is
// deliberately stricter than the DNS specification: everything Axon dials is a
// conventional public hostname, and the rejected shapes are exactly the ones
// that could confuse suffix matching or certificate verification.
func validateDNSName(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("target host has an invalid length")
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return fmt.Errorf("target host has an empty label")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("target host has an invalid label")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("target host has a label bounded by a hyphen")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			isASCIIAlnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !isASCIIAlnum && c != '-' {
				return fmt.Errorf("target host has an invalid character")
			}
		}
	}
	return nil
}
