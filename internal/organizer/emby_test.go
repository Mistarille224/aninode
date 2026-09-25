package organizer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithEmbyLayoutBuildsCanonicalTreeAndRule(t *testing.T) {
	cfg, err := WithEmbyLayout(Config{}, EmbyLayout{LibraryRoot: "/library", Title: `abcabc: defdef`, Year: 2026, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/library", "abcabc defdef (2026)")
	if cfg.Target != want {
		t.Fatalf("target=%q want=%q", cfg.Target, want)
	}
	got, ok, err := resolveEmby("ghighi E01 - v2.mkv", cfg.emby)
	if err != nil {
		t.Fatal(err)
	}
	wantFile := filepath.Join("Season 01", "abcabc defdef S01E01 - v2.mkv")
	if !ok || got != wantFile {
		t.Fatalf("rule=%q matched=%t", got, ok)
	}
}

func TestMovieLayoutPublishesVersionAndRecognizedExtra(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "downloads", "abcabc147 (2026)"), filepath.Join(root, "library", "abcabc147 (2026)")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(source, "abcabc147.2026.1080p.mkv")
	extra := filepath.Join(source, "abcabc147.2026.Interview.mkv")
	if err := os.WriteFile(main, []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := WithMovieLayout(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, MovieLayout{Title: "abcabc147", Year: 2026})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 || plan.Items[0].Source != main || plan.Items[0].Action != ActionLink || plan.Items[1].Action != ActionLink {
		t.Fatalf("plan=%+v", plan)
	}
	if _, err := ApplyPlan(context.Background(), cfg, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "abcabc147 (2026) - 1080p.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "interviews", filepath.Base(extra))); err != nil {
		t.Fatalf("recognized extra not linked: %v", err)
	}
}

func TestMovieOpaqueBluRayPreservesTreeAsHardlinks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	paths := []string{"BDMV/index.bdmv", "BDMV/PLAYLIST/00001.mpls", "BDMV/STREAM/00001.m2ts"}
	for _, rel := range paths {
		p := filepath.Join(source, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := WithMovieLayout(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, MovieLayout{Title: "abcabc147", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil || len(plan.Items) != len(paths) {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil || result.Linked != len(paths) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, rel := range paths {
		if !sameFile(filepath.Join(source, rel), filepath.Join(target, rel)) {
			t.Fatalf("not a preserved hardlink: %s", rel)
		}
	}
}

func TestMovieSidecarsMultipleAudioAreDistinctHardlinks(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	names := []string{"abcabc147.2160p.mkv", "abcabc147.2160p.zh-CN.ass", "abcabc147.2160p.ja.mka", "abcabc147.2160p.en.mka", "abcabc147.2160p.commentary.mka"}
	for _, name := range names {
		if err := os.MkdirAll(source, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := WithMovieLayout(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, MovieLayout{Title: "abcabc147", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range plan.Items {
		if item.Action != ActionLink {
			t.Fatalf("item=%+v", item)
		}
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil || result.Linked != len(names) {
		t.Fatalf("result=%+v err=%v plan=%+v", result, err, plan)
	}
	wants := map[string]string{"abcabc147.2160p.mkv": "abcabc147 (2024) - 2160p.mkv", "abcabc147.2160p.zh-CN.ass": "abcabc147 (2024) - 2160p.zh-CN.ass", "abcabc147.2160p.ja.mka": "abcabc147 (2024) - 2160p.ja.mka", "abcabc147.2160p.en.mka": "abcabc147 (2024) - 2160p.en.mka", "abcabc147.2160p.commentary.mka": "abcabc147 (2024) - 2160p.commentary.mka"}
	for sourceName, targetName := range wants {
		if !sameFile(filepath.Join(source, sourceName), filepath.Join(target, targetName)) {
			t.Fatalf("not a hardlink: %s -> %s", sourceName, targetName)
		}
	}
}

func TestMovieOpaqueDVDPreservesTreeAsHardlinks(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	names := []string{"VIDEO_TS/VIDEO_TS.IFO", "VIDEO_TS/VIDEO_TS.BUP", "VIDEO_TS/VTS_01_1.VOB"}
	for _, name := range names {
		p := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := WithMovieLayout(Config{Source: source, Target: target, Extensions: []string{".mkv"}}, MovieLayout{Title: "abcabc147", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyPlan(context.Background(), cfg, plan)
	if err != nil || result.Linked != len(names) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, name := range names {
		if !sameFile(filepath.Join(source, name), filepath.Join(target, name)) {
			t.Fatalf("not a preserved hardlink: %s", name)
		}
	}
}

func TestMovieCompositeVersionTargetsDoNotCollide(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	for _, name := range []string{"abcabc147.2160p.Theatrical.mkv", "abcabc147.2160p.Directors.Cut.mkv"} {
		if err := os.MkdirAll(source, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := WithMovieLayout(Config{Source: source, Target: target}, MovieLayout{Title: "abcabc147", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 || plan.Items[0].Destination == plan.Items[1].Destination || plan.Items[0].Action != ActionLink || plan.Items[1].Action != ActionLink {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestMovieTargetCollisionIsAPlanningConflictForEverySource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	for _, name := range []string{"abcabc147.2160p.WEB.mkv", "abcabc147.2160p.Remux.mkv"} {
		if err := os.MkdirAll(source, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := WithMovieLayout(Config{Source: source, Target: filepath.Join(root, "target")}, MovieLayout{Title: "abcabc147", Year: 2024})
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
		if item.Action != ActionConflict || !strings.Contains(item.Detail, "map to the same target") {
			t.Fatalf("item=%+v", item)
		}
	}
}

func TestEmbyExplicitSeasonOverridesFolder(t *testing.T) {
	cfg, err := WithEmbyLayout(Config{}, EmbyLayout{LibraryRoot: "/library", Title: "abcabc146", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := resolveEmby("abcabc146 S02E03.mkv", cfg.emby)
	if err != nil || got != filepath.Join("Season 02", "abcabc146 S02E03.mkv") {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestEmbySpecialMappingResolvesNumberlessFile(t *testing.T) {
	cfg, err := WithEmbyLayout(Config{}, EmbyLayout{LibraryRoot: "/library", Title: "abcabc146", Season: 1, Specials: []SpecialMapping{{Source: "abcabc146 OVA.mkv", Season: 0, Episode: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	got, matched, err := resolveEmby("abcabc146 OVA.mkv", cfg.emby)
	want := filepath.Join("Season 00", "abcabc146 S00E03.mkv")
	if err != nil || !matched || got != want {
		t.Fatalf("got=%q matched=%t err=%v", got, matched, err)
	}
}

func TestEmbySpecialMappingRequiresBasename(t *testing.T) {
	_, err := WithEmbyLayout(Config{}, EmbyLayout{LibraryRoot: "/library", Title: "abcabc146", Season: 1, Specials: []SpecialMapping{{Source: "../abcabc146.mkv", Season: 0, Episode: 1}}})
	if err == nil {
		t.Fatal("expected unsafe special source error")
	}
}

func TestEmbyPlanningUsesParentForBareEpisodeFilename(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "downloads")
	video := filepath.Join(source, "abcabc041", "03.mkv")
	if err := os.MkdirAll(filepath.Dir(video), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := WithEmbyLayout(Config{Source: source, Extensions: []string{".mkv"}}, EmbyLayout{LibraryRoot: filepath.Join(root, "library"), Title: "abcabc019", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlanForFiles(context.Background(), cfg, []string{video})
	if err != nil || len(plan.Items) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	want := filepath.Join(cfg.Target, "Season 01", "abcabc019 S01E03.mkv")
	if plan.Items[0].Destination != want || len(plan.Items[0].Parents) == 0 || plan.Items[0].Parents[0] != "abcabc041" {
		t.Fatalf("item=%+v want=%q", plan.Items[0], want)
	}
}
