package cloud

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The machine card's second line ("laptop offline · 5 min · +1") must never end in a bare
// separator, whatever the relative time returns. Runs the real offlineMeta from hosted.js.
func TestOfflineMachineLineHasNoTrailingSeparator(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	raw, err := os.ReadFile("static/hosted.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	begin := strings.Index(src, "// begin:offline-meta")
	end := strings.Index(src, "// end:offline-meta")
	if begin < 0 || end < begin {
		t.Fatal("offline-meta block not found in hosted.js")
	}
	script := filepath.Join(t.TempDir(), "check.cjs")
	js := src[begin:end] + `
const none = () => '';
const mins = () => '5 min';
const m = (name, seen) => ({ name, lastSeenAt: seen });
console.log(JSON.stringify([
  offlineMeta([m('Laptop', 1)], 1, mins),
  offlineMeta([m('Laptop', 1)], 1, none),
  offlineMeta([m('Laptop', 0)], 1, mins),
  offlineMeta([m('Laptop', 1), m('Desk', 2), m('Pi', 3)], 3, mins),
  offlineMeta([m('Laptop', 1), m('Desk', 2)], 2, none),
  offlineMeta([], 2, mins),
  offlineMeta([], 0, mins),
]));
`
	if err := os.WriteFile(script, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	want := []string{
		"Laptop offline · 5 min",
		"Laptop offline",
		"Laptop offline",
		"Laptop offline · 5 min · +2",
		"Laptop offline · +1",
		"",
		"Link your first machine",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("case %d: %q, want %q", i, got[i], want[i])
		}
		if strings.HasSuffix(strings.TrimSpace(got[i]), "·") {
			t.Errorf("case %d ends in a separator: %q", i, got[i])
		}
	}
}
