package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

func cmdWorkspace(args []string, env *Env) error {
	if len(args) == 0 {
		return cmdWorkspaceList(nil, env)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return cmdWorkspaceList(rest, env)
	case "add":
		return cmdWorkspaceAdd(rest, env)
	case "remove", "rm":
		return cmdWorkspaceRemove(rest, env)
	default:
		if strings.HasPrefix(sub, "-") {
			return cmdWorkspaceList(args, env)
		}
		return usagef("Unknown workspace subcommand: %s\n\nUsage: maestria workspace <list|add|remove> [options]", sub)
	}
}

func cmdWorkspaceList(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	list, _, err := loadMergedWorkspaces(env)
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{"workspaces": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(env.Stdout, "No workspaces registered or discovered.")
		return nil
	}
	for _, w := range list {
		tag := ""
		if w.Discovered {
			tag = " [discovered]"
		}
		prov := ""
		if w.MaestroProvider != "" {
			prov = ", provider: " + w.MaestroProvider
		}
		fmt.Fprintf(env.Stdout, "%s  %s  (%s, agent: %s%s)%s\n", w.ID, w.Name, w.Path, w.MaestroAgent, prov, tag)
	}
	return nil
}

func cmdWorkspaceAdd(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools: map[string]bool{"json": true},
		Strings: map[string]bool{
			"id": true, "name": true, "path": true,
			"agent": true, "maestro": true, "provider": true, "color": true, "roadmap": true,
		},
	})
	if err != nil {
		return usagef("%v", err)
	}
	name := parsed.String("name")
	path := parsed.String("path")
	switch {
	case len(parsed.Pos) >= 2 && name == "" && path == "":
		// V1 positional contract: maestria workspace add <name> <path>
		name = parsed.Pos[0]
		path = parsed.Pos[1]
	case len(parsed.Pos) >= 1 && path == "":
		path = parsed.Pos[0]
	}
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	agent := parsed.String("agent")
	if agent == "" {
		agent = parsed.String("maestro")
	}
	ws, err := workspaces.Add(configDir(env), workspaces.AddInput{
		ID:              parsed.String("id"),
		Name:            name,
		Path:            path,
		MaestroAgent:    agent,
		MaestroProvider: parsed.String("provider"),
		RoadmapPath:     parsed.String("roadmap"),
		Color:           parsed.String("color"),
	})
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, ws)
	}
	fmt.Fprintf(env.Stdout, "Added workspace %s (%s) at %s\n", ws.ID, ws.Name, ws.Path)
	return nil
}

func cmdWorkspaceRemove(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"id": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	target := strings.TrimSpace(parsed.String("id"))
	if target == "" && len(parsed.Pos) > 0 {
		target = parsed.Pos[0]
	}
	if target == "" {
		return usagef("Usage: maestria workspace remove <id-or-name-or-path>")
	}
	removed, err := workspaces.Remove(configDir(env), target)
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, removed)
	}
	fmt.Fprintf(env.Stdout, "Removed workspace %s (%s)\n", removed.ID, removed.Name)
	return nil
}

func loadMergedWorkspaces(env *Env) ([]workspaces.Workspace, *config.Config, error) {
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return nil, nil, err
	}
	list, err := workspaces.Merged(dir, cfg.MaestriDir, env.Config.Home)
	if err != nil {
		return nil, nil, err
	}
	return list, cfg, nil
}

func resolveWorkspaceCLI(env *Env, explicit string) (*workspaces.Workspace, []workspaces.Workspace, *config.Config, error) {
	list, cfg, err := loadMergedWorkspaces(env)
	if err != nil {
		return nil, nil, nil, err
	}
	ws, err := workspaces.Resolve(list, explicit, "")
	if err != nil {
		return nil, list, cfg, err
	}
	return ws, list, cfg, nil
}

func writeJSONLine(env *Env, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "%s\n", b)
	return err
}
