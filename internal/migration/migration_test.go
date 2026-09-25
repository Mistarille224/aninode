package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/medianame"
	"aninode/internal/moviebundle"
	"aninode/internal/organizer"
	"aninode/internal/releasefilter"
)

type concurrentFilesBackend struct {
	tasks    []download.Task
	entered  chan struct{}
	release  chan struct{}
	inFlight atomic.Int32
	max      atomic.Int32
}

func (b *concurrentFilesBackend) Name() string { return "qbit" }
func (b *concurrentFilesBackend) Get(_ context.Context, id string) (download.Task, error) {
	for _, task := range b.tasks {
		if task.ID == id {
			return task, nil
		}
	}
	return download.Task{}, errors.New("not found")
}
func (b *concurrentFilesBackend) List(context.Context) ([]download.Task, error) {
	return append([]download.Task(nil), b.tasks...), nil
}
func (b *concurrentFilesBackend) FilesForTask(ctx context.Context, task download.Task) ([]download.File, error) {
	current := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for {
		old := b.max.Load()
		if current <= old || b.max.CompareAndSwap(old, current) {
			break
		}
	}
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return []download.File{{Path: filepath.Join(task.SavePath, task.ID+".mkv"), Wanted: true}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *concurrentFilesBackend) Files(ctx context.Context, id string) ([]download.File, error) {
	task, err := b.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return b.FilesForTask(ctx, task)
}

type fakeBackend struct {
	name                                                         string
	task                                                         download.Task
	files                                                        []download.File
	listCalls, fileCalls, pauseCalls, relocateCalls, resumeCalls int
	progressAfterRelocate                                        float64
	copyOnRelocate                                               bool
	checkingAfterRelocate                                        bool
	getCalls                                                     int
}

type sharedBackend struct {
	tasks map[string]download.Task
	files map[string][]download.File
}

func (f *sharedBackend) Name() string { return "client" }
func (f *sharedBackend) Get(_ context.Context, id string) (download.Task, error) {
	v, ok := f.tasks[id]
	if !ok {
		return download.Task{}, fmt.Errorf("not found")
	}
	return v, nil
}
func (f *sharedBackend) List(context.Context) ([]download.Task, error) {
	var out []download.Task
	for _, v := range f.tasks {
		out = append(out, v)
	}
	return out, nil
}
func (f *sharedBackend) Files(_ context.Context, id string) ([]download.File, error) {
	return append([]download.File(nil), f.files[id]...), nil
}
func (f *sharedBackend) Pause(_ context.Context, id string) error {
	v := f.tasks[id]
	v.State = download.StatePaused
	f.tasks[id] = v
	return nil
}
func (f *sharedBackend) Resume(_ context.Context, id string) error {
	v := f.tasks[id]
	v.State = download.StateSeeding
	f.tasks[id] = v
	return nil
}
func (f *sharedBackend) Relocate(_ context.Context, id, destination string) error {
	for i, file := range f.files[id] {
		target := filepath.Join(destination, filepath.Base(file.Path))
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}
		if err := os.Rename(file.Path, target); err != nil {
			return err
		}
		f.files[id][i].Path = target
	}
	v := f.tasks[id]
	v.SavePath = destination
	f.tasks[id] = v
	return nil
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Get(context.Context, string) (download.Task, error) {
	f.getCalls++
	return f.task, nil
}
func (f *fakeBackend) List(context.Context) ([]download.Task, error) {
	f.listCalls++
	return []download.Task{f.task}, nil
}
func (f *fakeBackend) Files(context.Context, string) ([]download.File, error) {
	f.fileCalls++
	return append([]download.File(nil), f.files...), nil
}
func (f *fakeBackend) Pause(context.Context, string) error {
	f.pauseCalls++
	f.task.State = download.StatePaused
	return nil
}
func (f *fakeBackend) Resume(context.Context, string) error {
	f.resumeCalls++
	f.task.State = download.StateSeeding
	return nil
}
func (f *fakeBackend) Relocate(_ context.Context, _ string, path string) error {
	f.relocateCalls++
	oldSave := f.task.SavePath
	for i := range f.files {
		rel, err := filepath.Rel(oldSave, f.files[i].Path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("fake file outside save path")
		}
		to := filepath.Join(path, rel)
		if err := os.MkdirAll(filepath.Dir(to), 0755); err != nil {
			return err
		}
		if !sameCleanPath(f.files[i].Path, to) {
			if f.copyOnRelocate {
				data, err := os.ReadFile(f.files[i].Path)
				if err != nil {
					return err
				}
				if err := os.WriteFile(to, data, 0o644); err != nil {
					return err
				}
				if err := os.Remove(f.files[i].Path); err != nil {
					return err
				}
			} else if err := os.Rename(f.files[i].Path, to); err != nil {
				return err
			}
		}
		f.files[i].Path = to
		if f.progressAfterRelocate >= 0 {
			f.files[i].Progress = f.progressAfterRelocate
		}
	}
	if f.task.ContentPath != "" {
		rel, err := filepath.Rel(oldSave, f.task.ContentPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("fake content path outside save path")
		}
		f.task.ContentPath = filepath.Join(path, rel)
	}
	f.task.SavePath = path
	if f.checkingAfterRelocate {
		f.task.State = download.StateChecking
	}
	return nil
}

func TestWaitForTaskUsesTargetedGetWithoutList(t *testing.T) {
	backend := &fakeBackend{task: download.Task{ID: "one", State: download.StatePaused}}
	if err := waitForTask(context.Background(), backend, "one", func(task download.Task) bool { return task.State == download.StatePaused }); err != nil {
		t.Fatal(err)
	}
	if backend.listCalls != 0 || backend.getCalls < 1 {
		t.Fatalf("List=%d Get=%d", backend.listCalls, backend.getCalls)
	}
}

func TestManagedNonCanonicalSeriesTaskIsOmittedBeforePublication(t *testing.T) {
	root := t.TempDir()
	localDownloads := filepath.Join(root, "usb", "downloads")
	entryRoot := filepath.Join(localDownloads, "TV", "abcabc149")
	container := filepath.Join(entryRoot, "[grpabc165] S01 Batch")
	localFile := filepath.Join(container, "abcabc149 S01E01.mkv")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localFile, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	remoteRoot := "/downloads/TV/abcabc149"
	remoteContainer := remoteRoot + "/[grpabc165] S01 Batch"
	remoteFile := remoteContainer + "/abcabc149 S01E01.mkv"
	w := catalog.Entry{Key: "series/abcabc149", MediaType: catalog.MediaSeries, Title: "abcabc149", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc149"), Seasons: []int{1}, Enabled: true}
	bundle := configstore.Bundle{
		Organizer: organizer.Config{Source: localDownloads, Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}},
		Entries:   map[string]catalog.Entry{w.Key: w},
		Clients: map[string]configstore.Client{"client": {
			ID: "client", Enabled: true,
			PathMappings: []configstore.PathMapping{{Remote: "/downloads", Local: localDownloads}},
		}},
	}
	backend := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "mapped", Name: "abcabc149 S01", InfoHash: "0123456789abcdef0123456789abcdef01234567", SavePath: remoteRoot, ContentPath: remoteContainer, State: download.StateSeeding}, files: []download.File{{Path: remoteFile, Size: 5, Completed: 5, Progress: 1, Wanted: true}}}
	e := Engine{Bundle: bundle, Clients: map[string]download.Backend{"client": backend}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("managed source task was offered for migration: plans=%+v err=%v", plans, err)
	}
	if backend.relocateCalls != 0 {
		t.Fatalf("managed source task was relocated: calls=%d", backend.relocateCalls)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "Season 01", "abcabc149 S01E01.mkv")); !os.IsNotExist(err) {
		t.Fatalf("migration scan unexpectedly performed publication: %v", err)
	}
}

