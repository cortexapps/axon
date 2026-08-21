package redact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The inputs are shapes a Google token exchange actually returns. Each case
// asserts the secret is gone, not that the output takes a particular form.
func TestRedactRemovesCredentialMaterial(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{
			name:   "json access token",
			input:  `{"access_token": "ya29.a0ARrdaM-secret-value", "expires_in": 3599}`,
			secret: "ya29.a0ARrdaM-secret-value",
		},
		{
			name:   "json subject token echoed in an error",
			input:  `{"error":"invalid_request","subject_token":"eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLXZhbHVl"}`,
			secret: "eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLXZhbHVl",
		},
		{
			name:   "form encoded assertion",
			input:  `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJzYSJ9.c2lnbmF0dXJl`,
			secret: "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJzYSJ9.c2lnbmF0dXJl",
		},
		{
			name:   "private key from a credential configuration",
			input:  `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n"}`,
			secret: "BEGIN PRIVATE KEY",
		},
		{
			name:   "bare token with no field name",
			input:  `refresh failed for ya29.c.Kp8Bxxxxxxxxxxxx after 3 attempts`,
			secret: "ya29.c.Kp8Bxxxxxxxxxxxx",
		},
		{
			name:   "bearer token in an accept file header",
			input:  `{"headers":{"authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJhIjoiYiJ9.c2ln"}}`,
			secret: "eyJhbGciOiJIUzI1NiJ9.eyJhIjoiYiJ9.c2ln",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Redact(tc.input)
			require.NotContains(t, out, tc.secret)
			require.Contains(t, out, redacted)
		})
	}
}

// Redaction that also eats the surrounding structure makes a log line useless,
// which is the usual reason redaction gets removed later.
func TestRedactKeepsSurroundingContext(t *testing.T) {
	out := Redact(`{"error":"invalid_grant","error_description":"bad audience","access_token":"ya29.secret"}`)
	require.Contains(t, out, "invalid_grant")
	require.Contains(t, out, "bad audience")
	require.NotContains(t, out, "ya29.secret")
}

func TestRedactLeavesNonCredentialTextAlone(t *testing.T) {
	input := `GET https://storage.googleapis.com/b/my-bucket returned 403: caller lacks storage.buckets.get`
	require.Equal(t, input, Redact(input))
}
