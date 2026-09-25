package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aninode/internal/acquisition"
	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/filesystem"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
	"aninode/internal/naming"
	"aninode/internal/observation"
	"aninode/internal/organizer"
	"aninode/internal/releasefilter"
	"aninode/internal/source"
)

type observer interface {
	download.Backend
	Get(context.Context, string) (download.Task, error)
	List(context.Context) ([]download.Task, error)
	Files(context.Context, string) ([]download.File, error)
}

const migrationFileObservationWorkers = 16

type pauser interface {
	Pause(context.Context, string) error
}
type resumer interface {
	Resume(context.Context, string) error
}

type Decision string

const (
	DecisionAdopt    Decision = "adopt"
	DecisionRelocate Decision = "relocate"
	DecisionUnknown  Decision = "unknown"
	DecisionConflict Decision = "conflict"
	DecisionSkip     Decision = "skip"
)

// Target is part of a request-local plan. Migration has no journal or durable
// lifecycle state; every apply starts by observing the downloader again.
type Target struct {
	EntryKey     string `json:"entry_key"`
	Season       int    `json:"season"`
	EpisodeStart int    `json:"episode_start"`
	EpisodeEnd   int    `json:"episode_end"`
}

type Plan struct {
	Client              string                `json:"client"`
	TaskID              string                `json:"task_id"`
	TaskName            string                `json:"task_name,omitempty"`
	SavePath            string                `json:"save_path,omitempty"`
	Title               string                `json:"title,omitempty"`
	Season              int                   `json:"season,omitempty"`
	SeasonKnown         bool                  `json:"season_known,omitempty"`
	MediaType           string                `json:"media_type,omitempty"`
	Decision            Decision              `json:"decision"`
	InfoHash            string                `json:"info_hash,omitempty"`
	EntryKey            string                `json:"entry_key,omitempty"`
	DesiredPath         string                `json:"desired_path,omitempty"`
	SourceEpisode       Target                `json:"source_episode,omitempty"`
	Target              Target                `json:"target,omitempty"`
	AcquisitionID       string                `json:"acquisition_id,omitempty"`
	Evidence            []string              `json:"evidence,omitempty"`
	Detail              string                `json:"detail,omitempty"`
	FilterOptions       releasefilter.Options `json:"filter_options,omitempty"`
	ExpectedContentRoot string                `json:"-"`
	ObservedContentRoot string                `json:"-"`
	ExpectedPaths       []string              `json:"-"`
}

// ConfirmedIntent carries identity already resolved by the user. Unlike a
// scan candidate it is never passed through global external-name matching.
type ConfirmedIntent struct {
	Client       string
	TaskID       string
	InfoHash     string
	EntryKey     string
	SourceSeason int
}

type candidateMatch struct {
	w                  catalog.Entry
	mediaType          string
	sourceSeason       int
	sourceStart        int
	sourceEnd          int
	targetSeason       int
	targetStart        int
	targetEnd          int
	desired            string
	desiredContentRoot string
	evidence           []string
}

type migrationObservation struct {
	task          download.Task
	files         []download.File
	media         []download.File
	taskName      medianame.Name
	taskCohort    int
	fileAnalysis  medianame.Batch
	assetAnalysis medianame.Batch
	presentation  migrationPresentation
	err           error
}

type Result struct {
	Plan      Plan             `json:"plan"`
	Organized organizer.Result `json:"organized,omitempty"`
	Error     string           `json:"error,omitempty"`
}

type Engine struct {
	Bundle  configstore.Bundle
	Clients map[string]download.Backend
}

func (e Engine) Scan(ctx context.Context) ([]Plan, error) {
	var names naming.Index
	loadNames := func() error {
		if names != nil {
			return nil
		}
		names = naming.Index{}
		if len(e.Bundle.Entries) == 0 {
			return nil
		}
		graph, err := observation.Build(e.Bundle.Organizer.Source, e.Bundle.Organizer.Target, e.Bundle.Entries)
		if err != nil {
			return fmt.Errorf("observe filesystem naming evidence for migration: %w", err)
		}
		names = naming.BuildIndex(e.Bundle.Entries, graph.Sources, graph.Libraries, e.Bundle.Organizer.Extensions)
		return nil
	}
	ids := make([]string, 0, len(e.Clients))
	for id := range e.Clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Plan
	var errs []error
	for _, cid := range ids {
		ob, ok := e.Clients[cid].(observer)
		if !ok {
			out = append(out, Plan{Client: cid, Decision: DecisionUnknown, Detail: "backend cannot enumerate tasks and files"})
			continue
		}
		observed, listErr := e.observeClient(ctx, cid, ob)
		if listErr != nil {
			errs = append(errs, fmt.Errorf("migration scan %s: %w", cid, listErr))
			out = append(out, Plan{Client: cid, Decision: DecisionUnknown, Detail: listErr.Error()})
			continue
		}
		sort.Slice(observed, func(i, j int) bool { return observed[i].task.ID < observed[j].task.ID })
		taskNames := make([]string, len(observed))
		for i := range observed {
			taskNames[i] = observed[i].task.Name
		}
		taskAnalysis := medianame.AnalyzeMany(taskNames)
		for i := range observed {
			observed[i].taskName = taskAnalysis.Items[i]
			observed[i].taskCohort = cohortSize(taskAnalysis, i)
			seriesMedia := presentWantedSeriesMedia(observed[i].files, e.Bundle.Organizer.Extensions)
			fileNames := make([]string, len(seriesMedia))
			for fileIndex := range seriesMedia {
				fileNames[fileIndex] = filepath.Base(seriesMedia[fileIndex].Path)
			}
			observed[i].fileAnalysis = medianame.AnalyzeFiles(fileNames)
			assetNames := make([]string, 0, len(observed[i].files))
			for _, file := range observed[i].files {
				if file.Wanted {
					assetNames = append(assetNames, filepath.Base(file.Path))
				}
			}
			observed[i].assetAnalysis = medianame.AnalyzeFiles(assetNames)
			observed[i].presentation = inferMigrationPresentation(observed[i].task, seriesMedia, observed[i].taskName, observed[i].taskCohort, observed[i].fileAnalysis)
			if observed[i].presentation.MediaType == catalog.MediaMovie {
				observed[i].media = presentWantedMovieAssets(observed[i].files)
			} else {
				observed[i].media = seriesMedia
			}
		}
		for _, observation := range observed {
			task, files, ferr := observation.task, observation.files, observation.err
			filterOptions := migrationFilterOptions(observation)
			if ferr != nil {
				out = append(out, Plan{Client: cid, TaskID: task.ID, TaskName: task.Name, SavePath: task.SavePath, Decision: DecisionUnknown, Detail: "file observation failed: " + ferr.Error(), FilterOptions: filterOptions})
				continue
			}
			// Adoption discovers existing tasks from the whole configured download
			// workspace. Restricting this to the new TV subdirectory silently hid
			// every task using the pre-movie layout (for example downloads/Anime).
			sourceRoot := e.Bundle.Organizer.Source
			if !taskBelongsToSource(task, files, sourceRoot) {
				continue
			}
			// A downloader task already located inside one durable Entry source
			// namespace is managed by ordinary filesystem reconciliation. Migration
			// must not offer a second adoption lifecycle merely because publication
			// is incomplete or the release name is unusual.
			if e.taskAlreadyManaged(task, files) {
				continue
			}
			if len(observation.media) == 0 {
				out = append(out, Plan{Client: cid, TaskID: task.ID, TaskName: task.Name, SavePath: task.SavePath, Decision: DecisionUnknown, Detail: "no completed wanted assets for inferred media type", FilterOptions: filterOptions})
				continue
			}
			presentation := observation.presentation
			// Filesystem naming evidence can require a full shared namespace walk.
			// Defer it until a task actually needs identity routing; the common case
			// where every downloader task is already inside a durable Entry then
			// completes without scanning the media library at all.
			if err := loadNames(); err != nil {
				return nil, err
			}
			p := e.matchTask(ctx, cid, observation, names)
			p.FilterOptions = filterOptions
			if p.Decision == DecisionSkip {
				continue
			}
			p.TaskName = task.Name
			p.SavePath = task.SavePath
			p.Title, p.Season, p.SeasonKnown = presentation.Title, presentation.Season, presentation.SeasonKnown
			if p.EntryKey == "" {
				p.MediaType = presentation.MediaType
			} else {
				p.Season = p.SourceEpisode.Season
				p.SeasonKnown = true
				if value, exists := e.Bundle.Entries[p.EntryKey]; exists {
					p.Title = value.Title
				}
			}
			out = append(out, p)
		}
	}
	// A single acquisition identity may never be adopted from multiple tasks, even
	// when each task independently has convincing Entry/episode evidence.
	byAcquisition := map[string][]int{}
	for i := range out {
		if out[i].AcquisitionID != "" {
			byAcquisition[out[i].AcquisitionID] = append(byAcquisition[out[i].AcquisitionID], i)
		}
	}
	for acquisitionID, indexes := range byAcquisition {
		if len(indexes) < 2 {
			continue
		}
		for _, i := range indexes {
			out[i].Decision = DecisionConflict
			out[i].Detail = fmt.Sprintf("acquisition identity %s matches multiple downloader tasks", acquisitionID)
			out[i].EntryKey = ""
		}
	}
	return out, errors.Join(errs...)
}

