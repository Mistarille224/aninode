package medianame

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Parsed struct {
	Title          string `json:"title"`
	Year           int    `json:"year,omitempty"`
	Season         int    `json:"season,omitempty"`
	SeasonExplicit bool   `json:"season_explicit,omitempty"`
	EpisodeStart   int    `json:"episode_start,omitempty"`
	EpisodeEnd     int    `json:"episode_end,omitempty"`
	Version        int    `json:"version,omitempty"`
}

var (
	sxeRE             = regexp.MustCompile(`(?i)(?:^|[ ._-])S(\d{1,2})[ ._-]*E(\d{1,4})(?:[ ._-]*-[ ._-]*E?(\d{1,4}))?(?:v(\d+))?\b`)
	xRE               = regexp.MustCompile(`(?i)(?:^|[ ._-])(\d{1,2})x(\d{1,4})(?:-(\d{1,4}))?(?:v(\d+))?\b`)
	seasonEpisodeRE   = regexp.MustCompile(`(?i)(?:^|[ ._-])Season[ ._-]+(\d{1,2})[ ._-]+Episode[ ._-]+(\d{1,4})(?:[ ._-]*-[ ._-]*(\d{1,4}))?(?:v(\d+))?\b`)
	explicitEpisodeRE = regexp.MustCompile(`(?i)(?:^|[ ._-])(?:EP|E)(\d{1,4})(?:[ ._-]*-[ ._-]*E?(\d{1,4}))?(?:v(\d+))?\b`)
	weakTailRE        = regexp.MustCompile(`(?i)\s+-\s+(\d{1,4})(?:\s*[-~]\s*(\d{1,4}))?(?:v(\d+))?\s*$`)
	weakBracketRE     = regexp.MustCompile(`(?i)^\s*(\d{1,4})(?:\s*[-~]\s*(\d{1,4}))?(?:v(\d+))?\s*$`)
	bracketSxERE      = regexp.MustCompile(`(?i)^\s*S(\d{1,2})[ ._-]*E(\d{1,4})(?:[ ._-]*-[ ._-]*E?(\d{1,4}))?(?:v(\d+))?\s*$`)
	bracketXRE        = regexp.MustCompile(`(?i)^\s*(\d{1,2})x(\d{1,4})(?:-(\d{1,4}))?(?:v(\d+))?\s*$`)
	bracketEpisodeRE  = regexp.MustCompile(`(?i)^\s*(?:EP|E)(\d{1,4})(?:[ ._-]*-[ ._-]*E?(\d{1,4}))?(?:v(\d+))?\s*$`)
	seasonSuffixRE    = regexp.MustCompile(`(?i)(?:^|[ ._-])(?:season[ ._-]*(\d{1,2})|(\d{1,2})(?:st|nd|rd|th)[ ._-]+season|s(\d{1,2}))\s*$`)
	seasonAtomRE      = regexp.MustCompile(`(?i)^\s*(?:season[ ._-]*(\d{1,2})|(\d{1,2})(?:st|nd|rd|th)[ ._-]+season|s(\d{1,2}))\s*$`)
	suffixVersionRE   = regexp.MustCompile(`(?i)\s+-\s+v(\d+)\s*$`)
	titleVersionRE    = regexp.MustCompile(`(?i)(?:\s+|[._-]+)v(\d+)\s*$`)
	yearTailRE        = regexp.MustCompile(`(?:^|[ ._-])(19\d{2}|20\d{2})\s*$`)
	yearParenTailRE   = regexp.MustCompile(`\s*\((19\d{2}|20\d{2})\)\s*$`)
	yearAtomRE        = regexp.MustCompile(`^\s*(19\d{2}|20\d{2})\s*$`)
	sceneYearHintRE   = regexp.MustCompile(`(?i)(?:^|[._])(19\d{2}|20\d{2})(?:[._ -]+(?:season[._ -]*\d{1,2}|\d{1,2}(?:st|nd|rd|th)[._ -]+season|s\d{1,2}))?\s*$`)
)

type nameStructure struct {
	Core             string
	Leading          []string
	TrailingMetadata []string
	FullyBracketed   bool
}

type episodeAtom struct {
	Season    int
	HasSeason bool
	Start     int
	End       int
	Version   int
	Explicit  bool
}

