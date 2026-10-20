// Command actlet is the Actlet agent runtime.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
)

// Build information, injected at link time:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.commit=abc1234 -X main.date=2026-01-01T00:00:00Z"
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const usage = `Actlet is a lightweight agent runtime for isolated containers and sandboxes.

Usage:
  actlet <command> [arguments]

Commands:
  run        Run a session: connect to the gateway and run its task
  journal    Inspect and export session journals
  version    Print version information
  help       Print this help

Run "actlet <command> -h" for help on a command.
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
	case "run":
		return runRun(rest, stdout, stderr, nil)
	case "journal":
		return runJournal(rest, stdout, stderr)
	case "version":
		return runVersion(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "actlet: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: actlet version")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Print the version, commit, build date and platform of this binary.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "actlet version: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	fmt.Fprintf(stdout, "actlet %s\n", version)
	fmt.Fprintf(stdout, "  commit:   %s\n", commit)
	fmt.Fprintf(stdout, "  built:    %s\n", date)
	fmt.Fprintf(stdout, "  go:       %s\n", runtime.Version())
	fmt.Fprintf(stdout, "  platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return 0
}
