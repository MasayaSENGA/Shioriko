package mount

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestServerKey(t *testing.T) {
	k := serverKey("127.0.0.1:5001")
	if k != "//127.0.0.1:5001/" {
		t.Fatalf("serverKey = %q", k)
	}
	for _, src := range []string{"http://127.0.0.1:50012/Pixel/", "http://127.0.0.1:500/Pixel/"} {
		if contains(src, k) {
			t.Errorf("%q must not match %q", src, k)
		}
	}
	if !contains("http://127.0.0.1:5001/Pixel%208/", k) {
		t.Error("own mount not matched")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCstr(t *testing.T) {
	b := make([]int8, 16)
	for i, c := range "webdav" {
		b[i] = int8(c)
	}
	if got := cstr(b); got != "webdav" {
		t.Errorf("cstr = %q", got)
	}
	if got := cstr(nil); got != "" {
		t.Errorf("cstr(nil) = %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"  Pixel 8 ": "Pixel 8",
		"a/b:c\\d":   "a-b-c-d",
		"":           "Android",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanupStaleWithoutRecord(t *testing.T) {
	mp, err := CleanupStale(t.TempDir())
	if mp != "" || err != nil {
		t.Errorf("CleanupStale on empty dir = %q, %v", mp, err)
	}
	if mp, err := CleanupStale(""); mp != "" || err != nil {
		t.Errorf("CleanupStale(\"\") = %q, %v", mp, err)
	}
}

func TestSanitizeNameKeepsUTF8(t *testing.T) {
	long := strings.Repeat("端末", 20) // 120 bytes
	got := sanitizeName(long)
	if len(got) > 60 || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Errorf("sanitizeName(long) = %q (%d bytes)", got, len(got))
	}
}