func Parse(input string) Parsed {
	return ParseMany([]string{input})[0]
}

// parseOne performs context-free parsing. Public callers go through Parse or
// ParseMany so single-name and sibling-aware parsing always share one engine.
func parseOne(stem string) Parsed {
	if stem == "" {
		return Parsed{}
	}
	if cached, ok := contextFreeParseCache.get(stem); ok {
		return cached
	}
	p := parseOneUncached(stem)
	contextFreeParseCache.put(stem, p)
	return p
}

func parseOneUncached(stem string) Parsed {
	var p Parsed
	if p, ok := parseStrong(stem); ok {
		p.SeasonExplicit = hasExplicitSeasonSyntax(stem)
		return p
	}
	p = parseWeak(stem)
	p.SeasonExplicit = hasExplicitSeasonSyntax(stem)
	return p
}

// ParseEpisodeFile reuses the media-name parser for library inventory and
// permits conservative season-directory shorthand such as "01.mkv". The
// shorthand is accepted only when the caller already has an explicit season
// context; a bare number is never treated as a release title episode globally.
func ParseEpisodeFile(name string, seasonHint int) (Parsed, bool) {
	analysis := AnalyzeFiles([]string{name})
	return EpisodeWithSeasonHint(analysis.Items[0], seasonHint)
}

// EpisodeWithSeasonHint applies filesystem season context to an already
// analyzed name. It lets inventory and organizer retain batch/differential
// evidence without parsing the same basename again.
func EpisodeWithSeasonHint(name Name, seasonHint int) (Parsed, bool) {
	p := name.Parsed
	stem, _ := stripMediaExtension(name.Input)
	if name.Components.Extension != "" {
		value := strings.TrimSpace(name.Input)
		stem = strings.TrimSpace(strings.TrimSuffix(value, filepath.Ext(value)))
	}
	if p.EpisodeStart > 0 {
		if p.Season == 0 && seasonHint >= 0 && !p.SeasonExplicit {
			p.Season = seasonHint
		}
		return p, p.Season >= 0 && validEpisodeRange(p.EpisodeStart, p.EpisodeEnd)
	}
	if seasonHint < 0 {
		return Parsed{}, false
	}
	if stem == "" {
		return Parsed{}, false
	}
	// Once the filesystem already supplies a season context, a trailing
	// " - 01"/" - 01-02" is useful reversible publication evidence even for
	// a single file. AnalyzeMany deliberately requires sibling variation before
	// upgrading this syntax globally, but local filesystem adoption does not need
	// that stronger bar because a wrong guess only creates a hardlink projection.
	if m := weakTailRE.FindStringSubmatch(stem); len(m) > 1 {
		atom := weakEpisodeAtom(m)
		if validEpisodeRange(atom.Start, atom.End) {
			return Parsed{Season: seasonHint, EpisodeStart: atom.Start, EpisodeEnd: atom.End, Version: atom.Version}, true
		}
	}
	for _, r := range stem {
		if r < '0' || r > '9' {
			return Parsed{}, false
		}
	}
	n, err := strconv.Atoi(stem)
	if err != nil || n <= 0 || n > 9999 {
		return Parsed{}, false
	}
	return Parsed{Season: seasonHint, EpisodeStart: n, EpisodeEnd: n, Version: 1}, true
}

func parseStrong(stem string) (Parsed, bool) {
	type matcher struct {
		re                                                *regexp.Regexp
		seasonGroup, episodeGroup, endGroup, versionGroup int
	}
	ms := []matcher{
		{sxeRE, 1, 2, 3, 4}, {seasonEpisodeRE, 1, 2, 3, 4}, {xRE, 1, 2, 3, 4}, {explicitEpisodeRE, 0, 1, 2, 3},
	}
	for _, m := range ms {
		loc := m.re.FindStringSubmatchIndex(stem)
		if loc == nil {
			continue
		}
		sub := m.re.FindStringSubmatch(stem)
		p := Parsed{Version: 1}
		hasEpisodeSeason := m.seasonGroup > 0
		if hasEpisodeSeason {
			p.Season = atoi(sub[m.seasonGroup])
		}
		p.EpisodeStart = atoi(sub[m.episodeGroup])
		p.EpisodeEnd = p.EpisodeStart
		if m.endGroup > 0 && sub[m.endGroup] != "" {
			p.EpisodeEnd = atoi(sub[m.endGroup])
		}
		if !validEpisodeRange(p.EpisodeStart, p.EpisodeEnd) {
			// Once a strong token matched, an invalid range must not be
			// reinterpreted by a weaker matcher later in the same string.
			return Parsed{}, false
		}
		if m.versionGroup > 0 && sub[m.versionGroup] != "" && atoi(sub[m.versionGroup]) > 0 {
			p.Version = atoi(sub[m.versionGroup])
		}
		if vm := suffixVersionRE.FindStringSubmatch(stem[loc[1]:]); len(vm) > 1 && atoi(vm[1]) > 0 {
			p.Version = atoi(vm[1])
		}
		identity := parseIdentityPrefix(stem[:loc[0]], true)
		p.Title, p.Year = identity.Title, identity.Year
		if !hasEpisodeSeason {
			p.Season = identity.Season
		}
		if p.Title == "" {
			return Parsed{}, false
		}
		return p, true
	}
	return Parsed{}, false
}

