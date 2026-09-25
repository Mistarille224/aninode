package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aninode/internal/filesystem"
	"aninode/internal/releasefilter"
)

func TestEmptyDeclarationAndSchemaContainOnlyIntent(t *testing.T) {
	root := t.TempDir()
	if err := WriteDeclaration(root, Declaration{Title: "abcabc145"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".aninode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["title"]; !ok {
		t.Fatalf("title missing: %s", data)
	}
	if _, err := ReadDeclaration(root); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"id", "media_type", "managed", "version"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("found runtime field %q", forbidden)
		}
	}
}

func TestDeclarationPersistsExplicitFiltersAndSpecialsSeparately(t *testing.T) {
	root := t.TempDir()
	d := Declaration{
		Title: "abcabc145",
		Filters: releasefilter.Filters{
			Groups:      []string{"grpabc166"},
			Resolutions: []string{"1080p"},
		},
		Specials: []SpecialMapping{{Source: "OVA.mkv", Season: 0, Episode: 1}},
	}
	if err := WriteDeclaration(root, d); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".aninode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["filters"]; !ok {
		t.Fatalf("filters missing: %s", data)
	}
	if _, ok := fields["specials"]; !ok {
		t.Fatalf("specials missing: %s", data)
	}
	if _, ok := fields["policy"]; ok {
		t.Fatalf("removed policy field persisted: %s", data)
	}
	stored, err := ReadDeclaration(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Filters.Groups) != 1 || stored.Filters.Groups[0] != "grpabc166" || len(stored.Specials) != 1 {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestDeclarationRejectsRemovedPolicyField(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".aninode.json"), []byte(`{"title":"abcabc145","policy":{"groups":["grpabc166"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeclaration(root); err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("removed policy field was accepted: %v", err)
	}
}

func TestNewInitialDeclarationDefaultsByMediaType(t *testing.T) {
	series := NewInitialDeclaration(InitialDeclarationInput{MediaType: MediaSeries, CanonicalTitle: " abcabc145 "})
	if series.Title != "abcabc145" || series.Output.Title != "" || series.Blacklist == nil {
		t.Fatalf("series=%+v", series)
	}
	movie := NewInitialDeclaration(InitialDeclarationInput{MediaType: MediaMovie, CanonicalTitle: "abcabc defdef"})
	if movie.Title != "abcabc defdef" || movie.Output.Title != "" || movie.Blacklist == nil {
		t.Fatalf("movie=%+v", movie)
	}
}

func TestEmptyDeclarationIsEnabled(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "Existing (2024)", "Season 01")
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := AdoptUndeclared(root); err != nil {
		t.Fatal(err)
	}
	ws, err := Discover(root)
	if err != nil || len(ws) != 1 {
		t.Fatalf("%v %+v", err, ws)
	}
	for _, w := range ws {
		if !w.Enabled {
			t.Fatal("empty declaration should use enabled default")
		}
		if w.Title != "Existing" || w.Year != 2024 {
			t.Fatalf("entry=%+v", w)
		}
		if _, e := EnsureSeason(w, 2); e != nil {
			t.Fatal(e)
		}
	}
}
func TestCreateManagedUsesPlainTitleAndOptionalYear(t *testing.T) {
	root := t.TempDir()
	w, err := CreateManaged(root, "abcabc", 2024, 1, Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(w.Path) != "abcabc (2024)" || !w.Enabled {
		t.Fatalf("entry=%+v", w)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "Season 01")); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndDiscoverMovieWithoutSeasonDirectory(t *testing.T) {
	root := t.TempDir()
	w, err := CreateManagedMovie(root, "abcabc147", 2026, Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	if w.MediaType != MediaMovie || len(w.Seasons) != 0 {
		t.Fatalf("entry=%+v", w)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "Season 01")); !os.IsNotExist(err) {
		t.Fatalf("movie gained season directory: %v", err)
	}
	entries, err := DiscoverMovies(root)
	if err != nil || entries[w.Key].MediaType != MediaMovie {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
}

func TestMovieDiscoveryDoesNotAdoptUndeclaredFolders(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"abcabc134 (2024)", "abcabc130 (2024)"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "abcabc130 (2024)", "poster.jpg"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	mixed := filepath.Join(root, "abcabc133 (2024)")
	if err := os.MkdirAll(filepath.Join(mixed, "BDMV"), 0755); err != nil {
		t.Fatal(err)
	}
	for path := range map[string]bool{filepath.Join(mixed, "abcabc133.mkv"): true, filepath.Join(mixed, "BDMV", "index.bdmv"): true} {
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "abcabc132 (2024).mkv"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := DiscoverMovies(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries=%+v", entries)
	}
	for _, dir := range []string{"abcabc134 (2024)", "abcabc130 (2024)"} {
		if _, err := os.Stat(filepath.Join(root, dir, ".aninode.json")); !os.IsNotExist(err) {
			t.Fatalf("%s gained sidecar: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mixed, ".aninode.json")); !os.IsNotExist(err) {
		t.Fatal("read-only discovery adopted movie:", err)
	}
}

func TestRuntimeKeyFollowsSourceRename(t *testing.T) {
	root := t.TempDir()
	e, err := CreateManaged(root, "Original", 2026, 1, Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	oldKey := e.Key
	newPath := filepath.Join(root, "Renamed (2026)")
	if err := os.Rename(e.DeclarationPath, newPath); err != nil {
		t.Fatal(err)
	}
	entries, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := entries[oldKey]; ok {
		t.Fatalf("runtime key incorrectly survived rename: %+v", entries)
	}
	newKey, _ := Key(MediaSeries, "Renamed (2026)")
	got, ok := entries[newKey]
	if !ok || got.Title != "Original" || got.DeclarationPath != newPath {
		t.Fatalf("renamed entry=%+v", got)
	}
}

func TestAdoptionUsesPathDerivedRuntimeKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Existing (2024)", "Season 01")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	firstAdopted, err := AdoptUndeclared(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	secondAdopted, err := AdoptUndeclared(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstAdopted) != 1 || len(secondAdopted) != 0 || len(first) != 1 {
		t.Fatalf("adopted=%v/%v entries=%+v", firstAdopted, secondAdopted, first)
	}
	key, err := Key(MediaSeries, "Existing (2024)")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first[key]; !ok {
		t.Fatalf("missing path-derived key %q: %+v", key, first)
	}
}

func TestDiscoverDoesNotCreateDeclarationForCanonicalDirectory(t *testing.T) {
	root := t.TempDir()
	series := filepath.Join(root, "Existing (2024)")
	for _, season := range []string{"Season 01", "Season 02"} {
		if err := os.MkdirAll(filepath.Join(series, season), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{
		filepath.Join(series, "Season 01", "[grpabc151][abcabc][001][1080P].mkv"),
		filepath.Join(series, "Season 01", "[grpabc151][abcabc][002][1080P].mkv"),
		filepath.Join(series, "Season 02", "[grpabc167][甲乙甲乙][003][1080P].mkv"),
		filepath.Join(series, "Season 02", "NCOP S02E01.mkv"),
		filepath.Join(series, "Season 02", "sample.mkv"),
	}
	for _, path := range files {
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]os.FileInfo{}
	for _, path := range files {
		info, _ := os.Stat(path)
		before[path] = info
	}
	entries, err := Discover(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(series, ".aninode.json")); !os.IsNotExist(err) {
		t.Fatalf("load-time discovery wrote declaration: %v", err)
	}
	for _, path := range files {
		after, statErr := os.Stat(path)
		if statErr != nil || !os.SameFile(before[path], after) {
			t.Fatalf("source mutated %s: %v", path, statErr)
		}
	}
}

func TestLocalAdoptionPersistsOnlyCanonicalIntent(t *testing.T) {
	root := t.TempDir()
	series := filepath.Join(root, "abcabc131 (2026)")
	if err := os.MkdirAll(filepath.Join(series, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(series, "Season 01", "[grpabc165] Tracker Facing Name - 01.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AdoptUndeclared(root); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDeclaration(series)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "abcabc131" {
		t.Fatalf("declaration=%+v", got)
	}
	data, err := os.ReadFile(filepath.Join(series, ".aninode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Tracker Facing Name") {
		t.Fatalf("derived names were persisted: %s", data)
	}
}

func TestExplicitAdoptionCreatesDeclarationOnlyInSourceTreeAndPreservesIt(t *testing.T) {
	root := t.TempDir()
	source, library := filepath.Join(root, "source"), filepath.Join(root, "library")
	series := filepath.Join(source, "abcabc146 (2026)")
	if err := os.MkdirAll(filepath.Join(series, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := AdoptUndeclaredAt(library, source)
	// This fixture already lives in the source tree; explicit adoption from the
	// same tree is intentional and observable.
	if err == nil {
		_, err = AdoptUndeclaredAt(source, source)
	}
	entries, loadErr := DiscoverAt(source, library)
	if err != nil || loadErr != nil || len(entries) != 1 {
		t.Fatalf("entries=%+v err=%v load=%v", entries, err, loadErr)
	}
	if _, err := os.Stat(filepath.Join(series, ".aninode.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(library, "abcabc146 (2026)", ".aninode.json")); !os.IsNotExist(err) {
		t.Fatalf("sidecar written to library: %v", err)
	}
	custom := []byte("{\"obsolete_names\":[\"custom\"],\"title\":\"abcabc146\"}\n")
	declarationPath := filepath.Join(series, ".aninode.json")
	if err := os.WriteFile(declarationPath, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverAt(source, library); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("invalid declaration was not rejected: %v", err)
	}
	got, err := os.ReadFile(declarationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(custom) {
		t.Fatalf("read path mutated declaration: got=%s want=%s", got, custom)
	}
}

func TestRootDeclarationOwnsFolderProjection(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	libraryRoot := filepath.Join(root, "library")
	e, err := CreateManagedAt(sourceRoot, libraryRoot, "abcabc146", 2026, 1, Declaration{Title: "abcabc146", Folders: map[string]FolderProjection{"Season 01": {Season: 2, EpisodeOffset: -12}}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := DiscoverAt(sourceRoot, libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	got := entries[e.Key]
	if got.FolderProjections["Season 01"].Season != 2 || got.FolderProjections["Season 01"].EpisodeOffset != -12 {
		t.Fatalf("runtime folder projection=%+v", got)
	}
	rootData, err := os.ReadFile(filepath.Join(e.DeclarationPath, ".aninode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rootData), `"folders"`) || !strings.Contains(string(rootData), `"season": 2`) {
		t.Fatalf("root folder projection missing: %s", rootData)
	}
	if _, err := os.Stat(filepath.Join(e.DeclarationPath, "Season 01", ".aninode.json")); !os.IsNotExist(err) {
		t.Fatalf("season sidecar should not exist: %v", err)
	}
}

func TestFolderProjectionCanTargetSpecialsSeason(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	libraryRoot := filepath.Join(root, "library")
	entryRoot := filepath.Join(sourceRoot, "abcabc146 (2026)")
	container := filepath.Join(entryRoot, "Extras")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "abcabc146 - 01.mkv"), []byte("special"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteDeclaration(entryRoot, Declaration{Title: "abcabc146", Year: 2026, Folders: map[string]FolderProjection{"Extras": {Season: 0}}}); err != nil {
		t.Fatal(err)
	}

	entries, err := DiscoverAt(sourceRoot, libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["series/abcabc146 (2026)"]
	if len(entry.Seasons) != 1 || entry.Seasons[0] != 0 {
		t.Fatalf("seasons=%v, want [0]", entry.Seasons)
	}
}

func TestOutputLayoutAndBlacklist(t *testing.T) {
	w := Entry{Key: "series:id", Title: "Source", Path: "/library/Source", DeclarationPath: "/source/Source", Output: OutputLayout{Title: "Target"}}
	if OutputTitle(w) != "Target" || w.Title != "Source" || w.Key != "series:id" {
		t.Fatalf("identity changed: %+v", w)
	}
	if pattern, ok := BlacklistMatch([]string{"*NCOP*"}, "show ncop.mkv", "SP/show ncop.mkv"); !ok || pattern != "*NCOP*" {
		t.Fatal("case-insensitive blacklist did not match")
	}
	if err := ValidateBlacklist([]string{"["}); err == nil {
		t.Fatal("invalid glob accepted")
	}
}

func TestDiscoverCanonicalMovieDoesNotCreateDeclaration(t *testing.T) {
	root := t.TempDir()
	movie := filepath.Join(root, "abcabc148 (2026)")
	if err := os.MkdirAll(movie, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movie, "abcabc002 S01E01.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := DiscoverMovies(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(movie, ".aninode.json")); !os.IsNotExist(err) {
		t.Fatalf("read-only movie discovery wrote declaration: %v", err)
	}
}

func TestDeclarationRejectsPersistedDownloaderOwnership(t *testing.T) {
	root := t.TempDir()
	data := []byte(`{"id":"series/Test","media_type":"series","title":"abcabc146","sources":[{"client":"qbit","task_id":"123","content_root":"/downloads/abcabc146"}]}`)
	if err := os.WriteFile(filepath.Join(root, ".aninode.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeclaration(root); err == nil {
		t.Fatal("downloader ownership leaked back into semantic Entry declaration schema")
	}
}

func TestMoveDeclarationMovesOnlyRootSidecarWithoutMovingMedia(t *testing.T) {
	root := t.TempDir()
	oldRoot := filepath.Join(root, "source", "TV", "abcabc019 (2026)")
	newRoot := filepath.Join(root, "source", "TV", "[grpabc165] abcabc016 Root")
	library := filepath.Join(root, "library", "TV")
	entry, err := CreateManagedAt(filepath.Dir(oldRoot), library, "abcabc019", 2026, 1, Declaration{Folders: map[string]FolderProjection{"[grpabc165] S01 Batch": {Season: 2, EpisodeOffset: -12}}})
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(newRoot, "[grpabc165] S01 Batch", "abcabc019 S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, err := MoveDeclaration(entry, newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Key != "series/[grpabc165] abcabc016 Root" || moved.DeclarationPath != newRoot {
		t.Fatalf("moved=%+v", moved)
	}
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("destination media changed: %v", err)
	}
	d, err := ReadDeclaration(newRoot)
	if err != nil || d == nil {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
	if got := d.Folders["[grpabc165] S01 Batch"]; got.Season != 2 || got.EpisodeOffset != -12 {
		t.Fatalf("projection=%+v", got)
	}
}

func TestAdoptStructuredSourcesUsesTopologyWithoutMovingMedia(t *testing.T) {
	root := filepath.Join(t.TempDir(), "TV")
	container := filepath.Join(root, "abcabc146 (2026)", "batch-from-user")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(container, "abcabc146 S01E04.mkv")
	if err := os.WriteFile(media, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	adopted, err := AdoptStructuredSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != "series/abcabc146 (2026)" {
		t.Fatalf("adopted=%v", adopted)
	}
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("source media moved during adoption: %v", err)
	}
	d, err := ReadDeclaration(filepath.Join(root, "abcabc146 (2026)"))
	if err != nil || d == nil || d.Title != "abcabc146" || d.Year != 2026 {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
	second, err := AdoptStructuredSources(root)
	if err != nil || len(second) != 0 {
		t.Fatalf("second adoption=%v err=%v", second, err)
	}
}

func TestAdoptStructuredSourcesRestoresDeclarationFromEmptyTopology(t *testing.T) {
	root := filepath.Join(t.TempDir(), "TV")
	entryRoot := filepath.Join(root, "abcabc045 (2026)")
	if err := os.MkdirAll(filepath.Join(entryRoot, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}

	for pass := 0; pass < 2; pass++ {
		adopted, err := AdoptStructuredSources(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(adopted) != 1 || adopted[0] != "series/abcabc045 (2026)" {
			t.Fatalf("pass %d adopted=%v", pass, adopted)
		}
		d, err := ReadDeclaration(entryRoot)
		if err != nil || d == nil || d.Folders["Season 01"].Season != 1 {
			t.Fatalf("pass %d declaration=%+v err=%v", pass, d, err)
		}
		if err := os.Remove(filepath.Join(entryRoot, ".aninode.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdoptStructuredMoviesUsesPlacementWithoutRecognizableMedia(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Movies")
	name := "[grpabc165] abcabc111 [tagabc_816p]"
	entryRoot := filepath.Join(root, name)
	for _, child := range []string{"CDs", "Scans", "SPs"} {
		if err := os.MkdirAll(filepath.Join(entryRoot, child), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(entryRoot, "[grpabc165] abcabc [Fonts].zip"), []byte("fonts"), 0o644); err != nil {
		t.Fatal(err)
	}

	adopted, err := AdoptStructuredMovies(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != "movie/"+name {
		t.Fatalf("adopted=%v", adopted)
	}
	d, err := ReadDeclaration(entryRoot)
	if err != nil || d == nil || d.Title != name {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
}

func TestAdoptStructuredSourcesRecoversCanonicalSeasonWithWeakEpisodeNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "TV")
	container := filepath.Join(root, "abcabc022 (2026)", "Season 01")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"01", "02", "03"} {
		if err := os.WriteFile(filepath.Join(container, "[grpabc165] abcabc022 - "+ep+" [1080P].mkv"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	adopted, err := AdoptStructuredSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != "series/abcabc022 (2026)" {
		t.Fatalf("adopted=%v", adopted)
	}
	d, err := ReadDeclaration(filepath.Join(root, "abcabc022 (2026)"))
	if err != nil || d == nil {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
}

func TestAdoptStructuredSourcesGuessesSeasonOneForSingleUnlabeledContainer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "TV")
	container := filepath.Join(root, "abcabc053", "release-batch")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"01", "02"} {
		if err := os.WriteFile(filepath.Join(container, "abcabc053 - "+ep+".mkv"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	values, err := InferSeriesContainers(filepath.Join(root, "abcabc053"))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SuggestedSeason != 1 {
		t.Fatalf("inferred=%+v", values)
	}
	adopted, err := AdoptStructuredSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 {
		t.Fatalf("adopted=%v", adopted)
	}
}

func TestInferSeriesContainersIgnoresDirectoryNamesAndReadsNestedLeafNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "abcabc146")
	container := filepath.Join(root, "Season 09 - E99 misleading", "episode bundle S88E77")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"abcabc146 S02E03.mkv", "abcabc146 S02E04.mkv"} {
		if err := os.WriteFile(filepath.Join(container, name), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	values, err := InferSeriesContainers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SuggestedSeason != 2 || len(values[0].Media) != 2 {
		t.Fatalf("inferred=%+v", values)
	}
}

func TestInferSeriesContainersUsesConventionalSeasonContainerAsSuggestion(t *testing.T) {
	for _, folder := range []string{"Season 02", "season02"} {
		t.Run(folder, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "abcabc146")
			container := filepath.Join(root, folder)
			if err := os.MkdirAll(container, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(container, "abcabc146 - 41.mkv"), []byte("media"), 0o644); err != nil {
				t.Fatal(err)
			}
			values, err := InferSeriesContainers(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(values) != 1 || values[0].SuggestedSeason != 2 {
				t.Fatalf("inferred=%+v", values)
			}
		})
	}
}

func TestSyncFolderProjectionsTracksRealFolderNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "TV")
	show := filepath.Join(root, "abcabc146")
	oldFolder := filepath.Join(show, "random batch")
	if err := os.MkdirAll(oldFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldFolder, "abcabc146 - 01.mkv"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createDeclaration(show, Declaration{Title: "abcabc146"}); err != nil {
		t.Fatal(err)
	}
	changed, err := SyncFolderProjections(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed=%v", changed)
	}
	d, _ := ReadDeclaration(show)
	if got := d.Folders["random batch"]; got.Season != 1 {
		t.Fatalf("folders=%+v", d.Folders)
	}
	newFolder := filepath.Join(show, "第2あいう")
	if err := os.Rename(oldFolder, newFolder); err != nil {
		t.Fatal(err)
	}
	changed, err = SyncFolderProjections(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Fatalf("rename changed=%v", changed)
	}
	d, _ = ReadDeclaration(show)
	if _, ok := d.Folders["random batch"]; ok {
		t.Fatalf("stale folder rule survived: %+v", d.Folders)
	}
	if _, ok := d.Folders["第2あいう"]; !ok {
		t.Fatalf("new folder rule missing: %+v", d.Folders)
	}
}

func TestAdoptStructuredSourcesWritesDeclarationEvenBeforeEpisodeRecognition(t *testing.T) {
	root := filepath.Join(t.TempDir(), "TV")
	container := filepath.Join(root, "abcabc015 (2026)", "release-folder")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "[grpabc165] abcabc015 [1080P].mkv"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	adopted, err := AdoptStructuredSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != "series/abcabc015 (2026)" {
		t.Fatalf("adopted=%v", adopted)
	}
	d, err := ReadDeclaration(filepath.Join(root, "abcabc015 (2026)"))
	if err != nil || d == nil {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
	if got := d.Folders["release-folder"]; got.Season != 1 {
		t.Fatalf("folder projection=%+v declaration=%+v", got, d)
	}
}

func TestCreateManagedRejectsTitlesOutsideOneDirectoryComponent(t *testing.T) {
	root := t.TempDir()
	declarations := filepath.Join(root, "declarations")
	library := filepath.Join(root, "library")

	for _, title := range []string{"../escaped", "nested/show", ".", "..", "show\nname"} {
		t.Run(title, func(t *testing.T) {
			_, err := CreateManagedAt(declarations, library, title, 2026, 1, Declaration{})
			if err == nil {
				t.Fatalf("CreateManagedAt accepted unsafe title %q", title)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("unsafe title created a path outside the declaration root: %v", err)
	}
}

func TestCreateManagedValidatesBeforeCreatingDirectories(t *testing.T) {
	root := t.TempDir()
	declarations := filepath.Join(root, "declarations")
	library := filepath.Join(root, "library")
	if _, err := CreateManagedMovieAt(declarations, library, "movie/escape", 2026, Declaration{}); err == nil {
		t.Fatal("CreateManagedMovieAt accepted an unsafe title")
	}
	for _, path := range []string{declarations, library} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("validation failure created %s: %v", path, err)
		}
	}
}

func TestProjectObservedEpisodeExplicitSeasonIgnoresUnrelatedSoleFolder(t *testing.T) {
	w := Entry{Seasons: []int{1}}
	evidence := []FolderProjectionEvidence{{Folder: "Season 01", TargetSeason: 1}}

	season, episode, folder, ok := ProjectObservedEpisode(w, evidence, 2, true, 1)
	if !ok || season != 2 || episode != 1 || folder != "" {
		t.Fatalf("got season=%d episode=%d folder=%q ok=%v", season, episode, folder, ok)
	}
}

func TestWriteDeclarationDoesNotMutateCallerMovieClassify(t *testing.T) {
	root := filepath.Join(t.TempDir(), "abcabc147")
	d := Declaration{Title: "abcabc147", Movie: MovieDeclaration{Classify: map[string]string{"sample.mkv": "auto", "main.mkv": "extra"}}}
	if err := WriteDeclaration(root, d); err != nil {
		t.Fatal(err)
	}
	if got := d.Movie.Classify["sample.mkv"]; got != "auto" {
		t.Fatalf("WriteDeclaration mutated caller map: %+v", d.Movie.Classify)
	}
	stored, err := ReadDeclaration(root)
	if err != nil || stored == nil {
		t.Fatalf("read declaration: %v", err)
	}
	if _, ok := stored.Movie.Classify["sample.mkv"]; ok {
		t.Fatalf("auto classification persisted instead of being normalized: %+v", stored.Movie.Classify)
	}
}

func TestInferSeriesContainersPreservesSeasonZeroSuggestion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "abcabc146")
	container := filepath.Join(root, "Season 00")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "abcabc146 S00E03.mkv"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	values, err := InferSeriesContainers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SuggestedSeason != 0 {
		t.Fatalf("Season 00 was replaced by a default season: %+v", values)
	}
}

func TestProjectObservedEpisodeExplicitSeasonZeroIgnoresSeasonlessFallback(t *testing.T) {
	w := Entry{Seasons: []int{1}}
	evidence := []FolderProjectionEvidence{{Folder: "Season 01", TargetSeason: 1}}
	season, ep, folder, ok := ProjectObservedEpisode(w, evidence, 0, true, 3)
	if !ok || season != 0 || ep != 3 || folder != "" {
		t.Fatalf("explicit S00 was redirected: season=%d episode=%d folder=%q ok=%v", season, ep, folder, ok)
	}
}

func TestObserveFolderProjectionEvidenceReadsNestedCustomMediaExtension(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Pack")
	if err := os.MkdirAll(filepath.Join(folder, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "nested", "[grpabc165] abcabc146 - 03 [1080p].video"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := Entry{MediaType: MediaSeries, DeclarationPath: root, FolderProjections: map[string]FolderProjection{"Pack": {Season: 1}}}
	got := ObserveFolderProjectionEvidence(w, []string{"video"})
	if len(got) != 1 || got[0].MinEpisode != 3 || got[0].MaxEpisode != 3 || got[0].EpisodeCount != 1 {
		t.Fatalf("evidence=%+v", got)
	}
}

func TestInferSeriesContainersFromSnapshotUsesNestedCustomMediaExtension(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Pack")
	nested := filepath.Join(folder, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "abcabc146 S02E04.video")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := filesystem.ObserveTreeIfPresent(root)
	if err != nil {
		t.Fatal(err)
	}
	got := InferSeriesContainersFromSnapshot(root, snapshot, []string{"video"})
	if len(got) != 1 || got[0].SuggestedSeason != 2 || len(got[0].Media) != 1 || got[0].Media[0] != path {
		t.Fatalf("containers=%+v", got)
	}
}

func TestFolderProjectionEvidenceFromSnapshotDoesNotRescanFilesystem(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc146")
	folder := filepath.Join(entryRoot, "Batch")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"[grpabc165] abcabc146 - 01 [1080p].mkv", "[grpabc165] abcabc146 - 02 [1080p].mkv"} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := filesystem.ObserveTree(entryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(entryRoot); err != nil {
		t.Fatal(err)
	}
	w := Entry{MediaType: MediaSeries, DeclarationPath: entryRoot, FolderProjections: map[string]FolderProjection{"Batch": {Season: 1}}}
	evidence := ObserveFolderProjectionEvidenceFromSnapshot(w, snapshot, []string{".mkv"})
	if len(evidence) != 1 || evidence[0].MinEpisode != 1 || evidence[0].MaxEpisode != 2 || evidence[0].StableGroup != "grpabc165" {
		t.Fatalf("evidence=%+v", evidence)
	}
}
