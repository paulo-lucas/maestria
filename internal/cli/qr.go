package cli

import (
	"strings"

	"rsc.io/qr"
)

// RenderQR draws text as a QR code for a terminal. It forces black modules on a white background
// with ANSI colours, so it scans the same on dark and light terminals. Two rows share one line
// using half-block characters.
func RenderQR(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 2
	size := code.Size + 2*quiet
	black := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}
	var sb strings.Builder
	for y := 0; y < size; y += 2 {
		sb.WriteString("\x1b[47;30m")
		for x := 0; x < size; x++ {
			top, bottom := black(x, y), black(x, y+1)
			switch {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\x1b[0m\n")
	}
	return sb.String(), nil
}