func parseWeak(stem string) Parsed {
	p := Parsed{Version: 1}
	structure := splitNameStructure(stem)
	if structure.FullyBracketed {
		if parsed, ok := parseFullyBracketed(structure.Leading); ok {
			return parsed
		}
	}

	cleaned := structure.Core
	qualYear, qualSeason, hasQualSeason := qualifiersFromGroups(structure.TrailingMetadata)
	if len(structure.Leading) >= 2 {
		if parsed, ok := parseLeadingBracketWeakEpisode(structure.Leading, cleaned, qualYear, qualSeason, hasQualSeason); ok {
			return parsed
		}
		// A nonempty title continuation is identity evidence, not disposable
		// release metadata. Keep it together with the bracketed title.
		cleaned = strings.Join(structure.Leading[1:], " ") + " " + cleaned
	}
	for _, group := range structure.TrailingMetadata {
		if standaloneYear(group) > 0 {
			continue
		}
		if _, _, ok := seasonFromAtom(group); ok {
			continue
		}
		if atom, ok := parseBracketEpisodeAtom(group); ok {
			if parsed, ok := parsedWeakEpisodeAtom(cleaned, atom); ok {
				applyQualifiers(&parsed, qualYear, qualSeason, hasQualSeason)
				return parsed
			}
		}
	}

	// Parentheses after a weak episode commonly carry opaque release metadata,
	// but a standalone year is a semantic qualifier. Remove the group only when
	// doing so exposes the episode candidate and preserve a year if present.
	parenYear := 0
	if base, group, ok := trailingGroup(cleaned, '(', ')'); ok && weakTailRE.MatchString(base) {
		parenYear = standaloneYear(group)
		cleaned = base
	}
	if m := weakTailRE.FindStringSubmatch(cleaned); len(m) > 0 {
		idx := weakTailRE.FindStringIndex(cleaned)
		if parsed, ok := parsedWeakEpisode(cleaned[:idx[0]], m); ok {
			if qualYear == 0 {
				qualYear = parenYear
			}
			applyQualifiers(&parsed, qualYear, qualSeason, hasQualSeason)
			return parsed
		}
	}

	rawTitle := rawTitleFromPrefix(cleaned)
	if rawTitle == "" {
		rawTitle = strings.TrimSpace(cleaned)
	}
	p.Title, p.Year, p.Season = titleIdentityFromRaw(rawTitle)
	applyQualifiers(&p, qualYear, qualSeason, hasQualSeason)
	p.Version = 0
	return p
}

func parseLeadingBracketWeakEpisode(leading []string, core string, qualYear, qualSeason int, hasQualSeason bool) (Parsed, bool) {
	value := strings.TrimSpace(core)
	parenYear := 0
	if base, group, ok := trailingGroup(value, '(', ')'); ok {
		// Only strip the parenthesized suffix if the remainder is exactly an
		// episode tail. Otherwise it belongs to the title/core.
		if m := leadingEpisodeTail(strings.TrimSpace(base)); len(m) > 0 {
			value = base
			parenYear = standaloneYear(group)
		}
	}
	match := leadingEpisodeTail(value)
	if len(match) == 0 {
		return Parsed{}, false
	}
	identity, ok := parseFullyBracketed(leading)
	if !ok || strings.TrimSpace(identity.Title) == "" {
		return Parsed{}, false
	}
	atom := weakEpisodeAtom(match)
	if !validEpisodeRange(atom.Start, atom.End) {
		return Parsed{}, false
	}
	version := atom.Version
	if version <= 0 {
		version = 1
	}
	parsed := Parsed{
		Title:          identity.Title,
		Year:           identity.Year,
		Season:         identity.Season,
		SeasonExplicit: identity.SeasonExplicit,
		EpisodeStart:   atom.Start,
		EpisodeEnd:     atom.End,
		Version:        version,
	}
	if qualYear == 0 {
		qualYear = parenYear
	}
	applyQualifiers(&parsed, qualYear, qualSeason, hasQualSeason)
	return parsed, true
}

