package util

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadCACertPEM_EmptyPathIsNotAnError(t *testing.T) {
	pem, err := ReadCACertPEM("")
	require.NoError(t, err)
	require.Empty(t, pem)
}

func TestReadCACertPEM_SingleFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(f, []byte("ONE"), 0o600))

	pem, err := ReadCACertPEM(f)
	require.NoError(t, err)
	require.Equal(t, "ONE", string(pem))
}

// A directory is the form the HTTP client accepted and the tunnel did not.
func TestReadCACertPEM_DirectoryConcatenatesPEMFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.pem"), []byte("A"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.pem"), []byte("B"), 0o600))
	// Not a .pem, so not picked up.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("X"), 0o600))

	pem, err := ReadCACertPEM(dir)
	require.NoError(t, err)
	require.Equal(t, "AB", string(pem))
}

func TestReadCACertPEM_MissingPathIsAnError(t *testing.T) {
	_, err := ReadCACertPEM(filepath.Join(t.TempDir(), "nope.pem"))
	require.Error(t, err)
}
