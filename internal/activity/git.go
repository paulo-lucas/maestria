package activity

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const gitTimeout = 5 * time.Second

// git runs a read-only git command in dir with a timeout. It reports the trimmed stdout and
// whether the command exited 0.
func git(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err == nil
}

// validBranch rejects names that could be mistaken for options or revision syntax.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.Contains(b, "..") || strings.ContainsAny(b, " \t\n\r~^:?*[\\@{") {
		return false
	}
	return true
}

// defaultBranchRef finds the repository's default branch: origin/HEAD when there is one, else
// main, else master, else whatever is checked out.
func defaultBranchRef(dir string) string {
	if out, ok := git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); ok && out != "" {
		if _, ok := git(dir, "rev-parse", "--verify", "--quiet", "refs/remotes/"+out); ok {
			return "refs/remotes/" + out
		}
	}
	for _, b := range []string{"main", "master"} {
		if _, ok := git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); ok {
			return "refs/heads/" + b
		}
	}
	if out, ok := git(dir, "symbolic-ref", "--quiet", "HEAD"); ok && out != "" {
		return out
	}
	return ""
}

// BranchLanded reports whether branch's tip is an ancestor of the repository's default branch in
// dir, which means its work was merged. It is read-only, bounded by a timeout, and false whenever
// it cannot tell (no git, not a repository, the branch is gone).
func BranchLanded(dir, branch string) bool {
	if dir == "" || !validBranch(branch) {
		return false
	}
	ref := "refs/heads/" + branch
	if _, ok := git(dir, "rev-parse", "--verify", "--quiet", ref); !ok {
		return false
	}
	def := defaultBranchRef(dir)
	if def == "" || def == ref {
		return false
	}
	_, ancestor := git(dir, "merge-base", "--is-ancestor", ref, def)
	return ancestor
}
