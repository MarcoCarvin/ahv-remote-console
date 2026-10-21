package vdagent

import "testing"

// SEC-01: host-origin clipboard text must be re-sanitized before it reaches the
// guest clipboard. Control characters, bidi overrides and zero-width/separator
// characters are stripped; ordinary text and tab/newline/CR are preserved.
// Inputs are built from code points (not literal invisible Unicode) so the
// source stays ASCII and unambiguous.
func TestSanitizeClipboardText(t *testing.T) {
	cp := func(c rune) string { return string(c) }
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"nul", "a" + cp(0x00) + "b", "ab"},
		{"esc", "a" + cp(0x1b) + "b", "ab"},
		{"del", "a" + cp(0x7f) + "b", "ab"},
		{"c1-nel", "a" + cp(0x85) + "b", "ab"},
		{"keeps-whitespace", "l1\nl2\tx\r", "l1\nl2\tx\r"},
		{"bidi-rlo", "a" + cp(0x202e) + "b", "ab"},
		{"bidi-isolate", "a" + cp(0x2066) + "b" + cp(0x2069) + "c", "abc"},
		{"zero-width", "a" + cp(0x200b) + "b" + cp(0x200d) + "c", "abc"},
		{"bom", cp(0xfeff) + "hi", "hi"},
		{"line-sep", "a" + cp(0x2028) + "b" + cp(0x2029) + "c", "abc"},
	}
	for _, c := range cases {
		if got := sanitizeClipboardText(c.in); got != c.want {
			t.Errorf("%s: sanitizeClipboardText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