func TestScanBoundsConcurrentFileObservation(t *testing.T) {
	root := t.TempDir()
	backend := &concurrentFilesBackend{entered: make(chan struct{}, 20), release: make(chan struct{}, 20)}
	for i := 0; i < 20; i++ {
		backend.tasks = append(backend.tasks, download.Task{ID: fmt.Sprintf("%02d", i), SavePath: root})
	}
	done := make(chan error, 1)
	go func() {
		_, err := (Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: root, Extensions: []string{".mkv"}}}, Clients: map[string]download.Backend{"qbit": backend}}).Scan(context.Background())
		done <- err
	}()
	for i := 0; i < migrationFileObservationWorkers; i++ {
		<-backend.entered
	}
	if got := backend.max.Load(); got <= 1 || got > migrationFileObservationWorkers {
		t.Fatalf("max concurrency=%d", got)
	}
	for i := 0; i < 20; i++ {
		backend.release <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := backend.max.Load(); got > migrationFileObservationWorkers {
		t.Fatalf("max concurrency=%d", got)
	}
}

func TestObserveClientSkipsPerTaskFilesForManagedContentPath(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	entryRoot := filepath.Join(sourceRoot, "TV", "abcabc146")
	contentRoot := filepath.Join(entryRoot, "Season 01", "Batch")
	if err := os.MkdirAll(contentRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{name: "qbit", task: download.Task{ID: "managed", Name: "abcabc146 S01", SavePath: filepath.Join(entryRoot, "Season 01"), ContentPath: contentRoot}}
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", DeclarationPath: entryRoot}
	engine := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: sourceRoot}, Entries: map[string]catalog.Entry{entry.Key: entry}}}

	observed, err := engine.observeClient(context.Background(), "qbit", backend)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 0 || backend.fileCalls != 0 {
		t.Fatalf("observed=%d fileCalls=%d, want managed task filtered before Files", len(observed), backend.fileCalls)
	}
}

