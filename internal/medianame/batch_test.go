package medianame

import (
	"strings"
	"testing"
)

func TestParseManyFindsEpisodeAxisBeforeStableReleaseSuffix(t *testing.T) {
	names := []string{"abcabc033 01 WEB 1080p.mkv", "abcabc033 02 WEB 1080p.mkv", "abcabc033 03 WEB 1080p.mkv"}
	got := ParseMany(names)
	for i, parsed := range got {
		if parsed.Title != "abcabc033" || parsed.EpisodeStart != i+1 || parsed.EpisodeEnd != i+1 {
			t.Fatalf("batch[%d]=%+v", i, parsed)
		}
	}
}

func TestParseManyDoesNotTreatYearOrResolutionAsEpisode(t *testing.T) {
	got := ParseMany([]string{"abcabc148 2025 1080p.mkv", "abcabc148 2026 2160p.mkv"})
	if got[0].EpisodeStart != 0 || got[1].EpisodeStart != 0 {
		t.Fatalf("movies became episodes: %+v", got)
	}
}

func TestParseManyRanksEpisodeAboveChangingResolution(t *testing.T) {
	got := ParseMany([]string{"abcabc146 01 WEB 720p.mkv", "abcabc146 02 WEB 1080p.mkv", "abcabc146 03 WEB 2160p.mkv"})
	for i, parsed := range got {
		if parsed.Title != "abcabc146" || parsed.EpisodeStart != i+1 {
			t.Fatalf("ranked batch[%d]=%+v", i, parsed)
		}
	}
}

func TestParseManyRanksEpisodeAfterStableYear(t *testing.T) {
	got := ParseMany([]string{"abcabc146.2024.01.WEB.mkv", "abcabc146.2024.02.SRT.mkv", "abcabc146.2024.03.ASS.mkv"})
	for i, parsed := range got {
		if parsed.Title != "abcabc146" || parsed.Year != 2024 || parsed.EpisodeStart != i+1 {
			t.Fatalf("year batch[%d]=%+v", i, parsed)
		}
	}
}

func TestParseManyRecognizesIncompleteSeasonCohort(t *testing.T) {
	names := []string{
		"abcabc defdef ghighi - 05 [WebRip 1080p HEVC-10bit AAC SRTx2]",
		"abcabc defdef ghighi - 06 [WebRip 1080p HEVC-10bit AAC SRTx2]",
		"abcabc defdef ghighi - 08 [WebRip 1080p HEVC-10bit AAC SRTx2]",
	}
	got := ParseMany(names)
	want := []int{5, 6, 8}
	for i, parsed := range got {
		if parsed.Title != "abcabc defdef ghighi" || parsed.EpisodeStart != want[i] {
			t.Fatalf("batch[%d]=%+v", i, parsed)
		}
	}
}

func TestParseManyTreatsChangingReleaseMetadataAsLowerPriorityThanEpisode(t *testing.T) {
	names := []string{
		"[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 04 [1080P][metaabc][WEB-DL][AAC AVC][ASS].mp4",
		"[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 08v2 [1080P][metaabc][WEB-DL][AAC AVC][SRT].mp4",
		"[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 09 [1080P][metaabc][WEB-DL][AAC AVC][CHT].mp4",
	}
	want := []int{4, 8, 9}
	for i, parsed := range ParseMany(names) {
		if parsed.Title != "Ab: 甲乙丙丁戊己 庚辛" || parsed.EpisodeStart != want[i] {
			t.Fatalf("metadata variant[%d]=%+v", i, parsed)
		}
		if i == 1 && parsed.Version != 2 {
			t.Fatalf("version lost: %+v", parsed)
		}
	}
}

