package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/connect"
	"github.com/paulo-lucas/maestria/internal/devices"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/runlock"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/termsafe"
)

const connectUsage = `Usage:
  maestria connect [--link CODE] [--cloud URL] [--name NAME] [--yes]   Link this machine (first time) and stay connected
  maestria connect status [--json]                                     Show the link and the connection
  maestria connect unlink [--yes]                                      Revoke this machine's link and forget it
`

// LinkPollInterval is how often the CLI asks the cloud whether its code was entered.
var LinkPollInterval = 2 * time.Second

// kvState is where the running connector leaves its connection state for `connect status`.
const kvState = "connect.state"

type connState struct {
	State     string `json:"state"`
	Since     int64  `json:"since"`
	Error     string `json:"error,omitempty"`
	PID       int    `json:"pid"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Connector is the hosted-mode runtime: the same API handlers as local mode behind the relay gate,
// and the outbound connection that serves them.
type Connector struct {
	Link    *connect.Link
	Client  *connect.Client
	Server  *built
	Devices *devices.Manager
	Close   func()
}

// hostedOpts is what a server needs to also serve relayed requests: the device manager (policy
// from the machine's own config) and the machine's name. dm receives the manager once built.
func hostedOpts(env *Env, link *connect.Link, dm **devices.Manager) buildOpts {
	return buildOpts{
		ConnectorName: link.Name,
		Devices: func(db *sql.DB, cfg *config.Config) *devices.Manager {
			*dm = devices.New(db, func() devices.Policy {
				return devices.Policy{
					Enabled:       cfg.Relay.Enabled,
					Absolute:      time.Duration(cfg.Relay.ApprovalAbsoluteDays) * 24 * time.Hour,
					Idle:          time.Duration(cfg.Relay.IdleDays) * 24 * time.Hour,
					RatePerMinute: cfg.Relay.RatePerMinute,
				}
			}, env.Now)
			return *dm
		},
	}
}

// BuildConnector wires `maestria connect` from the config directory and a saved link: a server
// without a local login, used only through the relay gate.
func BuildConnector(env *Env, link *connect.Link, log func(string)) (*Connector, error) {
	var dm *devices.Manager
	b, err := buildServer(env, hostedOpts(env, link, &dm))
	if err != nil {
		return nil, err
	}
	return newConnector(env, b, dm, link, log), nil
}

// newConnector builds the outbound client around an already built server. `serve --connect` and
// `connect` share it, so the relay serves exactly the handlers (and the one Wire subscription and
// standby guard) of the server it is given.
func newConnector(env *Env, b *built, dm *devices.Manager, link *connect.Link, log func(string)) *Connector {
	gate := &connect.Gate{ConnectorID: link.ConnectorID, Devices: dm, Killed: b.Tun.Killed, Audit: b.Audit}
	dm.SetAudit(b.Audit)
	c := &connect.Client{
		Link: link, Version: env.Version, Name: link.Name, Handler: b.Srv.RelayHandler(), Gate: gate, Devices: dm, Killed: b.Tun.Killed,
		OnPairing: func(label string) {
			if log != nil {
				log(fmt.Sprintf("A browser (%q) asked to be approved. See \"maestria devices pending\"; it is only yours to approve if you started that pairing.", label))
			}
		},
		OnState: func(s connect.State, err error) {
			st := connState{State: string(s), Since: time.Now().UnixMilli(), PID: os.Getpid(), UpdatedAt: time.Now().UnixMilli()}
			if err != nil {
				st.Error = err.Error()
			}
			if raw, e := json.Marshal(st); e == nil {
				_ = store.SetKV(b.DB, kvState, string(raw))
			}
			if log != nil {
				switch s {
				case connect.StateOnline:
					log("Connected to " + link.Cloud + " as \"" + link.Name + "\".")
				case connect.StateOffline:
					msg := "Disconnected from the cloud; retrying."
					if err != nil {
						msg = "Disconnected from the cloud (" + err.Error() + "); retrying."
					}
					log(msg)
				case connect.StateRevoked:
					log("This machine was unlinked or revoked in the cloud. Run \"maestria connect unlink\", then link it again.")
				}
			}
		},
	}
	return &Connector{Link: link, Client: c, Server: b, Devices: dm, Close: b.Close}
}

func cloudURL(env *Env, flag string, cfg *config.Config) string {
	switch {
	case flag != "":
		return connect.CleanCloudURL(flag)
	case env.Config.Getenv != nil && strings.TrimSpace(env.Config.Getenv("MAESTRIA_CLOUD_URL")) != "":
		return connect.CleanCloudURL(env.Config.Getenv("MAESTRIA_CLOUD_URL"))
	case cfg != nil && cfg.Cloud != "":
		return cfg.Cloud
	}
	return ""
}

func machineInfo(env *Env) relay.Machine {
	host, _ := os.Hostname()
	return relay.Machine{Host: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: env.Version}
}

func cmdConnect(args []string, env *Env) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "status":
			return cmdConnectStatus(args[1:], env)
		case "unlink":
			return cmdConnectUnlink(args[1:], env)
		}
		return usagef("Unknown connect command %q\n\n%s", args[0], connectUsage)
	}
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"yes": true, "y": true},
		Strings: map[string]bool{"link": true, "code": true, "cloud": true, "name": true},
		Shorts:  map[string]string{"y": "yes"},
	})
	if err != nil || len(parsed.Pos) > 0 {
		return usagef("%s", connectUsage)
	}
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return err
	}
	link, err := connect.LoadLink(dir)
	if err != nil {
		return err
	}
	code := parsed.String("link")
	if code == "" {
		code = parsed.String("code")
	}
	if link != nil && (code != "" || parsed.String("cloud") != "") {
		return fmt.Errorf("this machine is already linked to %s as %q; run \"maestria connect unlink\" first to link it somewhere else", link.Cloud, link.Name)
	}
	justLinked := false
	if link == nil {
		justLinked = true
		cloud := cloudURL(env, parsed.String("cloud"), cfg)
		if cloud == "" {
			return usagef("No cloud to link to. Pass --cloud https://your-maestria-cloud (or set \"cloud\" in config.json).")
		}
		if _, err := relay.APIURL(cloud, "/"); err != nil {
			return usagef("%v", err)
		}
		link, err = linkMachine(env, dir, cloud, code, parsed.String("name"), parsed.Bool("yes"))
		if err != nil {
			return err
		}
	}
	return runConnector(env, link, justLinked)
}

func prompt(env *Env, r *bufio.Reader, q string) string {
	fmt.Fprint(env.Stdout, q)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func yes(s string) bool { return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes") }

// linkMachine runs either link flow and saves the result.
func linkMachine(env *Env, dir, cloud, code, name string, assumeYes bool) (*connect.Link, error) {
	api := &connect.API{Cloud: cloud, Machine: machineInfo(env)}
	in := bufio.NewReader(env.Stdin)
	ctx, stop := env.signalCtx()
	defer stop()

	// Linking hands a cloud the way to reach this machine, so it needs a person at a terminal
	// (--yes does not skip this, only the questions after it).
	log, closeLog := openAudit(env, dir)
	defer closeLog()
	const action = "Link this machine to a hosted Maestria"
	if err := env.requireHuman(in, action, []string{
		"Cloud: " + cloud,
		"That server will relay your browsers' requests to this machine. Each request is still signed and checked here,",
		"but it can read what you view through it and it serves the pages you approve browsers from.",
		"Only link a server you run or trust.",
	}, log); err != nil {
		return nil, err
	}

	var linked *relay.Linked
	if code != "" {
		red, err := api.Redeem(ctx, code)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(env.Stdout, "This machine would join the account %s.\n", red.AccountHint)
		if name == "" {
			name = red.DefaultName
			if !assumeYes {
				if n := prompt(env, in, fmt.Sprintf("Name for this machine [%s]: ", red.DefaultName)); n != "" {
					name = n
				}
			}
		}
		if !assumeYes && !yes(prompt(env, in, fmt.Sprintf("Link this machine to %s as %q? [y/N] ", red.AccountHint, name))) {
			return nil, errors.New("cancelled; nothing was linked")
		}
		if linked, err = api.Confirm(ctx, code, red.Token, name); err != nil {
			return nil, err
		}
	} else {
		st, err := api.Start(ctx)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(env.Stdout, "Open %s and enter this code (valid 10 minutes):\n\n    %s\n\nWaiting for you to enter it…\n", st.URL, st.Code)
		var claimed *relay.PollResponse
		for {
			select {
			case <-ctx.Done():
				return nil, errors.New("cancelled; nothing was linked")
			case <-time.After(LinkPollInterval):
			}
			p, err := api.Poll(ctx, st.PollToken, false, "")
			if err != nil {
				return nil, err
			}
			if p.Status == "expired" {
				return nil, errors.New("the code expired; run the command again")
			}
			if p.Status == "claimed" {
				claimed = p
				break
			}
		}
		fmt.Fprintf(env.Stdout, "The code was entered for the account %s.\n", claimed.AccountHint)
		if name == "" {
			name = claimed.DefaultName
			if !assumeYes {
				if n := prompt(env, in, fmt.Sprintf("Name for this machine [%s]: ", claimed.DefaultName)); n != "" {
					name = n
				}
			}
		}
		if !assumeYes && !yes(prompt(env, in, fmt.Sprintf("Link this machine to %s as %q? [y/N] ", claimed.AccountHint, name))) {
			return nil, errors.New("cancelled; nothing was linked")
		}
		p, err := api.Poll(ctx, st.PollToken, true, name)
		if err != nil {
			return nil, err
		}
		if p.Linked == nil {
			return nil, errors.New("the cloud did not complete the link; run the command again")
		}
		linked = p.Linked
	}
	link := &connect.Link{Cloud: cloud, ConnectorID: linked.ConnectorID, Credential: linked.Credential, Name: linked.Name, LinkedAt: env.now().UTC().Format(time.RFC3339)}
	if err := connect.SaveLink(dir, link); err != nil {
		return nil, err
	}
	fmt.Fprintf(env.Stdout, "Linked as %q. The credential is stored in %s (owner-only).\n", link.Name, connect.LinkPath(dir))
	return link, nil
}

func runConnector(env *Env, link *connect.Link, justLinked bool) error {
	lock, err := runlock.Acquire(configDir(env), "connect")
	if err != nil {
		var held *runlock.HeldError
		if justLinked && errors.As(err, &held) && strings.HasPrefix(held.Info.Mode, "serve") {
			fmt.Fprintf(env.Stdout, "maestria serve (pid %d) is already running on this config directory. Restart it (\"maestria service restart\", or stop and start it) and it will connect to the cloud as well.\n", held.Info.PID)
			return nil
		}
		return err
	}
	defer lock.Release()
	c, err := BuildConnector(env, link, func(s string) { fmt.Fprintln(env.Stdout, s) })
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, stop := env.signalCtx()
	defer stop()
	c.Server.Srv.StartBackground(ctx)
	fmt.Fprintf(env.Stdout, "Connecting to %s. Approve browsers on this machine with \"maestria devices\".\n", link.Cloud)
	err = c.Client.Run(ctx)
	if errors.Is(err, connect.ErrRevoked) {
		return err
	}
	fmt.Fprintln(env.Stdout, "Disconnected. Bye.")
	return nil
}

func cmdConnectStatus(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil || len(parsed.Pos) > 0 {
		return usagef("%s", connectUsage)
	}
	dir := configDir(env)
	link, err := connect.LoadLink(dir)
	if err != nil {
		return err
	}
	if link == nil {
		if parsed.Bool("json") {
			return writeJSONLine(env, map[string]any{"linked": false})
		}
		fmt.Fprintln(env.Stdout, "Not linked. Run: maestria connect --link CODE --cloud https://your-maestria-cloud")
		return nil
	}
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	state := "not running"
	var st connState
	if raw, ok, _ := store.GetKV(db, kvState); ok && json.Unmarshal([]byte(raw), &st) == nil {
		// The process refreshes its row at least every 30 s while it lives.
		if time.Since(time.UnixMilli(st.UpdatedAt)) < 2*time.Minute || st.State == string(connect.StateRevoked) {
			state = st.State
		}
	}
	dm := devices.New(db, nil, env.Now)
	list, _ := dm.List()
	active := 0
	for _, d := range list {
		if d.State == "active" {
			active++
		}
	}
	pending, _ := dm.Pending()
	killed, _, _ := store.GetKV(db, "tunnel_killed")
	out := map[string]any{"linked": true, "cloud": link.Cloud, "connectorId": link.ConnectorID, "name": link.Name, "state": state,
		"approvedDevices": active, "pendingPairings": len(pending), "killSwitch": killed == "1"}
	if parsed.Bool("json") {
		return writeJSONLine(env, out)
	}
	fmt.Fprintf(env.Stdout, "Linked to:        %s\nMachine:          %s (%s)\nConnection:       %s\nApproved devices: %d\nWaiting to pair:  %d\nKill switch:      %s\n",
		link.Cloud, link.Name, link.ConnectorID, state, active, len(pending), map[bool]string{true: "ON (relay blocked)", false: "off"}[killed == "1"])
	if st.Error != "" && state != "online" {
		fmt.Fprintf(env.Stdout, "Last error:       %s\n", st.Error)
	}
	return nil
}

func cmdConnectUnlink(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"yes": true, "y": true}, Shorts: map[string]string{"y": "yes"}})
	if err != nil || len(parsed.Pos) > 0 {
		return usagef("%s", connectUsage)
	}
	dir := configDir(env)
	link, err := connect.LoadLink(dir)
	if err != nil {
		return err
	}
	if link == nil {
		fmt.Fprintln(env.Stdout, "This machine is not linked.")
		return nil
	}
	if !parsed.Bool("yes") {
		in := bufio.NewReader(env.Stdin)
		if !yes(prompt(env, in, fmt.Sprintf("Unlink %q from %s? Browsers approved here stay approved until you revoke them. [y/N] ", link.Name, link.Cloud))) {
			fmt.Fprintln(env.Stdout, "Cancelled.")
			return nil
		}
	}
	api := &connect.API{Cloud: link.Cloud, Machine: machineInfo(env)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := api.Unlink(ctx, link.Credential); err != nil {
		var ae *connect.APIError
		if !(errors.As(err, &ae) && ae.Status == 401) { // 401: already revoked, nothing to tell the cloud
			fmt.Fprintf(env.Stdout, "Could not reach the cloud (%v). The local link is removed anyway; revoke this machine in the web too.\n", err)
		}
	}
	if err := connect.DeleteLink(dir); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "Unlinked. The connector credential is gone from this machine and revoked in the cloud.")
	return nil
}

// ---- maestria devices ----

const devicesUsage = `Usage:
  maestria devices pending           Browsers waiting for approval on this machine
  maestria devices approve <code>    Approve the pairing whose code your browser shows
  maestria devices reject <code>     Refuse it
  maestria devices list              Approved devices
  maestria devices revoke <id>       End a device for good
`

func cmdDevices(args []string, env *Env) error {
	if len(args) == 0 {
		return usagef("%s", devicesUsage)
	}
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return err
	}
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	dm := devices.New(db, func() devices.Policy {
		return devices.Policy{Enabled: cfg.Relay.Enabled, Absolute: time.Duration(cfg.Relay.ApprovalAbsoluteDays) * 24 * time.Hour,
			Idle: time.Duration(cfg.Relay.IdleDays) * 24 * time.Hour, RatePerMinute: cfg.Relay.RatePerMinute}
	}, env.Now)
	switch args[0] {
	case "pending":
		list, err := dm.Pending()
		if err != nil {
			return err
		}
		b := dm.Budget()
		if len(list) == 0 {
			fmt.Fprintln(env.Stdout, "No browser is waiting for approval.")
		}
		now := env.now()
		for _, p := range list {
			left := time.UnixMilli(p.ExpiresAt).Sub(now).Round(time.Second)
			offered := time.UnixMilli(p.CreatedAt).Format("15:04:05")
			if p.Code == "" {
				fmt.Fprintf(env.Stdout, "%s  (waiting for the browser to show its code)  offered %s, expires in %s\n", termsafe.Clean(p.Label, 80), offered, left)
				continue
			}
			fmt.Fprintf(env.Stdout, "%s  %s  key %s  offered %s, expires in %s\n", relay.FormatPairCode(p.Code), termsafe.Clean(p.Label, 80), p.Fingerprint, offered, left)
		}
		fmt.Fprintf(env.Stdout, "\nPairings offered in the last %d minutes: %d of %d allowed.\n", int(devices.PairWindow/time.Minute), b.Offers, b.MaxOffers)
		if b.Paused != nil {
			fmt.Fprintf(env.Stdout, "New pairings are refused for now: %s.\n", b.Paused.Error())
		}
		if len(list) > 0 {
			fmt.Fprintln(env.Stdout, "\nOnly approve a pairing you started yourself. Check that the code and the key match what your browser shows, then: maestria devices approve <code>")
		}
		return nil
	case "approve", "reject":
		if len(args) < 2 {
			return usagef("%s", devicesUsage)
		}
		code := strings.Join(args[1:], " ")
		if args[0] == "reject" {
			if err := dm.Reject(code); err != nil {
				return err
			}
			fmt.Fprintln(env.Stdout, "Rejected.")
			return nil
		}
		const action = "Approve a browser to control this machine"
		log := audit.New(db, env.Now)
		dm.SetAudit(log)
		// A script or an agent without a terminal gets nothing, not even an answer about a code.
		if err := env.needTTY(action, log); err != nil {
			return err
		}
		p, err := dm.Lookup(code)
		if err != nil {
			return err
		}
		in := bufio.NewReader(env.Stdin)
		details := []string{
			"Name (as the cloud described it): " + termsafe.Clean(p.Label, 80),
			"Key fingerprint:                  " + p.Fingerprint + "  (the pairing page shows the same one)",
			"Offered at:                       " + time.UnixMilli(p.CreatedAt).Format("2006-01-02 15:04:05") + " (" + env.now().Sub(time.UnixMilli(p.CreatedAt)).Round(time.Second).String() + " ago)",
			"Code:                             " + relay.FormatPairCode(p.Code),
			fmt.Sprintf("It will be able to send commands for %d days (%d if unused). That includes prompting your agents and answering their Y/n questions,", cfg.Relay.ApprovalAbsoluteDays, cfg.Relay.IdleDays),
			"which can make an agent run commands. Only approve a pairing you started yourself, from a browser you control.",
		}
		if err := env.requireHuman(in, action, details, log); err != nil {
			return err
		}
		d, err := dm.Approve(code)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Approved %q (key %s). It may send commands for %d days, or until it has been idle %d.\n",
			termsafe.Clean(d.Label, 80), d.Fingerprint(), cfg.Relay.ApprovalAbsoluteDays, cfg.Relay.IdleDays)
		return nil
	case "list":
		list, err := dm.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(env.Stdout, "No approved devices.")
			return nil
		}
		for _, d := range list {
			fmt.Fprintf(env.Stdout, "%s  key %s  %-8s  %s  approved %s, last used %s\n", d.ID, d.Fingerprint(), d.State, termsafe.Clean(d.Label, 80),
				time.UnixMilli(d.ApprovedAt).Format("2006-01-02 15:04"), time.UnixMilli(d.LastUsedAt).Format("2006-01-02 15:04"))
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return usagef("%s", devicesUsage)
		}
		id := args[1]
		if len(id) < 32 { // allow the 8-character prefix shown by approve and list
			list, _ := dm.List()
			var match []string
			for _, d := range list {
				if strings.HasPrefix(d.ID, id) {
					match = append(match, d.ID)
				}
			}
			if len(match) != 1 {
				return fmt.Errorf("no single device matches %q", id)
			}
			id = match[0]
		}
		if err := dm.Revoke(id); err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, "Revoked.")
		return nil
	}
	return usagef("Unknown devices command %q\n\n%s", args[0], devicesUsage)
}

// signalCtx is the context a long-running command stops on: env.Ctx (tests), or SIGINT/SIGTERM.
func (e *Env) signalCtx() (context.Context, context.CancelFunc) {
	if e.Ctx != nil {
		return context.WithCancel(e.Ctx)
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
