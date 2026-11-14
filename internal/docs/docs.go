// Package docs provides a read-only, path-safe mirror of a workspace's docs/ folder.
package docs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const (
	MaxFileBytes  = 1 << 20 // 1 MiB per Markdown file
	MaxAssetBytes = 4 << 20 // 4 MiB per image asset
	MaxTreeFiles  = 500
	MaxTreeDepth  = 10
)

var (
	ErrInvalidPath = errors.New("invalid doc path")
	ErrNotMarkdown = errors.New("only markdown (.md) files are supported")
	ErrNotImage    = errors.New("unsupported image type")
	ErrNotFound    = errors.New("doc not found")
	ErrTooLarge    = errors.New("doc exceeds size limit")
)

var allowedImageExts = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
}

// Node represents one file or directory in the docs tree.
type Node struct {
	Name           string  `json:"name"`
	Path           string  `json:"path"`
	IsDir          bool    `json:"isDir"`
	Active         bool    `json:"active,omitempty"`
	Open           bool    `json:"open,omitempty"`
	IsFallbackRoot bool    `json:"isFallbackRoot,omitempty"`
	Children       []*Node `json:"children,omitempty"`
}

// Document represents a rendered Markdown document.
type Document struct {
	Path        string `json:"path"`
	DisplayPath string `json:"displayPath"`
	HTML        string `json:"html"`
	ModTime     int64  `json:"modTime"`
	Size        int64  `json:"size"`
	ETag        string `json:"etag"`
}

// Snapshot holds the full docs tree and active rendered document for a workspace.
type Snapshot struct {
	HasDocs   bool      `json:"hasDocs"`
	Truncated bool      `json:"truncated,omitempty"`
	Tree      []*Node   `json:"tree"`
	Doc       *Document `json:"doc,omitempty"`
	ETag      string    `json:"etag"`
}

// Asset holds a validated image file from docs/.
type Asset struct {
	Path        string
	ContentType string
	Data        []byte
	ModTime     int64
	ETag        string
}

type cacheKey struct {
	wsID     string
	realPath string
}

type cachedDoc struct {
	modTime int64
	size    int64
	html    string
	etag    string
}

// Viewer reads, caches, and sanitizes workspace documentation on demand.
type Viewer struct {
	mu    sync.RWMutex
	cache map[cacheKey]cachedDoc
	md    goldmark.Markdown
	pol   *bluemonday.Policy
}

// NewViewer constructs a docs Viewer with GFM Markdown and strict HTML sanitization.
func NewViewer() *Viewer {
	return &Viewer{
		cache: map[cacheKey]cachedDoc{},
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		),
		pol: buildDocsPolicy(),
	}
}

func buildDocsPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"p", "br", "hr",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"strong", "b", "em", "i", "del", "s", "code", "pre", "blockquote",
		"ul", "ol", "li",
		"table", "thead", "tbody", "tr", "th", "td",
		"img",
	)
	p.AllowRelativeURLs(true)
	p.AllowURLSchemes("http", "https", "mailto")
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("src", "alt", "title").OnElements("img")
	p.AllowAttrs("id").Matching(regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)).OnElements("h1", "h2", "h3", "h4", "h5", "h6")
	p.RequireNoFollowOnLinks(false)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[a-zA-Z0-9_-]{1,32}$`)).OnElements("code")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|center|right)$`)).OnElements("th", "td")
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")
	p.RequireParseableURLs(true)
	return p
}

