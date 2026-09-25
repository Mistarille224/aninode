package automation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aninode/internal/acquisition"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/episode"
	"aninode/internal/observation"
	"aninode/internal/organizer"
	"aninode/internal/release"
	"aninode/internal/source"
)

func acquireWithObservedGraph(t *testing.T, r Runner, p candidate.Plan) ([]AcquireResult, error) {
	t.Helper()
	if err := os.MkdirAll(r.Bundle.Organizer.Source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.Bundle.Organizer.Target, 0o755); err != nil {
		t.Fatal(err)
	}
	graph, err := observation.Build(r.Bundle.Organizer.Source, r.Bundle.Organizer.Target, r.Bundle.Entries)
	if err != nil {
		t.Fatal(err)
	}
	return r.Acquire(context.Background(), p, graph)
}

func TestTaskIdentityDoesNotRequireLabels(t *testing.T) {
	i := acquisitionIntent{ID: "btih-0123456789012345678901234567890123456789", Identity: acquisition.Identity{ID: "btih-0123456789012345678901234567890123456789", Kind: "btih", Canonical: "0123456789012345678901234567890123456789"}, Correlation: "acq:btih-0123456789012345678901234567890123456789"}
	if !taskMatches(i, download.Task{InfoHash: "0123456789012345678901234567890123456789"}) {
		t.Fatal("infohash did not match without labels")
	}
	if !taskMatches(i, download.Task{ID: download.CorrelationTaskID(i.Correlation)}) {
		t.Fatal("deterministic task ID did not match without labels")
	}
}

type noLabelBackend struct {
	tasks []download.Task
	adds  int
}

type observedBackend struct {
	tasks []download.Task
	files map[string][]download.File
}

func (b *observedBackend) Name() string { return "fake" }
func (b *observedBackend) List(context.Context) ([]download.Task, error) {
	return append([]download.Task(nil), b.tasks...), nil
}
func (b *observedBackend) Files(_ context.Context, id string) ([]download.File, error) {
	return append([]download.File(nil), b.files[id]...), nil
}

type shapeBackend struct {
	shape       source.Shape
	task        download.Task
	files       []download.File
	relocations int
	verified    int
}

func (b *shapeBackend) Name() string { return "shape" }
func (b *shapeBackend) List(context.Context) ([]download.Task, error) {
	if b.task.ID == "" {
		return nil, nil
	}
	return []download.Task{b.task}, nil
}
func (b *shapeBackend) Files(context.Context, string) ([]download.File, error) {
	return append([]download.File(nil), b.files...), nil
}
func (b *shapeBackend) Add(_ context.Context, request download.AddRequest) (download.Task, error) {
	b.task = download.Task{ID: download.CorrelationTaskID(request.Correlation), Name: "abcabc146 S01", SavePath: request.SavePath}
	if b.shape == source.SingleFile {
		b.task.ContentPath = filepath.Join(request.SavePath, "abcabc146 S01E01.mkv")
		b.files = []download.File{{Path: b.task.ContentPath, Wanted: true}}
	} else {
		b.task.ContentPath = filepath.Join(request.SavePath, "[grpabc165] abcabc146 Batch")
		b.files = []download.File{{Path: filepath.Join(b.task.ContentPath, "abcabc146 S01E01.mkv"), Wanted: true}}
	}
	return b.task, nil
}
func (b *shapeBackend) Relocate(_ context.Context, _ string, destination string) error {
	b.relocations++
	oldContent := b.task.ContentPath
	b.task.SavePath = destination
	if b.shape == source.SingleFile {
		b.task.ContentPath = filepath.Join(destination, filepath.Base(oldContent))
		b.files[0].Path = b.task.ContentPath
	}
	return nil
}
func (b *shapeBackend) Verify(context.Context, string) error { b.verified++; return nil }

func (b *noLabelBackend) Name() string { return "aria" }
func (b *noLabelBackend) List(context.Context) ([]download.Task, error) {
	return append([]download.Task(nil), b.tasks...), nil
}
func (b *noLabelBackend) Add(_ context.Context, r download.AddRequest) (download.Task, error) {
	b.adds++
	t := download.Task{ID: download.CorrelationTaskID(r.Correlation), Sources: []string{r.URL}, SavePath: r.SavePath}
	b.tasks = append(b.tasks, t)
	return t, nil
}