func migrationFilterOptions(observation migrationObservation) releasefilter.Options {
	options := releasefilter.OptionsFromNames(observation.fileAnalysis.Items)
	if strings.TrimSpace(observation.taskName.Input) != "" {
		options = options.Merge(releasefilter.OptionsFromNames([]medianame.Name{observation.taskName}))
	}
	return options
}

func (e Engine) observeClient(ctx context.Context, client string, ob observer) ([]migrationObservation, error) {
	maps := mappingsForClient(e.Bundle, client)
	if batch, ok := any(ob).(download.ObservationLister); ok {
		values, err := batch.ListObservations(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]migrationObservation, 0, len(values))
		for _, value := range values {
			value.Task, value.Files = mapTaskFiles(value.Task, value.Files, maps)
			if !taskBelongsToSource(value.Task, nil, e.Bundle.Organizer.Source) {
				continue
			}
			out = append(out, migrationObservation{task: value.Task, files: value.Files})
		}
		return out, nil
	}
	tasks, err := ob.List(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	filtered := make([]download.Task, 0, len(tasks))
	for _, task := range tasks {
		task, _ = mapTaskFiles(task, nil, maps)
		if !taskBelongsToSource(task, nil, e.Bundle.Organizer.Source) {
			continue
		}
		// qBittorrent exposes content_path in its bulk task list but requires a
		// separate /torrents/files request for every task. When that content path
		// is present on the live filesystem and already sits inside a durable Entry
		// topology, migration has no work to do and can omit the task before the
		// expensive per-task remote observation. Any missing/unsafe/ambiguous path
		// falls back to the full file observation below.
		if e.taskClearlyManagedFromContentPath(task) {
			continue
		}
		filtered = append(filtered, task)
	}
	out := make([]migrationObservation, len(filtered))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := migrationFileObservationWorkers
	if len(filtered) < workers {
		workers = len(filtered)
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				task := filtered[i]
				out[i].task = task
				if optimized, ok := any(ob).(download.TaskFileObserver); ok {
					out[i].files, out[i].err = optimized.FilesForTask(ctx, task)
				} else {
					out[i].files, out[i].err = ob.Files(ctx, task.ID)
				}
				_, out[i].files = mapTaskFiles(download.Task{}, out[i].files, maps)
			}
		}()
	}
	for i := range filtered {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return out, nil
}

func mapTaskFiles(task download.Task, files []download.File, maps []download.PathMapping) (download.Task, []download.File) {
	task.SavePath = download.MapPath(task.SavePath, maps)
	task.ContentPath = download.MapPath(task.ContentPath, maps)
	mapped := make([]download.File, len(files))
	for i, file := range files {
		mapped[i] = file
		mapped[i].Path = download.MapPath(file.Path, maps)
	}
	return task, mapped
}

var migrationSpecialPathRE = regexp.MustCompile(`(?i)(?:^|[/\\ ._-])(?:season[ ._-]*0{1,2}|ova|oad|specials?|sp|特别篇)(?:$|[/\\ ._-])`)

type migrationPresentation struct {
	Title       string
	Season      int
	SeasonKnown bool
	MediaType   string
}

// inferMigrationPresentation is the migration boundary around medianame. The
// parser breaks every name into semantic components once; migration only
// decides how those components should be presented and grouped. Media type and
// season are deliberately returned together so a movie can never retain a
// series season.
func inferMigrationPresentation(task download.Task, media []download.File, name medianame.Name, taskCohort int, fileAnalysis medianame.Batch) migrationPresentation {
	title := strings.TrimSpace(name.Components.Title)
	if reliable, ok := medianame.ReliableEpisodeTitle(name); ok {
		title = reliable
	}
	season := name.Components.Season
	seasonKnown := name.Components.SeasonExplicit || season > 0
	series := taskCohort > 1 || hasSeriesComponents(name.Components)

	// Cohorts and semantic episode evidence define a series unit; raw torrent
	// file count does not. Movies commonly carry extra video files, while a
	// season commonly consists of many separate one-file downloader tasks.
	for _, item := range fileAnalysis.Items {
		if hasSeriesComponents(item.Components) {
			series = true
			if !seasonKnown && (item.Components.SeasonExplicit || item.Components.Season > 0) {
				season = item.Components.Season
				seasonKnown = true
			}
		}
		if title == "" {
			if reliable, ok := medianame.ReliableEpisodeTitle(item); ok {
				title = reliable
			}
		}
	}

	// Only an unresolved single parser cohort needs the migration-only topology
	// escape hatch: Season 00/OVA paths mean a special, otherwise it is a movie.
	if !series {
		if migrationSpecialPathRE.MatchString(task.Name) || migrationSpecialPathRE.MatchString(task.SavePath) || singleFileSpecialPath(media) {
			series = true
			season = 0
			seasonKnown = true
		} else {
			return migrationPresentation{Title: title, MediaType: catalog.MediaMovie}
		}
	}
	if !seasonKnown && season == 0 {
		season = 1
	}
	return migrationPresentation{Title: title, Season: season, SeasonKnown: seasonKnown, MediaType: catalog.MediaSeries}
}

