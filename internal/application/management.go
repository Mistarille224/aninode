package application

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aninode/internal/automation"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/completeness"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/episode"
	"aninode/internal/filesystem"
	"aninode/internal/inventory"
	"aninode/internal/jsonfile"
	"aninode/internal/medianame"
	"aninode/internal/migration"
	"aninode/internal/naming"
	"aninode/internal/observation"
	"aninode/internal/organizer"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
	"aninode/internal/rss"
	"aninode/internal/secretstore"
)

type ManagementErrorKind string

const (
	ErrorNotFound ManagementErrorKind = "not_found"
	ErrorInvalid  ManagementErrorKind = "invalid"
	ErrorConflict ManagementErrorKind = "conflict"
	ErrorUnknown  ManagementErrorKind = "unknown"
	ErrorInternal ManagementErrorKind = "internal"
)

type ManagementError struct {
	Kind ManagementErrorKind
	Err  error
}

func (e *ManagementError) Error() string { return e.Err.Error() }
func (e *ManagementError) Unwrap() error { return e.Err }
func managementErr(kind ManagementErrorKind, err error) error {
	return &ManagementError{Kind: kind, Err: err}
}

func rollbackFailure(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s failed: %w", action, err)
}

func (a *App) withOperation(ctx context.Context, fn func() error) error {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	lock, err := filesystem.AcquireWriterLock(a.configRoot)
	if err != nil {
		return managementErr(ErrorConflict, fmt.Errorf("acquire writer ownership: %w", err))
	}
	defer lock.Close()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return fn()
}

type EntryDeclaration struct {
	Title     string                              `json:"title"`
	Year      int                                 `json:"year,omitempty"`
	Enabled   *bool                               `json:"enabled,omitempty"`
	Sources   []string                            `json:"sources"`
	Filters   releasefilter.Filters               `json:"filters,omitempty"`
	Specials  []catalog.SpecialMapping            `json:"specials,omitempty"`
	Output    catalog.OutputLayout                `json:"output"`
	Blacklist []string                            `json:"blacklist"`
	Movie     catalog.MovieDeclaration            `json:"movie,omitempty"`
	Folders   map[string]catalog.FolderProjection `json:"folders,omitempty"`
}
type CreateEntryRequest struct {
	Season      *int             `json:"season,omitempty"`
	Declaration EntryDeclaration `json:"declaration"`
}

type EntrySummary struct {
	Status  string `json:"status"`
	Seasons int    `json:"seasons,omitempty"`
	Missing int    `json:"missing,omitempty"`
	Reviews int    `json:"reviews,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type EntryView struct {
	Entry             catalog.Entry           `json:"entry"`
	Declaration       EntryDeclaration        `json:"declaration"`
	FilterOptions     releasefilter.Options   `json:"filter_options"`
	Seasons           []SeasonManagementState `json:"seasons,omitempty"`
	Folders           []FolderManagementState `json:"folders,omitempty"`
	Summary           *EntrySummary           `json:"summary,omitempty"`
	ObservationIssues []string                `json:"observation_issues,omitempty"`
}
type SourceCapability struct {
	Current    bool `json:"current"`
	Historical bool `json:"historical"`
	Discovery  bool `json:"discovery"`
}
type ConfigView struct {
	Organizer          organizer.Config                     `json:"organizer"`
	Sources            map[string]configstore.ContentSource `json:"sources"`
	Clients            map[string]configstore.Client        `json:"clients"`
	SourceCapabilities map[string]SourceCapability          `json:"source_capabilities"`
}

type ClientStatus struct {
	ID        string `json:"id"`
	Enabled   bool   `json:"enabled"`
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type clientObserver interface {
	List(context.Context) ([]download.Task, error)
}

func (a *App) ClientStatuses(ctx context.Context) []ClientStatus {
	rt := a.snapshot()
	if rt == nil {
		return []ClientStatus{}
	}
	ids := make([]string, 0, len(rt.bundle.Clients))
	for id := range rt.bundle.Clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ClientStatus, len(ids))
	var probes sync.WaitGroup
	for i, id := range ids {
		cfg := rt.bundle.Clients[id]
		status := ClientStatus{ID: id, Enabled: cfg.Enabled}
		out[i] = status
		if !cfg.Enabled {
			continue
		}
		backend, ok := rt.backends[id]
		observer, observable := backend.(clientObserver)
		if !ok || !observable {
			out[i].Error = "backend does not support connectivity observation"
			continue
		}
		probes.Add(1)
		go func(index int, observer clientObserver) {
			defer probes.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			started := time.Now()
			_, err := observer.List(probeCtx)
			out[index].LatencyMS = time.Since(started).Milliseconds()
			if err != nil {
				out[index].Error = err.Error()
				return
			}
			out[index].Reachable = true
		}(i, observer)
	}
	probes.Wait()
	return out
}

func declarationOf(w catalog.Entry) EntryDeclaration {
	enabled := w.Enabled
	return EntryDeclaration{Title: w.Title, Year: w.Year, Enabled: &enabled, Sources: append([]string(nil), w.Sources...), Filters: w.Filters, Specials: append([]catalog.SpecialMapping(nil), w.Specials...), Output: w.Output, Blacklist: append([]string{}, w.Blacklist...), Movie: w.Movie, Folders: maps.Clone(w.FolderProjections)}
}
func viewOf(b configstore.Bundle, w catalog.Entry) EntryView {
	return EntryView{Entry: w, Declaration: declarationOf(w), FilterOptions: releasefilter.Options{}.WithFilters(w.Filters)}
}

func (a *App) entrySummariesObserved(ctx context.Context, b configstore.Bundle, ids []string, physical observation.Graph, observeErr error) map[string]EntrySummary {
	out := make(map[string]EntrySummary, len(ids))
	entries := map[string]catalog.Entry{}
	for _, id := range ids {
		if w, ok := b.Entries[id]; ok {
			entries[id] = w
			if !w.Enabled {
				out[id] = EntrySummary{Status: "disabled"}
			} else if w.MediaType == catalog.MediaMovie {
				out[id] = EntrySummary{Status: "managed"}
			} else {
				out[id] = EntrySummary{Status: "continuous"}
			}
		}
	}
	if len(entries) == 0 {
		return out
	}
	if observeErr != nil {
		for id, w := range entries {
			if w.Enabled && w.MediaType != catalog.MediaMovie {
				out[id] = EntrySummary{Status: "unknown", Detail: observeErr.Error()}
			}
		}
		return out
	}
	inv := inventory.ScanGraph(entries, physical, b.Organizer.Extensions)
	for id, w := range entries {
		select {
		case <-ctx.Done():
			return out
		default:
		}
		if !w.Enabled || w.MediaType == catalog.MediaMovie {
			continue
		}
		seasons := map[int]bool{}
		for _, sn := range w.Seasons {
			seasons[sn] = true
		}
		if ei, ok := inv.Entries[id]; ok {
			for sn := range ei.Seasons {
				seasons[sn] = true
			}
		}
		summary := EntrySummary{Status: "continuous", Seasons: len(seasons)}
		for sn := range seasons {
			si := inv.Season(id, sn)
			if !si.Known {
				if summary.Status != "conflict" && summary.Status != "incomplete" {
					summary.Status = "unknown"
				}
				if summary.Detail == "" && si.Err != nil {
					summary.Detail = si.Err.Error()
				}
				continue
			}
			missing, status := completeness.InternalGaps(inv, id, sn)
			summary.Missing += len(missing)
			if status == "conflict" {
				summary.Status = "conflict"
			} else if status == "incomplete" && summary.Status != "conflict" {
				summary.Status = "incomplete"
			}
		}
		a.repairMu.RLock()
		for _, review := range a.repairReviews {
			if review.EntryKey == id {
				summary.Reviews++
			}
		}
		a.repairMu.RUnlock()
		out[id] = summary
	}
	return out
}

func (a *App) currentBundle() (configstore.Bundle, error) {
	rt := a.snapshot()
	if rt == nil {
		return configstore.Bundle{}, errors.New("runtime unavailable")
	}
	return rt.bundle, nil
}

func (a *App) reconcileEntryPublication(ctx context.Context, rt *runtimeSnapshot, key string) error {
	physical, err := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
	if err != nil {
		return fmt.Errorf("observe filesystem graph after declaration update: %w", err)
	}
	return a.reconcileEntryPublicationObserved(ctx, rt, physical, key)
}

func (a *App) reconcileEntryPublicationObserved(ctx context.Context, rt *runtimeSnapshot, physical observation.Graph, key string) error {
	runner := automation.Runner{Bundle: rt.bundle, Clients: rt.backends, Now: a.now}
	_, err := runner.ReconcileLocalEntryWithGraph(ctx, physical, key)
	if err != nil {
		return fmt.Errorf("reconcile publication after declaration update: %w", err)
	}
	return nil
}

func (a *App) SearchEntries(ctx context.Context, query string, limit int) ([]EntryView, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	query = strings.ToLower(strings.TrimSpace(query))
	emptyQuery := query == ""
	if limit <= 0 || limit > 50 {
		limit = 30
	}
	b, err := a.currentBundle()
	if err != nil {
		return nil, err
	}
	if emptyQuery {
		limit = len(b.Entries)
	}
	nameIndex := naming.Index{}
	var graph observation.Graph
	var observeErr error
	if !emptyQuery {
		graph, observeErr = observation.Build(b.Organizer.Source, b.Organizer.Target, b.Entries)
		if observeErr != nil {
			return nil, fmt.Errorf("observe filesystem naming evidence: %w", observeErr)
		}
		nameIndex = naming.BuildIndex(b.Entries, graph.Sources, graph.Libraries, b.Organizer.Extensions)
	}
	type match struct {
		id    string
		rank  int
		title string
	}
	matches := make([]match, 0, min(limit, len(b.Entries)))
	for id, w := range b.Entries {
		rank := entrySearchRank(w, nameIndex[id], query)
		if rank < 0 {
			continue
		}
		matches = append(matches, match{id: id, rank: rank, title: strings.ToLower(w.Title)})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		if matches[i].title != matches[j].title {
			return matches[i].title < matches[j].title
		}
		return matches[i].id < matches[j].id
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.id)
	}
	if emptyQuery {
		graph, observeErr = observation.Build(b.Organizer.Source, b.Organizer.Target, b.Entries)
	}
	summaries := a.entrySummariesObserved(ctx, b, ids, graph, observeErr)
	out := make([]EntryView, 0, len(matches))
	for _, m := range matches {
		v := viewOf(b, b.Entries[m.id])
		if summary, ok := summaries[m.id]; ok {
			s := summary
			v.Summary = &s
		}
		out = append(out, v)
	}
	return out, nil
}

func entrySearchRank(w catalog.Entry, names naming.EntryEvidence, query string) int {
	values := append([]string{w.Title, catalog.OutputTitle(w)}, names.Names...)
	best := -1
	for _, value := range values {
		v := strings.ToLower(strings.TrimSpace(value))
		var rank int
		switch {
		case v == query:
			rank = 0
		case strings.HasPrefix(v, query):
			rank = 1
		case strings.Contains(v, query):
			rank = 2
		case query == "":
			rank = 3
		default:
			continue
		}
		if best == -1 || rank < best {
			best = rank
		}
	}
	return best
}
func (a *App) GetEntry(ctx context.Context, id string) (EntryView, error) {
	select {
	case <-ctx.Done():
		return EntryView{}, ctx.Err()
	default:
	}
	rt := a.snapshot()
	if rt == nil {
		return EntryView{}, errors.New("runtime unavailable")
	}
	w, ok := rt.bundle.Entries[id]
	if !ok {
		return EntryView{}, managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", id))
	}
	graph, err := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
	if err != nil {
		return EntryView{}, managementErr(ErrorUnknown, fmt.Errorf("observe filesystem graph: %w", err))
	}
	out := viewOf(rt.bundle, w)
	for _, issue := range graph.Sources[id].Issues {
		out.ObservationIssues = append(out.ObservationIssues, fmt.Sprintf("%s: %v", issue.Path, issue.Err))
	}
	for _, issue := range graph.Libraries[id].Issues {
		out.ObservationIssues = append(out.ObservationIssues, fmt.Sprintf("%s: %v", issue.Path, issue.Err))
	}
	names := naming.BuildIndex(rt.bundle.Entries, graph.Sources, graph.Libraries, rt.bundle.Organizer.Extensions)
	out.FilterOptions = a.entryFilterOptionsObserved(rt, w, graph.Sources[id], names)
	out.Folders = entryFolders(ctx, w, graph.Sources[id], rt.bundle.Organizer.Extensions)
	out.Seasons, err = a.entrySeasons(ctx, rt.bundle, w, graph)
	if err != nil {
		return EntryView{}, err
	}
	return out, nil
}
func (a *App) Config(ctx context.Context) (ConfigView, error) {
	select {
	case <-ctx.Done():
		return ConfigView{}, ctx.Err()
	default:
	}
	rt := a.snapshot()
	if rt == nil {
		return ConfigView{}, errors.New("runtime unavailable")
	}
	b := rt.bundle
	v := ConfigView{Organizer: b.Organizer, Sources: map[string]configstore.ContentSource{}, Clients: map[string]configstore.Client{}, SourceCapabilities: map[string]SourceCapability{}}
	for id, f := range b.Sources {
		v.Sources[id] = f
		src := rt.sources[id]
		caps := src.Capabilities()
		v.SourceCapabilities[id] = SourceCapability{Current: caps.RSS, Historical: caps.Historical, Discovery: caps.Search}
	}
	secrets := secretstore.Store{Root: a.configRoot}
	for id, c := range b.Clients {
		credentialSet, err := secrets.Exists(secretstore.DownloaderPassword(id))
		if err != nil {
			return ConfigView{}, fmt.Errorf("read downloader %q secret status: %w", id, err)
		}
		v.Clients[id] = c.Public(credentialSet)
	}
	return v, nil
}
func (a *App) ValidateConfig(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err := configstore.Load(a.configRoot)
	if err != nil {
		return managementErr(ErrorInvalid, err)
	}
	return nil
}

func (a *App) mutateConfig(ctx context.Context, fn func(configstore.Store) error) error {
	return a.withOperation(ctx, func() error {
		store := configstore.Store{Root: a.configRoot}
		backup, err := store.Backup()
		if err != nil {
			return err
		}
		if err := fn(store); err != nil {
			return managementErr(ErrorConflict, err)
		}
		if err := a.reloadRuntime(); err != nil {
			restoreErr := store.Restore(backup)
			reloadErr := a.reloadRuntime()
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("reload runtime after mutation: %w", err), restoreErr, reloadErr))
		}
		return nil
	})
}
func jsonBytes(v any) ([]byte, error) {
	return jsonfile.Marshal(v)
}
func (a *App) PutOrganizer(ctx context.Context, v organizer.Config) (ConfigView, error) {
	b, e := jsonBytes(v)
	if e != nil {
		return ConfigView{}, e
	}
	e = a.mutateConfig(ctx, func(s configstore.Store) error { return s.PutOrganizer(b) })
	if e != nil {
		return ConfigView{}, e
	}
	return a.Config(ctx)
}
func (a *App) PutClient(ctx context.Context, id string, v configstore.Client) (configstore.Client, error) {
	if v.ID != id {
		return configstore.Client{}, managementErr(ErrorInvalid, errors.New("client id must match path"))
	}
	v.CredentialSet = false
	b, e := jsonBytes(v)
	if e != nil {
		return v, e
	}
	e = a.mutateConfig(ctx, func(s configstore.Store) error { return s.Put("clients", id, b) })
	if e != nil {
		return v, e
	}
	credentialSet, e := (secretstore.Store{Root: a.configRoot}).Exists(secretstore.DownloaderPassword(id))
	if e != nil {
		return configstore.Client{}, e
	}
	rt := a.snapshot()
	return rt.bundle.Clients[id].Public(credentialSet), nil
}

// SaveClient accepts a write-only downloader password. Omission preserves the
// existing secret; an explicit empty string deletes it. Secret material lives
// only under /config/secrets and is never serialized into client configuration.
func (a *App) SaveClient(ctx context.Context, id string, v configstore.Client, password *string) (configstore.Client, error) {
	if v.ID != id {
		return configstore.Client{}, managementErr(ErrorInvalid, errors.New("client id must match path"))
	}
	v.CredentialSet = false
	data, err := jsonBytes(v)
	if err != nil {
		return v, err
	}
	secretID := secretstore.DownloaderPassword(id)
	secrets := secretstore.Store{Root: a.configRoot}
	err = a.withOperation(ctx, func() error {
		store := configstore.Store{Root: a.configRoot}
		backup, err := store.Backup()
		if err != nil {
			return err
		}
		oldSecret, oldSecretSet, err := secrets.Lookup(secretID)
		if err != nil {
			return err
		}
		restore := func(cause error) error {
			configErr := store.Restore(backup)
			secretErr := restoreSecret(secrets, secretID, oldSecret, oldSecretSet)
			reloadErr := a.reloadRuntime()
			return errors.Join(cause, rollbackFailure("restore client configuration", configErr), rollbackFailure("restore client secret", secretErr), rollbackFailure("reload restored runtime", reloadErr))
		}
		if err := store.Put("clients", id, data); err != nil {
			return managementErr(ErrorConflict, err)
		}
		if password != nil {
			if *password == "" {
				err = secrets.Delete(secretID)
			} else {
				err = secrets.Put(secretID, *password)
			}
			if err != nil {
				return managementErr(ErrorConflict, restore(fmt.Errorf("write client secret: %w", err)))
			}
		}
		if err := a.reloadRuntime(); err != nil {
			return managementErr(ErrorConflict, restore(fmt.Errorf("reload runtime after client mutation: %w", err)))
		}
		return nil
	})
	if err != nil {
		return configstore.Client{}, err
	}
	credentialSet, err := secrets.Exists(secretID)
	if err != nil {
		return configstore.Client{}, err
	}
	return v.Public(credentialSet), nil
}

func restoreSecret(store secretstore.Store, id secretstore.ID, value string, exists bool) error {
	if exists {
		return store.Put(id, value)
	}
	return store.Delete(id)
}

func (a *App) DeleteClient(ctx context.Context, id string) error {
	secretID := secretstore.DownloaderPassword(id)
	secrets := secretstore.Store{Root: a.configRoot}
	return a.withOperation(ctx, func() error {
		store := configstore.Store{Root: a.configRoot}
		backup, err := store.Backup()
		if err != nil {
			return err
		}
		oldSecret, oldSecretSet, err := secrets.Lookup(secretID)
		if err != nil {
			return err
		}
		if err := store.Delete("clients", id); err != nil {
			return managementErr(ErrorConflict, err)
		}
		if err := secrets.Delete(secretID); err != nil {
			configErr := store.Restore(backup)
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("delete client secret: %w", err), rollbackFailure("restore client configuration", configErr)))
		}
		if err := a.reloadRuntime(); err != nil {
			configErr := store.Restore(backup)
			secretErr := restoreSecret(secrets, secretID, oldSecret, oldSecretSet)
			reloadErr := a.reloadRuntime()
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("reload runtime after client deletion: %w", err), rollbackFailure("restore client configuration", configErr), rollbackFailure("restore client secret", secretErr), rollbackFailure("reload restored runtime", reloadErr)))
		}
		return nil
	})
}

func (a *App) PutSource(ctx context.Context, id string, v configstore.ContentSource) (configstore.ContentSource, error) {
	if v.ID != id {
		return configstore.ContentSource{}, managementErr(ErrorInvalid, errors.New("content source id must match path"))
	}
	b, e := jsonBytes(v)
	if e != nil {
		return v, e
	}
	e = a.mutateConfig(ctx, func(s configstore.Store) error { return s.Put("sources", id, b) })
	if e != nil {
		return v, e
	}
	rt := a.snapshot()
	return rt.bundle.Sources[id], nil
}
func (a *App) DeleteSource(ctx context.Context, id string) error {
	err := a.mutateConfig(ctx, func(s configstore.Store) error { return s.Delete("sources", id) })
	if err == nil {
	}
	return err
}

func (a *App) CreateEntry(ctx context.Context, mediaType string, season int, d EntryDeclaration) (EntryView, error) {
	var out EntryView
	err := a.withOperation(ctx, func() error {
		if mediaType != catalog.MediaSeries && mediaType != catalog.MediaMovie {
			return managementErr(ErrorInvalid, fmt.Errorf("unsupported media type %q", mediaType))
		}
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		title := strings.TrimSpace(d.Title)
		if err := catalog.ValidateDirectoryTitle(title); err != nil {
			return managementErr(ErrorInvalid, fmt.Errorf("entry %w", err))
		}
		if d.Year < 0 {
			return managementErr(ErrorInvalid, errors.New("entry year must not be negative"))
		}
		if mediaType == catalog.MediaSeries && (season < 0 || season > 99) {
			return managementErr(ErrorInvalid, errors.New("season must be 0-99"))
		}
		root, targetRoot := rt.bundle.Organizer.SeriesSource(), rt.bundle.Organizer.SeriesTarget()
		if mediaType == catalog.MediaMovie {
			root, targetRoot = rt.bundle.Organizer.MovieSource(), rt.bundle.Organizer.MovieTarget()
		}
		path := filepath.Join(root, catalog.SeriesDirName(title, d.Year))
		if old, readErr := catalog.ReadDeclaration(path); readErr != nil {
			return managementErr(ErrorInvalid, readErr)
		} else if old != nil {
			return managementErr(ErrorConflict, fmt.Errorf("entry declaration already exists at %s", path))
		}
		next := catalog.NewInitialDeclaration(catalog.InitialDeclarationInput{MediaType: mediaType, CanonicalTitle: title})
		next.Year, next.Enabled, next.Sources, next.Filters, next.Specials, next.Output, next.Blacklist, next.Movie, next.Folders = d.Year, d.Enabled, d.Sources, d.Filters, append([]catalog.SpecialMapping(nil), d.Specials...), d.Output, d.Blacklist, d.Movie, d.Folders
		sourceEntryExisted := false
		if _, err := os.Stat(path); err == nil {
			sourceEntryExisted = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return managementErr(ErrorInvalid, err)
		}
		createdDirs := []string{}
		mkdir := func(dir string) error {
			if _, err := os.Stat(dir); err == nil {
				return nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			createdDirs = append(createdDirs, dir)
			return nil
		}
		cleanup := func() {
			_ = os.Remove(filepath.Join(path, ".aninode.json"))
			for i := len(createdDirs) - 1; i >= 0; i-- {
				_ = os.Remove(createdDirs[i])
			}
			if !sourceEntryExisted {
				_ = os.Remove(path)
			}
		}
		// The configured media-library category root is a namespace prerequisite,
		// not evidence that this Entry or season already exists in the library.
		// Keep it present so inventory can distinguish an observed empty Entry from
		// an unavailable library, but never fabricate Entry/season output folders.
		if err := mkdir(targetRoot); err != nil {
			cleanup()
			return managementErr(ErrorInvalid, err)
		}
		if mediaType == catalog.MediaSeries {
			seasonName := fmt.Sprintf("Season %02d", season)
			if err := mkdir(filepath.Join(path, seasonName)); err != nil {
				cleanup()
				return managementErr(ErrorInvalid, err)
			}
		}
		if err := catalog.WriteDeclaration(path, next); err != nil {
			cleanup()
			return managementErr(ErrorInvalid, err)
		}
		rollback := func() error {
			cleanup()
			return a.reloadRuntime()
		}
		if err := a.reloadRuntime(); err != nil {
			rollbackErr := rollback()
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("entry declaration rejected: %w", err), rollbackFailure("restore runtime after rejected entry declaration", rollbackErr)))
		}
		rt = a.snapshot()
		key := mediaType + "/" + filepath.Base(path)
		w, ok := rt.bundle.Entries[key]
		if !ok {
			for _, candidate := range rt.bundle.Entries {
				if filepath.Clean(candidate.DeclarationPath) == filepath.Clean(path) {
					w, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			rollbackErr := rollback()
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("created entry did not appear after reload at %s", path), rollbackFailure("restore runtime after missing created entry", rollbackErr)))
		}
		out = viewOf(rt.bundle, w)
		out.FilterOptions = a.entryFilterOptions(rt, w)
		return nil
	})
	return out, err
}

func (a *App) PutEntry(ctx context.Context, id string, d EntryDeclaration) (EntryView, error) {
	var out EntryView
	err := a.withOperation(ctx, func() error {
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		old, ok := rt.bundle.Entries[id]
		if !ok {
			return managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", id))
		}
		oldSide, err := catalog.ReadDeclaration(old.DeclarationPath)
		if err != nil {
			return err
		}
		rollback := func() error {
			var restoreErr error
			if oldSide != nil {
				restoreErr = catalog.WriteDeclaration(old.DeclarationPath, *oldSide)
			} else {
				if err := os.Remove(filepath.Join(old.DeclarationPath, ".aninode.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
					restoreErr = err
				}
			}
			return errors.Join(restoreErr, a.reloadRuntime())
		}
		title := strings.TrimSpace(d.Title)
		if err := catalog.ValidateDirectoryTitle(title); err != nil {
			return managementErr(ErrorInvalid, fmt.Errorf("entry %w", err))
		}
		if d.Year < 0 {
			return managementErr(ErrorInvalid, errors.New("entry year must not be negative"))
		}
		year := d.Year
		next := catalog.Declaration{Title: title, Year: year, Enabled: d.Enabled, Sources: d.Sources, Filters: d.Filters, Specials: append([]catalog.SpecialMapping(nil), d.Specials...), Output: d.Output, Blacklist: d.Blacklist, Movie: d.Movie, Folders: d.Folders}
		if err := catalog.WriteDeclaration(old.DeclarationPath, next); err != nil {
			return managementErr(ErrorInvalid, err)
		}
		if err := a.reloadRuntime(); err != nil {
			rollbackErr := rollback()
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("entry declaration rejected: %w", err), rollbackFailure("restore rejected entry declaration", rollbackErr)))
		}
		rt = a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		nw, ok := rt.bundle.Entries[id]
		if !ok {
			rollbackErr := rollback()
			return managementErr(ErrorConflict, errors.Join(errors.New("entry disappeared after declaration update"), rollbackFailure("restore entry declaration after reload mismatch", rollbackErr)))
		}
		physical, observeErr := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
		if observeErr != nil {
			return fmt.Errorf("observe filesystem graph after declaration update: %w", observeErr)
		}
		names := naming.BuildIndex(rt.bundle.Entries, physical.Sources, physical.Libraries, rt.bundle.Organizer.Extensions)
		out = viewOf(rt.bundle, nw)
		out.FilterOptions = a.entryFilterOptionsObserved(rt, nw, physical.Sources[id], names)
		return a.reconcileEntryPublicationObserved(ctx, rt, physical, id)
	})
	return out, err
}

func (a *App) PutFolderProjection(ctx context.Context, key, folder string, rule catalog.FolderProjection) (catalog.FolderProjection, error) {
	var out catalog.FolderProjection
	err := a.withOperation(ctx, func() error {
		if err := catalog.ValidateFolderProjections(map[string]catalog.FolderProjection{folder: rule}); err != nil {
			return managementErr(ErrorInvalid, err)
		}
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		e, ok := rt.bundle.Entries[key]
		if !ok {
			return managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", key))
		}
		if e.MediaType != catalog.MediaSeries {
			return managementErr(ErrorInvalid, errors.New("folder projections apply only to series"))
		}
		old, err := catalog.ReadDeclaration(e.DeclarationPath)
		if err != nil || old == nil {
			return errors.Join(err, errors.New("series declaration unavailable"))
		}
		if info, statErr := os.Lstat(filepath.Join(e.DeclarationPath, folder)); statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return managementErr(ErrorInvalid, fmt.Errorf("source folder %q is not a real direct child directory", folder))
		}
		next := *old
		next.Folders = maps.Clone(old.Folders)
		if next.Folders == nil {
			next.Folders = map[string]catalog.FolderProjection{}
		}
		next.Folders[folder] = rule
		rollback := func() error {
			return errors.Join(catalog.WriteDeclaration(e.DeclarationPath, *old), a.reloadRuntime())
		}
		if err := catalog.WriteDeclaration(e.DeclarationPath, next); err != nil {
			return managementErr(ErrorInvalid, err)
		}
		if err := a.reloadRuntime(); err != nil {
			rollbackErr := rollback()
			return managementErr(ErrorConflict, errors.Join(fmt.Errorf("folder projection rejected: %w", err), rollbackFailure("restore rejected folder projection", rollbackErr)))
		}
		out = rule
		rt = a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable after folder projection update")
		}
		return a.reconcileEntryPublication(ctx, rt, key)
	})
	return out, err
}

type FolderManagementState struct {
	Name            string                   `json:"name"`
	Path            string                   `json:"path"`
	SuggestedSeason int                      `json:"suggested_season"`
	Projection      catalog.FolderProjection `json:"projection"`
}

func entryFolders(ctx context.Context, e catalog.Entry, source filesystem.Snapshot, extensions []string) []FolderManagementState {
	if e.MediaType != catalog.MediaSeries {
		return []FolderManagementState{}
	}
	inferred := catalog.InferSeriesContainersFromSnapshot(e.DeclarationPath, source, extensions)
	out := make([]FolderManagementState, 0, len(inferred))
	for _, c := range inferred {
		select {
		case <-ctx.Done():
			return out
		default:
		}
		name := filepath.Base(c.Path)
		rule, ok := e.FolderProjections[name]
		if !ok {
			rule.Season = c.SuggestedSeason
		}
		out = append(out, FolderManagementState{Name: name, Path: c.Path, SuggestedSeason: c.SuggestedSeason, Projection: rule})
	}
	return out
}

type SeasonManagementState struct {
	EntryKey        string         `json:"entry_key"`
	Season          int            `json:"season"`
	InventoryKnown  bool           `json:"inventory_known"`
	InventoryError  string         `json:"inventory_error,omitempty"`
	PresentEpisodes []int          `json:"present_episodes"`
	Missing         []int          `json:"missing"`
	Status          string         `json:"status"`
	Detail          string         `json:"detail,omitempty"`
	RepairReviews   []RepairReview `json:"repair_reviews,omitempty"`
}

func (a *App) entrySeasons(ctx context.Context, b configstore.Bundle, w catalog.Entry, physical observation.Graph) ([]SeasonManagementState, error) {
	id := w.Key
	inv := inventory.ScanGraph(map[string]catalog.Entry{id: w}, physical, b.Organizer.Extensions)
	seasons := map[int]bool{}
	for _, sn := range w.Seasons {
		seasons[sn] = true
	}
	if ei, ok := inv.Entries[id]; ok {
		for sn := range ei.Seasons {
			seasons[sn] = true
		}
	}
	nums := make([]int, 0, len(seasons))
	for sn := range seasons {
		nums = append(nums, sn)
	}
	sort.Ints(nums)
	out := make([]SeasonManagementState, 0, len(nums))
	for _, sn := range nums {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		si := inv.Season(id, sn)
		missing, status := completeness.InternalGaps(inv, id, sn)
		v := SeasonManagementState{EntryKey: id, Season: sn, InventoryKnown: si.Known, PresentEpisodes: []int{}, Missing: append([]int{}, missing...), Status: status, RepairReviews: a.repairReviewsFor(id, sn)}
		if si.Err != nil {
			v.InventoryError = si.Err.Error()
		}
		for ep := range si.Episodes {
			v.PresentEpisodes = append(v.PresentEpisodes, ep)
		}
		sort.Ints(v.PresentEpisodes)
		out = append(out, v)
	}
	return out, nil
}

type BackfillActionResult struct {
	State        completeness.SeasonState   `json:"state"`
	Selected     []candidate.Candidate      `json:"selected,omitempty"`
	Acquisitions []automation.AcquireResult `json:"acquisitions,omitempty"`
	Reviews      []RepairReview             `json:"reviews,omitempty"`
	Issues       []OperationIssue           `json:"issues,omitempty"`
}

func (a *App) Backfill(ctx context.Context, id string, season, from, through int) (out BackfillActionResult, finalErr error) {
	started := a.clock()
	defer func() {
		out.Issues = backfillIssues(out, finalErr)
		a.recordBackfill(started, out)
	}()
	finalErr = a.withOperation(ctx, func() error {
		if from <= 0 || through < from {
			return managementErr(ErrorInvalid, fmt.Errorf("completion range requires positive from and through >= from"))
		}
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		if _, ok := rt.bundle.Entries[id]; !ok {
			return managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", id))
		}
		physical, e := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
		if e != nil {
			return fmt.Errorf("observe filesystem graph: %w", e)
		}
		inv := inventory.ScanGraph(rt.bundle.Entries, physical, rt.bundle.Organizer.Extensions)
		runner := automation.Runner{Bundle: rt.bundle, Clients: rt.backends, Now: a.now}
		st, sel, acq, reviews, warn, e := a.runManualBackfill(ctx, rt, id, season, completeness.EpisodeRange{From: from, Through: through}, inv, physical, runner)
		out.State = st
		out.Selected = sel
		out.Acquisitions = acq
		out.Reviews = reviews
		a.replaceRepairReviewsFor(id, season, reviews)
		out.Issues = appendMessageIssues(out.Issues, "warning", "backfill", "historical_search_warning", warn)
		return e
	})
	return out, finalErr
}

// ConfirmRepair acquires one low-confidence historical candidate after the
// user explicitly approves it. The disposable review is revalidated against
// current configuration and filesystem availability before any downloader
// mutation occurs; stale confirmations fail closed.
func (a *App) ConfirmRepair(ctx context.Context, id string, season int, reviewID string) (out BackfillActionResult, finalErr error) {
	started := a.clock()
	defer func() {
		out.Issues = backfillIssues(out, finalErr)
		a.recordBackfill(started, out)
	}()
	finalErr = a.withOperation(ctx, func() error {
		review, ok := a.repairReview(reviewID)
		if !ok || review.EntryKey != id || review.Season != season {
			return managementErr(ErrorNotFound, fmt.Errorf("repair review %q is no longer available", reviewID))
		}
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		w, ok := rt.bundle.Entries[id]
		if !ok {
			return managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", id))
		}
		physical, err := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
		if err != nil {
			return fmt.Errorf("observe filesystem graph: %w", err)
		}
		inv := inventory.ScanGraph(rt.bundle.Entries, physical, rt.bundle.Organizer.Extensions)
		reviewStart, reviewEnd := review.episodeRange()
		if reviewStart <= 0 || reviewEnd < reviewStart {
			a.removeRepairReview(reviewID)
			return managementErr(ErrorConflict, fmt.Errorf("repair review %q has an invalid episode range", reviewID))
		}
		missing, status := completeness.MissingInRange(inv, id, season, completeness.EpisodeRange{From: reviewStart, Through: reviewEnd})
		out.State = completeness.SeasonState{EntryKey: id, Season: season, InventoryKnown: inv.Season(id, season).Known, Missing: missing, Status: status}
		if inv.Availability(review.candidate.Episode) == inventory.Present {
			a.removeRepairReview(reviewID)
			return managementErr(ErrorConflict, fmt.Errorf("repair review %q is stale because the episode is already present", reviewID))
		}
		if inv.Availability(review.candidate.Episode) == inventory.Unknown {
			return managementErr(ErrorConflict, fmt.Errorf("repair review %q cannot be confirmed while filesystem availability is unknown", reviewID))
		}

		plan, err := candidate.BuildBoundPlan(ctx, rt.bundle, id, map[string][]release.Release{review.SourceID: {review.candidate.Release}})
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			a.removeRepairReview(reviewID)
			return managementErr(ErrorConflict, fmt.Errorf("repair review %q is stale because its episode range is already complete", reviewID))
		}
		filtered, err := backfillSelection(plan, w, season, missing)
		if err != nil {
			return err
		}
		gateWarnings := gateAvailability(&filtered, inv)
		out.Issues = appendMessageIssues(out.Issues, "warning", "backfill", "repair_confirmation_warning", gateWarnings)

		confirmed := candidate.Plan{}
		for _, value := range filtered.Candidates {
			if value.Decision == candidate.DecisionSelected && value.Acquisition.ID == review.candidate.Acquisition.ID {
				confirmed.Candidates = append(confirmed.Candidates, value)
			}
		}
		if len(confirmed.Candidates) != 1 {
			return managementErr(ErrorConflict, fmt.Errorf("repair review %q is stale; wait for the next repair scan and confirm again", reviewID))
		}

		runner := automation.Runner{Bundle: rt.bundle, Clients: rt.backends, Now: a.now}
		values, err := runner.Acquire(ctx, confirmed, physical)
		out.Selected = append(out.Selected, confirmed.Candidates...)
		out.Acquisitions = append(out.Acquisitions, values...)
		if err != nil {
			return err
		}
		accepted := false
		for _, value := range values {
			if value.Decision == automation.AcquireSubmitted || value.Decision == automation.AcquireRecovered {
				accepted = true
				break
			}
		}
		if !accepted {
			return managementErr(ErrorConflict, fmt.Errorf("confirmed repair was not accepted by the downloader"))
		}
		a.removeRepairReview(reviewID)
		return nil
	})
	return out, finalErr
}

type PreviewRequest struct {
	Name string `json:"name"`
}
type PreviewResult struct {
	Parsed        medianame.Parsed     `json:"parsed"`
	EntryKey      string               `json:"entry_key"`
	SourceEpisode *episode.Key         `json:"source_episode,omitempty"`
	TargetEpisode *episode.Key         `json:"target_episode,omitempty"`
	Decision      string               `json:"decision"`
	Reason        string               `json:"reason,omitempty"`
	ExpectedPath  string               `json:"expected_path,omitempty"`
	OutputTitle   string               `json:"output_title,omitempty"`
	Items         []organizer.PlanItem `json:"items,omitempty"`
	Issues        []OperationIssue     `json:"issues,omitempty"`
}

func (a *App) PreviewEntry(ctx context.Context, id string, req PreviewRequest) (PreviewResult, error) {
	b, err := a.currentBundle()
	if err != nil {
		return PreviewResult{}, err
	}
	w, ok := b.Entries[id]
	if !ok {
		return PreviewResult{}, managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", id))
	}
	if w.MediaType == catalog.MediaMovie {
		cfg, ce := b.OrganizerForPreview(id, 0)
		if ce != nil {
			return PreviewResult{}, ce
		}
		plan, pe := organizer.BuildPlan(ctx, cfg)
		if pe != nil {
			return PreviewResult{}, pe
		}
		res := PreviewResult{EntryKey: id, OutputTitle: catalog.OutputTitle(w), Decision: "selected", Reason: "movie bundle publication plan", Items: plan.Items}
		res.Issues = appendMessageIssues(res.Issues, "warning", "publication", "publication_warning", plan.Warnings)
		for _, item := range plan.Items {
			if item.Action == organizer.ActionConflict || item.Action == organizer.ActionUnmatched {
				res.Decision = "conflict"
				res.Reason = item.Detail
				break
			}
		}
		return res, nil
	}
	tmp := b
	tmp.Sources = maps.Clone(b.Sources)
	tmp.Entries = map[string]catalog.Entry{id: w}
	const fid = "__preview__"
	tmp.Sources[fid] = configstore.ContentSource{ID: fid, Provider: "generic", RSS: []configstore.RSSSource{{URL: "https://preview.invalid/rss"}}, Enabled: true, Priority: -1}
	ww := w
	ww.Sources = []string{fid}
	tmp.Entries[id] = ww
	values, _ := release.EnrichManyObserved("generic", []rss.Entry{{Title: req.Name, GUID: "preview-guid", DownloadURL: "https://preview.invalid/item.torrent"}}, []release.NameObservation{{Value: req.Name, Source: "preview-native-name"}})
	rv := values[0]
	rv.MediaType = w.MediaType
	c, e := candidate.BuildBound(ctx, tmp, id, fid, rv)
	if e != nil {
		return PreviewResult{}, e
	}
	res := PreviewResult{Parsed: rv.Parsed, EntryKey: id, OutputTitle: catalog.OutputTitle(w), Decision: "unknown", Reason: "release cannot be used for this Entry"}
	if pattern, excluded := catalog.BlacklistMatch(w.Blacklist, filepath.Base(req.Name), filepath.ToSlash(req.Name)); excluded {
		res.Decision = string(organizer.ActionExcluded)
		res.Reason = fmt.Sprintf("matched Entry blacklist pattern %q", pattern)
		return res, nil
	}
	res.SourceEpisode = &c.SourceEpisode
	res.TargetEpisode = &c.Episode
	res.Decision = string(c.Decision)
	res.Reason = c.Reason
	cfg, ce := b.OrganizerForPreview(id, c.SourceEpisode.Season)
	if ce == nil {
		name := filepath.Base(req.Name)
		if filepath.Ext(name) == "" && len(cfg.Extensions) > 0 {
			name += cfg.Extensions[0]
		}
		if dest, matched, de := organizer.PreviewDestination(cfg, name); de == nil && matched {
			res.ExpectedPath = dest
		}
	}
	return res, nil
}

func (a *App) MigrationPlan(ctx context.Context) (MigrationResult, error) {
	return a.Migrate(ctx, false)
}

func (a *App) MigrationApplyGroups(ctx context.Context, keys []string) (MigrationResult, error) {
	selected := make(map[string]bool, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key != "" {
			selected[key] = true
		}
	}
	if len(selected) == 0 {
		return MigrationResult{}, managementErr(ErrorInvalid, errors.New("at least one migration group is required"))
	}
	return a.migrate(ctx, true, selected)
}

type MigrationUnitInput struct {
	Key       string
	EntryKey  string
	Title     string
	Season    int
	Year      int
	MediaType string
	Filters   releasefilter.Filters
	Movie     catalog.MovieDeclaration
	Tasks     []MigrationTaskIdentity
}

type MigrationTaskIdentity struct {
	Client   string `json:"client"`
	TaskID   string `json:"task_id"`
	InfoHash string `json:"info_hash,omitempty"`
}

type targetedMigrationBackend struct {
	download.Backend
	ids []string
}

func (b targetedMigrationBackend) Get(ctx context.Context, id string) (download.Task, error) {
	v, ok := b.Backend.(interface {
		Get(context.Context, string) (download.Task, error)
	})
	if !ok {
		return download.Task{}, download.ErrUnsupported
	}
	return v.Get(ctx, id)
}
func (b targetedMigrationBackend) List(ctx context.Context) ([]download.Task, error) {
	out := make([]download.Task, 0, len(b.ids))
	for _, id := range b.ids {
		task, err := b.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}
func (b targetedMigrationBackend) Files(ctx context.Context, id string) ([]download.File, error) {
	v, ok := b.Backend.(interface {
		Files(context.Context, string) ([]download.File, error)
	})
	if !ok {
		return nil, download.ErrUnsupported
	}
	return v.Files(ctx, id)
}

func targetedMigrationEngine(bundle configstore.Bundle, backends map[string]download.Backend, selected []MigrationTaskIdentity) (migration.Engine, error) {
	ids := map[string][]string{}
	for _, item := range selected {
		if item.Client == "" || item.TaskID == "" || backends[item.Client] == nil {
			return migration.Engine{}, errors.New("invalid migration task selection")
		}
		ids[item.Client] = append(ids[item.Client], item.TaskID)
	}
	clients := make(map[string]download.Backend, len(ids))
	for client, taskIDs := range ids {
		clients[client] = targetedMigrationBackend{Backend: backends[client], ids: taskIDs}
	}
	return migration.Engine{Bundle: bundle, Clients: clients}, nil
}

// MigrationApplyUnit turns the user's corrected presentation into a declared
// Entry, then re-observes and replans only the stable task identities from the
// confirmed unit. Paths and matches supplied by the browser are never trusted.
func (a *App) MigrationApplyUnit(ctx context.Context, input MigrationUnitInput) (MigrationResult, error) {
	input.Key, input.Title = strings.TrimSpace(input.Key), strings.TrimSpace(input.Title)
	input.Filters = input.Filters.Normalize()
	if err := input.Filters.Validate(); err != nil {
		return MigrationResult{}, managementErr(ErrorInvalid, err)
	}
	if input.MediaType == catalog.MediaMovie {
		input.Season = 0
	}
	if input.Key == "" || input.Title == "" {
		return MigrationResult{}, managementErr(ErrorInvalid, errors.New("migration key and title are required"))
	}
	if len(input.Tasks) == 0 {
		return MigrationResult{}, managementErr(ErrorConflict, errors.New("migration preview is stale; check download tasks again"))
	}
	if input.MediaType != catalog.MediaSeries && input.MediaType != catalog.MediaMovie {
		return MigrationResult{}, managementErr(ErrorInvalid, errors.New("media_type must be series or movie"))
	}
	if input.MediaType == catalog.MediaSeries && (input.Season < 0 || input.Season > 99) {
		return MigrationResult{}, managementErr(ErrorInvalid, errors.New("season must be 0-99"))
	}
	if input.Year < 0 || input.Year > 9999 || input.Title == "." || input.Title == ".." || strings.ContainsAny(input.Title, `/\\`) {
		return MigrationResult{}, managementErr(ErrorInvalid, errors.New("invalid migration title or year"))
	}

	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	lock, lockErr := filesystem.AcquireWriterLock(a.configRoot)
	if lockErr != nil {
		return MigrationResult{}, managementErr(ErrorConflict, fmt.Errorf("acquire writer ownership: %w", lockErr))
	}
	defer lock.Close()
	if err := a.reloadRuntime(); err != nil {
		return MigrationResult{}, err
	}
	rt := a.snapshot()
	if rt == nil {
		return MigrationResult{}, errors.New("runtime unavailable")
	}
	previewEngine, err := targetedMigrationEngine(rt.bundle, rt.backends, input.Tasks)
	if err != nil {
		return MigrationResult{}, managementErr(ErrorInvalid, err)
	}
	previewPlans, err := previewEngine.Scan(ctx)
	preview := MigrationResult{Plans: previewPlans, Groups: groupMigrationPlans(rt.bundle, previewPlans)}
	if err != nil {
		return preview, err
	}
	var selected *MigrationGroup
	for i := range preview.Groups {
		if preview.Groups[i].Key == input.Key {
			selected = &preview.Groups[i]
			break
		}
	}
	if selected == nil {
		return preview, managementErr(ErrorConflict, errors.New("migration unit changed; check download tasks again"))
	}
	tasks := make(map[string]bool, len(selected.Plans))
	for _, plan := range selected.Plans {
		tasks[plan.Client+"\x00"+plan.TaskID] = true
	}

	var chosen catalog.Entry
	if input.EntryKey != "" {
		var ok bool
		chosen, ok = rt.bundle.Entries[input.EntryKey]
		if !ok {
			return preview, managementErr(ErrorNotFound, fmt.Errorf("confirmed Entry %q not found", input.EntryKey))
		}
		if chosen.MediaType != input.MediaType {
			return preview, managementErr(ErrorConflict, errors.New("confirmed Entry media type does not match"))
		}
	} else if selected.EntryKey != "" {
		chosen = rt.bundle.Entries[selected.EntryKey]
	}
	// Explicit correction names the durable identity. Prefer an already existing
	// canonical Entry even when the current release name did not match it yet.
	var canonicalMatches []catalog.Entry
	for _, candidate := range rt.bundle.Entries {
		if candidate.MediaType == input.MediaType && candidate.Title == input.Title && candidate.Year == input.Year {
			canonicalMatches = append(canonicalMatches, candidate)
		}
	}
	if input.EntryKey == "" && len(canonicalMatches) == 1 {
		chosen = canonicalMatches[0]
	}
	if input.EntryKey == "" && len(canonicalMatches) > 1 {
		return preview, managementErr(ErrorConflict, errors.New("multiple Entries share the confirmed canonical identity; entry_key is required"))
	}

	// Confirmation is transactional for every media type. Build the Entry in
	// memory, prove the migration/publication plan first, and persist the
	// declaration only after Apply has safely placed the real downloader source.
	// Folder-shaped torrents therefore keep their own container name; canonical
	// naming never materializes a source placeholder.
	planningBundle := rt.bundle
	planningBundle.Entries = make(map[string]catalog.Entry, len(rt.bundle.Entries)+1)
	for id, candidate := range rt.bundle.Entries {
		planningBundle.Entries[id] = candidate
	}
	var side catalog.Declaration
	declarationPath := ""
	existingEntry := chosen.Key != ""
	if existingEntry {
		loaded, readErr := catalog.ReadDeclaration(chosen.DeclarationPath)
		if readErr != nil || loaded == nil {
			return preview, managementErr(ErrorInvalid, fmt.Errorf("read confirmed Entry declaration: %w", readErr))
		}
		side = *loaded
		declarationPath = chosen.DeclarationPath
	} else {
		side = catalog.NewInitialDeclaration(catalog.InitialDeclarationInput{MediaType: input.MediaType, CanonicalTitle: input.Title})
		name := catalog.SeriesDirName(input.Title, input.Year)
		if input.MediaType == catalog.MediaSeries {
			declarationPath = filepath.Join(rt.bundle.Organizer.SeriesSource(), name)
		} else {
			declarationPath = filepath.Join(rt.bundle.Organizer.MovieSource(), name)
		}
	}
	side.Title, side.Year = input.Title, input.Year
	side.Filters = input.Filters
	if input.MediaType == catalog.MediaMovie && input.Movie.Classify != nil {
		side.Movie = input.Movie
	}
	entryKey := input.MediaType + "/" + filepath.Base(declarationPath)
	// A newly confirmed Entry has no durable source namespace until the real
	// downloader task is safely placed. Keeping DeclarationPath empty lets the
	// migration domain distinguish transient intent from an existing Entry whose
	// source root must never be moved to follow a task.
	planningDeclarationPath := declarationPath
	if !existingEntry {
		planningDeclarationPath = ""
	}
	chosen = catalog.EntryFromDeclaration(side, input.MediaType, entryKey, planningDeclarationPath, func() string {
		if input.MediaType == catalog.MediaMovie {
			return rt.bundle.Organizer.MovieTarget()
		}
		return rt.bundle.Organizer.SeriesTarget()
	}())
	planningBundle.Entries[chosen.Key] = chosen

	rt = a.snapshot()
	if rt == nil {
		return preview, errors.New("runtime unavailable")
	}
	engine := migration.Engine{Bundle: planningBundle, Clients: rt.backends}
	plans := make([]migration.Plan, 0, len(input.Tasks))
	for _, identity := range input.Tasks {
		plan, planErr := engine.PlanConfirmed(ctx, migration.ConfirmedIntent{Client: identity.Client, TaskID: identity.TaskID, InfoHash: identity.InfoHash, EntryKey: chosen.Key, SourceSeason: input.Season})
		if planErr != nil {
			failed := migration.Plan{Client: identity.Client, TaskID: identity.TaskID, TaskName: selected.Title, EntryKey: chosen.Key, MediaType: input.MediaType, Decision: migration.DecisionConflict, Detail: planErr.Error()}
			plans = append(plans, failed)
			updated := MigrationResult{Plans: plans, Groups: groupMigrationPlans(planningBundle, plans)}
			return updated, managementErr(ErrorConflict, fmt.Errorf("confirmed migration planning for %s/%s: %w", identity.Client, identity.TaskID, planErr))
		}
		plans = append(plans, plan)
	}
	updated := MigrationResult{Plans: plans, Groups: groupMigrationPlans(planningBundle, plans)}
	if len(plans) != len(tasks) {
		return updated, managementErr(ErrorConflict, errors.New("selected migration tasks changed"))
	}
	for _, plan := range plans {
		if !tasks[plan.Client+"\x00"+plan.TaskID] || (plan.Decision != migration.DecisionRelocate && plan.Decision != migration.DecisionAdopt) {
			return updated, managementErr(ErrorConflict, fmt.Errorf("confirmed migration plan rejected for %s/%s: %s", plan.Client, plan.TaskID, plan.Detail))
		}
		for _, identity := range input.Tasks {
			if identity.Client == plan.Client && identity.TaskID == plan.TaskID && identity.InfoHash != "" && plan.InfoHash != "" && !strings.EqualFold(identity.InfoHash, plan.InfoHash) {
				return updated, managementErr(ErrorConflict, errors.New("migration task identity changed"))
			}
		}
		result, applyErr := (migration.Engine{Bundle: planningBundle, Clients: rt.backends}).Apply(ctx, plan)
		updated.Applied = append(updated.Applied, result)
		if applyErr != nil {
			return updated, applyErr
		}
	}
	if input.MediaType == catalog.MediaMovie {
		if err := a.reloadRuntime(); err != nil {
			return updated, err
		}
	}
	return updated, nil
}
