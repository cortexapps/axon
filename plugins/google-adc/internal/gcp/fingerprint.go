package gcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// fingerprintLength is enough to make a collision between two identities on one
// host not worth reasoning about, while keeping the cache filename readable.
const fingerprintLength = 16

// IdentityFingerprint summarises which Google identity Application Default
// Credentials would resolve to, without resolving it.
//
// A persisted token has to be keyed on this. The alternative - one cache file per
// source name - serves the previous identity until its token expires whenever the
// configuration changes, and presents as an IAM error on a service account the
// operator has already stopped using.
//
// It must stay cheap, because it runs on the cache-hit path where the point is to
// avoid touching the metadata server at all. So it hashes the inputs that decide
// detection rather than the outcome of detection.
//
// It is deliberately over-sensitive: an input that changes without changing the
// identity costs one extra mint, while an identity that changes without changing
// the fingerprint serves the wrong credential.
func IdentityFingerprint(scope string) string {
	hash := sha256.New()
	add := func(key, value string) {
		// Length-prefixed, so no combination of values can imitate a different
		// set of fields.
		fmt.Fprintf(hash, "%s=%d:%s\n", key, len(value), value)
	}

	add("scope", scope)

	// The environment variables the library consults to decide what to use, in
	// the order it consults them. HOME appears because the gcloud well-known file
	// is looked up beneath it.
	for _, name := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GCE_METADATA_HOST",
		"GOOGLE_API_USE_CLIENT_CERTIFICATE",
		"HOME",
	} {
		add("env:"+name, os.Getenv(name))
	}

	// Contents, not paths. A credential file rewritten in place keeps its path,
	// and that is exactly the rotation that must invalidate the cache.
	for _, path := range []string{
		os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		wellKnownCredentialsPath(),
	} {
		if path == "" {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			// Absent or unreadable is itself part of the state: a file that
			// appears later must not read a cache minted before it existed.
			add("file:"+path, "absent")
			continue
		}
		sum := sha256.Sum256(content)
		add("file:"+path, hex.EncodeToString(sum[:]))
	}

	return hex.EncodeToString(hash.Sum(nil))[:fingerprintLength]
}

// wellKnownCredentialsPath mirrors the library's own well-known path. The
// library computes it internally and does not export it, so a change there is a
// change here: the consequence of drifting is an extra mint, not a wrong
// identity, because the file's content is only ever an input to the hash.
func wellKnownCredentialsPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}