func singleFileSpecialPath(files []download.File) bool {
	for _, file := range files {
		if migrationSpecialPathRE.MatchString(file.Path) {
			return true
		}
	}
	return false
}

func cohortSize(batch medianame.Batch, itemIndex int) int {
	for _, cohort := range batch.Cohorts {
		for _, candidate := range cohort.ItemIndexes {
			if candidate == itemIndex {
				return len(cohort.ItemIndexes)
			}
		}
	}
	return 1
}

func hasSeriesComponents(components medianame.Components) bool {
	return components.SeasonExplicit || components.Season > 0 || medianame.ReliableEpisodeEvidence(components)
}

func inferTaskSeries(task download.Task, files []download.File) (string, int) {
	return inferTaskSeriesParsed(task, files, parseMigrationName(task.Name))
}

func inferTaskSeriesParsed(task download.Task, files []download.File, parsed medianame.Parsed) (string, int) {
	// Season identity comes only from media names. Ordinary Season XX directory
	// names describe storage topology and must never alter parsed identity.
	season := parsed.Season
	seasonKnown := parsed.SeasonExplicit || season > 0
	special := migrationSpecialPathRE.MatchString(task.Name) || migrationSpecialPathRE.MatchString(task.SavePath)
	for _, file := range files {
		special = special || migrationSpecialPathRE.MatchString(file.Path)
	}
	if special {
		seasonKnown = true
	}
	if season == 0 && !seasonKnown {
		season = 1
	}
	return strings.TrimSpace(parsed.Title), season
}

// PresentationIdentity is the single fallback identity rule used by both scan
// results and API grouping when no Entry has been matched yet.
func PresentationIdentity(taskName, savePath string) (string, int) {
	return inferTaskSeries(download.Task{Name: taskName, SavePath: savePath}, nil)
}

func parseMigrationName(name string) medianame.Parsed {
	return medianame.Parse(name)
}

func taskBelongsToSource(task download.Task, files []download.File, source string) bool {
	if pathWithin(source, task.SavePath) {
		return true
	}
	for _, file := range files {
		if file.Wanted && pathWithin(source, file.Path) {
			return true
		}
	}
	return false
}

