package claim

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/organizer"
)

func mkdirFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("media"), 0o644)
}

func claimBundle(root string, entries map[string]catalog.Entry) configstore.Bundle {
	return configstore.Bundle{
		Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}},
		Entries:   entries,
	}
}

func TestCanonicalPathAndPresentFilesResolveWithoutLabelsOrCompleteProgress(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	b := claimBundle(root, map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "abcabc146"), Seasons: []int{1}}})
	cfg, err := b.OrganizerForPreview(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cfg.Source, "abcabc146 S01E03.mkv")
	if err := mkdirFile(file); err != nil {
		t.Fatal(err)
	}
	got := Resolve(context.Background(), b, "aria", download.Task{SavePath: cfg.Source, State: download.StateChecking, Progress: .2}, []download.File{{Path: file, Size: 5, Wanted: true, Progress: .2}})
	if got.Status != Resolved || got.EntryKey != id || got.SourceEpisode.EpisodeStart != 3 {
		t.Fatalf("claim=%+v", got)
	}
}

func TestOverlappingDeclaredPathsAreAmbiguous(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "library", "abcabc146")
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	b := claimBundle(root, map[string]catalog.Entry{
		"a": {Key: "a", Title: "abcabc146", DeclarationPath: entryRoot, Path: path, Seasons: []int{1}},
		"b": {Key: "b", Title: "abcabc146", DeclarationPath: entryRoot, Path: path, Seasons: []int{1}},
	})
	cfg, err := b.OrganizerForPreview("a", 1)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cfg.Source, "abcabc146 S01E01.mkv")
	if err := mkdirFile(file); err != nil {
		t.Fatal(err)
	}
	got := Resolve(context.Background(), b, "aria", download.Task{SavePath: cfg.Source}, []download.File{{Path: file, Wanted: true, Progress: 1}})
	if got.Status != Ambiguous {
		t.Fatalf("claim=%+v", got)
	}
}

func TestNonCanonicalSeriesContainerResolvesFromContentPath(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	b := claimBundle(root, map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1}}})
	container := filepath.Join(entryRoot, "[grpabc165] abcabc138")
	file := filepath.Join(container, "abcabc146 S01E01.mkv")
	if err := mkdirFile(file); err != nil {
		t.Fatal(err)
	}
	got := Resolve(context.Background(), b, "fake", download.Task{ID: "task", Name: "abcabc146 S01", SavePath: entryRoot, ContentPath: container}, []download.File{{Path: file, Size: 5, Wanted: true, Progress: 1}})
	if got.Status != Resolved || got.EntryKey != id || got.SourceEpisode.Season != 1 {
		t.Fatalf("claim=%+v", got)
	}
}

func TestNonCanonicalMovieContainerResolvesFromContentPath(t *testing.T) {
	root := t.TempDir()
	id := "movie/abcabc148 (2024)"
	container := filepath.Join(root, "source", "Movies", "[grpabc165] abcabc148 BDRip")
	b := claimBundle(root, map[string]catalog.Entry{id: {Key: id, MediaType: catalog.MediaMovie, Title: "abcabc148", Year: 2024, DeclarationPath: container, Path: filepath.Join(root, "library", "Movies", "abcabc148 (2024)")}})
	file := filepath.Join(container, "abcabc148 (2024).mkv")
	if err := mkdirFile(file); err != nil {
		t.Fatal(err)
	}
	got := Resolve(context.Background(), b, "fake", download.Task{ID: "movie", Name: "abcabc148 2024", SavePath: b.Organizer.MovieSource(), ContentPath: container}, []download.File{{Path: file, Size: 5, Wanted: true, Progress: 1}})
	if got.Status != Resolved || got.EntryKey != id {
		t.Fatalf("claim=%+v", got)
	}
}

func TestMoviePhysicalOwnershipDoesNotDependOnParsedNames(t *testing.T) {
	root := t.TempDir()
	id := "movie/abcabc111"
	container := filepath.Join(root, "source", "Movies", "[grpabc202] abcabc114")
	b := claimBundle(root, map[string]catalog.Entry{id: {Key: id, MediaType: catalog.MediaMovie, Title: "甲乙Ⅰ：丙丁", DeclarationPath: container, Path: filepath.Join(root, "library", "Movies", "甲乙Ⅰ：丙丁")}})
	file := filepath.Join(container, "[grpabc202] abcabc111.mkv")
	if err := mkdirFile(file); err != nil {
		t.Fatal(err)
	}
	got := Resolve(context.Background(), b, "fake", download.Task{ID: "movie", Name: "unparseable downloader label", SavePath: b.Organizer.MovieSource(), ContentPath: container}, []download.File{{Path: file, Size: 5, Wanted: true, Progress: 1}})
	if got.Status != Resolved || got.EntryKey != id {
		t.Fatalf("physical movie ownership was rejected by mutable names: %+v", got)
	}
}