func TestAcquireAndRestartDoNotRequireLabels(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	target := filepath.Join(root, "library", "TV", "abcabc146")
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Clients: map[string]configstore.Client{"aria": {ID: "aria", Enabled: true}}, Entries: map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", Path: target, Seasons: []int{1}}}}
	x, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://example.test/show-01"})
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{MediaType: catalog.MediaSeries, EntryKey: id, Release: release.Release{DownloadURL: "https://example.test/show-01"}, SourceEpisode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Episode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Acquisition: x, Decision: candidate.DecisionSelected}
	backend := &noLabelBackend{}
	p := candidate.Plan{Candidates: []candidate.Candidate{c}}
	for run := 0; run < 3; run++ {
		r := Runner{Bundle: bundle, Clients: map[string]download.Backend{"aria": backend}}
		got, e := acquireWithObservedGraph(t, r, p)
		if e != nil {
			t.Fatal(e)
		}
		if len(got) != 1 {
			t.Fatalf("results=%v", got)
		}
	}
	if backend.adds != 1 {
		t.Fatalf("adds=%d, want 1 across logical restarts", backend.adds)
	}
}

func TestAcquireObservesAndRepairsSingleFileButKeepsMultifileDepth(t *testing.T) {
	for _, shape := range []source.Shape{source.SingleFile, source.MultiFile} {
		t.Run(string(shape), func(t *testing.T) {
			root := t.TempDir()
			id := "series/abcabc146"
			entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
			w := catalog.Entry{Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1}}
			bundle := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Clients: map[string]configstore.Client{"shape": {ID: "shape", Enabled: true}}, Entries: map[string]catalog.Entry{id: w}}
			identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://example.test/show"})
			if err != nil {
				t.Fatal(err)
			}
			candidateValue := candidate.Candidate{MediaType: catalog.MediaSeries, EntryKey: id, Release: release.Release{DownloadURL: "https://example.test/show"}, SourceEpisode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Episode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Acquisition: identity, Decision: candidate.DecisionSelected}
			backend := &shapeBackend{shape: shape}
			results, err := acquireWithObservedGraph(t, Runner{Bundle: bundle, Clients: map[string]download.Backend{"shape": backend}}, candidate.Plan{Candidates: []candidate.Candidate{candidateValue}})
			if err != nil || len(results) != 1 || results[0].Decision != AcquireSubmitted {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			topology, err := source.Observe(backend.task, backend.files, nil)
			if err != nil || !source.ValidateSeries(entryRoot, topology.ContentRoot, topology.Paths).Valid {
				t.Fatalf("observation=%+v err=%v", topology, err)
			}
			if shape == source.SingleFile && (backend.relocations != 0 || backend.verified != 0) {
				t.Fatalf("single-file placement should already be canonical: relocation=%d verify=%d", backend.relocations, backend.verified)
			}
			if shape == source.MultiFile && backend.relocations != 0 {
				t.Fatalf("multifile gained extra container: %+v", backend.task)
			}
		})
	}
}

func TestMovieAcquireObservesSingleAndMultifileTopology(t *testing.T) {
	for _, shape := range []source.Shape{source.SingleFile, source.MultiFile} {
		t.Run(string(shape), func(t *testing.T) {
			root := t.TempDir()
			id := "movie/abcabc147"
			w := catalog.Entry{Key: id, MediaType: catalog.MediaMovie, Title: "abcabc146", Path: filepath.Join(root, "library", "Movies", "abcabc146")}
			bundle := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Clients: map[string]configstore.Client{"shape": {ID: "shape", Enabled: true}}, Entries: map[string]catalog.Entry{id: w}}
			identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://example.test/movie"})
			if err != nil {
				t.Fatal(err)
			}
			value := candidate.Candidate{MediaType: catalog.MediaMovie, EntryKey: id, Release: release.Release{DownloadURL: "https://example.test/movie"}, SourceEpisode: episode.Key{EntryKey: id}, Episode: episode.Key{EntryKey: id}, Acquisition: identity, Decision: candidate.DecisionSelected}
			backend := &shapeBackend{shape: shape}
			results, err := acquireWithObservedGraph(t, Runner{Bundle: bundle, Clients: map[string]download.Backend{"shape": backend}}, candidate.Plan{Candidates: []candidate.Candidate{value}})
			if err != nil || len(results) != 1 || results[0].Decision != AcquireSubmitted {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			observation, err := source.Observe(backend.task, backend.files, nil)
			if err != nil || !source.ValidateMovie(bundle.Organizer.MovieSource(), observation.ContentRoot, observation.Paths).Valid {
				t.Fatalf("observation=%+v err=%v", observation, err)
			}
			if shape == source.SingleFile && backend.relocations != 1 {
				t.Fatalf("single movie was not relocated: %+v", backend.task)
			}
			if shape == source.MultiFile && backend.relocations != 0 {
				t.Fatalf("multifile movie gained an extra container: %+v", backend.task)
			}
		})
	}
}

