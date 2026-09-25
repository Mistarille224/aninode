package configstore

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aninode/internal/catalog"
	"aninode/internal/jsonfile"
	"aninode/internal/organizer"
)

type Bundle struct {
	Organizer organizer.Config
	Clients   map[string]Client
	Sources   map[string]ContentSource
	Entries   map[string]catalog.Entry
}
type Client struct {
	CredentialSet bool          `json:"credential_set,omitempty"`
	ID            string        `json:"id"`
	Type          string        `json:"type"`
	URL           string        `json:"url"`
	Username      string        `json:"username,omitempty"`
	PathMappings  []PathMapping `json:"path_mappings,omitempty"`
	Enabled       bool          `json:"enabled"`
}
type PathMapping struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}
type RSSSource struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url"`
}

type SearchSource struct {
	// URLTemplate is only used by generic providers. Built-in providers own
	// their endpoint/query construction and leave this empty.
	URLTemplate string `json:"url_template,omitempty"`
}

type ContentSource struct {
	ID       string        `json:"id"`
	Provider string        `json:"provider"`
	RSS      []RSSSource   `json:"rss,omitempty"`
	Search   *SearchSource `json:"search,omitempty"`
	Enabled  bool          `json:"enabled"`
	Priority int           `json:"priority,omitempty"`
}

func LoadOrganizer(root string) (organizer.Config, error) {
	var cfg organizer.Config
	if root == "" {
		return cfg, errors.New("configuration directory is required")
	}
	if err := decodeFile(filepath.Join(root, "organizer.json"), &cfg); err != nil {
		return cfg, fmt.Errorf("load organizer config: %w", err)
	}
	if err := organizer.ValidateConfig(cfg); err != nil {
		return cfg, fmt.Errorf("validate organizer config: %w", err)
	}
	return cfg, nil
}

func Load(root string) (Bundle, error) {
	b := Bundle{Clients: map[string]Client{}, Sources: map[string]ContentSource{}, Entries: map[string]catalog.Entry{}}
	cfg, err := LoadOrganizer(root)
	if err != nil {
		return b, err
	}
	b.Organizer = cfg
	if err := loadDirectory(filepath.Join(root, "clients"), func(id string, data []byte) error {
		var v Client
		if err := decode(data, &v); err != nil {
			return err
		}
		if err := validateID(id, v.ID); err != nil {
			return err
		}
		if v.Type != "qbittorrent" && v.Type != "transmission" && v.Type != "aria2" {
			return fmt.Errorf("client %q has unsupported type %q", id, v.Type)
		}
		if v.URL == "" {
			return fmt.Errorf("client %q URL is required", id)
		}
		if err := validateClientURL(v.URL); err != nil {
			return fmt.Errorf("client %q URL: %w", id, err)
		}
		if v.CredentialSet {
			return fmt.Errorf("client %q credential_set is runtime-only and must not be persisted", id)
		}
		for _, m := range v.PathMappings {
			if m.Remote == "" || m.Local == "" {
				return fmt.Errorf("client %q path mappings require remote and local", id)
			}
		}
		b.Clients[id] = v
		return nil
	}); err != nil {
		return b, fmt.Errorf("load clients: %w", err)
	}
	loadSources := func(dir string) error {
		return loadDirectory(filepath.Join(root, dir), func(id string, data []byte) error {
			var v ContentSource
			if err := decode(data, &v); err != nil {
				return err
			}
			if err := validateID(id, v.ID); err != nil {
				return err
			}
			if v.Provider == "" {
				return fmt.Errorf("content source %q provider is required", id)
			}
			if v.Provider != "generic" && v.Provider != "mikan" && v.Provider != "dmhy" && v.Provider != "nyaa" {
				return fmt.Errorf("content source %q has unsupported provider %q", id, v.Provider)
			}
			if v.Provider != "generic" {
				if id != v.Provider {
					return fmt.Errorf("built-in content source %q must use canonical id %q", id, v.Provider)
				}
				if v.Search != nil {
					return fmt.Errorf("built-in content source %q owns its search endpoint", id)
				}
			} else if id == "dmhy" || id == "nyaa" || id == "mikan" {
				return fmt.Errorf("generic content source %q uses a reserved built-in id", id)
			}
			seenRSS := make(map[string]struct{}, len(v.RSS))
			for i, feed := range v.RSS {
				feedURL := strings.TrimSpace(feed.URL)
				if feedURL == "" {
					return fmt.Errorf("content source %q RSS[%d] URL is required", id, i)
				}
				if err := validateHTTPURL(feedURL); err != nil {
					return fmt.Errorf("content source %q RSS[%d] URL: %w", id, i, err)
				}
				if _, exists := seenRSS[feedURL]; exists {
					return fmt.Errorf("content source %q has duplicate RSS URL %q", id, feedURL)
				}
				seenRSS[feedURL] = struct{}{}
			}
			if v.Provider == "generic" && len(v.RSS) == 0 && (v.Search == nil || v.Search.URLTemplate == "") {
				return fmt.Errorf("content source %q generic provider requires RSS or search.url_template", id)
			}
			if v.Provider == "generic" && v.Search != nil && v.Search.URLTemplate != "" {
				if strings.Count(v.Search.URLTemplate, "{query}") != 1 {
					return fmt.Errorf("content source %q search.url_template must contain exactly one {query}", id)
				}
				if err := validateHTTPURL(strings.Replace(v.Search.URLTemplate, "{query}", "probe", 1)); err != nil {
					return fmt.Errorf("content source %q search.url_template: %w", id, err)
				}
			}
			b.Sources[id] = v
			return nil
		})
	}
	if err := loadSources("sources"); err != nil {
		return b, fmt.Errorf("load content sources: %w", err)
	}
	entries, err := catalog.DiscoverAt(b.Organizer.SeriesSource(), b.Organizer.SeriesTarget())
	if err != nil {
		return b, fmt.Errorf("discover media library: %w", err)
	}
	movies, err := catalog.DiscoverMoviesAt(b.Organizer.MovieSource(), b.Organizer.MovieTarget())
	if err != nil {
		return b, fmt.Errorf("discover movies: %w", err)
	}
	for id, value := range movies {
		if _, exists := entries[id]; exists {
			return b, fmt.Errorf("duplicate Entry ID %q", id)
		}
		entries[id] = value
	}
	b.Entries = entries
	if err := validateReferences(b); err != nil {
		return b, err
	}
	return b, nil
}

