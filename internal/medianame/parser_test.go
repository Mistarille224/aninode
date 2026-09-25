package medianame

import "testing"

func TestParseMediaNames(t *testing.T) {
	tests := []struct {
		name, input, title                string
		year, season, start, end, version int
	}{
		{"case01", "abcabc.defdef.S05E14.ghighi.1080p.BluRay.x264.mkv", "abcabc defdef", 0, 5, 14, 14, 1},
		{"case02", "abcabc.defdef.ghighi.S06E13.1080p.WEB-DL.mkv", "abcabc defdef ghighi", 0, 6, 13, 13, 1},
		{"case03", "abcabc.2024.S01E03.1080p.WEB-DL.mkv", "abcabc", 2024, 1, 3, 3, 1},
		{"case04", "abcabc.defdef.ghighi.jkljkl.S01E03.mkv", "abcabc defdef ghighi jkljkl", 0, 1, 3, 3, 1},
		{"case05", "abcabc.100.S01E01.mkv", "abcabc 100", 0, 1, 1, 1, 1},
		{"case06", "abcabc.104.S01E01.mkv", "abcabc 104", 0, 1, 1, 1, 1},
		{"case07", "abcabc.19.S07E01.mkv", "abcabc 19", 0, 7, 1, 1, 1},
		{"case08", "7-2-4.S07E01.mkv", "7-2-4", 0, 7, 1, 1, 1},
		{"case09", "7-2-4.Lone.Star.S05E01.mkv", "7-2-4 Lone Star", 0, 5, 1, 1, 1},
		{"case10", "1923.S01E01.mkv", "1923", 0, 1, 1, 1, 1},
		{"case11", "1899.S01E01.mkv", "1899", 0, 1, 1, 1, 1},
		{"case12", "24.S01E01.mkv", "24", 0, 1, 1, 1, 1},
		{"case13", "3.abcabc.defdef.S01E01.mkv", "3 abcabc defdef", 0, 1, 1, 1, 1},
		{"case14", "abcabc.4400.S01E01.mkv", "abcabc 4400", 0, 1, 1, 1, 1},
		{"case15", "[grpabc166] abcabc defdef ghighi - 29 [1080P].mp4", "abcabc defdef ghighi", 0, 0, 29, 29, 1},
		{"brackets", "[grpabc165][abcabc142][12][1080p].mkv", "abcabc142", 0, 0, 12, 12, 1},
		{"brackets v2", "[grpabc165][abcabc142][12v2][1080p].mkv", "abcabc142", 0, 0, 12, 12, 2},
		{"bracket range", "[grpabc165][abcabc142][01-02][720p].mkv", "abcabc142", 0, 0, 1, 2, 1},
		{"case20", "[grpabc201] ABCABC DEFDEF - 1138 (metaabc 1920x1080 HEVC AAC MKV)", "ABCABC DEFDEF", 0, 0, 1138, 1138, 1},
		{"case21", "86 - abcabc defdef - 01.mkv", "86 - abcabc defdef", 0, 0, 1, 1, 1},
		{"case22", "[grpabc165] 86 - abcabc defdef - 01 [1080p].mkv", "86 - abcabc defdef", 0, 0, 1, 1, 1},
		{"case23", "AbcD Efghij Ver1.1a - 01.mkv", "AbcD Efghij Ver1.1a", 0, 0, 1, 1, 1},
		{"case24", "A4BC abcabc defdef - 01.mkv", "A4BC abcabc defdef", 0, 0, 1, 1, 1},
		{"1x02", "abcabc.1x02.mkv", "abcabc", 0, 1, 2, 2, 1},
		{"season episode", "abcabc142 Season 1 Episode 2.mkv", "abcabc142", 0, 1, 2, 2, 1},
		{"ep", "abcabc142 EP12.mkv", "abcabc142", 0, 0, 12, 12, 1},
		{"ordinal season", "abcabc-defdef 2nd Season - 04.mkv", "abcabc-defdef", 0, 2, 4, 4, 1},
		{"season suffix", "abcabc145 Season 3 - 07.mkv", "abcabc145", 0, 3, 7, 7, 1},
		{"opaque release metadata", "[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 08v2 [1080P][metaabc][WEB-DL][AAC AVC][SRT].mp4", "Ab: 甲乙丙丁戊己 庚辛", 0, 0, 8, 8, 2},
		{"weak episode parenthesized year", "[grpabc166] abcabc068 (2026) - 01 [1080P].mp4", "abcabc068", 2026, 0, 1, 1, 1},
		{"weak episode season then year", "abcabc033 S2 (2026) - 03.mkv", "abcabc033", 2026, 2, 3, 3, 1},
		{"weak episode year then season", "abcabc033 (2026) S2 - 03.mkv", "abcabc033", 2026, 2, 3, 3, 1},
		{"season zero suffix", "abcabc033 S00 - 03.mkv", "abcabc033", 0, 0, 3, 3, 1},
		{"fully bracketed title year", "[grpabc165][abcabc033 (2026)][12][1080p].mkv", "abcabc033", 2026, 0, 12, 12, 1},
		{"fully bracketed separate year", "[grpabc165][abcabc033][2026][12][1080p].mkv", "abcabc033", 2026, 0, 12, 12, 1},
		{"fully bracketed separate season", "[grpabc165][abcabc033][S2][03][1080p].mkv", "abcabc033", 0, 2, 3, 3, 1},
		{"fully bracketed explicit episode", "[grpabc165][abcabc033][E03][1080p].mkv", "abcabc033", 0, 0, 3, 3, 1},
		{"trailing bracket year", "[grpabc165] abcabc033 - 01 [2026][1080p].mkv", "abcabc033", 2026, 0, 1, 1, 1},
		{"trailing parenthesized year", "abcabc033 - 01 (2026).mkv", "abcabc033", 2026, 0, 1, 1, 1},
		{"mixed bracket title weak episode", "[grpabc165][abcabc033] - 01 [1080p].mkv", "abcabc033", 0, 0, 1, 1, 1},
		{"mixed bracket title year weak episode", "[grpabc165][abcabc033][2026] - 01 [1080p].mkv", "abcabc033", 2026, 0, 1, 1, 1},
		{"mixed bracket title strong episode", "[grpabc165][abcabc033] S02E03 [1080p].mkv", "abcabc033", 0, 2, 3, 3, 1},
		{"movie parenthesized year", "abcabc026 (2026).mkv", "abcabc026", 2026, 0, 0, 0, 0},
		{"ambiguous scene year stays title", "abcabc.defdef.2026.mkv", "abcabc defdef 2026", 0, 0, 0, 0, 0},
		{"numeric scene title is preserved", "abcabc.defdef.2049.mkv", "abcabc defdef 2049", 0, 0, 0, 0, 0},
		{"numeric title is preserved", "abcabc defdef 2049.mkv", "abcabc defdef 2049", 0, 0, 0, 0, 0},
		{"numeric episodic title is preserved", "abcabc defdef 2049 - 01.mkv", "abcabc defdef 2049", 0, 0, 1, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.input)
			if got.Title != tt.title || got.Year != tt.year || got.Season != tt.season || got.EpisodeStart != tt.start || got.EpisodeEnd != tt.end || got.Version != tt.version {
				t.Fatalf("Parse(%q)=%+v", tt.input, got)
			}
		})
	}
}

