// Package inbox manages human tasks (notice, approve, input, choice, postpone) and safe Markdown rendering.
package inbox

import (
	"bytes"
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
	return strings.TrimSpace(mdPolicy.Sanitize(buf.String()))
}
