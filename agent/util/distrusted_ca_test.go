package util

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func readTestCert(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../test/certs/" + name)
	require.NoError(t, err)
	return data
}

func TestFilterDistrustedCAs_RemovesDistrustedKeepsTheRest(t *testing.T) {
	trusted := readTestCert(t, "selfsigned-1.pem")
	bundle := append([]byte{}, readTestCert(t, "distrusted/emsign-ecc-root-ca-g3.pem")...)
	bundle = append(bundle, trusted...)
	// Matched by key, so a copy cross-signed by another issuer is removed too.
	bundle = append(bundle, readTestCert(t, "distrusted/em-dv-tls-ca-g3b-aaa-cross-sign.pem")...)

	filtered, removed := FilterDistrustedCAs(bundle)

	require.Len(t, removed, 2)
	require.Contains(t, removed[0], "emSign ECC Root CA - G3")
	require.Contains(t, removed[1], "EM DV TLS CA - G3B")

	require.Equal(t, string(trusted), string(filtered))
}

func TestFilterDistrustedCAs_UnchangedWhenNothingIsDistrusted(t *testing.T) {
	bundle := append(readTestCert(t, "selfsigned-1.pem"), readTestCert(t, "selfsigned-2.pem")...)

	filtered, removed := FilterDistrustedCAs(bundle)

	require.Empty(t, removed)
	require.Equal(t, bundle, filtered)
}

func TestFilterDistrustedCAs_KeepsBlocksItCannotParse(t *testing.T) {
	bundle := []byte("# corporate bundle\n-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")

	filtered, removed := FilterDistrustedCAs(bundle)

	require.Empty(t, removed)
	require.Equal(t, bundle, filtered)
}