func TestNumericTitlesAreNotEpisodes(t *testing.T) {
	tests := map[string]string{
		"24": "24", "1923": "1923", "1899": "1899", "86": "86",
		"86 abcabc-defdef": "86 abcabc-defdef", "86 - abcabc defdef": "86 - abcabc defdef",
		"abcabc 100": "abcabc 100", "3 abcabc defdef": "3 abcabc defdef", "abcabc 104": "abcabc 104",
		"abcabc 19": "abcabc 19", "abcabc-defdef": "abcabc-defdef",
		"7-2-4": "7-2-4", "7-2-4: Lone Star": "7-2-4: Lone Star",
		"Abcabc-22": "Abcabc-22", "44.55.66": "44.55.66", "55.66.77": "55.66.77",
		"abcabc defdef '07": "abcabc defdef '07", "abcabc056": "abcabc056", "abcabc 4400": "abcabc 4400",
		"A4BC": "A4BC", "AB:CDEFGH": "AB:CDEFGH", "AbcD:efghij Ver1.1a": "AbcD:efghij Ver1.1a",
		"AB:CDE": "AB:CDE", "Abc/Def Ghi": "Abc/Def Ghi",
	}
	for in, want := range tests {
		got := Parse(in)
		if got.Title != want || got.EpisodeStart != 0 {
			t.Errorf("Parse(%q)=%+v want title %q no episode", in, got, want)
		}
	}
}

