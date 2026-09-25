package medianame

import "testing"

func TestTechnicalTraitsOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  TechnicalTraits
	}{
		{
			name:  "webrip hevc 10bit aac",
			input: "[grpabc154] abcabc146 - 05 [WebRip 1080p HEVC-10bit AAC SRTx2].mkv",
			want:  TechnicalTraits{Source: "webrip", SourceKind: "web", VideoCodec: "hevc", BitDepth: "10bit", AudioCodec: "aac"},
		},
		{
			name:  "webdl avc aac",
			input: "[grpabc153] abcabc146 - 06 [CR WEB-DL 1080p AVC AAC][CHT].mkv",
			want:  TechnicalTraits{Source: "web-dl", SourceKind: "web", VideoCodec: "avc", AudioCodec: "aac"},
		},
		{
			name:  "unknown source stays unknown",
			input: "[grpabc155] abcabc146[1080P][BIG5][06][MP4]",
			want:  TechnicalTraits{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TechnicalTraitsOf(tc.input); got != tc.want {
				t.Fatalf("traits=%+v want=%+v", got, tc.want)
			}
		})
	}
}
