package medianame

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var batchLeadingGroupRE = regexp.MustCompile(`^\s*\[[^]]+\]\s*`)
var batchEpisodeTokenRE = regexp.MustCompile(`(?i)^(\d{1,4})(p|v\d+)?$`)

type batchToken struct {
	value string
	start int
	end   int
}

type batchRow struct {
	index   int
	source  string
	values  []string
	starts  map[int]int
	numbers map[int]int
	suffix  map[int]string
}

type batchCandidate struct {
	rows     []batchRow
	position int
}

const (
	minimumEpisodeAxisScore  = 50
	axisBaseScore            = 20
	continuousAxisBonus      = 50
	smallOrdinalBonus        = 20
	sparseCohortBonus        = 15
	yearAxisPenalty          = 55
	resolutionAxisPenalty    = 45
	pixelSuffixPenalty       = 60
	varyingYearSuffixPenalty = 70
)

// AnalyzeMany is the canonical media-name parser. It first applies the same
// context-free grammar used for a single name, then refines unresolved names
// with evidence shared by siblings. Parse is the one-element form of this
// function, so callers cannot accidentally select a different parsing engine.
func AnalyzeMany(names []string) Batch {
	return analyzeMany(names, false)
}

// AnalyzeFiles analyzes known file basenames using the same grammar as
// AnalyzeMany. Callers select eligible files; their configured extension is
// container information, even when it is not a built-in media suffix.
func AnalyzeFiles(names []string) Batch {
	return analyzeMany(names, true)
}

func analyzeMany(names []string, files bool) Batch {
	stems := make([]string, len(names))
	extensions := make([]string, len(names))
	for i, name := range names {
		if !files {
			stems[i], extensions[i] = stripMediaExtension(name)
			continue
		}
		value := strings.TrimSpace(name)
		extension := filepath.Ext(value)
		stems[i] = strings.TrimSpace(strings.TrimSuffix(value, extension))
		extensions[i] = strings.ToLower(extension)
	}
	out := make([]Parsed, len(names))
	differential := make(map[int]bool)
	rowsByIndex := make([]batchRow, len(names))
	for i, stem := range stems {
		out[i] = parseOne(stem)
		source, tokens := batchLex(stem)
		row := batchRow{index: i, source: source, starts: map[int]int{}, numbers: map[int]int{}, suffix: map[int]string{}}
		for position, token := range tokens {
			row.values = append(row.values, token.value)
			row.starts[position] = token.start
		}
		rowsByIndex[i] = row
	}

	// Build one differential cohort for every possible numeric axis. Cohort
	// identity is the stable prefix before that axis; suffix tokens are metadata
	// and may legitimately change between releases (ASS/SRT, codec, source,
	// checksum, and so on). The scoring pass below gives small varying ordinals
	// priority over years and resolutions.
	cohorts := map[string][]batchRow{}
	cohortPositions := map[string]int{}
	for i := range rowsByIndex {
		current := rowsByIndex[i]
		for position, token := range current.values {
			match := batchEpisodeTokenRE.FindStringSubmatch(token)
			if match == nil {
				continue
			}
			episode, err := strconv.Atoi(match[1])
			if err != nil || episode <= 0 || episode > 9999 {
				continue
			}
			current.numbers[position] = episode
			current.suffix[position] = strings.ToLower(match[2])
			key := strings.ToLower(strings.Join(current.values[:position], "\x00")) + "\x00#number#"
			cohorts[key] = append(cohorts[key], current)
			cohortPositions[key] = position
		}
		rowsByIndex[i] = current
	}

	best := make(map[int]batchCandidate)
	bestScores := make(map[int]int)
	for key, rows := range cohorts {
		if len(rows) < 2 {
			continue
		}
		position := cohortPositions[key]
		values := map[int]bool{}
		for _, row := range rows {
			values[row.numbers[position]] = true
		}
		if len(values) < 2 {
			continue
		}
		score := batchAxisScore(rows, position, values)
		for _, row := range rows {
			if score >= minimumEpisodeAxisScore && (best[row.index].rows == nil || score > bestScores[row.index]) {
				best[row.index] = batchCandidate{rows: rows, position: position}
				bestScores[row.index] = score
			}
		}
	}

	for index, candidate := range best {
		if out[index].EpisodeStart > 0 {
			continue
		}
		var row batchRow
		for _, value := range candidate.rows {
			if value.index == index {
				row = value
				break
			}
		}
		episode := row.numbers[candidate.position]
		if episode <= 0 {
			continue
		}
		out[index].EpisodeStart, out[index].EpisodeEnd, out[index].Version = episode, episode, 1
		differential[index] = true
		if suffix := row.suffix[candidate.position]; strings.HasPrefix(suffix, "v") {
			if version := atoi(strings.TrimPrefix(suffix, "v")); version > 0 {
				out[index].Version = version
			}
		}
		if start, ok := row.starts[candidate.position]; ok && start > 0 && start <= len(row.source) {
			identity := parseBatchIdentity(row.source[:start])
			if identity.Title != "" {
				out[index].Title = identity.Title
			}
			if out[index].Year == 0 {
				out[index].Year = identity.Year
			}
			if out[index].Season == 0 && !out[index].SeasonExplicit {
				out[index].Season = identity.Season
				out[index].SeasonExplicit = identity.SeasonExplicit
			}
		}
	}

	// Weak single-name syntax is intentionally ambiguous: "Title - 01" can be
	// part of a movie title. Only sibling variation inside the same complete
	// identity (title + year + season) upgrades it to differential evidence.
	weakCohorts := map[string][]int{}
	for i, parsed := range out {
		if parsed.EpisodeStart <= 0 || hasStrongEpisodeSyntax(stems[i]) {
			continue
		}
		key := episodeCohortKey(parsed)
		if key == "" {
			continue
		}
		weakCohorts[key] = append(weakCohorts[key], i)
	}
	for _, indexes := range weakCohorts {
		episodes := map[int]bool{}
		for _, index := range indexes {
			episodes[out[index].EpisodeStart] = true
		}
		if len(episodes) < 2 {
			continue
		}
		for _, index := range indexes {
			differential[index] = true
		}
	}
	return buildBatch(names, stems, extensions, out, differential)
}