func pathWithin(root, path string) bool {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (e Engine) matchTask(ctx context.Context, client string, observation migrationObservation, names naming.Index) Plan {
	task, media := observation.task, observation.media
	if len(media) == 0 {
		return Plan{Client: client, TaskID: task.ID, Decision: DecisionSkip, Detail: "no completed wanted media files"}
	}
	ident, dl, err := taskIdentity(task)
	if err != nil {
		return Plan{Client: client, TaskID: task.ID, TaskName: task.Name, SavePath: task.SavePath, InfoHash: strings.ToLower(task.InfoHash), Decision: DecisionUnknown, Detail: err.Error()}
	}
	base := Plan{Client: client, TaskID: task.ID, TaskName: task.Name, SavePath: task.SavePath, InfoHash: strings.ToLower(task.InfoHash), AcquisitionID: ident.ID}
	result := func(decision Decision, detail string) Plan {
		p := base
		p.Decision = decision
		p.Detail = detail
		return p
	}
	var matches []candidateMatch
	var movieBlock string
	for _, w := range e.Bundle.Entries {
		if !taskMayMatchEntry(observation, w, names[w.Key]) {
			continue
		}
		if w.MediaType == catalog.MediaMovie {
			if m, ok, detail := e.matchesMovie(ctx, observation, w); ok {
				matches = append(matches, m)
			} else if detail != "" {
				movieBlock = detail
			}
			continue
		}
		seasons := catalog.SourceSeasons(w)
		for _, season := range seasons {
			m, ok := e.matchesWork(ctx, observation, w, names[w.Key], season)
			if ok {
				matches = append(matches, m)
			}
		}
	}
	if len(matches) == 0 {
		if movieBlock != "" {
			return result(DecisionConflict, movieBlock)
		}
		return result(DecisionUnknown, "no unique Entry/season match from task/file topology and episode evidence")
	}
	// Deduplicate identical semantic matches reached through repeated season evidence.
	uniq := map[string]candidateMatch{}
	for _, m := range matches {
		k := fmt.Sprintf("%s/%s/%d/%d/%d/%d/%d", m.w.Key, m.mediaType, m.sourceSeason, m.sourceStart, m.sourceEnd, m.targetSeason, m.targetStart)
		uniq[k] = m
	}
	matches = matches[:0]
	for _, m := range uniq {
		matches = append(matches, m)
	}
	if len(matches) != 1 {
		return result(DecisionConflict, fmt.Sprintf("task matches %d Entry/season interpretations", len(matches)))
	}
	m := matches[0]
	contentRoot, known := taskContentRoot(task, observation.files)
	if !known {
		return result(DecisionUnknown, "cannot determine downloader content root")
	}
	paths := filePaths(media)
	topology := e.requiredTopology(m.w, contentRoot, paths)
	if topology.Valid {
		return result(DecisionSkip, "task already resides in the matched Entry source namespace")
	}
	decision := DecisionRelocate
	safe, known, detail := relocationDestinationSafe(task, observation.files, m.desired, mappingsForClient(e.Bundle, client))
	if !known {
		return result(DecisionUnknown, detail)
	}
	if !safe {
		return result(DecisionConflict, detail)
	}
	if m.desiredContentRoot == "" || !e.requiredTopology(m.w, m.desiredContentRoot, pathsAtRelocatedRoot(task, observation.files, m.desired)).Valid {
		return result(DecisionConflict, "relocation cannot produce the required Entry source namespace: "+topology.Detail)
	}
	_ = dl
	base.EntryKey = m.w.Key
	base.MediaType = m.mediaType
	base.Decision = decision
	base.DesiredPath = m.desired
	base.ExpectedContentRoot = m.desiredContentRoot
	base.ObservedContentRoot = contentRoot
	base.ExpectedPaths = filePaths(observation.files)
	base.SourceEpisode = Target{EntryKey: m.w.Key, Season: m.sourceSeason, EpisodeStart: m.sourceStart, EpisodeEnd: m.sourceEnd}
	base.Target = Target{EntryKey: m.w.Key, Season: m.targetSeason, EpisodeStart: m.targetStart, EpisodeEnd: m.targetEnd}
	base.Evidence = append(m.evidence, topology.Detail)
	return base
}

func taskMayMatchEntry(observation migrationObservation, w catalog.Entry, names naming.EntryEvidence) bool {
	analysis := observation.assetAnalysis
	if w.MediaType != catalog.MediaMovie {
		analysis = observation.fileAnalysis
	}
	if naming.Match(w, names, observation.taskName.Components.Title, observation.taskName.Components.Year) {
		return true
	}
	for _, file := range analysis.Items {
		if naming.Match(w, names, file.Components.Title, file.Components.Year) {
			return true
		}
	}
	return false
}

func (e Engine) matchesWork(ctx context.Context, observation migrationObservation, w catalog.Entry, names naming.EntryEvidence, season int) (match candidateMatch, ok bool) {
	return e.validateSeriesMatch(ctx, observation, w, names, season, true)
}

func (e Engine) validateSeriesMatch(ctx context.Context, observation migrationObservation, w catalog.Entry, names naming.EntryEvidence, season int, requireTitle bool) (match candidateMatch, ok bool) {
	task, files := observation.task, observation.media
	contentRoot, known := taskContentRoot(task, observation.files)
	if !known {
		return match, false
	}
	sourceStart, sourceEnd := 0, 0
	covered := map[int]bool{}
	for i := range files {
		p := observation.fileAnalysis.Items[i].Parsed
		if p.EpisodeStart <= 0 {
			p = parseTaskFile(observation.taskName, observation.fileAnalysis.Items[i], season)
		}
		if p.EpisodeStart <= 0 {
			return match, false
		}
		ps := p.Season
		if ps == 0 {
			ps = season
		}
		if ps != season {
			return match, false
		}
		if requireTitle && !naming.Match(w, names, p.Title, p.Year) && !naming.Match(w, names, observation.taskName.Components.Title, observation.taskName.Components.Year) {
			return match, false
		}
		if sourceStart == 0 || p.EpisodeStart < sourceStart {
			sourceStart = p.EpisodeStart
		}
		if p.EpisodeEnd > sourceEnd {
			sourceEnd = p.EpisodeEnd
		}
		for ep := p.EpisodeStart; ep <= p.EpisodeEnd; ep++ {
			covered[ep] = true
		}
	}
	if sourceStart <= 0 || sourceEnd < sourceStart {
		return match, false
	}
	ts, tstart := 0, 0
	for ep := sourceStart; ep <= sourceEnd; ep++ {
		if !covered[ep] {
			return match, false
		}
		seasonOut, episodeOut := season, ep
		if ep == sourceStart {
			ts, tstart = seasonOut, episodeOut
			continue
		}
		if seasonOut != ts || episodeOut != tstart+(ep-sourceStart) {
			return match, false
		}
	}
	te, tend := season, sourceEnd
	if ts != te {
		return match, false
	}
	cfg, err := e.Bundle.OrganizerForPublication(w.Key, season, ts, contentRoot)
	if err != nil {
		return match, false
	}
	// Zero-move publication must be possible from the observed task root. Use the same organizer parser,
	// but never mutate here.
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	plan, err := organizer.BuildPlanForAnalyzedFiles(ctx, cfg, paths, observation.fileAnalysis.Items)
	if err != nil || len(plan.Items) != len(files) {
		return match, false
	}
	for _, it := range plan.Items {
		if it.Action == organizer.ActionConflict || it.Action == organizer.ActionUnmatched {
			return match, false
		}
	}
	desired := e.relocationSavePath(w, task, contentRoot, season, ts)
	desiredContentRoot := relocatedContentRoot(task, contentRoot, desired)
	if desiredContentRoot == "" {
		return match, false
	}
	return candidateMatch{w: w, mediaType: catalog.MediaSeries, sourceSeason: season, sourceStart: sourceStart, sourceEnd: sourceEnd, targetSeason: ts, targetStart: tstart, targetEnd: tend, desired: desired, desiredContentRoot: desiredContentRoot, evidence: []string{"unique filesystem-derived Entry name match", "all completed wanted media files parse in one source season", "organizer plan has no unmatched/conflict"}}, true
}

// PlanConfirmed re-observes one stable downloader identity and validates a
// user-confirmed Entry without consulting the generic Entry matcher.
func (e Engine) PlanConfirmed(ctx context.Context, intent ConfirmedIntent) (Plan, error) {
	w, ok := e.Bundle.Entries[intent.EntryKey]
	if !ok {
		return Plan{}, fmt.Errorf("confirmed Entry %q not found", intent.EntryKey)
	}
	backend, ok := e.Clients[intent.Client]
	if !ok {
		return Plan{}, fmt.Errorf("migration client %q unavailable", intent.Client)
	}
	ob, ok := backend.(observer)
	if !ok {
		return Plan{}, errors.New("migration backend cannot observe tasks/files")
	}
	task, files, err := observeOne(ctx, ob, intent.TaskID)
	if err != nil {
		return Plan{}, fmt.Errorf("observe confirmed migration task: %w", err)
	}
	task, files = mapTaskFiles(task, files, mappingsForClient(e.Bundle, intent.Client))
	if intent.InfoHash != "" && task.InfoHash != "" && !strings.EqualFold(intent.InfoHash, task.InfoHash) {
		return Plan{}, fmt.Errorf("task infohash changed since confirmation: got %s", task.InfoHash)
	}
	media := presentWantedAssetsForType(files, e.Bundle.Organizer.Extensions, w.MediaType)
	if len(media) == 0 {
		return Plan{}, errors.New("no completed wanted assets for confirmed media type")
	}
	obs := migrationObservation{task: task, files: files, media: media, taskName: medianame.AnalyzeMany([]string{task.Name}).Items[0]}
	names := make([]string, len(media))
	for i := range media {
		names[i] = filepath.Base(media[i].Path)
	}
	obs.fileAnalysis = medianame.AnalyzeFiles(names)
	obs.assetAnalysis = obs.fileAnalysis
	var match candidateMatch
	if w.MediaType == catalog.MediaMovie {
		var valid bool
		var detail string
		match, valid, detail = e.validateMovieMatch(ctx, obs, w, false)
		if !valid {
			if detail == "" {
				detail = "confirmed movie publication is unsafe"
			}
			return Plan{}, errors.New(detail)
		}
	} else {
		var valid bool
		match, valid = e.validateSeriesMatch(ctx, obs, w, naming.EntryEvidence{}, intent.SourceSeason, false)
		if !valid {
			return Plan{}, fmt.Errorf("confirmed series files do not uniquely resolve in source season %d or publication is unsafe", intent.SourceSeason)
		}
	}
	contentRoot, known := taskContentRoot(task, files)
	if !known {
		return Plan{}, errors.New("cannot determine downloader content root")
	}
	topology := e.requiredTopology(w, contentRoot, filePaths(media))
	decision := DecisionAdopt
	if !topology.Valid {
		decision = DecisionRelocate
		safe, known, detail := relocationDestinationSafe(task, files, match.desired, mappingsForClient(e.Bundle, intent.Client))
		if !known {
			return Plan{}, errors.New(detail)
		}
		if !safe {
			return Plan{}, errors.New(detail)
		}
		if match.desiredContentRoot == "" || !e.requiredTopology(w, match.desiredContentRoot, pathsAtRelocatedRoot(task, files, match.desired)).Valid {
			return Plan{}, fmt.Errorf("relocation cannot produce required Entry source namespace: %s", topology.Detail)
		}
	}
	ident, _, err := taskIdentity(task)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Client: intent.Client, TaskID: task.ID, TaskName: task.Name, SavePath: task.SavePath, InfoHash: strings.ToLower(task.InfoHash), EntryKey: w.Key, MediaType: w.MediaType, Season: match.sourceSeason, SeasonKnown: true, Decision: decision, DesiredPath: match.desired, ExpectedContentRoot: match.desiredContentRoot, ObservedContentRoot: contentRoot, ExpectedPaths: filePaths(files), SourceEpisode: Target{EntryKey: w.Key, Season: match.sourceSeason, EpisodeStart: match.sourceStart, EpisodeEnd: match.sourceEnd}, Target: Target{EntryKey: w.Key, Season: match.targetSeason, EpisodeStart: match.targetStart, EpisodeEnd: match.targetEnd}, AcquisitionID: ident.ID, Evidence: []string{"user-confirmed Entry identity", "current task observation", "safe topology and publication plan"}, FilterOptions: migrationFilterOptions(obs)}, nil
}

