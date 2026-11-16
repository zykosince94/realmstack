package achievements

import (
	"testing"
	"time"

	"realmstack-backend/internal/goals"
)

func TestViewStatusAndHiddenMasking(t *testing.T) {
	d := Definition{Key: "secret", Name: "Secret", Description: "Find it", Hidden: true,
		Conditions: []goals.Condition{{Type: goals.Flag, FlagKey: "found"}}, Rewards: goals.Rewards{XP: 10}}
	notMet := []goals.Progress{{Type: goals.Flag, Target: 1}}
	met := []goals.Progress{{Type: goals.Flag, Current: 1, Target: 1, Met: true}}

	masked := view(d, notMet, nil, true)
	if masked["status"] != StatusInProgress || masked["name"] != nil || masked["conditions"] != nil || masked["rewards"] != nil || masked["progress"] != nil {
		t.Fatalf("hidden incomplete achievement not masked: %v", masked)
	}
	if masked["key"] != "secret" {
		t.Fatal("the key stays visible so the client can reference the achievement")
	}
	if unmasked := view(d, notMet, nil, false); unmasked["name"] != "Secret" {
		t.Fatalf("server view must not mask: %v", unmasked)
	}
	if completed := view(d, met, nil, true); completed["status"] != StatusCompleted || completed["name"] != "Secret" {
		t.Fatalf("completed hidden achievement should be revealed: %v", completed)
	}
	claimed := view(d, notMet, &claim{At: time.Now(), By: "player"}, true)
	if claimed["status"] != StatusClaimed || claimed["name"] != "Secret" {
		t.Fatalf("claimed status takes precedence and reveals: %v", claimed)
	}
}