func TestScanManagedContentPathsAvoidNamingNamespaceWalk(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	entryRoot := filepath.Join(sourceRoot, "TV", "abcabc146")
	contentRoot := filepath.Join(entryRoot, "Season 01", "Batch")
	if err := os.MkdirAll(contentRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{name: "qbit", task: download.Task{ID: "managed", Name: "abcabc146 S01", SavePath: filepath.Join(entryRoot, "Season 01"), ContentPath: contentRoot}}
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", DeclarationPath: entryRoot}
	engine := Engine{
		Bundle: configstore.Bundle{
			Organizer: organizer.Config{Source: sourceRoot, Target: filepath.Join(string(filepath.Separator), "outside-aninode-library")},
			Entries:   map[string]catalog.Entry{entry.Key: entry},
		},
		Clients: map[string]download.Backend{"qbit": backend},
	}

	plans, err := engine.Scan(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if backend.fileCalls != 0 {
		t.Fatalf("fileCalls=%d, want zero", backend.fileCalls)
	}
}

func TestObserveClientFallsBackToFilesWhenContentPathCannotBeVerified(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	entryRoot := filepath.Join(sourceRoot, "TV", "abcabc146")
	if err := os.MkdirAll(entryRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{name: "qbit", task: download.Task{ID: "uncertain", Name: "abcabc146 S01", SavePath: entryRoot, ContentPath: filepath.Join(entryRoot, "missing")}}
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", DeclarationPath: entryRoot}
	engine := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: sourceRoot}, Entries: map[string]catalog.Entry{entry.Key: entry}}}

	observed, err := engine.observeClient(context.Background(), "qbit", backend)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || backend.fileCalls != 1 {
		t.Fatalf("observed=%d fileCalls=%d, want conservative Files fallback", len(observed), backend.fileCalls)
	}
}

func testEngine(t *testing.T, episodeCount int) (Engine, *fakeBackend, string, string, Plan) {
	t.Helper()
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	source := filepath.Join(sourceRoot, "TV")
	target := filepath.Join(targetRoot, "TV")
	old := filepath.Join(source, "incoming")
	for _, p := range []string{source, target, old} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	id := "series/abcabc146"
	w := catalog.Entry{Key: id, Title: "abcabc146", MediaType: catalog.MediaSeries, Path: filepath.Join(target, "abcabc146"), Seasons: []int{1}}
	if err := os.MkdirAll(filepath.Join(w.Path, "Season 01"), 0755); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "task", Name: "abcabc146 S01", InfoHash: "0123456789abcdef0123456789abcdef01234567", SavePath: old, State: download.StateSeeding}}
	for ep := 1; ep <= episodeCount; ep++ {
		p := filepath.Join(old, fmt.Sprintf("abcabc146 S01E%02d.mkv", ep))
		data := []byte(fmt.Sprintf("episode-%d", ep))
		if err := os.WriteFile(p, data, 0644); err != nil {
			t.Fatal(err)
		}
		b.files = append(b.files, download.File{Path: p, Size: int64(len(data)), Completed: int64(len(data)), Progress: 1, Wanted: true})
	}
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: sourceRoot, Target: targetRoot, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}, Clients: map[string]configstore.Client{"client": {ID: "client", Type: "qbittorrent", Enabled: true}}}
	e := Engine{Bundle: bundle, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionRelocate {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	return e, b, source, target, plans[0]
}

func TestMigrationConvergesAndRestartScanIsEmpty(t *testing.T) {
	e, b, _, target, p := testEngine(t, 1)
	if _, err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	restarted := Engine{Bundle: e.Bundle, Clients: e.Clients}
	plans, err := restarted.Scan(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("second scan=%+v err=%v", plans, err)
	}
	if b.relocateCalls != 1 {
		t.Fatalf("relocate calls=%d", b.relocateCalls)
	}
	dst := filepath.Join(target, "abcabc146", "Season 01", "abcabc146 S01E01.mkv")
	src := filepath.Join(p.DesiredPath, "abcabc146 S01E01.mkv")
	a, _ := os.Stat(src)
	z, err := os.Stat(dst)
	if err != nil || !os.SameFile(a, z) {
		t.Fatalf("publication: %v", err)
	}
}

func TestMigrationRejectsDownloaderCopyRecreateRelocation(t *testing.T) {
	e, backend, _, _, plan := testEngine(t, 1)
	backend.copyOnRelocate = true
	_, err := e.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "replaced physical filesystem objects") {
		t.Fatalf("copy/recreate relocation was accepted: %v", err)
	}
}

func TestManagedCanonicalTaskStillRequiresTopologyObservation(t *testing.T) {
	e, b, _, _, p := testEngine(t, 1)
	old := b.files[0].Path
	b.task.SavePath = p.DesiredPath
	b.files[0].Path = filepath.Join(p.DesiredPath, filepath.Base(old))
	_ = os.MkdirAll(p.DesiredPath, 0755)
	_ = os.Rename(old, b.files[0].Path)
	b.fileCalls = 0
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 0 || b.fileCalls != 1 {
		t.Fatalf("managed task should be observed once then omitted: plans=%+v files=%d err=%v", plans, b.fileCalls, err)
	}
}

func TestIncompleteTaskInsideManagedEntryIsNotMigrationWork(t *testing.T) {
	e, b, _, _, p := testEngine(t, 1)
	old := b.files[0].Path
	b.task.SavePath = p.DesiredPath
	b.files[0].Path = filepath.Join(p.DesiredPath, filepath.Base(old))
	b.files[0].Completed = 0
	b.files[0].Progress = 0.2
	b.task.Progress = 0.2
	b.task.State = download.StateDownloading
	if err := os.MkdirAll(p.DesiredPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, b.files[0].Path); err != nil {
		t.Fatal(err)
	}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("incomplete task already inside Entry source was offered for migration: plans=%+v err=%v", plans, err)
	}
}