// ParseMany projects the shared analysis for callers which only need resolved
// identities. Workflow code uses AnalyzeMany when it also needs evidence.
func ParseMany(names []string) []Parsed { return AnalyzeMany(names).Parsed() }

func batchAxisScore(rows []batchRow, position int, distinct map[int]bool) int {
	score := axisBaseScore + position
	minValue, maxValue := 10000, 0
	allYears, allResolutions, hasPixelSuffix := true, true, false
	for _, row := range rows {
		value := row.numbers[position]
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
		allYears = allYears && value >= 1900 && value <= 2099
		allResolutions = allResolutions && (value == 480 || value == 720 || value == 1080 || value == 2160)
		hasPixelSuffix = hasPixelSuffix || row.suffix[position] == "p"
	}
	if maxValue-minValue+1 == len(distinct) {
		score += continuousAxisBonus
	}
	if maxValue <= 200 {
		score += smallOrdinalBonus
	}
	// Three or more different small values at the same position are strong
	// episode evidence even when the observed set has gaps. Download clients and
	// RSS sources commonly contain only part of a season (for example 05, 06, 08),
	// so continuity improves confidence rather than being a prerequisite.
	if len(distinct) >= 3 && maxValue <= 200 {
		score += sparseCohortBonus
	}
	if allYears {
		score -= yearAxisPenalty
	}
	if allResolutions {
		score -= resolutionAxisPenalty
	}
	if hasPixelSuffix {
		score -= pixelSuffixPenalty
	}
	// A changing year after the candidate number is a strong sign that the
	// candidate is an installment/title number (Movie 1 2024, Movie 2 2025),
	// not an episode axis. This guards differential inference from manufacturing
	// a series out of similarly named sequels.
	if hasVaryingYearSuffix(rows, position) {
		score -= varyingYearSuffixPenalty
	}
	return score
}

func hasVaryingYearSuffix(rows []batchRow, position int) bool {
	maxPosition := position
	for _, row := range rows {
		if len(row.values)-1 > maxPosition {
			maxPosition = len(row.values) - 1
		}
	}
	for candidate := position + 1; candidate <= maxPosition; candidate++ {
		values := map[int]bool{}
		allYears := true
		for _, row := range rows {
			value, ok := row.numbers[candidate]
			if !ok || value < 1900 || value > 2099 {
				allYears = false
				break
			}
			values[value] = true
		}
		if allYears && len(values) > 1 {
			return true
		}
	}
	return false
}

func episodeCohortKey(parsed Parsed) string {
	title := comparableComponent(parsed.Title)
	if title == "" {
		return ""
	}
	return title + "\x00" + strconv.Itoa(parsed.Year) + "\x00" + strconv.Itoa(parsed.Season) + "\x00" + strconv.FormatBool(parsed.SeasonExplicit)
}

func parseBatchIdentity(prefix string) Parsed {
	prefix = strings.Trim(strings.TrimSpace(prefix), "-_. ")
	return parseIdentityPrefix(prefix, true)
}

func batchLex(stem string) (string, []batchToken) {
	stem = batchLeadingGroupRE.ReplaceAllString(stem, "")
	stem = strings.TrimSpace(stem)
	var tokens []batchToken
	start := -1
	for index, r := range stem {
		if isBatchDelimiter(r) {
			if start >= 0 {
				tokens = append(tokens, batchToken{value: stem[start:index], start: start, end: index})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = index
		}
	}
	if start >= 0 {
		tokens = append(tokens, batchToken{value: stem[start:], start: start, end: len(stem)})
	}
	return stem, tokens
}

func isBatchDelimiter(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '[', ']', '(', ')', '.', '_', '-':
		return true
	default:
		return false
	}
}
