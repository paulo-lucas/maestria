package termsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanRemovesControlSequences(t *testing.T) {
	rlo := string(rune(0x202e))  // right-to-left override
	zwsp := string(rune(0x200b)) // zero-width space
	c1 := string(rune(0x9b))     // C1 control sequence introducer
	lsep := string(rune(0x2028)) // line separator
	psep := string(rune(0x2029)) // paragraph separator
	cases := map[string]string{
		"Safari on iPhone":                  "Safari on iPhone",
		"\x1b]52;c;Zm9v\x07\x1b[2K\rSafari": "]52;c;Zm9v [2K Safari",
		"line1\nline2\ttab":                 "line1 line2 tab",
		"a" + rlo + "b":                     "a b",
		"zero" + zwsp + "width":             "zero width",
		"c1" + c1 + "control":               "c1 control",
		"  padded  ":                        "padded",
		"bell\x07":                          "bell",
		"del\x7fchar":                       "del char",
		"\xff\xfeinvalid utf8":              "invalid utf8",
		"emoji 🙂 and accents é":             "emoji 🙂 and accents é",
		"line" + lsep + "x" + psep + "y":    "line x y",
	}
	for in, want := range cases {
		if got := Clean(in, 0); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanCutsOnACharacterBoundary(t *testing.T) {
	got := Clean(strings.Repeat("é", 50), 11)
	if len(got) > 11 || !utf8.ValidString(got) {
		t.Fatalf("bad cut: %q (%d bytes)", got, len(got))
	}
	if got := Clean("abcdef", 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