func (e Engine) matchesMovie(ctx context.Context, observation migrationObservation, w catalog.Entry) (candidateMatch, bool, string) {
	return e.validateMovieMatch(ctx, observation, w, true)
}

func (e Engine) validateMovieMatch(ctx context.Context, observation migrationObservation, w catalog.Entry, requireObservedYear bool) (candidateMatch, bool, string) {
	task, files := observation.task, observation.media
	contentRoot, known := taskContentRoot(task, observation.files)
	if !known {
		return candidateMatch{}, false, "cannot determine downloader content root"
	}
	parsed := observation.taskName.Parsed
	if requireObservedYear && parsed.Year != 0 && w.Year != 0 && parsed.Year != w.Year {
		return candidateMatch{}, false, "movie year conflicts with the selected Entry"
	}
	cfg, err := e.Bundle.OrganizerForPublication(w.Key, 0, 0, contentRoot)
	if err != nil {
		return candidateMatch{}, false, err.Error()
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	plan, err := organizer.BuildPlanForAnalyzedFiles(ctx, cfg, paths, observation.fileAnalysis.Items)
	if err != nil {
		return candidateMatch{}, false, "movie publication plan could not be built: " + err.Error()
	}
	for _, item := range plan.Items {
		if item.Action == organizer.ActionConflict || item.Action == organizer.ActionUnmatched {
			detail := strings.TrimSpace(item.Detail)
			if detail == "" {
				detail = "unclassified movie asset " + item.Relative
			}
			return candidateMatch{}, false, "movie bundle requires classification: " + detail
		}
	}
	if len(plan.Items) != len(files) {
		return candidateMatch{}, false, fmt.Sprintf("movie publication plan covered %d of %d observed assets", len(plan.Items), len(files))
	}
	desired := e.relocationSavePath(w, task, contentRoot, 0, 0)
	desiredContentRoot := relocatedContentRoot(task, contentRoot, desired)
	if desiredContentRoot == "" {
		return candidateMatch{}, false, "cannot derive a safe movie relocation root"
	}
	evidence := []string{"organizer plan has no unmatched/conflict"}
	if requireObservedYear {
		evidence = append([]string{"unique filesystem-derived movie name match", "movie year does not conflict"}, evidence...)
	}
	return candidateMatch{w: w, mediaType: catalog.MediaMovie, desired: desired, desiredContentRoot: desiredContentRoot, evidence: evidence}, true, ""
}

// taskClearlyManagedFromContentPath is a cheap read-only fast path for download
// clients that expose ContentPath in their bulk task listing. The path must be
// verified on the live filesystem before it can stand in for the more expensive
// per-task file observation. This only proves that migration has no namespace
// work to do; publication and downloader ownership are still reconstructed by
// normal reconciliation.
func (e Engine) taskClearlyManagedFromContentPath(task download.Task) bool {
	contentPath := strings.TrimSpace(task.ContentPath)
	if contentPath == "" {
		return false
	}
	// Check every directory inside the configured source before inspecting the
	// leaf: Lstat alone still follows symbolic links in parent components.
	root := filepath.Clean(e.Bundle.Organizer.Source)
	rel, err := filepath.Rel(root, filepath.Clean(contentPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parent := root
	if _, err := filesystem.ObserveDirectory(parent); err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		if _, err := filesystem.ObserveDirectory(parent); err != nil {
			return false
		}
	}
	info, err := os.Lstat(contentPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	contentRoot := filepath.Clean(contentPath)
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		contentRoot = filepath.Dir(contentRoot)
	default:
		return false
	}
	for _, w := range e.Bundle.Entries {
		if strings.TrimSpace(w.DeclarationPath) == "" {
			continue
		}
		if w.MediaType == catalog.MediaMovie {
			if sameCleanPath(contentRoot, w.DeclarationPath) && source.ValidateMovieRoot(e.Bundle.Organizer.MovieSource(), contentRoot).Valid {
				return true
			}
			continue
		}
		if source.ValidateSeriesRoot(w.DeclarationPath, contentRoot).Valid {
			return true
		}
	}
	return false
}

// taskAlreadyManaged treats placement inside a durable Entry namespace as
// authoritative ownership evidence. Publication state is intentionally absent:
// ordinary reconciliation owns publication and may still report its own local
// conflict/unknown state.
func (e Engine) taskAlreadyManaged(task download.Task, files []download.File) bool {
	contentRoot, known := taskContentRoot(task, files)
	if !known {
		return false
	}
	paths := filePaths(files)
	if len(paths) == 0 {
		return false
	}
	for _, w := range e.Bundle.Entries {
		if strings.TrimSpace(w.DeclarationPath) == "" {
			continue
		}
		if e.entryTopology(w, contentRoot, paths).Valid {
			return true
		}
	}
	return false
}

// requiredTopology pins an existing Entry to its durable source declaration
// root. A transient Entry created only for a user-confirmed migration has no
// declaration path yet and may instead adopt any structurally valid source
// container; Apply will persist its declaration at that observed ownership root.
func (e Engine) requiredTopology(w catalog.Entry, contentRoot string, paths []string) source.Topology {
	if strings.TrimSpace(w.DeclarationPath) == "" {
		return e.topology(w, contentRoot, paths)
	}
	return e.entryTopology(w, contentRoot, paths)
}

func (e Engine) entryTopology(w catalog.Entry, contentRoot string, paths []string) source.Topology {
	if strings.TrimSpace(w.DeclarationPath) == "" {
		return source.Topology{Detail: "Entry has no durable source namespace"}
	}
	if w.MediaType == catalog.MediaMovie {
		if !sameCleanPath(contentRoot, w.DeclarationPath) {
			return source.Topology{Detail: fmt.Sprintf("movie content root %q is outside Entry source %q", contentRoot, w.DeclarationPath)}
		}
		return source.ValidateMovie(e.Bundle.Organizer.MovieSource(), contentRoot, paths)
	}
	topology := source.ValidateSeries(w.DeclarationPath, contentRoot, paths)
	if !topology.Valid && topology.Detail == "series content root is outside the configured source" {
		topology.Detail = fmt.Sprintf("series content root %q is outside Entry source %q", contentRoot, w.DeclarationPath)
	}
	return topology
}

// taskContentRoot returns the directory organizer may safely use as the task
// bundle root. content_path is authoritative. Without it, SavePath is accepted
// only for a flat observed layout; a nested common parent is not guessed.
func taskContentRoot(task download.Task, files []download.File) (string, bool) {
	observation, err := source.Observe(task, files, nil)
	if err != nil {
		return "", false
	}
	return observation.ContentRoot, true
}

func relocatedContentRoot(task download.Task, contentRoot, destination string) string {
	rel, err := filepath.Rel(filepath.Clean(task.SavePath), filepath.Clean(contentRoot))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.Join(destination, rel)
}

func (e Engine) entrySourceRoot(w catalog.Entry) string {
	if w.DeclarationPath != "" {
		return w.DeclarationPath
	}
	_, name, err := catalog.SplitKey(w.Key)
	if err != nil {
		return ""
	}
	return filepath.Join(e.Bundle.Organizer.SeriesSource(), name)
}

func entryName(w catalog.Entry) string {
	_, name, err := catalog.SplitKey(w.Key)
	if err != nil {
		return filepath.Base(w.DeclarationPath)
	}
	return name
}

func (e Engine) relocationSavePath(w catalog.Entry, task download.Task, contentRoot string, sourceSeason, targetSeason int) string {
	contentIsSave := sameCleanPath(task.SavePath, contentRoot)
	if w.MediaType == catalog.MediaMovie {
		if !contentIsSave {
			return e.Bundle.Organizer.MovieSource()
		}
		placement, err := source.PlacementFor(source.Movie, source.SingleFile, "", e.Bundle.Organizer.MovieSource(), entryName(w))
		if err != nil {
			return ""
		}
		return placement.SavePath
	}
	if !contentIsSave {
		return e.entrySourceRoot(w)
	}
	placement, err := source.PlacementFor(source.Series, source.SingleFile, e.entrySourceRoot(w), "", fmt.Sprintf("Season %02d", sourceSeason))
	if err != nil {
		return ""
	}
	return placement.SavePath
}

func (e Engine) topology(w catalog.Entry, contentRoot string, paths []string) source.Topology {
	if w.MediaType == catalog.MediaMovie {
		return source.ValidateMovie(e.Bundle.Organizer.MovieSource(), contentRoot, paths)
	}
	return source.ValidateSeriesAtSource(e.Bundle.Organizer.SeriesSource(), contentRoot, paths)
}

func filePaths(files []download.File) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if file.Wanted {
			paths = append(paths, file.Path)
		}
	}
	return paths
}

