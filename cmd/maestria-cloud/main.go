// Command maestria-cloud is the hosted multi-tenant server: accounts, connector linking, the
// relay and device pairing. It stores no workspace data. Run it behind a reverse proxy or tunnel.
package main

import (
	"os"

	"github.com/paulo-lucas/maestria/internal/cloud"
	"github.com/paulo-lucas/maestria/internal/cloudupdate"
)

// Build metadata injected at build time:
// -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-11-14T00:00:00Z".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	// `maestria-cloud update` replaces this binary from a GitHub release; it is not part of the
	// server, so it is handled here rather than in internal/cloud.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(cloudupdate.Run(os.Args[2:], cloudupdate.DefaultEnv(version)))
	}
	env := cloud.DefaultEnv(version)
	env.Commit = commit
	env.Date = date
	os.Exit(cloud.Run(os.Args[1:], env))
}
