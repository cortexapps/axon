// Package redact strips credential material out of text before it is logged or
// returned.
//
// It is a copy of the agent's util.Redact rather than a shared import. This
// module deliberately depends on nothing in the agent - the two communicate over
// a process contract, not an API - and a third module existing only to hold one
// pure function would cost more than the duplication. A fix to the patterns here
// belongs in agent/util/redact.go too, and vice versa.
//
// util.Fingerprint is not copied: nothing in this binary has a long-lived secret
// to tell apart across log lines.
package redact

import (
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

// Names whose value is a credential wherever it appears. Token exchanges report
// failure by echoing part of the request, so an error string from an identity
// provider can carry the very material the caller sent it.
var sensitiveFieldNames = []string{
	"access_token",
	"assertion",
	"authorization",
	"client_secret",
	"id_token",
	"private_key",
	"refresh_token",
	"subject_token",
	"token",
}

// Two shapes for a named field: form encoded first, then quoted JSON. The order
// fixes the capture-group numbering the replacement template below relies on.
var reSensitiveField = regexp.MustCompile(
	`(?i)\b(` + fieldNameAlternation() + `=)[^&\s"]+` +
		`|("(?:` + fieldNameAlternation() + `)"\s*:\s*")[^"]*(")`)

// Only the form branch or only the JSON branch matches, and an unmatched group
// expands to nothing, so one template serves both.
const sensitiveFieldReplacement = "${1}${2}" + redacted + "${3}"

// Google access tokens and anything JWT-shaped, for values that arrive without
// their field name - a bare token in a message, or one embedded in a URL.
//
// The `eyJ` prefix is base64url for `{"`, so the first segment carries almost
// all of the specificity; the later segments are only loosely bounded because a
// short signature is still a signature.
var reBareCredential = regexp.MustCompile(
	`ya29\.[A-Za-z0-9._\-]+` +
		`|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}`)

func fieldNameAlternation() string {
	quoted := make([]string, 0, len(sensitiveFieldNames))
	for _, name := range sensitiveFieldNames {
		quoted = append(quoted, regexp.QuoteMeta(name))
	}
	return strings.Join(quoted, "|")
}

// Redact replaces credential material in s with a fixed marker. Use it on
// anything derived from a credential exchange before it is logged or returned,
// including error strings.
//
// It is a net, not a guarantee: it recognizes the shapes identity providers are
// known to emit. Code holding a credential directly should decline to log it
// rather than rely on this.
func Redact(s string) string {
	s = reSensitiveField.ReplaceAllString(s, sensitiveFieldReplacement)
	return reBareCredential.ReplaceAllString(s, redacted)
}