func TestRelocateCheckingLowProgressUsesPresentFiles(t *testing.T) {
	e, b, _, target, p := testEngine(t, 3)
	b.progressAfterRelocate = .2
	b.checkingAfterRelocate = true
	if _, err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for ep := 1; ep <= 3; ep++ {
		if _, err := os.Stat(filepath.Join(target, "abcabc146", "Season 01", fmt.Sprintf("abcabc146 S01E%02d.mkv", ep))); err != nil {
			t.Fatalf("episode %d: %v", ep, err)
		}
	}
}

func TestRepeatedApplyIsIdempotent(t *testing.T) {
	e, b, _, _, p := testEngine(t, 1)
	if _, err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if b.relocateCalls != 1 {
		t.Fatalf("relocate calls=%d", b.relocateCalls)
	}
}

func TestPresentWantedMediaDoesNotTrustProgress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "episode.mkv")
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	got := presentWantedSeriesMedia([]download.File{{Path: p, Size: 1, Progress: .1, Wanted: true}}, []string{".mkv"})
	if len(got) != 1 {
		t.Fatalf("present=%v", got)
	}
}

func TestMovieAssetCohortPreservesBundleMembersWhileSeriesFilters(t *testing.T) {
	root := t.TempDir()
	names := []string{"abcabc147.mkv", "abcabc147.zh.ass", "abcabc147.ja.mka", "abcabc147.bluray.iso", "trailers/trailer.mkv", "BDMV/index.bdmv", "BDMV/PLAYLIST/00001.mpls", "BDMV/CLIPINF/00001.clpi", "BDMV/STREAM/00001.m2ts", "VIDEO_TS/VIDEO_TS.IFO", "VIDEO_TS/VTS_01_0.BUP", "VIDEO_TS/VTS_01_1.VOB"}
	var input []download.File
	for _, name := range names {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		data := []byte(name)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		input = append(input, download.File{Path: path, Size: int64(len(data)), Wanted: true})
	}
	movie := presentWantedMovieAssets(input)
	if len(movie) != len(names) {
		t.Fatalf("movie cohort=%d want=%d", len(movie), len(names))
	}
	series := presentWantedSeriesMedia(input, []string{".mkv", ".mp4"})
	if len(series) != 2 {
		t.Fatalf("series cohort=%v", series)
	}
	for _, tc := range []struct {
		name  string
		paths []string
		kind  moviebundle.Kind
	}{{"folder", []string{"abcabc147.mkv", "abcabc147.zh.ass", "abcabc147.ja.mka"}, moviebundle.Files}, {"iso", []string{"abcabc147.bluray.iso"}, moviebundle.ISO}, {"bluray", []string{"BDMV/index.bdmv", "BDMV/PLAYLIST/00001.mpls", "BDMV/CLIPINF/00001.clpi", "BDMV/STREAM/00001.m2ts"}, moviebundle.BluRay}, {"dvd", []string{"VIDEO_TS/VIDEO_TS.IFO", "VIDEO_TS/VTS_01_0.BUP", "VIDEO_TS/VTS_01_1.VOB"}, moviebundle.DVD}, {"extras", []string{"abcabc147.mkv", "trailers/trailer.mkv"}, moviebundle.Files}} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			for _, rel := range tc.paths {
				paths = append(paths, filepath.Join(root, filepath.FromSlash(rel)))
			}
			b, err := moviebundle.AnalyzePaths(root, paths, moviebundle.Options{Title: "abcabc147"})
			if err != nil || b.Kind != tc.kind {
				t.Fatalf("bundle=%+v err=%v", b, err)
			}
			if tc.name == "bluray" && len(b.OpaqueTree.Assets) != 4 {
				t.Fatalf("opaque assets=%+v", b.OpaqueTree.Assets)
			}
			if tc.name == "dvd" && len(b.OpaqueTree.Assets) != 3 {
				t.Fatalf("opaque assets=%+v", b.OpaqueTree.Assets)
			}
			if tc.name == "extras" && (len(b.Extras) != 1 || b.Extras[0].Asset.Relative != "trailers/trailer.mkv") {
				t.Fatalf("extras=%+v", b.Extras)
			}
		})
	}
}

