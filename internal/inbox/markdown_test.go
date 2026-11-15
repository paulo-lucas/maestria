package inbox

import (
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/ui"
)

func TestFencedShellLineBecomesCommandBlock(t *testing.T) {
	got := RenderMarkdown("Run this:\n\n```\ngo test ./... -run 'A&B' --flag=\"x\"\n```\n\nThen confirm.")
	want := string(ui.CommandBlock(`go test ./... -run 'A&B' --flag="x"`))
	if !strings.Contains(got, want) {
		t.Fatalf("no command block in %q\nwant %q", got, want)
	}
	if strings.Contains(got, "<pre>") {
		t.Fatalf("a single line should not stay a <pre>: %q", got)
	}
	// The language tag and a leading prompt are accepted.
	if got := RenderMarkdown("```bash\n$ make build\n```"); !strings.Contains(got, string(ui.CommandBlock("make build"))) {
		t.Fatalf("bash fence: %q", got)
	}
}

func TestFencedCodeStaysPreWhenNotOneShellLine(t *testing.T) {
	for name, src := range map[string]string{
		"multi-line": "```\nline one\nline two\n```",
		"other lang": "```go\nfmt.Println(1)\n```",
		"empty":      "```\n\n```",
		"too long":   "```\n" + strings.Repeat("a", maxCommandChars+1) + "\n```",
	} {
		got := RenderMarkdown(src)
		if strings.Contains(got, "code-inline") || !strings.Contains(got, "<pre>") {
			t.Errorf("%s: want a plain <pre>, got %q", name, got)
		}
	}
}

func TestCommandBlockCannotCarryMarkup(t *testing.T) {
	got := RenderMarkdown("```\necho <script>alert(1)</script> \"><img src=x onerror=alert(1)>\n```")
	if strings.Contains(got, "<script") || strings.Contains(got, "<img") {
		t.Fatalf("markup leaked: %q", got)
	}
}