func TestAnalyzeManyExposesSemanticPartsAndTheirDifferences(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"[grpabc166] abcabc033 - 01 [1080P][ASS].mp4",
		"[grpabc166] abcabc033 - 02v2 [1080P][SRT].mp4",
	})
	if len(analysis.Items) != 2 || analysis.Items[0].Components.Title != "abcabc033" || analysis.Items[0].Components.ReleaseGroup != "grpabc166" {
		t.Fatalf("components=%+v", analysis.Items)
	}
	states := map[string]bool{}
	differences := analysis.DifferencesFor(0)
	for _, difference := range differences {
		states[difference.Field] = difference.Stable
	}
	if !states["title"] || !states["release_group"] || !states["extension"] || states["episode"] || states["version"] || states["metadata"] {
		t.Fatalf("differences=%+v", differences)
	}
}

func TestReliableEpisodeTitlesForMigrationAndLocalAdoption(t *testing.T) {
	batch := AnalyzeMany([]string{"[grpabc151][abcabc][001][1080P].mkv", "[grpabc151][abcabc][002][1080P].mkv", "[grpabc167][甲乙甲乙][003][1080P].mkv", "NCOP S01E01.mkv", "[grpabc165] abcabc NCED S01E02.mkv", "sample.mkv"})
	got := ReliableEpisodeTitles(batch)
	if len(got) != 2 || got[0] != "abcabc" || got[1] != "甲乙甲乙" {
		t.Fatalf("titles=%v", got)
	}
}

func TestAnalyzeManyClassifiesSharedTechnicalComponents(t *testing.T) {
	analysis := AnalyzeMany([]string{"[grpabc165] abcabc145 - 01 [1080P][CHS].mkv"})
	c := analysis.Items[0].Components
	if c.ReleaseGroup != "grpabc165" || c.Resolution != "1080p" || c.Subtitle != "zh-Hans" {
		t.Fatalf("technical components=%+v", c)
	}
}

func TestParseMovesLeadingRevisionOutOfTitle(t *testing.T) {
	p := Parse("abcabc033 v2 - 02 [1080P].mkv")
	if p.Title != "abcabc033" || p.EpisodeStart != 2 || p.Version != 2 {
		t.Fatalf("parsed=%+v", p)
	}
}

func TestAnalyzeManyNeverUsesChangingTitleAsAnEpisodeCohort(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"abcabc050 - 01 [1080P].mkv",
		"abcabc058 - 02 [1080P].mkv",
	})
	if len(analysis.Cohorts) != 2 {
		t.Fatalf("different titles merged: %+v", analysis.Cohorts)
	}
}

func TestAnalyzeManyDifferencesAreScopedPerTitleCohort(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"abcabc050 - 01 [ASS].mkv",
		"abcabc050 - 02 [SRT].mkv",
		"abcabc058 - 01 [ASS].mkv",
	})
	if len(analysis.Cohorts) != 2 {
		t.Fatalf("cohorts=%+v", analysis.Cohorts)
	}
	alpha := analysis.DifferencesFor(0)
	beta := analysis.DifferencesFor(2)
	if fieldStable(alpha, "episode") || fieldStable(alpha, "metadata") || !fieldStable(beta, "episode") || !fieldStable(beta, "metadata") {
		t.Fatalf("alpha=%+v beta=%+v", alpha, beta)
	}
}

func TestAnalyzeManyAllowsProvenanceAndMetadataToVaryWithinTitle(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"[grpabc160] abcabc033 01 [WEB][ASS].mkv",
		"[grpabc161] abcabc033 02 [BDRip][SRT].mp4",
		"[grpabc162] abcabc033 03v2 [HEVC].mkv",
	})
	if len(analysis.Cohorts) != 1 {
		t.Fatalf("metadata split cohort: %+v", analysis.Cohorts)
	}
	for i, item := range analysis.Items {
		if item.Parsed.Title != "abcabc033" || item.Parsed.EpisodeStart != i+1 {
			t.Fatalf("item[%d]=%+v", i, item)
		}
	}
	if analysis.Items[2].Parsed.Version != 2 {
		t.Fatalf("differential version=%+v", analysis.Items[2].Parsed)
	}
	differences := analysis.DifferencesFor(0)
	if !fieldStable(differences, "title") || fieldStable(differences, "release_group") || fieldStable(differences, "metadata") || fieldStable(differences, "extension") {
		t.Fatalf("differences=%+v", differences)
	}
	for _, difference := range differences {
		if difference.Field == "title" && (difference.Role != "identity" || difference.Expectation != "invariant") {
			t.Fatalf("title priority=%+v", difference)
		}
		if difference.Field == "episode" && (difference.Role != "sequence" || difference.Expectation != "expected_varying") {
			t.Fatalf("episode priority=%+v", difference)
		}
	}
}

