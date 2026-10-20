package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/view"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const viewUsage = `Usage: actlet-cli replay view --json <file>

Fold a recording's events into the session's view, as the terminal viewer
shows it at the end, and write it as JSON in the form the web viewer uses,
to compare the two. The file is an activity recording (.actrec) or its
events as JSON Lines, as replay export --jsonl writes them. Exits 1 if it is
corrupt, after folding the events before the damage.

Flags:
`

// maxJSONLine bounds a line of an exported recording: an event, protobuf
// JSON encoded, at the journal's largest record.
var maxJSONLine = 2*journal.DefaultLimits().MaxRecordData + 1024

func runReplayView(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay view", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "write the view as JSON (required)")
	fs.Usage = func() {
		fmt.Fprint(stderr, viewUsage)
		fs.PrintDefaults()
	}
	path, code, ok := parseFileArgs(fs, args, stderr)
	if !ok {
		return code
	}
	if !*asJSON {
		fmt.Fprintln(stderr, "actlet-cli replay view: choose an output format: --json")
		return 2
	}
	s := view.New()
	var endErr error
	if isJSONL(path) {
		endErr = readJSONL(path, s.Apply)
	} else {
		r, _, closeJournal, err := openJournal(path, journal.ReaderOptions{})
		if err != nil {
			fmt.Fprintf(stderr, "actlet-cli replay view: %v\n", err)
			return 1
		}
		defer closeJournal()
		endErr = readEvents(r, s.Apply)
		if errors.Is(endErr, io.EOF) {
			endErr = nil
		} else if errors.Is(endErr, journal.ErrTornTail) {
			fmt.Fprintf(stderr, "actlet-cli replay view: stopped at an incomplete final batch: %v\n", endErr)
			endErr = nil
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s.ViewState()); err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay view: %v\n", err)
		return 1
	}
	if endErr != nil {
		fmt.Fprintf(stderr, "actlet-cli replay view: %v\n", endErr)
		return 1
	}
	return 0
}

// isJSONL reports whether a file holds exported events rather than a
// recording: by its name, or by its starting with a brace.
func isJSONL(path string) bool {
	if strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".json") {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	_, err = f.Read(b[:])
	return err == nil && b[0] == '{'
}

// readJSONL reads events written by replay export --jsonl and calls fn
// for each, skipping blank lines.
func readJSONL(path string, fn func(*gatewayv0.ActivityEvent)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxJSONLine)
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	for line := 1; sc.Scan(); line++ {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		ev := new(gatewayv0.ActivityEvent)
		if err := opts.Unmarshal(b, ev); err != nil {
			return fmt.Errorf("line %d is not an activity event: %w", line, err)
		}
		fn(ev)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}
