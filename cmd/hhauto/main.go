// hhauto — hh.ru automation CLI (docs/PLAN.md). The command tree and
// wiring live in internal/cli; this stub only carries the release-stamped
// version and delegates.
package main

import (
	"os"

	"hhauto/internal/cli"
)

// Injected at release time via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(cli.Execute(version, commit))
}