func TestAnalyzeManyUpgradesParsedWeakEpisodesOnlyWithSiblingVariation(t *testing.T) {
	single := AnalyzeMany([]string{"abcabc145 - 01 [1080P].mkv"})
	if single.Items[0].Components.EpisodeEvidence != "weak" {
		t.Fatalf("single evidence=%q", single.Items[0].Components.EpisodeEvidence)
	}
	series := AnalyzeMany([]string{"abcabc145 - 01 [ASS].mkv", "abcabc145 - 02v2 [SRT].mkv"})
	for i, item := range series.Items {
		if item.Components.EpisodeEvidence != "differential" {
			t.Fatalf("series[%d] evidence=%q", i, item.Components.EpisodeEvidence)
		}
	}
}

func fieldStable(values []Difference, field string) bool {
	for _, value := range values {
		if value.Field == field {
			return value.Stable
		}
	}
	return false
}

func TestParseManyFindsEpisodeAxisWithoutUnderstandingTitleLanguage(t *testing.T) {
	names := []string{
		"[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 01 [1080P][metaabc][WEB-DL][AAC AVC][CHT].mp4",
		"[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 02 [1080P][metaabc][WEB-DL][AAC AVC][CHT].mp4",
		"[grpabc166] Ab: 甲乙丙丁戊己 庚辛 - 03 [1080P][metaabc][WEB-DL][AAC AVC][CHT].mp4",
	}
	got := ParseMany(names)
	for i, parsed := range got {
		if parsed.EpisodeStart != i+1 || parsed.Season != 0 {
			t.Fatalf("batch[%d]=%+v", i, parsed)
		}
	}
}

func TestParseManyDoesNotInventEpisodeMeaningForRomanTitleVariants(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"abcabc111 [tagabc_816p]",
		"abcabc112 [tagabc_816p]",
		"abcabc113 [tagabc_816p]",
	})
	for i, item := range analysis.Items {
		parsed := item.Parsed
		if parsed.EpisodeStart != 0 || parsed.Season != 0 {
			t.Fatalf("title variant[%d]=%+v", i, parsed)
		}
		if parsed.Title == "" || strings.Contains(parsed.Title, "Ab10c") {
			t.Fatalf("metadata leaked into title[%d]=%+v", i, item)
		}
	}
}

func TestAnalyzeManySeparatesProviderMetadataAndSeasonQualifier(t *testing.T) {
	input := "Tenkousaki no Seiso Karen na Bishoujo ga Mukashi Danshi to Omotte Issho ni Asonda Osananajimi datta Ken S01 [CR WEB-DL 1080p AVC AAC][SC_TC]"
	item := AnalyzeMany([]string{input}).Items[0]
	if item.Components.Title != "Tenkousaki no Seiso Karen na Bishoujo ga Mukashi Danshi to Omotte Issho ni Asonda Osananajimi datta Ken" || item.Components.Season != 1 {
		t.Fatalf("identity=%+v", item.Components)
	}
	if len(item.Components.Metadata) != 2 || item.Components.Metadata[0] != "CR WEB-DL 1080p AVC AAC" || item.Components.Metadata[1] != "SC_TC" {
		t.Fatalf("metadata=%+v", item.Components.Metadata)
	}
}

func TestAnalyzeManyPreservesBareYearLikeTitleToken(t *testing.T) {
	got := AnalyzeMany([]string{"abcabc defdef 2049 01 WEB.mkv", "abcabc defdef 2049 02 SRT.mkv", "abcabc defdef 2049 03 ASS.mkv"})
	for i, item := range got.Items {
		if item.Parsed.Title != "abcabc defdef 2049" || item.Parsed.Year != 0 || item.Parsed.EpisodeStart != i+1 {
			t.Fatalf("item[%d]=%+v", i, item)
		}
	}
}

