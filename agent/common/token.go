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
//
// The algorithm and encoding are therefore a contract with those two, not a
// local choice. A change here breaks the join silently: each line still carries
// a plausible tokenHash, and none of them match. A pinned test guards it.
func TokenHash(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", h[:])
}
