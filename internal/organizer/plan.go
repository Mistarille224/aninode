package organizer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"aninode/internal/catalog"
	"aninode/internal/filesystem"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
	"aninode/internal/moviebundle"
)

type Action string

const (
	ActionLink      Action = "link"
	ActionExisting  Action = "existing"
	ActionUnmatched Action = "unmatched"
	ActionConflict  Action = "conflict"
	ActionExcluded  Action = "excluded"
)

type Plan struct {
	Items    []PlanItem `json:"items"`
	Warnings []string   `json:"warnings,omitempty"`
}

type PlanItem struct {
	Source       string              `json:"source"`
	SourceObject filesystem.ObjectID `json:"-"`
	Relative     string              `json:"relative_source,omitempty"`
	Parents      []string            `json:"source_parents,omitempty"`
	Destination  string              `json:"destination,omitempty"`
	Normalized   string              `json:"normalized"`
	Action       Action              `json:"action"`
	Detail       string              `json:"detail,omitempty"`
}

// BuildPlan observes source and target once, then plans from those immutable
// snapshots. Standalone previews therefore use the same observation boundary as
// reconciliation instead of a private WalkDir/stat path.
func BuildPlan(ctx context.Context, cfg Config) (Plan, error) {
	sourceSnapshot, err := filesystem.ObserveTree(cfg.Source)
	if err != nil {
		return Plan{}, fmt.Errorf("observe source: %w", err)
	}
	targetSnapshot, err := filesystem.ObserveTreeIfPresent(cfg.Target)
	if err != nil {
		return Plan{}, fmt.Errorf("observe target: %w", err)
	}
	return BuildPlanForSnapshot(ctx, cfg, sourceSnapshot, targetSnapshot)
}

// BuildPlanForFiles plans only the supplied files without rescanning unrelated
// source material.
func BuildPlanForFiles(ctx context.Context, cfg Config, paths []string) (Plan, error) {
	return BuildPlanForAnalyzedFiles(ctx, cfg, paths, nil)
}

// BuildPlanForAnalyzedFiles accepts upstream name analysis when the caller has
// already analyzed the same path cohort. A nil analysis preserves the ordinary
// standalone entry point.
func BuildPlanForAnalyzedFiles(ctx context.Context, cfg Config, paths []string, names []medianame.Name) (Plan, error) {
	planner, err := newPlanner(cfg)
	if err != nil {
		return Plan{}, err
	}
	if cfg.movie != nil {
		return planner.planMovie(ctx, paths)
	}
	if len(names) == len(paths) {
		planner.prepareNames(paths, names)
	} else {
		planner.prepare(paths)
	}
	return planner.planPaths(ctx, paths, true, nil)
}

// BuildPlanForObservedFiles reuses request-local name and filesystem
// observations. Callers must keep paths, names, and infos index-aligned.
func BuildPlanForObservedFiles(ctx context.Context, cfg Config, paths []string, names []medianame.Name, infos []fs.FileInfo) (Plan, error) {
	planner, err := newPlanner(cfg)
	if err != nil {
		return Plan{}, err
	}
	if cfg.movie != nil {
		return planner.planMovie(ctx, paths)
	}
	if len(names) != len(paths) || len(infos) != len(paths) {
		return Plan{}, errors.New("observed file metadata does not match paths")
	}
	metadata := make([]filesystem.Metadata, len(infos))
	for i, info := range infos {
		meta, ok := filesystem.MetadataOf(info)
		if !ok {
			return Plan{}, fmt.Errorf("filesystem identity unavailable for %s", paths[i])
		}
		metadata[i] = meta
	}
	planner.prepareNames(paths, names)
	return planner.planPaths(ctx, paths, true, metadata)
}