func TestMovieMigrationScanAcceptsCompleteBundleCohorts(t *testing.T) {
	cases := []struct {
		name  string
		files []string
	}{{"loose", []string{"abcabc147.2024.mkv"}}, {"sidecars", []string{"abcabc147.mkv", "abcabc147.zh.ass", "abcabc147.ja.mka"}}, {"versions", []string{"abcabc147.1080p.mkv", "abcabc147.2160p.mkv"}}, {"extras", []string{"abcabc147.mkv", "trailers/trailer.mkv"}}, {"iso", []string{"abcabc147.bluray.iso"}}, {"bluray", []string{"BDMV/index.bdmv", "BDMV/MovieObject.bdmv", "BDMV/PLAYLIST/00001.mpls", "BDMV/CLIPINF/00001.clpi", "BDMV/STREAM/00001.m2ts"}}, {"dvd", []string{"VIDEO_TS/VIDEO_TS.IFO", "VIDEO_TS/VIDEO_TS.BUP", "VIDEO_TS/VTS_01_1.VOB"}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
			incoming := filepath.Join(downloads, "incoming")
			canonical := filepath.Join(downloads, "Movies", "abcabc147 (2024)")
			target := filepath.Join(library, "Movies", "abcabc147 (2024)")
			if err := os.MkdirAll(incoming, 0755); err != nil {
				t.Fatal(err)
			}
			backend := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "movie-task", Name: "abcabc147 2024", InfoHash: "0123456789abcdef0123456789abcdef01234567", SavePath: incoming, ContentPath: incoming, State: download.StateSeeding}}
			for _, rel := range tc.files {
				p := filepath.Join(incoming, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				data := []byte(rel)
				if err := os.WriteFile(p, data, 0644); err != nil {
					t.Fatal(err)
				}
				backend.files = append(backend.files, download.File{Path: p, Size: int64(len(data)), Completed: int64(len(data)), Progress: 1, Wanted: true})
			}
			id := "movie/abcabc147 (2024)"
			w := catalog.Entry{Key: id, MediaType: catalog.MediaMovie, Title: "abcabc147", Year: 2024, DeclarationPath: canonical, Path: target, Enabled: true}
			bundle := configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv", ".mp4"}}, Entries: map[string]catalog.Entry{id: w}}
			engine := Engine{Bundle: bundle, Clients: map[string]download.Backend{"client": backend}}
			plans, err := engine.Scan(context.Background())
			if err != nil || len(plans) != 1 || plans[0].Decision != DecisionRelocate || plans[0].MediaType != catalog.MediaMovie {
				t.Fatalf("plans=%+v err=%v", plans, err)
			}
		})
	}
}

func TestLooseSingleFileMovieUsesOperationalSourceContainer(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	incoming := filepath.Join(downloads, "incoming")
	canonical := filepath.Join(downloads, "Movies", "abcabc147 (2024)")
	target := filepath.Join(library, "Movies", "abcabc147 (2024)")
	if err := os.MkdirAll(incoming, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(incoming, "abcabc147.2024.mkv")
	data := []byte("movie")
	if err := os.WriteFile(source, data, 0644); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "movie-task", Name: "abcabc147 2024", InfoHash: "0123456789abcdef0123456789abcdef01234567", SavePath: incoming, State: download.StateSeeding}, files: []download.File{{Path: source, Size: int64(len(data)), Completed: int64(len(data)), Progress: 1, Wanted: true}}}
	id := "movie/abcabc147 (2024)"
	w := catalog.Entry{Key: id, MediaType: catalog.MediaMovie, Title: "abcabc147", Year: 2024, DeclarationPath: canonical, Path: target, Enabled: true}
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}}
	engine := Engine{Bundle: bundle, Clients: map[string]download.Backend{"client": backend}}
	plans, err := engine.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionRelocate {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	result, err := engine.Apply(context.Background(), plans[0])
	if err != nil || result.Organized.Linked != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	canonicalSource := filepath.Join(downloads, "Movies", "abcabc147 (2024)", "abcabc147.2024.mkv")
	published := filepath.Join(target, "abcabc147 (2024).mkv")
	a, err := os.Stat(canonicalSource)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(published)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("publication is not a hardlink to the physical source")
	}
}

func TestExistingMultifileMovieCannotRenameTorrentRootToMatchDeclaration(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	save := filepath.Join(downloads, "movie")
	torrentRoot := filepath.Join(save, "abcabc147.2024.Bundle")
	canonicalSave := filepath.Join(downloads, "Movies", "abcabc147 (2024)")
	target := filepath.Join(library, "Movies", "abcabc147 (2024)")
	names := []string{"abcabc147.2024.mkv", "abcabc147.2024.mka", "abcabc147.2024.ass", "CDs/disc.flac", "Scans/001.jpg", "SPs/trailer.mkv", "Fonts.zip"}
	b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "movie", InfoHash: "0123456789abcdef0123456789abcdef01234567", Name: "abcabc147 2024", SavePath: save, ContentPath: torrentRoot, State: download.StateSeeding}}
	for _, name := range names {
		path := filepath.Join(torrentRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		b.files = append(b.files, download.File{Path: path, Size: int64(len(name)), Completed: int64(len(name)), Progress: 1, Wanted: true})
	}
	w := catalog.Entry{Key: "movie/abcabc147 (2024)", MediaType: catalog.MediaMovie, Title: "abcabc147", Year: 2024, DeclarationPath: canonicalSave, Path: target, Enabled: true}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}}, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionConflict {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if !strings.Contains(plans[0].Detail, "required Entry source namespace") {
		t.Fatalf("conflict did not explain pinned Entry source root: %+v", plans[0])
	}
	if b.relocateCalls != 0 {
		t.Fatalf("conflicting movie root was relocated: %d", b.relocateCalls)
	}
}

