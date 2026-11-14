package cli

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/service"
	"github.com/maestria-dev/maestria/internal/wire"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

//go:embed skill/SKILL.md
var embeddedSkillMD []byte

const initUsage = "Usage: maestria init [--maestri-dir <path>] [--workspaces all|none|id1,id2] [--standby default|none] [--skill claude,antigravity|both|none] [--service install|start|none] [--service-mode serve|connect] [--skip-auth] [--skip-wire] [--yes]"

var providerIDRE = regexp.MustCompile(`(?i)^[a-z0-9*][a-z0-9._:-]{0,63}$`)

func (e *Env) isTTY() bool {
	if e.IsTTY != nil {
		return e.IsTTY()
	}
	if f, ok := e.Stdin.(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

func cmdInit(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools: map[string]bool{
			"yes":             true,
			"non-interactive": true,
			"skip-auth":       true,
			"skip-wire":       true,
			"help":            true,
		},
		Strings: map[string]bool{
			"maestri-dir":   true,
			"workspaces":    true,
			"standby":       true,
			"skill":         true,
			"install-skill": true,
			"service":       true,
			"service-mode":  true,
		},
		Shorts: map[string]string{"y": "yes", "h": "help"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if parsed.Bool("help") {
		fmt.Fprintln(env.Stdout, initUsage)
		return nil
	}
	if len(parsed.Pos) > 0 {
		return usagef(initUsage)
	}
	if parsed.HasString("skill") && parsed.HasString("install-skill") && parsed.String("skill") != parsed.String("install-skill") {
		return usagef("Pass either --skill or --install-skill, not both with different values.")
	}
	svcMode, err := service.ValidateMode(parsed.String("service-mode"))
	if err != nil {
		return usagef("%v", err)
	}

	nonInteractive := parsed.Bool("yes") || parsed.Bool("non-interactive") || !env.isTTY()
	in := bufio.NewReader(env.Stdin)

	dir := configDir(env)
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Using config directory: %s\n", dir)

	maestriDir, err := initMaestriDirStep(in, dir, parsed.String("maestri-dir"), parsed.HasString("maestri-dir"), nonInteractive, env)
	if err != nil {
		return err
	}

	if err := initWorkspacesStep(in, dir, maestriDir, parsed.String("workspaces"), parsed.HasString("workspaces"), nonInteractive, env); err != nil {
		return err
	}

	if err := initStandbyStep(in, dir, parsed.String("standby"), parsed.HasString("standby"), nonInteractive, env); err != nil {
		return err
	}

	if err := initAuthStep(in, dir, parsed.Bool("skip-auth"), nonInteractive, env); err != nil {
		return err
	}

	if err := initWireStep(in, dir, parsed.Bool("skip-wire"), nonInteractive, env); err != nil {
		return err
	}

	rawSkill := parsed.String("skill")
	hasSkill := parsed.HasString("skill")
	if !hasSkill && parsed.HasString("install-skill") {
		rawSkill = parsed.String("install-skill")
		hasSkill = true
	}
	if err := initSkillStep(in, rawSkill, hasSkill, nonInteractive, env); err != nil {
		return err
	}

	svcResult, err := initServiceStep(in, parsed.String("service"), svcMode, parsed.HasString("service"), nonInteractive, env)
	if err != nil {
		return err
	}

	printInitNextSteps(env.Stdout, svcResult, env.Config.GOOS)
	return nil
}

func initMaestriDirStep(in *bufio.Reader, dir, cliDir string, hasCLIDir, nonInteractive bool, env *Env) (string, error) {
	top, hasConfigFile, err := config.RawKeys(dir)
	if err != nil {
		return "", err
	}
	var configured string
	if raw, ok := top["maestriDir"]; ok {
		_ = json.Unmarshal(raw, &configured)
	}
	envDefault, err := config.ResolveMaestriDir("", env.Config)
	if err != nil {
		return "", err
	}

	if hasCLIDir {
		resolved, err := config.ResolveMaestriDir(cliDir, env.Config)
		if err != nil {
			return "", err
		}
		st := config.InspectMaestriDir(resolved)
		if nonInteractive && !st.HasProvidersDir {
			return "", fmt.Errorf("Maestri directory %q must contain usage/providers (enable Settings -> Agents -> Usage in Maestri)", resolved)
		}
		if err := config.SaveMaestriDir(dir, resolved, true, env.Config); err != nil {
			return "", err
		}
		printMaestriDirStatus(env.Stdout, st)
		return resolved, nil
	}

	if hasConfigFile {
		label := envDefault + " (default)"
		if configured != "" {
			label = configured
		}
		resolved, err := config.ResolveMaestriDir(configured, env.Config)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(env.Stdout, "Existing configuration found at %s (maestriDir: %s).\n", config.FilePath(dir), label)
		if nonInteractive {
			printMaestriDirStatus(env.Stdout, config.InspectMaestriDir(resolved))
			return resolved, nil
		}
		choice, err := askChoice(in, env, "Configuration:", []string{
			fmt.Sprintf("Keep existing config (%s)", label),
			"Change Maestri data directory",
		}, 0)
		if err != nil {
			return "", err
		}
		if choice == 0 {
			printMaestriDirStatus(env.Stdout, config.InspectMaestriDir(resolved))
			return resolved, nil
		}
	} else if nonInteractive {
		if err := config.SaveMaestriDir(dir, envDefault, false, env.Config); err != nil {
			return "", err
		}
		printMaestriDirStatus(env.Stdout, config.InspectMaestriDir(envDefault))
		return envDefault, nil
	}

	for {
		picked, custom, err := promptMaestriDir(in, envDefault, env)
		if err != nil {
			return "", err
		}
		st := config.InspectMaestriDir(picked)
		printMaestriDirStatus(env.Stdout, st)
		if !st.HasProvidersDir {
			retry, err := askConfirm(in, env, "usage/providers not found there. Enter a different Maestri path?", false)
			if err != nil {
				return "", err
			}
			if retry {
				continue
			}
		}
		if err := config.SaveMaestriDir(dir, picked, custom, env.Config); err != nil {
			return "", err
		}
		return picked, nil
	}
}

func promptMaestriDir(in *bufio.Reader, envDefault string, env *Env) (string, bool, error) {
	choice, err := askChoice(in, env, "Maestri data directory:", []string{
		fmt.Sprintf("Use default (%s)", envDefault),
		"Enter a custom path",
	}, 0)
	if err != nil {
		return "", false, err
	}
	if choice == 0 {
		return envDefault, false, nil
	}
	for {
		raw, err := env.readLine(in, "Custom Maestri data directory path: ")
		if err != nil {
			return "", false, err
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			fmt.Fprintln(env.Stdout, "Please enter a path (e.g. ~/.maestri or /path/to/.maestri).")
			continue
		}
		resolved, err := config.ResolveMaestriDir(trimmed, env.Config)
		if err != nil {
			fmt.Fprintf(env.Stdout, "%v\n", err)
			continue
		}
		return resolved, resolved != envDefault, nil
	}
}

func printMaestriDirStatus(out io.Writer, st config.MaestriDirStatus) {
	switch {
	case !st.HasDir:
		fmt.Fprintf(out, "Warning: Maestri directory not found at %q. Set \"maestriDir\" in config.json or MAESTRI_HOME if Maestri lives elsewhere.\n", st.Dir)
	case !st.HasProvidersDir:
		fmt.Fprintf(out, "Warning: %q not found (Agent Usage may be off in Maestri Settings -> Agents -> Usage).\n", st.ProvidersDir)
	case !st.HasStatusFile:
		fmt.Fprintf(out, "Note: %q found, waiting for Maestri to write .status.json while usage rings are active.\n", st.ProvidersDir)
	default:
		fmt.Fprintf(out, "Verified Maestri usage providers at %s\n", st.ProvidersDir)
	}
}

func initWorkspacesStep(in *bufio.Reader, dir, maestriDir, flagWS string, hasFlagWS, nonInteractive bool, env *Env) error {
	discovered, err := workspaces.Discover(maestriDir, env.Config.Home)
	if err != nil {
		return err
	}
	existing, err := workspaces.Load(dir)
	if err != nil {
		return err
	}
	existingPaths := map[string]bool{}
	for _, w := range existing {
		if w.Path != "" {
			existingPaths[canonicalPathKey(w.Path, env.Config.GOOS)] = true
		}
	}
	var newOnes []workspaces.Workspace
	for _, d := range discovered {
		if d.Path != "" && existingPaths[canonicalPathKey(d.Path, env.Config.GOOS)] {
			continue
		}
		newOnes = append(newOnes, d)
	}

	if hasFlagWS {
		picked, err := selectWorkspacesByFlag(flagWS, newOnes)
		if err != nil {
			return err
		}
		if len(picked) == 0 {
			fmt.Fprintln(env.Stdout, "Skipped adding workspaces (--workspaces none).")
			return nil
		}
		merged := mergeWorkspaces(existing, picked)
		if err := workspaces.Save(dir, merged); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Saved %d workspace(s) to workspaces.json.\n", len(merged))
		return nil
	}

	if len(newOnes) > 0 {
		fmt.Fprintf(env.Stdout, "Detected %d new Maestri workspace(s):\n", len(newOnes))
		for i, w := range newOnes {
			extra := ""
			if w.RoadmapPath != "" {
				extra = fmt.Sprintf(" [roadmap: %s]", w.RoadmapPath)
			}
			fmt.Fprintf(env.Stdout, "  %d) %s (%s)%s\n", i+1, w.Name, w.Path, extra)
		}
		var picked []workspaces.Workspace
		if nonInteractive {
			picked = newOnes
		} else {
			idxs, err := askMultiPick(in, env, "Select workspaces to add (numbers like 1,2, 'all', or 'none')", len(newOnes), true)
			if err != nil {
				return err
			}
			for _, idx := range idxs {
				picked = append(picked, newOnes[idx])
			}
		}
		if len(picked) > 0 {
			merged := mergeWorkspaces(existing, picked)
			if err := workspaces.Save(dir, merged); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "Saved %d workspace(s) to workspaces.json.\n", len(merged))
		} else {
			fmt.Fprintln(env.Stdout, "Skipped adding newly detected workspaces.")
		}
		return nil
	}

	if len(existing) == 0 {
		fmt.Fprintln(env.Stdout, `No Maestri workspaces found automatically. You can add them later with "maestria workspace add".`)
		return nil
	}

	fmt.Fprintf(env.Stdout, "Existing workspaces.json has %d configured workspace(s).\n", len(existing))
	if !nonInteractive && len(discovered) > 0 {
		choice, err := askChoice(in, env, "Workspaces:", []string{
			fmt.Sprintf("Keep existing workspaces (%d)", len(existing)),
			fmt.Sprintf("Replace from discovery (%d)", len(discovered)),
		}, 0)
		if err != nil {
			return err
		}
		if choice == 1 {
			for i, w := range discovered {
				fmt.Fprintf(env.Stdout, "  %d) %s (%s)\n", i+1, w.Name, w.Path)
			}
			idxs, err := askMultiPick(in, env, "Select workspaces to keep (numbers like 1,2, 'all', or 'none')", len(discovered), true)
			if err != nil {
				return err
			}
			var picked []workspaces.Workspace
			for _, idx := range idxs {
				picked = append(picked, discovered[idx])
			}
			merged := mergeWorkspaces(nil, picked)
			if err := workspaces.Save(dir, merged); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "Replaced workspaces.json with %d workspace(s).\n", len(merged))
		}
	}
	return nil
}

func selectWorkspacesByFlag(raw string, candidates []workspaces.Workspace) ([]workspaces.Workspace, error) {
	trimmed := strings.TrimSpace(raw)
	switch strings.ToLower(trimmed) {
	case "", "none":
		return nil, nil
	case "all":
		return candidates, nil
	}
	var out []workspaces.Workspace
	seen := map[string]bool{}
	for _, tok := range strings.Split(trimmed, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil, usagef("invalid --workspaces value %q", raw)
		}
		var matched *workspaces.Workspace
		for i := range candidates {
			if candidates[i].ID == tok || strings.EqualFold(candidates[i].Name, tok) {
				matched = &candidates[i]
				break
			}
		}
		if matched == nil {
			return nil, fmt.Errorf("unknown discovered workspace %q", tok)
		}
		if !seen[matched.ID] {
			seen[matched.ID] = true
			out = append(out, *matched)
		}
	}
	return out, nil
}