func TestStandaloneSeasonTitleCarriesSeriesIdentity(t *testing.T) {
	got := Parse("abcabc-defdef 2nd Season")
	if got.Title != "abcabc-defdef" || got.Season != 2 || got.EpisodeStart != 0 {
		t.Fatalf("season identity=%+v", got)
	}
}

func TestStructuralBracketsDoNotNeedMetadataVocabulary(t *testing.T) {
	tests := []struct {
		input string
		want  Parsed
	}{
		{"[Unknown grpabc165] abcabc033 [12v2][abcabc129].mkv", Parsed{Title: "abcabc033", EpisodeStart: 12, EpisodeEnd: 12, Version: 2}},
		{"[Unknown grpabc165][abcabc026][abcabc129].mkv", Parsed{Title: "abcabc026"}},
		{"abcabc033 - 03 (abcabc129).mkv", Parsed{Title: "abcabc033", EpisodeStart: 3, EpisodeEnd: 3, Version: 1}},
	}
	for _, tt := range tests {
		if got := Parse(tt.input); got != tt.want {
			t.Errorf("Parse(%q)=%+v want %+v", tt.input, got, tt.want)
		}
	}
}

func TestFullyBracketedStandaloneSeasonCarriesSeriesIdentity(t *testing.T) {
	got := Parse("[grpabc150][abcabc067 S4][1080p][CHS]")
	if got.Title != "abcabc067" || got.Season != 4 || got.EpisodeStart != 0 {
		t.Fatalf("fully bracketed season identity=%+v", got)
	}
}

func TestFullyBracketedMovieYearRemainsMovieIdentity(t *testing.T) {
	got := Parse("[grpabc165][abcabc026 (2026)][1080p]")
	if got.Title != "abcabc026" || got.Year != 2026 || got.Season != 0 || got.EpisodeStart != 0 {
		t.Fatalf("fully bracketed movie identity=%+v", got)
	}
}

func TestInvalidEpisodeRangesAreNotAccepted(t *testing.T) {
	for _, input := range []string{"abcabc145 - 03-01.mkv", "[grpabc165][abcabc145][03-01][1080p].mkv", "abcabc145 S01E03-E01.mkv"} {
		got := Parse(input)
		if got.EpisodeStart != 0 {
			t.Fatalf("Parse(%q) accepted invalid range: %+v", input, got)
		}
	}
}

func TestDottedEpisodeWithoutMediaExtensionKeepsEpisodeToken(t *testing.T) {
	got := AnalyzeMany([]string{"abcabc145.01", "abcabc145.02"})
	for i, item := range got.Items {
		if item.Parsed.Title != "abcabc145" || item.Parsed.EpisodeStart != i+1 || item.Components.Extension != "" {
			t.Fatalf("item[%d]=%+v", i, item)
		}
	}
}