// BuildPlanForSnapshot plans directly from one shared physical observation.
// target may be a projection of the reconciliation graph; no target rescan is
// performed during planning.
func BuildPlanForSnapshot(ctx context.Context, cfg Config, source, target filesystem.Snapshot) (Plan, error) {
	planner, err := newPlannerWithTarget(cfg, target)
	if err != nil {
		return Plan{}, err
	}
	planner.sourceIDs = make(map[string]filesystem.ObjectID, len(source.Paths))
	for path, id := range source.Paths {
		planner.sourceIDs[path] = id
	}
	paths := make([]string, 0, len(source.Paths))
	for path := range source.Paths {
		if cfg.movie != nil || mediafile.HasAllowedExtension(path, planner.extensions) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	if cfg.movie != nil {
		return planner.planMovie(ctx, paths)
	}
	planner.prepare(paths)
	metadata := make([]filesystem.Metadata, len(paths))
	for i, path := range paths {
		id := source.Paths[path]
		metadata[i] = source.Objects[id].Metadata
	}
	return planner.planPaths(ctx, paths, true, metadata)
}

func (planner *filePlanner) planMovie(ctx context.Context, paths []string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	b, err := moviebundle.AnalyzePaths(planner.cfg.Source, paths, moviebundle.Options{Title: planner.cfg.movie.title, Year: planner.cfg.movie.year, Blacklist: planner.cfg.blacklist, Overrides: planner.cfg.movie.overrides})
	if err != nil {
		return Plan{}, fmt.Errorf("observe movie bundle: %w", err)
	}
	var plan Plan
	add := func(a moviebundle.Asset, relative string, action Action, detail string) {
		item := PlanItem{Source: a.Path, Relative: a.Relative, Normalized: filepath.Base(a.Relative), Action: action, Detail: detail}
		if id, ok := planner.objectID(a.Path); ok {
			item.SourceObject = id
		}
		if relative != "" {
			destination, err := safeDestination(planner.cfg.Target, relative)
			if err != nil {
				item.Action = ActionConflict
				item.Detail = err.Error()
			} else {
				item.Destination = destination
				if action == ActionLink {
					planner.finalizeMovieItem(&item)
				}
			}
		}
		plan.Items = append(plan.Items, item)
	}
	for _, a := range b.Excluded {
		add(a, "", ActionExcluded, a.Detail)
	}
	plan.Warnings = append(plan.Warnings, b.Warnings...)
	if b.Kind == moviebundle.Conflict || b.Conflict != "" {
		for _, a := range b.Unknown {
			add(a, "", ActionConflict, a.Detail)
		}
		if len(b.Unknown) == 0 {
			plan.Items = append(plan.Items, PlanItem{Source: planner.cfg.Source, Normalized: filepath.Base(planner.cfg.Source), Action: ActionConflict, Detail: b.Conflict})
		}
		return plan, nil
	}
	switch b.Kind {
	case moviebundle.BluRay, moviebundle.DVD:
		for _, a := range b.OpaqueTree.Assets {
			add(a, a.Relative, ActionLink, "opaque disc tree: preserve relative path and filename")
		}
	case moviebundle.ISO:
		a := b.OpaqueTree.Assets[0]
		add(a, moviebundle.ISOTargetName(planner.cfg.movie.title, planner.cfg.movie.year, a), ActionLink, "opaque ISO image")
	case moviebundle.Files:
		for _, v := range b.Versions {
			add(v.Video, moviebundle.TargetName(planner.cfg.movie.title, planner.cfg.movie.year, v, v.Video), ActionLink, "movie version")
			for _, a := range v.Subtitles {
				add(a, moviebundle.TargetName(planner.cfg.movie.title, planner.cfg.movie.year, v, a), ActionLink, "associated subtitle")
			}
			for _, a := range v.Audios {
				add(a, moviebundle.TargetName(planner.cfg.movie.title, planner.cfg.movie.year, v, a), ActionLink, "associated external audio")
			}
		}
		for _, x := range b.Extras {
			add(x.Asset, moviebundle.ExtraTargetName(x), ActionLink, "recognized movie extra")
		}
	}
	byDestination := map[string][]int{}
	for i, item := range plan.Items {
		if item.Destination != "" && (item.Action == ActionLink || item.Action == ActionExisting || item.Action == ActionConflict) {
			byDestination[item.Destination] = append(byDestination[item.Destination], i)
		}
	}
	for destination, indexes := range byDestination {
		if len(indexes) < 2 {
			continue
		}
		var sources []string
		for _, i := range indexes {
			sources = append(sources, plan.Items[i].Relative)
		}
		detail := fmt.Sprintf("movie assets map to the same target %s: %s", filepath.Base(destination), strings.Join(sources, ", "))
		for _, i := range indexes {
			plan.Items[i].Action = ActionConflict
			plan.Items[i].Detail = detail
		}
	}
	return plan, nil
}

func (planner *filePlanner) finalizeMovieItem(item *PlanItem) {
	id, ok := planner.objectID(item.Source)
	if !ok {
		item.Action = ActionConflict
		item.Detail = "filesystem identity unavailable"
		return
	}
	if did, exists := planner.targetIDs[filepath.Clean(item.Destination)]; exists {
		if did == id {
			item.Action = ActionExisting
		} else {
			item.Action = ActionConflict
			item.Detail = fmt.Sprintf("destination %q exists and is a different filesystem object from source %q", item.Destination, item.Source)
		}
		return
	}
	if owner, exists := planner.plannedDestinations[item.Destination]; exists && owner != id {
		item.Action = ActionConflict
		item.Detail = "multiple source files resolve to the same destination"
		return
	}
	planner.plannedDestinations[item.Destination] = id
}

func (planner *filePlanner) prepare(paths []string) {
	if planner.cfg.emby == nil {
		return
	}
	names := make([]string, len(paths))
	for i := range paths {
		names[i] = filepath.Base(paths[i])
	}
	analysis := medianame.AnalyzeFiles(names)
	planner.prepareNames(paths, analysis.Items)
}

func (planner *filePlanner) prepareNames(paths []string, names []medianame.Name) {
	planner.batchNames = make(map[string]medianame.Name, len(paths))
	for i := range paths {
		planner.batchNames[paths[i]] = names[i]
	}
}

func (planner *filePlanner) planPaths(ctx context.Context, paths []string, requireWithinRoot bool, metadata []filesystem.Metadata) (Plan, error) {
	var plan Plan
	var planErrs []error
	for pathIndex, sourcePath := range paths {
		if err := ctx.Err(); err != nil {
			planErrs = append(planErrs, err)
			break
		}
		if requireWithinRoot && (!withinRoot(planner.cfg.Source, sourcePath) || !realPathWithinRoot(planner.cfg.Source, sourcePath)) {
			planErrs = append(planErrs, fmt.Errorf("completed file escapes configured source: %s", sourcePath))
			continue
		}
		var meta filesystem.Metadata
		if len(metadata) == len(paths) {
			meta = metadata[pathIndex]
		} else {
			info, err := os.Lstat(sourcePath)
			if err != nil {
				planErrs = append(planErrs, fmt.Errorf("stat %s: %w", sourcePath, err))
				continue
			}
			var ok bool
			meta, ok = filesystem.MetadataOf(info)
			if !ok {
				planErrs = append(planErrs, fmt.Errorf("filesystem identity unavailable for %s", sourcePath))
				continue
			}
		}
		item, included, planErr := planner.file(sourcePath, meta)
		if planErr != nil {
			planErrs = append(planErrs, planErr)
			continue
		}
		if included {
			plan.Items = append(plan.Items, item)
		}
	}
	return plan, errors.Join(planErrs...)
}

type filePlanner struct {
	cfg                 Config
	extensions          map[string]bool
	linked              map[filesystem.ObjectID]string
	plannedDestinations map[string]filesystem.ObjectID
	batchNames          map[string]medianame.Name
	sourceIDs           map[string]filesystem.ObjectID
	targetIDs           map[string]filesystem.ObjectID
}

func newPlanner(cfg Config) (*filePlanner, error) {
	target, err := filesystem.ObserveTreeIfPresent(cfg.Target)
	if err != nil {
		return nil, fmt.Errorf("index target: %w", err)
	}
	return newPlannerWithTarget(cfg, target)
}

func newPlannerWithTarget(cfg Config, target filesystem.Snapshot) (*filePlanner, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	if cfg.emby == nil && cfg.movie == nil {
		return nil, errors.New("media layout is required for planning")
	}
	linked := make(map[filesystem.ObjectID]string, len(target.Objects))
	for id, object := range target.Objects {
		if len(object.Paths) > 0 {
			linked[id] = object.Paths[0]
		}
	}
	targetIDs := make(map[string]filesystem.ObjectID, len(target.Paths))
	for path, id := range target.Paths {
		targetIDs[filepath.Clean(path)] = id
	}
	return &filePlanner{cfg: cfg, extensions: mediafile.ExtensionSet(cfg.Extensions), linked: linked, plannedDestinations: make(map[string]filesystem.ObjectID), targetIDs: targetIDs}, nil
}

func (planner *filePlanner) file(sourcePath string, meta filesystem.Metadata) (PlanItem, bool, error) {
	if !meta.Mode.IsRegular() || !mediafile.HasAllowedExtension(sourcePath, planner.extensions) {
		return PlanItem{}, false, nil
	}
	item := PlanItem{Source: sourcePath, SourceObject: meta.ID, Normalized: filepath.Base(sourcePath)}
	if relative, err := filepath.Rel(planner.cfg.Source, sourcePath); err == nil {
		item.Relative = filepath.ToSlash(relative)
		parent := filepath.Dir(relative)
		for parent != "." && parent != string(filepath.Separator) && len(item.Parents) < 4 {
			item.Parents = append(item.Parents, filepath.Base(parent))
			next := filepath.Dir(parent)
			if next == parent {
				break
			}
			parent = next
		}
	}
	if pattern, matched := catalog.BlacklistMatch(planner.cfg.blacklist, item.Normalized, item.Relative); matched {
		item.Action, item.Detail = ActionExcluded, fmt.Sprintf("matched Entry blacklist pattern %q", pattern)
		return item, true, nil
	}
	id := meta.ID
	var relative string
	var matched bool
	var resolveErr error
	name, analyzed := planner.batchNames[sourcePath]
	if analyzed && planner.cfg.emby != nil {
		relative, matched, resolveErr = resolveEmbyParsed(item.Normalized, planner.cfg.emby, name.Parsed)
	} else {
		relative, matched, resolveErr = resolveRelative(item.Normalized, planner.cfg)
	}
	if !matched && resolveErr == nil && planner.cfg.emby != nil {
		// The configured source for folder publication IS already the filesystem
		// season/container context. A direct child therefore needs no additional
		// parent path component before weak names such as "Show - 01.mkv" or
		// "01.mkv" may use the folder's suggested season hint. Requiring
		// item.Parents here made the most common topology fail to republish after
		// declaration edits.
		if parsed, ok := medianame.EpisodeWithSeasonHint(name, planner.cfg.emby.season); analyzed && ok {
			item.Normalized = filepath.Base(sourcePath)
			relative, matched, resolveErr = resolveEmbyParsed(item.Normalized, planner.cfg.emby, parsed)
		}
	}
	if resolveErr != nil {
		item.Action, item.Detail = ActionUnmatched, resolveErr.Error()
		return item, true, nil
	}
	if !matched {
		item.Action = ActionUnmatched
		if planner.cfg.movie != nil {
			item.Detail = "movie file excluded by blacklist"
		} else {
			item.Detail = "episode filename was not recognized"
		}
		return item, true, nil
	}
	var err error
	item.Destination, err = safeDestination(planner.cfg.Target, relative)
	if err != nil {
		return item, false, fmt.Errorf("resolved destination for %s: %w", sourcePath, err)
	}
	if destinationID, exists := planner.targetIDs[filepath.Clean(item.Destination)]; exists {
		if destinationID == id {
			item.Action = ActionExisting
			planner.linked[id] = item.Destination
		} else {
			item.Action, item.Detail = ActionConflict, fmt.Sprintf("destination %q exists and is a different filesystem object from source %q", item.Destination, item.Source)
		}
	} else if owner, exists := planner.plannedDestinations[item.Destination]; exists && owner != id {
		item.Action, item.Detail = ActionConflict, "multiple source files resolve to the same destination"
	} else {
		item.Action = ActionLink
		planner.plannedDestinations[item.Destination] = id
		if existing, exists := planner.linked[id]; exists {
			item.Detail = "canonical path is missing; same filesystem object also exists at " + existing
		}
	}
	return item, true, nil
}

// ApplyPlan executes only link actions. It rechecks destinations so a plan is
// safe to apply even if the filesystem changed after planning.
func ApplyPlan(ctx context.Context, cfg Config, plan Plan) (Result, error) {
	var result Result
	var applyErrs []error
	if err := validate(cfg); err != nil {
		return result, err
	}
	needsTarget := false
	for _, item := range plan.Items {
		if item.Action == ActionLink {
			needsTarget = true
			break
		}
	}
	if needsTarget {
		if err := filesystem.EnsureHardlinkDomain(cfg.Source, cfg.Target, 0o755); err != nil {
			return result, err
		}
	}
	for _, item := range plan.Items {
		if err := ctx.Err(); err != nil {
			applyErrs = append(applyErrs, err)
			break
		}
		result.Scanned++
		switch item.Action {
		case ActionExisting:
			result.Existing++
		case ActionUnmatched:
			result.Unmatched++
		case ActionExcluded:
			result.Excluded++
		case ActionConflict:
			// A conflict discovered while planning is an observable business state,
			// not an execution failure. Preserve the destination and report it through
			// Result.Conflicts; callers surface a structured warning. Runtime races
			// while executing ActionLink remain errors below.
			result.Conflicts++
		case ActionLink:
			if !withinRoot(cfg.Source, item.Source) || !withinRoot(cfg.Target, item.Destination) {
				applyErrs = append(applyErrs, fmt.Errorf(
					"plan item escapes configured roots: %s -> %s",
					item.Source,
					item.Destination,
				))
				continue
			}

			if !realPathWithinRoot(cfg.Source, item.Source) {
				applyErrs = append(applyErrs, fmt.Errorf(
					"source escapes configured source: %s",
					item.Source,
				))
				continue
			}
			sourceInfo, sourceErr := os.Lstat(item.Source)
			if sourceErr != nil || sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
				applyErrs = append(applyErrs, fmt.Errorf("source is no longer the planned regular file: %s", item.Source))
				continue
			}
			currentID, ok := filesystem.Identity(sourceInfo)
			if !ok || currentID != item.SourceObject {
				result.Conflicts++
				applyErrs = append(applyErrs, fmt.Errorf("source filesystem object changed since planning: %s", item.Source))
				continue
			}

			created, err := filesystem.LinkObserved(cfg.Source, item.Source, cfg.Target, item.Destination, item.SourceObject, 0o755)
			if err != nil {
				if errors.Is(err, syscall.EXDEV) {
					applyErrs = append(applyErrs,
						errors.New("hardlink unavailable: source and library are on different filesystems"))
					continue
				}
				if errors.Is(err, fs.ErrExist) {
					result.Conflicts++
				}
				applyErrs = append(applyErrs, fmt.Errorf(
					"link %s to %s: %w",
					item.Source,
					item.Destination,
					err,
				))
				continue
			}
			if created {
				result.Linked++
			} else {
				result.Existing++
			}
		default:
			applyErrs = append(applyErrs, fmt.Errorf("unknown plan action %q", item.Action))
		}
	}
	return result, errors.Join(applyErrs...)
}