func mergeWorkspaces(existing, additions []workspaces.Workspace) []workspaces.Workspace {
	out := make([]workspaces.Workspace, len(existing))
	copy(out, existing)
	for _, w := range additions {
		w.ID = workspaces.UniqueID(w.ID, out)
		w.Discovered = false
		out = append(out, w)
	}
	return out
}

func canonicalPathKey(p, goos string) string {
	cleaned := filepath.Clean(p)
	if abs, err := filepath.Abs(cleaned); err == nil {
		cleaned = abs
	}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	}
	if goos == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func initStandbyStep(in *bufio.Reader, dir, flagStandby string, hasFlagStandby, nonInteractive bool, env *Env) error {
	defaults, err := config.Defaults(dir, env.Config)
	if err != nil {
		return err
	}
	top, _, err := config.RawKeys(dir)
	if err != nil {
		return err
	}
	_, hasStandbyKey := top["standby"]

	if hasFlagStandby {
		switch strings.ToLower(strings.TrimSpace(flagStandby)) {
		case "default":
			if err := config.SaveStandby(dir, defaults.Standby, env.Config); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "Standby thresholds set to defaults (%s).\n", formatStandbySummary(defaults.Standby))
			return nil
		case "none", "disable", "off":
			sb := defaults.Standby
			sb.Thresholds = map[string]map[string]int{}
			sb.ResumeBelowPercent = nil
			if err := config.SaveStandby(dir, sb, env.Config); err != nil {
				return err
			}
			fmt.Fprintln(env.Stdout, "Usage standby disabled (--standby none).")
			return nil
		default:
			return usagef(`--standby must be "default" or "none"`)
		}
	}

	if hasStandbyKey {
		cfg, err := config.Load(dir, env.Config)
		if err != nil {
			return err
		}
		summary := formatStandbySummary(cfg.Standby)
		fmt.Fprintf(env.Stdout, "Existing standby thresholds (%s).\n", summary)
		if nonInteractive {
			return nil
		}
		choice, err := askChoice(in, env, "Standby thresholds:", []string{
			fmt.Sprintf("Keep existing standby (%s)", summary),
			"Change standby thresholds",
		}, 0)
		if err != nil {
			return err
		}
		if choice == 0 {
			return nil
		}
	} else if nonInteractive {
		if err := config.SaveStandby(dir, defaults.Standby, env.Config); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Configured default standby thresholds (%s).\n", formatStandbySummary(defaults.Standby))
		return nil
	}

	mode, err := askChoice(in, env, "Usage standby thresholds (auto-pause agents near quota limits):", []string{
		"Use defaults (claude: session 95%, weekly 90%; resume on reset or below 80%)",
		"Customize provider/window thresholds",
		"Disable automatic standby",
	}, 0)
	if err != nil {
		return err
	}
	switch mode {
	case 0:
		if err := config.SaveStandby(dir, defaults.Standby, env.Config); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Standby thresholds confirmed (%s).\n", formatStandbySummary(defaults.Standby))
		return nil
	case 2:
		sb := defaults.Standby
		sb.Thresholds = map[string]map[string]int{}
		sb.ResumeBelowPercent = nil
		if err := config.SaveStandby(dir, sb, env.Config); err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, "Usage standby disabled.")
		return nil
	}

	var provider string
	for {
		raw, err := env.readLine(in, "Provider ID to guard [claude]: ")
		if err != nil {
			return err
		}
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			p = "claude"
		}
		if !providerIDRE.MatchString(p) {
			fmt.Fprintln(env.Stdout, "Enter a valid provider ID (letters, digits, ., _, :, -).")
			continue
		}
		provider = p
		break
	}
	sessionPct, err := askPercentOrNone(in, env, fmt.Sprintf(`Session window limit %% for %s (1-100, or "none")`, provider), "95", 1, 100)
	if err != nil {
		return err
	}
	weeklyPct, err := askPercentOrNone(in, env, fmt.Sprintf(`Weekly window limit %% for %s (1-100, or "none")`, provider), "90", 1, 100)
	if err != nil {
		return err
	}
	resumePct, err := askPercentOrNone(in, env, `Resume below % before reset (0-99, or "none" for reset-only)`, "80", 0, 99)
	if err != nil {
		return err
	}

	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return err
	}
	sb := cfg.Standby
	if sb.Thresholds == nil {
		sb.Thresholds = map[string]map[string]int{}
	}
	rules := map[string]int{}
	if sessionPct != nil {
		rules["session"] = *sessionPct
	}
	if weeklyPct != nil {
		rules["weekly"] = *weeklyPct
	}
	if len(rules) > 0 {
		sb.Thresholds[provider] = rules
	} else {
		delete(sb.Thresholds, provider)
	}
	sb.ResumeBelowPercent = resumePct
	if err := config.SaveStandby(dir, sb, env.Config); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Standby thresholds saved (%s).\n", formatStandbySummary(sb))
	return nil
}

