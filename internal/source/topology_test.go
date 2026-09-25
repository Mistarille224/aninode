package source

import (
	"path/filepath"
	"strings"
	"testing"

	"aninode/internal/download"
)

func TestRootOnlyTopologyValidationMatchesFullStructuralRules(t *testing.T) {
	seriesRoot := filepath.Join("downloads", "TV", "abcabc146")
	if got := ValidateSeriesRoot(seriesRoot, filepath.Join(seriesRoot, "Season 01")); !got.Valid || got.RelativeDepth != 1 {
		t.Fatalf("series root: %+v", got)
	}
	if got := ValidateSeriesRoot(seriesRoot, filepath.Join(seriesRoot, "Season 01", "Batch")); !got.Valid || got.RelativeDepth != 2 {
		t.Fatalf("series grouped root: %+v", got)
	}
	if got := ValidateSeriesRoot(seriesRoot, filepath.Join(seriesRoot, "Season 01", "Batch", "TooDeep")); got.Valid {
		t.Fatalf("too-deep series root accepted: %+v", got)
	}

	movieRoot := filepath.Join("downloads", "Movies")
	if got := ValidateMovieRoot(movieRoot, filepath.Join(movieRoot, "abcabc147 (2026)")); !got.Valid || got.RelativeDepth != 1 {
		t.Fatalf("movie root: %+v", got)
	}
	if got := ValidateMovieRoot(movieRoot, filepath.Join(movieRoot, "abcabc147 (2026)", "abcabc137")); got.Valid {
		t.Fatalf("nested movie root accepted: %+v", got)
	}
}

func TestSeriesTopologyUsesDepthNotContainerName(t *testing.T) {
	root := filepath.Join("downloads", "TV", "abcabc146")
	for _, name := range []string{"Season 01", "[grpabc165] S01 Batch"} {
		container := filepath.Join(root, name)
		if got := ValidateSeries(root, container, []string{filepath.Join(container, "01.mkv")}); !got.Valid || got.RelativeDepth != 1 {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if got := ValidateSeries(root, root, []string{filepath.Join(root, "01.mkv")}); got.Valid {
		t.Fatalf("flat topology accepted: %+v", got)
	}
	grouped := filepath.Join(root, "Season 01", "Batch")
	if got := ValidateSeries(root, grouped, []string{filepath.Join(grouped, "01.mkv")}); !got.Valid || got.RelativeDepth != 2 {
		t.Fatalf("grouping topology rejected: %+v", got)
	}
}

func TestObserveRejectsEscapesAndMixedContentNamespaces(t *testing.T) {
	root := filepath.Join("downloads", "TV", "abcabc146")
	container := filepath.Join(root, "Batch")
	tests := []struct {
		name  string
		task  download.Task
		files []download.File
	}{
		{"relative escape", download.Task{SavePath: root, ContentPath: container}, []download.File{{Path: filepath.Join(container, "..", "outside.mkv"), Wanted: true}}},
		{"absolute outside", download.Task{SavePath: root, ContentPath: container}, []download.File{{Path: filepath.Join(string(filepath.Separator), "etc", "outside.mkv"), Wanted: true}}},
		{"mixed remote local", download.Task{SavePath: root, ContentPath: container}, []download.File{{Path: filepath.Join(container, "01.mkv"), Wanted: true}, {Path: filepath.Join(string(filepath.Separator), "remote", "01.mkv"), Wanted: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Observe(tt.task, tt.files, nil); err == nil || !strings.Contains(err.Error(), "outside content path") {
				t.Fatalf("escape accepted: %v", err)
			}
		})
	}
}

func TestSeriesAllowsGroupingDirectoriesBelowSeasonContainer(t *testing.T) {
	root := filepath.Join("downloads", "TV", "abcabc146")
	season := filepath.Join(root, "arbitrary-season-container")
	if got := ValidateSeries(root, season, []string{filepath.Join(season, "episode-torrent", "abcabc146 - 01.mkv")}); !got.Valid {
		t.Fatalf("nested grouping media rejected: %+v", got)
	}
	content := filepath.Join(season, "episode-torrent")
	if got := ValidateSeries(root, content, []string{filepath.Join(content, "video", "abcabc146 - 01.mkv")}); !got.Valid {
		t.Fatalf("multifile content root below season rejected: %+v", got)
	}
}

func TestMovieTopologyUsesOneContainerWithNestedBundleAssets(t *testing.T) {
	root := filepath.Join("downloads", "Movies")
	container := filepath.Join(root, "[grpabc165] abcabc147")
	paths := []string{filepath.Join(container, "abcabc147.mkv"), filepath.Join(container, "SPs", "trailer.mkv")}
	if got := ValidateMovie(root, container, paths); !got.Valid {
		t.Fatalf("movie topology: %+v", got)
	}
	if got := ValidateMovie(root, root, []string{filepath.Join(root, "abcabc147.mkv")}); got.Valid {
		t.Fatalf("flat movie accepted: %+v", got)
	}
}

func TestPlacementAccountsForTorrentShape(t *testing.T) {
	entryRoot := filepath.Join("downloads", "TV", "abcabc146")
	movieRoot := filepath.Join("downloads", "Movies")
	tests := []struct {
		media     MediaType
		shape     Shape
		container string
		want      string
	}{
		{Series, MultiFile, "Season 01", filepath.Join(entryRoot, "Season 01")},
		{Series, SingleFile, "Season 01", filepath.Join(entryRoot, "Season 01")},
		{Movie, MultiFile, "abcabc147 (2024)", movieRoot},
		{Movie, SingleFile, "abcabc147 (2024)", filepath.Join(movieRoot, "abcabc147 (2024)")},
	}
	for _, tt := range tests {
		got, err := PlacementFor(tt.media, tt.shape, entryRoot, movieRoot, tt.container)
		if err != nil || got.SavePath != tt.want {
			t.Fatalf("%s/%s: %+v err=%v want=%q", tt.media, tt.shape, got, err, tt.want)
		}
	}
}

func TestValidateSeriesAtSourceAllowsArbitraryWorkAndSeasonNamesButRequiresDepth(t *testing.T) {
	root := t.TempDir()
	content := filepath.Join(root, "[grpabc165] Arbitrary Entry Root", "[grpabc165] S01 Batch")
	media := filepath.Join(content, "abcabc146 S01E01.mkv")
	if got := ValidateSeriesAtSource(root, content, []string{media}); !got.Valid || got.RelativeDepth != 2 {
		t.Fatalf("valid arbitrary source topology rejected: %+v", got)
	}
	tooShallow := filepath.Join(root, "Seasonish")
	if got := ValidateSeriesAtSource(root, tooShallow, []string{filepath.Join(tooShallow, "01.mkv")}); got.Valid {
		t.Fatalf("shallow source topology accepted: %+v", got)
	}
	grouped := filepath.Join(root, "Entry", "Season", "Episode Torrent")
	if got := ValidateSeriesAtSource(root, grouped, []string{filepath.Join(grouped, "abcabc146 - 01.mkv")}); !got.Valid {
		t.Fatalf("grouping content root rejected: %+v", got)
	}
	tooDeep := filepath.Join(root, "Entry", "Season", "Episode Torrent", "abcabc137 Root")
	if got := ValidateSeriesAtSource(root, tooDeep, []string{filepath.Join(tooDeep, "01.mkv")}); got.Valid {
		t.Fatalf("too-deep task content root accepted: %+v", got)
	}
}
