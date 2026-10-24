package textsafe

import (
	"bytes"
	"strings"
	"testing"
)

func TestStrip(t *testing.T) {
	cases := map[string]string{
		"plain text":                    "plain text",
		"keep\nnew\tlines":              "keep\nnew\tlines",
		"red \x1b[31mtext\x1b[0m":       "red [31mtext[0m",
		"bell\x07 nul\x00 cr\r del\x7f": "bell nul cr del",
		"c1 \u009b31m rune":             "c1 31m rune",
		"héllo ✓":                       "héllo ✓",
	}
	for in, want := range cases {
		if got := Strip(in); got != want {
			t.Errorf("Strip(%q) = %q, want %q", in, got, want)
		}
	}
	// A raw 0x9b byte is invalid UTF-8; terminals may read it as CSI.
	got := Strip("raw \x9b31m csi")
	if strings.ContainsRune(got, 0x9b) || strings.Contains(got, "\x9b") || got != "raw �31m csi" {
		t.Errorf("raw CSI: %q", got)
	}
}

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	w := Writer(&buf)
	in := []byte("log \x1b]0;title\x07 line\n")
	n, err := w.Write(in)
	if err != nil || n != len(in) {
		t.Fatalf("Write = %d, %v; want %d", n, err, len(in))
	}
	if got := buf.String(); got != "log ]0;title line\n" {
		t.Errorf("wrote %q", got)
	}
}
