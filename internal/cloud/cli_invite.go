package cloud

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/paulo-lucas/maestria/internal/termsafe"
)

func cmdInvite(args []string, env *Env) error {
	const usage = "usage: maestria-cloud invite create [--email ADDRESS] [--expires 7d] [--note TEXT] | list | revoke ID"
	if len(args) == 0 {
		return errors.New(usage)
	}
	cfg, srv, closeAll, err := openAll(env)
	if err != nil {
		return err
	}
	defer closeAll()
	acc := srv.Accounts()
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("invite create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		email := fs.String("email", "", "only this address can use it")
		expires := fs.String("expires", "7d", "how long it works (1h to 30d)")
		note := fs.String("note", "", "a note for you (200 characters at most)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
			return errors.New(usage)
		}
		ttl, err := ParseExpiry(*expires)
		if err != nil {
			return err
		}
		inv, tok, err := acc.CreateInvite(InviteOptions{TTL: ttl, Note: *note, Email: *email})
		if err != nil {
			return err
		}
		link := acc.InviteLink(tok)
		if cfg.PublicURL == "" {
			link = "<your server>" + link
			fmt.Fprintln(env.Stderr, "warning: publicUrl is not set, so the link has no address; put your server's address in front of it")
		}
		fmt.Fprintf(env.Stdout, "Invitation %s, valid until %s", inv.ID, time.UnixMilli(inv.ExpiresAt).UTC().Format("2006-01-02 15:04 UTC"))
		if inv.Email != "" {
			fmt.Fprintf(env.Stdout, ", only for %s", termsafe.Clean(inv.Email, 254))
		}
		fmt.Fprintf(env.Stdout, ".\nIt works once. This link is shown only now:\n\n  %s\n", link)
		if cfg.Signup != SignupInvite {
			fmt.Fprintf(env.Stderr, "note: signup is %q on this server, so invitations are not used right now\n", cfg.Signup)
		}
		return nil
	case "list":
		list, err := acc.ListInvites()
		if err != nil {
			return err
		}
		now := env.Now()
		users := map[string]string{}
		if all, err := acc.ListUsers(); err == nil {
			for _, u := range all {
				users[u.ID] = u.Email
			}
		}
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tEXPIRES\tFOR\tBY\tNOTE")
		for i := range list {
			in := &list[i]
			by := "server"
			if in.CreatedBy != "" {
				by = termsafe.Clean(users[in.CreatedBy], 254)
			}
			who := "anyone"
			if in.Email != "" {
				who = termsafe.Clean(in.Email, 254)
			}
			status := in.Status(now)
			if status == "used" {
				status = "used (no new account)"
				if in.UsedBy != "" {
					status = "used by " + termsafe.Clean(users[in.UsedBy], 254)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", in.ID, status, time.UnixMilli(in.ExpiresAt).UTC().Format("2006-01-02 15:04 UTC"), who, by, termsafe.Clean(in.Note, InviteNoteMax))
		}
		return tw.Flush()
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: maestria-cloud invite revoke ID")
		}
		if err := acc.RevokeInvite("", "", strings.TrimSpace(args[1])); err != nil {
			return errors.New("no such invitation")
		}
		fmt.Fprintf(env.Stdout, "Invitation %s is withdrawn (if it was not already used).\n", termsafe.Clean(args[1], 40))
		return nil
	}
	return fmt.Errorf("unknown invite command %q\n%s", args[0], usage)
}