func TestTaskSourceURLIdentityDoesNotRequireLabels(t *testing.T) {
	x, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://EXAMPLE.test/a?b=2&a=1"})
	if err != nil {
		t.Fatal(err)
	}
	i := acquisitionIntent{ID: x.ID, Identity: x, Correlation: "acq:" + x.ID}
	if !taskMatches(i, download.Task{Sources: []string{"https://example.test/a?a=1&b=2"}}) {
		t.Fatal("canonical source URL did not match")
	}
}

func TestTargetOverlapUsesCurrentEpisodeIdentity(t *testing.T) {
	a := episode.Key{EntryKey: "w", Season: 2, EpisodeStart: 1, EpisodeEnd: 2}
	b := episode.Key{EntryKey: "w", Season: 2, EpisodeStart: 2, EpisodeEnd: 3}
	if !sameEpisode(a, b) {
		t.Fatal("overlap not detected")
	}
	b.Season = 1
	if sameEpisode(a, b) {
		t.Fatal("different season overlapped")
	}
}

func TestSeriesPlacementUsesObservedSourceFolder(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc")
	b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library")}, Entries: map[string]catalog.Entry{id: {Key: id, Title: "abcabc", DeclarationPath: entryRoot, Seasons: []int{1, 2}}}}
	r := Runner{Bundle: b}
	got, err := r.placement(acquisitionIntent{EntryKey: id, Source: episode.Key{Season: 1}, Target: episode.Key{Season: 2}, SourceFolder: "Season 02"}, source.MultiFile)
	want := filepath.Join(entryRoot, "Season 02")
	if err != nil || got.SavePath != want {
		t.Fatalf("placement=%+v err=%v want=%q", got, err, want)
	}
}

func TestSingleFileSeriesPlacementSeparatesTargetSeasons(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library")}, Entries: map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Seasons: []int{1, 2}}}}
	r := Runner{Bundle: b}
	for _, season := range []int{1, 2} {
		got, err := r.placement(acquisitionIntent{EntryKey: id, Target: episode.Key{Season: season}}, source.SingleFile)
		want := filepath.Join(entryRoot, fmt.Sprintf("Season %02d", season))
		if err != nil || got.SavePath != want {
			t.Fatalf("season %d placement=%+v err=%v want=%q", season, got, err, want)
		}
	}
}

func TestAcquisitionRejectsMixedSeasonContainer(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	container := filepath.Join(entryRoot, "[grpabc165] abcabc133")
	b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Seasons: []int{1, 2}}}}
	backend := &observedBackend{tasks: []download.Task{{ID: "mixed", SavePath: entryRoot, ContentPath: container}}, files: map[string][]download.File{"mixed": {
		{Path: filepath.Join(container, "abcabc146 S01E01.mkv"), Wanted: true},
		{Path: filepath.Join(container, "abcabc146 S02E01.mkv"), Wanted: true},
	}}}
	i := acquisitionIntent{EntryKey: id, Client: "fake", Source: episode.Key{Season: 1}, Target: episode.Key{Season: 1}}
	err := (Runner{Bundle: b}).ensureTaskTopology(context.Background(), i, backend, backend, backend.tasks[0])
	if err == nil || !strings.Contains(err.Error(), "one target season") {
		t.Fatalf("mixed-season acquisition was accepted: %v", err)
	}
}

func TestPublicationConvergesFromOwnedDownloaderObservationAfterRestart(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	w := catalog.Entry{Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "abcabc146"), Seasons: []int{1}}
	b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}}
	cfg, err := b.OrganizerForPreview(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(cfg.Source, "abcabc146 S01E03.mkv")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		backend := &observedBackend{tasks: []download.Task{{ID: "task", Name: "abcabc146 S01", SavePath: filepath.Dir(cfg.Source), ContentPath: cfg.Source}}, files: map[string][]download.File{"task": {{Path: source, Size: 5, Wanted: true, Progress: 1}}}}
		r := Runner{Bundle: b, Clients: map[string]download.Backend{"fake": backend}}
		if _, err := r.ReconcilePublications(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(w.Path, "Season 01", "abcabc146 S01E03.mkv")
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	binfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, binfo) {
		t.Fatal("publication is not a hard link")
	}
}

