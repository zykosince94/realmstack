package inventory

import "testing"

func TestSlot(t *testing.T) {
	if slot, ok := Slot(map[string]any{"slot": "weapon"}); !ok || slot != "weapon" {
		t.Fatalf("slot=%q ok=%v", slot, ok)
	}
	if _, ok := Slot(map[string]any{"healing": 50}); ok {
		t.Fatal("item without slot reported as equippable")
	}
	if _, ok := Slot(map[string]any{"slot": 5}); ok {
		t.Fatal("non-string slot reported as equippable")
	}
	if _, ok := Slot(nil); ok {
		t.Fatal("nil metadata reported as equippable")
	}
}
