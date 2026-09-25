package automation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aninode/internal/acquisition"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/claim"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/episode"
	"aninode/internal/filesystem"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
	"aninode/internal/observation"
	"aninode/internal/organizer"
	"aninode/internal/source"
)

type AcquireDecision string

const (
	AcquireSubmitted AcquireDecision = "submitted"
	AcquireRecovered AcquireDecision = "recovered"
	AcquireSkipped   AcquireDecision = "skipped"
	AcquireFailed    AcquireDecision = "failed"
)

type AcquireResult struct {
	AcquisitionID string          `json:"acquisition_id"`
	EntryKey      string          `json:"entry_key"`
	Client        string          `json:"client,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	Decision      AcquireDecision `json:"decision"`
	Reason        string          `json:"reason,omitempty"`
	Error         string          `json:"error,omitempty"`
}
type ReconcileResult struct {
	Client   string           `json:"client"`
	TaskID   string           `json:"task_id"`
	EntryKey string           `json:"entry_key,omitempty"`
	Decision string           `json:"decision"`
	Plan     organizer.Plan   `json:"plan,omitempty"`
	Result   organizer.Result `json:"result,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	Error    string           `json:"error,omitempty"`
}
type submitter interface {
	download.Backend
	Add(context.Context, download.AddRequest) (download.Task, error)
}
type taskLister interface {
	download.Backend
	List(context.Context) ([]download.Task, error)
}
type taskSource interface {
	taskLister
	Files(context.Context, string) ([]download.File, error)
}
type Runner struct {
	Bundle  configstore.Bundle
	Clients map[string]download.Backend
	Now     func() time.Time
}
type acquisitionIntent struct {
	ID, EntryKey, Client, URL string
	Identity                  acquisition.Identity
	Source, Target            episode.Key
	MediaType                 string
	Correlation               string
	SourceFolder              string
}
type snapshot struct {
	tasks    map[string][]download.Task
	errs     map[string]error
	files    map[string][]download.File
	fileErrs map[string]error
}

func (r Runner) observe(ctx context.Context) snapshot {
	s := snapshot{tasks: map[string][]download.Task{}, errs: map[string]error{}, files: map[string][]download.File{}, fileErrs: map[string]error{}}
	for id, b := range r.Clients {
		if batch, ok := b.(download.ObservationLister); ok {
			observations, err := batch.ListObservations(ctx)
			s.errs[id] = err
			if err != nil {
				continue
			}
			tasks := make([]download.Task, 0, len(observations))
			for _, observation := range observations {
				tasks = append(tasks, observation.Task)
				key := id + "\x00" + observation.Task.ID
				s.files[key] = observation.Files
				s.fileErrs[key] = nil
			}
			s.tasks[id] = tasks
			continue
		}
		if l, ok := b.(taskLister); ok {
			s.tasks[id], s.errs[id] = l.List(ctx)
		}
	}
	return s
}
func observeFiles(ctx context.Context, s snapshot, client string, src taskSource, task download.Task) ([]download.File, error) {
	key := client + "\x00" + task.ID
	if files, ok := s.files[key]; ok {
		return files, s.fileErrs[key]
	}
	files, err := taskFiles(ctx, src, task)
	s.files[key], s.fileErrs[key] = files, err
	return files, err
}

