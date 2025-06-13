package snykbroker

import (
	"os"
	"testing"
)

func TestSubstituteEnvVars(t *testing.T) {
	// Set up test environment variables
	os.Setenv("TEST_VAR", "test_value")
	os.Setenv("PRIVATE_KEY", "secret123")
	defer os.Unsetenv("TEST_VAR")
	defer os.Unsetenv("PRIVATE_KEY")

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "substitute single variable",
			input:    "${TEST_VAR}",
			expected: "test_value",
		},
		{
			name:     "substitute multiple variables",
			input:    "Bearer ${PRIVATE_KEY}",
			expected: "Bearer secret123",
		},
		{
			name:     "substitute in middle of string",
			input:    "prefix-${TEST_VAR}-suffix",
			expected: "prefix-test_value-suffix",
		},
		{
			name:     "no substitution needed",
			input:    "no variables here",
			expected: "no variables here",
		},
		{
			name:     "undefined variable",
			input:    "${UNDEFINED_VAR}",
			expected: "${UNDEFINED_VAR}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := substituteEnvVars(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestProxyEntryWithHeaders(t *testing.T) {
	headers := map[string]string{
		"x-private-key": "secret123",
		"x-custom":      "value",
	}

	entry := newProxyEntryWithHeaders("https://api.example.com", false, nil, 8080, headers)

	if entry.targetURI != "https://api.example.com" {
		t.Errorf("Expected targetURI https://api.example.com, got %s", entry.targetURI)
	}
	if entry.isDefault {
		t.Error("Expected isDefault to be false")
	}
	if len(entry.headers) != 2 {
		t.Errorf("Expected 2 headers, got %d", len(entry.headers))
	}
	if entry.headers["x-private-key"] != "secret123" {
		t.Errorf("Expected x-private-key header to be secret123, got %s", entry.headers["x-private-key"])
	}
}