func formatStandbySummary(sb config.Standby) string {
	var parts []string
	for prov, wins := range sb.Thresholds {
		if len(wins) == 0 {
			continue
		}
		var wp []string
		for w, pct := range wins {
			wp = append(wp, fmt.Sprintf("%s %d%%", w, pct))
		}
		parts = append(parts, fmt.Sprintf("%s: %s", prov, strings.Join(wp, ", ")))
	}
	if len(parts) == 0 {
		return "disabled"
	}
	resumeText := "resume on reset or below threshold"
	if sb.ResumeBelowPercent != nil {
		resumeText = fmt.Sprintf("resume on reset or below %d%%", *sb.ResumeBelowPercent)
	}
	return strings.Join(parts, " | ") + "; " + resumeText
}

func askPercentOrNone(in *bufio.Reader, env *Env, label, def string, min, max int) (*int, error) {
	for {
		raw, err := env.readLine(in, fmt.Sprintf("%s [%s]: ", label, def))
		if err != nil {
			return nil, err
		}
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			t = def
		}
		if t == "none" || t == "off" {
			return nil, nil
		}
		n, err := strconv.Atoi(t)
		if err != nil || n < min || n > max {
			fmt.Fprintf(env.Stdout, "Enter a whole number between %d and %d, or \"none\".\n", min, max)
			continue
		}
		return &n, nil
	}
}

