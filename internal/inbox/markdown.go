// Package inbox manages human tasks (notice, approve, input, choice, postpone) and safe Markdown rendering.
package inbox

import (
	"bytes"
	stdhtml "html"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var (
	mdParser = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
		),
	)
	mdPolicy = buildMarkdownPolicy()
)

func buildMarkdownPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"p", "br", "hr",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"strong", "b", "em", "i", "del", "s", "code", "pre", "blockquote",
		"ul", "ol", "li",
		"table", "thead", "tbody", "tr", "th", "td",
	)
	p.AllowURLSchemes("http", "https", "mailto")
	p.AllowAttrs("href").OnElements("a")
	p.RequireNoFollowOnLinks(false)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[a-zA-Z0-9_-]{1,32}$`)).OnElements("code")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|center|right)$`)).OnElements("th", "td")
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")
	p.RequireParseableURLs(true)
	_ = url.URL{}
	return p
}

// RenderMarkdown renders untrusted Markdown into CSP-safe sanitized HTML without inline styles or scripts.
func RenderMarkdown(src string) string {
	var buf bytes.Buffer
	if err := mdParser.Convert([]byte(src), &buf); err != nil {
		return "<p>" + bluemonday.StrictPolicy().Sanitize(src) + "</p>"
	}
	return commandBlocks(strings.TrimSpace(mdPolicy.Sanitize(buf.String())))
}

// A fenced block of one shell line (what a Maestro posts for the human to run) is shown as the
// panel's command block, with a Copy button; anything longer stays a plain scrollable <pre>.
var (
	fencedBlock = regexp.MustCompile(`<pre><code(?: class="language-([a-zA-Z0-9_-]{1,32})")?>([^<]*)</code></pre>`)
	shellLangs  = map[string]bool{"": true, "sh": true, "bash": true, "zsh": true, "shell": true, "console": true, "terminal": true, "cmd": true, "powershell": true, "ps1": true, "fish": true}
)

const maxCommandChars = 4000

func commandBlocks(h string) string {
	if !strings.Contains(h, "<pre>") {
		return h
	}
	return fencedBlock.ReplaceAllStringFunc(h, func(m string) string {
		parts := fencedBlock.FindStringSubmatch(m)
		if !shellLangs[strings.ToLower(parts[1])] {
			return m
		}
		cmd := strings.TrimRight(stdhtml.UnescapeString(parts[2]), "\r\n")
		cmd = strings.TrimSpace(cmd)
		cmd = strings.TrimPrefix(cmd, "$ ")
		if cmd == "" || strings.ContainsAny(cmd, "\r\n") || len(cmd) > maxCommandChars {
			return m
		}
		return commandBlockHTML(cmd)
	})
}

// commandBlockHTML is the markup of ui.CommandBlock (kept identical; a test compares them). It is
// built here from escaped text only, after sanitizing, so the sanitizer never sees the button.
func commandBlockHTML(command string) string {
	words := strings.Split(command, " ")
	for i, w := range words {
		words[i] = `<span class="cmd-t">` + stdhtml.EscapeString(w) + `</span>`
	}
	return `<div class="code-inline"><code>` + strings.Join(words, " ") + `</code>` +
		`<button class="btn btn--ghost btn--sm" type="button" data-copy aria-label="Copy command" title="Copy command">` +
		`<svg class="icon" aria-hidden="true"><use href="#i-copy"/></svg><span data-copy-label>Copy</span></button></div>`
}
