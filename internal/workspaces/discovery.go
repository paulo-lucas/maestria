package workspaces

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/maestria-dev/maestria/internal/config"
)

type rawAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Role      string `json:"role"`
	IsMaestro bool   `json:"isMaestro"`
}

type rawManifestEntry struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Path            string     `json:"path"`
	WorkspacePath   string     `json:"workspacePath"`
	Dir             string     `json:"dir"`
	Root            string     `json:"root"`
	MaestroAgent    string     `json:"maestroAgent"`
	Maestro         string     `json:"maestro"`
	MaestroProvider string     `json:"maestroProvider"`
	Provider        string     `json:"provider"`
	Color           string     `json:"color"`
	Agents          []rawAgent `json:"agents"`
}

// Discover reads Maestri's manifest.json and workspaces/*/workspace.json in strictly read-only mode.
func Discover(maestriDir, homeDir string) ([]Workspace, error) {
	if strings.TrimSpace(maestriDir) == "" {
		return nil, nil
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}

	var entries []rawManifestEntry

	manifestPath := filepath.Join(maestriDir, "manifest.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		entries = append(entries, parseManifest(data)...)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	wsDir := filepath.Join(maestriDir, "workspaces")
	if dirEntries, err := os.ReadDir(wsDir); err == nil {
		sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })
		for _, de := range dirEntries {
			if !de.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(wsDir, de.Name(), "workspace.json"))
			if err != nil {
				continue
			}
			var entry rawManifestEntry
			if json.Unmarshal(raw, &entry) != nil {
				continue
			}
			if entry.ID == "" {
				entry.ID = de.Name()
			}
			entries = append(entries, entry)
		}
	}

	seenPaths := map[string]bool{}
	seenIDs := map[string]bool{}
	out := make([]Workspace, 0, len(entries))
	for i, e := range entries {
		ws, ok := convertDiscovered(e, homeDir, i)
		if !ok {
			continue
		}
		canon := canonicalPath(ws.Path)
		if canon != "" {
			if seenPaths[canon] {
				continue
			}
			seenPaths[canon] = true
		} else if seenIDs[ws.ID] {
			continue
		}
		seenIDs[ws.ID] = true
		out = append(out, ws)
	}
	return out, nil
}

func parseManifest(data []byte) []rawManifestEntry {
	var wrapper struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(data, &wrapper) == nil && len(wrapper.Workspaces) > 0 {
		return parseEntryCollection(wrapper.Workspaces)
	}
	return parseEntryCollection(data)
}

func parseEntryCollection(raw json.RawMessage) []rawManifestEntry {
	var slice []rawManifestEntry
	if json.Unmarshal(raw, &slice) == nil && slice != nil {
		return slice
	}
	var byID map[string]rawManifestEntry
	if json.Unmarshal(raw, &byID) == nil && byID != nil {
		keys := make([]string, 0, len(byID))
		for k := range byID {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]rawManifestEntry, 0, len(keys))
		for _, k := range keys {
			entry := byID[k]
			if entry.ID == "" {
				entry.ID = k
			}
			out = append(out, entry)
		}
		return out
	}
	return nil
}

func convertDiscovered(e rawManifestEntry, homeDir string, idx int) (Workspace, bool) {
	p := firstNonEmpty(e.Path, e.WorkspacePath, e.Dir, e.Root)
	if p != "" && homeDir != "" {
		p = config.ExpandHome(p, homeDir)
	}
	p = cleanPath(p)
	name := strings.TrimSpace(e.Name)
	if name == "" && p != "" {
		name = filepath.Base(p)
	}
	if name == "" {
		name = strings.TrimSpace(e.ID)
	}
	if name == "" && p == "" {
		return Workspace{}, false
	}
	id := Slugify(firstNonEmpty(e.ID, name))
	agent := firstNonEmpty(e.MaestroAgent, e.Maestro)
	provider := firstNonEmpty(e.MaestroProvider, e.Provider)
	for _, a := range e.Agents {
		if a.IsMaestro || strings.EqualFold(a.Role, "maestro") || strings.EqualFold(a.Name, "maestro") || strings.EqualFold(a.ID, "maestro") {
			if agent == "" {
				agent = firstNonEmpty(a.Name, a.ID)
			}
			if provider == "" {
				provider = strings.TrimSpace(a.Provider)
			}
			break
		}
	}
	if agent == "" {
		agent = "Maestro"
	}
	color := normalizeColor(e.Color, idx)
	return Workspace{
		ID:              id,
		Name:            name,
		Path:            p,
		MaestroAgent:    agent,
		MaestroProvider: provider,
		Color:           color,
		Tone:            ColorTone(color),
		Discovered:      true,
	}, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
