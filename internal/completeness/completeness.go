package completeness

import (
	"aninode/internal/episode"
	"aninode/internal/inventory"
)

type EpisodeRange struct {
	From    int `json:"from"`
	Through int `json:"through"`
}

type SeasonState struct {
	EntryKey       string `json:"entry_key"`
	Season         int    `json:"season"`
	InventoryKnown bool   `json:"inventory_known"`
	Missing        []int  `json:"missing,omitempty"`
	Status         string `json:"status"`
	Detail         string `json:"detail,omitempty"`
}

// InternalGaps returns holes that are provable from the filesystem alone.
// It never invents a missing head or tail: only episodes strictly inside the
// observed [min,max] interval can be missing.
func InternalGaps(inv inventory.Snapshot, entryKey string, season int) ([]int, string) {
	si := inv.Season(entryKey, season)
	if !si.Known {
		return nil, "unknown"
	}
	first, last := 0, 0
	for ep := range si.Episodes {
		if ep <= 0 {
			continue
		}
		if first == 0 || ep < first {
			first = ep
		}
		if ep > last {
			last = ep
		}
	}
	if first == 0 || first == last {
		return []int{}, "continuous"
	}
	missing := make([]int, 0)
	for ep := first; ep <= last; ep++ {
		if _, ok := si.Episodes[ep]; !ok {
			missing = append(missing, ep)
		}
	}
	if len(missing) == 0 {
		return missing, "continuous"
	}
	return missing, "incomplete"
}

// MissingInRange computes exact missing episodes inside an explicit range.
// The range is supplied by the caller (normally the user), never discovered
// from remote history or persisted as runtime memory.
func MissingInRange(inv inventory.Snapshot, entryKey string, season int, r EpisodeRange) ([]int, string) {
	if r.From <= 0 || r.Through < r.From {
		return nil, "invalid"
	}
	si := inv.Season(entryKey, season)
	if !si.Known {
		return nil, "unknown"
	}
	missing := make([]int, 0)
	for ep := r.From; ep <= r.Through; ep++ {
		if _, ok := si.Episodes[ep]; !ok {
			missing = append(missing, ep)
		}
	}
	if len(missing) == 0 {
		return missing, "complete"
	}
	return missing, "incomplete"
}

// EpisodeKeys turns filesystem-proven target holes into exact bound search keys.
// Folder/offset projection is applied later to each candidate using current
// source evidence; there is no separate reverse-projection model.
func EpisodeKeys(entryKey string, season int, missing []int) []episode.Key {
	out := make([]episode.Key, 0, len(missing))
	for _, ep := range missing {
		if ep <= 0 {
			continue
		}
		out = append(out, episode.Key{EntryKey: entryKey, Season: season, EpisodeStart: ep, EpisodeEnd: ep, Special: season == 0})
	}
	return out
}
