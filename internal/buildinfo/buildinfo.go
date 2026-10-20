// Package buildinfo holds the version information of the binaries, injected
// at link time:
//
//	go build -ldflags "-X github.com/actlet/actlet/internal/buildinfo.Version=v0.1.0 ..."
package buildinfo

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
)

// Set at link time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// RunVersion implements the "version" command of a binary named name.
func RunVersion(name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s version\n\n", name)
		fmt.Fprintln(stderr, "Print the version, commit, build date and platform of this binary.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s version: unexpected argument %q\n", name, fs.Arg(0))
		return 2
	}
	fmt.Fprintf(stdout, "%s %s\n", name, Version)
	fmt.Fprintf(stdout, "  commit:   %s\n", Commit)
	fmt.Fprintf(stdout, "  built:    %s\n", Date)
	fmt.Fprintf(stdout, "  go:       %s\n", runtime.Version())
	fmt.Fprintf(stdout, "  platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return 0
}
