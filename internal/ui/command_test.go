package ui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every command is one component: the whole line, a Copy button, no inline script or handler.
func TestCommandBlockMarkup(t *testing.T) {
	got := string(CommandBlock(`maestria floor plan <name> --brief "a & b"`))
	for _, want := range []string{
		`<div class="code-inline">`,
		`<span class="cmd-t">maestria</span> <span class="cmd-t">floor</span>`, `<span class="cmd-t">&lt;name&gt;</span>`, `<span class="cmd-t">&amp;</span>`, `<span class="cmd-t">--brief</span>`,
		`data-copy`, `aria-label="Copy command"`, `<use href="#i-copy"/>`, `<span data-copy-label>Copy</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command block lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"onclick", "<script", "javascript:"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Errorf("command block has %q: strict CSP forbids inline scripts", bad)
		}
	}
}

// No template shows a command as a code span inside a sentence any more: every `maestria …`
// command is a command block.
func TestNoTemplateShowsACommandAsInlineCode(t *testing.T) {
	entries, err := files.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, err := files.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			// `maestria say` / `maestria wait` in the chat hint name what agents run; the rest
			// would be something the person has to type.
			if strings.Contains(line, "<code>maestria ") && !strings.Contains(e.Name(), "workspace-chat") {
				t.Errorf("%s:%d shows a command inline, use {{cmd \"…\"}}: %s", e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The Copy button works: run the real copy.js under node with a minimal page, press the button of
// a block and check what reached the clipboard, the label, and that the markup builder agrees
// with the Go helper.
func TestCopyScriptCopiesTheWholeLine(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := filepath.Join(t.TempDir(), "check.cjs")
	src := `
const fs = require('fs');
let clickHandler, copied = [];
const label = { textContent: 'Copy', attrs: {}, hasAttribute(k) { return k in this.attrs; }, getAttribute(k) { return this.attrs[k]; }, setAttribute(k, v) { this.attrs[k] = v; } };
const codeEl = { textContent: '  maestria devices approve 47128659 \n' };
const classes = new Set();
const btn = {
  attrs: { 'data-copy': '' }, classList: { toggle(c, on) { on ? classes.add(c) : classes.delete(c); }, remove(c) { classes.delete(c); } },
  hasAttribute(k) { return k in this.attrs; }, getAttribute(k) { return this.attrs[k]; }, setAttribute(k, v) { this.attrs[k] = v; },
  querySelector(s) { return s === '[data-copy-label]' ? label : null; },
  closest(s) { return s === '.code-inline' ? { querySelector: () => codeEl } : (s === '[data-copy]' ? this : null); },
};
global.document = { addEventListener(type, fn) { if (type === 'click') clickHandler = fn; } };
Object.defineProperty(global, 'navigator', { value: { clipboard: { writeText: (t) => { copied.push(t); return Promise.resolve(); } } }, configurable: true }); // node >= 21 has a read-only navigator
global.window = global;
require(process.argv[2]);
const html = window.MaestriaCmd.html('maestria connect --link AB-CD & <x>');
(async () => {
  let prevented = false;
  clickHandler({ target: { closest: (s) => (s === '[data-copy]' ? btn : null) }, preventDefault() { prevented = true; } });
  await new Promise((r) => setTimeout(r, 20));
  console.log(JSON.stringify({ copied, label: label.textContent, copiedClass: classes.has('is-copied'), prevented, html }));
  process.exit(0);
})();
`
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	// The repository's package.json makes .js files ES modules for node; copy.js is a classic
	// script, so run a copy as CommonJS.
	code, err := os.ReadFile("static/copy.js")
	if err != nil {
		t.Fatal(err)
	}
	cjs := filepath.Join(filepath.Dir(script), "copy.cjs")
	if err := os.WriteFile(cjs, code, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, cjs).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Copied      []string
		Label       string
		CopiedClass bool
		Prevented   bool
		HTML        string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(got.Copied) != 1 || got.Copied[0] != "maestria devices approve 47128659" {
		t.Fatalf("clipboard got %q, want the whole trimmed line", got.Copied)
	}
	if got.Label != "Copied" || !got.CopiedClass || !got.Prevented {
		t.Fatalf("feedback: %+v", got)
	}
	if want := string(CommandBlock("maestria connect --link AB-CD & <x>")); got.HTML != want {
		t.Fatalf("the browser builder and the Go helper differ:\n js: %s\n go: %s", got.HTML, want)
	}
}
