package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/activity"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/wire"
)

const wireHelp = `Usage: maestria wire <subcommand>

Subcommands:
  pair [--host host:port] [--key <security key>]
      Pair with Maestri Wire (Settings → Wire in Maestri). It pairs twice: a read-only
      device for everything Maestria reads, and an owner device used only to send prompts,
      answer Y/n prompts and interrupt agents. Both tokens go to wire.json (0600) in the config
      directory and are never shown or sent to a browser.
  status [--json]
      Show the host, pinned key, roles, capabilities and the last error.
  unpair
      Revoke both devices in Maestri and delete wire.json.
`

func cmdWire(args []string, env *Env) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stdout, wireHelp)
		return nil
	}
	switch args[0] {
	case "pair":
		return cmdWirePair(args[1:], env)
	case "status":
		return cmdWireStatus(args[1:], env)
	case "unpair":
		return cmdWireUnpair(args[1:], env)
	case "help", "--help", "-h":
		fmt.Fprint(env.Stdout, wireHelp)
		return nil
	}
	return usagef("Unknown wire subcommand %q\n\n%s", args[0], wireHelp)
}

func cmdWirePair(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Strings: map[string]bool{"host": true, "key": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria wire pair [--host host:port] [--key <security key>]")
	}
	return runWirePair(bufio.NewReader(env.Stdin), parsed.String("host"), parsed.String("key"), parsed.HasString("host"), parsed.HasString("key"), env)
}

const wirePairIntro = `
Pairing with Maestri Wire. First, in the Maestri app: Settings → Wire, turn Wire on.
You will answer three questions in order: where Wire runs, its security key, and a pairing code.
`

func runWirePair(in *bufio.Reader, host, keyStr string, hasHost, hasKey bool, env *Env) error {
	var err error
	if !hasHost || !hasKey {
		fmt.Fprint(env.Stdout, wirePairIntro)
	}
	if !hasHost {
		fmt.Fprintln(env.Stdout, "\n1/3 Where does Maestri run? Press Enter if it is this machine.")
		host, err = env.readLine(in, fmt.Sprintf("    Host [%s]: ", wire.DefaultHost))
		if err != nil {
			return err
		}
	}
	host, err = wire.NormalizeHost(host)
	if err != nil {
		return usagef("%v", err)
	}
	var pin wire.Pin
	if hasKey {
		if pin, err = wire.ParsePin(keyStr); err != nil {
			return usagef("%v", err)
		}
	} else if pin, err = askWirePin(in, host, env); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "\n3/3 Pairing code: in Maestri, open the \"Pairing\" tab and type the 6-digit code shown there.")
	fmt.Fprintln(env.Stdout, "    (Or the Wire password, if you set one in the \"Manual\" tab.)")
	secret, err := env.readSecret(in, "    Pairing code or password: ")
	if err != nil {
		return err
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return usagef("A pairing code or password is required.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fmt.Fprintln(env.Stdout, "Pairing the owner device and the read-only device…")
	creds, err := wire.Provision(ctx, host, pin, "Maestria", secret, func() (string, error) {
		fmt.Fprintln(env.Stdout, "First device paired. Maestria pairs a second device (one reads, one acts), and a code works once:")
		fmt.Fprintln(env.Stdout, "wait for the Pairing tab to show a NEW code and type it.")
		next, err := env.readSecret(in, "    New pairing code: ")
		return strings.TrimSpace(next), err
	})
	if err != nil {
		return errors.New(wire.PairMessage(err))
	}
	path := wire.Path(configDir(env))
	if err := creds.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Paired with %s.\n", host)
	fmt.Fprintln(env.Stdout, "  Reads:  a read-only (guest) device")
	fmt.Fprintln(env.Stdout, "  Writes: an owner device, used only for prompts, Y/n answers and interrupts")
	fmt.Fprintf(env.Stdout, "Saved to %s (readable only by you). The panel picks it up by itself.\n", path)
	return nil
}

// askWirePin gets the host's security key. On this machine it reads the key Wire presents and
// lets the user accept it with Enter (trust on first use; the key is pinned from then on).
// Elsewhere it must be typed from the Manual tab. A 6-digit answer gets a hint, not an error.
func askWirePin(in *bufio.Reader, host string, env *Env) (wire.Pin, error) {
	fmt.Fprintln(env.Stdout, "\n2/3 Security key: it proves Maestria is talking to your Maestri.")
	var found *wire.Pin
	if wire.IsLoopbackHost(host) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		p, err := wire.FetchPin(ctx, host)
		cancel()
		if err != nil {
			fmt.Fprintf(env.Stdout, "    Could not reach Wire at %s (%v). Is Wire turned on?\n", host, err)
		} else {
			found = &p
			fmt.Fprintf(env.Stdout, "    Wire on this machine presents this key:\n      %s\n", wire.FormatPin(p))
			fmt.Fprintln(env.Stdout, "    It should match the \"Security key\" in Maestri's \"Manual\" tab. Press Enter to trust it.")
		}
	}
	if found == nil {
		fmt.Fprintln(env.Stdout, "    In Maestri, open the \"Manual\" tab and copy the \"Security key\" (pairs like 3F:A2:09:…).")
	}
	for {
		prompt := "    Security key: "
		if found != nil {
			prompt = "    Security key [Enter = trust the key above]: "
		}
		raw, err := env.readLine(in, prompt)
		if err != nil {
			return wire.Pin{}, err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" && found != nil {
			return *found, nil
		}
		if wire.IsPairingCode(raw) {
			fmt.Fprintln(env.Stdout, "    That looks like the 6-digit pairing code: it comes in the next step. Here we need the long security key.")
			continue
		}
		p, err := wire.ParsePin(raw)
		if err != nil {
			fmt.Fprintf(env.Stdout, "    %v\n", err)
			continue
		}
		if found != nil && p != *found {
			fmt.Fprintln(env.Stdout, "    That key does not match the one Wire presents on this machine. Check the Manual tab and try again.")
			continue
		}
		return p, nil
	}
}