func taskFiles(ctx context.Context, src taskSource, task download.Task) ([]download.File, error) {
	if optimized, ok := any(src).(download.TaskFileObserver); ok {
		return optimized.FilesForTask(ctx, task)
	}
	return src.Files(ctx, task.ID)
}
func (r Runner) Acquire(ctx context.Context, p candidate.Plan, graph observation.Graph) ([]AcquireResult, error) {
	hasSelected := false
	for _, c := range p.Candidates {
		if c.Decision == candidate.DecisionSelected {
			hasSelected = true
			break
		}
	}
	if hasSelected {
		if err := filesystem.EnsureHardlinkDomain(r.Bundle.Organizer.Source, r.Bundle.Organizer.Target, 0o755); err != nil {
			return nil, fmt.Errorf("prepare publication hardlink domain: %w", err)
		}
	}
	s := r.observe(ctx)
	sourceIndex := claim.BuildSourceIndexFromGraph(graph, r.Bundle.Entries, r.Bundle.Organizer.Extensions)
	var out []AcquireResult
	var errs []error
	seenAcquisitions := map[string]bool{}
	for _, c := range p.Candidates {
		if c.Decision != candidate.DecisionSelected {
			continue
		}
		w, ok := r.Bundle.Entries[c.EntryKey]
		if !ok {
			e := fmt.Errorf("entry %s not found", c.EntryKey)
			out = append(out, failed(c, e))
			errs = append(errs, e)
			continue
		}
		client := configstore.EffectiveClient(r.Bundle)
		if c.Acquisition.ID != "" && seenAcquisitions[c.Acquisition.ID] {
			out = append(out, AcquireResult{AcquisitionID: c.Acquisition.ID, EntryKey: c.EntryKey, Client: client, Decision: AcquireSkipped, Reason: "duplicate acquisition suppressed in cycle"})
			continue
		}
		if c.Acquisition.ID != "" {
			seenAcquisitions[c.Acquisition.ID] = true
		}
		i := acquisitionIntent{
			ID: c.Acquisition.ID, EntryKey: c.EntryKey, Client: client, URL: c.Release.DownloadURL,
			Identity: c.Acquisition, Source: c.SourceEpisode, Target: c.Episode, MediaType: c.MediaType,
			Correlation: "acq:" + c.Acquisition.ID, SourceFolder: c.SourceFolder,
		}
		res, e := r.ensureOwned(ctx, i, s, sourceIndex)
		out = append(out, res)
		if e != nil {
			errs = append(errs, e)
		}
		// ensureOwned may relocate this Entry's task. Refresh only that Entry's
		// source subtree so subsequent candidates never consume stale ownership
		// facts while avoiding a full source-root rescan.
		if refreshErr := sourceIndex.RefreshEntry(w); refreshErr != nil {
			errs = append(errs, refreshErr)
		}
	}
	return out, errors.Join(errs...)
}
func failed(c candidate.Candidate, e error) AcquireResult {
	return AcquireResult{AcquisitionID: c.Acquisition.ID, EntryKey: c.EntryKey, Decision: AcquireFailed, Error: e.Error()}
}
func (r Runner) ensureOwned(ctx context.Context, i acquisitionIntent, s snapshot, sourceIndex claim.SourceIndex) (AcquireResult, error) {
	res := AcquireResult{AcquisitionID: i.ID, EntryKey: i.EntryKey, Client: i.Client}
	b, ok := r.Clients[i.Client]
	if !ok {
		return fail(res, fmt.Errorf("acquisition %s: client %q unavailable", i.ID, i.Client))
	}
	if e := s.errs[i.Client]; e != nil {
		return fail(res, fmt.Errorf("observe acquisition %s before Add: %w", i.ID, e))
	}
	match, e := uniqueMatch(i, s.tasks[i.Client])
	if e != nil {
		return fail(res, e)
	}
	if match != nil {
		if src, ok := b.(taskSource); ok {
			if err := r.ensureTaskTopology(ctx, i, b, src, *match); err != nil {
				return fail(res, err)
			}
		}
		res.TaskID, res.Decision = match.ID, AcquireSkipped
		res.Reason = "acquisition already exists in downloader"
		return res, nil
	}
	for cid, tasks := range s.tasks {
		if s.errs[cid] != nil {
			return fail(res, fmt.Errorf("observe acquisition duplicates from %s: %w", cid, s.errs[cid]))
		}
		src, ok := r.Clients[cid].(taskSource)
		if !ok {
			continue
		}
		for _, t := range tasks {
			if t.State == download.StateFailed || taskOutsideManagedSource(r.Bundle, cid, t) {
				continue
			}
			fs, err := observeFiles(ctx, s, cid, src, t)
			if err != nil {
				return fail(res, fmt.Errorf("observe target duplication from %s/%s: %w", cid, t.ID, err))
			}
			cl := claim.ResolveWithIndex(ctx, r.Bundle, sourceIndex, cid, t, fs)
			if cl.EntryKey != i.EntryKey {
				continue
			}
			if cl.Status != claim.Resolved {
				return fail(res, fmt.Errorf("target interpretation for %s is %s: %s", i.EntryKey, cl.Status, cl.Detail))
			}
			if sameEpisode(cl.TargetEpisode, i.Target) && !taskMatches(i, t) {
				res.Decision = AcquireSkipped
				res.TaskID = t.ID
				res.Reason = "target episode already has an observable acquisition"
				return res, nil
			}
		}
	}
	adder, ok := b.(submitter)
	if !ok {
		return fail(res, fmt.Errorf("acquisition %s: backend does not support acquisition", i.ID))
	}
	save, e := r.savePath(i)
	if e != nil {
		return fail(res, e)
	}
	task, addErr := adder.Add(ctx, download.AddRequest{URL: i.URL, SavePath: save, Correlation: i.Correlation})
	if addErr != nil {
		lister, ok := b.(taskLister)
		if !ok {
			return fail(res, addErr)
		}
		tasks, oe := lister.List(ctx)
		if oe == nil {
			s.tasks[i.Client] = tasks
		}
		if oe == nil {
			if found, me := uniqueMatch(i, tasks); me == nil && found != nil {
				res.Decision, res.TaskID = AcquireRecovered, found.ID
				return res, nil
			}
		}
		return fail(res, errors.Join(addErr, oe))
	}
	if task.ID == "" {
		lister, ok := b.(taskLister)
		if !ok {
			res.Decision = AcquireSubmitted
			res.Reason = "Add was accepted; backend does not expose task listing yet"
			return res, nil
		}
		tasks, oe := lister.List(ctx)
		if oe != nil {
			res.Decision = AcquireSubmitted
			res.Reason = "Add was accepted; task observation is pending: " + oe.Error()
			return res, nil
		}
		found, me := uniqueMatch(i, tasks)
		if me != nil {
			return fail(res, me)
		}
		if found == nil {
			res.Decision = AcquireSubmitted
			res.Reason = "Add was accepted; matching task is not observable yet"
			return res, nil
		}
		task = *found
	}
	if src, ok := b.(taskSource); ok {
		// Keep the eager topology repair for stable file metadata (especially
		// single-file torrents), but ensureTaskTopology treats missing wanted-file
		// metadata as a normal not-ready state rather than an acquisition failure.
		if err := r.ensureTaskTopology(ctx, i, b, src, task); err != nil {
			return fail(res, fmt.Errorf("Add accepted for %s but source topology is not established: %w", i.ID, err))
		}
	}
	s.tasks[i.Client] = append(s.tasks[i.Client], task)
	res.Decision, res.TaskID = AcquireSubmitted, task.ID
	return res, nil
}

