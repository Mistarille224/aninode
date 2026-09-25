package episode

import (
	"fmt"

	"aninode/internal/release"
)

type Key struct {
	EntryKey     string `json:"entry_key"`
	Season       int    `json:"season"`
	EpisodeStart int    `json:"episode_start"`
	EpisodeEnd   int    `json:"episode_end,omitempty"`
	Revision     int    `json:"revision,omitempty"`
	Special      bool   `json:"special,omitempty"`
}

func FromRelease(entryKey string, defaultSeason int, r release.Release) (Key, error) {
	c := r.Components
	if c.EpisodeStart <= 0 {
		return Key{}, fmt.Errorf("release has no episode identity: %s", r.Title)
	}
	season := c.Season
	if season <= 0 && !c.SeasonExplicit {
		season = defaultSeason
	}
	if season <= 0 && !c.SeasonExplicit {
		season = 1
	}
	end := c.EpisodeEnd
	if end <= 0 {
		end = c.EpisodeStart
	}
	rev := c.Version
	if rev <= 0 {
		rev = 1
	}
	return Key{EntryKey: entryKey, Season: season, EpisodeStart: c.EpisodeStart, EpisodeEnd: end, Revision: rev, Special: season == 0}, nil
}

// End returns the normalized inclusive end of the episode interval.
func (k Key) End() int {
	if k.EpisodeEnd < k.EpisodeStart {
		return k.EpisodeStart
	}
	return k.EpisodeEnd
}

// Contains reports whether episode is covered by this key's inclusive range.
func (k Key) Contains(episode int) bool {
	return k.EpisodeStart > 0 && episode >= k.EpisodeStart && episode <= k.End()
}

// Overlaps reports whether two keys address any of the same logical episodes.
// Revision is intentionally ignored: competing revisions still overlap.
func (k Key) Overlaps(other Key) bool {
	if k.EntryKey != other.EntryKey || k.Season != other.Season || k.Special != other.Special {
		return false
	}
	if k.EpisodeStart <= 0 || other.EpisodeStart <= 0 {
		return false
	}
	return k.EpisodeStart <= other.End() && other.EpisodeStart <= k.End()
}

// Covers reports whether k contains the complete logical interval of other.
func (k Key) Covers(other Key) bool {
	if k.EntryKey != other.EntryKey || k.Season != other.Season || k.Special != other.Special {
		return false
	}
	return other.EpisodeStart > 0 && k.Contains(other.EpisodeStart) && k.Contains(other.End())
}

// Width returns the number of episodes in the normalized inclusive range.
func (k Key) Width() int {
	if k.EpisodeStart <= 0 {
		return 0
	}
	return k.End() - k.EpisodeStart + 1
}

func Logical(k Key) string {
	return fmt.Sprintf("%s/S%02d/E%04d-%04d/special=%t", k.EntryKey, k.Season, k.EpisodeStart, k.End(), k.Special)
}
func String(k Key) string { return Logical(k) + fmt.Sprintf("/v%d", k.Revision) }
