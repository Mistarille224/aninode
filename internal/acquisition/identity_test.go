package acquisition

import "testing"

func TestCanonicalIdentity(t *testing.T) {
	tests := []struct {
		name  string
		input IdentityInput
		kind  string
		id    string
	}{
		{
			name:  "hex info hash",
			input: IdentityInput{InfoHash: "0123456789ABCDEF0123456789ABCDEF01234567", DownloadURL: "https://ignored.test"},
			kind:  "btih", id: "btih-0123456789abcdef0123456789abcdef01234567",
		},
		{
			name:  "base32 magnet",
			input: IdentityInput{MagnetURL: "magnet:?dn=abcabc146&xt=urn%3Abtih%3AAERUKZ4JVPG66AJDIVTYTK6N54ASGRLH"},
			kind:  "btih", id: "btih-0123456789abcdef0123456789abcdef01234567",
		},
		{
			name:  "normalized URL",
			input: IdentityInput{DownloadURL: "HTTPS://abcabc145.COM:443/path?b=2&a=1#fragment"},
			kind:  "url",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalIdentity(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.kind || (test.id != "" && got.ID != test.id) {
				t.Fatalf("identity = %+v", got)
			}
			if test.name == "normalized URL" && got.Canonical != "https://abcabc145.com/path?a=1&b=2" {
				t.Fatalf("canonical URL = %q", got.Canonical)
			}
		})
	}
}

func TestCanonicalIdentityRejectsInvalidInput(t *testing.T) {
	if _, err := CanonicalIdentity(IdentityInput{MagnetURL: "magnet:?dn=no-hash"}); err == nil {
		t.Fatal("expected error")
	}
}
