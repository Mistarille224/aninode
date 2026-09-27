package application

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aninode/internal/atomicfile"
	"aninode/internal/catalog"
	"aninode/internal/download"
	"aninode/internal/filesystem"
	"aninode/internal/jsonfile"
	"aninode/internal/migration"
)

const trashFile = ".aninode-trash.json"
const trashRetention = 7 * 24 * time.Hour

type trashFileRecord struct {
	Path     string              `json:"path"`
	ID       filesystem.ObjectID `json:"id"`
	Size     int64               `json:"size"`
	Modified int64               `json:"modified"`
}

type TrashTask struct {
	Client      string         `json:"client"`
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	InfoHash    string         `json:"info_hash,omitempty"`
	ContentPath string         `json:"content_path,omitempty"`
	State       download.State `json:"state"`
}

type TrashPreview struct {
	Key          string      `json:"key"`
	Title        string      `json:"title"`
	Token        string      `json:"token"`
	SourceFiles  int         `json:"source_files"`
	LibraryFiles int         `json:"library_files"`
	SourceBytes  int64       `json:"source_bytes"`
	Tasks        []TrashTask `json:"tasks"`
	ExpiresAt    time.Time   `json:"expires_at"`
}

type TrashEntry struct {
	Signature    string            `json:"signature"`
	Token        string            `json:"token"`
	Key          string            `json:"key"`
	Title        string            `json:"title"`
	Year         int               `json:"year,omitempty"`
	MediaType    string            `json:"media_type"`
	Status       string            `json:"status"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	SourceFiles  int               `json:"source_files"`
	LibraryFiles int               `json:"library_files"`
	SourceBytes  int64             `json:"source_bytes"`
	Tasks        []TrashTask       `json:"tasks"`
	Error        string            `json:"error,omitempty"`
	SourceRoot   string            `json:"source_root"`
	LibraryRoot  string            `json:"library_root"`
	Source       []trashFileRecord `json:"source"`
	Library      []trashFileRecord `json:"library"`
}

func (a *App) trashSecret(create bool) ([]byte, error) {
	path := filepath.Join(a.configRoot, "secrets", "trash.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := atomicfile.Create(path, key, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		key, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("trash signing key is invalid")
	}
	return key, nil
}

func (a *App) signTrash(entry *TrashEntry, create bool) error {
	key, err := a.trashSecret(create)
	if err != nil {
		return err
	}
	copy := *entry
	copy.Signature = ""
	data, err := jsonfile.Marshal(copy)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	entry.Signature = hex.EncodeToString(mac.Sum(nil))
	return nil
}

func (a *App) saveTrash(entry *TrashEntry, create bool) error {
	if err := a.signTrash(entry, create); err != nil {
		return err
	}
	path := filepath.Join(entry.SourceRoot, trashFile)
	if create {
		return jsonfile.Create(path, entry)
	}
	return jsonfile.Write(path, entry)
}

func (a *App) verifyTrash(entry TrashEntry) error {
	provided := entry.Signature
	if err := a.signTrash(&entry, false); err != nil {
		return err
	}
	if !hmac.Equal([]byte(provided), []byte(entry.Signature)) {
		return errors.New("trash record signature is invalid")
	}
	return nil
}

type trashPlan struct {
	entry   catalog.Entry
	source  []trashFileRecord
	library []trashFileRecord
	tasks   []TrashTask
	bytes   int64
}

type trashClient interface {
	List(context.Context) ([]download.Task, error)
	Files(context.Context, string) ([]download.File, error)
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Remove(context.Context, string, bool) error
}

func under(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func recordTree(root string, exclude map[string]bool) ([]trashFileRecord, error) {
	snapshot, err := filesystem.ObserveTreeIfPresent(root)
	if err != nil {
		return nil, err
	}
	if !snapshot.Complete() {
		return nil, fmt.Errorf("cannot safely inspect %s: %v", root, snapshot.Issues)
	}
	if !snapshot.RootExists {
		return nil, nil
	}
	for path, dir := range snapshot.Directories {
		if dir.ID.Device != snapshot.RootObject.ID.Device {
			return nil, fmt.Errorf("nested mount in work directory: %s", path)
		}
	}
	out := make([]trashFileRecord, 0, len(snapshot.Paths))
	for path, id := range snapshot.Paths {
		if id.Device != snapshot.RootObject.ID.Device {
			return nil, fmt.Errorf("nested mount file in work directory: %s", path)
		}
		if exclude[filepath.Base(path)] && filepath.Dir(path) == root {
			continue
		}
		meta := snapshot.Objects[id].Metadata
		out = append(out, trashFileRecord{Path: path, ID: id, Size: meta.Size, Modified: meta.Modified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (a *App) planTrash(ctx context.Context, rt *runtimeSnapshot, key string) (trashPlan, error) {
	for id := range rt.bundle.Clients {
		if _, ok := rt.backends[id]; !ok {
			return trashPlan{}, managementErr(ErrorConflict, fmt.Errorf("downloader %s must be enabled before deleting a work", id))
		}
	}
	w, ok := rt.bundle.Entries[key]
	if !ok {
		return trashPlan{}, managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", key))
	}
	if !under(rt.bundle.Organizer.Source, w.DeclarationPath) || !under(rt.bundle.Organizer.Target, w.Path) {
		return trashPlan{}, managementErr(ErrorConflict, errors.New("work paths are outside configured roots"))
	}
	source, err := recordTree(w.DeclarationPath, map[string]bool{".aninode.json": true, trashFile: true})
	if err != nil {
		return trashPlan{}, managementErr(ErrorUnknown, err)
	}
	library, err := recordTree(w.Path, nil)
	if err != nil {
		return trashPlan{}, managementErr(ErrorUnknown, err)
	}
	sourceIDs := map[filesystem.ObjectID]bool{}
	for _, file := range source {
		sourceIDs[file.ID] = true
	}
	for _, file := range library {
		if !sourceIDs[file.ID] {
			return trashPlan{}, managementErr(ErrorConflict, fmt.Errorf("library file has no matching source object: %s", file.Path))
		}
	}
	for otherKey, other := range rt.bundle.Entries {
		if otherKey != key && (under(w.DeclarationPath, other.DeclarationPath) || under(other.DeclarationPath, w.DeclarationPath) || under(w.Path, other.Path) || under(other.Path, w.Path) || other.Path == w.Path) {
			return trashPlan{}, managementErr(ErrorConflict, errors.New("work directories overlap another managed entry"))
		}
	}
	tasks, err := observeTrashTasks(ctx, rt, w.DeclarationPath, sourceIDs)
	if err != nil {
		return trashPlan{}, managementErr(ErrorUnknown, err)
	}
	var bytes int64
	seen := map[filesystem.ObjectID]bool{}
	for _, file := range source {
		if !seen[file.ID] {
			bytes += file.Size
			seen[file.ID] = true
		}
	}
	return trashPlan{entry: w, source: source, library: library, tasks: tasks, bytes: bytes}, nil
}

func observeTrashTasks(ctx context.Context, rt *runtimeSnapshot, sourceRoot string, sourceIDs map[filesystem.ObjectID]bool) ([]TrashTask, error) {
	var result []TrashTask
	for clientID, backend := range rt.backends {
		client, ok := backend.(trashClient)
		if !ok {
			return nil, fmt.Errorf("downloader %s cannot pause and remove tasks", clientID)
		}
		observations, err := trashObservations(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("inspect downloader %s: %w", clientID, err)
		}
		mappings := rt.bundle.ClientPathMappings(clientID)
		for _, observation := range observations {
			task := observation.Task
			task.ContentPath = download.MapPath(task.ContentPath, mappings)
			files := observation.Files
			inside, outside := false, false
			for _, file := range files {
				path := filepath.Clean(download.MapPath(file.Path, mappings))
				if under(sourceRoot, path) {
					inside = true
				} else {
					outside = true
				}
				if info, err := os.Lstat(path); err == nil {
					if id, ok := filesystem.Identity(info); ok && sourceIDs[id] && !under(sourceRoot, path) {
						inside = true
					}
				}
			}
			if !inside && under(sourceRoot, filepath.Clean(task.ContentPath)) {
				return nil, fmt.Errorf("task %s/%s points into the work but has no verifiable files", clientID, task.ID)
			}
			if !inside {
				continue
			}
			if outside || len(files) == 0 {
				return nil, fmt.Errorf("task %s/%s spans files outside this work", clientID, task.ID)
			}
			result = append(result, TrashTask{Client: clientID, ID: task.ID, Name: task.Name, InfoHash: task.InfoHash, ContentPath: task.ContentPath, State: task.State})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Client == result[j].Client {
			return result[i].ID < result[j].ID
		}
		return result[i].Client < result[j].Client
	})
	return result, nil
}

func trashObservations(ctx context.Context, client trashClient) ([]download.Observation, error) {
	if bulk, ok := client.(download.ObservationLister); ok {
		return bulk.ListObservations(ctx)
	}
	tasks, err := client.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]download.Observation, 0, len(tasks))
	for _, task := range tasks {
		var files []download.File
		if optimized, ok := client.(download.TaskFileObserver); ok {
			files, err = optimized.FilesForTask(ctx, task)
		} else {
			files, err = client.Files(ctx, task.ID)
		}
		if err != nil {
			return nil, fmt.Errorf("inspect task %s: %w", task.ID, err)
		}
		out = append(out, download.Observation{Task: task, Files: files})
	}
	return out, nil
}

func trashToken(plan trashPlan) string {
	tasks := make([]TrashTask, len(plan.tasks))
	copy(tasks, plan.tasks)
	for i := range tasks {
		tasks[i].State = ""
	}
	data, _ := jsonfile.Marshal(struct {
		Key, Title, MediaType, SourceRoot, LibraryRoot string
		Year                                           int
		Source, Library                                []trashFileRecord
		Tasks                                          []TrashTask
	}{plan.entry.Key, plan.entry.Title, plan.entry.MediaType, plan.entry.DeclarationPath, plan.entry.Path, plan.entry.Year, plan.source, plan.library, tasks})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (a *App) PreviewTrash(ctx context.Context, key string) (TrashPreview, error) {
	rt := a.snapshot()
	if rt == nil {
		return TrashPreview{}, errors.New("runtime unavailable")
	}
	plan, err := a.planTrash(ctx, rt, key)
	if err != nil {
		return TrashPreview{}, err
	}
	return TrashPreview{Key: key, Title: plan.entry.Title, Token: trashToken(plan), SourceFiles: len(plan.source), LibraryFiles: len(plan.library), SourceBytes: plan.bytes, Tasks: plan.tasks, ExpiresAt: a.clock().Add(trashRetention)}, nil
}

func (a *App) TrashEntry(ctx context.Context, key, token string) (TrashEntry, error) {
	var result TrashEntry
	err := a.withOperation(ctx, func() error {
		listed, err := a.ListTrash(ctx)
		if err != nil {
			return err
		}
		for _, existing := range listed {
			if existing.Key == key {
				if token != "" && token == existing.Token {
					result = existing
					return nil
				}
				return managementErr(ErrorConflict, errors.New("work is already in Trash"))
			}
		}
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		plan, err := a.planTrash(ctx, rt, key)
		if err != nil {
			return err
		}
		if token == "" || token != trashToken(plan) {
			return managementErr(ErrorConflict, errors.New("delete preview changed; review it again"))
		}
		created := a.clock().UTC()
		result = TrashEntry{Key: key, Token: token, Title: plan.entry.Title, Year: plan.entry.Year, MediaType: plan.entry.MediaType, Status: "trashed", CreatedAt: created, ExpiresAt: created.Add(trashRetention), SourceFiles: len(plan.source), LibraryFiles: len(plan.library), SourceBytes: plan.bytes, Tasks: plan.tasks, SourceRoot: plan.entry.DeclarationPath, LibraryRoot: plan.entry.Path, Source: plan.source, Library: plan.library}
		if err := a.saveTrash(&result, true); err != nil {
			return managementErr(ErrorConflict, err)
		}
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		for _, task := range result.Tasks {
			if task.State == download.StatePaused || task.State == download.StateCompleted || task.State == download.StateFailed {
				continue
			}
			client := rt.backends[task.Client].(trashClient)
			if err := client.Pause(ctx, task.ID); err != nil {
				result.Error = fmt.Sprintf("pause task %s/%s: %v", task.Client, task.ID, err)
				_ = a.saveTrash(&result, false)
				return nil
			}
		}
		return nil
	})
	return result, err
}

func (a *App) ListTrash(ctx context.Context) ([]TrashEntry, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	rt := a.snapshot()
	if rt == nil {
		return nil, errors.New("runtime unavailable")
	}
	var out []TrashEntry
	var failures []error
	for _, root := range []string{rt.bundle.Organizer.SeriesSource(), rt.bundle.Organizer.MovieSource()} {
		items, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, item := range items {
			if !item.IsDir() || strings.HasPrefix(item.Name(), ".") {
				continue
			}
			path := filepath.Join(root, item.Name(), trashFile)
			if info, statErr := os.Lstat(path); statErr == nil {
				if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
					failures = append(failures, fmt.Errorf("unsafe trash marker: %s", path))
					continue
				}
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				failures = append(failures, statErr)
				continue
			}
			data, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				failures = append(failures, err)
				continue
			}
			var entry TrashEntry
			if err := jsonfile.Decode(data, &entry); err != nil {
				failures = append(failures, fmt.Errorf("read trash %s: %w", path, err))
				continue
			}
			if err := a.verifyTrash(entry); err != nil {
				failures = append(failures, fmt.Errorf("read trash %s: %w", path, err))
				continue
			}
			if entry.SourceRoot != filepath.Dir(path) {
				failures = append(failures, fmt.Errorf("trash source path changed: %s", path))
				continue
			}
			media := catalog.MediaSeries
			if root == rt.bundle.Organizer.MovieSource() {
				media = catalog.MediaMovie
			}
			if entry.MediaType != media || entry.Key != media+"/"+item.Name() {
				failures = append(failures, fmt.Errorf("trash identity does not match source directory: %s", path))
				continue
			}
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, errors.Join(failures...)
}

func (a *App) RestoreTrash(ctx context.Context, key string) ([]string, error) {
	var warnings []string
	err := a.withOperation(ctx, func() error {
		entries, err := a.ListTrash(ctx)
		if err != nil {
			return err
		}
		var entry *TrashEntry
		for i := range entries {
			if entries[i].Key == key {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			return managementErr(ErrorNotFound, fmt.Errorf("trash entry %q not found", key))
		}
		if entry.Status != "trashed" {
			return managementErr(ErrorConflict, errors.New("permanent deletion has started; this work cannot be restored"))
		}
		if side, err := catalog.ReadDeclaration(entry.SourceRoot); err != nil || side == nil {
			return managementErr(ErrorConflict, errors.Join(err, errors.New("work declaration is unavailable")))
		}
		marker := filepath.Join(entry.SourceRoot, trashFile)
		if err := os.Remove(marker); err != nil {
			return err
		}
		if err := a.reloadRuntime(); err != nil {
			_ = a.saveTrash(entry, true)
			_ = a.reloadRuntime()
			return err
		}
		rt := a.snapshot()
		for _, task := range entry.Tasks {
			if task.State == download.StatePaused || task.State == download.StateCompleted || task.State == download.StateFailed {
				continue
			}
			client, ok := rt.backends[task.Client].(trashClient)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("downloader %s is unavailable", task.Client))
				continue
			}
			items, err := client.List(ctx)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("list downloader %s: %v", task.Client, err))
				continue
			}
			var current *download.Task
			for i := range items {
				if items[i].ID == task.ID {
					current = &items[i]
					break
				}
			}
			if current == nil {
				warnings = append(warnings, fmt.Sprintf("task %s/%s no longer exists", task.Client, task.ID))
				continue
			}
			if current.InfoHash != task.InfoHash || download.MapPath(current.ContentPath, rt.bundle.ClientPathMappings(task.Client)) != task.ContentPath {
				warnings = append(warnings, fmt.Sprintf("task %s/%s changed; it was not resumed", task.Client, task.ID))
				continue
			}
			if current.State == download.StatePaused {
				if err := client.Resume(ctx, task.ID); err != nil {
					warnings = append(warnings, fmt.Sprintf("resume task %s/%s: %v", task.Client, task.ID, err))
				}
			}
		}
		return nil
	})
	return warnings, err
}

func (a *App) PurgeTrash(ctx context.Context, key string) (TrashEntry, error) {
	var result TrashEntry
	err := a.withOperation(ctx, func() error {
		entries, err := a.ListTrash(ctx)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Key == key {
				result = entry
				break
			}
		}
		if result.Key == "" {
			return managementErr(ErrorNotFound, fmt.Errorf("trash entry %q not found", key))
		}
		return a.purgeTrashLocked(ctx, &result)
	})
	return result, err
}

func (a *App) purgeTrashLocked(ctx context.Context, entry *TrashEntry) error {
	marker := filepath.Join(entry.SourceRoot, trashFile)
	rt := a.snapshot()
	if rt == nil {
		return errors.New("runtime unavailable")
	}
	if !under(rt.bundle.Organizer.Source, entry.SourceRoot) || !under(rt.bundle.Organizer.Target, entry.LibraryRoot) {
		return managementErr(ErrorConflict, errors.New("trash paths are outside configured roots"))
	}
	currentSource, err := recordTree(entry.SourceRoot, map[string]bool{".aninode.json": true, trashFile: true})
	if err != nil {
		return err
	}
	currentLibrary, err := recordTree(entry.LibraryRoot, nil)
	if err != nil {
		return err
	}
	if err := checkTrashFiles(entry.Source, currentSource); err != nil {
		return err
	}
	if err := checkTrashFiles(entry.Library, currentLibrary); err != nil {
		return err
	}
	sourceIDs := map[filesystem.ObjectID]bool{}
	for _, file := range entry.Source {
		sourceIDs[file.ID] = true
	}
	observedTasks, err := observeTrashTasks(ctx, rt, entry.SourceRoot, sourceIDs)
	if err != nil {
		return err
	}
	knownTasks := map[string]bool{}
	for _, task := range entry.Tasks {
		knownTasks[task.Client+"\x00"+task.ID] = true
	}
	for _, task := range observedTasks {
		if !knownTasks[task.Client+"\x00"+task.ID] {
			return managementErr(ErrorConflict, fmt.Errorf("new downloader task uses trashed work: %s/%s", task.Client, task.ID))
		}
	}
	// Verify every task before changing any more files. Never let a downloader
	// recursively delete data outside the reviewed manifest.
	currentTasks := make(map[string]download.Task)
	listedClients := make(map[string]bool)
	for _, task := range entry.Tasks {
		client, ok := rt.backends[task.Client].(trashClient)
		if !ok {
			return managementErr(ErrorConflict, fmt.Errorf("downloader %s unavailable", task.Client))
		}
		clientKey := task.Client + "\x00"
		if !listedClients[task.Client] {
			items, err := client.List(ctx)
			if err != nil {
				return err
			}
			listedClients[task.Client] = true
			for _, item := range items {
				currentTasks[clientKey+item.ID] = item
			}
		}
		if item, exists := currentTasks[clientKey+task.ID]; exists {
			mappedPath := download.MapPath(item.ContentPath, rt.bundle.ClientPathMappings(task.Client))
			if item.InfoHash != task.InfoHash || mappedPath != task.ContentPath {
				return managementErr(ErrorConflict, fmt.Errorf("task %s/%s changed", task.Client, task.ID))
			}
		}
	}
	if entry.Status != "purging" {
		entry.Status = "purging"
		if err := a.saveTrash(entry, false); err != nil {
			return err
		}
	}
	for _, task := range entry.Tasks {
		client := rt.backends[task.Client].(trashClient)
		if _, exists := currentTasks[task.Client+"\x00"+task.ID]; exists {
			if err := client.Remove(ctx, task.ID, false); err != nil {
				return err
			}
		}
	}
	for _, file := range entry.Library {
		if err := filesystem.RemoveObservedLink(rt.bundle.Organizer.Target, file.Path, file.ID); err != nil {
			return err
		}
	}
	for _, file := range entry.Source {
		if err := filesystem.RemoveObservedLink(rt.bundle.Organizer.Source, file.Path, file.ID); err != nil {
			return err
		}
	}
	if err := pruneTrashDirectories(rt.bundle.Organizer.Target, entry.LibraryRoot); err != nil {
		return err
	}
	if err := pruneTrashDirectories(rt.bundle.Organizer.Source, entry.SourceRoot); err != nil {
		return err
	}
	remainingSource, err := recordTree(entry.SourceRoot, map[string]bool{".aninode.json": true, trashFile: true})
	if err != nil {
		return err
	}
	remainingLibrary, err := recordTree(entry.LibraryRoot, nil)
	if err != nil {
		return err
	}
	if len(remainingSource) > 0 || len(remainingLibrary) > 0 {
		return managementErr(ErrorConflict, errors.New("new files appeared while deleting; trash remains for review"))
	}
	// The declaration is removed only after all reviewed media paths are gone.
	if err := os.Remove(filepath.Join(entry.SourceRoot, ".aninode.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(marker); err != nil {
		return err
	}
	if err := filesystem.RemoveEmptyParents(rt.bundle.Organizer.Source, entry.SourceRoot); err != nil {
		_ = a.saveTrash(entry, true)
		return err
	}
	if _, err := os.Lstat(entry.SourceRoot); err == nil {
		_ = a.saveTrash(entry, true)
		return managementErr(ErrorConflict, errors.New("source directory is not empty after file deletion"))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = filesystem.RemoveEmptyParents(rt.bundle.Organizer.Target, entry.LibraryRoot)
	return a.reloadRuntime()
}

func pruneTrashDirectories(root, workRoot string) error {
	var dirs []string
	err := filepath.WalkDir(workRoot, func(path string, item fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() && path != workRoot {
			dirs = append(dirs, path)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err := filesystem.RemoveEmptyParents(root, dir); err != nil {
			return err
		}
	}
	return nil
}

func checkTrashFiles(expected, current []trashFileRecord) error {
	known := map[string]trashFileRecord{}
	for _, file := range expected {
		known[file.Path] = file
	}
	for _, file := range current {
		old, ok := known[file.Path]
		if !ok || old.ID != file.ID || old.Size != file.Size || old.Modified != file.Modified {
			return managementErr(ErrorConflict, fmt.Errorf("file changed since deletion was confirmed: %s", file.Path))
		}
	}
	return nil
}

func (a *App) purgeExpiredTrashLocked(ctx context.Context) []error {
	entries, err := a.ListTrash(ctx)
	var failures []error
	if err != nil {
		failures = append(failures, err)
	}
	for _, entry := range entries {
		if a.clock().Before(entry.ExpiresAt) {
			if err := a.pauseTrashedTasksLocked(ctx, &entry); err != nil {
				failures = append(failures, fmt.Errorf("pause trashed work %s: %w", entry.Key, err))
			}
			continue
		}
		if err := a.purgeTrashLocked(ctx, &entry); err != nil {
			failures = append(failures, fmt.Errorf("purge %s: %w", entry.Key, err))
			entry.Error = err.Error()
			_ = a.saveTrash(&entry, false)
		}
	}
	return failures
}

func (a *App) pauseTrashedTasksLocked(ctx context.Context, entry *TrashEntry) error {
	if entry.Status != "trashed" {
		return nil
	}
	rt := a.snapshot()
	for _, task := range entry.Tasks {
		client, ok := rt.backends[task.Client].(trashClient)
		if !ok {
			return fmt.Errorf("downloader %s unavailable", task.Client)
		}
		items, err := client.List(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.ID != task.ID {
				continue
			}
			if item.InfoHash != task.InfoHash || download.MapPath(item.ContentPath, rt.bundle.ClientPathMappings(task.Client)) != task.ContentPath {
				return fmt.Errorf("task %s/%s changed", task.Client, task.ID)
			}
			if item.State != download.StatePaused && item.State != download.StateCompleted && item.State != download.StateFailed {
				if err := client.Pause(ctx, item.ID); err != nil {
					return err
				}
			}
		}
	}
	if entry.Error != "" {
		entry.Error = ""
		return a.saveTrash(entry, false)
	}
	return nil
}

func (a *App) excludeTrashedMigrationPlans(ctx context.Context, plans []migration.Plan) ([]migration.Plan, error) {
	trash, err := a.ListTrash(ctx)
	if err != nil {
		return nil, err
	}
	blocked := map[string]bool{}
	for _, entry := range trash {
		for _, task := range entry.Tasks {
			blocked[task.Client+"\x00"+task.ID] = true
		}
	}
	out := make([]migration.Plan, 0, len(plans))
	for _, plan := range plans {
		if !blocked[plan.Client+"\x00"+plan.TaskID] {
			out = append(out, plan)
		}
	}
	return out, nil
}