// View builds the docs tree and renders the requested (or default) Markdown file.
func (v *Viewer) View(wsID, wsPath, requested string) (Snapshot, error) {
	if strings.TrimSpace(wsPath) == "" {
		return Snapshot{}, ErrNotFound
	}
	tree, truncated, treeSig, err := buildTree(wsPath)
	if err != nil {
		return Snapshot{}, err
	}
	if len(tree) == 0 {
		if requested != "" {
			if _, err := CleanRelPath(requested); err != nil {
				return Snapshot{}, err
			}
			return Snapshot{}, ErrNotFound
		}
		return Snapshot{HasDocs: false, Tree: []*Node{}, ETag: `"empty"`}, nil
	}

	targetRel := requested
	if strings.TrimSpace(targetRel) == "" {
		targetRel = firstFilePath(tree)
	}
	cleanRel, err := CleanRelPath(targetRel)
	if err != nil {
		return Snapshot{}, err
	}
	if !strings.EqualFold(path.Ext(cleanRel), ".md") {
		return Snapshot{}, ErrNotMarkdown
	}

	realFile, displayPath, info, isFallbackRoot, err := resolveMarkdownFile(wsPath, cleanRel)
	if err != nil {
		return Snapshot{}, err
	}
	if info.Size() > MaxFileBytes {
		return Snapshot{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, cleanRel, MaxFileBytes)
	}

	html, docETag, err := v.renderCached(wsID, realFile, cleanRel, isFallbackRoot, info)
	if err != nil {
		return Snapshot{}, err
	}

	markTreeSelection(tree, cleanRel)
	snapETag := hashETag(docETag + "|" + treeSig + "|" + cleanRel)

	return Snapshot{
		HasDocs:   true,
		Truncated: truncated,
		Tree:      tree,
		Doc: &Document{
			Path:        cleanRel,
			DisplayPath: displayPath,
			HTML:        html,
			ModTime:     info.ModTime().UnixMilli(),
			Size:        info.Size(),
			ETag:        docETag,
		},
		ETag: snapETag,
	}, nil
}

// ReadAsset safely reads an allowlisted image file inside the workspace's docs/ folder.
func (v *Viewer) ReadAsset(wsPath, requested string) (Asset, error) {
	if strings.TrimSpace(wsPath) == "" {
		return Asset{}, ErrNotFound
	}
	cleanRel, err := CleanRelPath(requested)
	if err != nil {
		return Asset{}, err
	}
	ext := strings.ToLower(path.Ext(cleanRel))
	ct, ok := allowedImageExts[ext]
	if !ok {
		return Asset{}, fmt.Errorf("%w: %s", ErrNotImage, ext)
	}

	realDocsRoot, err := resolveDocsRoot(wsPath)
	if err != nil {
		return Asset{}, err
	}
	realFile, info, err := resolveInsideRoot(realDocsRoot, cleanRel)
	if err != nil {
		return Asset{}, err
	}
	if info.Size() > MaxAssetBytes {
		return Asset{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, cleanRel, MaxAssetBytes)
	}
	data, err := readLimited(realFile, MaxAssetBytes)
	if err != nil {
		return Asset{}, err
	}
	etag := hashETag(fmt.Sprintf("%s|%d|%d", realFile, info.ModTime().UnixNano(), len(data)))
	return Asset{
		Path:        cleanRel,
		ContentType: ct,
		Data:        data,
		ModTime:     info.ModTime().UnixMilli(),
		ETag:        etag,
	}, nil
}

func (v *Viewer) renderCached(wsID, realFile, relPath string, isFallbackRoot bool, info os.FileInfo) (string, string, error) {
	modNano := info.ModTime().UnixNano()
	size := info.Size()
	key := cacheKey{wsID: wsID, realPath: realFile}

	v.mu.RLock()
	if cached, ok := v.cache[key]; ok && cached.modTime == modNano && cached.size == size {
		v.mu.RUnlock()
		return cached.html, cached.etag, nil
	}
	v.mu.RUnlock()

	raw, err := readLimited(realFile, MaxFileBytes)
	if err != nil {
		return "", "", err
	}
	rendered := v.renderMarkdown(wsID, relPath, isFallbackRoot, raw)
	etag := hashETag(fmt.Sprintf("%s|%d|%d|%s", realFile, modNano, size, rendered))

	v.mu.Lock()
	v.cache[key] = cachedDoc{
		modTime: modNano,
		size:    size,
		html:    rendered,
		etag:    etag,
	}
	v.mu.Unlock()
	return rendered, etag, nil
}

