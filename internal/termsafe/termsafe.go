// Package termsafe cleans text that someone else controls (a hosted server, a browser, a peer)
// before it is stored or printed to a terminal. Control characters could carry escape sequences
// that rewrite earlier lines, change the title or write to the clipboard.
package termsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean returns s without control characters (C0, DEL, C1), without invisible format characters
// (bidirectional overrides, zero-width marks) and with runs of white space folded to one space. It
// is cut to at most max bytes on a character boundary. max <= 0 means no cut.
func Clean(s string, max int) string {
	var sb strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			// A control character ends a word the way a space does, so "a\x1b[2Kb" cannot glue
			// into something that reads differently.
			if sb.Len() > 0 {
				space = true
			}
			continue
		case unicode.IsSpace(r):
			if sb.Len() > 0 {
				space = true
			}
			continue
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		sb.WriteRune(r)
		if max > 0 && sb.Len() >= max {
			break
		}
	}
	out := sb.String()
	if max > 0 && len(out) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}
