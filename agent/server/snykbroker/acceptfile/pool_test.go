package acceptfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPool_RoundRobin(t *testing.T) {
	t.Setenv("TEST_API_POOL", "https://api1.example.com,https://api2.example.com,https://api3.example.com")

	pm := NewPoolManager()

	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		results[i] = pm.ResolvePoolVars("${TEST_API}")
	}

	assert.Equal(t, "https://api1.example.com", results[0])
	assert.Equal(t, "https://api2.example.com", results[1])
	assert.Equal(t, "https://api3.example.com", results[2])
	assert.Equal(t, "https://api1.example.com", results[3])
	assert.Equal(t, "https://api2.example.com", results[4])
	assert.Equal(t, "https://api3.example.com", results[5])
}

func TestPool_FallbackToEnvVar(t *testing.T) {
	t.Setenv("SINGLE_API", "https://api.example.com")

	pm := NewPoolManager()
	result := pm.ResolvePoolVars("${SINGLE_API}")
	assert.Equal(t, "https://api.example.com", result)
}

func TestPool_NoMatch(t *testing.T) {
	pm := NewPoolManager()
	result := pm.ResolvePoolVars("https://static.example.com")
	assert.Equal(t, "https://static.example.com", result)
}