func (v *Viewer) renderMarkdown(wsID, relPath string, isFallbackRoot bool, src []byte) string {
	doc := v.md.Parser().Parse(text.NewReader(src))
	curDir := path.Dir(relPath)
	if curDir == "." {
		curDir = ""
	}

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := n.(type) {
		case *ast.Link:
			node.Destination = rewriteLinkDest(wsID, curDir, isFallbackRoot, string(node.Destination))
		case *ast.Image:
			node.Destination = rewriteImageDest(wsID, curDir, isFallbackRoot, string(node.Destination))
		}
		return ast.WalkContinue, nil
	})

	var buf bytes.Buffer
	if err := v.md.Renderer().Render(&buf, src, doc); err != nil {
		return "<p>" + bluemonday.StrictPolicy().Sanitize(string(src)) + "</p>"
	}
	return strings.TrimSpace(v.pol.Sanitize(buf.String()))
}

func rewriteLinkDest(wsID, curDir string, isFallbackRoot bool, raw string) []byte {
	s := strings.TrimSpace(raw)
	if s == "" || strings.HasPrefix(s, "#") {
		return []byte(s)
	}
	if strings.HasPrefix(s, "//") {
		return []byte("#")
	}
	u, err := url.Parse(s)
	if err != nil {
		return []byte("#")
	}
	if u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "mailto":
			return []byte(s)
		default:
			return []byte("#")
		}
	}

	linkPath := u.Path
	if linkPath == "" {
		if u.Fragment != "" {
			return []byte("#" + u.Fragment)
		}
		return []byte("#")
	}
	if strings.HasPrefix(linkPath, "/") || strings.Contains(linkPath, "\\") {
		return []byte("#")
	}
	resolved := path.Clean(path.Join(curDir, linkPath))
	if isFallbackRoot && strings.HasPrefix(resolved, "docs/") {
		resolved = strings.TrimPrefix(resolved, "docs/")
	}
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") || !strings.EqualFold(path.Ext(resolved), ".md") {
		return []byte("#")
	}
	if _, err := CleanRelPath(resolved); err != nil {
		return []byte("#")
	}

	target := "/workspaces/" + url.PathEscape(wsID) + "/docs?doc=" + url.QueryEscape(resolved)
	if u.Fragment != "" {
		target += "#" + url.PathEscape(u.Fragment)
	}
	return []byte(target)
}

func rewriteImageDest(wsID, curDir string, isFallbackRoot bool, raw string) []byte {
	s := strings.TrimSpace(raw)
	if s == "" || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Path == "" {
		return nil
	}
	resolved := path.Clean(path.Join(curDir, u.Path))
	if isFallbackRoot && strings.HasPrefix(resolved, "docs/") {
		resolved = strings.TrimPrefix(resolved, "docs/")
	}
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return nil
	}
	if _, err := CleanRelPath(resolved); err != nil {
		return nil
	}
	if _, ok := allowedImageExts[strings.ToLower(path.Ext(resolved))]; !ok {
		return nil
	}
	return []byte("/workspaces/" + url.PathEscape(wsID) + "/docs/asset?path=" + url.QueryEscape(resolved))
}

// CleanRelPath validates and normalizes a relative path inside docs/.
// It rejects empty paths, NUL bytes, backslashes, absolute paths, drive letters,
// hidden segments, and any literal or percent-encoded '..' traversal.
func CleanRelPath(raw string) (string, error) {
	if strings.ContainsRune(raw, '\x00') {
		return "", ErrInvalidPath
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidPath
	}
	if err := validateRawSegments(trimmed); err != nil {
		return "", err
	}
	if unescaped, err := url.PathUnescape(trimmed); err == nil && unescaped != trimmed {
		if err := validateRawSegments(unescaped); err != nil {
			return "", err
		}
		trimmed = unescaped
	}
	parts := strings.Split(trimmed, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return "", ErrInvalidPath
		}
	}
	return strings.Join(parts, "/"), nil
}

