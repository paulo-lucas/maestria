package update

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// CommandOpts is what `maestria update` and `maestria-cloud update` share.
type CommandOpts struct {
	// Check only reports; nothing is downloaded.
	Check bool
	// Yes skips the confirmation. Without a terminal it is required.
	Yes bool
	// Force installs even when the release is not newer (a downgrade, a reinstall) and lets a
	// development build update.
	Force bool
	// Channel is stable (default) or prerelease.
	Channel Channel
	// Version installs a specific tag instead of the newest release.
	Version string

	IsTTY  bool
	Stdin  io.Reader
	Stdout io.Writer
}

// RunCommand implements the update command around u. It returns a *UsageError for refusals the
// user can fix by changing the command line.
func RunCommand(ctx context.Context, u *Updater, o CommandOpts) error {
	name := u.Binary
	if IsDevBuild(u.Current) && !o.Force {
		fmt.Fprintf(o.Stdout, "%s %s is a development build; it does not update itself (use --force to install a release anyway).\n", name, u.Current)
		return nil
	}

	var (
		rel Release
		err error
	)
	if o.Version != "" {
		rel, err = u.ByTag(ctx, o.Version)
	} else {
		rel, err = u.Latest(ctx, o.Channel)
	}
	if err != nil {
		if errors.Is(err, ErrNoRelease) {
			return errors.New("no release has been published yet")
		}
		return err
	}
	target := CleanVersion(rel.Tag)

	status := StatusAvailable
	if IsDevBuild(u.Current) {
		status = StatusAvailable // forced over a dev build
	} else if status, err = u.Compare(rel); err != nil {
		return err
	}

	switch status {
	case StatusUpToDate:
		if !o.Force {
			fmt.Fprintf(o.Stdout, "%s %s is up to date.\n", name, u.Current)
			return nil
		}
	case StatusAhead:
		if !o.Force {
			if o.Version != "" {
				return &UsageError{fmt.Sprintf("%s is older than the running %s; use --force to downgrade", target, u.Current)}
			}
			fmt.Fprintf(o.Stdout, "%s %s is newer than the latest release (%s); nothing to do.\n", name, u.Current, target)
			return nil
		}
	}

	verb := "Update"
	switch status {
	case StatusAhead:
		verb = "Downgrade"
	case StatusUpToDate:
		verb = "Reinstall"
	}
	if o.Check {
		if status == StatusAvailable {
			fmt.Fprintf(o.Stdout, "%s %s is available (you have %s). Run: %s update\n", name, target, u.Current, name)
			if rel.HTMLURL != "" {
				fmt.Fprintf(o.Stdout, "Release notes: %s\n", rel.HTMLURL)
			}
		} else {
			fmt.Fprintf(o.Stdout, "%s %s -> %s would be applied.\n", name, u.Current, target)
		}
		return nil
	}

	if !o.Yes {
		if !o.IsTTY {
			return &UsageError{"not on a terminal: pass --yes to update without asking"}
		}
		fmt.Fprintf(o.Stdout, "%s %s from %s to %s? [y/N] ", verb, name, u.Current, target)
		line, _ := bufio.NewReader(o.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(o.Stdout, "Cancelled.")
			return nil
		}
	}

	u.Log = func(format string, args ...any) { fmt.Fprintf(o.Stdout, format+"\n", args...) }
	res, err := u.Apply(ctx, rel)
	if err != nil {
		return err
	}
	if res.Restarted {
		fmt.Fprintf(o.Stdout, "%s is now %s and running (restarted). The previous version is kept at %s.\n", name, res.To, res.Previous)
	} else {
		fmt.Fprintf(o.Stdout, "%s is now %s. The previous version is kept at %s.\n", name, res.To, res.Previous)
	}
	return nil
}

// UsageError marks a mistake in how the command was invoked.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }
