package medianame

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Components are the semantic building blocks of one media name. They contain
// syntax facts only; path-dependent rules such as OVA/Season 00 intentionally
// live in the consuming workflow.
type Components struct {
	Title           string   `json:"title,omitempty"`
	Year            int      `json:"year,omitempty"`
	Season          int      `json:"season,omitempty"`
	SeasonExplicit  bool     `json:"season_explicit,omitempty"`
	EpisodeStart    int      `json:"episode_start,omitempty"`
	EpisodeEnd      int      `json:"episode_end,omitempty"`
	Version         int      `json:"version,omitempty"`
	ReleaseGroup    string   `json:"release_group,omitempty"`
	Metadata        []string `json:"metadata,omitempty"`
	Resolution      string   `json:"resolution,omitempty"`
	Subtitle        string   `json:"subtitle,omitempty"`
	Extension       string   `json:"extension,omitempty"`
	EpisodeEvidence string   `json:"episode_evidence,omitempty"`
}

const (
	EpisodeEvidenceExplicit     = "explicit"
	EpisodeEvidencePublication  = "publication"
	EpisodeEvidenceDifferential = "differential"
	EpisodeEvidenceWeak         = "weak"
)

type Name struct {
	Input      string     `json:"input"`
	Parsed     Parsed     `json:"parsed"`
	Components Components `json:"components"`
}

type ComponentRole string
type ChangeExpectation string

const (
	RoleIdentity   ComponentRole = "identity"
	RoleQualifier  ComponentRole = "qualifier"
	RoleSequence   ComponentRole = "sequence"
	RoleRevision   ComponentRole = "revision"
	RoleProvenance ComponentRole = "provenance"
	RoleTechnical  ComponentRole = "technical"
	RoleContainer  ComponentRole = "container"

	ChangeInvariant       ChangeExpectation = "invariant"
	ChangeUsuallyStable   ChangeExpectation = "usually_stable"
	ChangeExpectedVarying ChangeExpectation = "expected_varying"
	ChangeFreelyVarying   ChangeExpectation = "freely_varying"
)

// Difference reports the values observed for one semantic component. Stable
// means every item has the same value; absent and present are deliberately
// different values so consumers can choose their own interpretation.
type Difference struct {
	Field       string            `json:"field"`
	Role        ComponentRole     `json:"role"`
	Expectation ChangeExpectation `json:"expectation"`
	Stable      bool              `json:"stable"`
	Values      []string          `json:"values"`
}

type Cohort struct {
	Key         string       `json:"key"`
	ItemIndexes []int        `json:"item_indexes"`
	Differences []Difference `json:"differences"`
}

type Batch struct {
	Items   []Name   `json:"items"`
	Cohorts []Cohort `json:"cohorts,omitempty"`
}

func (b Batch) Parsed() []Parsed {
	out := make([]Parsed, len(b.Items))
	for i := range b.Items {
		out[i] = b.Items[i].Parsed
	}
	return out
}

func (b Batch) DifferencesFor(index int) []Difference {
	for _, cohort := range b.Cohorts {
		for _, candidate := range cohort.ItemIndexes {
			if candidate == index {
				return cohort.Differences
			}
		}
	}
	return nil
}

var componentBracketRE = regexp.MustCompile(`\[([^\]]*)\]`)

func buildBatch(inputs, stems, extensions []string, parsed []Parsed, differential map[int]bool) Batch {
	b := Batch{Items: make([]Name, len(inputs))}
	for i := range inputs {
		b.Items[i] = Name{Input: inputs[i], Parsed: parsed[i], Components: componentsOf(stems[i], parsed[i], differential[i])}
		b.Items[i].Components.Extension = extensions[i]
	}
	groups := map[string][]int{}
	order := []string{}
	for i, item := range b.Items {
		key := componentIdentityKey(item.Components)
		if key == "" {
			key = fmt.Sprintf("#unknown-%d", i)
		}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	for _, key := range order {
		indexes := groups[key]
		b.Cohorts = append(b.Cohorts, Cohort{Key: key, ItemIndexes: indexes, Differences: compareComponents(b.Items, indexes)})
	}
	return b
}

type componentField struct {
	name        string
	role        ComponentRole
	expectation ChangeExpectation
	value       func(Components) string
}

var componentFields = []componentField{
	{"title", RoleIdentity, ChangeInvariant, func(c Components) string { return comparableComponent(c.Title) }},
	{"year", RoleQualifier, ChangeUsuallyStable, func(c Components) string { return number(c.Year) }},
	{"season", RoleQualifier, ChangeUsuallyStable, func(c Components) string { return seasonValue(c.Season, c.SeasonExplicit) }},
	{"episode", RoleSequence, ChangeExpectedVarying, func(c Components) string { return episodeValue(c) }},
	{"version", RoleRevision, ChangeFreelyVarying, func(c Components) string { return number(c.Version) }},
	{"release_group", RoleProvenance, ChangeFreelyVarying, func(c Components) string { return c.ReleaseGroup }},
	{"metadata", RoleTechnical, ChangeFreelyVarying, func(c Components) string { return strings.Join(c.Metadata, "\x1f") }},
	{"resolution", RoleTechnical, ChangeFreelyVarying, func(c Components) string { return c.Resolution }},
	{"subtitle", RoleTechnical, ChangeFreelyVarying, func(c Components) string { return c.Subtitle }},
	{"extension", RoleContainer, ChangeFreelyVarying, func(c Components) string { return c.Extension }},
}

func compareComponents(items []Name, indexes []int) []Difference {
	out := make([]Difference, 0, len(componentFields))
	for _, field := range componentFields {
		set := map[string]bool{}
		for _, index := range indexes {
			set[field.value(items[index].Components)] = true
		}
		values := make([]string, 0, len(set))
		for value := range set {
			values = append(values, strings.ReplaceAll(value, "\x1f", " | "))
		}
		sort.Strings(values)
		out = append(out, Difference{Field: field.name, Role: field.role, Expectation: field.expectation, Stable: len(values) <= 1, Values: values})
	}
	return out
}

func componentIdentityKey(c Components) string {
	title := comparableComponent(c.Title)
	if title == "" {
		return ""
	}
	return fmt.Sprintf("%s\x00%d\x00%d\x00%t", title, c.Year, c.Season, c.SeasonExplicit)
}

func comparableComponent(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(value)), " "))
}

func componentsOf(input string, p Parsed, differential bool) Components {
	c := Components{Title: p.Title, Year: p.Year, Season: p.Season, SeasonExplicit: p.SeasonExplicit, EpisodeStart: p.EpisodeStart, EpisodeEnd: p.EpisodeEnd, Version: p.Version}
	c.EpisodeEvidence = episodeEvidenceOf(input, p, differential)
	matches := componentBracketRE.FindAllStringSubmatch(input, -1)
	for i, match := range matches {
		value := strings.TrimSpace(match[1])
		if value == "" {
			continue
		}
		if i == 0 && strings.HasPrefix(strings.TrimSpace(input), "[") {
			c.ReleaseGroup = value
			continue
		}
		if bracketBelongsToIdentity(value, p) {
			continue
		}
		c.Metadata = append(c.Metadata, value)
	}
	c.Resolution = resolutionOf(input)
	c.Subtitle = subtitleOf(input)
	return c
}

func seasonValue(season int, explicit bool) string {
	if season == 0 && !explicit {
		return ""
	}
	return number(season)
}

func episodeEvidenceOf(input string, p Parsed, differential bool) string {
	if p.EpisodeStart <= 0 {
		return ""
	}
	switch {
	case differential:
		return EpisodeEvidenceDifferential
	case hasStrongEpisodeSyntax(input) || hasFullyBracketedEpisodeSyntax(input) || hasExplicitSeasonSyntax(input):
		return EpisodeEvidenceExplicit
	case hasPublicationEpisodeSyntax(input):
		return EpisodeEvidencePublication
	default:
		return EpisodeEvidenceWeak
	}
}

// ReliableEpisodeEvidence is the shared confidence boundary for workflows that
// need to distinguish an actual episodic release from a merely numeric title.
// A plain lone "Title - 01" remains weak; release wrappers, explicit syntax,
// and sibling differential evidence are reliable.
func ReliableEpisodeEvidence(c Components) bool {
	switch c.EpisodeEvidence {
	case EpisodeEvidenceExplicit, EpisodeEvidencePublication, EpisodeEvidenceDifferential:
		return true
	default:
		return false
	}
}

func bracketBelongsToIdentity(value string, p Parsed) bool {
	if standaloneYear(value) > 0 {
		return true
	}
	if _, _, ok := seasonFromAtom(value); ok {
		return true
	}
	if atom, ok := parseBracketEpisodeAtom(value); ok {
		return p.EpisodeStart > 0 && atom.Start == p.EpisodeStart && atom.End == p.EpisodeEnd
	}
	title, year, season := titleIdentity(value, false)
	if comparableComponent(title) != comparableComponent(p.Title) {
		return false
	}
	if year > 0 && year != p.Year {
		return false
	}
	if hasExplicitSeasonSyntax(value) && season != p.Season {
		return false
	}
	return title != ""
}

func hasFullyBracketedEpisodeSyntax(stem string) bool {
	structure := splitNameStructure(stem)
	if !structure.FullyBracketed {
		return false
	}
	p, ok := parseFullyBracketed(structure.Leading)
	return ok && p.EpisodeStart > 0
}

func hasPublicationEpisodeSyntax(stem string) bool {
	structure := splitNameStructure(stem)
	if structure.FullyBracketed || len(structure.Leading) == 0 {
		return false
	}
	cleaned := structure.Core
	if base, _, ok := trailingGroup(cleaned, '(', ')'); ok && weakTailRE.MatchString(base) {
		cleaned = base
	}
	if weakTailRE.MatchString(cleaned) {
		return true
	}
	for _, group := range structure.TrailingMetadata {
		if standaloneYear(group) > 0 {
			continue
		}
		if _, _, ok := seasonFromAtom(group); ok {
			continue
		}
		if _, ok := parseBracketEpisodeAtom(group); ok {
			return true
		}
	}
	return false
}

var componentResolutionRE = regexp.MustCompile(`(?i)(2160p|1080p|720p|480p)`)

func resolutionOf(input string) string {
	if match := componentResolutionRE.FindStringSubmatch(input); len(match) > 1 {
		return strings.ToLower(match[1])
	}
	return ""
}

func subtitleOf(input string) string {
	lower := strings.ToLower(input)
	switch {
	case strings.Contains(input, "简繁") || strings.Contains(input, "繁简") || strings.Contains(lower, "chs&cht"):
		return "zh-Hans+zh-Hant"
	case strings.Contains(input, "简") || strings.Contains(lower, "chs") || strings.Contains(lower, "zh-hans"):
		return "zh-Hans"
	case strings.Contains(input, "繁") || strings.Contains(lower, "cht") || strings.Contains(lower, "zh-hant"):
		return "zh-Hant"
	default:
		return ""
	}
}

func hasStrongEpisodeSyntax(stem string) bool {
	_, ok := parseStrong(stem)
	return ok
}

func number(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}

func episodeValue(c Components) string {
	if c.EpisodeStart == 0 {
		return ""
	}
	if c.EpisodeEnd > c.EpisodeStart {
		return fmt.Sprintf("%d-%d", c.EpisodeStart, c.EpisodeEnd)
	}
	return fmt.Sprintf("%d", c.EpisodeStart)
}

var nonContentTitleRE = regexp.MustCompile(`(?i)(?:^|[ ._-])(?:ncop|nced|sp\d*|sample|menu|scans?|fonts?)(?:$|[ ._-])`)

// ReliableEpisodeTitle exposes the parsed, cohort-aware identity used by
// migration. Weak single-file guesses and common extras are not identities.
func ReliableEpisodeTitle(name Name) (string, bool) {
	title := strings.Join(strings.Fields(strings.TrimSpace(name.Components.Title)), " ")
	if title == "" || name.Parsed.EpisodeStart <= 0 || !ReliableEpisodeEvidence(name.Components) || nonContentTitleRE.MatchString(title) {
		return "", false
	}
	return title, true
}

func ReliableEpisodeTitles(batch Batch) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range batch.Items {
		if title, ok := ReliableEpisodeTitle(item); ok {
			key := comparableComponent(title)
			if !seen[key] {
				seen[key] = true
				out = append(out, title)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := comparableComponent(out[i]), comparableComponent(out[j])
		if a == b {
			return out[i] < out[j]
		}
		return a < b
	})
	return out
}