func TestResolveMapsRemoteTaskAndFilesIntoOneLocalNamespace(t *testing.T) {
	root := t.TempDir()
	localDownloads := filepath.Join(root, "usb", "downloads")
	entryRoot := filepath.Join(localDownloads, "TV", "abcabc149")
	container := filepath.Join(entryRoot, "[grpabc165] S01")
	localFile := filepath.Join(container, "abcabc149 S01E01.mkv")
	if err := mkdirFile(localFile); err != nil {
		t.Fatal(err)
	}
	id := "series/Test"
	b := configstore.Bundle{
		Organizer: organizer.Config{Source: localDownloads, Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}},
		Clients:   map[string]configstore.Client{"remote": {PathMappings: []configstore.PathMapping{{Remote: "/downloads", Local: localDownloads}}}},
		Entries:   map[string]catalog.Entry{id: {Key: id, Title: "abcabc149", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc149"), Seasons: []int{1}}},
	}
	remoteContainer := "/downloads/TV/abcabc149/[grpabc165] S01"
	remoteFile := remoteContainer + "/abcabc149 S01E01.mkv"
	files := []download.File{{Path: remoteFile, Size: 5, Wanted: true, Progress: 1}}
	observed := observeFiles(files, mappings(b, "remote"))
	if len(observed.files) != 1 || observed.files[0].Path != localFile || len(observed.paths) != 1 || observed.paths[0] != localFile {
		t.Fatalf("mapped observation leaked or duplicated paths: %+v", observed)
	}
	got := Resolve(context.Background(), b, "remote", download.Task{ID: "task", Name: "abcabc149 S01", SavePath: "/downloads/TV/abcabc149", ContentPath: remoteContainer}, files)
	if got.Status != Resolved || got.EntryKey != id {
		t.Fatalf("mapped claim=%+v", got)
	}
}

func TestSeriesContainerWithMultipleTargetSeasonsIsRejected(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc146")
	b := claimBundle(root, map[string]catalog.Entry{id: {Key: id, Title: "abcabc146", DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc146"), Seasons: []int{1, 2}}})
	container := filepath.Join(entryRoot, "[grpabc165] abcabc133")
	paths := []string{filepath.Join(container, "abcabc146 S01E01.mkv"), filepath.Join(container, "abcabc146 S02E01.mkv")}
	for _, path := range paths {
		if err := mkdirFile(path); err != nil {
			t.Fatal(err)
		}
	}
	got := Resolve(context.Background(), b, "fake", download.Task{ID: "mixed", Name: "abcabc146", SavePath: entryRoot, ContentPath: container}, []download.File{
		{Path: paths[0], Size: 5, Wanted: true, Progress: 1},
		{Path: paths[1], Size: 5, Wanted: true, Progress: 1},
	})
	if got.Status == Resolved {
		t.Fatalf("mixed-season container acquired ownership: %+v", got)
	}
}

func TestSeasonlessAbsoluteTaskUsesDeclaredFolderProjectionWithoutAmbiguity(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc (2004)"
	entryRoot := filepath.Join(root, "source", "TV", "abcabc (2004)")
	seasonRoot := filepath.Join(entryRoot, "Season 02")
	if err := os.MkdirAll(seasonRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"41", "42", "43", "44"} {
		if err := mkdirFile(filepath.Join(seasonRoot, "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - "+ep+" [1080P][CHT].mkv")); err != nil {
			t.Fatal(err)
		}
	}
	w := catalog.Entry{
		Key: id, MediaType: catalog.MediaSeries, Title: "abcabc", DeclarationPath: entryRoot,
		Path: filepath.Join(root, "library", "TV", "abcabc (2004)"), Seasons: []int{1, 2}, Enabled: true,
		FolderProjections: map[string]catalog.FolderProjection{"Season 01": {Season: 1}, "Season 02": {Season: 2}},
	}
	b := claimBundle(root, map[string]catalog.Entry{id: w})
	file := filepath.Join(seasonRoot, "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - 44 [1080P][CHT].mkv")
	got := Resolve(context.Background(), b, "fake", download.Task{
		ID: "taskabc44", Name: "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - 44 [1080P][CHT].mkv",
		SavePath: seasonRoot, ContentPath: file,
	}, []download.File{{Path: file, Size: 5, Wanted: true, Progress: 1}})
	if got.Status != Resolved {
		t.Fatalf("seasonless task remained ambiguous: %+v", got)
	}
	if got.EntryKey != id || got.TargetEpisode.Season != 2 || got.TargetEpisode.EpisodeStart != 44 {
		t.Fatalf("wrong folder-projected claim: %+v", got)
	}
}
