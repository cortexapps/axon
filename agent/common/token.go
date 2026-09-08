package common

import (
	"crypto/sha256"
	"fmt"
)

// TokenHash renders a broker token for a log line.
//
// The relay server and BROKER_SERVER log this same value as tokenHash for the
// same token, so it is what joins an agent log line to theirs. It also stays
// stable across restarts, which is what makes a token rotation visible.
func TokenHash(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", h[:])
}