func validateRawSegments(s string) error {
	if strings.HasPrefix(s, "/") || strings.Contains(s, "\\") {
		return ErrInvalidPath
	}
	// Reject Windows-style drive prefix like C:...
	if len(s) >= 2 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' {
		return ErrInvalidPath
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return ErrInvalidPath
		}
	}
	return nil
}

func resolveWorkspaceRoot(wsPath string) (string, error) {
	absWs, err := filepath.Abs(wsPath)
	if err != nil {
		return "", ErrNotFound
	}
	realWs, err := filepath.EvalSymlinks(absWs)
	if err != nil {
		return "", ErrNotFound
	}
	info, err := os.Stat(realWs)
	if err != nil || !info.IsDir() {
		return "", ErrNotFound
	}
	return realWs, nil
}

func resolveDocsRoot(wsPath string) (string, error) {
	realWs, err := resolveWorkspaceRoot(wsPath)
	if err != nil {
		return "", err
	}
	docsCandidate := filepath.Join(realWs, "docs")
	realDocs, err := filepath.EvalSymlinks(docsCandidate)
	if err != nil {
		return "", ErrNotFound
	}
	if realDocs != realWs && !isRelInside(realWs, realDocs) {
		return "", ErrInvalidPath
	}
	info, err := os.Stat(realDocs)
	if err != nil || !info.IsDir() {
		return "", ErrNotFound
	}
	return realDocs, nil
}

func resolveMarkdownFile(wsPath, cleanRel string) (realFile, displayPath string, info os.FileInfo, isFallbackRoot bool, err error) {
	if realDocs, derr := resolveDocsRoot(wsPath); derr == nil {
		if rf, fi, rerr := resolveInsideRoot(realDocs, cleanRel); rerr == nil {
			return rf, "docs/" + cleanRel, fi, false, nil
		} else if errors.Is(rerr, ErrInvalidPath) {
			return "", "", nil, false, ErrInvalidPath
		}
	} else if errors.Is(derr, ErrInvalidPath) {
		return "", "", nil, false, ErrInvalidPath
	}

	if cleanRel == "README.md" {
		realWs, werr := resolveWorkspaceRoot(wsPath)
		if werr != nil {
			return "", "", nil, false, ErrNotFound
		}
		rf, fi, rerr := resolveInsideRoot(realWs, "README.md")
		if rerr != nil {
			return "", "", nil, false, rerr
		}
		return rf, "README.md", fi, true, nil
	}
	return "", "", nil, false, ErrNotFound
}

func resolveInsideRoot(realRoot, cleanRel string) (string, os.FileInfo, error) {
	candidate := filepath.Join(realRoot, filepath.FromSlash(cleanRel))
	if _, err := os.Lstat(candidate); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, ErrNotFound
		}
		return "", nil, ErrNotFound
	}
	realTarget, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, ErrNotFound
		}
		return "", nil, ErrInvalidPath
	}
	if !isRelInside(realRoot, realTarget) {
		return "", nil, ErrInvalidPath
	}
	info, err := os.Stat(realTarget)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, ErrNotFound
	}
	return realTarget, info, nil
}

func isRelInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func readLimited(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrTooLarge, maxBytes)
	}
	return data, nil
}