// OrganizerForPreview builds canonical publication naming without claiming that
// its provisional Source identifies downloader ownership. Runtime publication
// must use OrganizerForPublication with an observed source root.
func (b Bundle) OrganizerForPreview(id string, season int) (organizer.Config, error) {
	w, ok := b.Entries[id]
	if !ok {
		return organizer.Config{}, fmt.Errorf("entry %q not found", id)
	}
	observed := w.DeclarationPath
	if observed == "" {
		if w.MediaType == catalog.MediaMovie {
			observed = filepath.Join(b.Organizer.MovieSource(), catalog.SeriesDirName(w.Title, w.Year))
		} else {
			observed = filepath.Join(b.Organizer.SeriesSource(), catalog.SeriesDirName(w.Title, w.Year), fmt.Sprintf("Season %02d", season))
		}
	} else if w.MediaType != catalog.MediaMovie {
		observed = filepath.Join(observed, fmt.Sprintf("Season %02d", season))
	}
	return b.organizerForWork(id, season, observed)
}

// OrganizerForPublication combines Entry-controlled canonical output semantics
// with an already claimed downloader source. The source argument is an
// observation, never a path inferred from publication naming.
func (b Bundle) OrganizerForPublication(id string, sourceSeason, targetSeason int, observedSource string) (organizer.Config, error) {
	if strings.TrimSpace(observedSource) == "" {
		return organizer.Config{}, errors.New("observed publication source is required")
	}
	observedSource = filepath.Clean(observedSource)
	w, ok := b.Entries[id]
	if !ok {
		return organizer.Config{}, fmt.Errorf("entry %q not found", id)
	}
	// A downloader task may be seasonless even though its physical content is
	// already inside a declared first-level source folder. In that case the
	// folder projection is the authoritative publication rule. Reuse it here
	// instead of dropping the target season (and, importantly, its episode
	// offset) after claim resolution.
	if w.MediaType == catalog.MediaSeries {
		if rule, ok := folderProjectionForObservedSource(w, observedSource); ok && rule.Season == targetSeason {
			return b.OrganizerForFolderPublication(id, sourceSeason, rule.Season, rule.EpisodeOffset, observedSource)
		}
	}
	return b.organizerForWork(id, sourceSeason, observedSource)
}

func folderProjectionForObservedSource(w catalog.Entry, observedSource string) (catalog.FolderProjection, bool) {
	if strings.TrimSpace(w.DeclarationPath) == "" || len(w.FolderProjections) == 0 {
		return catalog.FolderProjection{}, false
	}
	rel, err := filepath.Rel(filepath.Clean(w.DeclarationPath), filepath.Clean(observedSource))
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return catalog.FolderProjection{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return catalog.FolderProjection{}, false
	}
	rule, ok := w.FolderProjections[parts[0]]
	return rule, ok
}

// OrganizerForFolderPublication projects one observed series source folder directly
// into its declared target season. The real current folder name is the declaration
// key; suggestedSeason is leaf-basename parser context or a deterministic fallback only; directory names never create media identity and are not persisted as source-season identity.
func (b Bundle) OrganizerForFolderPublication(id string, suggestedSeason, targetSeason, episodeOffset int, observedSource string) (organizer.Config, error) {
	if strings.TrimSpace(observedSource) == "" {
		return organizer.Config{}, errors.New("observed publication source is required")
	}
	w, ok := b.Entries[id]
	if !ok {
		return organizer.Config{}, fmt.Errorf("entry %q not found", id)
	}
	if w.MediaType != catalog.MediaSeries {
		return organizer.Config{}, errors.New("folder publication applies only to series")
	}
	cfg := b.Organizer
	cfg.Managed = w.Enabled
	cfg.PublicationRoot = cfg.SeriesTarget()
	cfg.Source = filepath.Clean(observedSource)
	cfg = organizer.WithEntryBlacklist(cfg, w.Blacklist)
	projection := &organizer.FolderProjection{TargetSeason: targetSeason, EpisodeOffset: episodeOffset}
	configured, err := organizer.WithEmbyLayout(cfg, organizer.EmbyLayout{LibraryRoot: filepath.Dir(w.Path), Title: catalog.OutputTitle(w), Year: w.Year, Season: suggestedSeason, Specials: organizerSpecials(w.Specials), FolderProjection: projection})
	if err != nil {
		return organizer.Config{}, err
	}
	return configured, organizer.ValidateRuntimeConfig(configured)
}

