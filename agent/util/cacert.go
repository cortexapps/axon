package util

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReadCACertPEM reads the configured CA certificate material and returns it
// as concatenated PEM.
//
// The path may name a single file or a directory of *.pem files, because
// CA_CERT_PATH has always accepted both and customers mount it either way.
// It lives here so every transport reads it the same way: the HTTP client
// handled the directory form while the gRPC tunnel called os.ReadFile
// directly, so pointing CA_CERT_PATH at a directory left upstream requests
// working and the tunnel refusing to connect with "is a directory".
//
// An empty path returns no bytes and no error — no custom CA is configured.
func ReadCACertPEM(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}

	stat, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", path, err)
	}

	if !stat.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %q: %w", path, err)
		}
		return data, nil
	}

	files, err := filepath.Glob(filepath.Join(path, "*.pem"))
	if err != nil {
		return nil, fmt.Errorf("read CA cert directory %q: %w", path, err)
	}
	var pem []byte
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %q: %w", file, err)
		}
		pem = append(pem, data...)
	}
	return pem, nil
}