// leadingEpisodeTail accepts only a complete episode tail after a bracketed
// title; matching a suffix alone would silently discard title continuation.
func leadingEpisodeTail(value string) []string {
	const prefix = "Title "
	input := prefix + strings.TrimSpace(value)
	loc := weakTailRE.FindStringIndex(input)
	if loc == nil || loc[0] != len(prefix)-1 {
		return nil
	}
	return weakTailRE.FindStringSubmatch(input)
}

func parsedWeakEpisode(titleValue string, match []string) (Parsed, bool) {
	return parsedWeakEpisodeAtom(titleValue, weakEpisodeAtom(match))
}

func parsedWeakEpisodeAtom(titleValue string, atom episodeAtom) (Parsed, bool) {
	if !validEpisodeRange(atom.Start, atom.End) {
		return Parsed{}, false
	}
	rawTitle := rawTitleFromPrefix(strings.TrimSpace(titleValue))
	if rawTitle == "" {
		return Parsed{}, false
	}
	rawTitle, titleVersion := titleAndVersion(rawTitle)
	title, year, season := titleIdentityFromRaw(rawTitle)
	seasonExplicit := hasExplicitSeasonSyntax(rawTitle)
	version := atom.Version
	if version <= 0 {
		version = 1
	}
	if version == 1 && titleVersion > 0 {
		version = titleVersion
	}
	if atom.HasSeason {
		season = atom.Season
		seasonExplicit = true
	}
	return Parsed{Title: title, Year: year, Season: season, SeasonExplicit: seasonExplicit, EpisodeStart: atom.Start, EpisodeEnd: atom.End, Version: version}, true
}

func weakEpisodeAtom(match []string) episodeAtom {
	atom := episodeAtom{Version: 1}
	if len(match) > 1 {
		atom.Start = atoi(match[1])
		atom.End = atom.Start
	}
	if len(match) > 2 && match[2] != "" {
		atom.End = atoi(match[2])
	}
	if len(match) > 3 && match[3] != "" && atoi(match[3]) > 0 {
		atom.Version = atoi(match[3])
	}
	return atom
}

func parseBracketEpisodeAtom(value string) (episodeAtom, bool) {
	for _, candidate := range []struct {
		re                                                *regexp.Regexp
		seasonGroup, episodeGroup, endGroup, versionGroup int
	}{
		{bracketSxERE, 1, 2, 3, 4},
		{bracketXRE, 1, 2, 3, 4},
		{bracketEpisodeRE, 0, 1, 2, 3},
	} {
		match := candidate.re.FindStringSubmatch(value)
		if match == nil {
			continue
		}
		atom := episodeAtom{Version: 1, Explicit: true}
		if candidate.seasonGroup > 0 {
			atom.Season = atoi(match[candidate.seasonGroup])
			atom.HasSeason = true
		}
		atom.Start = atoi(match[candidate.episodeGroup])
		atom.End = atom.Start
		if candidate.endGroup > 0 && match[candidate.endGroup] != "" {
			atom.End = atoi(match[candidate.endGroup])
		}
		if candidate.versionGroup > 0 && match[candidate.versionGroup] != "" && atoi(match[candidate.versionGroup]) > 0 {
			atom.Version = atoi(match[candidate.versionGroup])
		}
		return atom, validEpisodeRange(atom.Start, atom.End)
	}
	match := weakBracketRE.FindStringSubmatch(value)
	if match == nil {
		return episodeAtom{}, false
	}
	atom := weakEpisodeAtom(match)
	return atom, validEpisodeRange(atom.Start, atom.End)
}