func initAuthStep(in *bufio.Reader, dir string, skipAuth, nonInteractive bool, env *Env) error {
	if skipAuth {
		fmt.Fprintln(env.Stdout, "Skipping auth setup (--skip-auth).")
		return nil
	}
	sec, err := auth.LoadSecrets(dir)
	if err != nil {
		return err
	}
	if sec != nil {
		fmt.Fprintf(env.Stdout, "Existing credentials found (%s).\n", auth.SecretsPath(dir))
		if nonInteractive {
			fmt.Fprintln(env.Stdout, "Keeping existing credentials.")
			return nil
		}
		choice, err := askChoice(in, env, "Credentials:", []string{
			"Keep existing credentials",
			"Reset password & TOTP",
		}, 0)
		if err != nil {
			return err
		}
		if choice == 0 {
			return nil
		}
		return runAuthSetup(in, true, env)
	}
	if nonInteractive {
		fmt.Fprintln(env.Stdout, `No credentials configured yet; run "maestria auth setup" interactively to set password & TOTP.`)
		return nil
	}
	return runAuthSetup(in, false, env)
}

func initWireStep(in *bufio.Reader, dir string, skipWire, nonInteractive bool, env *Env) error {
	creds, err := wire.LoadCredentials(wire.Path(dir))
	if err == nil && creds != nil {
		fmt.Fprintf(env.Stdout, "Maestri Wire is already paired (%s).\n", creds.Host)
		if nonInteractive || skipWire {
			return nil
		}
		choice, err := askChoice(in, env, "Maestri Wire:", []string{
			fmt.Sprintf("Keep existing Wire pairing (%s)", creds.Host),
			"Re-pair with Maestri Wire",
		}, 0)
		if err != nil {
			return err
		}
		if choice == 0 {
			return nil
		}
		if err := runWirePair(in, "", "", false, false, env); err != nil {
			fmt.Fprintf(env.Stdout, "Warning: Wire pairing failed (%v). You can run \"maestria wire pair\" later.\n", err)
		}
		return nil
	}
	if err != nil && !errors.Is(err, wire.ErrNotPaired) {
		fmt.Fprintf(env.Stdout, "Note: existing wire.json is unreadable (%v).\n", err)
	}
	if skipWire || nonInteractive {
		fmt.Fprintln(env.Stdout, `Skipped Maestri Wire pairing (run "maestria wire pair" anytime).`)
		return nil
	}
	ok, err := askConfirm(in, env, "Pair with Maestri Wire now (live agents, chat, floors)?", false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(env.Stdout, `Skipped Maestri Wire pairing (run "maestria wire pair" anytime).`)
		return nil
	}
	if err := runWirePair(in, "", "", false, false, env); err != nil {
		fmt.Fprintf(env.Stdout, "Warning: Wire pairing failed (%v). You can run \"maestria wire pair\" later.\n", err)
	}
	return nil
}

