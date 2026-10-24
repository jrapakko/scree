// Package textsafe removes bytes that could drive a terminal or corrupt
// tool output, for text that comes from notes or file names.
package textsafe

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Strip replaces invalid UTF-8 with U+FFFD and drops control characters:
// C0 except newline and tab, DEL, and C1 (U+0080 to U+009F).
func Strip(s string) string {
	if clean(s) {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if unsafe(r) {
			return -1
		}
		return r
	}, s)
}

func unsafe(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// clean reports whether Strip would leave s unchanged.
func clean(s string) bool {
	for i, r := range s {
		if unsafe(r) {
			return false
		}
		if r == utf8.RuneError {
			if _, n := utf8.DecodeRuneInString(s[i:]); n == 1 {
				return false
			}
		}
	}
	return true
}

type writer struct{ w io.Writer }

// Writer returns a writer that applies Strip to each Write before passing it
// to w. It reports len(p) as written, so callers such as log see a full
// write. A rune split across two writes is replaced, not passed through.
func Writer(w io.Writer) io.Writer { return writer{w} }

func (w writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.w, Strip(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
