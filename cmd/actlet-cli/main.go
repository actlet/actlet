// Command actlet-cli holds Actlet's tools for people: inspecting,
// exporting and playing back session journals and activity recordings,
// offline, and attaching the terminal viewer to a running session. It
// never runs a session.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/actlet/actlet/internal/buildinfo"
)

const usage = `actlet-cli works with Actlet's files offline, and attaches to running
sessions.

Usage:
  actlet-cli <command> [arguments]

Commands:
  attach     Watch and steer a running session, through the orchestrator or directly
  journal    Inspect and export session journals (.actlet)
  replay     Inspect, export and play activity recordings (.actrec)
  version    Print version information
  help       Print this help

Run "actlet-cli <command> help" for the subcommands.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "attach":
		return runAttach(rest, stdout, stderr)
	case "journal":
		return runJournal(rest, stdout, stderr)
	case "replay":
		return runReplay(rest, stdout, stderr)
	case "version":
		return buildinfo.RunVersion("actlet-cli", rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "actlet-cli: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return 2
	}
}
