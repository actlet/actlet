package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/journal"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const replayUsage = `Usage:
  actlet-cli replay <command> [arguments]

Commands:
  inspect    Summarise an activity recording: epochs, event kinds, gaps
  export     Export its events
  play       Play it back in a terminal viewer
  view       Write the session's view at the end as JSON

Activity recordings (.actrec) are written by gateways, and by a worker that
spills activity to its session folder while the gateway is unreachable.
`

func runReplay(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, replayUsage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "inspect":
		return runReplayInspect(rest, stdout, stderr)
	case "export":
		return runReplayExport(rest, stdout, stderr)
	case "play":
		return runReplayPlay(rest, stdout, stderr)
	case "view":
		return runReplayView(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, replayUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "actlet-cli replay: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, replayUsage)
		return 2
	}
}

// readEvents reads the activity events of a recording in order and calls fn
// for each. It returns the error that ended the recording: io.EOF, a torn
// tail, or a problem with the file.
func readEvents(r *journal.Reader, fn func(*gatewayv0.ActivityEvent)) error {
	for {
		b, err := r.Next()
		if err != nil {
			return err
		}
		for _, rec := range b.Records {
			ev := new(gatewayv0.ActivityEvent)
			if err := proto.Unmarshal(rec.Data, ev); err != nil {
				return fmt.Errorf("record %d is not an activity event: %w", rec.Seq, err)
			}
			fn(ev)
		}
	}
}

// eventKind names an event's kind after its body's field in the
// protocol, such as "text" or "model_turn". A body this build doesn't know,
// from a newer worker, is "unknown".
func eventKind(ev *gatewayv0.ActivityEvent) string {
	m := ev.ProtoReflect()
	if fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("body")); fd != nil {
		return string(fd.Name())
	}
	return "unknown"
}

type epochSummary struct {
	first, last     uint64
	events, holes   uint64
	firstAt, lastAt time.Time
}

func runReplayInspect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: actlet-cli replay inspect <file>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Summarise an activity recording. Exits 1 if it is corrupt or unreadable.")
	}
	path, code, ok := parseFileArgs(fs, args, stderr)
	if !ok {
		return code
	}
	r, size, closeJournal, err := openJournal(path, journal.ReaderOptions{})
	var fe *journal.FormatError
	if errors.As(err, &fe) {
		fmt.Fprintf(stdout, "file:     %s\n", path)
		printInvalid(stdout, path, err, -1)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay inspect: %v\n", err)
		return 1
	}
	defer closeJournal()

	epochs := map[uint64]*epochSummary{}
	kinds := map[string]int{}
	workers := map[string]int{}
	var total, dropped, gaps int64
	endErr := readEvents(r, func(ev *gatewayv0.ActivityEvent) {
		total++
		kinds[eventKind(ev)]++
		if w := ev.GetWorker(); w != "" {
			workers[w]++
		}
		if g := ev.GetGap(); g != nil {
			gaps++
			dropped += int64(g.GetDropped())
		}
		e := epochs[ev.GetEpoch()]
		at := ev.GetTime().AsTime()
		if e == nil {
			e = &epochSummary{first: ev.GetSeq(), firstAt: at}
			epochs[ev.GetEpoch()] = e
		} else if ev.GetSeq() > e.last+1 {
			e.holes += ev.GetSeq() - e.last - 1
		}
		e.last, e.lastAt = max(e.last, ev.GetSeq()), at
		e.events++
	})

	fmt.Fprintf(stdout, "file:     %s\n", path)
	fmt.Fprintf(stdout, "size:     %d bytes\n", size)
	fmt.Fprintf(stdout, "session:  %s\n", r.Header().SessionID)
	fmt.Fprintf(stdout, "events:   %d in %d epochs\n", total, len(epochs))
	ids := make([]uint64, 0, len(epochs))
	for id := range epochs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		e := epochs[id]
		holes := ""
		if e.holes > 0 {
			holes = fmt.Sprintf(", %d missing", e.holes)
		}
		fmt.Fprintf(stdout, "  epoch %d: seq %d-%d (%d events%s), %s to %s\n", id, e.first, e.last, e.events, holes,
			e.firstAt.UTC().Format(time.RFC3339), e.lastAt.UTC().Format(time.RFC3339))
	}
	if len(kinds) > 0 {
		fmt.Fprintf(stdout, "kinds:    %s\n", counts(kinds))
	}
	if len(workers) > 0 {
		fmt.Fprintf(stdout, "workers:  %s\n", counts(workers))
	}
	if gaps > 0 {
		fmt.Fprintf(stdout, "dropped:  %d display-only events, reported by %d gap events\n", dropped, gaps)
	}

	switch {
	case errors.Is(endErr, io.EOF):
		fmt.Fprintln(stdout, "status:   ok")
		return 0
	case errors.Is(endErr, journal.ErrTornTail):
		fmt.Fprintf(stdout, "status:   ok, but the last %d bytes are an incomplete final batch\n", size-r.Offset())
		return 0
	default:
		printInvalid(stdout, path, endErr, total)
		return 1
	}
}

// counts lists counts by name, sorted by name.
func counts(m map[string]int) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func runReplayExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonl := fs.Bool("jsonl", false, "export as JSON Lines, one event per line (required)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: actlet-cli replay export --jsonl <file>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Write every event of the recording to standard output as protobuf JSON,")
		fmt.Fprintln(stderr, "one per line. Exits 1 if the recording is corrupt, after exporting the")
		fmt.Fprintln(stderr, "events before the damage.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	path, code, ok := parseFileArgs(fs, args, stderr)
	if !ok {
		return code
	}
	if !*jsonl {
		fmt.Fprintln(stderr, "actlet-cli replay export: choose an output format: --jsonl")
		return 2
	}
	r, _, closeJournal, err := openJournal(path, journal.ReaderOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay export: %v\n", err)
		return 1
	}
	defer closeJournal()

	out := bufio.NewWriter(stdout)
	opts := protojson.MarshalOptions{UseProtoNames: true}
	var writeErr error
	endErr := readEvents(r, func(ev *gatewayv0.ActivityEvent) {
		if writeErr != nil {
			return
		}
		b, err := opts.Marshal(ev)
		if err == nil {
			_, err = out.Write(append(b, '\n'))
		}
		writeErr = err
	})
	if err := out.Flush(); err != nil && writeErr == nil {
		writeErr = err
	}
	if writeErr != nil {
		fmt.Fprintf(stderr, "actlet-cli replay export: %v\n", writeErr)
		return 1
	}
	switch {
	case errors.Is(endErr, io.EOF):
		return 0
	case errors.Is(endErr, journal.ErrTornTail):
		fmt.Fprintf(stderr, "actlet-cli replay export: stopped at an incomplete final batch: %v\n", endErr)
		return 0
	default:
		fmt.Fprintf(stderr, "actlet-cli replay export: %v\n", endErr)
		return 1
	}
}