func TestMultifileSeriesUsesContentRoot(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	save, content := filepath.Join(downloads, "anime"), filepath.Join(downloads, "anime", "abcabc146 S01")
	b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "series", InfoHash: "1123456789abcdef0123456789abcdef01234567", Name: "abcabc146 S01", SavePath: save, ContentPath: content, State: download.StateSeeding}}
	for ep := 1; ep <= 3; ep++ {
		path := filepath.Join(content, fmt.Sprintf("abcabc146 S01E%02d.mkv", ep))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte{byte(ep)}, 0o644); err != nil {
			t.Fatal(err)
		}
		b.files = append(b.files, download.File{Path: path, Size: 1, Completed: 1, Progress: 1, Wanted: true})
	}
	w := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Seasons: []int{1}, Path: filepath.Join(library, "TV", "abcabc146"), Enabled: true}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}}, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionRelocate {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if plans[0].ExpectedContentRoot == plans[0].DesiredPath {
		t.Fatalf("multifile root collapsed into save path: %+v", plans[0])
	}
	wantSave := filepath.Join(downloads, "TV", "abcabc146")
	wantContent := filepath.Join(wantSave, filepath.Base(content))
	if plans[0].DesiredPath != wantSave || plans[0].ExpectedContentRoot != wantContent {
		t.Fatalf("folder-shaped series did not preserve torrent container: plan=%+v wantSave=%q wantContent=%q", plans[0], wantSave, wantContent)
	}
	canonicalSource := filepath.Join(downloads, "TV", catalog.SeriesDirName(w.Title, w.Year))
	if _, err := os.Stat(canonicalSource); !os.IsNotExist(err) {
		t.Fatalf("planning materialized canonical source placeholder %q: %v", canonicalSource, err)
	}
}

func TestManagedNonCanonicalSeriesContainersAreOmitted(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	entryRoot := filepath.Join(downloads, "TV", "abcabc146")
	container := filepath.Join(entryRoot, "[grpabc165] S01 Batch")
	makeEngine := func(content string) (Engine, *fakeBackend) {
		b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "series", InfoHash: "1123456789abcdef0123456789abcdef01234567", Name: "abcabc146 S01", SavePath: filepath.Dir(content), ContentPath: content, State: download.StateSeeding}}
		for ep := 1; ep <= 2; ep++ {
			path := filepath.Join(content, fmt.Sprintf("abcabc146 S01E%02d.mkv", ep))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte{byte(ep)}, 0o644); err != nil {
				t.Fatal(err)
			}
			b.files = append(b.files, download.File{Path: path, Size: 1, Completed: 1, Progress: 1, Wanted: true})
		}
		w := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Seasons: []int{1}, DeclarationPath: entryRoot, Path: filepath.Join(library, "TV", "abcabc146"), Enabled: true}
		return Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}}, Clients: map[string]download.Backend{"client": b}}, b
	}
	e, _ := makeEngine(container)
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("managed non-canonical container was offered for migration: plans=%+v err=%v", plans, err)
	}
	deep := filepath.Join(entryRoot, "Season 01", "Batch")
	e, _ = makeEngine(deep)
	plans, err = e.Scan(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("managed grouped season was offered for migration: plans=%+v err=%v", plans, err)
	}
}

func TestExistingMovieContainerMismatchConflictsInsteadOfMovingDeclaration(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	container := filepath.Join(downloads, "Movies", "[grpabc165] abcabc139")
	path := filepath.Join(container, "abcabc147.2024.mkv")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{name: "client", task: download.Task{ID: "movie", InfoHash: "2123456789abcdef0123456789abcdef01234567", Name: "abcabc147 2024", SavePath: filepath.Dir(container), ContentPath: container, State: download.StateSeeding}, files: []download.File{{Path: path, Size: 5, Completed: 5, Progress: 1, Wanted: true}}}
	w := catalog.Entry{Key: "movie/abcabc147 (2024)", MediaType: catalog.MediaMovie, Title: "abcabc147", Year: 2024, DeclarationPath: filepath.Join(downloads, "Movies", "abcabc147 (2024)"), Path: filepath.Join(library, "Movies", "abcabc147 (2024)"), Enabled: true}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}}, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionConflict || !strings.Contains(plans[0].Detail, "required Entry source namespace") {
		t.Fatalf("existing movie declaration root was allowed to drift: plans=%+v err=%v", plans, err)
	}
}

func TestTaskContentRootFallbackRejectsNestedUnknownTopology(t *testing.T) {
	save := filepath.Join(t.TempDir(), "incoming")
	files := []download.File{{Path: filepath.Join(save, "unknown-root", "abcabc147.mkv"), Wanted: true}}
	if root, ok := taskContentRoot(download.Task{SavePath: save}, files); ok || root != "" {
		t.Fatalf("root=%q ok=%v", root, ok)
	}
}