// ApplyPlanWithLibrarySnapshot applies a publication plan and then converges
// the managed library namespace for the same physical source objects. Cleanup
// happens only after every desired destination for an object is proven to name
// that object. It therefore removes stale publication paths without ever
// treating path history as state.
func ApplyPlanWithLibrarySnapshot(ctx context.Context, cfg Config, plan Plan, library filesystem.Snapshot) (Result, error) {
	result, applyErr := ApplyPlan(ctx, cfg, plan)
	removed, cleanupErr := convergePublicationAliases(ctx, cfg, plan, library)
	result.Removed += removed
	return result, errors.Join(applyErr, cleanupErr)
}

func convergePublicationAliases(ctx context.Context, cfg Config, plan Plan, library filesystem.Snapshot) (int, error) {
	if !cfg.Managed || strings.TrimSpace(cfg.PublicationRoot) == "" {
		return 0, nil
	}
	publicationRoot := filepath.Clean(cfg.PublicationRoot)
	if library.Root == "" || !withinRoot(library.Root, publicationRoot) {
		return 0, fmt.Errorf("publication root %s is outside observed library root %s", publicationRoot, library.Root)
	}
	desired := map[filesystem.ObjectID]map[string]bool{}
	for _, item := range plan.Items {
		if item.SourceObject == (filesystem.ObjectID{}) {
			continue
		}
		if item.Action == ActionExcluded {
			// An excluded source object intentionally has no managed publication.
			// Keep an empty desired set so prior hardlink projections converge away.
			if desired[item.SourceObject] == nil {
				desired[item.SourceObject] = map[string]bool{}
			}
			continue
		}
		if item.Destination == "" || (item.Action != ActionLink && item.Action != ActionExisting) {
			continue
		}
		if desired[item.SourceObject] == nil {
			desired[item.SourceObject] = map[string]bool{}
		}
		desired[item.SourceObject][filepath.Clean(item.Destination)] = true
	}
	var removed int
	var cleanupErrs []error
	for objectID, destinations := range desired {
		if err := ctx.Err(); err != nil {
			cleanupErrs = append(cleanupErrs, err)
			break
		}
		// The new canonical projection must exist before any old name is removed.
		proven := true
		for destination := range destinations {
			info, err := os.Lstat(destination)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				proven = false
				break
			}
			got, ok := filesystem.Identity(info)
			if !ok || got != objectID {
				proven = false
				break
			}
		}
		if !proven {
			continue
		}
		object, ok := library.Objects[objectID]
		if !ok {
			continue
		}
		for _, stale := range object.Paths {
			stale = filepath.Clean(stale)
			if destinations[stale] || !withinRoot(publicationRoot, stale) {
				continue
			}
			if err := filesystem.RemoveObservedLink(publicationRoot, stale, objectID); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("remove stale publication %s: %w", stale, err))
				continue
			}
			removed++
			if err := filesystem.RemoveEmptyParents(publicationRoot, filepath.Dir(stale)); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("prune stale publication directories for %s: %w", stale, err))
			}
		}
	}
	return removed, errors.Join(cleanupErrs...)
}

