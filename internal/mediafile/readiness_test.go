package mediafile

import (
	"testing"
	"testing/fstest"
)

func TestReadyDownloaderEvidence(t *testing.T) {
	exts := ExtensionSet([]string{"mkv", ".mp4"})
	tests := []struct {
		name string
		fs   fstest.MapFS
		path string
		want bool
	}{
		{"qbit mkv", fstest.MapFS{"Episode.mkv.!qB": {}}, "Episode.mkv.!qB", false},
		{"qbit mp4", fstest.MapFS{"Episode.mp4.!qB": {}}, "Episode.mp4.!qB", false},
		{"transmission mkv", fstest.MapFS{"Episode.mkv.part": {}}, "Episode.mkv.part", false},
		{"transmission mp4", fstest.MapFS{"Episode.mp4.part": {}}, "Episode.mp4.part", false},
		{"aria2 active", fstest.MapFS{"Episode.mkv": {}, "Episode.mkv.aria2": {}}, "Episode.mkv", false},
		{"ready", fstest.MapFS{"Episode.mkv": {}}, "Episode.mkv", true},
		{"tmp", fstest.MapFS{"Episode.mkv.tmp": {}}, "Episode.mkv.tmp", false},
		{"random", fstest.MapFS{"Episode.mkv.random": {}}, "Episode.mkv.random", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Ready(tt.fs, tt.path, exts)
			if err != nil || got != tt.want {
				t.Fatalf("Ready() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}
