package completeness

import (
	"aninode/internal/inventory"
	"testing"
)

func snapshot(episodes ...int) inventory.Snapshot {
	set := map[int]struct{}{}
	for _, ep := range episodes {
		set[ep] = struct{}{}
	}
	return inventory.Snapshot{Entries: map[string]inventory.EntryInventory{"series/Test": {Known: true, Seasons: map[int]inventory.SeasonInventory{1: {Known: true, Episodes: set}}}}}
}
func TestInternalGapsAreFilesystemProvable(t *testing.T) {
	m, s := InternalGaps(snapshot(1, 2, 3, 5), "series/Test", 1)
	if s != "incomplete" || len(m) != 1 || m[0] != 4 {
		t.Fatalf("missing=%v status=%s", m, s)
	}
}
func TestInternalGapsDoNotInventHeadOrTail(t *testing.T) {
	m, s := InternalGaps(snapshot(7, 8, 10), "series/Test", 1)
	if s != "incomplete" || len(m) != 1 || m[0] != 9 {
		t.Fatalf("missing=%v status=%s", m, s)
	}
	m, s = InternalGaps(snapshot(10), "series/Test", 1)
	if s != "continuous" || len(m) != 0 {
		t.Fatalf("single episode missing=%v status=%s", m, s)
	}
}

func TestInternalGapsIgnoreMissingHeadAndRepairOnlyMiddleHole(t *testing.T) {
	m, s := InternalGaps(snapshot(3, 4, 5, 7), "series/Test", 1)
	if s != "incomplete" || len(m) != 1 || m[0] != 6 {
		t.Fatalf("missing=%v status=%s", m, s)
	}
}
func TestMissingInExplicitRangeCanFillHeadAndTail(t *testing.T) {
	m, s := MissingInRange(snapshot(3, 5), "series/Test", 1, EpisodeRange{From: 1, Through: 6})
	want := []int{1, 2, 4, 6}
	if s != "incomplete" || len(m) != len(want) || m[0] != 1 || m[1] != 2 || m[2] != 4 || m[3] != 6 {
		t.Fatalf("missing=%v status=%s", m, s)
	}
}
func TestUnknownInventoryRemainsUnknown(t *testing.T) {
	inv := inventory.Snapshot{Entries: map[string]inventory.EntryInventory{"series/Test": {Known: false}}}
	m, s := InternalGaps(inv, "series/Test", 1)
	if m != nil || s != "unknown" {
		t.Fatalf("missing=%v status=%s", m, s)
	}
}