// ClaudeSkillPath returns the Claude Code skill destination path under homeDir.
func ClaudeSkillPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "skills", "maestria", "SKILL.md")
}

// AntigravitySkillDir returns the Antigravity skill directory under homeDir.
func AntigravitySkillDir(homeDir string) string {
	cfgSkills := filepath.Join(homeDir, ".gemini", "config", "skills")
	if info, err := os.Stat(cfgSkills); err == nil && info.IsDir() {
		return filepath.Join(cfgSkills, "maestria")
	}
	return filepath.Join(homeDir, ".gemini", "antigravity", "skills", "maestria")
}

// AntigravitySkillPath returns the Antigravity skill destination path under homeDir.
func AntigravitySkillPath(homeDir string) string {
	return filepath.Join(AntigravitySkillDir(homeDir), "SKILL.md")
}

// ParseSkillSelection parses a comma-separated skill selection ("claude", "antigravity", "both", "all", "none").
func ParseSkillSelection(raw string) (claude, antigravity, none bool, err error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return false, false, false, usagef(`--skill must be "none" or a comma-separated combination of: claude, antigravity (or "both")`)
	}
	for _, tok := range strings.Split(trimmed, ",") {
		t := strings.TrimSpace(tok)
		switch t {
		case "":
			return false, false, false, usagef(`--skill must be "none" or a comma-separated combination of: claude, antigravity (or "both")`)
		case "claude":
			claude = true
		case "antigravity":
			antigravity = true
		case "both", "all":
			claude = true
			antigravity = true
		case "none":
			none = true
		default:
			return false, false, false, usagef(`--skill must be "none" or a comma-separated combination of: claude, antigravity (or "both")`)
		}
	}
	if none && (claude || antigravity) {
		return false, false, false, usagef(`--skill cannot combine "none" with agent targets`)
	}
	return claude, antigravity, none, nil
}

