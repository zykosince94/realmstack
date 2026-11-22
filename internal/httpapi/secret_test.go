package httpapi

import "testing"

func TestValidateJWTSecret(t *testing.T) {
	random := "q8Zr2vLxN4tY7pKd9WsB3cHf6JmA1eUg5oTi0bXa"
	cases := []struct {
		name, secret, env string
		wantErr           bool
	}{
		{"random secret in production", random, "production", false},
		{"random secret locally", random, "local", false},
		{"too short", "short-secret", "production", true},
		{"too short locally", "short-secret", "local", true},
		{"repetitive", "abababababababababababababababababab", "production", true},
		{"selfhost example placeholder", "replace-with-at-least-32-random-characters", "production", true},
		{"backend example placeholder", "replace-this-with-a-long-random-secret", "production", true},
		{"frontend demo default", "local-only-change-this-secret-before-deploying", "production", true},
		{"backend compose default", "local-only-change-me-before-deploy-please", "production", true},
		{"upper-case placeholder", "REPLACE-WITH-AT-LEAST-32-RANDOM-CHARACTERS", "production", true},
		{"placeholder allowed in the local demo", "local-only-change-this-secret-before-deploying", "local", false},
		{"unset environment is not local", "replace-this-with-a-long-random-secret", "", true},
	}
	for _, c := range cases {
		err := ValidateJWTSecret(c.secret, c.env)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}
