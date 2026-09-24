package main

import (
	"fmt"
	"os"
)

// Bootstrap stub — replaced by internal/cli wiring in the CLI task.
// Version is injected at release time via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("hhauto %s (%s)\n", version, commit)
		return
	}
	fmt.Println("hhauto: implementation in progress — see docs/PLAN.md")
	os.Exit(1)
}