func (r Runner) ensureTaskTopology(ctx context.Context, i acquisitionIntent, backend download.Backend, src taskSource, task download.Task) error {
	files, err := taskFiles(ctx, src, task)
	if err != nil {
		return fmt.Errorf("observe task files: %w", err)
	}
	// Magnet metadata may not exist immediately after Add. Identity recovery on
	// the next cycle re-enters this function; once any file is observable the
	// topology is mandatory and is either repaired or reported as failed.
	if len(files) == 0 {
		return nil
	}
	mappings := clientMappings(r.Bundle, i.Client)
	observation, err := source.Observe(task, files, mappings)
	if errors.Is(err, source.ErrNoWantedTaskFiles) {
		return nil
	}
	if err != nil {
		return err
	}
	w := r.Bundle.Entries[i.EntryKey]
	valid := sourceTopology(r.Bundle, w, observation)
	seasonValid := r.intentSeasonConsistent(i, w, observation.Paths)
	if valid.Valid && seasonValid {
		return nil
	}
	if valid.Valid && !seasonValid {
		return errors.New("source container media does not map to one target season")
	}
	before, err := filesystem.ObservePresentPaths(observation.Paths)
	if err != nil {
		return fmt.Errorf("observe physical objects before topology repair: %w", err)
	}
	placement, err := r.placement(i, observation.Shape)
	if err != nil {
		return err
	}
	relocator, ok := backend.(download.Relocator)
	if !ok {
		return fmt.Errorf("invalid source topology (%s) and backend cannot relocate", valid.Detail)
	}
	remoteDestination := download.UnmapPath(placement.SavePath, mappings)
	if err := relocator.Relocate(ctx, task.ID, remoteDestination); err != nil {
		return fmt.Errorf("repair source topology: %w", err)
	}
	if verifier, ok := backend.(download.Verifier); ok {
		if err := verifier.Verify(ctx, task.ID); err != nil {
			return fmt.Errorf("verify relocated task: %w", err)
		}
	}
	tasks, err := src.List(ctx)
	if err != nil {
		return fmt.Errorf("re-observe relocated task: %w", err)
	}
	var current *download.Task
	for index := range tasks {
		if tasks[index].ID == task.ID {
			current = &tasks[index]
			break
		}
	}
	if current == nil {
		return fmt.Errorf("relocated task is no longer observable")
	}
	files, err = taskFiles(ctx, src, *current)
	if err != nil {
		return fmt.Errorf("re-observe relocated files: %w", err)
	}
	observation, err = source.Observe(*current, files, mappings)
	if err != nil {
		return fmt.Errorf("re-observe relocated source: %w", err)
	}
	if topology := sourceTopology(r.Bundle, w, observation); !topology.Valid {
		return fmt.Errorf("source topology remains invalid after relocation: %s", topology.Detail)
	}
	if len(before.Objects) > 0 {
		after, observeErr := filesystem.ObservePresentPaths(observation.Paths)
		if observeErr != nil {
			return fmt.Errorf("observe physical objects after topology repair: %w", observeErr)
		}
		if !after.ContainsAll(before) {
			return errors.New("downloader topology repair replaced existing filesystem objects; hardlink-safe relocation requires preserving observed inodes")
		}
	}
	if !r.intentSeasonConsistent(i, w, observation.Paths) {
		return errors.New("relocated source container media does not map to one target season")
	}
	return nil
}