func pathsAtRelocatedRoot(task download.Task, files []download.File, destination string) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if !file.Wanted {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(task.SavePath), filepath.Clean(file.Path))
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			paths = append(paths, filepath.Join(destination, rel))
		}
	}
	return paths
}

func (e Engine) Apply(ctx context.Context, requested Plan) (result Result, finalErr error) {
	if (requested.Decision != DecisionRelocate && requested.Decision != DecisionAdopt) || requested.Client == "" || requested.TaskID == "" || requested.EntryKey == "" {
		return Result{Plan: requested}, errors.New("only an unambiguous current adopt or relocate plan can be applied")
	}
	if err := filesystem.EnsureHardlinkDomain(e.Bundle.Organizer.Source, e.Bundle.Organizer.Target, 0o755); err != nil {
		return Result{Plan: requested}, fmt.Errorf("prepare publication hardlink domain: %w", err)
	}
	backend, ok := e.Clients[requested.Client]
	if !ok {
		return Result{Plan: requested}, fmt.Errorf("migration client %q unavailable", requested.Client)
	}
	ob, ok := backend.(observer)
	if !ok {
		return Result{Plan: requested}, errors.New("migration backend cannot observe tasks/files")
	}
	task, files, err := observeOne(ctx, ob, requested.TaskID)
	if err != nil {
		return Result{Plan: requested}, fmt.Errorf("observe migration task: %w", err)
	}
	maps := mappingsForClient(e.Bundle, requested.Client)
	task, files = mapTaskFiles(task, files, maps)
	if requested.InfoHash != "" && task.InfoHash != "" && !strings.EqualFold(requested.InfoHash, task.InfoHash) {
		return Result{Plan: requested}, errors.New("task infohash changed since confirmation")
	}
	initialObservation, observeErr := source.Observe(task, files, nil)
	if observeErr != nil {
		return Result{Plan: requested}, fmt.Errorf("observe confirmed source: %w", observeErr)
	}
	alreadyRelocated := requested.ExpectedContentRoot != "" && sameCleanPath(initialObservation.ContentRoot, requested.ExpectedContentRoot) && sameCleanPath(task.SavePath, requested.DesiredPath)
	if requested.ObservedContentRoot != "" && !sameCleanPath(initialObservation.ContentRoot, requested.ObservedContentRoot) && !alreadyRelocated {
		return Result{Plan: requested}, fmt.Errorf("task content root changed since confirmation: got %q want %q", initialObservation.ContentRoot, requested.ObservedContentRoot)
	}
	expectedPaths := requested.ExpectedPaths
	if alreadyRelocated {
		expectedPaths = pathsAtRelocatedRoot(download.Task{SavePath: requested.SavePath}, filesFromPaths(requested.ExpectedPaths), requested.DesiredPath)
	}
	if len(expectedPaths) > 0 && !samePathSet(filePaths(files), expectedPaths) {
		return Result{Plan: requested}, errors.New("wanted file set changed since confirmation")
	}
	media := presentWantedAssetsForType(files, e.Bundle.Organizer.Extensions, requested.MediaType)
	if len(media) == 0 {
		return Result{Plan: requested}, errors.New("no completed wanted assets for confirmed media type")
	}
	fresh := migrationObservation{task: task, files: files, media: media, taskName: medianame.AnalyzeMany([]string{task.Name}).Items[0]}
	names := make([]string, len(media))
	for i := range media {
		names[i] = filepath.Base(media[i].Path)
	}
	fresh.fileAnalysis = medianame.AnalyzeFiles(names)
	fresh.assetAnalysis = fresh.fileAnalysis
	current := requested
	current.TaskName, current.SavePath, current.Title, current.Season = task.Name, task.SavePath, requested.Title, requested.Season
	beforeRelocate, identityErr := filesystem.ObservePaths(filePaths(media))
	if identityErr != nil {
		return Result{Plan: current}, fmt.Errorf("observe physical objects before migration: %w", identityErr)
	}
	didRelocate := false
	pausedByUs := false
	defer func() {
		if !pausedByUs {
			return
		}
		r, ok := backend.(resumer)
		if !ok {
			finalErr = errors.Join(finalErr, errors.New("resume task after migration failure: backend does not support resume"))
			return
		}
		resumeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Resume(resumeCtx, task.ID); err != nil {
			finalErr = errors.Join(finalErr, fmt.Errorf("resume task after migration failure: %w", err))
		}
	}()
	if current.Decision == DecisionRelocate && !sameCleanPath(task.SavePath, current.DesiredPath) {
		if task.State != download.StatePaused {
			p, ok := backend.(pauser)
			if !ok {
				return Result{Plan: current}, download.ErrUnsupported
			}
			if err := p.Pause(ctx, task.ID); err != nil {
				return Result{Plan: current}, err
			}
			pausedByUs = true
			if err := waitForTask(ctx, ob, task.ID, func(t download.Task) bool { return t.State == download.StatePaused }); err != nil {
				return Result{Plan: current}, err
			}
		}
		rel, ok := backend.(download.Relocator)
		if !ok {
			return Result{Plan: current}, fmt.Errorf("managed relocation: %w", download.ErrUnsupported)
		}
		remoteDestination := download.UnmapPath(current.DesiredPath, maps)
		if err := rel.Relocate(ctx, task.ID, remoteDestination); err != nil {
			// An RPC may fail after the downloader accepted it; observation below is authoritative.
			if observed, oe := observeTask(ctx, ob, task.ID); oe != nil || !sameCleanPath(download.MapPath(observed.SavePath, maps), current.DesiredPath) {
				return Result{Plan: current}, err
			}
		}
		if err := waitForTask(ctx, ob, task.ID, func(t download.Task) bool {
			return sameCleanPath(download.MapPath(t.SavePath, maps), current.DesiredPath)
		}); err != nil {
			return Result{Plan: current}, err
		}
		didRelocate = true
	}
	task, err = observeTask(ctx, ob, task.ID)
	if err != nil {
		return Result{Plan: current}, err
	}
	files, err = ob.Files(ctx, task.ID)
	if err != nil {
		return Result{Plan: current}, err
	}
	task, files = mapTaskFiles(task, files, maps)
	media = presentWantedAssetsForType(files, e.Bundle.Organizer.Extensions, current.MediaType)
	if len(media) == 0 {
		return Result{Plan: current}, errors.New("no completed wanted assets for confirmed media type")
	}
	if didRelocate {
		afterRelocate, observeErr := filesystem.ObservePaths(filePaths(media))
		if observeErr != nil {
			return Result{Plan: current}, fmt.Errorf("observe physical objects after migration: %w", observeErr)
		}
		if !sameObjectSet(beforeRelocate, afterRelocate) {
			return Result{Plan: current}, errors.New("downloader relocation replaced physical filesystem objects; hardlink-safe migration requires namespace relocation of the same inodes")
		}
	}
	contentRoot, known := taskContentRoot(task, files)
	if !known {
		return Result{Plan: current}, errors.New("cannot determine task content root after relocation")
	}
	if current.Decision == DecisionRelocate && current.ExpectedContentRoot != "" && !sameCleanPath(contentRoot, current.ExpectedContentRoot) {
		return Result{Plan: current}, fmt.Errorf("relocated task content root %q does not match expected topology %q", contentRoot, current.ExpectedContentRoot)
	}
	w, ok := e.Bundle.Entries[current.EntryKey]
	if !ok {
		return Result{Plan: current}, errors.New("migration Entry disappeared")
	}
	if topology := e.requiredTopology(w, contentRoot, filePaths(media)); !topology.Valid {
		return Result{Plan: current}, fmt.Errorf("source topology invalid after observation: %s", topology.Detail)
	}
	if requested.InfoHash != "" && task.InfoHash != "" && !strings.EqualFold(requested.InfoHash, task.InfoHash) {
		return Result{Plan: current}, errors.New("task identity changed after relocation")
	}
	org, organizeErr := e.organizeObserved(ctx, current, contentRoot, media)
	if organizeErr != nil {
		return Result{Plan: current, Organized: org, Error: organizeErr.Error()}, organizeErr
	}
	var declared *catalog.Declaration
	if w.DeclarationPath != "" {
		var readErr error
		declared, readErr = catalog.ReadDeclaration(w.DeclarationPath)
		if readErr != nil {
			return Result{Plan: current, Organized: org, Error: readErr.Error()}, fmt.Errorf("read Entry declaration: %w", readErr)
		}
	}
	declarationRoot := e.sourceDeclarationRoot(w.MediaType, contentRoot)
	if declarationRoot == "" {
		return Result{Plan: current, Organized: org}, errors.New("cannot determine declaration root for confirmed source")
	}
	if declared != nil && !sameCleanPath(w.DeclarationPath, declarationRoot) {
		return Result{Plan: current, Organized: org}, fmt.Errorf("existing Entry source namespace changed during migration: got %q want %q", declarationRoot, w.DeclarationPath)
	}
	if declared == nil {
		// A newly confirmed Entry has no durable declaration until the real
		// downloader source is safely in place. Persist at the physical ownership
		// root derived from topology, never in a canonical-name placeholder.
		side := catalog.DeclarationFromEntry(w)
		if err := catalog.WriteDeclaration(declarationRoot, side); err != nil {
			return Result{Plan: current, Organized: org, Error: err.Error()}, fmt.Errorf("persist confirmed Entry declaration: %w", err)
		}
		w.DeclarationPath = declarationRoot
		declared = &side
	}
	declarationDirty := false
	if declared != nil && !declared.Filters.Equal(w.Filters) {
		declared.Filters = w.Filters.Normalize()
		declarationDirty = true
	}
	if w.MediaType == catalog.MediaSeries && declared != nil {
		// A source directory name is never parsed into season/episode identity.
		// When migration has explicit source->target semantics, persist that
		// projection against the real first-level source container so a restart
		// does not need to rediscover meaning from a directory name.
		if folder, ok := firstSeriesContainer(declarationRoot, contentRoot); ok {
			if declared.Folders == nil {
				declared.Folders = map[string]catalog.FolderProjection{}
			}
			offset := 0
			if current.SourceEpisode.EpisodeStart > 0 && current.Target.EpisodeStart > 0 {
				offset = current.Target.EpisodeStart - current.SourceEpisode.EpisodeStart
			}
			rule := catalog.FolderProjection{Season: current.Target.Season, EpisodeOffset: offset}
			if declared.Folders[folder] != rule {
				declared.Folders[folder] = rule
				declarationDirty = true
			}
		}
	}
	if declarationDirty {
		if err := catalog.WriteDeclaration(declarationRoot, *declared); err != nil {
			return Result{Plan: current, Organized: org, Error: err.Error()}, fmt.Errorf("persist confirmed Entry declaration: %w", err)
		}
	}
	if pausedByUs {
		r, ok := backend.(resumer)
		if !ok {
			return Result{Plan: current, Organized: org}, download.ErrUnsupported
		}
		if err := r.Resume(ctx, task.ID); err != nil {
			return Result{Plan: current, Organized: org}, err
		}
		pausedByUs = false
	}
	return Result{Plan: current, Organized: org}, nil
}

