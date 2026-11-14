// Command maestria is the Maestria control panel and CLI.
package main

import (
	"os"

	"github.com/maestria-dev/maestria/internal/cli"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.DefaultEnv(version)))
}
