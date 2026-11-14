// Command maestria is the Maestria control panel and CLI.
package main

import (
	"os"

	"github.com/maestria-dev/maestria/internal/cli"
)

// Build metadata injected at build time:
// -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-11-14T00:00:00Z".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	env := cli.DefaultEnv(version)
	env.Commit = commit
	env.Date = date
	os.Exit(cli.Run(os.Args[1:], env))
}