// splitNameStructure is a vocabulary-free lexer for the outer shape of a
// release name. It separates bracket groups by position; it never needs to
// know whether their contents name a codec, provider, subtitle, or profile.
func splitNameStructure(stem string) nameStructure {
	value := strings.TrimSpace(stem)
	var leading []string
	rest := value
	for strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			break
		}
		leading = append(leading, strings.TrimSpace(rest[1:end]))
		rest = strings.TrimSpace(rest[end+1:])
	}
	if rest == "" && len(leading) > 0 {
		return nameStructure{Leading: leading, FullyBracketed: true}
	}
	core := rest
	var trailing []string
	for {
		base, group, ok := trailingGroup(core, '[', ']')
		if !ok {
			break
		}
		trailing = append([]string{group}, trailing...)
		core = base
	}
	return nameStructure{Core: strings.TrimSpace(core), Leading: leading, TrailingMetadata: trailing}
}

func trailingGroup(value string, open, close byte) (string, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value[len(value)-1] != close {
		return value, "", false
	}
	start := strings.LastIndexByte(value, open)
	if start < 0 || start == 0 {
		return value, "", false
	}
	return strings.TrimSpace(value[:start]), strings.TrimSpace(value[start+1 : len(value)-1]), true
}

func parseFullyBracketed(groups []string) (Parsed, bool) {
	if len(groups) < 2 {
		return Parsed{}, false
	}

	// The established fully-bracketed convention is [group][title][qualifiers]
	// [episode][metadata]. Keep unknown groups before the episode as part of the
	// title, but peel structurally unambiguous year/season qualifiers first.
	titleGroups := []string{groups[1]}
	p := Parsed{}
	for index := 2; index < len(groups); index++ {
		group := strings.TrimSpace(groups[index])
		if group == "" {
			continue
		}
		if year := standaloneYear(group); year > 0 {
			if p.Year == 0 {
				p.Year = year
			}
			// A bare 19xx/20xx bracket is much more likely to be a year qualifier
			// than an episode. Explicit E2026/SxxE2026 still parses as an episode.
			continue
		}
		if _, season, ok := seasonFromAtom(group); ok {
			p.Season = season
			p.SeasonExplicit = true
			continue
		}
		if atom, ok := parseBracketEpisodeAtom(group); ok {
			title, year, titleSeason := titleIdentity(cleanTitle(strings.Join(titleGroups, " ")), false)
			if title == "" {
				return Parsed{}, false
			}
			p.Title = title
			if p.Year == 0 {
				p.Year = year
			}
			if atom.HasSeason {
				p.Season = atom.Season
				p.SeasonExplicit = true
			} else if p.Season == 0 && !p.SeasonExplicit {
				p.Season = titleSeason
				p.SeasonExplicit = hasExplicitSeasonSyntax(strings.Join(titleGroups, " "))
			}
			p.EpisodeStart, p.EpisodeEnd, p.Version = atom.Start, atom.End, atom.Version
			if p.Version <= 0 {
				p.Version = 1
			}
			return p, true
		}
		// Preserve the old vocabulary-free behavior before an episode atom: a
		// bracket we cannot classify may be part of a split title.
		titleGroups = append(titleGroups, group)
	}

	// With no episode atom, position provides a conservative identity: the first
	// bracket is provenance and the second bracket is the title. Later unknown
	// brackets are metadata, not title continuation.
	title, year, season := titleIdentity(cleanTitle(groups[1]), false)
	if p.Year == 0 {
		p.Year = year
	}
	if p.Season == 0 && !p.SeasonExplicit {
		p.Season = season
		p.SeasonExplicit = hasExplicitSeasonSyntax(groups[1])
	}
	p.Title = title
	return p, p.Title != ""
}

func qualifiersFromGroups(groups []string) (year, season int, hasSeason bool) {
	for _, group := range groups {
		if year == 0 {
			year = standaloneYear(group)
		}
		if !hasSeason {
			_, season, hasSeason = seasonFromAtom(group)
		}
	}
	return year, season, hasSeason
}

func applyQualifiers(p *Parsed, year, season int, hasSeason bool) {
	if p.Year == 0 && year > 0 {
		p.Year = year
	}
	if p.Season == 0 && !p.SeasonExplicit && hasSeason {
		p.Season = season
		p.SeasonExplicit = true
	}
}

