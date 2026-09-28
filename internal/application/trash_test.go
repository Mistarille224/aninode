package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aninode/internal/catalog"
	"aninode/internal/download"
	"aninode/internal/rss"
)

func trashFixture(t *testing.T) (*App, fixture, catalog.Entry, string, string) {
	t.Helper()
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "Trash Example", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(w.DeclarationPath, "Season 01", "Trash Example - 01.mkv")
	target := filepath.Join(w.Path, "Season 01", "Trash Example - S01E01.mkv")
	mustWrite(t, source, "media")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, target); err != nil {
		t.Fatal(err)
	}
	return service(t, f, fakeFetcher{entries: map[string][]rss.Entry{}}), f, w, source, target
}

func TestTrashRestoreAndPurge(t *testing.T) {
	app, _, w, source, target := trashFixture(t)
	ctx := context.Background()
	preview, err := app.PreviewTrash(ctx, w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceFiles != 1 || preview.LibraryFiles != 1 || preview.SourceBytes != 5 {
		t.Fatalf("preview: %+v", preview)
	}
	if _, err := app.TrashEntry(ctx, w.Key, "stale"); err == nil {
		t.Fatal("stale preview accepted")
	}
	item, err := app.TrashEntry(ctx, w.Key, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := app.TrashEntry(ctx, w.Key, preview.Token); err != nil || repeated.CreatedAt != item.CreatedAt {
		t.Fatalf("idempotent trash retry: %+v, %v", repeated, err)
	}
	if item.ExpiresAt.Sub(item.CreatedAt) != 7*24*time.Hour {
		t.Fatalf("retention: %+v", item)
	}
	if _, err := app.GetEntry(ctx, w.Key); err == nil {
		t.Fatal("trashed work still in catalog")
	}
	if adopted, err := app.EnsureSourceDeclarations(ctx); err != nil || len(adopted) != 0 {
		t.Fatalf("trashed work was readopted: %v, %v", adopted, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	if values, err := app.ListTrash(ctx); err != nil || len(values) != 1 {
		t.Fatalf("trash: %+v, %v", values, err)
	}
	if _, err := app.RestoreTrash(ctx, w.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetEntry(ctx, w.Key); err != nil {
		t.Fatal(err)
	}
	preview, err = app.PreviewTrash(ctx, w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.TrashEntry(ctx, w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := app.PurgeTrash(ctx, w.Key); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, target} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
	if values, err := app.ListTrash(ctx); err != nil || len(values) != 0 {
		t.Fatalf("trash after purge: %+v, %v", values, err)
	}
}

func TestTrashWithoutPreview(t *testing.T) {
	app, _, w, _, _ := trashFixture(t)
	item, err := app.TrashEntry(context.Background(), w.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if item.Token == "" || item.SourceFiles != 1 || item.LibraryFiles != 1 {
		t.Fatalf("trash entry: %+v", item)
	}
}

func TestTrashRefusesChangedFileAndCrossWorkTask(t *testing.T) {
	app, f, w, source, _ := trashFixture(t)
	ctx := context.Background()
	f.client.tasks = []download.Task{{ID: "mixed", State: download.StateSeeding, ContentPath: w.DeclarationPath}}
	f.client.files["mixed"] = []download.File{{Path: source, Wanted: true}, {Path: filepath.Join(f.root, "other.mkv"), Wanted: true}}
	if _, err := app.PreviewTrash(ctx, w.Key); err == nil {
		t.Fatal("cross-work task was accepted")
	}
	f.client.tasks = nil
	preview, err := app.PreviewTrash(ctx, w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.TrashEntry(ctx, w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.PurgeTrash(ctx, w.Key); err == nil {
		t.Fatal("changed source was deleted")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RestoreTrash(ctx, w.Key); err != nil {
		t.Fatal(err)
	}
}

func TestTrashPausesAndRestoresDownloaderTask(t *testing.T) {
	app, f, w, source, _ := trashFixture(t)
	ctx := context.Background()
	f.client.tasks = []download.Task{{ID: "seed", Name: "seed", State: download.StateSeeding, ContentPath: w.DeclarationPath}}
	f.client.files["seed"] = []download.File{{Path: source, Wanted: true}}
	preview, err := app.PreviewTrash(ctx, w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Tasks) != 1 {
		t.Fatalf("tasks: %+v", preview.Tasks)
	}
	if _, err := app.TrashEntry(ctx, w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	if f.client.tasks[0].State != download.StatePaused {
		t.Fatalf("task was not paused: %+v", f.client.tasks[0])
	}
	if _, err := app.RestoreTrash(ctx, w.Key); err != nil {
		t.Fatal(err)
	}
	if f.client.tasks[0].State != download.StateSeeding {
		t.Fatalf("task was not resumed: %+v", f.client.tasks[0])
	}
}

func TestTrashPreviewSurvivesDownloaderStateChange(t *testing.T) {
	app, f, w, source, _ := trashFixture(t)
	f.client.tasks = []download.Task{{ID: "seed", State: download.StateSeeding, ContentPath: w.DeclarationPath}}
	f.client.files["seed"] = []download.File{{Path: source}}
	preview, err := app.PreviewTrash(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	f.client.tasks[0].State = download.StatePaused
	if _, err := app.TrashEntry(context.Background(), w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
}

func TestTrashMapsRemoteDownloaderPaths(t *testing.T) {
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "Remote Trash", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(w.DeclarationPath, "Remote Trash - 01.mkv")
	mustWrite(t, source, "media")
	remote := filepath.Join("/remote", strings.TrimPrefix(source, filepath.Join(f.root, "downloads")))
	mustWrite(t, filepath.Join(f.cfg, "clients", "c.json"), `{"id":"c","type":"aria2","url":"http://client","enabled":true,"path_mappings":[{"remote":"/remote","local":"`+filepath.Join(f.root, "downloads")+`"}]}`)
	f.client.tasks = []download.Task{{ID: "remote", State: download.StateSeeding, ContentPath: filepath.Dir(remote)}}
	f.client.files["remote"] = []download.File{{Path: remote}}
	app := service(t, f, fakeFetcher{})
	preview, err := app.PreviewTrash(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Tasks) != 1 || preview.Tasks[0].ContentPath != w.DeclarationPath {
		t.Fatalf("remote task not matched: %+v", preview.Tasks)
	}
	if _, err := app.TrashEntry(context.Background(), w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := app.PurgeTrash(context.Background(), w.Key); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredTrashPurgesOnCycle(t *testing.T) {
	app, _, w, source, target := trashFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	preview, err := app.PreviewTrash(ctx, w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.TrashEntry(ctx, w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7*24*time.Hour + time.Minute)
	if _, err := app.Cycle(ctx, CycleOptions{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, target} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired trash left %s: %v", path, err)
		}
	}
}

func TestTrashRejectsModifiedOperationRecord(t *testing.T) {
	app, _, w, source, _ := trashFixture(t)
	ctx := context.Background()
	preview, err := app.PreviewTrash(ctx, w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.TrashEntry(ctx, w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(w.DeclarationPath, trashFile)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), source, filepath.Join(filepath.Dir(source), "other.mkv"), 1))
	if err := os.WriteFile(marker, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ListTrash(ctx); err == nil {
		t.Fatal("modified deletion manifest was accepted")
	}
}

func TestCorruptTrashDoesNotBlockOtherExpiredPurge(t *testing.T) {
	app, f, w, source, target := trashFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	preview, err := app.PreviewTrash(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.TrashEntry(context.Background(), w.Key, preview.Token); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(f.src, "Broken", trashFile)
	mustWrite(t, bad, "invalid")
	now = now.Add(8 * 24 * time.Hour)
	if failures := app.purgeExpiredTrashLocked(context.Background()); len(failures) == 0 {
		t.Fatal("corrupt trash marker was not reported")
	}
	for _, path := range []string{source, target} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("valid expired trash was not purged: %s: %v", path, err)
		}
	}
}
