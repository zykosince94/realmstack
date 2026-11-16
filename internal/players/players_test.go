package players

import (
	"strings"
	"testing"
)

func TestDefaultDisplayNameIsNotDerivedFromEmail(t *testing.T) {
	name := defaultDisplayName()
	if !strings.HasPrefix(name, "Player-") || len(name) != len("Player-")+6 {
		t.Fatalf("unexpected display name %q", name)
	}
	if name == defaultDisplayName() {
		t.Fatal("display names should be random")
	}
}