func sameObjectSet(a, b filesystem.Snapshot) bool {
	if len(a.Objects) != len(b.Objects) {
		return false
	}
	for id := range a.Objects {
		if _, ok := b.Objects[id]; !ok {
			return false
		}
	}
	return true
}

func (e Engine) organizeObserved(ctx context.Context, p Plan, contentRoot string, files []download.File) (organizer.Result, error) {
	cfg, err := e.Bundle.OrganizerForPublication(p.EntryKey, p.SourceEpisode.Season, p.Target.Season, contentRoot)
	if err != nil {
		return organizer.Result{}, err
	}
	paths := []string{}
	for _, f := range files {
		if f.Wanted {
			paths = append(paths, f.Path)
		}
	}
	plan, pe := organizer.BuildPlanForFiles(ctx, cfg, paths)
	if pe != nil {
		return organizer.Result{}, pe
	}
	library, observeErr := filesystem.ObserveTreeIfPresent(e.Bundle.Organizer.Target)
	if observeErr != nil {
		return organizer.Result{}, fmt.Errorf("observe library before publication convergence: %w", observeErr)
	}
	res, ae := organizer.ApplyPlanWithLibrarySnapshot(ctx, cfg, plan, library)
	return res, ae
}

func firstSeriesContainer(entryRoot, contentRoot string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(entryRoot), filepath.Clean(contentRoot))
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." {
		return "", false
	}
	return parts[0], true
}

