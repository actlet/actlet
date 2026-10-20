// Command actlet is the Actlet agent runtime: it runs one session in its
// session folder, under the control of an orchestrator's gateway.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/actlet/actlet/internal/buildinfo"
	"github.com/actlet/actlet/internal/supervise"
)

const usage = `Actlet is a lightweight agent runtime for isolated containers and sandboxes.

Usage:
  actlet <command> [arguments]

Commands:
  run        Run a session: connect to the gateway and run its task
  attach     Watch the box running in a session folder, and send notes
  version    Print version information
  help       Print this help

Run "actlet <command> -h" for help on a command. Offline tools for
journals and recordings are in actlet-cli.

Exit codes of "actlet run":
  0  the task completed
  1  the run failed; it can be run again
  2  usage error
  3  the session paused; run again on the same folder to resume
  4  the session was stopped; it won't resume
`

func main() {
	if supervise.ShouldInit() {
		// The container's first process: reap orphans and pass signals on
		// to a copy of this command, which does the work.
		os.Exit(supervise.Init())
	}
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
	case "run":
		return runRun(rest, stdout, stderr, nil)
	case "attach":
		return runAttach(rest, stdout, stderr)
	case "version":
		return buildinfo.RunVersion("actlet", rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "journal":
		fmt.Fprintln(stderr, `actlet: "journal" has moved to actlet-cli: use "actlet-cli journal ..."`)
		return 2
	default:
		fmt.Fprintf(stderr, "actlet: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return 2
	}
}
