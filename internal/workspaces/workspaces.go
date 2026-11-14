// Package workspaces loads, saves, discovers, and resolves Maestria workspaces.
package workspaces

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/paulo-lucas/maestria/internal/privfile"
)

// DefaultPalette matches the V1 workspace accent colors.
var DefaultPalette = []string{
	"#4f46e5",
	"#0ea5e9",
	"#10b981",
	"#f59e0b",
	"#ef4444",
	"#ec4899",
	"#8b5cf6",
	"#14b8a6",
}

// Workspace represents a registered or discovered Maestri workspace.
type Workspace struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	MaestroAgent    string `json:"maestroAgent"`
	MaestroProvider string `json:"maestroProvider,omitempty"`
	RoadmapPath     string `json:"roadmapPath,omitempty"`
	Color           string `json:"color"`
	Discovered      bool   `json:"discovered,omitempty"`
	Tone            string `json:"tone,omitempty"`
}

// fileWorkspace omits runtime-only fields (Discovered, Tone) so workspaces.json matches V1.
type fileWorkspace struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	MaestroAgent    string `json:"maestroAgent"`
	MaestroProvider string `json:"maestroProvider,omitempty"`
	RoadmapPath     string `json:"roadmapPath,omitempty"`
	Color           string `json:"color"`
}

// AddInput describes a workspace to add to workspaces.json.
type AddInput struct {
	ID              string
	Name            string
	Path            string
	MaestroAgent    string
	MaestroProvider string
	RoadmapPath     string
	Color           string
}

var (
	hexColorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	nonSlugRE  = regexp.MustCompile(`[^a-z0-9]+`)
)

// FilePath returns the path to workspaces.json in configDir.
func FilePath(configDir string) string {
	return filepath.Join(configDir, "workspaces.json")
}

// Load reads workspaces.json from configDir. A missing file returns an empty slice.
func Load(configDir string) ([]Workspace, error) {
	data, err := os.ReadFile(FilePath(configDir))
	if errors.Is(err, fs.ErrNotExist) {
		return []Workspace{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspaces: read %s: %w", FilePath(configDir), err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return []Workspace{}, nil
	}
	var raw []fileWorkspace
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("workspaces: parse %s: %w", FilePath(configDir), err)
	}
	out := make([]Workspace, 0, len(raw))
	for i, w := range raw {
		color := normalizeColor(w.Color, i)
		agent := strings.TrimSpace(w.MaestroAgent)
		if agent == "" {
			agent = "Maestro"
		}
		name := strings.TrimSpace(w.Name)
		if name == "" {
			name = w.ID
		}
		out = append(out, Workspace{
			ID:              strings.TrimSpace(w.ID),
			Name:            name,
			Path:            cleanPath(w.Path),
			MaestroAgent:    agent,
			MaestroProvider: strings.TrimSpace(w.MaestroProvider),
			RoadmapPath:     strings.TrimSpace(w.RoadmapPath),
			Color:           color,
			Tone:            ColorTone(color),
		})
	}
	return out, nil
}

