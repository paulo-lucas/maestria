package wire

import (
	"strings"
	"testing"
)

func TestParsePinForms(t *testing.T) {
	var want Pin
	for i := range want {
		want[i] = byte(i * 7)
	}
	hex := want.Hex()
	colon := strings.ToUpper(strings.Join(func() []string {
		var p []string
		for i := 0; i < len(hex); i += 2 {
			p = append(p, hex[i:i+2])
		}
		return p
	}(), ":"))
	for _, in := range []string{hex, colon, " " + hex + "\n", strings.ToLower(colon)} {
		got, err := ParsePin(in)
		if err != nil || got != want {
			t.Errorf("ParsePin(%q) = %v %v", in, got, err)
		}
	}
	got, err := ParsePin("AAcOFRwjKjE4P0ZNVFtiaXB3foWMk5qhqK+2vcTL0tk=")
	if err != nil || got != want {
		t.Errorf("base64: %v %v", got, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("z", 64), hex[:62], hex + "00"} {
		if _, err := ParsePin(bad); err == nil {
			t.Errorf("ParsePin(%q) accepted", bad)
		}
	}
}
