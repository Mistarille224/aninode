package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/episode"
	"aninode/internal/observation"
)

func TestAvailabilityDistinguishesPresentAbsentUnknown(t *testing.T) {
	root := t.TempDir()
	w := catalog.Entry{Key: "series/Test", Path: filepath.Join(root, "abcabc146")}
	if err := os.MkdirAll(filepath.Join(w.Path, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "Season 01", "abcabc146 S01E01.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Scan(map[string]catalog.Entry{w.Key: w})
	if got := s.Availability(episode.Key{EntryKey: w.Key, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Present {
		t.Fatalf("present=%v", got)
	}
	if got := s.Availability(episode.Key{EntryKey: w.Key, Season: 1, EpisodeStart: 2, EpisodeEnd: 2}); got != Absent {
		t.Fatalf("absent=%v", got)
	}
	if got := s.Availability(episode.Key{EntryKey: "series/Missing", Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Unknown {
		t.Fatalf("unknown=%v", got)
	}
}

func TestMovieInventoryRecognizesFileDiscAndISO(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
	}{{"files", []string{"abcabc147 (2024).mkv"}}, {"bluray", []string{"BDMV/index.bdmv", "BDMV/STREAM/00001.m2ts"}}, {"dvd", []string{"VIDEO_TS/VIDEO_TS.IFO"}}, {"iso", []string{"abcabc147.bluray.iso"}}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, rel := range tc.files {
				p := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(rel), 0644); err != nil {
					t.Fatal(err)
				}
			}
			id := "movie:" + tc.name
			w := catalog.Entry{Key: id, MediaType: catalog.MediaMovie, Path: root, Title: "abcabc147", Year: 2024}
			got := Scan(map[string]catalog.Entry{id: w}).Entries[id]
			if !got.Known || !got.Available {
				t.Fatalf("inventory=%+v", got)
			}
		})
	}
}

func TestAria2IncompleteEvidenceDoesNotPolluteInventory(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(season, "abcabc146 S01E01.mkv")
	for _, path := range []string{media, media + ".aria2", filepath.Join(season, "abcabc146 S01E02.mkv.part"), filepath.Join(season, "abcabc146 S01E03.mkv.!qB")} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if got := snapshot.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Absent {
		t.Fatalf("active aria2 target availability=%v, want absent", got)
	}
	if err := os.Remove(media + ".aria2"); err != nil {
		t.Fatal(err)
	}
	snapshot = ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if got := snapshot.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Present {
		t.Fatalf("ready aria2 target availability=%v, want present", got)
	}
}

func TestGraphInventoryPreservesAria2IncompleteEvidence(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	libraryRoot := filepath.Join(root, "library")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(libraryRoot, "TV", "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(season, "abcabc146 S01E01.mkv")
	if err := os.WriteFile(media, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media+".aria2", []byte("control"), 0o644); err != nil {
		t.Fatal(err)
	}
	graph, err := observation.Build(sourceRoot, libraryRoot, map[string]catalog.Entry{id: w})
	if err != nil {
		t.Fatal(err)
	}
	inv := ScanGraph(map[string]catalog.Entry{id: w}, graph, []string{".mkv"})
	if got := inv.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Absent {
		t.Fatalf("graph inventory availability=%v, want absent", got)
	}
}

func TestGraphInventoryMissingConfiguredLibraryRootIsUnknown(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	libraryRoot := filepath.Join(root, "missing-library")
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(libraryRoot, "TV", "abcabc146"), Seasons: []int{1}}
	graph, err := observation.Build(sourceRoot, libraryRoot, map[string]catalog.Entry{id: w})
	if err != nil {
		t.Fatal(err)
	}
	inv := ScanGraph(map[string]catalog.Entry{id: w}, graph, []string{".mkv"})
	if got := inv.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Unknown {
		t.Fatalf("availability=%v, want unknown", got)
	}
}

func TestMissingWorkIsUnknownAndRequireKnownFails(t *testing.T) {
	id := "series/Test"
	s := Scan(map[string]catalog.Entry{id: {Key: id, Path: filepath.Join(t.TempDir(), "gone")}})
	if got := s.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Unknown {
		t.Fatalf("got=%v", got)
	}
	if err := s.RequireKnown(); err == nil {
		t.Fatal("expected unknown inventory error")
	}
}

func TestSharedMediaParserRecognizesHistoricalSeasonNames(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"01.mkv", "[grpabc165] abcabc146 - 02.mkv", "abcabc146 S01E03.mkv"} {
		if err := os.WriteFile(filepath.Join(season, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	for ep := 1; ep <= 3; ep++ {
		if got := s.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: ep, EpisodeEnd: ep}); got != Present {
			t.Fatalf("episode %d=%v", ep, got)
		}
	}
}

func TestInventoryUsesSiblingDifferencesBeforeApplyingSeasonContext(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"abcabc146 01 WEB 1080p.mkv", "abcabc146 02 WEB 1080p.mkv", "abcabc146 03 WEB 1080p.mkv"} {
		if err := os.WriteFile(filepath.Join(season, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	for ep := 1; ep <= 3; ep++ {
		if got := snapshot.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: ep, EpisodeEnd: ep}); got != Present {
			t.Fatalf("episode %d=%v", ep, got)
		}
	}
}

func TestUnparseableMediaMakesSeasonUnknownButSidecarsDoNot(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "poster.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if !s.Season(id, 1).Known {
		t.Fatal("non-media file polluted inventory")
	}
	if err := os.WriteFile(filepath.Join(season, "mystery.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if s.Season(id, 1).Known {
		t.Fatal("ambiguous media was treated as known")
	}
	if got := s.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Unknown {
		t.Fatalf("availability=%v", got)
	}
}

func TestEmptyKnownSeasonIsKnownEmptyAndSymlinkIsUnknown(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	s := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if !s.Season(id, 1).Known {
		t.Fatal("empty readable season should be known")
	}
	if got := s.Availability(episode.Key{EntryKey: id, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}); got != Absent {
		t.Fatalf("availability=%v", got)
	}
	target := filepath.Join(root, "outside.mkv")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(season, "abcabc146 S01E01.mkv")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	s = ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if s.Entries[id].Known {
		t.Fatal("symlink scan should be unknown")
	}
}

func TestOnePhysicalObjectWithTwoEpisodeNamesMakesSeasonUnknown(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	e01 := filepath.Join(season, "abcabc146 S01E01.mkv")
	e02 := filepath.Join(season, "abcabc146 S01E02.mkv")
	if err := os.WriteFile(e01, []byte("same media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(e01, e02); err != nil {
		t.Fatal(err)
	}

	s := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if s.Season(id, 1).Known {
		t.Fatal("one inode interpreted as two episodes must be unknown")
	}
}

func TestOneEpisodeWithTwoPhysicalObjectsMakesSeasonUnknown(t *testing.T) {
	root := t.TempDir()
	id := "series/Test"
	w := catalog.Entry{Key: id, Path: filepath.Join(root, "abcabc146"), Seasons: []int{1}}
	season := filepath.Join(w.Path, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"abcabc146 S01E01.mkv", "abcabc146 S01E01 [alt].mkv"} {
		if err := os.WriteFile(filepath.Join(season, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := ScanWithExtensions(map[string]catalog.Entry{id: w}, []string{".mkv"})
	if s.Season(id, 1).Known {
		t.Fatal("one episode backed by multiple inodes must be unknown")
	}
}

func TestConfiguredExtensionEpisodeAvailability(t *testing.T) {
	for _, name := range []string{"01.m2ts", "abcabc145 - 01.m2ts", "[grpabc165][abcabc145][01].m2ts"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			entry := catalog.Entry{Key: "series/abcabc145", Path: root}
			season := filepath.Join(root, "Season 01")
			if err := os.MkdirAll(season, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(season, name), []byte("media"), 0o644); err != nil {
				t.Fatal(err)
			}
			snapshot := ScanWithExtensions(map[string]catalog.Entry{entry.Key: entry}, []string{".m2ts"})
			key := episode.Key{EntryKey: entry.Key, Season: 1, EpisodeStart: 1, EpisodeEnd: 1}
			if got := snapshot.Availability(key); got != Present {
				t.Fatalf("availability=%v, want present", got)
			}
		})
	}
}

func TestExplicitSeasonZeroOverridesInventoryDirectoryHint(t *testing.T) {
	root := t.TempDir()
	entry := catalog.Entry{Key: "series/abcabc146", Path: root}
	season := filepath.Join(root, "Season 02")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "[grpabc165][abcabc146 S00] - 03.mkv"), []byte("special"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := Scan(map[string]catalog.Entry{entry.Key: entry})
	key := episode.Key{EntryKey: entry.Key, Season: 0, EpisodeStart: 3, EpisodeEnd: 3, Special: true}
	if got := snapshot.Availability(key); got != Present {
		t.Fatalf("special availability=%v, want present", got)
	}
	key.Season, key.Special = 2, false
	if got := snapshot.Availability(key); got == Present {
		t.Fatal("special was counted as season two episode")
	}
}

func TestAvailabilityNormalizesSingleEpisodeKeyWithoutEnd(t *testing.T) {
	s := Snapshot{Entries: map[string]EntryInventory{
		"series/Test": {Known: true, Seasons: map[int]SeasonInventory{1: {Known: true, Episodes: map[int]struct{}{1: {}}}}},
	}}
	if got := s.Availability(episode.Key{EntryKey: "series/Test", Season: 1, EpisodeStart: 1}); got != Present {
		t.Fatalf("single episode with omitted end=%v want present", got)
	}
	if got := s.Availability(episode.Key{EntryKey: "series/Test", Season: 1, EpisodeStart: 2}); got != Absent {
		t.Fatalf("missing single episode with omitted end=%v want absent", got)
	}
}
