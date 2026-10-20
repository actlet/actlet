// Command actlet-cli holds Actlet's offline tools: inspecting and exporting
// session journals and activity recordings. It never runs a session and
// needs no gateway.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/actlet/actlet/internal/buildinfo"
)

const usage = `actlet-cli works with Actlet's files offline.

Usage:
  actlet-cli <command> [arguments]

Commands:
  journal    Inspect and export session journals (.actlet)
  replay     Inspect and export activity recordings (.actrec)
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
