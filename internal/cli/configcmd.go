package cli

import (
	"fmt"
	"strings"

	"github.com/paulo-lucas/maestria/internal/config"
)

const configUsage = `Usage:
  maestria config hosts                  List the extra host names the panel answers to
  maestria config allow-host <host>      Let the panel answer to <host> (e.g. a Cloudflare Tunnel hostname)
  maestria config disallow-host <host>   Remove <host> from the allowed hosts

The panel always answers to localhost and 127.0.0.1. Restart it after a change
(maestria service restart, or stop and start maestria serve).`

func cmdConfig(args []string, env *Env) error {
	if len(args) == 0 {
		return usagef(configUsage)
	}
	dir := configDir(env)
	switch args[0] {
	case "hosts":
		if len(args) != 1 {
			return usagef(configUsage)
		}
		cfg, err := config.Load(dir, env.Config)
		if err != nil {
			return err
		}
		if len(cfg.AllowedHosts) == 0 {
			fmt.Fprintln(env.Stdout, "No extra hosts: the panel answers to localhost and 127.0.0.1 only.")
			return nil
		}
		for _, h := range cfg.AllowedHosts {
			fmt.Fprintln(env.Stdout, h)
		}
		return nil
	case "allow-host", "disallow-host":
		if len(args) != 2 {
			return usagef(configUsage)
		}
		host := normalizeHostArg(args[1])
		if !config.ValidHost(host) {
			return usagef("%q is not a valid host name (example: panel.example.com or panel.example.com:8443)", args[1])
		}
		cfg, err := config.Load(dir, env.Config)
		if err != nil {
			return err
		}
		hosts := append([]string(nil), cfg.AllowedHosts...)
		if args[0] == "allow-host" {
			for _, h := range hosts {
				if h == host {
					fmt.Fprintf(env.Stdout, "%s is already allowed.\n", host)
					return nil
				}
			}
			hosts = append(hosts, host)
		} else {
			kept := hosts[:0]
			found := false
			for _, h := range hosts {
				if h == host {
					found = true
					continue
				}
				kept = append(kept, h)
			}
			if !found {
				fmt.Fprintf(env.Stdout, "%s was not allowed.\n", host)
				return nil
			}
			hosts = kept
		}
		if err := config.SaveAllowedHosts(dir, hosts, env.Config); err != nil {
			return err
		}
		verb := "now answers to"
		if args[0] == "disallow-host" {
			verb = "no longer answers to"
		}
		fmt.Fprintf(env.Stdout, "The panel %s %s. Restart it to apply (maestria service restart, or restart maestria serve).\n", verb, host)
		if args[0] == "allow-host" && !cfg.CookieSecure {
			fmt.Fprintln(env.Stdout, `Note: "cookieSecure" is false in config.json. Set it to true when the host is served over HTTPS.`)
		}
		return nil
	}
	return usagef("Unknown config command %q\n\n%s", args[0], configUsage)
}

// normalizeHostArg accepts a pasted URL ("https://panel.example.com/") and keeps the host part.
func normalizeHostArg(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	for _, p := range []string{"https://", "http://"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}