func TestRelocationSafetyIsPerTaskFileNotSeasonDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "incoming")
	destination := filepath.Join(root, "abcabc (2004)", "Season 02")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	e41 := filepath.Join(source, "episode41.mkv")
	if err := os.WriteFile(e41, []byte("41"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "episode42.mkv"), []byte("42"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := download.Task{SavePath: source}
	files := []download.File{{Path: e41, Size: 2, Wanted: true}}
	if safe, known, detail := relocationDestinationSafe(task, files, destination, nil); !safe || !known {
		t.Fatalf("another episode blocked shared Season: safe=%v known=%v detail=%q", safe, known, detail)
	}
	if err := os.WriteFile(filepath.Join(destination, "episode41.mkv"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if safe, known, detail := relocationDestinationSafe(task, files, destination, nil); safe || !known || detail == "" {
		t.Fatalf("file conflict was not detected: safe=%v known=%v detail=%q", safe, known, detail)
	}
}

func TestThreeTasksShareCanonicalSeasonAndConvergeAfterRestart(t *testing.T) {
	root := t.TempDir()
	sourceRoot, libraryRoot := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	seasonSource := filepath.Join(sourceRoot, "TV", "abcabc (2004)", "Season 02")
	seasonLibrary := filepath.Join(libraryRoot, "TV", "abcabc (2004)", "Season 02")
	w := catalog.Entry{Key: "series/abcabc (2004)", MediaType: catalog.MediaSeries, Title: "abcabc", Year: 2004, Path: filepath.Dir(seasonLibrary), Seasons: []int{2}, Enabled: true}
	b := &sharedBackend{tasks: map[string]download.Task{}, files: map[string][]download.File{}}
	for _, ep := range []int{41, 42, 43} {
		id := fmt.Sprintf("task%d", ep)
		incoming := filepath.Join(sourceRoot, fmt.Sprintf("incoming%d", ep))
		if err := os.MkdirAll(incoming, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(incoming, fmt.Sprintf("abcabc S02E%d.mkv", ep))
		data := []byte(fmt.Sprintf("episode-%d", ep))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		b.tasks[id] = download.Task{ID: id, Name: fmt.Sprintf("abcabc S02E%d", ep), SavePath: incoming, State: download.StateSeeding, InfoHash: fmt.Sprintf("%040d", ep)}
		b.files[id] = []download.File{{Path: path, Size: int64(len(data)), Completed: int64(len(data)), Progress: 1, Wanted: true}}
	}
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: sourceRoot, Target: libraryRoot, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}, Clients: map[string]configstore.Client{"client": {ID: "client", Enabled: true}}}
	e := Engine{Bundle: bundle, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 3 {
		t.Fatalf("initial plans=%+v err=%v", plans, err)
	}
	for _, id := range []string{"task41", "task42", "task43"} {
		plans, err = e.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var chosen Plan
		for _, p := range plans {
			if p.TaskID == id {
				chosen = p
			}
		}
		if chosen.Decision != DecisionRelocate {
			t.Fatalf("%s after non-empty Season: %+v", id, chosen)
		}
		if _, err := e.Apply(context.Background(), chosen); err != nil {
			t.Fatal(err)
		}
	}
	restarted := Engine{Bundle: bundle, Clients: map[string]download.Backend{"client": b}}
	if plans, err := restarted.Scan(context.Background()); err != nil || len(plans) != 0 {
		t.Fatalf("restart plans=%+v err=%v", plans, err)
	}
	for _, ep := range []int{41, 42, 43} {
		source := filepath.Join(seasonSource, fmt.Sprintf("abcabc S02E%d.mkv", ep))
		target := filepath.Join(seasonLibrary, fmt.Sprintf("abcabc S02E%d.mkv", ep))
		a, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		z, err := os.Stat(target)
		if err != nil || !os.SameFile(a, z) {
			t.Fatalf("episode %d publication/inode: %v", ep, err)
		}
	}
}

func TestPlanConfirmedMovieDoesNotRevalidateParsedYearIdentity(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	existingSave := filepath.Join(downloads, "incoming")
	container := filepath.Join(existingSave, "Opaque.abcabc147.1999.Release")
	path := filepath.Join(container, "feature.mkv")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "movie", InfoHash: "6123456789abcdef0123456789abcdef01234567", Name: "abcabc038 1999", SavePath: existingSave, ContentPath: container, State: download.StateSeeding}, files: []download.File{{Path: path, Size: 5, Completed: 5, Progress: 1, Wanted: true}}}
	id := "movie/abcabc013 (2024)"
	w := catalog.Entry{Key: id, MediaType: catalog.MediaMovie, Title: "abcabc013", Year: 2024, Path: filepath.Join(library, "Movies", "abcabc013 (2024)"), Enabled: true}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}}, Clients: map[string]download.Backend{"client": b}}
	plan, err := e.PlanConfirmed(context.Background(), ConfirmedIntent{Client: "client", TaskID: "movie", InfoHash: b.task.InfoHash, EntryKey: id})
	if err != nil {
		t.Fatalf("user-confirmed identity was rejected by release year evidence: %v", err)
	}
	if plan.EntryKey != id || plan.Decision != DecisionRelocate {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestExistingSeriesOutsideEntryRootRelocatesInsteadOfMovingDeclaration(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	declarationRoot := filepath.Join(downloads, "TV", "abcabc019 (2024)")
	observedEntryRoot := filepath.Join(downloads, "TV", "[grpabc165] abcabc141")
	content := filepath.Join(observedEntryRoot, "[grpabc165] S01 Batch")
	path := filepath.Join(content, "abcabc019 S01E01.mkv")
	if err := os.MkdirAll(content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "series", InfoHash: "7123456789abcdef0123456789abcdef01234567", Name: "abcabc019 S01", SavePath: observedEntryRoot, ContentPath: content, State: download.StateSeeding}, files: []download.File{{Path: path, Size: 7, Completed: 7, Progress: 1, Wanted: true}}}
	w := catalog.Entry{Key: "series/abcabc019 (2024)", MediaType: catalog.MediaSeries, Title: "abcabc019", Year: 2024, Seasons: []int{1}, DeclarationPath: declarationRoot, Path: filepath.Join(library, "TV", "abcabc019 (2024)"), Enabled: true}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}}, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionRelocate {
		t.Fatalf("task outside durable Entry root did not require relocation: plans=%+v err=%v", plans, err)
	}
	if plans[0].DesiredPath != declarationRoot || !strings.HasPrefix(plans[0].ExpectedContentRoot, declarationRoot+string(filepath.Separator)) {
		t.Fatalf("relocation did not preserve existing Entry namespace: %+v", plans[0])
	}
}