// InstallMaestroSkill writes the embedded SKILL.md to the selected agent directories under homeDir.
func InstallMaestroSkill(claude, antigravity bool, homeDir string) ([]string, error) {
	var installed []string
	writeOne := func(dest string) error {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, embeddedSkillMD, 0o644); err != nil {
			return err
		}
		installed = append(installed, dest)
		return nil
	}
	if claude {
		if err := writeOne(ClaudeSkillPath(homeDir)); err != nil {
			return nil, err
		}
	}
	if antigravity {
		if err := writeOne(AntigravitySkillPath(homeDir)); err != nil {
			return nil, err
		}
	}
	return installed, nil
}

func initSkillStep(in *bufio.Reader, flagSkill string, hasFlagSkill, nonInteractive bool, env *Env) error {
	homeDir := env.Config.Home
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}

	if hasFlagSkill {
		claude, ag, none, err := ParseSkillSelection(flagSkill)
		if err != nil {
			return err
		}
		if none {
			fmt.Fprintln(env.Stdout, "Skipped Maestro protocol skill installation (--skill none).")
			return nil
		}
		installed, err := InstallMaestroSkill(claude, ag, homeDir)
		if err != nil {
			return err
		}
		for _, dest := range installed {
			fmt.Fprintf(env.Stdout, "Installed Maestro skill: %s\n", dest)
		}
		return nil
	}

	var existing []string
	if _, err := os.Stat(ClaudeSkillPath(homeDir)); err == nil {
		existing = append(existing, "claude: "+ClaudeSkillPath(homeDir))
	}
	if _, err := os.Stat(AntigravitySkillPath(homeDir)); err == nil {
		existing = append(existing, "antigravity: "+AntigravitySkillPath(homeDir))
	}

	if len(existing) > 0 {
		fmt.Fprintf(env.Stdout, "Existing Maestro skill(s) found (%s).\n", strings.Join(existing, ", "))
		if nonInteractive {
			fmt.Fprintln(env.Stdout, "Keeping existing Maestro skill(s).")
			return nil
		}
		choice, err := askChoice(in, env, "Maestro protocol skill:", []string{
			"Keep existing skill(s)",
			"Re-install / change target agents",
		}, 0)
		if err != nil {
			return err
		}
		if choice == 0 {
			return nil
		}
	} else if nonInteractive {
		fmt.Fprintln(env.Stdout, "Skipping skill installation in non-interactive mode (pass --skill claude,antigravity to install).")
		return nil
	}

	choice, err := askChoice(in, env, "Install Maestro protocol skill for:", []string{
		fmt.Sprintf("Both Claude Code and Antigravity (%s, %s)", ClaudeSkillPath(homeDir), AntigravitySkillPath(homeDir)),
		fmt.Sprintf("Claude Code only (%s)", ClaudeSkillPath(homeDir)),
		fmt.Sprintf("Antigravity only (%s)", AntigravitySkillPath(homeDir)),
		"Skip skill installation",
	}, 0)
	if err != nil {
		return err
	}
	var claude, ag bool
	switch choice {
	case 0:
		claude, ag = true, true
	case 1:
		claude = true
	case 2:
		ag = true
	case 3:
		fmt.Fprintln(env.Stdout, "Skipped Maestro protocol skill installation.")
		return nil
	}
	installed, err := InstallMaestroSkill(claude, ag, homeDir)
	if err != nil {
		return err
	}
	for _, dest := range installed {
		fmt.Fprintf(env.Stdout, "Installed Maestro skill: %s\n", dest)
	}
	return nil
}