func TestPublicationIgnoresKnownIncompleteDownloaderArtifacts(t *testing.T) {
	for _, name := range []string{"abcabc146 S01E01.mkv.!qB", "abcabc146 S01E02.mp4.!qB", "abcabc146 S01E03.mkv.part", "abcabc146 S01E04.mp4.part", "abcabc146 S01E05.mkv.tmp", "abcabc146 S01E06.mkv.random"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "series/abcabc146"
			w := catalog.Entry{Key: id, Title: "abcabc146", Path: filepath.Join(root, "library", "abcabc146"), Seasons: []int{1}}
			b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv", ".mp4"}}, Entries: map[string]catalog.Entry{id: w}}
			cfg, err := b.OrganizerForPreview(id, 1)
			if err != nil {
				t.Fatal(err)
			}
			mustWritePublicationFile(t, filepath.Join(cfg.Source, name))
			results, err := (Runner{Bundle: b}).ReconcilePublications(context.Background())
			if err != nil {
				t.Fatalf("incomplete artifact caused reconciliation error: %v", err)
			}
			if len(results) != 0 {
				t.Fatalf("incomplete artifact entered publication pipeline: %+v", results)
			}
		})
	}
}

func TestAria2ControlFileDefersPublicationUntilNextFilesystemObservation(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	w := catalog.Entry{Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "abcabc146"), Seasons: []int{1}}
	b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}}
	cfg, err := b.OrganizerForPreview(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(cfg.Source, "abcabc146 S01E07.mkv")
	control := source + ".aria2"
	mustWritePublicationFile(t, source)
	mustWritePublicationFile(t, control)
	backend := &observedBackend{tasks: []download.Task{{ID: "task", Name: "abcabc146 S01", SavePath: filepath.Dir(cfg.Source), ContentPath: cfg.Source}}, files: map[string][]download.File{"task": {{Path: source, Size: 5, Wanted: true, Progress: 1}}}}
	if results, err := (Runner{Bundle: b, Clients: map[string]download.Backend{"fake": backend}}).ReconcilePublications(context.Background()); err != nil || len(results) != 0 {
		t.Fatalf("active aria2 file was not ignored: results=%+v err=%v", results, err)
	}
	if err := os.Remove(control); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Bundle: b, Clients: map[string]download.Backend{"fake": backend}}).ReconcilePublications(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(w.Path, "Season 01", "abcabc146 S01E07.mkv")
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(sourceInfo, targetInfo) {
		t.Fatalf("next reconciliation did not hard-link ready file: %v", err)
	}
}

func TestNonCanonicalSourcesSurviveRestartAndMergeIntoCanonicalSeason(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	localDownloads := filepath.Join(root, "usb", "downloads")
	entryRoot := filepath.Join(localDownloads, "TV", "abcabc146")
	w := catalog.Entry{Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1}}
	b := configstore.Bundle{Organizer: organizer.Config{Source: localDownloads, Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Clients: map[string]configstore.Client{"fake": {PathMappings: []configstore.PathMapping{{Remote: "/downloads", Local: localDownloads}}}}, Entries: map[string]catalog.Entry{id: w}}
	containers := []string{filepath.Join(entryRoot, "[grpabc163] Episodes 01-12"), filepath.Join(entryRoot, "[grpabc164] Episodes 13-24")}
	paths := []string{filepath.Join(containers[0], "abcabc146 S01E01.mkv"), filepath.Join(containers[1], "abcabc146 S01E13.mkv")}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	backend := &observedBackend{tasks: []download.Task{
		{ID: "a", Name: "abcabc146 S01 01-12", SavePath: "/downloads/TV/abcabc146", ContentPath: "/downloads/TV/abcabc146/[grpabc163] Episodes 01-12"},
		{ID: "b", Name: "abcabc146 S01 13-24", SavePath: "/downloads/TV/abcabc146", ContentPath: "/downloads/TV/abcabc146/[grpabc164] Episodes 13-24"},
	}, files: map[string][]download.File{
		"a": {{Path: "/downloads/TV/abcabc146/[grpabc163] Episodes 01-12/abcabc146 S01E01.mkv", Size: 5, Wanted: true, Progress: 1}},
		"b": {{Path: "/downloads/TV/abcabc146/[grpabc164] Episodes 13-24/abcabc146 S01E13.mkv", Size: 5, Wanted: true, Progress: 1}},
	}}
	for restart := 0; restart < 2; restart++ {
		results, err := (Runner{Bundle: b, Clients: map[string]download.Backend{"fake": backend}}).ReconcilePublications(context.Background())
		if err != nil || len(results) != 2 {
			t.Fatalf("restart %d results=%+v err=%v", restart, results, err)
		}
		for _, result := range results {
			if result.Decision != "reconciled" || result.EntryKey != id {
				t.Fatalf("result=%+v", result)
			}
		}
	}
	for _, episode := range []string{"abcabc146 S01E01.mkv", "abcabc146 S01E13.mkv"} {
		if _, err := os.Stat(filepath.Join(w.Path, "Season 01", episode)); err != nil {
			t.Fatal(err)
		}
	}
	for _, container := range containers {
		if _, err := os.Stat(container); err != nil {
			t.Fatalf("source container changed: %v", err)
		}
	}
}

func TestNonCanonicalMovieSourceSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	id := "movie/abcabc147"
	localDownloads := filepath.Join(root, "usb", "downloads")
	container := filepath.Join(localDownloads, "Movies", "[grpabc165] abcabc148 BDRip")
	w := catalog.Entry{Key: id, MediaType: catalog.MediaMovie, Title: "abcabc148", Year: 2024, DeclarationPath: container, Path: filepath.Join(root, "library", "Movies", "abcabc148 (2024)")}
	b := configstore.Bundle{Organizer: organizer.Config{Source: localDownloads, Target: filepath.Join(root, "library"), Extensions: []string{".mkv", ".ass"}}, Clients: map[string]configstore.Client{"fake": {PathMappings: []configstore.PathMapping{{Remote: "/downloads", Local: localDownloads}}}}, Entries: map[string]catalog.Entry{id: w}}
	video := filepath.Join(container, "abcabc148 (2024).mkv")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := &observedBackend{tasks: []download.Task{{ID: "movie", Name: "abcabc148 2024", SavePath: "/downloads/Movies", ContentPath: "/downloads/Movies/[grpabc165] abcabc148 BDRip"}}, files: map[string][]download.File{"movie": {{Path: "/downloads/Movies/[grpabc165] abcabc148 BDRip/abcabc148 (2024).mkv", Size: 5, Wanted: true, Progress: 1}}}}
	for restart := 0; restart < 2; restart++ {
		results, err := (Runner{Bundle: b, Clients: map[string]download.Backend{"fake": backend}}).ReconcilePublications(context.Background())
		if err != nil || len(results) != 1 || results[0].Decision != "reconciled" {
			t.Fatalf("restart %d results=%+v err=%v", restart, results, err)
		}
	}
	if _, err := os.Stat(filepath.Join(w.Path, "abcabc148 (2024).mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(container); err != nil {
		t.Fatalf("movie source renamed: %v", err)
	}
}

func TestSeasonlessFolderProjectedTaskDoesNotOscillateBetweenSeasons(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc")
	seasonRoot := filepath.Join(entryRoot, "Season 02")
	libraryRoot := filepath.Join(root, "library")
	w := catalog.Entry{
		Key: id, MediaType: catalog.MediaSeries, Title: "abcabc", Enabled: true,
		DeclarationPath: entryRoot, Path: filepath.Join(libraryRoot, "TV", "abcabc"), Seasons: []int{1, 2},
		FolderProjections: map[string]catalog.FolderProjection{"Season 02": {Season: 2}},
	}
	b := configstore.Bundle{
		Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: libraryRoot, Extensions: []string{".mkv"}},
		Entries:   map[string]catalog.Entry{id: w},
	}
	var files []download.File
	for episodeNumber := 41; episodeNumber <= 47; episodeNumber++ {
		path := filepath.Join(seasonRoot, fmt.Sprintf("abcabc - %02d.mkv", episodeNumber))
		mustWritePublicationFile(t, path)
		files = append(files, download.File{Path: path, Size: 5, Wanted: true, Progress: 1})
	}
	backend := &observedBackend{
		tasks: []download.Task{{ID: "taskabc", Name: "abcabc 41-47", SavePath: seasonRoot, ContentPath: seasonRoot}},
		files: map[string][]download.File{"taskabc": files},
	}
	runner := Runner{Bundle: b, Clients: map[string]download.Backend{"fake": backend}}
	for cycle := 0; cycle < 4; cycle++ {
		results, err := runner.ReconcilePublications(context.Background())
		if err != nil {
			t.Fatalf("cycle %d results=%+v err=%v", cycle, results, err)
		}
		for episodeNumber := 41; episodeNumber <= 47; episodeNumber++ {
			target := filepath.Join(w.Path, "Season 02", fmt.Sprintf("abcabc S02E%02d.mkv", episodeNumber))
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("cycle %d missing target %s: %v", cycle, target, err)
			}
		}
		seasonOne := filepath.Join(w.Path, "Season 01")
		if _, err := os.Stat(seasonOne); err == nil || !os.IsNotExist(err) {
			t.Fatalf("cycle %d unexpectedly published Season 01: %v", cycle, err)
		}
	}
}

func mustWritePublicationFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSourcePublishesWithoutDownloaderTaskAndAppliesOffsets(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	container := filepath.Join(entryRoot, "anything")
	libraryEntry := filepath.Join(root, "library", "TV", "abcabc146")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(container, "abcabc146 S01E13.mkv")
	if err := os.WriteFile(sourcePath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := catalog.Entry{Key: id, MediaType: catalog.MediaSeries, Title: "abcabc146", DeclarationPath: entryRoot, Path: libraryEntry, Seasons: []int{2}, Enabled: true, FolderProjections: map[string]catalog.FolderProjection{"anything": {Season: 2, EpisodeOffset: -10}}}
	b := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}}
	results, err := (Runner{Bundle: b, Clients: map[string]download.Backend{}}).ReconcilePublications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("local source produced no reconcile result")
	}
	target := filepath.Join(libraryEntry, "Season 02", "abcabc146 S02E03.mkv")
	a, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("offset publication missing: %v; results=%+v", err, results)
	}
	if !os.SameFile(a, bi) {
		t.Fatal("local publication is not a hard link")
	}
	if _, err := os.Stat(filepath.Join(entryRoot, "Season 02")); !os.IsNotExist(err) {
		t.Fatalf("target season leaked into source namespace: %v", err)
	}
}

type pendingFilesBackend struct {
	task download.Task
}

func (b *pendingFilesBackend) Name() string { return "pending" }
func (b *pendingFilesBackend) List(context.Context) ([]download.Task, error) {
	if b.task.ID == "" {
		return nil, nil
	}
	return []download.Task{b.task}, nil
}
func (b *pendingFilesBackend) Files(context.Context, string) ([]download.File, error) {
	return []download.File{{Path: "/not-ready/video.mkv", Wanted: false}}, nil
}
func (b *pendingFilesBackend) Add(_ context.Context, request download.AddRequest) (download.Task, error) {
	b.task = download.Task{ID: download.CorrelationTaskID(request.Correlation), Name: "abcabc146", SavePath: request.SavePath}
	return b.task, nil
}

func TestAcquireAcceptedAddIsSubmittedWhenWantedFilesAreNotReady(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	w := catalog.Entry{Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1}}
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Clients: map[string]configstore.Client{"pending": {ID: "pending", Enabled: true}}, Entries: map[string]catalog.Entry{id: w}}
	identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://example.test/show"})
	if err != nil {
		t.Fatal(err)
	}
	value := candidate.Candidate{MediaType: catalog.MediaSeries, EntryKey: id, Release: release.Release{DownloadURL: "https://example.test/show"}, SourceEpisode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Episode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Acquisition: identity, Decision: candidate.DecisionSelected}
	backend := &pendingFilesBackend{}
	results, err := acquireWithObservedGraph(t, Runner{Bundle: bundle, Clients: map[string]download.Backend{"pending": backend}}, candidate.Plan{Candidates: []candidate.Candidate{value}})
	if err != nil || len(results) != 1 || results[0].Decision != AcquireSubmitted {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

type delayedObservationBackend struct{ adds int }

func (b *delayedObservationBackend) Name() string { return "delayed" }
func (b *delayedObservationBackend) List(context.Context) ([]download.Task, error) {
	return nil, nil
}
func (b *delayedObservationBackend) Add(context.Context, download.AddRequest) (download.Task, error) {
	b.adds++
	return download.Task{}, nil
}

func TestAcquireAcceptedAddDoesNotFailDuringVisibilityDelay(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc146"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	w := catalog.Entry{Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1}}
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Clients: map[string]configstore.Client{"delayed": {ID: "delayed", Enabled: true}}, Entries: map[string]catalog.Entry{id: w}}
	identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://example.test/show.torrent", InfoHash: "0123456789abcdef0123456789abcdef01234567"})
	if err != nil {
		t.Fatal(err)
	}
	value := candidate.Candidate{MediaType: catalog.MediaSeries, EntryKey: id, Release: release.Release{DownloadURL: "https://example.test/show.torrent"}, SourceEpisode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Episode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}, Acquisition: identity, Decision: candidate.DecisionSelected}
	backend := &delayedObservationBackend{}
	results, err := acquireWithObservedGraph(t, Runner{Bundle: bundle, Clients: map[string]download.Backend{"delayed": backend}}, candidate.Plan{Candidates: []candidate.Candidate{value}})
	if err != nil || len(results) != 1 || results[0].Decision != AcquireSubmitted || results[0].TaskID != "" || backend.adds != 1 {
		t.Fatalf("results=%+v adds=%d err=%v", results, backend.adds, err)
	}
}