func (b Bundle) organizerForWork(id string, parseSeason int, observedSource string) (organizer.Config, error) {
	w, ok := b.Entries[id]
	if !ok {
		return organizer.Config{}, fmt.Errorf("entry %q not found", id)
	}
	if w.MediaType == catalog.MediaMovie {
		cfg := b.Organizer
		target := cfg.MovieTarget()
		cfg.Managed = w.Enabled
		cfg.PublicationRoot = target
		cfg.Source = observedSource
		cfg.Target = catalog.TargetPathForEntry(target, w)
		cfg = organizer.WithEntryBlacklist(cfg, w.Blacklist)
		return organizer.WithMovieLayout(cfg, organizer.MovieLayout{Title: catalog.OutputTitle(w), Year: w.Year, Overrides: w.Movie.Classify})
	}
	resolveSeason := func(season int) (int, error) {
		if season < 0 || season > 99 {
			return 0, fmt.Errorf("season %d is outside supported range 0-99", season)
		}
		return season, nil
	}
	var err error
	parseSeason, err = resolveSeason(parseSeason)
	if err != nil {
		return organizer.Config{}, err
	}
	cfg := b.Organizer
	cfg.Managed = w.Enabled
	cfg.PublicationRoot = cfg.SeriesTarget()
	cfg.Source = observedSource
	cfg = organizer.WithEntryBlacklist(cfg, w.Blacklist)
	configured, err := organizer.WithEmbyLayout(cfg, organizer.EmbyLayout{LibraryRoot: filepath.Dir(w.Path), Title: catalog.OutputTitle(w), Year: w.Year, Season: parseSeason, Specials: organizerSpecials(w.Specials)})
	if err != nil {
		return organizer.Config{}, err
	}
	return configured, organizer.ValidateRuntimeConfig(configured)
}
func EnabledSourceIDs(b Bundle) []string {
	ids := []string{}
	for id, v := range b.Sources {
		if v.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := b.Sources[ids[i]].Priority, b.Sources[ids[j]].Priority
		if pi == pj {
			return ids[i] < ids[j]
		}
		return pi < pj
	})
	return ids
}

// EffectiveClient returns the single enabled downloader. Configuration
// validation rejects multiple configured downloaders, so selection is never
// user-visible and Entries cannot override it.
func EffectiveClient(b Bundle) string {
	for id, client := range b.Clients {
		if client.Enabled {
			return id
		}
	}
	return ""
}

func validateReferences(b Bundle) error {
	if len(b.Clients) > 1 {
		return fmt.Errorf("only one downloader client may be configured")
	}
	for id, w := range b.Entries {
		for _, sourceID := range w.Sources {
			_, ok := b.Sources[sourceID]
			if !ok {
				return fmt.Errorf("entry %q references unknown content source %q", id, sourceID)
			}
		}
	}
	return nil
}
func organizerSpecials(v []catalog.SpecialMapping) []organizer.SpecialMapping {
	r := make([]organizer.SpecialMapping, 0, len(v))
	for _, s := range v {
		r = append(r, organizer.SpecialMapping{Source: s.Source, Season: s.Season, Episode: s.Episode})
	}
	return r
}

func decodeFile(path string, target any) error {
	d, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decode(d, target)
}
func decode(data []byte, target any) error {
	return jsonfile.Decode(data, target)
}
func loadDirectory(root string, load func(string, []byte) error) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		d, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			return err
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if err := load(id, d); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return nil
}
func validateID(fileID, contentID string) error {
	if contentID == "" {
		return errors.New("id is required")
	}
	if fileID != contentID {
		return fmt.Errorf("id %q must match filename %q", contentID, fileID+".json")
	}
	if strings.ContainsAny(contentID, `/\\`) || contentID == "." || contentID == ".." {
		return fmt.Errorf("invalid id %q", contentID)
	}
	return nil
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("URL scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("URL host is required")
	}
	return nil
}

func validateClientURL(raw string) error {
	if err := validateHTTPURL(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	if u.User != nil {
		return errors.New("URL must not contain embedded credentials; use the username and password fields")
	}
	return nil
}

// Public returns a management view with only secret presence, never secret material.
func (c Client) Public(credentialSet bool) Client {
	c.CredentialSet = credentialSet
	return c
}