func (r Runner) intentSeasonConsistent(i acquisitionIntent, w catalog.Entry, paths []string) bool {
	if w.MediaType == catalog.MediaMovie {
		return true
	}
	exts := mediafile.ExtensionSet(r.Bundle.Organizer.Extensions)
	evidence := catalog.ObserveFolderProjectionEvidence(w, r.Bundle.Organizer.Extensions)
	seen := false
	for _, path := range paths {
		if !mediafile.HasAllowedExtension(path, exts) {
			continue
		}
		name := medianame.AnalyzeFiles([]string{filepath.Base(path)}).Items[0]
		parsed, ok := medianame.EpisodeWithSeasonHint(name, i.Source.Season)
		if !ok || parsed.EpisodeStart <= 0 {
			return false
		}
		season := name.Components.Season
		seasonKnown := name.Components.SeasonExplicit || season > 0
		if !seasonKnown {
			season = i.Source.Season
			seasonKnown = true
		}
		end := parsed.EpisodeEnd
		if end < parsed.EpisodeStart {
			end = parsed.EpisodeStart
		}
		for ep := parsed.EpisodeStart; ep <= end; ep++ {
			targetSeason, _, _, projected := catalog.ProjectObservedEpisode(w, evidence, season, seasonKnown, ep)
			if !projected || targetSeason != i.Target.Season {
				return false
			}
		}
		seen = true
	}
	return seen
}

func sourceTopology(b configstore.Bundle, w catalog.Entry, observation source.Observation) source.Topology {
	if w.MediaType == catalog.MediaMovie {
		return source.ValidateMovie(b.Organizer.MovieSource(), observation.ContentRoot, observation.Paths)
	}
	return source.ValidateSeriesAtSource(b.Organizer.SeriesSource(), observation.ContentRoot, observation.Paths)
}
func fail(r AcquireResult, e error) (AcquireResult, error) {
	r.Decision, r.Error = AcquireFailed, e.Error()
	return r, e
}
func (r Runner) savePath(i acquisitionIntent) (string, error) {
	placement, err := r.placement(i, source.MultiFile)
	if err != nil {
		return "", err
	}
	save := placement.SavePath
	if cc, ok := r.Bundle.Clients[i.Client]; ok {
		var ms []download.PathMapping
		for _, m := range cc.PathMappings {
			ms = append(ms, download.PathMapping{Remote: m.Remote, Local: m.Local})
		}
		save = download.UnmapPath(save, ms)
	}
	return save, nil
}

