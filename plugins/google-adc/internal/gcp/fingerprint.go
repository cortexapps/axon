package gcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

const fingerprintLength = 16

// IdentityFingerprint summarises which Google identity Application Default
// Credentials would resolve to, without resolving it. A persisted token is keyed
// on this, so rotating the configuration stops serving the previous identity.
//
// It hashes the inputs that decide detection rather than its outcome, because it
// runs on the cache-hit path where the point is to avoid the metadata server. It
// is deliberately over-sensitive: an input that changes without changing the
// identity costs one extra mint, the other way round serves the wrong credential.
func IdentityFingerprint(scope string) string {
	hash := sha256.New()
	// Length-prefixed, so no combination of values can imitate a different set of
	// fields.
	add := func(key, value string) {
		fmt.Fprintf(hash, "%s=%d:%s\n", key, len(value), value)
	}

	add("scope", scope)

	// The environment the library consults to decide what to use. HOME appears
	// because the gcloud well-known file is looked up beneath it.
	for _, name := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GCE_METADATA_HOST",
		"GOOGLE_API_USE_CLIENT_CERTIFICATE",
		"HOME",
	} {
		add("env:"+name, os.Getenv(name))
	}

	// Contents, not paths. A credential file rewritten in place keeps its path, and
	// that is exactly the rotation that must invalidate the cache.
	for _, path := range []string{
		os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		wellKnownCredentialsPath(),
	} {
		if path == "" {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			// Absent is itself part of the state: a file that appears later must not
			// read a cache minted before it existed.
			add("file:"+path, "absent")
			continue
		}
		sum := sha256.Sum256(content)
		add("file:"+path, hex.EncodeToString(sum[:]))
	}

	return hex.EncodeToString(hash.Sum(nil))[:fingerprintLength]
}

// wellKnownCredentialsPath mirrors the library's own well-known path, which it
// computes internally and does not export. Drifting from it costs an extra mint,
// not a wrong identity, because the content is only ever an input to the hash.
func wellKnownCredentialsPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}