func TestAnalyzeManyDoesNotCrossYearBoundariesWhenUpgradingWeakEpisodes(t *testing.T) {
	got := AnalyzeMany([]string{"abcabc145 (2025) - 01.mkv", "abcabc145 (2026) - 02.mkv"})
	for i, item := range got.Items {
		if item.Components.EpisodeEvidence != EpisodeEvidenceWeak {
			t.Fatalf("item[%d] evidence=%q parsed=%+v", i, item.Components.EpisodeEvidence, item.Parsed)
		}
	}
}

func TestComponentsDoNotLeakBracketedIdentityIntoMetadata(t *testing.T) {
	item := AnalyzeMany([]string{"[grpabc165][abcabc033 (2026)][12][1080p].mkv"}).Items[0]
	if item.Components.Title != "abcabc033" || item.Components.Year != 2026 || item.Components.EpisodeEvidence != EpisodeEvidenceExplicit {
		t.Fatalf("identity=%+v", item.Components)
	}
	if len(item.Components.Metadata) != 1 || item.Components.Metadata[0] != "1080p" {
		t.Fatalf("metadata=%+v", item.Components.Metadata)
	}
}

func TestEpisodeEvidenceSeparatesPublicationFromWeakGuess(t *testing.T) {
	publication := AnalyzeMany([]string{"[grpabc166] abcabc145 - 03 [1080p].mkv"}).Items[0]
	if publication.Components.EpisodeEvidence != EpisodeEvidencePublication || !ReliableEpisodeEvidence(publication.Components) {
		t.Fatalf("publication=%+v", publication.Components)
	}
	weak := AnalyzeMany([]string{"abcabc145 - 03.mkv"}).Items[0]
	if weak.Components.EpisodeEvidence != EpisodeEvidenceWeak || ReliableEpisodeEvidence(weak.Components) {
		t.Fatalf("weak=%+v", weak.Components)
	}
}

func TestAnalyzeManyKeepsYearAndSeasonIdentityCohortsSeparate(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"abcabc145 (2025) - 01.mkv",
		"abcabc145 (2026) - 01.mkv",
		"abcabc145 (2026) S2 - 01.mkv",
	})
	if len(analysis.Cohorts) != 3 {
		t.Fatalf("identity qualifiers were merged into one cohort: %+v", analysis.Cohorts)
	}
}

func TestAnalyzeManyDoesNotTreatSequelNumberBeforeChangingYearAsEpisode(t *testing.T) {
	analysis := AnalyzeMany([]string{
		"abcabc147 1 2024.mkv",
		"abcabc147 2 2025.mkv",
		"abcabc147 3 2026.mkv",
	})
	for i, item := range analysis.Items {
		if item.Parsed.EpisodeStart != 0 {
			t.Fatalf("sequel[%d] became episode: %+v", i, item)
		}
	}
}

func TestAnalyzeFilesKeepsCustomContainersSeparateFromIdentity(t *testing.T) {
	inputs := []string{"[grpabc165] abcabc145 01 WEB.custom", "[grpabc165] abcabc145 02 WEB.M2TS"}
	batch := AnalyzeFiles(inputs)
	for i, item := range batch.Items {
		if item.Input != inputs[i] || item.Parsed.Title != "abcabc145" || item.Parsed.EpisodeStart != i+1 || item.Components.EpisodeEvidence != EpisodeEvidenceDifferential {
			t.Fatalf("item[%d]=%+v", i, item)
		}
	}
	if batch.Items[0].Components.Extension != ".custom" || batch.Items[1].Components.Extension != ".m2ts" {
		t.Fatalf("container extensions lost: %+v", batch.Items)
	}
	// Strip the actual container once: a title ending in a familiar media
	// suffix must not lose a second component of its identity.
	item := AnalyzeFiles([]string{"abcabc145.mkv.custom"}).Items[0]
	if item.Parsed.Title != "abcabc145 mkv" {
		t.Fatalf("double extension stripping: %+v", item)
	}
}
