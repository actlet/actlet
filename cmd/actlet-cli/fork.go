package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/actlet/actlet/sdk"
)

const forkUsage = `Usage:
  actlet-cli fork [flags] <session-folder> --to <new-folder>
  actlet-cli fork --list <session-folder>

Start a new session from a checkpoint of another: the new folder gets a
journal that holds the checkpoint and where it comes from, and the
parent's task.json. "actlet run" on it continues from the checkpoint's
conversation, as a session of its own, with its own ID; it tells the
gateway the session it was forked from. The parent is only read, and may
be running, finished or crashed.

Checkpoints are taken at model-turn boundaries. Calls that were running
at the checkpoint are recovered as after a crash; nothing after it is
copied. Files are not snapshotted: --copy-workspace copies the parent's
inputs/ and workspace/ as they are now, which may include changes made
after the checkpoint.

Flags:
`

func runFork(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, forkUsage)
		fs.PrintDefaults()
	}
	list := fs.Bool("list", false, "list the session's checkpoints")
	at := fs.String("checkpoint", "latest", `the checkpoint: "latest", a journal sequence number, or "turn N" for the latest at the agent's model turn N`)
	to := fs.String("to", "", "the new session's folder, which must be empty or not exist")
	id := fs.String("session-id", "", "the new session's ID (default: a new random one)")
	copyWorkspace := fs.Bool("copy-workspace", false, "copy inputs/ and workspace/ as they are now")
	// Flags may come before or after the folder.
	var folders []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		folders = append(folders, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(folders) != 1 {
		fs.Usage()
		return 2
	}
	parent := folders[0]
	if *list {
		cps, err := sdk.SessionCheckpoints(parent)
		if err != nil {
			fmt.Fprintf(stderr, "actlet-cli fork: %v\n", err)
			return 1
		}
		if len(cps) == 0 {
			fmt.Fprintln(stdout, "no checkpoints")
			return 0
		}
		fmt.Fprintf(stdout, "%-8s %-6s %-8s %-7s %s\n", "SEQ", "TURN", "WORKERS", "PACKED", "TIME")
		for _, c := range cps {
			fmt.Fprintf(stdout, "%-8d %-6d %-8d %-7v %s\n", c.Seq, c.Turn, c.Workers, c.Packed, c.Time.UTC().Format(time.RFC3339))
		}
		return 0
	}
	if *to == "" {
		fmt.Fprintln(stderr, "actlet-cli fork: give the new session's folder with --to")
		return 2
	}
	opts := sdk.ForkOptions{SessionID: *id, CopyWorkspace: *copyWorkspace}
	switch spec := strings.TrimSpace(*at); {
	case spec == "latest":
	case strings.HasPrefix(spec, "turn"):
		n, err := strconv.ParseUint(strings.TrimLeft(spec[len("turn"):], " :="), 10, 64)
		if err != nil || n == 0 {
			fmt.Fprintf(stderr, "actlet-cli fork: --checkpoint %q: use \"turn N\" with N from 1\n", spec)
			return 2
		}
		opts.Turn = n
	default:
		n, err := strconv.ParseUint(spec, 10, 64)
		if err != nil || n == 0 {
			fmt.Fprintf(stderr, "actlet-cli fork: --checkpoint %q: use \"latest\", a sequence number or \"turn N\"\n", spec)
			return 2
		}
		opts.Seq = n
	}
	f, err := sdk.ForkSession(parent, *to, opts)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-cli fork: %v\n", err)
		return 1
	}
	l := f.Lineage
	fmt.Fprintf(stdout, "forked session %s at checkpoint %d (turn %d) into %s as session %s\n",
		l.GetParentSessionId(), l.GetCheckpointSeq(), l.GetTurn(), *to, f.SessionID)
	if *copyWorkspace {
		fmt.Fprintln(stdout, "copied inputs/ and workspace/ as they are now, which may differ from the checkpoint's")
	} else {
		fmt.Fprintln(stdout, "inputs/ and workspace/ are empty: copy what the task needs, or fork with --copy-workspace")
	}
	return 0
}
