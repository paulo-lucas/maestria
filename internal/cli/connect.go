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

	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/connect"
	"github.com/maestria-dev/maestria/internal/devices"
	"github.com/maestria-dev/maestria/internal/relay"
	"github.com/maestria-dev/maestria/internal/store"
)

const connectUsage = `Usage:
  maestria connect [--link CODE] [--cloud URL] [--name NAME] [--yes]   Link this machine (first time) and stay connected
  maestria connect status [--json]                                     Show the link and the connection
  maestria connect unlink [--yes]                                      Revoke this machine's link and forget it
`

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

// BuildConnector wires `maestria connect` from the config directory and a saved link.
func BuildConnector(env *Env, link *connect.Link, log func(string)) (*Connector, error) {
	var dm *devices.Manager
	b, err := buildServer(env, buildOpts{
		ConnectorName: link.Name,
		Devices: func(db *sql.DB, cfg *config.Config) *devices.Manager {
			dm = devices.New(db, func() devices.Policy {
				return devices.Policy{
					Enabled:       cfg.Relay.Enabled,
					Absolute:      time.Duration(cfg.Relay.ApprovalAbsoluteDays) * 24 * time.Hour,
					Idle:          time.Duration(cfg.Relay.IdleDays) * 24 * time.Hour,
					RatePerMinute: cfg.Relay.RatePerMinute,
				}
			}, env.Now)
			return dm
		},
	})
	if err != nil {
		return nil, err
	}
	gate := &connect.Gate{ConnectorID: link.ConnectorID, Devices: dm, Killed: b.Tun.Killed, Audit: b.Audit}
	c := &connect.Client{
		Link: link, Version: env.Version, Name: link.Name, Handler: b.Srv.RelayHandler(), Gate: gate, Devices: dm, Killed: b.Tun.Killed,
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
	return &Connector{Link: link, Client: c, Server: b, Devices: dm, Close: b.Close}, nil
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
	if link == nil {
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
	return runConnector(env, link)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
			case <-time.After(2 * time.Second):
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

func runConnector(env *Env, link *connect.Link) error {
	c, err := BuildConnector(env, link, func(s string) { fmt.Fprintln(env.Stdout, s) })
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
		if len(list) == 0 {
			fmt.Fprintln(env.Stdout, "No browser is waiting for approval.")
			return nil
		}
		for _, p := range list {
			left := time.Until(time.UnixMilli(p.ExpiresAt)).Round(time.Second)
			if p.Code == "" {
				fmt.Fprintf(env.Stdout, "%s  (waiting for the browser to show its code)  expires in %s\n", p.Label, left)
				continue
			}
			fmt.Fprintf(env.Stdout, "%s  %s  expires in %s\n", relay.FormatPairCode(p.Code), p.Label, left)
		}
		fmt.Fprintln(env.Stdout, "\nCheck the code matches the one your browser shows, then: maestria devices approve <code>")
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
		d, err := dm.Approve(code)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Approved %q (device %s). It may send commands for %d days, or until it has been idle %d.\n",
			d.Label, d.ID[:8], cfg.Relay.ApprovalAbsoluteDays, cfg.Relay.IdleDays)
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
			fmt.Fprintf(env.Stdout, "%s  %-8s  %s  approved %s, last used %s\n", d.ID, d.State, d.Label,
				time.UnixMilli(d.ApprovedAt).Format("Jan 2 15:04"), time.UnixMilli(d.LastUsedAt).Format("Jan 2 15:04"))
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
