package medianame

import "testing"

func TestContextFreeParseCacheIsDisposable(t *testing.T) {
	resetParseCacheForTest()
	stem := "[grpabc165] abcabc146 S02E07 1080p"
	want := parseOne(stem)
	if _, ok := contextFreeParseCache.get(stem); !ok {
		t.Fatal("parse result was not cached")
	}
	resetParseCacheForTest()
	got := parseOne(stem)
	if got != want {
		t.Fatalf("cache reset changed parser result: got=%+v want=%+v", got, want)
	}
}

func BenchmarkParseOneUncached(b *testing.B) {
	stem := "[grpabc154] abcabc033 S02E07 [WebRip 1080p HEVC-10bit AAC][CHS]"
	for i := 0; i < b.N; i++ {
		_ = parseOneUncached(stem)
	}
}

func BenchmarkParseOneCached(b *testing.B) {
	resetParseCacheForTest()
	stem := "[grpabc154] abcabc033 S02E07 [WebRip 1080p HEVC-10bit AAC][CHS]"
	_ = parseOne(stem)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = parseOne(stem)
	}
}