func TestReconcileIgnoresDownloaderTasksOutsideDeclaredSourceNamespace(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "source", "TV", "Managed")
	if err := os.MkdirAll(entryRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "other-downloads", "Unmanaged", "Unmanaged S01E01.mkv")
	mustWritePublicationFile(t, outside)
	id := "series/Managed"
	w := catalog.Entry{Key: id, Title: "Managed", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "Managed"), Seasons: []int{1}}
	bundle := configstore.Bundle{Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}}, Entries: map[string]catalog.Entry{id: w}}
	backend := &observedBackend{tasks: []download.Task{{ID: "unmanaged", Name: "Unmanaged", SavePath: filepath.Dir(outside), ContentPath: outside}}, files: map[string][]download.File{"unmanaged": {{Path: outside, Wanted: true}}}}
	results, err := (Runner{Bundle: bundle, Clients: map[string]download.Backend{"fake": backend}}).ReconcilePublications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Decision != "ignored" || results[0].Error != "" {
		t.Fatalf("results=%+v", results)
	}
}

func TestPublicationConflictReasonSummarizesWithoutEscalating(t *testing.T) {
	plan := organizer.Plan{Items: []organizer.PlanItem{
		{Action: organizer.ActionConflict, Detail: "destination A differs"},
		{Action: organizer.ActionConflict, Detail: "destination B differs"},
		{Action: organizer.ActionConflict, Detail: "destination C differs"},
	}}
	got := publicationConflictReason(plan, 3)
	if !strings.Contains(got, "3 publication conflict") || !strings.Contains(got, "destination A differs") || !strings.Contains(got, "destination B differs") {
		t.Fatalf("unexpected summary: %q", got)
	}
	if strings.Contains(got, "destination C differs") {
		t.Fatalf("summary should stay bounded: %q", got)
	}
}

type addOnlyBackend struct {
	addErr error
	empty  bool
}

func (b *addOnlyBackend) Name() string { return "add-only" }
func (b *addOnlyBackend) Add(context.Context, download.AddRequest) (download.Task, error) {
	if b.addErr != nil {
		return download.Task{}, b.addErr
	}
	if b.empty {
		return download.Task{}, nil
	}
	return download.Task{ID: "accepted"}, nil
}

func TestAcquireAddOnlyBackendDoesNotPanicDuringRecovery(t *testing.T) {
	makeRunner := func(t *testing.T, backend download.Backend) (Runner, candidate.Plan) {
		t.Helper()
		root := t.TempDir()
		id := "series/abcabc146"
		bundle := configstore.Bundle{
			Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}},
			Clients:   map[string]configstore.Client{"add": {ID: "add", Enabled: true}},
			Entries:   map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1}}},
		}
		identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{DownloadURL: "https://example.test/show-03"})
		if err != nil {
			t.Fatal(err)
		}
		value := candidate.Candidate{
			MediaType: catalog.MediaSeries, EntryKey: id,
			Release:       release.Release{DownloadURL: "https://example.test/show-03"},
			SourceEpisode: episode.Key{EntryKey: id, Season: 1, EpisodeStart: 3, EpisodeEnd: 3},
			Episode:       episode.Key{EntryKey: id, Season: 1, EpisodeStart: 3, EpisodeEnd: 3},
			Acquisition:   identity, Decision: candidate.DecisionSelected,
		}
		return Runner{Bundle: bundle, Clients: map[string]download.Backend{"add": backend}}, candidate.Plan{Candidates: []candidate.Candidate{value}}
	}

	t.Run("add error", func(t *testing.T) {
		runner, plan := makeRunner(t, &addOnlyBackend{addErr: fmt.Errorf("add failed")})
		results, err := acquireWithObservedGraph(t, runner, plan)
		if err == nil || len(results) != 1 || results[0].Decision != AcquireFailed {
			t.Fatalf("results=%+v err=%v", results, err)
		}
	})

	t.Run("accepted without task id", func(t *testing.T) {
		runner, plan := makeRunner(t, &addOnlyBackend{empty: true})
		results, err := acquireWithObservedGraph(t, runner, plan)
		if err != nil || len(results) != 1 || results[0].Decision != AcquireSubmitted {
			t.Fatalf("results=%+v err=%v", results, err)
		}
	})
}

