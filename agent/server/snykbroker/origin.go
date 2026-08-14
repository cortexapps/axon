package snykbroker

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// HeaderTargetHost is routing input, never authorization: the deployed origin
// stays the policy, and this value is only ever checked against it.
const HeaderTargetHost = "x-cortex-target-host"

const ErrClassDestinationRejected = "AXON_DESTINATION_REJECTED"

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
		return asURL, nil, nil
	}

	// Certificate verification is the destination control, so a family cannot
	// be served over plaintext.
	if asURL.Scheme != "https" {
		return nil, nil, fmt.Errorf("wildcard origin %q must use https", origin)
	}
	if asURL.Port() != "" {
		return nil, nil, fmt.Errorf("wildcard origin %q cannot declare a port", origin)
	}

	host := asURL.Hostname()
	suffix, found := strings.CutPrefix(host, "*.")
	if !found || strings.Contains(suffix, "*") {
		return nil, nil, fmt.Errorf("wildcard origin %q must have the form https://*.example.com", origin)
	}
	if err := validateDNSName(suffix); err != nil {
		return nil, nil, fmt.Errorf("wildcard origin %q: %w", origin, err)
	}
	// ICANN division only. The private section holds ordinary registrable
	// domains added for cookie scoping, and rejecting those would rule out
	// legitimate families.
	if publicSuffix, icann := publicsuffix.PublicSuffix(suffix); icann && publicSuffix == suffix {
		return nil, nil, fmt.Errorf("wildcard origin %q must contain a registrable domain", origin)
	}

	return asURL, &wildcardOrigin{suffix: "." + suffix}, nil
}

// parseTargetHost normalizes a header value into a bare ASCII DNS hostname, or
// reports why it is unusable. Malformed values are rejected, never repaired.
func parseTargetHost(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("empty target host")
	}
	if strings.ContainsAny(value, ",") {
		return "", fmt.Errorf("target host carries multiple values")
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("target host contains whitespace")
	}

	// 443 is the only port an origin implies, so it normalizes away; any other
	// would dial somewhere the policy never authorized.
	if hostOnly, port, err := net.SplitHostPort(value); err == nil {
		if port != "443" {
			return "", fmt.Errorf("target host declares a non-default port")
		}
		value = hostOnly
	}

	host := strings.ToLower(value)
	if err := validateDNSName(host); err != nil {
		return "", err
	}
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("target host is an IP address")
	}
	return host, nil
}

// validateDNSName is deliberately stricter than DNS itself: the shapes it
// rejects are the ones that could confuse suffix matching or certificate
// verification.
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
