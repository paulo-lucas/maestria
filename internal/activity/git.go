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

// BranchTip returns the commit a local branch points at in dir ("" when there is no such branch,
// no git, or dir is not a repository). The panel records it while a floor is alive, so that the
// floor can still be recognised as landed after its branch has been deleted.
func BranchTip(dir, branch string) string {
	if dir == "" || !validBranch(branch) {
		return ""
	}
	out, ok := git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if !ok || !validSHA(out) {
		return ""
	}
	return out
}

// CommitLanded reports whether commit is an ancestor of the repository's default branch in dir:
// the same test as BranchLanded, for a commit that outlived its branch. Like BranchLanded it
// cannot tell a floor that was merged from one that never got a commit of its own (its tip is
// already in the default branch): such a floor reads as landed.
func CommitLanded(dir, commit string) bool {
	if dir == "" || !validSHA(commit) {
		return false
	}
	if _, ok := git(dir, "cat-file", "-e", commit+"^{commit}"); !ok {
		return false
	}
	def := defaultBranchRef(dir)
	if def == "" {
		return false
	}
	_, ancestor := git(dir, "merge-base", "--is-ancestor", commit, def)
	return ancestor
}

// validSHA accepts a full hexadecimal object name (SHA-1 or SHA-256) and nothing else.
func validSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
