package organizer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func seriesConfig(t *testing.T, source, library string, managed bool) Config {
	t.Helper()
	cfg, err := WithEmbyLayout(Config{Source: source, Target: library, Extensions: []string{".mkv"}, Managed: managed}, EmbyLayout{LibraryRoot: library, Title: "abcabc146", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestWorkBlacklistExcludesBeforeParsingAndApply(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "abcabc146 NCOP.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := WithEntryBlacklist(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, []string{"*ncop*"})
	var err error
	cfg, err = WithEmbyLayout(cfg, EmbyLayout{LibraryRoot: root, Title: "abcabc146", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].Action != ActionExcluded {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil || result.Excluded != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("source changed:", err)
	}
}

func TestWorkBlacklistMatchesRelativePathAndLeavesNormalEpisode(t *testing.T) {
	root := t.TempDir()
	source, library := filepath.Join(root, "source"), filepath.Join(root, "library")
	if err := os.MkdirAll(filepath.Join(source, "SP"), 0o755); err != nil {
		t.Fatal(err)
	}
	excluded := filepath.Join(source, "SP", "abcabc146 S01E01.mkv")
	normal := filepath.Join(source, "abcabc146 S01E02.mkv")
	for _, path := range []string{excluded, normal} {
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := WithEntryBlacklist(Config{Source: source, Target: library, Extensions: []string{".mkv"}}, []string{"SP/*"})
	var err error
	cfg, err = WithEmbyLayout(cfg, EmbyLayout{LibraryRoot: root, Title: "abcabc146", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	actions := map[string]Action{}
	for _, item := range plan.Items {
		actions[item.Source] = item.Action
	}
	if actions[excluded] != ActionExcluded || actions[normal] != ActionLink {
		t.Fatalf("actions=%v", actions)
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil || result.Excluded != 1 || result.Linked != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(excluded); err != nil {
		t.Fatal("excluded source changed:", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Target, "Season 01", "abcabc146 S01E01.mkv")); !os.IsNotExist(err) {
		t.Fatalf("excluded destination exists: %v", err)
	}
}

func applyFullPlan(ctx context.Context, cfg Config) (Result, error) {
	plan, planErr := BuildPlan(ctx, cfg)
	result, applyErr := ApplyPlan(ctx, cfg, plan)
	return result, errors.Join(planErr, applyErr)
}

func TestReconcileRestoresCanonicalPathAfterManualRename(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(source, "abcabc146 E01.mkv")
	if err := os.WriteFile(input, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, source, library, false)
	if _, err := applyFullPlan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(cfg.Target, "Season 01", "abcabc146 S01E01.mkv")
	manual := filepath.Join(cfg.Target, "manual.mkv")
	if err := os.Rename(canonical, manual); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].Action != ActionLink {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err := ApplyPlan(context.Background(), cfg, plan); err != nil {
		t.Fatal(err)
	}
	if !sameFile(input, canonical) || !sameFile(input, manual) {
		t.Fatal("canonical path was not restored as a hard link")
	}
}

func TestSafeDestinationRejectsEscape(t *testing.T) {
	if _, err := safeDestination("/library", "../escape.mkv"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildPlanDoesNotMutateAndApplyCreatesLink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(source, "abcabc146 E01.mkv")
	if err := os.WriteFile(input, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, source, library, false)
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].Action != ActionLink {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err := os.Stat(cfg.Target); !os.IsNotExist(err) {
		t.Fatalf("planning unexpectedly created target: %v", err)
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(cfg.Target, "Season 01", "abcabc146 S01E01.mkv")
	if result.Linked != 1 || !sameFile(input, canonical) {
		t.Fatalf("result=%+v", result)
	}
}

func TestApplyPlanRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Source: source, Target: target, Extensions: []string{".mkv"}}
	plan := Plan{Items: []PlanItem{{Source: filepath.Join(source, "file.mkv"), Destination: filepath.Join(root, "escape.mkv"), Action: ActionLink}}}
	if _, err := ApplyPlan(context.Background(), cfg, plan); err == nil {
		t.Fatal("expected escaping plan to be rejected")
	}
}

func TestApplyPlanRejectsSymlinkedTargetDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	outside := filepath.Join(root, "outside")
	for _, directory := range []string{source, target, outside} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(source, "abcabc146 E01.mkv")
	if err := os.WriteFile(input, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "Season 01")); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Source: source, Target: target, Extensions: []string{".mkv"}}
	plan := Plan{Items: []PlanItem{{Source: input, Destination: filepath.Join(target, "Season 01", "abcabc146 E01.mkv"), Action: ActionLink}}}
	if _, err := ApplyPlan(context.Background(), cfg, plan); err == nil {
		t.Fatal("expected symlinked destination directory to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "abcabc146 E01.mkv")); !os.IsNotExist(err) {
		t.Fatalf("link escaped target: %v", err)
	}
}

func TestBuildPlanForFilesRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "target")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(outside, "abcabc146 E01.mkv")
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "redirect")); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, source, library, false)
	plan, err := BuildPlanForFiles(context.Background(), cfg, []string{filepath.Join(source, "redirect", "abcabc146 E01.mkv")})
	if err == nil || len(plan.Items) != 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestFullAndExplicitFilePlanningShareDecisionCore(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(source, "[grpabc165][abcabc146] [02][1080p].mkv")
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "ignored.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, source, library, false)
	full, fullErr := BuildPlan(context.Background(), cfg)
	explicit, explicitErr := BuildPlanForFiles(context.Background(), cfg, []string{video})
	if fullErr != nil || explicitErr != nil {
		t.Fatalf("fullErr=%v explicitErr=%v", fullErr, explicitErr)
	}
	if len(full.Items) != 1 || len(explicit.Items) != 1 || !reflect.DeepEqual(full.Items[0], explicit.Items[0]) {
		t.Fatalf("full=%+v explicit=%+v", full, explicit)
	}
}

func TestExplicitFilePlanningUsesBatchEpisodeDifferences(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, name := range []string{"abcabc033 01 WEB 1080p.mkv", "abcabc033 02 WEB 1080p.mkv"} {
		path := filepath.Join(source, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	cfg, err := WithEmbyLayout(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, EmbyLayout{LibraryRoot: target, Title: "abcabc033", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlanForFiles(context.Background(), cfg, paths)
	if err != nil || len(plan.Items) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	for i, item := range plan.Items {
		want := fmt.Sprintf("abcabc033 S01E%02d.mkv", i+1)
		if filepath.Base(item.Destination) != want || item.Action != ActionLink {
			t.Fatalf("item[%d]=%+v want %s", i, item, want)
		}
	}
}

func TestFullPlanningUsesBatchEpisodeDifferences(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"abcabc033 01 WEB 1080p.mkv", "abcabc033 02 WEB 1080p.mkv"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := WithEmbyLayout(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, EmbyLayout{LibraryRoot: target, Title: "abcabc033", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil || len(plan.Items) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	for i, item := range plan.Items {
		want := fmt.Sprintf("abcabc033 S01E%02d.mkv", i+1)
		if filepath.Base(item.Destination) != want || item.Action != ActionLink {
			t.Fatalf("item[%d]=%+v want %s", i, item, want)
		}
	}
}

func TestMediaNamespaceRejectsRootsThatOnlyMeetAtFilesystemRoot(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm unavailable")
	}
	sourceRoot := t.TempDir()
	targetRoot, err := os.MkdirTemp("/dev/shm", "aninode-target-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(targetRoot)
	if sameDeviceForTest(sourceRoot, targetRoot) {
		t.Skip("test roots are on same filesystem")
	}
	err = ValidateConfig(Config{Source: sourceRoot, Target: targetRoot, Extensions: []string{".mkv"}})
	if err == nil || !strings.Contains(err.Error(), "dedicated media namespace") {
		t.Fatalf("error=%v", err)
	}
}

func TestManagedCorrectionPreservesOldTargetAndAddsCanonicalLink(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	libraryRoot := filepath.Join(root, "library")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceRoot, "abcabc146 E01.mkv")
	if err := os.WriteFile(source, []byte("video-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, sourceRoot, libraryRoot, true)
	old := filepath.Join(cfg.Target, "Season 01", "Wrong S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(source)
	beforeData, _ := os.ReadFile(source)
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked != 1 {
		t.Fatalf("result=%+v", res)
	}
	correct := filepath.Join(cfg.Target, "Season 01", "abcabc146 S01E01.mkv")
	after, _ := os.Stat(source)
	target, err := os.Stat(correct)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !os.SameFile(after, target) {
		t.Fatal("source/target inode identity changed")
	}
	afterData, _ := os.ReadFile(source)
	if string(beforeData) != string(afterData) {
		t.Fatal("source content changed")
	}
	oldInfo, err := os.Stat(old)
	if err != nil {
		t.Fatalf("old target was removed: %v", err)
	}
	if !os.SameFile(after, oldInfo) {
		t.Fatal("old target no longer refers to the source inode")
	}
}

func TestValidateRejectsOverlappingRoots(t *testing.T) {
	root := t.TempDir()
	for _, cfg := range []Config{
		{Source: filepath.Join(root, "downloads"), Target: filepath.Join(root, "downloads", "library")},
		{Source: filepath.Join(root, "library", "downloads"), Target: filepath.Join(root, "library")},
	} {
		if err := ValidateConfig(cfg); err == nil {
			t.Fatalf("ValidateConfig accepted overlapping roots: %+v", cfg)
		}
	}
}

func sameDeviceForTest(a, b string) bool {
	ai, _ := os.Stat(a)
	bi, _ := os.Stat(b)
	as, _ := ai.Sys().(*syscall.Stat_t)
	bs, _ := bi.Sys().(*syscall.Stat_t)
	return as != nil && bs != nil && as.Dev == bs.Dev
}

func TestApplyPlanRejectsSourceObjectReplacementAfterPlanning(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(source, "abcabc146 E01.mkv")
	if err := os.WriteFile(input, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, source, library, false)
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].Action != ActionLink {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	replacement := filepath.Join(source, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, input); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err == nil || result.Conflicts != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(library, "Season 01", "abcabc146 S01E01.mkv")); !os.IsNotExist(err) {
		t.Fatalf("replacement was published: %v", err)
	}
}

func TestSeriesGroupingDirectoriesDoNotCreateMediaIdentityAndAreFlattened(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "library")
	group := filepath.Join(source, "Season 99 misleading", "Episode 88 misleading")
	if err := os.MkdirAll(group, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(group, "abcabc033 S02E03.mkv")
	sub := filepath.Join(group, "abcabc033 S02E03.ass")
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("subtitle"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := WithEmbyLayout(Config{Source: source, Target: target, Extensions: []string{".mkv", ".ass"}}, EmbyLayout{LibraryRoot: target, Title: "abcabc033", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	for _, item := range plan.Items {
		if filepath.Base(filepath.Dir(item.Destination)) != "Season 02" {
			t.Fatalf("directory topology leaked into target: %+v", item)
		}
		if strings.Contains(item.Destination, "Season 99") || strings.Contains(item.Destination, "Episode 88") {
			t.Fatalf("grouping directory leaked into target: %+v", item)
		}
	}
}

func TestApplyPlanTreatsPlannedPublicationConflictAsObservableState(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := seriesConfig(t, source, target, false)
	plan := Plan{Items: []PlanItem{{
		Source: filepath.Join(source, "abcabc146 E08.mkv"), Destination: filepath.Join(target, "Season 01", "abcabc146 S01E08.mkv"),
		Action: ActionConflict, Detail: "destination already names a different filesystem object",
	}}}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil {
		t.Fatalf("planned conflict must not become an execution error: %v", err)
	}
	if result.Conflicts != 1 || result.Scanned != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestPublicationPreservesFileExtensionAndExplicitSeasonZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		season int
	}{
		{"01.m2ts", 2},
		{"[grpabc165][abcabc146] - 01.m2ts", 2},
		{"[grpabc165][abcabc146 S00] - 01.m2ts", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
			if err := os.MkdirAll(source, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(source, tc.name)
			if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := WithEmbyLayout(Config{Source: source, Target: target, Extensions: []string{".m2ts"}}, EmbyLayout{LibraryRoot: target, Title: "abcabc146", Season: 2})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := BuildPlan(context.Background(), cfg)
			if err != nil || len(plan.Items) != 1 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			want := filepath.Join(target, "abcabc146", fmt.Sprintf("Season %02d", tc.season), fmt.Sprintf("abcabc146 S%02dE01.m2ts", tc.season))
			if plan.Items[0].Action != ActionLink || plan.Items[0].Destination != want {
				t.Fatalf("item=%+v want=%s", plan.Items[0], want)
			}
			if _, err := ApplyPlan(context.Background(), cfg, plan); err != nil {
				t.Fatal(err)
			}
			sourceInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			targetInfo, err := os.Stat(want)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(sourceInfo, targetInfo) {
				t.Fatal("published media is not a hardlink to the source")
			}
		})
	}
}