// titleIdentity removes only suffixes that are identity qualifiers with enough
// context to be trusted. Parenthesized years are high-confidence everywhere;
// bare years are removed only when the caller has scene-style separator
// evidence. Season suffixes may appear on either side of a year qualifier.
func titleIdentity(title string, allowBareYear bool) (string, int, int) {
	title = cleanTitle(title)
	year, season := 0, 0
	hasSeason := false
	for i := 0; i < 4; i++ {
		if year == 0 {
			if stripped, value, ok := stripParenthesizedYear(title); ok {
				title, year = stripped, value
				continue
			}
			if allowBareYear {
				if stripped, value, ok := stripBareYear(title); ok {
					title, year = stripped, value
					continue
				}
			}
		}
		if !hasSeason {
			if stripped, value, ok := stripSeasonSuffix(title); ok {
				title, season, hasSeason = stripped, value, true
				continue
			}
		}
		break
	}
	return cleanTitle(title), year, season
}

func stripSeasonSuffix(title string) (string, int, bool) {
	match := seasonSuffixRE.FindStringSubmatchIndex(title)
	if match == nil {
		return title, 0, false
	}
	parts := seasonSuffixRE.FindStringSubmatch(title)
	season, ok := seasonFromParts(parts[1:])
	if !ok {
		return title, 0, false
	}
	cleaned := strings.Trim(strings.TrimSpace(title[:match[0]]), "-_. ")
	if cleaned == "" {
		return title, 0, false
	}
	return cleaned, season, true
}

func seasonFromAtom(value string) (string, int, bool) {
	match := seasonAtomRE.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return value, 0, false
	}
	season, ok := seasonFromParts(match[1:])
	return strings.TrimSpace(value), season, ok
}

func seasonFromParts(parts []string) (int, bool) {
	for _, part := range parts {
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err == nil && n >= 0 && n <= 99 {
			return n, true
		}
	}
	return 0, false
}

func titleAndVersion(title string) (string, int) {
	match := titleVersionRE.FindStringSubmatch(strings.TrimSpace(title))
	if len(match) < 2 {
		return title, 0
	}
	index := titleVersionRE.FindStringIndex(strings.TrimSpace(title))
	if index == nil || index[0] == 0 {
		return title, 0
	}
	version := atoi(match[1])
	if version <= 0 {
		return title, 0
	}
	return strings.Trim(strings.TrimSpace(title)[:index[0]], "-_. "), version
}

func parseIdentityPrefix(raw string, allowBareYear bool) Parsed {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Parsed{}
	}
	seasonExplicit := hasExplicitSeasonSyntax(raw)
	structure := splitNameStructure(raw)
	if structure.FullyBracketed && len(structure.Leading) >= 2 {
		if parsed, ok := parseFullyBracketed(structure.Leading); ok {
			parsed.SeasonExplicit = parsed.SeasonExplicit || seasonExplicit
			return parsed
		}
	}
	raw = stripLeadingReleaseGroup(raw)
	normalized := normalizeSceneSeparators(raw)
	title, year, season := titleIdentity(normalized, allowBareYear && sceneYearHintRE.MatchString(raw))
	return Parsed{Title: title, Year: year, Season: season, SeasonExplicit: seasonExplicit}
}

func rawTitleFromPrefix(s string) string {
	structure := splitNameStructure(s)
	if structure.FullyBracketed {
		if parsed, ok := parseFullyBracketed(structure.Leading); ok {
			return parsed.Title
		}
		return ""
	}
	return strings.TrimSpace(structure.Core)
}

func titleIdentityFromRaw(raw string) (string, int, int) {
	// Dots/underscores are harmless scene separators, but a trailing bare four-
	// digit token is not a trustworthy year by itself: Blade.Runner.2049 is a
	// real title. Bare years are promoted only by stronger contexts (an explicit
	// SxxExx token or a sibling-derived episode axis). Parenthesized years remain
	// high-confidence and are handled by titleIdentity below.
	raw = normalizeSceneSeparators(raw)
	return titleIdentity(raw, false)
}

func stripLeadingReleaseGroup(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if i := strings.IndexByte(s, ']'); i >= 0 {
			return strings.TrimSpace(s[i+1:])
		}
	}
	return s
}

