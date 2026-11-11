package broker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMaskToken(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"66fa3dbe-46fa-4949-9713-89a2ead1e7c2", "66fa3dbe-****-****-****-89a2ead1e7c2"},
		{"abcdefghijklmnopqrst", "abcd****qrst"},
		{"abcdefgh-ijklmnop", "abcd****mnop"},
		{"shorttoken", "**********"},
		{"", ""},
	} {
		assert.Equal(t, tc.want, MaskToken(tc.raw), tc.raw)
	}
}

func TestToken_Masked(t *testing.T) {
	assert.Equal(t, "66fa3dbe-****-****-****-89a2ead1e7c2", NewToken("66fa3dbe-46fa-4949-9713-89a2ead1e7c2").Masked())
	assert.Equal(t, "", TokenFromHash(NewToken("x").Hashed()).Masked(), "no raw token, nothing to mask")
}

func TestLooksHashed(t *testing.T) {
	assert.True(t, LooksHashed(NewToken("66fa3dbe-46fa-4949-9713-89a2ead1e7c2").Hashed()))
	assert.True(t, LooksHashed(strings.ToUpper(NewToken("x").Hashed())))
	assert.False(t, LooksHashed("66fa3dbe-46fa-4949-9713-89a2ead1e7c2"))
	assert.False(t, LooksHashed(strings.Repeat("g", 64)))
	assert.False(t, LooksHashed(""))
}
