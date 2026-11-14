// Command maestria-cloud is the hosted multi-tenant server: accounts, connector linking, the
// relay and device pairing. It stores no workspace data. Run it behind a reverse proxy or tunnel.
package main

import (
	"os"

	"github.com/maestria-dev/maestria/internal/cloud"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cloud.Run(os.Args[1:], cloud.DefaultEnv(version)))
}