func (e Engine) sourceDeclarationRoot(mediaType, contentRoot string) string {
	contentRoot = filepath.Clean(contentRoot)
	switch mediaType {
	case catalog.MediaMovie:
		return contentRoot
	case catalog.MediaSeries:
		seriesRoot := filepath.Clean(e.Bundle.Organizer.SeriesSource())
		rel, err := filepath.Rel(seriesRoot, contentRoot)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			return ""
		}
		return filepath.Join(seriesRoot, filepath.FromSlash(parts[0]))
	default:
		return ""
	}
}

func samePathSet(a, b []string) bool {
	clean := func(values []string) []string {
		out := make([]string, len(values))
		for i := range values {
			out[i] = filepath.Clean(values[i])
		}
		sort.Strings(out)
		return out
	}
	a, b = clean(a), clean(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func filesFromPaths(paths []string) []download.File {
	out := make([]download.File, len(paths))
	for i := range paths {
		out[i] = download.File{Path: paths[i], Wanted: true}
	}
	return out
}

func presentWantedSeriesMedia(files []download.File, exts []string) []download.File {
	set := mediafile.ExtensionSet(exts)
	var out []download.File
	for _, f := range files {
		if !f.Wanted || !mediafile.HasAllowedExtension(f.Path, set) {
			continue
		}
		info, err := os.Lstat(f.Path)
		if err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && (f.Size <= 0 || info.Size() == f.Size) {
			out = append(out, f)
		}
	}
	return out
}

func presentWantedMovieAssets(files []download.File) []download.File {
	var out []download.File
	for _, f := range files {
		if !f.Wanted {
			continue
		}
		info, err := os.Lstat(f.Path)
		if err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && (f.Size <= 0 || info.Size() == f.Size) {
			out = append(out, f)
		}
	}
	return out
}

func presentWantedAssetsForType(files []download.File, exts []string, mediaType string) []download.File {
	if mediaType == catalog.MediaMovie {
		return presentWantedMovieAssets(files)
	}
	return presentWantedSeriesMedia(files, exts)
}

func parseTaskFile(taskName, fileName medianame.Name, fallbackSeason int) medianame.Parsed {
	p := fileName.Parsed
	if p.EpisodeStart > 0 {
		return p
	}
	stem := strings.TrimSuffix(filepath.Base(fileName.Input), filepath.Ext(fileName.Input))
	ep, err := strconv.Atoi(stem)
	if err != nil || ep <= 0 {
		return p
	}
	return medianame.Parsed{Title: taskName.Components.Title, Season: fallbackSeason, EpisodeStart: ep, EpisodeEnd: ep, Version: 1}
}

func relocationDestinationSafe(task download.Task, files []download.File, destination string, maps []download.PathMapping) (safe, known bool, detail string) {
	save := download.MapPath(task.SavePath, maps)
	for _, file := range files {
		if !file.Wanted {
			continue
		}
		source := download.MapPath(file.Path, maps)
		rel, err := filepath.Rel(save, source)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false, false, "cannot derive downloader file layout relative to save path"
		}
		target := filepath.Join(destination, rel)
		targetInfo, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, false, "cannot inspect relocation target " + target + ": " + err.Error()
		}
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			return false, true, "relocation target is not a regular filesystem object: " + target
		}
		sourceInfo, err := os.Lstat(source)
		if err != nil {
			return false, false, "cannot inspect relocation source " + source + ": " + err.Error()
		}
		if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
			return false, false, "relocation source is not a regular filesystem object: " + source
		}
		sourceID, sourceOK := filesystem.Identity(sourceInfo)
		targetID, targetOK := filesystem.Identity(targetInfo)
		if !sourceOK || !targetOK || sourceID != targetID {
			return false, true, "relocation target conflicts with current task file: " + target
		}
	}
	return true, true, ""
}

func mappingsForClient(b configstore.Bundle, client string) []download.PathMapping {
	var out []download.PathMapping
	for _, m := range b.Clients[client].PathMappings {
		out = append(out, download.PathMapping{Remote: m.Remote, Local: m.Local})
	}
	return out
}

func normTitle(v string) string {
	if parsed := parseMigrationName(v); parsed.Title != "" {
		v = parsed.Title
	}
	return strings.ToLower(strings.Join(strings.Fields(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(strings.TrimSpace(v))), " "))
}
func sameCleanPath(a, b string) bool {
	aa, e1 := filepath.Abs(a)
	bb, e2 := filepath.Abs(b)
	return e1 == nil && e2 == nil && filepath.Clean(aa) == filepath.Clean(bb)
}
func taskIdentity(t download.Task) (acquisition.Identity, string, error) {
	if t.InfoHash != "" {
		i, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{InfoHash: t.InfoHash})
		if err == nil {
			return i, "magnet:?xt=urn:btih:" + i.Canonical, nil
		}
	}
	var found []struct {
		i   acquisition.Identity
		url string
	}
	for _, s := range t.Sources {
		in := acquisition.IdentityInput{}
		if strings.HasPrefix(strings.ToLower(s), "magnet:") {
			in.MagnetURL = s
		} else {
			in.DownloadURL = s
		}
		if i, err := acquisition.CanonicalIdentity(in); err == nil {
			found = append(found, struct {
				i   acquisition.Identity
				url string
			}{i, s})
		}
	}
	if len(found) != 1 {
		return acquisition.Identity{}, "", errors.New("task has no unique stable infohash/source identity")
	}
	return found[0].i, found[0].url, nil
}
func observeOne(ctx context.Context, o observer, id string) (download.Task, []download.File, error) {
	t, err := observeTask(ctx, o, id)
	if err != nil {
		return download.Task{}, nil, err
	}
	fs, err := o.Files(ctx, id)
	return t, fs, err
}
func observeTask(ctx context.Context, o observer, id string) (download.Task, error) {
	return o.Get(ctx, id)
}
func waitForTask(ctx context.Context, o observer, id string, converged func(download.Task) bool) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		t, err := observeTask(ctx, o, id)
		if err != nil {
			return err
		}
		if converged(t) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
