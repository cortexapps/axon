package broker

import "strings"

// LooksHashed reports whether s has the shape of a Token hash: 64 hex
// characters, the SHA-256 that Hashed returns. Raw broker tokens are UUIDs, so
// this is how a value from a URL is told apart from a hash without a registry
// hit to go on.
func LooksHashed(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// MaskToken returns a raw broker token for logging with its middle
// dash-separated sections replaced by "****", keeping the first and last
// sections so it can still be searched for:
//
//	66fa3dbe-46fa-4949-9713-89a2ead1e7c2 -> 66fa3dbe-****-****-****-89a2ead1e7c2
//
// A token with fewer than three sections keeps its first and last four
// characters, or is starred out entirely when shorter than 12.
func MaskToken(raw string) string {
	if raw == "" {
		return ""
	}
	if parts := strings.Split(raw, "-"); len(parts) >= 3 {
		for i := 1; i < len(parts)-1; i++ {
			parts[i] = "****"
		}
		return strings.Join(parts, "-")
	}
	if len(raw) < 12 {
		return strings.Repeat("*", len(raw))
	}
	return raw[:4] + "****" + raw[len(raw)-4:]
}

// Masked returns the raw token masked for logging, or "" when only the hash
// is known.
func (t Token) Masked() string { return MaskToken(t.raw) }