type batchObservationBackend struct {
	observations []download.Observation
	listCalls    int
	fileCalls    int
	batchCalls   int
}

func (b *batchObservationBackend) Name() string { return "batch" }
func (b *batchObservationBackend) List(context.Context) ([]download.Task, error) {
	b.listCalls++
	return nil, nil
}
func (b *batchObservationBackend) Files(context.Context, string) ([]download.File, error) {
	b.fileCalls++
	return nil, nil
}
func (b *batchObservationBackend) ListObservations(context.Context) ([]download.Observation, error) {
	b.batchCalls++
	return append([]download.Observation(nil), b.observations...), nil
}

type taskAwareFilesBackend struct {
	tasks         []download.Task
	files         map[string][]download.File
	listCalls     int
	fileCalls     int
	taskFileCalls int
}

func (b *taskAwareFilesBackend) Name() string { return "task-aware" }
func (b *taskAwareFilesBackend) List(context.Context) ([]download.Task, error) {
	b.listCalls++
	return append([]download.Task(nil), b.tasks...), nil
}
func (b *taskAwareFilesBackend) Files(_ context.Context, id string) ([]download.File, error) {
	b.fileCalls++
	return append([]download.File(nil), b.files[id]...), nil
}
func (b *taskAwareFilesBackend) FilesForTask(_ context.Context, task download.Task) ([]download.File, error) {
	b.taskFileCalls++
	return append([]download.File(nil), b.files[task.ID]...), nil
}

func TestObserveReusesBatchTaskAndFileObservation(t *testing.T) {
	backend := &batchObservationBackend{observations: []download.Observation{{
		Task: download.Task{ID: "one"}, Files: []download.File{{Path: "/source/one.mkv", Wanted: true}},
	}}}
	r := Runner{Clients: map[string]download.Backend{"batch": backend}}
	s := r.observe(context.Background())
	src := any(backend).(taskSource)
	files, err := observeFiles(context.Background(), s, "batch", src, s.tasks["batch"][0])
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if backend.batchCalls != 1 || backend.listCalls != 0 || backend.fileCalls != 0 {
		t.Fatalf("calls batch=%d list=%d files=%d", backend.batchCalls, backend.listCalls, backend.fileCalls)
	}
}

func TestObserveFilesUsesTaskAwareEndpointWithoutRedundantGet(t *testing.T) {
	backend := &taskAwareFilesBackend{
		tasks: []download.Task{{ID: "one", ContentPath: "/source/one.mkv"}},
		files: map[string][]download.File{"one": {{Path: "/source/one.mkv", Wanted: true}}},
	}
	r := Runner{Clients: map[string]download.Backend{"qbit": backend}}
	s := r.observe(context.Background())
	src := any(backend).(taskSource)
	files, err := observeFiles(context.Background(), s, "qbit", src, s.tasks["qbit"][0])
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if backend.listCalls != 1 || backend.taskFileCalls != 1 || backend.fileCalls != 0 {
		t.Fatalf("calls list=%d task-files=%d files=%d", backend.listCalls, backend.taskFileCalls, backend.fileCalls)
	}
}

func TestTaskOutsideManagedSourceUsesMappedContentPath(t *testing.T) {
	bundle := configstore.Bundle{
		Organizer: organizer.Config{Source: "/local/media/source"},
		Clients:   map[string]configstore.Client{"qbit": {ID: "qbit", PathMappings: []configstore.PathMapping{{Remote: "/remote", Local: "/local/media"}}}},
	}
	if taskOutsideManagedSource(bundle, "qbit", download.Task{ContentPath: "/remote/source/TV/abcabc146/file.mkv"}) {
		t.Fatal("mapped managed path classified as outside")
	}
	if !taskOutsideManagedSource(bundle, "qbit", download.Task{ContentPath: "/remote/other/file.mkv"}) {
		t.Fatal("mapped unrelated path was not classified as outside")
	}
	if taskOutsideManagedSource(bundle, "qbit", download.Task{}) {
		t.Fatal("missing content path must remain observable rather than guessed outside")
	}
}
