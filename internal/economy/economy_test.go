package economy

import (
	"testing"
	"time"

	"realmstack-backend/internal/platform"
)

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 0, 0, 123456000, time.UTC)
	id := platform.RandomUUID()
	gotAt, gotID, ok := DecodeCursor(EncodeCursor(at, id))
	if !ok || !gotAt.Equal(at) || gotID != id {
		t.Fatalf("round trip: %v %v %v", gotAt, gotID, ok)
	}
	for _, bad := range []string{"", "!!", EncodeCursor(at, "not-a-uuid")} {
		if _, _, ok := DecodeCursor(bad); ok {
			t.Fatalf("accepted bad cursor %q", bad)
		}
	}
}
