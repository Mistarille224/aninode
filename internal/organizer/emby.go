package organizer

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"aninode/internal/medianame"
)

type EmbyLayout struct {
	LibraryRoot      string
	Title            string
	Year             int
	Season           int
	Specials         []SpecialMapping
	FolderProjection *FolderProjection
}
type FolderProjection struct{ TargetSeason, EpisodeOffset int }

// SpecialMapping is a user-owned exception for files whose names contain no
// reliable episode number. Source is an exact basename, keeping the rule
// deterministic and safe without guessing from ambiguous filenames.
type SpecialMapping struct {
	Source  string
	Season  int
	Episode int
}

var whitespaceRE = regexp.MustCompile(`\s+`)

type embyConfig struct {
	title      string
	season     int
	specials   map[string]SpecialMapping
	projection *FolderProjection
}

type MovieLayout struct {
	Title     string
	Year      int
	Overrides map[string]string
}
type movieConfig struct {
	title     string
	year      int
	overrides map[string]string
}

func WithMovieLayout(cfg Config, layout MovieLayout) (Config, error) {
	title := safeMediaName(layout.Title)
	if title == "" {
		return cfg, errors.New("movie title is empty after sanitizing")
	}
	cfg.movie = &movieConfig{title: title, year: layout.Year, overrides: layout.Overrides}
	return cfg, nil
}

// WithEmbyLayout derives a complete deterministic series root. Episode and
// season destinations are resolved per file, allowing conservative absolute
// numbering conversion without storing source-to-target mappings.
func WithEmbyLayout(cfg Config, layout EmbyLayout) (Config, error) {
	title := safeMediaName(layout.Title)
	if title == "" {
		return cfg, errors.New("Emby title is empty after sanitizing")
	}
	if layout.LibraryRoot == "" {
		return cfg, errors.New("Emby library root is required")
	}
	if layout.Year < 0 || layout.Year > 9999 {
		return cfg, errors.New("Emby year must be between 0 and 9999")
	}
	if layout.Season < 0 || layout.Season > 99 {
		return cfg, errors.New("Emby season must be between 0 and 99")
	}
	series := title
	if layout.Year != 0 {
		series += fmt.Sprintf(" (%04d)", layout.Year)
	}
	cfg.Target = filepath.Join(layout.LibraryRoot, series)
	specials := make(map[string]SpecialMapping, len(layout.Specials))
	for index, special := range layout.Specials {
		source := strings.TrimSpace(special.Source)
		if source == "" || filepath.Base(source) != source {
			return cfg, fmt.Errorf("specials[%d] source must be a basename", index)
		}
		if special.Season < 0 || special.Season > 99 || special.Episode <= 0 {
			return cfg, fmt.Errorf("specials[%d] requires season 0-99 and a positive episode", index)
		}
		key := strings.ToLower(source)
		if _, exists := specials[key]; exists {
			return cfg, fmt.Errorf("duplicate special source %q", source)
		}
		special.Source = source
		specials[key] = special
	}
	cfg.emby = &embyConfig{title: title, season: layout.Season, specials: specials, projection: layout.FolderProjection}
	return cfg, nil
}

func resolveEmby(name string, cfg *embyConfig) (string, bool, error) {
	return resolveEmbyParsed(name, cfg, medianame.AnalyzeFiles([]string{name}).Items[0].Parsed)
}

func resolveEmbyParsed(name string, cfg *embyConfig, parsed medianame.Parsed) (string, bool, error) {
	ext := filepath.Ext(name)
	if ext == "" {
		return "", false, nil
	}
	if special, exists := cfg.specials[strings.ToLower(name)]; exists {
		filename := fmt.Sprintf("%s S%02dE%02d%s", cfg.title, special.Season, special.Episode, ext)
		return filepath.Join(fmt.Sprintf("Season %02d", special.Season), filename), true, nil
	}
	if parsed.EpisodeStart <= 0 {
		return "", false, nil
	}
	hint := cfg.season
	if hint <= 0 {
		hint = 1
	}
	resolved, ok := medianame.EpisodeWithSeasonHint(medianame.Name{
		Input: name, Parsed: parsed, Components: medianame.Components{Extension: ext},
	}, hint)
	if !ok {
		return "", false, nil
	}
	season := resolved.Season
	start := parsed.EpisodeStart
	end := parsed.EpisodeEnd
	if end == start {
		end = 0
	}
	mapOne := func(value int) (int, int, error) {
		if value <= 0 {
			return 0, 0, errors.New("episode must be positive")
		}
		if cfg.projection != nil {
			episode := value + cfg.projection.EpisodeOffset
			if episode <= 0 {
				return 0, 0, fmt.Errorf("projected episode %d is not positive", episode)
			}
			if cfg.projection.TargetSeason < 0 || cfg.projection.TargetSeason > 99 {
				return 0, 0, fmt.Errorf("projected season %d is outside 0-99", cfg.projection.TargetSeason)
			}
			return cfg.projection.TargetSeason, episode, nil
		}
		return season, value, nil
	}
	targetSeason, targetStart, err := mapOne(start)
	if err != nil {
		return "", true, err
	}
	targetEnd := 0
	if end != 0 {
		endSeason, mappedEnd, mapErr := mapOne(end)
		if mapErr != nil {
			return "", true, mapErr
		}
		if endSeason != targetSeason {
			return "", true, errors.New("multi-episode file crosses an Emby season boundary")
		}
		targetEnd = mappedEnd
	}
	episode := fmt.Sprintf("E%02d", targetStart)
	if targetEnd != 0 {
		episode += fmt.Sprintf("-E%02d", targetEnd)
	}
	version := ""
	if parsed.Version > 1 {
		version = fmt.Sprintf(" - v%d", parsed.Version)
	}
	filename := fmt.Sprintf("%s S%02d%s%s%s", cfg.title, targetSeason, episode, version, ext)
	return filepath.Join(fmt.Sprintf("Season %02d", targetSeason), filename), true, nil
}

func safeMediaName(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(value))
	value = whitespaceRE.ReplaceAllString(value, " ")
	return strings.Trim(value, " .")
}

// PreviewDestination resolves a media basename through the same Emby naming
// and source->target episode mapping rules used by the organizer, without
// touching the filesystem.
func PreviewDestination(cfg Config, name string) (string, bool, error) {
	if cfg.emby == nil {
		return "", false, errors.New("series layout is required")
	}
	rel, matched, err := resolveEmby(filepath.Base(name), cfg.emby)
	if err != nil || !matched {
		return "", matched, err
	}
	return filepath.Join(cfg.Target, rel), true, nil
}
