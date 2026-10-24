package policy

import "testing"

func TestFrontMatterIgnore(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"plain", "---\nscree: ignore\n---\nbody\n", true},
		{"single quoted", "---\nscree: 'ignore'\n---\n", true},
		{"double quoted", "---\ntitle: x\nscree: \"ignore\"\n---\n", true},
		{"crlf", "---\r\nscree: ignore\r\n---\r\nbody\r\n", true},
		{"bom", "\ufeff---\nscree: ignore\n---\n", true},
		{"trailing comment", "---\nscree: ignore # keep local\n---\n", true},
		{"dots close", "---\nscree:\tignore\n...\n", true},
		{"in body", "# Title\n\nscree: ignore\n", false},
		{"after front matter", "---\ntitle: x\n---\nscree: ignore\n", false},
		{"other value", "---\nscree: ignored\n---\n", false},
		{"indented", "---\nmeta:\n  scree: ignore\n---\n", false},
		{"unclosed", "---\nscree: ignore\nbody\n", false},
		{"no space", "---\nscree:ignore\n---\n", false},
		{"hash without space", "---\nscree: ignore#x\n---\n", false},
		{"empty", "", false},
		{"not first line", "\n---\nscree: ignore\n---\n", false},
		{"double-quoted key", "---\n\"scree\": ignore\n---\n", true},
		{"single-quoted key", "---\n'scree': ignore\n---\n", true},
		{"space before colon", "---\nscree : ignore\n---\n", true},
		{"tab after colon", "---\nscree:\tignore\n---\n", true},
		{"upper case value", "---\nscree: IGNORE\n---\n", true},
		{"quoted mixed case and comment", "---\nscree: \"Ignore\"   # c\n---\n", true},
		{"nested two spaces", "---\n  scree: ignore\n---\n", false},
		{"other key", "---\nscreen: ignore\n---\n", false},
		{"whole line quoted", "---\n\"scree: ignore\"\n---\n", false},
		{"mismatched quotes", "---\nscree: \"ignore'\n---\n", false},
	}
	for _, c := range cases {
		if got := frontMatterIgnore([]byte(c.src)); got != c.want {
			t.Errorf("%s: frontMatterIgnore = %v, want %v", c.name, got, c.want)
		}
	}
}
