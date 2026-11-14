// Command maestria-cloud is the hosted multi-tenant server: accounts, connector linking, the
// relay and device pairing. It stores no workspace data. Run it behind a reverse proxy or tunnel.
package main

import (
	"os"

	"github.com/paulo-lucas/maestria/internal/cloud"
)

// Build metadata injected at build time:
// -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-11-14T00:00:00Z".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	env := cloud.DefaultEnv(version)
	env.Commit = commit
	env.Date = date
	os.Exit(cloud.Run(os.Args[1:], env))
}
