// Package natswire holds the NATS wire rules shared by the server and client.
package natswire

import (
	"strings"
	"unicode"
)

// ValidToken reports whether s is safe as a NATS subject token or stream,
// consumer, or agent name: 1 to 128 ASCII letters, digits, or hyphens.
func ValidToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// ValidID reports whether s is an acceptable task, run, attempt, or message
// identifier: 1 to 256 bytes, no surrounding space, no control characters,
// and no "nats/" prefix. That prefix is the channel address namespace applied
// by chat.Core.Address; wire IDs stay in the channel-local form, not the
// fully qualified address form. Core-mode task IDs become those local addresses.
func ValidID(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s &&
		strings.IndexFunc(s, unicode.IsControl) < 0 && !strings.HasPrefix(s, "nats/")
}
