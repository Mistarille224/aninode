// Package releasefilter owns user-declared release attribute filters and
// transient option aggregation. An empty dimension is deliberately open: only
// values the user explicitly keeps in a dimension restrict acquisition.
package releasefilter

import (
	"fmt"
	"sort"
	"strings"

	"aninode/internal/medianame"
	"aninode/internal/release"
)

type Filters struct {
	Groups      []string `json:"groups,omitempty"`
	Resolutions []string `json:"resolutions,omitempty"`
	Subtitles   []string `json:"subtitles,omitempty"`
}

type Options struct {
	Groups      []string `json:"groups,omitempty"`
	Resolutions []string `json:"resolutions,omitempty"`
	Subtitles   []string `json:"subtitles,omitempty"`
}

func (f Filters) Empty() bool {
	return len(f.Groups) == 0 && len(f.Resolutions) == 0 && len(f.Subtitles) == 0
}

func (f Filters) Equal(other Filters) bool {
	a, b := f.Normalize(), other.Normalize()
	return equalValues(a.Groups, b.Groups) && equalValues(a.Resolutions, b.Resolutions) && equalValues(a.Subtitles, b.Subtitles)
}

// Normalize trims, de-duplicates case-insensitively and sorts every dimension.
// Persisted declarations and transient observations therefore share one value
// identity rule instead of reimplementing string cleanup at each call site.
func (f Filters) Normalize() Filters {
	return Filters{
		Groups:      normalizeValues(f.Groups),
		Resolutions: normalizeValues(f.Resolutions),
		Subtitles:   normalizeValues(f.Subtitles),
	}
}

func (f Filters) Validate() error {
	for name, values := range map[string][]string{
		"groups": f.Groups, "resolutions": f.Resolutions, "subtitles": f.Subtitles,
	} {
		seen := map[string]bool{}
		for i, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				return fmt.Errorf("filters.%s[%d] is empty", name, i)
			}
			key := comparable(value)
			if seen[key] {
				return fmt.Errorf("filters.%s contains duplicate value %q", name, value)
			}
			seen[key] = true
		}
	}
	return nil
}

// RejectReason is the single hard-filter decision used by RSS, search and
// backfill candidates. Empty dimensions always allow every observed value.
func (f Filters) RejectReason(r release.Release) string {
	if !allows(f.Groups, r.Group) {
		return "release group is not allowed by entry filters"
	}
	if !allows(f.Resolutions, r.Resolution) {
		return "resolution is not allowed by entry filters"
	}
	if !allows(f.Subtitles, r.Subtitle) {
		return "subtitle is not allowed by entry filters"
	}
	return ""
}

func (o Options) Merge(other Options) Options {
	return Options{
		Groups:      mergeValues(o.Groups, other.Groups),
		Resolutions: mergeValues(o.Resolutions, other.Resolutions),
		Subtitles:   mergeValues(o.Subtitles, other.Subtitles),
	}
}

func (o Options) WithFilters(f Filters) Options {
	return o.Merge(Options{Groups: f.Groups, Resolutions: f.Resolutions, Subtitles: f.Subtitles})
}

func OptionsFromReleases(values []release.Release) Options {
	var out Options
	for _, value := range values {
		out.Groups = appendValue(out.Groups, value.Group)
		out.Resolutions = appendValue(out.Resolutions, value.Resolution)
		out.Subtitles = appendValue(out.Subtitles, value.Subtitle)
	}
	out.Groups = sortValues(out.Groups)
	out.Resolutions = sortValues(out.Resolutions)
	out.Subtitles = sortValues(out.Subtitles)
	return out
}

// OptionsFromNames turns already-parsed media names into the same transient
// option set used by RSS/search releases. Migration and filesystem observation
// can therefore share the release-filter vocabulary without inventing a second
// aggregation rule around medianame.Components.
func OptionsFromNames(values []medianame.Name) Options {
	var out Options
	for _, value := range values {
		out.Groups = appendValue(out.Groups, value.Components.ReleaseGroup)
		out.Resolutions = appendValue(out.Resolutions, value.Components.Resolution)
		out.Subtitles = appendValue(out.Subtitles, value.Components.Subtitle)
	}
	out.Groups = sortValues(out.Groups)
	out.Resolutions = sortValues(out.Resolutions)
	out.Subtitles = sortValues(out.Subtitles)
	return out
}

func NewOptions(groups, resolutions, subtitles []string) Options {
	return Options{
		Groups: normalizeValues(groups), Resolutions: normalizeValues(resolutions), Subtitles: normalizeValues(subtitles),
	}
}

func Same(a, b string) bool {
	return comparable(a) == comparable(b)
}

func allows(allowed []string, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if Same(candidate, value) {
			return true
		}
	}
	return false
}

func mergeValues(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, value := range b {
		out = appendValue(out, value)
	}
	return sortValues(out)
}

func normalizeValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = appendValue(out, value)
	}
	return sortValues(out)
}

func appendValue(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, current := range values {
		if Same(current, value) {
			return values
		}
	}
	return append(values, value)
}

func sortValues(values []string) []string {
	sort.Slice(values, func(i, j int) bool {
		ai, aj := comparable(values[i]), comparable(values[j])
		if ai == aj {
			return values[i] < values[j]
		}
		return ai < aj
	})
	return values
}

func equalValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Same(a[i], b[i]) {
			return false
		}
	}
	return true
}

func comparable(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
