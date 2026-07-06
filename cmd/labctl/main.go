// Command labctl is the agent-side CLI; all logic lives in internal/labctl
// so it stays testable.
package main

import (
	"os"

	"git.cloonar.com/Cloonar/coding-lab/internal/labctl"
)

// version is stamped via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(labctl.Run(os.Args[1:], labctl.Env{
		Getenv:  os.Getenv,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
	}))
}
