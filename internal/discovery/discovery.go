package discovery

import (
	"fmt"
	"sort"
	"strings"

	"aninode/internal/catalog"
	"aninode/internal/naming"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
)

// Group is a transient projection of releases that appear to describe the same
// work. It is deliberately not persisted: once an Entry declaration exists the
// same releases resolve normally and disappear from discovery.
type Group struct {
	MediaType string            `json:"media_type"`
	Title     string            `json:"title"`
	Year      int               `json:"year,omitempty"`
	Season    int               `json:"season"`
	Releases  []release.Release `json:"releases"`
	SourceIDs []string          `json:"source_ids,omitempty"`
	releasefilter.Options
	EntryKey       string   `json:"entry_key,omitempty"`
	AcquisitionIDs []string `json:"-"`
}

type Resolution struct {
	EntryKey string `json:"entry_key,omitempty"`
	Matched  bool   `json:"matched"`
	Reason   string `json:"reason,omitempty"`
}

type Resolver struct{}

func (Resolver) Resolve(g Group, entries map[string]catalog.Entry, names naming.Index) Resolution {
	if strings.TrimSpace(g.Title) == "" {
		return Resolution{Reason: "empty series title"}
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if entries[id].MediaType == g.MediaType && MatchEntry(entries[id], names[id], g.Title, g.Year) {
			return Resolution{Matched: true, EntryKey: id}
		}
	}
	return Resolution{}
}

func MatchEntry(w catalog.Entry, names naming.EntryEvidence, title string, year int) bool {
	return naming.Match(w, names, title, year)
}

func GroupReleases(values []release.Release, sourceID string) []Group {
	m := map[string]*Group{}
	order := []string{}
	for _, v := range values {
		c := v.Components
		season := c.Season
		if season <= 0 && v.MediaType != catalog.MediaMovie && !c.SeasonExplicit {
			season = 1
		}
		if v.MediaType == catalog.MediaMovie {
			season = 0
		}
		key := fmt.Sprintf("%s|%s|%d|%d", v.MediaType, catalog.Comparable(c.Title), c.Year, season)
		g := m[key]
		if g == nil {
			g = &Group{MediaType: v.MediaType, Title: c.Title, Year: c.Year, Season: season}
			m[key] = g
			order = append(order, key)
		}
		g.Releases = append(g.Releases, v)
		if identity, err := release.AcquisitionIdentity(v); err == nil {
			g.AcquisitionIDs = appendUnique(g.AcquisitionIDs, identity.ID)
		}
		g.SourceIDs = appendUnique(g.SourceIDs, sourceID)
	}
	out := make([]Group, 0, len(order))
	for _, k := range order {
		g := *m[k]
		sort.Strings(g.SourceIDs)
		g.Options = releasefilter.OptionsFromReleases(g.Releases)
		sort.Strings(g.AcquisitionIDs)
		out = append(out, g)
	}
	return MergeGroups(out)
}

func MergeGroups(groups []Group) []Group {
	buckets := map[string][]*Group{}
	order := []*Group{}
	for _, value := range groups {
		base := fmt.Sprintf("%s|%s|%d", value.MediaType, catalog.Comparable(value.Title), value.Season)
		var g *Group
		for _, candidate := range buckets[base] {
			if candidate.Year == value.Year || candidate.Year == 0 || value.Year == 0 {
				g = candidate
				break
			}
		}
		if g == nil {
			copy := value
			copy.Releases = nil
			copy.SourceIDs = nil
			copy.Options = releasefilter.Options{}
			copy.AcquisitionIDs = nil
			g = &copy
			buckets[base] = append(buckets[base], g)
			order = append(order, g)
		} else if g.Year == 0 && value.Year != 0 {
			g.Year = value.Year
		}
		seen := map[string]bool{}
		for _, release := range g.Releases {
			seen[release.GUID+"\x00"+release.DownloadURL+"\x00"+release.Title] = true
		}
		for _, release := range value.Releases {
			rk := release.GUID + "\x00" + release.DownloadURL + "\x00" + release.Title
			if !seen[rk] {
				g.Releases = append(g.Releases, release)
				seen[rk] = true
			}
		}
		for _, sourceID := range value.SourceIDs {
			g.SourceIDs = appendUnique(g.SourceIDs, sourceID)
		}
		for _, x := range value.AcquisitionIDs {
			g.AcquisitionIDs = appendUnique(g.AcquisitionIDs, x)
		}
	}
	out := make([]Group, 0, len(order))
	for _, value := range order {
		g := *value
		sort.Strings(g.SourceIDs)
		g.Options = releasefilter.OptionsFromReleases(g.Releases)
		sort.Strings(g.AcquisitionIDs)
		out = append(out, g)
	}
	return out
}

func Unmatched(values []release.Release, sourceID string, entries map[string]catalog.Entry, names naming.Index) []Group {
	resolver := Resolver{}
	out := []Group{}
	for _, g := range GroupReleases(values, sourceID) {
		if strings.TrimSpace(g.Title) == "" {
			continue
		}
		if !resolver.Resolve(g, entries, names).Matched {
			out = append(out, g)
		}
	}
	return out
}

func appendUnique(v []string, s string) []string {
	for _, x := range v {
		if x == s {
			return v
		}
	}
	return append(v, s)
}
