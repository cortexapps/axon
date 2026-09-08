package common

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// The relay server and BROKER_SERVER log the same hash for a token, and joining
// an agent log line to theirs is the only reason the hash is logged at all. So
// the exact output for a fixed input is a compatibility contract across three
// components, not an implementation detail. A different algorithm or encoding
// breaks the join silently: every line still carries a plausible tokenHash, and
// none of them match.
//
// This value is SHA-256, hex, lower case, of the token below. It is the same
// value broker.Token.Hashed() produces in the server module. Do not update it
// to match a changed implementation.
const goldenTokenHash = "8b47065f7f6f41a26ac87f4425984c5134275d7b4a98675ac94cecf9f94209c3"

const goldenToken = "4f49654b-1111-2222-3333-9deef1d9f2f6"

func TestTokenHash_PinnedGoldenValue(t *testing.T) {
	require.Equal(t, goldenTokenHash, TokenHash(goldenToken))
}

func TestTokenHash_IsLowercaseHexOfFixedWidth(t *testing.T) {
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), TokenHash(goldenToken))
}

func TestTokenHash_EmptyTokenHashesToEmpty(t *testing.T) {
	require.Empty(t, TokenHash(""), "an absent token must not look like a real one")
}