func (planner *filePlanner) objectID(path string) (filesystem.ObjectID, bool) {
	if planner.sourceIDs != nil {
		id, ok := planner.sourceIDs[filepath.Clean(path)]
		return id, ok
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return filesystem.ObjectID{}, false
	}
	return filesystem.Identity(info)
}

func resolveRelative(name string, cfg Config) (string, bool, error) {
	if cfg.movie != nil {
		return "", false, errors.New("movie destinations require bundle planning")
	}
	if cfg.emby == nil {
		return "", false, errors.New("series layout is required")
	}
	return resolveEmby(name, cfg.emby)
}

func withinRoot(root, path string) bool {
	root, rootErr := filepath.Abs(root)
	path, pathErr := filepath.Abs(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// realPathWithinRoot rejects symlink traversal anywhere below the configured
// root. Missing suffix components are allowed for destinations that are about
// to be created. This deliberately matches the observation graph rule:
// arbitrary subtree symlinks are uncertainty, not transparent paths.
func realPathWithinRoot(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil || !withinRoot(rootAbs, pathAbs) {
		return false
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	current := rootAbs
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			// Once a component is absent no descendant can currently be a
			// symlink through this path; Apply will recheck after mkdir.
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 && !info.IsDir() {
			return false
		}
	}
	return true
}
