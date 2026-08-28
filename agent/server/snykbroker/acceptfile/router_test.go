package acceptfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTargetURL(t *testing.T) {
	tests := []struct {
		origin string
		path   string
		query  string
		want   string
	}{
		{"https://api.github.com", "/repos/foo", "", "https://api.github.com/repos/foo"},
		{"https://api.github.com/v3", "/repos/foo", "", "https://api.github.com/v3/repos/foo"},
		{"https://api.github.com/", "/repos/foo", "", "https://api.github.com/repos/foo"},
		{"https://api.github.com", "/search", "q=x%2Fy", "https://api.github.com/search?q=x%2Fy"},
		{"https://gitlab.com", "/api/v4/projects/a%2Fb", "", "https://gitlab.com/api/v4/projects/a%2Fb"},
	}

	for _, tt := range tests {
		t.Run(tt.origin+tt.path, func(t *testing.T) {
			decoded, err := urlPathUnescape(tt.path)
			require.NoError(t, err)
			parsed, err := parseOrigin(tt.origin)
			require.NoError(t, err)
			got, err := buildTargetURL(parsed.url, "", tt.path, decoded, tt.query)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestRouter_NoRouteAndInvalid(t *testing.T) {
	router, err := NewRouter(nil, nil)
	require.NoError(t, err)

	_, err = router.Route("GET", "/nope", nil)
	assert.ErrorIs(t, err, ErrNoRoute)

	_, err = router.Route("", "/nope", nil)
	var invalid *InvalidRequestError
	assert.ErrorAs(t, err, &invalid)

	_, err = router.Route("GET", "/bad%zz", nil)
	assert.ErrorAs(t, err, &invalid)
}
