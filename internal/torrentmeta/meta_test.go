package torrentmeta

import (
	"fmt"
	"testing"
)

func TestParseNativeNameAndInfoHash(t *testing.T) {
	name := "[grpabc166] abcabc060 - 01 [1080P].mkv"
	data := []byte(fmt.Sprintf("d8:announce15:https://tracker4:infod6:lengthi1e4:name%d:%see", len(name), name))
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name {
		t.Fatalf("name=%q", got.Name)
	}
	if len(got.InfoHash) != 40 {
		t.Fatalf("hash=%q", got.InfoHash)
	}
}

func TestParsePrefersUTF8Name(t *testing.T) {
	data := []byte("d4:infod4:name3:old10:name.utf-86:new.mkee")
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new.mk" {
		t.Fatalf("name=%q", got.Name)
	}
}

func TestParseMultiFileNativePaths(t *testing.T) {
	bstr := func(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }
	info := "d5:filesl" +
		"d6:lengthi1e4:pathl" + bstr("abcabc146 S01E06.mkv") + "ee" +
		"d6:lengthi1e4:pathl" + bstr("abcabc146 S01E07.mkv") + "ee" +
		"e4:name" + bstr("abcabc144") + "e"
	data := []byte("d4:info" + info + "e")
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "abcabc144" || len(got.Files) != 2 || got.Files[0] != "abcabc146 S01E06.mkv" || got.Files[1] != "abcabc146 S01E07.mkv" {
		t.Fatalf("metadata=%+v", got)
	}
}