// Save writes workspaces.json atomically (0600) in V1-compatible format.
func Save(configDir string, list []Workspace) error {
	out := make([]fileWorkspace, 0, len(list))
	for i, w := range list {
		agent := strings.TrimSpace(w.MaestroAgent)
		if agent == "" {
			agent = "Maestro"
		}
		out = append(out, fileWorkspace{
			ID:              strings.TrimSpace(w.ID),
			Name:            strings.TrimSpace(w.Name),
			Path:            cleanPath(w.Path),
			MaestroAgent:    agent,
			MaestroProvider: strings.TrimSpace(w.MaestroProvider),
			RoadmapPath:     strings.TrimSpace(w.RoadmapPath),
			Color:           normalizeColor(w.Color, i),
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return privfile.WriteAtomic(FilePath(configDir), append(data, '\n'))
}

// Add registers a workspace in configDir/workspaces.json.
func Add(configDir string, in AddInput) (Workspace, error) {
	list, err := Load(configDir)
	if err != nil {
		return Workspace{}, err
	}
	p := strings.TrimSpace(in.Path)
	if p == "" {
		return Workspace{}, errors.New("workspaces: path is required")
	}
	absPath, err := filepath.Abs(p)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspaces: resolve path %q: %w", p, err)
	}
	absPath = filepath.Clean(absPath)

	for _, existing := range list {
		if samePath(existing.Path, absPath) {
			return Workspace{}, fmt.Errorf("workspace already registered at %s (id: %s)", absPath, existing.ID)
		}
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = filepath.Base(absPath)
	}
	baseID := strings.TrimSpace(in.ID)
	if baseID == "" {
		baseID = name
	}
	id := UniqueID(baseID, list)

	agent := strings.TrimSpace(in.MaestroAgent)
	if agent == "" {
		agent = "Maestro"
	}
	color := normalizeColor(in.Color, len(list))
	ws := Workspace{
		ID:              id,
		Name:            name,
		Path:            absPath,
		MaestroAgent:    agent,
		MaestroProvider: strings.TrimSpace(in.MaestroProvider),
		RoadmapPath:     strings.TrimSpace(in.RoadmapPath),
		Color:           color,
		Tone:            ColorTone(color),
	}
	list = append(list, ws)
	if err := Save(configDir, list); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

// Remove deletes a workspace matched by id, name, or path from configDir/workspaces.json.
func Remove(configDir, target string) (Workspace, error) {
	list, err := Load(configDir)
	if err != nil {
		return Workspace{}, err
	}
	found, err := Resolve(list, target, "")
	if err != nil {
		return Workspace{}, err
	}
	next := make([]Workspace, 0, len(list)-1)
	for _, w := range list {
		if w.ID != found.ID {
			next = append(next, w)
		}
	}
	if err := Save(configDir, next); err != nil {
		return Workspace{}, err
	}
	return *found, nil
}

// Merged combines explicit workspaces.json entries with read-only Maestri discovery.
// Explicit entries always win when paths or IDs overlap.
func Merged(configDir, maestriDir, homeDir string) ([]Workspace, error) {
	explicit, err := Load(configDir)
	if err != nil {
		return nil, err
	}
	discovered, err := Discover(maestriDir, homeDir)
	if err != nil {
		// Discovery is best-effort; keep explicit workspaces working even if ~/.maestri is unreadable.
		return explicit, nil
	}
	out := make([]Workspace, len(explicit))
	copy(out, explicit)
	for _, d := range discovered {
		dup := false
		for _, e := range out {
			if (d.Path != "" && e.Path != "" && samePath(d.Path, e.Path)) || d.ID == e.ID && d.Path == "" {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		d.ID = UniqueID(d.ID, out)
		d.Color = normalizeColor(d.Color, len(out))
		d.Tone = ColorTone(d.Color)
		d.Discovered = true
		out = append(out, d)
	}
	return out, nil
}

// Resolve matches an explicit id, name, or path, or infers the deepest workspace containing cwd.
func Resolve(list []Workspace, arg, cwd string) (*Workspace, error) {
	arg = strings.TrimSpace(arg)
	if arg != "" {
		for i := range list {
			if list[i].ID == arg {
				w := list[i]
				return &w, nil
			}
		}
		for i := range list {
			if strings.EqualFold(list[i].Name, arg) {
				w := list[i]
				return &w, nil
			}
		}
		if absArg, err := filepath.Abs(arg); err == nil {
			for i := range list {
				if samePath(list[i].Path, absArg) {
					w := list[i]
					return &w, nil
				}
			}
		}
		return nil, fmt.Errorf("unknown workspace: %q (run `maestria workspace list`)", arg)
	}

	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("cannot resolve workspace from cwd: %w", err)
		}
	}
	canonCwd := canonicalPath(cwd)
	var best *Workspace
	bestLen := -1
	for i := range list {
		if list[i].Path == "" {
			continue
		}
		wp := canonicalPath(list[i].Path)
		if isSameOrChild(canonCwd, wp) && len(wp) > bestLen {
			w := list[i]
			best = &w
			bestLen = len(wp)
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no registered workspace covers %s (pass --workspace <id>)", cwd)
	}
	return best, nil
}

// Slugify produces a lowercase hyphenated identifier.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "workspace"
	}
	return s
}

// UniqueID returns slug or slug-2, slug-3, etc. not present in list.
func UniqueID(raw string, list []Workspace) string {
	base := Slugify(raw)
	used := make(map[string]bool, len(list))
	for _, w := range list {
		used[w.ID] = true
	}
	if !used[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !used[candidate] {
			return candidate
		}
	}
}

// Initials returns up to two uppercase initials for a workspace name.
func Initials(name string) string {
	words := strings.FieldsFunc(strings.TrimSpace(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) == 0 {
		return "WS"
	}
	if len(words) >= 2 {
		r1 := []rune(words[0])
		r2 := []rune(words[1])
		return strings.ToUpper(string([]rune{r1[0], r2[0]}))
	}
	r := []rune(words[0])
	if len(r) == 1 {
		return strings.ToUpper(string(r))
	}
	return strings.ToUpper(string(r[:2]))
}

type rgbTone struct {
	tone string
	r    float64
	g    float64
	b    float64
}

var exactTones = map[string]string{
	"#4f46e5": "blue",
	"#3b82f6": "blue",
	"#0ea5e9": "sky",
	"#14b8a6": "teal",
	"#10b981": "green",
	"#22c55e": "green",
	"#f59e0b": "amber",
	"#f97316": "orange",
	"#ef4444": "rose",
	"#f43f5e": "rose",
	"#ec4899": "pink",
	"#8b5cf6": "violet",
	"#64748b": "slate",
}

var tonePalette = []rgbTone{
	{"blue", 59, 130, 246},
	{"sky", 14, 165, 233},
	{"teal", 20, 184, 166},
	{"green", 16, 185, 129},
	{"amber", 245, 158, 11},
	{"orange", 249, 115, 22},
	{"rose", 239, 68, 68},
	{"pink", 236, 72, 153},
	{"violet", 139, 92, 246},
	{"slate", 100, 116, 139},
}

// ColorTone maps any hex color to the nearest design token tone name so the UI avoids inline styles.
func ColorTone(hex string) string {
	norm := strings.ToLower(strings.TrimSpace(hex))
	if t, ok := exactTones[norm]; ok {
		return t
	}
	if !hexColorRE.MatchString(norm) {
		return "blue"
	}
	r := float64(hexByte(norm[1], norm[2]))
	g := float64(hexByte(norm[3], norm[4]))
	b := float64(hexByte(norm[5], norm[6]))
	best := "blue"
	bestDist := math.MaxFloat64
	for _, p := range tonePalette {
		dr, dg, db := r-p.r, g-p.g, b-p.b
		d := dr*dr + dg*dg + db*db
		if d < bestDist {
			bestDist = d
			best = p.tone
		}
	}
	return best
}

func hexByte(hi, lo byte) byte {
	return hexNibble(hi)<<4 | hexNibble(lo)
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}

func normalizeColor(raw string, idx int) string {
	trimmed := strings.TrimSpace(raw)
	if hexColorRE.MatchString(trimmed) {
		return strings.ToLower(trimmed)
	}
	return DefaultPalette[idx%len(DefaultPalette)]
}

func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

func canonicalPath(p string) string {
	p = cleanPath(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func samePath(a, b string) bool {
	ca, cb := canonicalPath(a), canonicalPath(b)
	return ca != "" && ca == cb
}

func isSameOrChild(candidate, root string) bool {
	if candidate == root {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(candidate, root)
}

// Keep slices referenced if needed by discovery sorting.
var _ = slices.SortFunc[[]Workspace]
