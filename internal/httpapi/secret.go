package httpapi

import (
	"errors"
	"strings"
)

// minSecretLength is the shortest JWT_SECRET the API accepts in any environment.
const minSecretLength = 32

// placeholderMarkers are fragments the example files and Compose defaults use in their
// stand-in secrets ("replace-with-...", "local-only-change-me-..."). A secret that still
// contains one was copied from an example and never changed.
var placeholderMarkers = []string{
	"replace", "changeme", "change-me", "change_me", "change-this", "change_this",
	"placeholder", "example", "your-secret", "your_secret",
}

// ValidateJWTSecret rejects a signing secret that is too short, too repetitive, or (outside
// APP_ENV=local) still a placeholder from an example file.
//
// Why: the example files ship stand-in values longer than 32 characters, so a length check
// alone lets an operator who copies the file and forgets to edit it run production with a
// secret that is published in the repository, and anyone can then forge dashboard and
// player tokens. The local demo stacks use such values on purpose, so only non-local
// environments refuse them.
func ValidateJWTSecret(secret, appEnv string) error {
	if len(secret) < minSecretLength {
		return errors.New("JWT_SECRET must be set to at least 32 characters")
	}
	if distinctBytes(secret) < 8 {
		return errors.New("JWT_SECRET is too repetitive; generate a random value (for example: openssl rand -base64 48)")
	}
	if appEnv == "local" {
		return nil
	}
	lower := strings.ToLower(secret)
	for _, marker := range placeholderMarkers {
		if strings.Contains(lower, marker) {
			return errors.New("JWT_SECRET is still a placeholder from an example file; generate a random value (for example: openssl rand -base64 48)")
		}
	}
	return nil
}

func distinctBytes(s string) int {
	var seen [256]bool
	n := 0
	for i := 0; i < len(s); i++ {
		if !seen[s[i]] {
			seen[s[i]] = true
			n++
		}
	}
	return n
}