func (r Runner) placement(i acquisitionIntent, shape source.Shape) (source.Placement, error) {
	w, ok := r.Bundle.Entries[i.EntryKey]
	if !ok {
		return source.Placement{}, fmt.Errorf("entry %q not found", i.EntryKey)
	}
	// RSS candidates do not carry a trustworthy torrent file list. Place at
	// the parent where a multifile torrent creates exactly one content container.
	// A single-file task remains deliberately unowned until observation can move
	// it into an explicit container; guessing here can create an invalid extra
	// directory for the much more common multifile-with-sidecars case.
	media := source.Series
	entryRoot := w.DeclarationPath
	if entryRoot == "" {
		_, name, splitErr := catalog.SplitKey(w.Key)
		if splitErr != nil {
			return source.Placement{}, splitErr
		}
		entryRoot = filepath.Join(r.Bundle.Organizer.SeriesSource(), name)
	}
	if w.MediaType == catalog.MediaMovie {
		media = source.Movie
	}
	_, container, err := catalog.SplitKey(w.Key)
	if err != nil {
		return source.Placement{}, err
	}
	if media == source.Series {
		if i.SourceFolder != "" {
			container = i.SourceFolder
			return source.PlacementFor(media, shape, entryRoot, r.Bundle.Organizer.MovieSource(), container)
		}
		placementSeason := i.Source.Season
		if placementSeason == 0 && !i.Source.Special && i.Target.Season > 0 {
			// A manual/search candidate may carry only resolved target context.
			// Falling back here chooses a source container; it does not teach the
			// filename parser anything about directory names.
			placementSeason = i.Target.Season
		}
		if placementSeason < 0 || placementSeason > 99 {
			return source.Placement{}, fmt.Errorf("source placement season %d is outside supported range", placementSeason)
		}
		// Source topology describes the release as downloaded. Target season
		// projection is a separate namespace concern and may intentionally differ.
		container = fmt.Sprintf("Season %02d", placementSeason)
	}
	return source.PlacementFor(media, shape, entryRoot, r.Bundle.Organizer.MovieSource(), container)
}
func uniqueMatch(i acquisitionIntent, tasks []download.Task) (*download.Task, error) {
	var m []download.Task
	for _, t := range tasks {
		if taskMatches(i, t) {
			m = append(m, t)
		}
	}
	if len(m) > 1 {
		return nil, fmt.Errorf("acquisition %s matches multiple downloader tasks", i.ID)
	}
	if len(m) == 1 {
		return &m[0], nil
	}
	return nil, nil
}
func taskMatches(i acquisitionIntent, t download.Task) bool {
	if t.ID == download.CorrelationTaskID(i.Correlation) {
		return true
	}
	if i.Identity.Kind == "btih" && strings.EqualFold(t.InfoHash, i.Identity.Canonical) {
		return true
	}
	for _, s := range t.Sources {
		in := acquisition.IdentityInput{}
		if strings.HasPrefix(strings.ToLower(s), "magnet:") {
			in.MagnetURL = s
		} else {
			in.DownloadURL = s
		}
		if x, e := acquisition.CanonicalIdentity(in); e == nil && x.ID == i.ID {
			return true
		}
	}
	return false
}
func taskOutsideManagedSource(bundle configstore.Bundle, client string, task download.Task) bool {
	if strings.TrimSpace(task.ContentPath) == "" {
		return false
	}
	path := download.MapPath(task.ContentPath, clientMappings(bundle, client))
	root := filepath.Clean(bundle.Organizer.Source)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	return rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameEpisode(a, b episode.Key) bool {
	return a.Overlaps(b)
}

func hasWantedFiles(files []download.File) bool {
	for _, file := range files {
		if file.Wanted {
			return true
		}
	}
	return false
}

func (r Runner) ReconcilePublications(ctx context.Context) ([]ReconcileResult, error) {
	physical, err := observation.Build(r.Bundle.Organizer.Source, r.Bundle.Organizer.Target, r.Bundle.Entries)
	if err != nil {
		return nil, fmt.Errorf("observe filesystem graph: %w", err)
	}
	return r.ReconcilePublicationsWithGraph(ctx, physical)
}
func (r Runner) ReconcilePublicationsWithGraph(ctx context.Context, physical observation.Graph) ([]ReconcileResult, error) {
	var out []ReconcileResult
	var errs []error
	local, localErr := r.reconcileLocalSources(ctx, physical)
	out = append(out, local...)
	if localErr != nil {
		errs = append(errs, localErr)
	}
	// Local publication may have created or removed hardlink aliases. Do not let
	// downloader reconciliation make destructive stale-alias decisions from the
	// pre-local library snapshot: that can make two otherwise valid authorities
	// oscillate across reconcile cycles.
	if reconcileMutatedLibrary(local) {
		library, err := filesystem.ObserveTreeIfPresent(r.Bundle.Organizer.Target)
		if err != nil {
			errs = append(errs, fmt.Errorf("refresh library after local publication: %w", err))
		} else {
			physical.LibraryRoot = library
		}
	}
	sourceIndex := claim.BuildSourceIndexFromGraph(physical, r.Bundle.Entries, r.Bundle.Organizer.Extensions)
	remote := r.observe(ctx)
	clients := make([]string, 0, len(r.Clients))
	for id := range r.Clients {
		clients = append(clients, id)
	}
	sort.Strings(clients)
	for _, client := range clients {
		src, ok := r.Clients[client].(taskSource)
		if !ok {
			continue
		}
		if err := remote.errs[client]; err != nil {
			errs = append(errs, err)
			continue
		}
		tasks := append([]download.Task(nil), remote.tasks[client]...)
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
		for _, task := range tasks {
			res := ReconcileResult{Client: client, TaskID: task.ID}
			if taskOutsideManagedSource(r.Bundle, client, task) {
				res.Decision = "ignored"
				res.Reason = "downloader content path is outside the managed source namespace"
				out = append(out, res)
				continue
			}
			files, err := observeFiles(ctx, remote, client, src, task)
			if err != nil {
				res.Decision, res.Error = "failed", err.Error()
				out = append(out, res)
				errs = append(errs, err)
				continue
			}
			if !hasWantedFiles(files) {
				res.Decision = "pending"
				res.Reason = "wanted file metadata is not available yet"
				out = append(out, res)
				continue
			}
			cl := claim.ResolveWithIndex(ctx, r.Bundle, sourceIndex, client, task, files)
			res.EntryKey = cl.EntryKey
			if cl.Status != claim.Resolved {
				res.Reason = cl.Detail
				switch {
				case cl.EntryKey == "" && cl.Status == claim.Unknown:
					// The downloader is allowed to contain arbitrary user-owned tasks.
					// Absence from every declared source namespace means aninode has
					// nothing to reconcile, not that automation failed.
					res.Decision = "ignored"
				case cl.Status == claim.Ambiguous:
					res.Decision = "conflict"
				default:
					res.Decision = "conflict"
				}
				out = append(out, res)
				continue
			}
			maps := clientMappings(r.Bundle, client)
			observation, err := source.Observe(task, files, maps)
			if err != nil {
				res.Decision, res.Error = "unknown", err.Error()
				out = append(out, res)
				continue
			}
			cfg, err := r.Bundle.OrganizerForPublication(cl.EntryKey, cl.SourceEpisode.Season, cl.TargetEpisode.Season, observation.ContentRoot)
			if err != nil {
				res.Decision, res.Error = "failed", err.Error()
				out = append(out, res)
				errs = append(errs, err)
				continue
			}
			extensions := mediafile.ExtensionSet(cfg.Extensions)
			var paths []string
			for _, path := range observation.Paths {
				ready, readyErr := mediafile.ReadyPath(path, extensions)
				if readyErr != nil {
					err = errors.Join(err, readyErr)
					continue
				}
				if ready {
					paths = append(paths, path)
				}
			}
			if err != nil {
				res.Decision, res.Error = "failed", err.Error()
				out = append(out, res)
				errs = append(errs, err)
				continue
			}
			if len(paths) == 0 {
				continue
			}
			plan, pe := organizer.BuildPlanForSnapshot(ctx, cfg, cl.Physical, physical.Libraries[cl.EntryKey].Subtree(cfg.Target))
			applied, ae := organizer.ApplyPlanWithLibrarySnapshot(ctx, cfg, plan, physical.LibraryRoot)
			res.Plan, res.Result = plan, applied
			if err = errors.Join(pe, ae); err != nil {
				res.Decision, res.Error = "failed", err.Error()
				errs = append(errs, err)
			} else if applied.Conflicts > 0 {
				res.Decision = "conflict"
				res.Reason = publicationConflictReason(plan, applied.Conflicts)
			} else {
				res.Decision = "reconciled"
			}
			out = append(out, res)
			// Do not rescan the entire library after every task. ApplyPlan performs
			// syscall-level destination/object rechecks, so the request-local snapshot
			// can safely remain the planning baseline for the rest of this batch. The
			// application performs one coherent graph refresh after publication.
		}
	}
	return out, errors.Join(errs...)
}

func reconcileMutatedLibrary(results []ReconcileResult) bool {
	for _, result := range results {
		if result.Result.Linked > 0 || result.Result.Removed > 0 {
			return true
		}
	}
	return false
}

func publicationConflictReason(plan organizer.Plan, count int) string {
	if count <= 0 {
		return ""
	}
	const maxSamples = 2
	var samples []string
	for _, item := range plan.Items {
		if item.Action != organizer.ActionConflict {
			continue
		}
		detail := strings.TrimSpace(item.Detail)
		if detail == "" && item.Destination != "" {
			detail = fmt.Sprintf("destination %q conflicts with the observed source", item.Destination)
		}
		if detail != "" {
			samples = append(samples, detail)
		}
		if len(samples) >= maxSamples {
			break
		}
	}
	prefix := fmt.Sprintf("%d publication conflict(s); aninode preserved the existing destination", count)
	if len(samples) == 0 {
		return prefix
	}
	return prefix + ": " + strings.Join(samples, "; ")
}

func (r Runner) reconcileLocalSources(ctx context.Context, physical observation.Graph) ([]ReconcileResult, error) {
	var out []ReconcileResult
	var errs []error
	keys := make([]string, 0, len(r.Bundle.Entries))
	for key := range r.Bundle.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		e := r.Bundle.Entries[key]
		if !e.Enabled {
			continue
		}
		snap, ok := physical.Sources[key]
		if !ok {
			continue
		}
		if e.MediaType == catalog.MediaMovie {
			cfg, err := r.Bundle.OrganizerForPublication(key, 0, 0, e.DeclarationPath)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			plan, pe := organizer.BuildPlanForSnapshot(ctx, cfg, snap, physical.Libraries[key].Subtree(cfg.Target))
			res, ae := organizer.ApplyPlanWithLibrarySnapshot(ctx, cfg, plan, physical.LibraryRoot)
			rr := ReconcileResult{EntryKey: key, Decision: "local-reconciled", Plan: plan, Result: res}
			if err := errors.Join(pe, ae); err != nil {
				rr.Decision = "failed"
				rr.Error = err.Error()
				errs = append(errs, err)
			} else if res.Conflicts > 0 {
				rr.Decision = "conflict"
				rr.Reason = publicationConflictReason(plan, res.Conflicts)
			}
			out = append(out, rr)
			continue
		}
		// Local source publication uses the same best-effort inference as
		// declaration adoption. Guessing a source season is cheap here: only the
		// library hardlink projection changes and declaration offsets can correct it.
		inferred := catalog.InferSeriesContainersFromSnapshot(e.DeclarationPath, snap, r.Bundle.Organizer.Extensions)
		for _, inferredContainer := range inferred {
			container := inferredContainer.Path
			folderName := filepath.Base(container)
			rule, configured := e.FolderProjections[folderName]
			if !configured {
				rule.Season = inferredContainer.SuggestedSeason
			}
			if rule.Season < 0 {
				continue
			}
			cfg, err := r.Bundle.OrganizerForFolderPublication(key, inferredContainer.SuggestedSeason, rule.Season, rule.EpisodeOffset, container)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			containerSnap := snap.Subtree(container)
			plan, pe := organizer.BuildPlanForSnapshot(ctx, cfg, containerSnap, physical.Libraries[key].Subtree(cfg.Target))
			res, ae := organizer.ApplyPlanWithLibrarySnapshot(ctx, cfg, plan, physical.LibraryRoot)
			rr := ReconcileResult{EntryKey: key, Decision: "local-reconciled", Plan: plan, Result: res}
			if err := errors.Join(pe, ae); err != nil {
				rr.Decision = "failed"
				rr.Error = err.Error()
				errs = append(errs, err)
			} else if res.Conflicts > 0 {
				rr.Decision = "conflict"
				rr.Reason = publicationConflictReason(plan, res.Conflicts)
			}
			out = append(out, rr)
		}
	}
	return out, errors.Join(errs...)
}

// ReconcileLocalEntryWithGraph immediately converges publication for one Entry
// after its declaration changes. Downloader inspection and unrelated Entries
// are intentionally excluded from this management fast path.
func (r Runner) ReconcileLocalEntryWithGraph(ctx context.Context, physical observation.Graph, key string) ([]ReconcileResult, error) {
	e, ok := r.Bundle.Entries[key]
	if !ok {
		return nil, fmt.Errorf("entry %q not found", key)
	}
	r.Bundle.Entries = map[string]catalog.Entry{key: e}
	return r.reconcileLocalSources(ctx, physical)
}

func clientMappings(b configstore.Bundle, client string) []download.PathMapping {
	var mappings []download.PathMapping
	for _, value := range b.Clients[client].PathMappings {
		mappings = append(mappings, download.PathMapping{Remote: value.Remote, Local: value.Local})
	}
	return mappings
}