func TestMigrationApplyUpdatesExplicitFiltersOnExistingEntry(t *testing.T) {
	root := t.TempDir()
	downloads, library := filepath.Join(root, "downloads"), filepath.Join(root, "library")
	declarationRoot := filepath.Join(downloads, "TV", "abcabc146")
	incoming := filepath.Join(downloads, "TV", "incoming")
	path := filepath.Join(incoming, "[grpabc166] abcabc146 S01E01 [1080p].mkv")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldFilters := releasefilter.Filters{Groups: []string{"grpabc157"}}
	if err := catalog.WriteDeclaration(declarationRoot, catalog.Declaration{Title: "abcabc146", Filters: oldFilters}); err != nil {
		t.Fatal(err)
	}
	w := catalog.EntryFromDeclaration(catalog.Declaration{Title: "abcabc146", Filters: oldFilters}, catalog.MediaSeries, "series/abcabc146", declarationRoot, filepath.Join(library, "TV"))
	w.Seasons = []int{1}
	w.Filters = releasefilter.Filters{Groups: []string{"grpabc166"}, Resolutions: []string{"1080p"}}
	b := &fakeBackend{name: "client", progressAfterRelocate: -1, task: download.Task{ID: "series", InfoHash: "8123456789abcdef0123456789abcdef01234567", Name: "[grpabc166] abcabc146 S01", SavePath: incoming, State: download.StateSeeding}, files: []download.File{{Path: path, Size: 7, Completed: 7, Progress: 1, Wanted: true}}}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: downloads, Target: library, Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{w.Key: w}}, Clients: map[string]download.Backend{"client": b}}
	plans, err := e.Scan(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Decision != DecisionRelocate {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if _, err := e.Apply(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	declared, err := catalog.ReadDeclaration(declarationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !declared.Filters.Equal(w.Filters) {
		t.Fatalf("persisted filters=%+v want=%+v", declared.Filters, w.Filters)
	}
}

func TestRelocationUsesSourceSeasonNotPublicationOffset(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	w := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, DeclarationPath: entryRoot}
	e := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library")}}}
	task := download.Task{SavePath: filepath.Join(root, "incoming")}
	contentRoot := task.SavePath
	got := e.relocationSavePath(w, task, contentRoot, 1, 2)
	want := filepath.Join(entryRoot, "Season 01")
	if got != want {
		t.Fatalf("relocation path=%q want source-season path %q", got, want)
	}
	if strings.Contains(got, "Season 02") {
		t.Fatalf("publication target season leaked into source relocation: %q", got)
	}
}

func TestManagedContentPathRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	entryRoot := filepath.Join(sourceRoot, "TV", "abcabc146")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "Batch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(entryRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(entryRoot, "Season 01")); err != nil {
		t.Fatal(err)
	}
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, DeclarationPath: entryRoot}
	engine := Engine{Bundle: configstore.Bundle{Organizer: organizer.Config{Source: sourceRoot}, Entries: map[string]catalog.Entry{entry.Key: entry}}}
	backend := &fakeBackend{name: "qbit", task: download.Task{ID: "uncertain", SavePath: entryRoot, ContentPath: filepath.Join(entryRoot, "Season 01", "Batch")}}
	observed, err := engine.observeClient(context.Background(), "qbit", backend)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || backend.fileCalls != 1 {
		t.Fatalf("observed=%d fileCalls=%d, want full observation for symlink parent", len(observed), backend.fileCalls)
	}
}

func TestInferMigrationPresentationPreservesExplicitSeasonZero(t *testing.T) {
	name := medianame.AnalyzeMany([]string{"abcabc042 S00E03"}).Items[0]
	files := []download.File{{Path: "/downloads/abcabc042 S00E03.mkv", Wanted: true, Progress: 1}}
	fileAnalysis := medianame.AnalyzeFiles([]string{"abcabc042 S00E03.mkv"})
	got := inferMigrationPresentation(download.Task{Name: "abcabc042 S00E03", SavePath: "/downloads"}, files, name, 1, fileAnalysis)
	if got.MediaType != catalog.MediaSeries || got.Season != 0 || !got.SeasonKnown {
		t.Fatalf("explicit S00 migration presentation lost identity: %+v", got)
	}
}