func initServiceStep(in *bufio.Reader, flagService, mode string, hasFlagService, nonInteractive bool, env *Env) (string, error) {
	action := "skip"
	if hasFlagService {
		switch strings.ToLower(strings.TrimSpace(flagService)) {
		case "start":
			action = "start"
		case "install":
			action = "install"
		case "none", "skip":
			action = "skip"
		default:
			return "", usagef("--service must be one of: install, start, none")
		}
	} else if !service.IsSupported(env.Config.GOOS) || nonInteractive {
		return "skipped", nil
	} else {
		opts := env.serviceOpts(mode)
		installed := service.IsInstalled(opts)
		sec, _ := auth.LoadSecrets(configDir(env))
		if installed {
			choice, err := askChoice(in, env, "Background service (already installed):", []string{
				"Keep existing service configuration",
				"Reinstall and restart background service",
			}, 0)
			if err != nil {
				return "", err
			}
			if choice == 1 {
				action = "start"
			}
		} else {
			defIdx := 2
			if sec != nil {
				defIdx = 0
			}
			choice, err := askChoice(in, env, "Keep Maestria running in the background?", []string{
				"Install and start background service now (recommended)",
				"Install service now, start later",
				`Skip for now (start manually with "maestria serve")`,
			}, defIdx)
			if err != nil {
				return "", err
			}
			switch choice {
			case 0:
				action = "start"
			case 1:
				action = "install"
			default:
				action = "skip"
			}
		}
	}

	if action == "skip" {
		return "skipped", nil
	}

	opts := env.serviceOpts(mode)
	wasInstalled := service.IsInstalled(opts)
	res, err := service.Install(context.Background(), opts)
	if err != nil {
		var se *service.Error
		if errors.As(err, &se) {
			fmt.Fprintf(env.Stdout, "Warning: could not set up background service (%v).\n", err)
			return "skipped", nil
		}
		return "", err
	}
	fmt.Fprintf(env.Stdout, "Installed Maestria service (%s): %s\n", res.Mode, res.UnitPath)
	fmt.Fprintf(env.Stdout, "Service logs: %s\n", res.LogDir)
	if action == "start" {
		if wasInstalled {
			err = service.Restart(context.Background(), opts)
		} else {
			err = service.Start(context.Background(), opts)
		}
		if err != nil {
			var se *service.Error
			if errors.As(err, &se) {
				fmt.Fprintf(env.Stdout, "Warning: service installed but failed to start (%v).\n", err)
				return "installed", nil
			}
			return "", err
		}
		fmt.Fprintln(env.Stdout, "Started Maestria background service.")
		return "started", nil
	}
	return "installed", nil
}

