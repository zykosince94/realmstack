package catalog

import (
	"encoding/json"
	"testing"
)

func TestMergeProposal(t *testing.T) {
	draft := []byte(`{"items":[{"key":"sword","name":"Sword","category":"weapon","stackable":false,"max_stack":1}],"progression":[100],"editor_layout":{"quests":{"q":{"x":1,"y":2}}}}`)
	p := Proposal{
		Items:       []ItemConfig{{Key: "sword", Name: "Better Sword", MaxStack: 1}, {Key: "herb", Name: "Herb", Stackable: true, MaxStack: 20}},
		Progression: []int64{100, 250},
	}
	// Add mode: the existing key is a conflict, reported against the proposal.
	merged, conflicts, err := MergeProposal(draft, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Field != "proposal.items[0].key" {
		t.Fatalf("conflicts: %v", conflicts)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(merged, &got)
	var items []ItemConfig
	_ = json.Unmarshal(got["items"], &items)
	if len(items) != 2 || items[0].Name != "Sword" || items[1].Key != "herb" {
		t.Fatalf("add mode items: %+v", items)
	}
	if string(got["progression"]) != "[100,250]" {
		t.Fatalf("progression: %s", got["progression"])
	}
	if _, ok := got["editor_layout"]; !ok {
		t.Fatal("editor_layout was dropped")
	}
	if string(got["quests"]) != "[]" {
		t.Fatalf("collections should be normalized: %s", got["quests"])
	}
	// Replace mode: overwritten in place, no conflict.
	merged, conflicts, _ = MergeProposal(draft, p, true)
	_ = json.Unmarshal(merged, &got)
	_ = json.Unmarshal(got["items"], &items)
	if len(conflicts) != 0 || len(items) != 2 || items[0].Name != "Better Sword" {
		t.Fatalf("replace mode: %v %+v", conflicts, items)
	}
	if !(Proposal{}).Empty() || p.Empty() {
		t.Fatal("Empty")
	}
}
