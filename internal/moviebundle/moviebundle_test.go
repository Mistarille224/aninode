package moviebundle

import (
	"os"
	"path/filepath"
	"testing"
)

func files(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		p := filepath.Join(root, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(n), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFileBundleAssociationsVersionsExtrasAndUnknown(t *testing.T) {
	root := files(t, "abcabc147.1080p.mkv", "abcabc147.1080p.zh-CN.ass", "abcabc147.1080p.en.forced.srt", "abcabc147.1080p.mka", "abcabc147.2160p.mkv", "trailer.mkv", "sample.mkv")
	b, err := Observe(root, Options{Title: "abcabc147", Year: 2024, Blacklist: []string{"*sample*"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != Files || len(b.Versions) != 2 || len(b.Versions[0].Subtitles) != 2 || len(b.Versions[0].Audios) != 1 || len(b.Extras) != 1 || len(b.Excluded) != 1 || b.Conflict != "" {
		t.Fatalf("bundle=%+v", b)
	}
}

func TestAmbiguousAudioAndSecondaryVideoConflict(t *testing.T) {
	root := files(t, "abcabc147.mkv", "Japanese.mka", "bonus01.mkv")
	b, err := Observe(root, Options{Title: "abcabc147"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Conflict == "" || len(b.Unknown) < 2 {
		t.Fatalf("bundle=%+v", b)
	}
}

func TestOpaqueDiscAndMixedKinds(t *testing.T) {
	root := files(t, "BDMV/index.bdmv", "BDMV/STREAM/00001.m2ts")
	b, err := Observe(root, Options{Title: "abcabc147"})
	if err != nil || b.Kind != BluRay || len(b.OpaqueTree.Assets) != 2 {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	mixed := files(t, "BDMV/index.bdmv", "abcabc147.mkv")
	b, err = Observe(mixed, Options{Title: "abcabc147"})
	if err != nil || b.Kind != Conflict {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	dvd := files(t, "VIDEO_TS/VIDEO_TS.IFO", "VIDEO_TS/VTS_01_1.VOB")
	b, err = Observe(dvd, Options{Title: "abcabc147"})
	if err != nil || b.Kind != DVD {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
}

func TestISOAndOverride(t *testing.T) {
	root := files(t, "abcabc147.bluray.iso")
	b, err := Observe(root, Options{Title: "abcabc147"})
	if err != nil || b.Kind != ISO {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	root = files(t, "abcabc147.mkv", "Extras/bonus01.mkv")
	b, err = Observe(root, Options{Title: "abcabc147", Overrides: map[string]string{"Extras/bonus01.mkv": "extra"}})
	if err != nil || len(b.Extras) != 1 || b.Conflict != "" {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
}

func TestMultipleAudioSuffixesAndVersionAssociation(t *testing.T) {
	root := files(t, "abcabc147.1080p.mkv", "abcabc147.1080p.ja.mka", "abcabc147.2160p.mkv", "abcabc147.2160p.en.mka", "abcabc147.2160p.commentary.mka")
	b, err := Observe(root, Options{Title: "abcabc147", Year: 2024})
	if err != nil || b.Conflict != "" {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	want := map[string]bool{"abcabc147 (2024) - 1080p.ja.mka": true, "abcabc147 (2024) - 2160p.en.mka": true, "abcabc147 (2024) - 2160p.commentary.mka": true}
	for _, v := range b.Versions {
		for _, a := range v.Audios {
			delete(want, TargetName("abcabc147", 2024, v, a))
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing targets=%v bundle=%+v", want, b)
	}
	ambiguous := files(t, "abcabc147.1080p.mkv", "abcabc147.2160p.mkv", "Japanese.mka")
	b, err = Observe(ambiguous, Options{Title: "abcabc147"})
	if err != nil || b.Conflict == "" || len(b.Unknown) != 1 {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
}

func TestExtrasAreTitleAndTokenAware(t *testing.T) {
	for _, tc := range []struct{ title, file string }{{"The Interview", "The.Interview.2014.mkv"}, {"abcabc001", "abcabc120.mkv"}, {"Interviewing Monsters", "Interviewing.Monsters.mkv"}} {
		b, err := Observe(files(t, tc.file), Options{Title: tc.title, Year: 2014})
		if err != nil || len(b.Versions) != 1 || len(b.Extras) != 0 {
			t.Fatalf("%s bundle=%+v err=%v", tc.title, b, err)
		}
	}
	b, err := Observe(files(t, "abcabc147.mkv", "abcabc147.Interview.With.Director.mkv"), Options{Title: "abcabc147"})
	if err != nil || len(b.Extras) != 1 || b.Extras[0].Kind != Interview {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	b, err = Observe(files(t, "abcabc147.mkv", "trailers/trailer.mkv"), Options{Title: "abcabc147"})
	if err != nil || len(b.Extras) != 1 || b.Extras[0].Kind != Trailer {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
}

func TestCompositeVersionLabelsAreStableAndDistinct(t *testing.T) {
	wants := map[string]string{"abcabc147.1080p.mkv": "1080p", "abcabc147.2160p.mkv": "2160p", "abcabc147.2160p.Directors.Cut.mkv": "2160p Directors Cut", "abcabc147.2160p.Theatrical.mkv": "2160p Theatrical", "abcabc147.3D.HSBS.mkv": "3D HSBS", "abcabc147.2160p.3D.HSBS.IMAX.Remastered.mkv": "2160p 3D HSBS IMAX Remastered"}
	for name, want := range wants {
		if got := versionLabel(name); got != want {
			t.Errorf("%s label=%q want=%q", name, got, want)
		}
	}
	b, err := Observe(files(t, "abcabc147.2160p.Theatrical.mkv", "abcabc147.2160p.Directors.Cut.mkv"), Options{Title: "abcabc147", Year: 2024})
	if err != nil || b.Conflict != "" || b.Versions[0].Label == b.Versions[1].Label {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
}

func TestRecognitionDistinguishesNonMovieAndConflict(t *testing.T) {
	b, err := Observe(files(t, "poster.jpg", "notes.txt"), Options{Title: "abcabc135", Year: 2024})
	if err != nil || b.Recognized {
		t.Fatalf("non-movie=%+v err=%v", b, err)
	}
	b, err = Observe(files(t, "abcabc147.mkv", "BDMV/index.bdmv"), Options{Title: "abcabc147"})
	if err != nil || !b.Recognized || b.Kind != Conflict {
		t.Fatalf("mixed=%+v err=%v", b, err)
	}
	b, err = Observe(files(t, "abcabc147.mkv", "bonus01.mkv"), Options{Title: "abcabc147"})
	if err != nil || !b.Recognized || b.Conflict == "" {
		t.Fatalf("unknown secondary=%+v err=%v", b, err)
	}
}

func TestBlacklistPrecedesOverrideAndStaleOverrideWarns(t *testing.T) {
	b, err := Observe(files(t, "abcabc147.mkv"), Options{Title: "abcabc147", Blacklist: []string{"abcabc147.mkv"}, Overrides: map[string]string{"abcabc147.mkv": "version", "missing.mkv": "extra"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Excluded) != 1 || len(b.Versions) != 0 || len(b.Warnings) != 1 {
		t.Fatalf("bundle=%+v", b)
	}
}

func TestAnimeSupplementFoldersAreRecognizedAsExtras(t *testing.T) {
	b, err := Observe(files(t, "abcabc147.mkv", "SPs/NCOP.mkv", "SPs/PV01.mkv", "Specials/bonus.mkv"), Options{Title: "abcabc147", Year: 2024})
	if err != nil || b.Conflict != "" || len(b.Versions) != 1 || len(b.Extras) != 3 {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	for _, extra := range b.Extras {
		if extra.Kind != OtherExtra {
			t.Fatalf("unexpected extra kind: %+v", extra)
		}
	}
}

func TestUnknownSecondaryVideoDetailNamesAsset(t *testing.T) {
	b, err := Observe(files(t, "abcabc147.mkv", "bonus01.mkv"), Options{Title: "abcabc147"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, unknown := range b.Unknown {
		if unknown.Relative == "bonus01.mkv" && unknown.Detail == `secondary video "bonus01.mkv" has no reliable version or extra marker` {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown=%+v", b.Unknown)
	}
}