type wireStatusOut struct {
	Paired       bool     `json:"paired"`
	Host         string   `json:"host,omitempty"`
	Pin          string   `json:"pin,omitempty"`
	Link         string   `json:"link"`
	Detail       string   `json:"detail,omitempty"`
	GuestRole    string   `json:"guestRole,omitempty"`
	OwnerRole    string   `json:"ownerRole,omitempty"`
	OwnerPaired  bool     `json:"ownerPaired"`
	Protocol     int      `json:"protocolVersion,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	LastError    string   `json:"lastError,omitempty"`
	LastErrorAt  string   `json:"lastErrorAt,omitempty"`
}

func cmdWireStatus(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	out := wireStatusOut{Link: string(wire.StateDisabled)}
	creds, err := wire.LoadCredentials(wire.Path(configDir(env)))
	switch {
	case errors.Is(err, wire.ErrNotPaired):
		out.Detail = "Not paired. Run maestria wire pair."
	case err != nil:
		return err
	default:
		out.Paired, out.Host, out.Pin, out.OwnerPaired = true, creds.Host, creds.Pin, creds.Owner != nil
		probeWire(creds, &out)
	}
	if db, err := store.Open(configDir(env)); err == nil {
		if st, ok := activity.LoadStoredStatus(db); ok && st.LastError != "" {
			out.LastError = st.LastError
			if st.LastErrorAt > 0 {
				out.LastErrorAt = time.UnixMilli(st.LastErrorAt).Format(time.RFC3339)
			}
		}
		db.Close()
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, out)
	}
	printWireStatus(env, out)
	return nil
}

// probeWire checks the link with the stored tokens: GET /api/info only, so nothing wakes.
func probeWire(creds *wire.Credentials, out *wireStatusOut) {
	guest, owner, err := creds.Clients()
	if err != nil {
		out.Link, out.Detail = string(wire.StateDisabled), err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := guest.Info(ctx)
	if err != nil {
		st := wire.Classify(err)
		out.Link = string(st)
		out.Detail = err.Error()
		if st == wire.StatePinMismatch {
			out.Detail = err.Error() + ". Run maestria wire unpair and pair again if you reset Wire on purpose."
		}
		return
	}
	out.Protocol, out.Capabilities, out.GuestRole = info.ProtocolVersion, append([]string(nil), info.Capabilities...), info.Role
	sort.Strings(out.Capabilities)
	switch {
	case info.Role == "":
		out.Link, out.Detail = string(wire.StateRevoked), "Maestri no longer accepts the read token. Pair again."
	case info.ProtocolVersion != wire.ProtocolVersion:
		out.Link, out.Detail = string(wire.StateIncompatible), fmt.Sprintf("The host speaks Wire protocol %d.", info.ProtocolVersion)
	case info.Role != "guest":
		out.Link, out.Detail = string(wire.StateRole), "The read token is not read-only. Unpair and pair again."
	default:
		out.Link = string(wire.StateConnected)
	}
	if owner != nil {
		if oi, err := owner.Info(ctx); err != nil {
			out.OwnerRole = "unreachable"
		} else if oi.Role == "" {
			out.OwnerRole = "revoked"
		} else {
			out.OwnerRole = oi.Role
		}
	}
}

func printWireStatus(env *Env, o wireStatusOut) {
	w := env.Stdout
	if !o.Paired {
		fmt.Fprintln(w, "Wire: not paired. Run maestria wire pair.")
		if o.LastError != "" {
			fmt.Fprintf(w, "Last error: %s\n", o.LastError)
		}
		return
	}
	fmt.Fprintf(w, "Host:          %s\n", o.Host)
	fmt.Fprintf(w, "Pinned key:    %s (SHA-256 of the certificate's public key)\n", o.Pin)
	fmt.Fprintf(w, "Link:          %s\n", o.Link)
	if o.Detail != "" {
		fmt.Fprintf(w, "               %s\n", o.Detail)
	}
	fmt.Fprintf(w, "Reads:         %s\n", valueOr(o.GuestRole, "unknown"))
	if o.OwnerPaired {
		fmt.Fprintf(w, "Writes:        %s\n", valueOr(o.OwnerRole, "unknown"))
	} else {
		fmt.Fprintln(w, "Writes:        off (no owner device)")
	}
	if o.Protocol != 0 {
		fmt.Fprintf(w, "Protocol:      %d\n", o.Protocol)
	}
	if len(o.Capabilities) > 0 {
		fmt.Fprintf(w, "Capabilities:  %s\n", strings.Join(o.Capabilities, ", "))
	}
	if o.LastError != "" {
		fmt.Fprintf(w, "Last error:    %s", o.LastError)
		if o.LastErrorAt != "" {
			fmt.Fprintf(w, " (%s)", o.LastErrorAt)
		}
		fmt.Fprintln(w)
	}
}

func valueOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func cmdWireUnpair(args []string, env *Env) error {
	if len(args) > 0 {
		return usagef("Usage: maestria wire unpair")
	}
	path := wire.Path(configDir(env))
	creds, err := wire.LoadCredentials(path)
	if errors.Is(err, wire.ErrNotPaired) {
		fmt.Fprintln(env.Stdout, "Not paired; nothing to do.")
		return nil
	}
	if err != nil {
		// A damaged file cannot be used to revoke anything; still let the person start over.
		fmt.Fprintf(env.Stdout, "wire.json is unusable (%v); removing it.\n", err)
		return os.Remove(path)
	}
	guest, owner, err := creds.Clients()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	failed := false
	revoke := func(label string, c *wire.Client) {
		err := c.DeleteDevice(ctx)
		var ae *wire.APIError
		switch {
		case err == nil:
			fmt.Fprintf(env.Stdout, "Revoked the %s device in Maestri.\n", label)
		case errors.As(err, &ae) && ae.Status == 401:
			fmt.Fprintf(env.Stdout, "The %s device was already revoked.\n", label)
		default:
			failed = true
			fmt.Fprintf(env.Stdout, "Could not revoke the %s device (%v). Remove it in Maestri: Settings → Wire.\n", label, err)
		}
	}
	revoke("read-only", guest)
	if owner != nil {
		revoke("owner", owner)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "Deleted wire.json.")
	if failed {
		fmt.Fprintln(env.Stdout, "The tokens stay valid in Maestri until you remove them there (or 30 idle days).")
	}
	return nil
}
