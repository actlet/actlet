package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/search"
	"github.com/actlet/actlet/internal/view"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const searchUsage = `Usage: actlet-cli replay search [flags] <file> [text]

Find the events of a recording (.actrec), or of its export as JSON Lines,
that match: the text anywhere in an event, regardless of case, or with
--regex a regular expression, and the filters. It reads the recording as
a stream, so a recording of any length takes little memory.

Each match is printed in a line, as the event log shows it, or with --json
as the event itself in JSON. Exits 0 if something matched, 1 if nothing
did, and 2 on an error.

Flags:
`

func runReplaySearch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, searchUsage)
		fs.PrintDefaults()
	}
	kinds := fs.String("kind", "", "event kinds, comma-separated, such as tool_finished,model_turn")
	worker := fs.String("worker", "", "events of this worker, such as root.1")
	tool := fs.String("tool", "", "the start and end of this tool's calls")
	since := fs.String("since", "", "events at or after this time: RFC 3339, or a duration after the recording's first event, such as 2m")
	until := fs.String("until", "", "events at or before this time, as --since")
	isRegexp := fs.Bool("regex", false, "the text is a regular expression")
	asJSON := fs.Bool("json", false, "print the matching events as JSON, one per line")
	limit := fs.Int("limit", 0, "print at most this many matches (default: all)")
	var pos []string
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
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return 2
	}
	path := pos[0]
	f := &search.Filter{Worker: *worker, Tool: *tool}
	if len(pos) == 2 {
		f.Text = pos[1]
	}
	if *isRegexp {
		re, err := regexp.Compile(f.Text)
		if err != nil {
			fmt.Fprintf(stderr, "actlet-cli replay search: %v\n", err)
			return 2
		}
		f.Regexp = re
	}
	if *kinds != "" {
		known := search.Kinds()
		for k := range strings.SplitSeq(*kinds, ",") {
			k = strings.TrimSpace(k)
			if !slices.Contains(known, k) {
				fmt.Fprintf(stderr, "actlet-cli replay search: --kind %q: use %s\n", k, strings.Join(known, ", "))
				return 2
			}
			f.Kinds = append(f.Kinds, k)
		}
	}
	sinceAt, sinceRel, err := parseWhen(*since)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay search: --since: %v\n", err)
		return 2
	}
	untilAt, untilRel, err := parseWhen(*until)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay search: --until: %v\n", err)
		return 2
	}
	f.Since, f.Until = sinceAt, untilAt

	out := bufio.NewWriter(stdout)
	defer func() { _ = out.Flush() }()
	opts := protojson.MarshalOptions{UseProtoNames: true}
	matches, first := 0, true
	var writeErr error
	match := func(ev *gatewayv0.ActivityEvent) {
		if first {
			// Durations count from the recording's first event.
			first = false
			start := ev.GetTime().AsTime()
			if sinceRel >= 0 {
				f.Since = start.Add(sinceRel)
			}
			if untilRel >= 0 {
				f.Until = start.Add(untilRel)
			}
		}
		if writeErr != nil || (*limit > 0 && matches >= *limit) || !f.Match(ev) {
			return
		}
		matches++
		if *asJSON {
			b, err := opts.Marshal(ev)
			if err == nil {
				_, err = out.Write(append(b, '\n'))
			}
			writeErr = err
			return
		}
		who := ev.GetWorker()
		if who == "" {
			who = "session"
		}
		_, writeErr = fmt.Fprintf(out, "%s  %s  %-8s %s\n", ev.GetId(), ev.GetTime().AsTime().UTC().Format(time.RFC3339Nano),
			who, view.Sanitize(firstLineOf(view.Describe(ev))))
	}
	var endErr error
	if isJSONL(path) {
		endErr = readJSONL(path, match)
	} else {
		r, _, closeJournal, err := openJournal(path, journal.ReaderOptions{})
		if err != nil {
			fmt.Fprintf(stderr, "actlet-cli replay search: %v\n", err)
			return 2
		}
		defer closeJournal()
		endErr = readEvents(r, match)
		if errors.Is(endErr, io.EOF) || errors.Is(endErr, journal.ErrTornTail) {
			endErr = nil
		}
	}
	if writeErr == nil {
		writeErr = out.Flush()
	}
	for _, err := range []error{writeErr, endErr} {
		if err != nil {
			fmt.Fprintf(stderr, "actlet-cli replay search: %v\n", err)
			return 2
		}
	}
	if matches == 0 {
		return 1
	}
	return 0
}

// parseWhen reads a time: RFC 3339, or a duration after the recording's
// start, returned as rel; rel is -1 for none.
func parseWhen(s string) (at time.Time, rel time.Duration, err error) {
	if s == "" {
		return time.Time{}, -1, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return time.Time{}, d, nil
	}
	at, err = time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, -1, fmt.Errorf("%q: use RFC 3339, such as 2026-10-21T09:30:00Z, or a duration, such as 2m", s)
	}
	return at, -1, nil
}

func firstLineOf(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
