package catalog

import (
	"aninode/internal/filesystem"
	"aninode/internal/jsonfile"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
	"aninode/internal/releasefilter"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	MediaSeries = "series"
	MediaMovie  = "movie"
)

type SpecialMapping struct {
	Source  string `json:"source"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

type OutputLayout struct {
	Title string `json:"title,omitempty"`
}

type MovieDeclaration struct {
	Classify map[string]string `json:"classify,omitempty"`
}

type FolderProjection struct {
	Season        int `json:"season"`
	EpisodeOffset int `json:"episode_offset,omitempty"`
}

type Declaration struct {
	Title     string                      `json:"title,omitempty"`
	Year      int                         `json:"year,omitempty"`
	Sources   []string                    `json:"sources,omitempty"`
	Enabled   *bool                       `json:"enabled,omitempty"`
	Filters   releasefilter.Filters       `json:"filters,omitempty"`
	Specials  []SpecialMapping            `json:"specials,omitempty"`
	Output    OutputLayout                `json:"output,omitempty"`
	Blacklist []string                    `json:"blacklist,omitempty"`
	Movie     MovieDeclaration            `json:"movie,omitempty"`
	Folders   map[string]FolderProjection `json:"folders,omitempty"`
}

// InitialDeclarationInput is the durable projection of a workflow-local
// observation. Runtime filesystem facts which can be rediscovered do not
// belong here. Season publication offsets are sparse series-level intent in
// the root .aninode.json; folder keys are real source namespace names and target seasons are direct publication intent.

type InitialDeclarationInput struct {
	MediaType      string
	CanonicalTitle string
}

// NewInitialDeclaration owns defaults for every newly created Entry.
func NewInitialDeclaration(in InitialDeclarationInput) Declaration {
	return Declaration{Title: strings.TrimSpace(in.CanonicalTitle), Blacklist: []string{}}
}

func (d Declaration) MarshalJSON() ([]byte, error) {
	type declarationJSON struct {
		Title    string                 `json:"title,omitempty"`
		Year     int                    `json:"year,omitempty"`
		Sources  []string               `json:"sources,omitempty"`
		Enabled  *bool                  `json:"enabled,omitempty"`
		Filters  *releasefilter.Filters `json:"filters,omitempty"`
		Specials []SpecialMapping       `json:"specials,omitempty"`
		Output   *struct {
			Title string `json:"title,omitempty"`
		} `json:"output,omitempty"`
		Blacklist *[]string                   `json:"blacklist,omitempty"`
		Movie     *MovieDeclaration           `json:"movie,omitempty"`
		Folders   map[string]FolderProjection `json:"folders,omitempty"`
	}
	var filters *releasefilter.Filters
	if !d.Filters.Empty() {
		normalized := d.Filters.Normalize()
		filters = &normalized
	}
	var output *struct {
		Title string `json:"title,omitempty"`
	}
	if strings.TrimSpace(d.Output.Title) != "" {
		output = &struct {
			Title string `json:"title,omitempty"`
		}{Title: d.Output.Title}
	}
	var blacklist *[]string
	if d.Blacklist != nil {
		blacklist = &d.Blacklist
	}
	var movie *MovieDeclaration
	if len(d.Movie.Classify) > 0 {
		movie = &d.Movie
	}
	return json.Marshal(declarationJSON{Title: d.Title, Year: d.Year, Sources: d.Sources, Enabled: d.Enabled, Filters: filters, Specials: d.Specials, Output: output, Blacklist: blacklist, Movie: movie, Folders: d.Folders})
}

func DeclarationFromEntry(e Entry) Declaration {
	d := Declaration{Title: e.Title, Year: e.Year, Sources: append([]string(nil), e.Sources...), Filters: e.Filters, Specials: append([]SpecialMapping(nil), e.Specials...), Output: OutputLayout{Title: e.Output.Title}, Blacklist: append([]string(nil), e.Blacklist...), Movie: e.Movie, Folders: cloneFolderProjections(e.FolderProjections)}
	if !e.Enabled {
		disabled := false
		d.Enabled = &disabled
	}
	return d
}

func EntryFromDeclaration(d Declaration, mediaType, key, declarationPath, libraryRoot string) Entry {
	enabled := d.Enabled == nil || *d.Enabled
	e := Entry{Key: key, MediaType: mediaType, DeclarationPath: declarationPath, Title: d.Title, Year: d.Year, Enabled: enabled, Sources: append([]string(nil), d.Sources...), Filters: d.Filters, Specials: append([]SpecialMapping(nil), d.Specials...), Blacklist: append([]string(nil), d.Blacklist...), Movie: d.Movie, FolderProjections: cloneFolderProjections(d.Folders)}
	e.Output.Title = d.Output.Title
	e.Path = TargetPathForEntry(libraryRoot, e)
	return e
}

type Entry struct {
	Key               string                      `json:"key"`
	MediaType         string                      `json:"media_type"`
	Path              string                      `json:"path"`
	Title             string                      `json:"title"`
	Year              int                         `json:"year,omitempty"`
	Seasons           []int                       `json:"seasons"`
	Enabled           bool                        `json:"enabled"`
	Sources           []string                    `json:"sources,omitempty"`
	Filters           releasefilter.Filters       `json:"filters,omitempty"`
	Specials          []SpecialMapping            `json:"specials,omitempty"`
	Output            OutputLayout                `json:"output,omitempty"`
	Blacklist         []string                    `json:"blacklist,omitempty"`
	Movie             MovieDeclaration            `json:"movie,omitempty"`
	FolderProjections map[string]FolderProjection `json:"folder_projections,omitempty"`
	DeclarationPath   string                      `json:"declaration_path"`
}

var (
	seriesRE          = regexp.MustCompile(`^(.*?)(?: \(([0-9]{4})\))?$`)
	seasonRE          = regexp.MustCompile(`(?i)^Season ([0-9]{1,2})$`)
	seasonContainerRE = regexp.MustCompile(`(?i)^season[ ._-]*([0-9]{1,2})$`)
)

// Key is a runtime locator derived from filesystem namespace, never persisted.
func Key(mediaType, name string) (string, error) {
	name = strings.TrimSpace(name)
	if mediaType != MediaSeries && mediaType != MediaMovie {
		return "", fmt.Errorf("unknown media type %q", mediaType)
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("entry name must be one filesystem directory name")
	}
	return mediaType + "/" + name, nil
}

func SplitKey(key string) (mediaType, name string, err error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid entry key %q", key)
	}
	if _, err := Key(parts[0], parts[1]); err != nil {
		return "", "", err
	}
	return parts[0], parts[1], nil
}

// Discover loads declared Entries only and is strictly read-only. Automatic
// adoption of structurally valid source directories is an explicit cycle
// mutation (AdoptStructuredSources), never a hidden load/reload/readiness side effect.
func Discover(root string) (map[string]Entry, error) {
	return discover(root, root, MediaSeries)
}

func DiscoverMovies(root string) (map[string]Entry, error) {
	return discover(root, root, MediaMovie)
}

func DiscoverAt(declarationRoot, libraryRoot string) (map[string]Entry, error) {
	return discover(declarationRoot, libraryRoot, MediaSeries)
}

func DiscoverMoviesAt(declarationRoot, libraryRoot string) (map[string]Entry, error) {
	return discover(declarationRoot, libraryRoot, MediaMovie)
}

func discover(root, libraryRoot, mediaType string) (map[string]Entry, error) {
	result := make(map[string]Entry)
	if root == "" || !filepath.IsAbs(root) {
		return result, nil
	}
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return result, fmt.Errorf("declaration root is not a real directory: %s", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return result, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("symlink is not a reliable declaration namespace entry: %s", path)
		}
		d, err := ReadDeclaration(path)
		if err != nil {
			return result, fmt.Errorf("%s declaration: %w", path, err)
		}
		if d == nil {
			continue
		}
		if strings.TrimSpace(d.Title) == "" {
			return result, fmt.Errorf("%s declaration title is required", path)
		}
		key := mediaType + "/" + entry.Name()
		item := EntryFromDeclaration(*d, mediaType, key, path, libraryRoot)
		if mediaType == MediaSeries {
			inferred, inferErr := InferSeriesContainers(path)
			if inferErr != nil {
				return result, inferErr
			}
			seasonSet := map[int]bool{}
			for _, container := range inferred {
				name := filepath.Base(container.Path)
				rule, configured := d.Folders[name]
				if !configured {
					rule.Season = container.SuggestedSeason
				}
				seasonSet[rule.Season] = true
			}
			for _, rule := range d.Folders {
				seasonSet[rule.Season] = true
			}
			for season := range seasonSet {
				item.Seasons = append(item.Seasons, season)
			}
			sort.Ints(item.Seasons)
			if len(item.Seasons) == 0 {
				continue
			}
		}
		result[key] = item
	}
	return result, nil
}

type SeriesContainerInference struct {
	Path            string
	SuggestedSeason int
	Media           []string
	seasonKnown     bool
}

// InferSeriesContainers treats every real first-level directory as one source
// projection container. Directory names never create episode identity. A
// conventional Season NN/seasonNN container name may provide a reversible
// target-season suggestion; otherwise suggestions come from leaf media
// basenames parsed by the same medianame pipeline used by RSS/search. If no
// evidence exists, stable directory order supplies a reversible default that
// the user may override in .aninode.json.
//
// Media may be nested arbitrarily below the first-level container. Nested
// directories are grouping topology only (for example a multifile torrent with
// video/subtitle components) and are never parsed as media names.
func InferSeriesContainers(root string) ([]SeriesContainerInference, error) {
	return InferSeriesContainersWithExtensions(root, nil)
}

func InferSeriesContainersWithExtensions(root string, extensions []string) ([]SeriesContainerInference, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	type candidate struct{ SeriesContainerInference }
	var values []candidate
	used := map[int]bool{}
	for _, container := range entries {
		if !container.IsDir() || strings.HasPrefix(container.Name(), ".") {
			continue
		}
		path := filepath.Join(root, container.Name())
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		var media []string
		walkErr := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if current == path {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			extset := mediafile.ExtensionSet(extensions)
			if len(extset) == 0 {
				extset = mediafile.ExtensionSet([]string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "webm"})
			}
			if mediafile.HasAllowedExtension(current, extset) {
				media = append(media, current)
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
		sort.Strings(media)
		names := make([]string, len(media))
		for i := range media {
			names[i] = filepath.Base(media[i])
		}
		analysis := medianame.AnalyzeFiles(names)
		counts := map[int]int{}
		for _, item := range analysis.Items {
			if item.Parsed.SeasonExplicit || item.Parsed.Season > 0 {
				counts[item.Parsed.Season]++
			}
		}
		season := 0
		seasonKnown := false
		if match := seasonContainerRE.FindStringSubmatch(container.Name()); len(match) == 2 {
			season, _ = strconv.Atoi(match[1])
			seasonKnown = true
		} else {
			bestCount := -1
			for candidateSeason, count := range counts {
				if count > bestCount || (count == bestCount && (!seasonKnown || candidateSeason < season)) {
					season, bestCount = candidateSeason, count
					seasonKnown = true
				}
			}
		}
		if seasonKnown {
			used[season] = true
		}
		values = append(values, candidate{SeriesContainerInference: SeriesContainerInference{Path: path, SuggestedSeason: season, Media: media, seasonKnown: seasonKnown}})
	}
	// A directory with no season-bearing leaf names still needs a usable default
	// projection. Ordering is deterministic and carries no claim about what the
	// source directory name means.
	sort.Slice(values, func(i, j int) bool { return values[i].Path < values[j].Path })
	next := 1
	for i := range values {
		if values[i].seasonKnown {
			continue
		}
		for used[next] {
			next++
		}
		values[i].SuggestedSeason = next
		values[i].seasonKnown = true
		used[next] = true
		next++
	}
	out := make([]SeriesContainerInference, 0, len(values))
	for _, value := range values {
		out = append(out, value.SeriesContainerInference)
	}
	return out, nil
}

// InferSeriesContainersFromSnapshot derives the same container hints from an
// already observed tree. It never touches the filesystem, so callers can keep
// one coherent observation across all detail projections.
func InferSeriesContainersFromSnapshot(root string, snapshot filesystem.Snapshot, extensions []string) []SeriesContainerInference {
	root = filepath.Clean(root)
	extset := mediafile.ExtensionSet(extensions)
	if len(extset) == 0 {
		extset = mediafile.ExtensionSet([]string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "webm"})
	}
	type candidate struct{ SeriesContainerInference }
	byPath := map[string]*candidate{}
	for path := range snapshot.Directories {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 1 || strings.HasPrefix(parts[0], ".") {
			continue
		}
		c := candidate{SeriesContainerInference: SeriesContainerInference{Path: path}}
		byPath[path] = &c
	}
	for path := range snapshot.Paths {
		if !mediafile.HasAllowedExtension(path, extset) {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 {
			continue
		}
		container := filepath.Join(root, parts[0])
		if c := byPath[container]; c != nil {
			c.Media = append(c.Media, path)
		}
	}
	values := make([]candidate, 0, len(byPath))
	used := map[int]bool{}
	for _, ptr := range byPath {
		c := *ptr
		sort.Strings(c.Media)
		names := make([]string, len(c.Media))
		for i, path := range c.Media {
			names[i] = filepath.Base(path)
		}
		counts := map[int]int{}
		for _, item := range medianame.AnalyzeFiles(names).Items {
			if item.Parsed.SeasonExplicit || item.Parsed.Season > 0 {
				counts[item.Parsed.Season]++
			}
		}
		name := filepath.Base(c.Path)
		if match := seasonContainerRE.FindStringSubmatch(name); len(match) == 2 {
			c.SuggestedSeason, _ = strconv.Atoi(match[1])
			c.seasonKnown = true
		} else {
			best := -1
			for season, count := range counts {
				if count > best || (count == best && (!c.seasonKnown || season < c.SuggestedSeason)) {
					c.SuggestedSeason, best, c.seasonKnown = season, count, true
				}
			}
		}
		if c.seasonKnown {
			used[c.SuggestedSeason] = true
		}
		values = append(values, c)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Path < values[j].Path })
	next := 1
	for i := range values {
		if values[i].seasonKnown {
			continue
		}
		for used[next] {
			next++
		}
		values[i].SuggestedSeason, values[i].seasonKnown, used[next] = next, true, true
		next++
	}
	out := make([]SeriesContainerInference, 0, len(values))
	for _, c := range values {
		out = append(out, c.SeriesContainerInference)
	}
	return out
}

// DefaultDeclarationForLocalAdoption derives only durable user-facing
// defaults. It reads canonical media names but never mutates media or target
// paths and never communicates with a downloader.
func DefaultDeclarationForLocalAdoption(path, title string, folders map[string]FolderProjection, mediaType string) (Declaration, error) {
	if mediaType == MediaMovie {
		return NewInitialDeclaration(InitialDeclarationInput{MediaType: mediaType, CanonicalTitle: title}), nil
	}
	if _, err := os.Lstat(path); err != nil {
		return Declaration{}, err
	}
	d := NewInitialDeclaration(InitialDeclarationInput{MediaType: mediaType, CanonicalTitle: title})
	d.Folders = cloneFolderProjections(folders)
	return d, nil
}

func cloneFolderProjections(in map[string]FolderProjection) map[string]FolderProjection {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]FolderProjection, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// AdoptStructuredSources adopts undeclared series that already satisfy the
// source namespace contract: SeriesRoot/<one season container>/<media>. The
// container name is deliberately irrelevant; season identity must be stated
// consistently by the media filenames. Adoption writes only the root
// declaration. It never renames or relocates source media.
func AdoptStructuredSources(root string) ([]string, error) {
	var adopted []string
	if root == "" || !filepath.IsAbs(root) {
		return adopted, nil
	}
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return adopted, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("source adoption root is not a real directory: %s", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		childInfo, err := os.Lstat(path)
		if err != nil {
			return adopted, err
		}
		if childInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if d, err := ReadDeclaration(path); err != nil {
			return adopted, fmt.Errorf("%s declaration: %w", path, err)
		} else if d != nil {
			continue
		}
		inferred, err := InferSeriesContainers(path)
		if err != nil {
			return adopted, err
		}
		if len(inferred) == 0 {
			continue
		}
		folders := map[string]FolderProjection{}
		for _, container := range inferred {
			folders[filepath.Base(container.Path)] = FolderProjection{Season: container.SuggestedSeason}
		}
		match := seriesRE.FindStringSubmatch(entry.Name())
		if len(match) == 0 || strings.TrimSpace(match[1]) == "" {
			continue
		}
		title := strings.TrimSpace(match[1])
		initial, err := DefaultDeclarationForLocalAdoption(path, title, folders, MediaSeries)
		if err != nil {
			return adopted, fmt.Errorf("analyze %s for source adoption: %w", path, err)
		}
		initial.Year, _ = strconv.Atoi(match[2])
		if err := createDeclaration(path, initial); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return adopted, fmt.Errorf("adopt structured source %s: %w", path, err)
		}
		key, err := Key(MediaSeries, entry.Name())
		if err != nil {
			return adopted, err
		}
		adopted = append(adopted, key)
	}
	sort.Strings(adopted)
	return adopted, nil
}

// AdoptStructuredMovies adopts every real first-level directory in the movie
// source namespace. Placement is sufficient ownership intent; bundle contents
// may be incomplete, auxiliary-only, or not recognizable yet. Adoption writes
// only the root declaration and never renames or moves source assets.
func AdoptStructuredMovies(root string) ([]string, error) {
	var adopted []string
	if root == "" || !filepath.IsAbs(root) {
		return adopted, nil
	}
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return adopted, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("movie source adoption root is not a real directory: %s", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		childInfo, err := os.Lstat(path)
		if err != nil {
			return adopted, err
		}
		if childInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if d, err := ReadDeclaration(path); err != nil {
			return adopted, fmt.Errorf("%s declaration: %w", path, err)
		} else if d != nil {
			continue
		}
		match := seriesRE.FindStringSubmatch(entry.Name())
		if len(match) == 0 || strings.TrimSpace(match[1]) == "" {
			continue
		}
		title := strings.TrimSpace(match[1])
		year, _ := strconv.Atoi(match[2])
		initial, err := DefaultDeclarationForLocalAdoption(path, title, nil, MediaMovie)
		if err != nil {
			return adopted, fmt.Errorf("initialize %s for movie source adoption: %w", path, err)
		}
		initial.Year = year
		if err := createDeclaration(path, initial); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return adopted, fmt.Errorf("adopt structured movie source %s: %w", path, err)
		}
		key, err := Key(MediaMovie, entry.Name())
		if err != nil {
			return adopted, err
		}
		adopted = append(adopted, key)
	}
	sort.Strings(adopted)
	return adopted, nil
}

// AdoptUndeclared explicitly adopts a canonical library tree. Ordinary source
// discovery performs the same declaration initialization automatically.
func AdoptUndeclared(root string) ([]string, error) {
	return AdoptUndeclaredAt(root, root)
}

func AdoptUndeclaredAt(libraryRoot, declarationRoot string) ([]string, error) {
	var adopted []string
	root := libraryRoot
	if root == "" || !filepath.IsAbs(root) {
		return adopted, nil
	}
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return adopted, nil
	}
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, fmt.Errorf("adoption root is not a real directory: %s", root)
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not a reliable adoption namespace entry: %s", path)
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root && strings.HasPrefix(entry.Name(), ".") {
			return fs.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var seasons []int
		for _, child := range entries {
			if child.IsDir() {
				if match := seasonRE.FindStringSubmatch(child.Name()); len(match) == 2 {
					season, _ := strconv.Atoi(match[1])
					seasons = append(seasons, season)
				}
			}
		}
		if len(seasons) == 0 {
			return nil
		}
		sort.Ints(seasons)
		relative, err := filepath.Rel(libraryRoot, path)
		if err != nil {
			return err
		}
		declarationPath := filepath.Join(declarationRoot, relative)
		if side, err := ReadDeclaration(declarationPath); err != nil {
			return fmt.Errorf("%s declaration: %w", path, err)
		} else if side != nil {
			return fs.SkipDir
		}
		if err := os.MkdirAll(declarationPath, 0o755); err != nil {
			return err
		}
		for _, child := range entries {
			if child.IsDir() && seasonRE.MatchString(child.Name()) {
				if err := os.MkdirAll(filepath.Join(declarationPath, child.Name()), 0o755); err != nil {
					return err
				}
			}
		}
		match := seriesRE.FindStringSubmatch(entry.Name())
		if len(match) == 0 || strings.TrimSpace(match[1]) == "" {
			return fs.SkipDir
		}
		folders := map[string]FolderProjection{}
		for _, season := range seasons {
			folders[fmt.Sprintf("Season %02d", season)] = FolderProjection{Season: season}
		}
		initial, err := DefaultDeclarationForLocalAdoption(path, strings.TrimSpace(match[1]), folders, MediaSeries)
		initial.Year, _ = strconv.Atoi(match[2])
		if err != nil {
			return fmt.Errorf("analyze %s for local adoption: %w", path, err)
		}
		if err := createDeclaration(declarationPath, initial); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fs.SkipDir
			}
			return fmt.Errorf("adopt declaration for %s: %w", path, err)
		}
		adopted = append(adopted, MediaSeries+"/"+entry.Name())
		return fs.SkipDir
	})
	return adopted, err
}

func ReadDeclaration(seriesPath string) (*Declaration, error) {
	data, err := os.ReadFile(filepath.Join(seriesPath, ".aninode.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Declaration
	if err := jsonfile.Decode(data, &s); err != nil {
		return nil, err
	}
	if err := s.Filters.Validate(); err != nil {
		return nil, err
	}
	s.Filters = s.Filters.Normalize()
	if err := ValidateSpecialMappings(s.Specials); err != nil {
		return nil, err
	}
	if err := ValidateOutput(s.Output); err != nil {
		return nil, err
	}
	if err := ValidateBlacklist(s.Blacklist); err != nil {
		return nil, err
	}
	if err := ValidateMovieDeclaration(s.Movie); err != nil {
		return nil, err
	}
	if err := ValidateFolderProjections(s.Folders); err != nil {
		return nil, err
	}
	return &s, nil
}

func createDeclaration(seriesPath string, s Declaration) error {
	if err := prepareDeclaration(&s); err != nil {
		return err
	}
	return jsonfile.Create(filepath.Join(seriesPath, ".aninode.json"), s)
}

func WriteDeclaration(seriesPath string, s Declaration) error {
	if err := prepareDeclaration(&s); err != nil {
		return err
	}
	if err := os.MkdirAll(seriesPath, 0o755); err != nil {
		return err
	}
	return jsonfile.Write(filepath.Join(seriesPath, ".aninode.json"), s)
}

// MoveDeclaration moves only aninode's declaration tree into the confirmed
// physical source namespace. Series season scopes move with the root declaration,
// but media files never do. Destination media directories may already exist;
// any destination declaration collision is a conflict.
func MoveDeclaration(e Entry, destination string) (Entry, error) {
	oldRoot := filepath.Clean(e.DeclarationPath)
	newRoot := filepath.Clean(destination)
	if oldRoot == newRoot {
		return e, nil
	}
	if oldRoot == "" || newRoot == "" {
		return e, errors.New("declaration roots are required")
	}
	oldPath := filepath.Join(oldRoot, ".aninode.json")
	newPath := filepath.Join(newRoot, ".aninode.json")
	if _, err := os.Lstat(oldPath); err != nil {
		return e, fmt.Errorf("root declaration is unavailable: %w", err)
	}
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return e, err
	}
	if _, err := os.Lstat(newPath); err == nil {
		return e, fmt.Errorf("destination already has a declaration: %s", newPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return e, err
	}
	if err := filesystem.RenameNoReplace(oldPath, newPath); err != nil {
		return e, err
	}
	// Remove the old placeholder only when it is empty. A real source tree may
	// contain media and must never be moved or deleted as declaration cleanup.
	_ = os.Remove(oldRoot)
	_ = filesystem.SyncDirectory(filepath.Dir(oldRoot))
	if err := filesystem.SyncDirectory(newRoot); err != nil {
		return e, err
	}
	e.DeclarationPath = newRoot
	if key, err := Key(e.MediaType, filepath.Base(newRoot)); err == nil {
		e.Key = key
	} else {
		return e, fmt.Errorf("derive moved declaration locator: %w", err)
	}
	return e, nil
}

func prepareDeclaration(s *Declaration) error {
	if err := ValidateDirectoryTitle(s.Title); err != nil {
		return err
	}
	if err := s.Filters.Validate(); err != nil {
		return err
	}
	s.Filters = s.Filters.Normalize()
	if err := ValidateSpecialMappings(s.Specials); err != nil {
		return err
	}
	// Declaration is passed by value at the public persistence boundary, but map
	// fields still share backing storage. Clone before normalization so writing a
	// declaration never mutates the caller's in-memory value.
	s.Movie.Classify = maps.Clone(s.Movie.Classify)
	for path, kind := range s.Movie.Classify {
		if strings.EqualFold(strings.TrimSpace(kind), "auto") {
			delete(s.Movie.Classify, path)
		}
	}
	if err := ValidateOutput(s.Output); err != nil {
		return err
	}
	if err := ValidateBlacklist(s.Blacklist); err != nil {
		return err
	}
	if err := ValidateMovieDeclaration(s.Movie); err != nil {
		return err
	}
	if err := ValidateFolderProjections(s.Folders); err != nil {
		return err
	}
	return nil
}

func ValidateSpecialMappings(values []SpecialMapping) error {
	seen := map[string]bool{}
	for i, value := range values {
		source := strings.TrimSpace(value.Source)
		if source == "" || source == "." || source == ".." || filepath.IsAbs(source) {
			return fmt.Errorf("specials[%d].source must be a source-relative path", i)
		}
		clean := filepath.ToSlash(filepath.Clean(source))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("specials[%d].source must stay inside the entry", i)
		}
		if value.Season < 0 || value.Season > 99 || value.Episode <= 0 {
			return fmt.Errorf("specials[%d] requires season 0-99 and a positive episode", i)
		}
		key := strings.ToLower(clean)
		if seen[key] {
			return fmt.Errorf("duplicate specials source %q", value.Source)
		}
		seen[key] = true
	}
	return nil
}

// ValidateDirectoryTitle rejects semantic titles that cannot safely name one
// direct child of a managed namespace. Titles are used to derive source and
// publication directories, so separators must never become path traversal.
func ValidateDirectoryTitle(title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is required")
	}
	if title == "." || title == ".." || filepath.IsAbs(title) || filepath.Base(title) != title || strings.ContainsAny(title, "\x00\r\n\t") {
		return errors.New("title must be a safe directory title")
	}
	return nil
}

func ValidateFolderProjections(v map[string]FolderProjection) error {
	for name, rule := range v {
		if strings.TrimSpace(name) == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return fmt.Errorf("folders[%q] must be a direct child directory name", name)
		}
		if rule.Season < 0 || rule.Season > 99 {
			return fmt.Errorf("folders[%q].season must be 0-99", name)
		}
	}
	return nil
}

func TitleYearFromDirName(name string) (string, int) {
	m := seriesRE.FindStringSubmatch(strings.TrimSpace(name))
	if len(m) == 0 {
		return strings.TrimSpace(name), 0
	}
	title := strings.TrimSpace(m[1])
	year, _ := strconv.Atoi(m[2])
	return title, year
}

func ValidateMovieDeclaration(v MovieDeclaration) error {
	allowed := map[string]bool{"version": true, "extra": true, "trailer": true, "interview": true, "featurette": true, "deleted_scene": true, "behind_the_scenes": true, "exclude": true, "auto": true}
	for path, kind := range v.Classify {
		clean := filepath.ToSlash(filepath.Clean(path))
		if path == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(path) {
			return fmt.Errorf("movie.classify path %q must be source-relative", path)
		}
		if !allowed[strings.ToLower(strings.TrimSpace(kind))] {
			return fmt.Errorf("movie.classify[%q] has unsupported value %q", path, kind)
		}
	}
	return nil
}

func OutputTitle(w Entry) string {
	if title := strings.TrimSpace(w.Output.Title); title != "" {
		return title
	}
	return w.Title
}

func TargetPathForEntry(libraryRoot string, w Entry) string {
	return filepath.Join(libraryRoot, SeriesDirName(OutputTitle(w), w.Year))
}

func ValidateOutput(v OutputLayout) error {
	if title := strings.TrimSpace(v.Title); title != "" {
		if title == "." || title == ".." || filepath.Base(title) != title || strings.ContainsAny(title, "<>:\"\\|?*\r\n\t") {
			return errors.New("output.title must be a safe directory title")
		}
	}
	return nil
}

func ValidateBlacklist(v []string) error {
	for i, pattern := range v {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("blacklist[%d] is empty", i)
		}
		if _, err := filepath.Match(strings.ToLower(pattern), "probe"); err != nil {
			return fmt.Errorf("blacklist[%d]: %w", i, err)
		}
	}
	return nil
}

func BlacklistMatch(patterns []string, basename, relative string) (string, bool) {
	values := []string{strings.ToLower(filepath.Base(basename))}
	if relative != "" {
		values = append(values, strings.ToLower(filepath.ToSlash(relative)))
	}
	for _, pattern := range patterns {
		for _, value := range values {
			if ok, _ := filepath.Match(strings.ToLower(pattern), value); ok {
				return pattern, true
			}
		}
	}
	return "", false
}

func SeriesDirName(title string, year int) string {
	name := strings.TrimSpace(title)
	if year > 0 {
		name += fmt.Sprintf(" (%04d)", year)
	}
	return name
}

func CreateManaged(root, title string, year, season int, side Declaration) (Entry, error) {
	return CreateManagedAt(root, root, title, year, season, side)
}

func CreateManagedAt(declarationRoot, libraryRoot, title string, year, season int, side Declaration) (Entry, error) {
	if err := ValidateDirectoryTitle(title); err != nil {
		return Entry{}, err
	}
	if year < 0 {
		return Entry{}, errors.New("year must not be negative")
	}
	if season < 0 || season > 99 {
		return Entry{}, errors.New("season must be 0-99")
	}
	name := SeriesDirName(title, year)
	series := filepath.Join(declarationRoot, name)
	side.Title, side.Year = strings.TrimSpace(title), year
	if err := os.MkdirAll(filepath.Join(series, fmt.Sprintf("Season %02d", season)), 0o755); err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Join(libraryRoot, name, fmt.Sprintf("Season %02d", season)), 0o755); err != nil {
		return Entry{}, err
	}
	if err := WriteDeclaration(series, side); err != nil {
		return Entry{}, err
	}
	entries, err := DiscoverAt(declarationRoot, libraryRoot)
	if err != nil {
		return Entry{}, err
	}
	key := MediaSeries + "/" + name
	item, ok := entries[key]
	if !ok {
		return Entry{}, fmt.Errorf("created series %q was not loaded", name)
	}
	return item, nil
}

func CreateManagedMovie(root, title string, year int, side Declaration) (Entry, error) {
	return CreateManagedMovieAt(root, root, title, year, side)
}

func CreateManagedMovieAt(declarationRoot, libraryRoot, title string, year int, side Declaration) (Entry, error) {
	if err := ValidateDirectoryTitle(title); err != nil {
		return Entry{}, err
	}
	if year < 0 {
		return Entry{}, errors.New("year must not be negative")
	}
	name := SeriesDirName(title, year)
	path := filepath.Join(declarationRoot, name)
	side.Title, side.Year = strings.TrimSpace(title), year
	if err := os.MkdirAll(path, 0o755); err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Join(libraryRoot, name), 0o755); err != nil {
		return Entry{}, err
	}
	if err := WriteDeclaration(path, side); err != nil {
		return Entry{}, err
	}
	entries, err := DiscoverMoviesAt(declarationRoot, libraryRoot)
	if err != nil {
		return Entry{}, err
	}
	key := MediaMovie + "/" + name
	item, ok := entries[key]
	if !ok {
		return Entry{}, fmt.Errorf("created movie %q was not loaded", name)
	}
	return item, nil
}

func EnsureSeason(w Entry, season int) (bool, error) {
	if season < 0 || season > 99 {
		return false, errors.New("season must be 0-99")
	}
	declaration := w.DeclarationPath
	if declaration == "" {
		declaration = w.Path
	}
	p := filepath.Join(declaration, fmt.Sprintf("Season %02d", season))
	if info, err := os.Lstat(p); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("season namespace is not a real directory: %s", p)
		}
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if !w.Enabled {
		return false, fmt.Errorf("%q has automation disabled", w.Title)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return false, err
	}
	return true, os.MkdirAll(filepath.Join(w.Path, fmt.Sprintf("Season %02d", season)), 0o755)
}

type FolderProjectionEvidence struct {
	Folder              string
	TargetSeason        int
	EpisodeOffset       int
	SourceSeason        int
	SourceSeasonKnown   bool
	MinEpisode          int
	MaxEpisode          int
	EpisodeCount        int
	ObservedGroups      []string
	ObservedResolutions []string
	ObservedSubtitles   []string
	StableGroup         string
	StableGroupN        int
	StableResolution    string
	StableSubtitle      string
	StableSource        string
	StableSourceKind    string
	StableVideoCodec    string
	StableBitDepth      string
	StableAudioCodec    string
}

// ObserveFolderProjectionEvidence derives ephemeral routing evidence from the
// real source namespace. Folder projections are durable user intent, while
// episode ranges and release/technical traits are rediscoverable filename
// facts. This is deliberately not persisted: changing the files changes the
// evidence.
func ObserveFolderProjectionEvidence(w Entry, extensions ...[]string) []FolderProjectionEvidence {
	var configured []string
	if len(extensions) > 0 {
		configured = extensions[0]
	}
	return observeFolderProjectionEvidence(w, configured, nil)
}

// ObserveFolderProjectionEvidenceFromSnapshot derives the same transient routing
// evidence from an already observed Entry source snapshot. Operation paths use
// this form so candidate planning and publication do not silently rescan disk.
func ObserveFolderProjectionEvidenceFromSnapshot(w Entry, snapshot filesystem.Snapshot, extensions []string) []FolderProjectionEvidence {
	return observeFolderProjectionEvidence(w, extensions, &snapshot)
}

func observeFolderProjectionEvidence(w Entry, extensions []string, snapshot *filesystem.Snapshot) []FolderProjectionEvidence {
	if w.MediaType != MediaSeries || w.DeclarationPath == "" || len(w.FolderProjections) == 0 {
		return nil
	}
	names := make([]string, 0, len(w.FolderProjections))
	for name := range w.FolderProjections {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]FolderProjectionEvidence, 0, len(names))
	for _, name := range names {
		rule := w.FolderProjections[name]
		ev := FolderProjectionEvidence{Folder: name, TargetSeason: rule.Season, EpisodeOffset: rule.EpisodeOffset}
		root := filepath.Join(w.DeclarationPath, name)
		extset := mediafile.ExtensionSet(extensions)
		if len(extset) == 0 {
			extset = mediafile.ExtensionSet([]string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "webm"})
		}
		var mediaNames []string
		if snapshot != nil {
			for path := range snapshot.Paths {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					continue
				}
				if mediafile.HasAllowedExtension(path, extset) {
					mediaNames = append(mediaNames, filepath.Base(path))
				}
			}
		} else {
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if path == root {
					return nil
				}
				if entry.Type()&os.ModeSymlink != 0 {
					if entry.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if entry.IsDir() {
					if strings.HasPrefix(entry.Name(), ".") {
						return fs.SkipDir
					}
					return nil
				}
				if mediafile.HasAllowedExtension(path, extset) {
					mediaNames = append(mediaNames, entry.Name())
				}
				return nil
			})
			if err != nil {
				out = append(out, ev)
				continue
			}
		}
		sort.Strings(mediaNames)
		if len(mediaNames) == 0 {
			out = append(out, ev)
			continue
		}
		batch := medianame.AnalyzeFiles(mediaNames)
		seasonCounts := map[int]int{}
		groupCounts := map[string]int{}
		groupDisplay := map[string]string{}
		resolutionCounts := map[string]int{}
		resolutionDisplay := map[string]string{}
		subtitleCounts := map[string]int{}
		subtitleDisplay := map[string]string{}
		sourceCounts := map[string]int{}
		sourceDisplay := map[string]string{}
		sourceKindCounts := map[string]int{}
		sourceKindDisplay := map[string]string{}
		videoCodecCounts := map[string]int{}
		videoCodecDisplay := map[string]string{}
		bitDepthCounts := map[string]int{}
		bitDepthDisplay := map[string]string{}
		audioCodecCounts := map[string]int{}
		audioCodecDisplay := map[string]string{}
		for _, item := range batch.Items {
			c := item.Components
			if c.EpisodeStart <= 0 {
				continue
			}
			end := c.EpisodeEnd
			if end < c.EpisodeStart {
				end = c.EpisodeStart
			}
			if ev.MinEpisode == 0 || c.EpisodeStart < ev.MinEpisode {
				ev.MinEpisode = c.EpisodeStart
			}
			if end > ev.MaxEpisode {
				ev.MaxEpisode = end
			}
			ev.EpisodeCount += end - c.EpisodeStart + 1
			if c.SeasonExplicit || c.Season > 0 {
				seasonCounts[c.Season]++
			}
			if g := strings.TrimSpace(c.ReleaseGroup); g != "" {
				key := Comparable(g)
				groupCounts[key]++
				if _, ok := groupDisplay[key]; !ok {
					groupDisplay[key] = g
				}
			}
			if value := strings.TrimSpace(c.Resolution); value != "" {
				key := Comparable(value)
				resolutionCounts[key]++
				resolutionDisplay[key] = value
			}
			if value := strings.TrimSpace(c.Subtitle); value != "" {
				key := Comparable(value)
				subtitleCounts[key]++
				subtitleDisplay[key] = value
			}
			technical := medianame.TechnicalTraitsOf(item.Input)
			recordTechnical := func(value string, counts map[string]int, display map[string]string) {
				value = strings.TrimSpace(value)
				if value == "" {
					return
				}
				key := Comparable(value)
				counts[key]++
				display[key] = value
			}
			recordTechnical(technical.Source, sourceCounts, sourceDisplay)
			recordTechnical(technical.SourceKind, sourceKindCounts, sourceKindDisplay)
			recordTechnical(technical.VideoCodec, videoCodecCounts, videoCodecDisplay)
			recordTechnical(technical.BitDepth, bitDepthCounts, bitDepthDisplay)
			recordTechnical(technical.AudioCodec, audioCodecCounts, audioCodecDisplay)
		}
		collectObserved := func(display map[string]string) []string {
			values := make([]string, 0, len(display))
			for _, value := range display {
				values = append(values, value)
			}
			sort.Slice(values, func(i, j int) bool { return Comparable(values[i]) < Comparable(values[j]) })
			return values
		}
		ev.ObservedGroups = collectObserved(groupDisplay)
		ev.ObservedResolutions = collectObserved(resolutionDisplay)
		ev.ObservedSubtitles = collectObserved(subtitleDisplay)
		if len(seasonCounts) == 1 {
			for season := range seasonCounts {
				ev.SourceSeason = season
				ev.SourceSeasonKnown = true
			}
		}
		// Continuity is only considered stable when at least two parseable
		// episodes agree on one non-empty group. A single historical file is too
		// weak to silently constrain future acquisitions.
		if ev.EpisodeCount >= 2 && len(groupCounts) == 1 {
			for key, n := range groupCounts {
				ev.StableGroup, ev.StableGroupN = groupDisplay[key], n
			}
		}
		if ev.EpisodeCount >= 2 && len(resolutionCounts) == 1 {
			for key, n := range resolutionCounts {
				if n >= 2 {
					ev.StableResolution = resolutionDisplay[key]
				}
			}
		}
		if ev.EpisodeCount >= 2 && len(subtitleCounts) == 1 {
			for key, n := range subtitleCounts {
				if n >= 2 {
					ev.StableSubtitle = subtitleDisplay[key]
				}
			}
		}
		stableTechnical := func(counts map[string]int, display map[string]string) string {
			if ev.EpisodeCount < 2 || len(counts) != 1 {
				return ""
			}
			for key, n := range counts {
				if n >= 2 {
					return display[key]
				}
			}
			return ""
		}
		ev.StableSource = stableTechnical(sourceCounts, sourceDisplay)
		ev.StableSourceKind = stableTechnical(sourceKindCounts, sourceKindDisplay)
		ev.StableVideoCodec = stableTechnical(videoCodecCounts, videoCodecDisplay)
		ev.StableBitDepth = stableTechnical(bitDepthCounts, bitDepthDisplay)
		ev.StableAudioCodec = stableTechnical(audioCodecCounts, audioCodecDisplay)
		out = append(out, ev)
	}
	return out
}

// ProjectObservedEpisode applies explicit episode projection first, then the folder
// projection selected by real source filename evidence. The latter is needed
// for seasonless/absolute-numbered releases: a remote RSS item has no source
// folder path, but existing files can unambiguously show which declared folder
// continues that episode sequence.
func ProjectObservedEpisode(w Entry, evidence []FolderProjectionEvidence, parsedSeason int, seasonExplicit bool, episode int) (targetSeason, targetEpisode int, folder string, ok bool) {
	if episode <= 0 {
		return 0, 0, "", false
	}
	if parsedSeason > 0 || seasonExplicit {
		var matches []FolderProjectionEvidence
		for _, ev := range evidence {
			if ev.SourceSeasonKnown && ev.SourceSeason == parsedSeason {
				matches = append(matches, ev)
			}
		}
		if len(matches) == 1 {
			ev := matches[0]
			return ev.TargetSeason, episode + ev.EpisodeOffset, ev.Folder, true
		}

		// An explicit season in the release is authoritative unless projection or
		// evidence for that same source season says otherwise. Do not let the
		// generic seasonless continuity heuristics below redirect S02E01 into an
		// unrelated sole Season 01 folder merely because it is the only folder
		// currently present.
		return parsedSeason, episode, "", true
	}

	// Exact/range evidence wins for seasonless releases.
	var containing []FolderProjectionEvidence
	for _, ev := range evidence {
		if ev.MinEpisode > 0 && episode >= ev.MinEpisode && episode <= ev.MaxEpisode {
			containing = append(containing, ev)
		}
	}
	if len(containing) == 1 {
		ev := containing[0]
		return ev.TargetSeason, episode + ev.EpisodeOffset, ev.Folder, true
	}
	// For a new episode immediately beyond existing content, continue the folder
	// with the nearest preceding observed episode range. This is deterministic for
	// absolute-numbered series such as 41,42,43 -> 45 and does not guess when two
	// folders have the same inferred boundary.
	best := -1
	var chosen *FolderProjectionEvidence
	ambiguous := false
	for i := range evidence {
		ev := &evidence[i]
		if ev.MaxEpisode <= 0 || ev.MaxEpisode >= episode {
			continue
		}
		if ev.MaxEpisode > best {
			best, chosen, ambiguous = ev.MaxEpisode, ev, false
		} else if ev.MaxEpisode == best {
			ambiguous = true
		}
	}
	if chosen != nil && !ambiguous {
		return chosen.TargetSeason, episode + chosen.EpisodeOffset, chosen.Folder, true
	}
	// With a single declared source folder there is no ambiguity even before the
	// first file arrives.
	if len(evidence) == 1 {
		ev := evidence[0]
		return ev.TargetSeason, episode + ev.EpisodeOffset, ev.Folder, true
	}
	if parsedSeason > 0 || seasonExplicit {
		return parsedSeason, episode, "", true
	}
	if len(w.Seasons) == 1 {
		return w.Seasons[0], episode, "", true
	}
	return 0, 0, "", false
}

func StableObservedGroup(evidence []FolderProjectionEvidence, targetSeason int) (string, bool) {
	var group string
	count := 0
	for _, ev := range evidence {
		if ev.TargetSeason != targetSeason || ev.StableGroup == "" {
			continue
		}
		if group == "" {
			group = ev.StableGroup
		} else if Comparable(group) != Comparable(ev.StableGroup) {
			return "", false
		}
		count += ev.StableGroupN
	}
	return group, count >= 2
}

func StableObservedGroupAny(evidence []FolderProjectionEvidence) (string, bool) {
	var group string
	count := 0
	for _, ev := range evidence {
		if ev.StableGroup == "" {
			continue
		}
		if group == "" {
			group = ev.StableGroup
		} else if Comparable(group) != Comparable(ev.StableGroup) {
			return "", false
		}
		count += ev.StableGroupN
	}
	return group, count >= 2
}

// ObservedReleaseTraits are stable, rediscoverable traits from managed files.
// Empty fields mean the filesystem did not prove one value consistently.
type ObservedReleaseTraits struct {
	Resolution string
	Subtitle   string
	Source     string
	SourceKind string
	VideoCodec string
	BitDepth   string
	AudioCodec string
}

// FilterOptionsFromEvidence exposes every release attribute observed in managed
// media. Unlike the stable-trait helpers below, UI option discovery is intentionally
// high-recall: one observed value is enough to make it selectable.
func FilterOptionsFromEvidence(evidence []FolderProjectionEvidence) releasefilter.Options {
	var out releasefilter.Options
	for _, value := range evidence {
		out = out.Merge(releasefilter.NewOptions(value.ObservedGroups, value.ObservedResolutions, value.ObservedSubtitles))
	}
	return out
}

// StableObservedReleaseTraits returns continuity evidence for fields that
// normally remain stable between adjacent episodes. Conflicts clear only the
// affected field so one changing trait does not erase unrelated evidence.
func StableObservedReleaseTraits(evidence []FolderProjectionEvidence, targetSeason int) ObservedReleaseTraits {
	collect := func(anySeason bool) (ObservedReleaseTraits, bool) {
		var out ObservedReleaseTraits
		conflict := map[string]bool{}
		found := false
		merge := func(name string, dst *string, value string) {
			value = strings.TrimSpace(value)
			if value == "" || conflict[name] {
				return
			}
			found = true
			if *dst == "" {
				*dst = value
				return
			}
			if Comparable(*dst) != Comparable(value) {
				*dst = ""
				conflict[name] = true
			}
		}
		for _, ev := range evidence {
			if !anySeason && ev.TargetSeason != targetSeason {
				continue
			}
			merge("resolution", &out.Resolution, ev.StableResolution)
			merge("subtitle", &out.Subtitle, ev.StableSubtitle)
			merge("source", &out.Source, ev.StableSource)
			merge("source_kind", &out.SourceKind, ev.StableSourceKind)
			merge("video_codec", &out.VideoCodec, ev.StableVideoCodec)
			merge("bit_depth", &out.BitDepth, ev.StableBitDepth)
			merge("audio_codec", &out.AudioCodec, ev.StableAudioCodec)
		}
		return out, found
	}
	if traits, ok := collect(false); ok {
		return traits
	}
	traits, _ := collect(true)
	return traits
}

func SourceSeasons(w Entry) []int {
	seasons := append([]int(nil), w.Seasons...)
	sort.Ints(seasons)
	return seasons
}

func EffectiveSources(w Entry, enabled []string) []string {
	if len(w.Sources) > 0 {
		return append([]string(nil), w.Sources...)
	}
	return append([]string(nil), enabled...)
}
func Comparable(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			r += 32
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			return r
		}
		return -1
	}, v)
}

// SyncFolderProjections keeps a series declaration keyed by the real current
// first-level source folders. Missing rules are filled from best-effort
// inference; stale rules whose folder no longer exists are removed. This is a
// declaration projection only and never mutates source media or directories.
func SyncFolderProjections(seriesRoot string) ([]string, error) {
	if strings.TrimSpace(seriesRoot) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(seriesRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		root := filepath.Join(seriesRoot, ent.Name())
		d, err := ReadDeclaration(root)
		if err != nil {
			return changed, err
		}
		if d == nil {
			continue
		}
		inferred, err := InferSeriesContainers(root)
		if err != nil {
			return changed, err
		}
		inferredByName := map[string]int{}
		for _, c := range inferred {
			inferredByName[filepath.Base(c.Path)] = c.SuggestedSeason
		}
		dirs, err := os.ReadDir(root)
		if err != nil {
			return changed, err
		}
		existing := map[string]bool{}
		for _, child := range dirs {
			if !child.IsDir() || strings.HasPrefix(child.Name(), ".") {
				continue
			}
			info, e := os.Lstat(filepath.Join(root, child.Name()))
			if e != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			existing[child.Name()] = true
		}
		next := cloneFolderProjections(d.Folders)
		if next == nil {
			next = map[string]FolderProjection{}
		}
		dirty := false
		for name := range next {
			if !existing[name] {
				delete(next, name)
				dirty = true
			}
		}
		for name, season := range inferredByName {
			if _, ok := next[name]; !ok {
				next[name] = FolderProjection{Season: season}
				dirty = true
			}
		}
		if !dirty {
			continue
		}
		d.Folders = next
		if err := WriteDeclaration(root, *d); err != nil {
			return changed, err
		}
		key, _ := Key(MediaSeries, ent.Name())
		changed = append(changed, key)
	}
	sort.Strings(changed)
	return changed, nil
}
