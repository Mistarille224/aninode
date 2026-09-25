package application

import (
	"testing"

	"aninode/internal/candidate"
	"aninode/internal/episode"
	"aninode/internal/inventory"
)

func TestAvailabilityGateSuppressesPresentAndUnknownForEveryPipeline(t *testing.T) {
	entryKey := "series/Test"
	makeCandidate := func(id string, ep int) candidate.Candidate {
		return candidate.Candidate{EntryKey: id, Episode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: ep, EpisodeEnd: ep}, Decision: candidate.DecisionSelected}
	}
	inv := inventory.Snapshot{Entries: map[string]inventory.EntryInventory{
		entryKey: {Known: true, Seasons: map[int]inventory.SeasonInventory{1: {Known: true, Episodes: map[int]struct{}{1: {}}}}},
	}}
	current := candidate.Plan{Candidates: []candidate.Candidate{makeCandidate(entryKey, 1), makeCandidate(entryKey, 2), makeCandidate("missing", 1)}}
	historical := candidate.Plan{Candidates: append([]candidate.Candidate(nil), current.Candidates...)}
	gateAvailability(&current, inv)
	gateAvailability(&historical, inv)
	for i, want := range []candidate.Decision{candidate.DecisionDuplicate, candidate.DecisionSelected, candidate.DecisionFiltered} {
		if current.Candidates[i].Decision != want || historical.Candidates[i].Decision != want {
			t.Fatalf("candidate %d: current=%s historical=%s want=%s", i, current.Candidates[i].Decision, historical.Candidates[i].Decision, want)
		}
	}
}
