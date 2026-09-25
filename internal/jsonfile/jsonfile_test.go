package jsonfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type document struct {
	Name string `json:"name"`
}

func TestDecodeIsStrict(t *testing.T) {
	for _, input := range []string{
		`{"name":"one","unknown":true}`,
		`{"name":"one"} {"name":"two"}`,
	} {
		var value document
		if err := Decode([]byte(input), &value); err == nil {
			t.Fatalf("Decode(%q) unexpectedly succeeded", input)
		}
	}
}

func TestWriteCreatesParentAndUsesCanonicalFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := Write(path, document{Name: "value"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("state file has no trailing newline: %q", data)
	}
	var value document
	if err := Decode(data, &value); err != nil {
		t.Fatal(err)
	}
	if value.Name != "value" {
		t.Fatalf("Decode(written file) = %#v", value)
	}
}