func printInitNextSteps(out io.Writer, serviceResult, goos string) {
	fmt.Fprintln(out, "\nSetup complete. Next steps:")
	switch {
	case serviceResult == "started":
		fmt.Fprintln(out, "  Panel service is running.")
		fmt.Fprintln(out, "  Logs:    maestria service logs")
		fmt.Fprintln(out, "  Restart: maestria service restart")
		fmt.Fprintln(out, "  Status:  maestria status")
	case serviceResult == "installed":
		fmt.Fprintln(out, "  Start:   maestria service start")
		fmt.Fprintln(out, "  Logs:    maestria service logs")
		fmt.Fprintln(out, "  Status:  maestria status")
	case service.IsSupported(goos):
		fmt.Fprintln(out, "  1. Start now in foreground: maestria serve")
		fmt.Fprintln(out, "     Or install background service: maestria service install && maestria service start")
	default:
		fmt.Fprintln(out, "  1. Start the panel locally: maestria serve")
	}
	fmt.Fprintln(out, "  2. Optionally expose securely via Cloudflare Tunnel + Access (see README.md).")
}

func askConfirm(in *bufio.Reader, env *Env, prompt string, defaultYes bool) (bool, error) {
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	for {
		raw, err := env.readLine(in, fmt.Sprintf("%s %s: ", prompt, hint))
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(env.Stdout, `Please answer "y" or "n".`)
		}
	}
}

func askChoice(in *bufio.Reader, env *Env, title string, options []string, defaultIdx int) (int, error) {
	fmt.Fprintln(env.Stdout, title)
	for i, opt := range options {
		mark := ""
		if i == defaultIdx {
			mark = " (default)"
		}
		fmt.Fprintf(env.Stdout, "  %d) %s%s\n", i+1, opt, mark)
	}
	for {
		raw, err := env.readLine(in, fmt.Sprintf("Choice [%d]: ", defaultIdx+1))
		if err != nil {
			return 0, err
		}
		t := strings.TrimSpace(raw)
		if t == "" {
			return defaultIdx, nil
		}
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || n > len(options) {
			fmt.Fprintf(env.Stdout, "Enter a number between 1 and %d.\n", len(options))
			continue
		}
		return n - 1, nil
	}
}

func askMultiPick(in *bufio.Reader, env *Env, prompt string, total int, defaultAll bool) ([]int, error) {
	def := "none"
	if defaultAll {
		def = "all"
	}
	for {
		raw, err := env.readLine(in, fmt.Sprintf("%s [%s]: ", prompt, def))
		if err != nil {
			return nil, err
		}
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			t = def
		}
		if t == "none" {
			return nil, nil
		}
		if t == "all" {
			out := make([]int, total)
			for i := 0; i < total; i++ {
				out[i] = i
			}
			return out, nil
		}
		seen := map[int]bool{}
		var out []int
		valid := true
		for _, tok := range strings.Split(t, ",") {
			tok = strings.TrimSpace(tok)
			n, err := strconv.Atoi(tok)
			if err != nil || n < 1 || n > total {
				fmt.Fprintf(env.Stdout, "Enter numbers between 1 and %d separated by commas, \"all\", or \"none\".\n", total)
				valid = false
				break
			}
			idx := n - 1
			if !seen[idx] {
				seen[idx] = true
				out = append(out, idx)
			}
		}
		if valid {
			return out, nil
		}
	}
}