func stripParenthesizedYear(s string) (string, int, bool) {
	s = cleanTitle(s)
	m := yearParenTailRE.FindStringSubmatchIndex(s)
	if m == nil {
		return s, 0, false
	}
	prefix := strings.Trim(strings.TrimSpace(s[:m[0]]), " ._-")
	if prefix == "" {
		return s, 0, false
	}
	return cleanTitle(prefix), atoi(s[m[2]:m[3]]), true
}

func stripBareYear(s string) (string, int, bool) {
	s = cleanTitle(s)
	m := yearTailRE.FindStringSubmatchIndex(s)
	if m == nil {
		return s, 0, false
	}
	prefix := strings.Trim(strings.TrimSpace(s[:m[0]]), " ._-")
	// A bare four-digit numeric title (1923, 1899) is a title, not a year.
	if prefix == "" {
		return s, 0, false
	}
	return cleanTitle(prefix), atoi(s[m[2]:m[3]]), true
}

func standaloneYear(value string) int {
	match := yearAtomRE.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) < 2 {
		return 0
	}
	return atoi(match[1])
}

// hasExplicitSeasonSyntax distinguishes an absent season from an explicit S00.
// Parsed.Season intentionally keeps the public zero value compact, so callers
// that would otherwise replace season zero with path context must consult the
// original syntax before doing so.
func hasExplicitSeasonSyntax(stem string) bool {
	for _, re := range []*regexp.Regexp{sxeRE, xRE, seasonEpisodeRE} {
		if re.MatchString(stem) {
			return true
		}
	}
	structure := splitNameStructure(stem)
	leadingIdentity := structure.FullyBracketed
	if len(structure.Leading) >= 2 && !leadingIdentity {
		_, leadingIdentity = parseLeadingBracketWeakEpisode(structure.Leading, structure.Core, 0, 0, false)
	}
	if leadingIdentity {
		for _, group := range structure.Leading[1:] {
			if _, _, ok := seasonFromAtom(group); ok {
				return true
			}
			if bracketSxERE.MatchString(group) || bracketXRE.MatchString(group) {
				return true
			}
		}
		if len(structure.Leading) > 1 {
			if _, _, ok := stripSeasonSuffix(seasonIdentityPrefix(structure.Leading[1])); ok {
				return true
			}
		}
		if structure.FullyBracketed {
			return false
		}
	}
	for _, group := range structure.TrailingMetadata {
		if _, _, ok := seasonFromAtom(group); ok {
			return true
		}
		if bracketSxERE.MatchString(group) || bracketXRE.MatchString(group) {
			return true
		}
	}
	core := structure.Core
	if m := weakTailRE.FindStringIndex(core); m != nil {
		core = core[:m[0]]
	}
	core = rawTitleFromPrefix(core)
	if core == "" {
		return false
	}
	_, _, ok := stripSeasonSuffix(seasonIdentityPrefix(core))
	return ok
}

func seasonIdentityPrefix(value string) string {
	value = normalizeSceneSeparators(value)
	if title, _, ok := stripParenthesizedYear(value); ok {
		return title
	}
	return value
}

func normalizeSceneSeparators(s string) string {
	r := []rune(s)
	for i, ch := range r {
		if ch == '_' {
			r[i] = ' '
			continue
		}
		if ch == '.' {
			prevDigit := i > 0 && unicode.IsDigit(r[i-1])
			nextDigit := i+1 < len(r) && unicode.IsDigit(r[i+1])
			if !prevDigit || !nextDigit {
				r[i] = ' '
			}
		}
	}
	return cleanTitle(string(r))
}

func validEpisodeRange(start, end int) bool {
	return start > 0 && start <= 9999 && end >= start && end <= 9999
}

func stripMediaExtension(value string) (string, string) {
	value = strings.TrimSpace(value)
	ext := strings.ToLower(filepath.Ext(value))
	switch ext {
	case ".mkv", ".mp4", ".avi", ".mov", ".m4v", ".ts", ".webm":
		return strings.TrimSpace(strings.TrimSuffix(value, filepath.Ext(value))), ext
	default:
		return value, ""
	}
}

func cleanTitle(s string) string { return strings.Join(strings.Fields(strings.Trim(s, " ._-")), " ") }
func atoi(s string) int          { n, _ := strconv.Atoi(s); return n }