func TestSeasonHintDoesNotOverwriteExplicitSeasonZero(t *testing.T) {
	analysis := AnalyzeMany([]string{"abcabc146 S00E03.mkv"})
	got, ok := EpisodeWithSeasonHint(analysis.Items[0], 2)
	if !ok || got.Season != 0 || got.EpisodeStart != 3 {
		t.Fatalf("explicit S00 was overwritten by folder hint: ok=%v parsed=%+v", ok, got)
	}
}

func TestWeakEpisodeWithExplicitSeasonZeroIsReliableSeriesEvidence(t *testing.T) {
	item := AnalyzeMany([]string{"abcabc146 S00 - 03.mkv"}).Items[0]
	if item.Parsed.Title != "abcabc146" || item.Parsed.Season != 0 || item.Parsed.EpisodeStart != 3 {
		t.Fatalf("parsed=%+v", item.Parsed)
	}
	if item.Components.EpisodeEvidence != EpisodeEvidenceExplicit || !ReliableEpisodeEvidence(item.Components) {
		t.Fatalf("components=%+v", item.Components)
	}
}

func TestFileParsingSupportsConfiguredExtensions(t *testing.T) {
	for _, extension := range []string{".m2ts", ".ass", ".custom", ".M2TS"} {
		for _, stem := range []string{"01", "abcabc145 - 01", "[grpabc165][abcabc145][01]", "abcabc145 S01E01"} {
			t.Run(stem+extension, func(t *testing.T) {
				got, ok := ParseEpisodeFile(stem+extension, 1)
				if !ok || got.Season != 1 || got.EpisodeStart != 1 || got.EpisodeEnd != 1 {
					t.Fatalf("parsed=%+v ok=%v", got, ok)
				}
			})
		}
	}
}

func TestSeasonHintPreservesMixedBracketSeasonZero(t *testing.T) {
	for _, input := range []string{
		"[grpabc165][abcabc146 S00] - 03 [1080p].mkv",
		"[grpabc165][abcabc146][S00] - 03.mkv",
		"[grpabc165][abcabc146] - 03 [S00].mkv",
		"[grpabc165][abcabc146 S00 (2026)] - 03.m2ts",
		"abcabc146 S00 (2026) - 03.mkv",
	} {
		t.Run(input, func(t *testing.T) {
			got, ok := ParseEpisodeFile(input, 2)
			if !ok || got.Title != "abcabc146" || got.Season != 0 || got.EpisodeStart != 3 {
				t.Fatalf("parsed=%+v ok=%v", got, ok)
			}
		})
	}
	got, ok := ParseEpisodeFile("[grpabc165][abcabc146] - 03.mkv", 2)
	if !ok || got.Season != 2 {
		t.Fatalf("missing season did not use hint: parsed=%+v ok=%v", got, ok)
	}
}

func TestMixedBracketEpisodePreservesTitleContinuation(t *testing.T) {
	for _, input := range []string{
		"[grpabc165][abcabc146] defdef - 03.mkv",
		"[grpabc165][abcabc146] defdef - 03 (2026).mkv",
		"[grpabc165][abcabc146] defdef - 03 [1080p].mkv",
	} {
		t.Run(input, func(t *testing.T) {
			got := Parse(input)
			if got.Title != "abcabc146 defdef" || got.EpisodeStart != 3 {
				t.Fatalf("title continuation lost: %+v", got)
			}
		})
	}
}

func TestParseDistinguishesExplicitSeasonZeroFromSeasonlessEpisode(t *testing.T) {
	explicit := Parse("abcabc146 S00E03.mkv")
	if explicit.Season != 0 || !explicit.SeasonExplicit || explicit.EpisodeStart != 3 {
		t.Fatalf("explicit S00 lost presence: %+v", explicit)
	}
	seasonless := Parse("abcabc146 E03.mkv")
	if seasonless.Season != 0 || seasonless.SeasonExplicit || seasonless.EpisodeStart != 3 {
		t.Fatalf("seasonless episode gained explicit season: %+v", seasonless)
	}
}
