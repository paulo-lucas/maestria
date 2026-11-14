package wire

import (
	"path/filepath"
	"runtime"
	"strings"
)

// WorkspaceRef is a Maestria workspace to look for in Maestri.
type WorkspaceRef struct {
	ID           string
	Name         string
	Path         string
	MaestroAgent string
}

// MapWorkspaces pairs Maestria workspaces with Maestri workspaces by working directory first and
// by name (case-insensitive, only when unambiguous) as a fallback. Each Maestri workspace maps at
// most once. The result is keyed by the Maestria workspace id.
func MapWorkspaces(refs []WorkspaceRef, metas []WorkspaceMeta) map[string]WorkspaceMeta {
	out := map[string]WorkspaceMeta{}
	used := map[string]bool{}
	for _, ref := range refs {
		if ref.Path == "" {
			continue
		}
		want := normPath(ref.Path)
		for _, m := range metas {
			if used[m.ID] || m.WorkingDirectory == "" {
				continue
			}
			if pathsEqual(want, normPath(m.WorkingDirectory)) {
				out[ref.ID] = m
				used[m.ID] = true
				break
			}
		}
	}
	for _, ref := range refs {
		if _, ok := out[ref.ID]; ok || strings.TrimSpace(ref.Name) == "" {
			continue
		}
		var found []WorkspaceMeta
		for _, m := range metas {
			if !used[m.ID] && strings.EqualFold(strings.TrimSpace(m.Name), strings.TrimSpace(ref.Name)) {
				found = append(found, m)
			}
		}
		if len(found) == 1 {
			out[ref.ID] = found[0]
			used[found[0].ID] = true
		}
	}
	return out
}

func normPath(p string) string {
	p = filepath.Clean(strings.TrimSpace(p))
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		p = rp
	}
	return p
}

func pathsEqual(a, b string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// PickMaestro finds the workspace's Maestro terminal in a feed: the terminal named like the
// workspace's Maestro agent, else a terminal Maestri marks as manager. Ground wins over floors.
func PickMaestro(f *FeedSnapshot, agentName string) (FeedTerminal, bool) {
	var named, manager []FeedTerminal
	for _, t := range f.Terminals() {
		if agentName != "" && strings.EqualFold(strings.TrimSpace(t.Name), strings.TrimSpace(agentName)) {
			named = append(named, t)
		} else if t.IsManager {
			manager = append(manager, t)
		}
	}
	for _, list := range [][]FeedTerminal{named, manager} {
		for _, t := range list {
			if t.FloorID == "" {
				return t, true
			}
		}
		if len(list) > 0 {
			return list[0], true
		}
	}
	return FeedTerminal{}, false
}