func buildTree(wsPath string) ([]*Node, bool, string, error) {
	realWs, err := resolveWorkspaceRoot(wsPath)
	if err != nil {
		return nil, false, "", nil
	}
	var rootNodes []*Node
	truncated := false
	fileCount := 0
	var sigBuf strings.Builder

	if realDocs, err := resolveDocsRoot(wsPath); err == nil {
		visited := map[string]bool{realDocs: true}
		rootNodes, truncated = walkDirTree(realDocs, realDocs, "", 0, &fileCount, visited, &sigBuf)
	}

	hasReadme := len(rootNodes) > 0 && !rootNodes[0].IsDir && strings.EqualFold(rootNodes[0].Name, "README.md")
	if !hasReadme {
		if rf, fi, err := resolveInsideRoot(realWs, "README.md"); err == nil {
			_ = rf
			readmeNode := &Node{
				Name:           "README.md",
				Path:           "README.md",
				IsFallbackRoot: true,
			}
			rootNodes = append([]*Node{readmeNode}, rootNodes...)
			fmt.Fprintf(&sigBuf, "README.md:%d:%d;", fi.ModTime().UnixNano(), fi.Size())
		}
	}

	return rootNodes, truncated, sigBuf.String(), nil
}

func walkDirTree(
	realDocsRoot, dirPath, relPrefix string,
	depth int,
	fileCount *int,
	visited map[string]bool,
	sigBuf *strings.Builder,
) ([]*Node, bool) {
	if depth > MaxTreeDepth || *fileCount >= MaxTreeFiles {
		return nil, *fileCount >= MaxTreeFiles
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, false
	}

	var readme *Node
	var files []*Node
	var dirs []*Node
	truncated := false

	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		fullPath := filepath.Join(dirPath, name)
		realEntry, err := filepath.EvalSymlinks(fullPath)
		if err != nil || !isRelInside(realDocsRoot, realEntry) {
			continue
		}
		info, err := os.Stat(realEntry)
		if err != nil {
			continue
		}

		rel := name
		if relPrefix != "" {
			rel = relPrefix + "/" + name
		}

		if info.IsDir() {
			if visited[realEntry] || depth+1 > MaxTreeDepth {
				continue
			}
			visited[realEntry] = true
			children, childTrunc := walkDirTree(realDocsRoot, realEntry, rel, depth+1, fileCount, visited, sigBuf)
			if childTrunc {
				truncated = true
			}
			if len(children) > 0 {
				dirs = append(dirs, &Node{
					Name:     name,
					Path:     rel,
					IsDir:    true,
					Children: children,
				})
			}
			continue
		}

		if !info.Mode().IsRegular() || !strings.EqualFold(path.Ext(name), ".md") {
			continue
		}
		if *fileCount >= MaxTreeFiles {
			truncated = true
			break
		}
		*fileCount++
		fmt.Fprintf(sigBuf, "%s:%d:%d;", rel, info.ModTime().UnixNano(), info.Size())

		node := &Node{Name: name, Path: rel}
		if strings.EqualFold(name, "README.md") && readme == nil {
			readme = node
		} else {
			files = append(files, node)
		}
	}

	sortNodes := func(list []*Node) {
		slices.SortFunc(list, func(a, b *Node) int {
			la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
			if la != lb {
				return strings.Compare(la, lb)
			}
			return strings.Compare(a.Name, b.Name)
		})
	}
	sortNodes(files)
	sortNodes(dirs)

	out := make([]*Node, 0, 1+len(files)+len(dirs))
	if readme != nil {
		out = append(out, readme)
	}
	out = append(out, files...)
	out = append(out, dirs...)
	return out, truncated
}

func firstFilePath(nodes []*Node) string {
	for _, n := range nodes {
		if !n.IsDir {
			return n.Path
		}
		if p := firstFilePath(n.Children); p != "" {
			return p
		}
	}
	return ""
}

func markTreeSelection(nodes []*Node, activePath string) bool {
	found := false
	for _, n := range nodes {
		if n.IsDir {
			if markTreeSelection(n.Children, activePath) {
				n.Open = true
				found = true
			}
		} else if n.Path == activePath {
			n.Active = true
			found = true
		}
	}
	return found
}

func hashETag(s string) string {
	sum := sha256.Sum256([]byte(s))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// FormatModTime formats an epoch ms timestamp for display.
func FormatModTime(ms int64, now time.Time) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).Local()
	if t.Year() == now.Local().Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}
