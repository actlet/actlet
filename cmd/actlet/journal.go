package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/actlet/actlet/internal/journal"
)

const journalUsage = `Usage:
  actlet journal <command> [arguments]

Commands:
  inspect    Show a journal's header, batches and validity
  export     Export a journal's records

Journals are read without taking the writer lock, so a journal that is being
written can be inspected; it may then end in an incomplete batch.
`

func runJournal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, journalUsage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "inspect":
		return runJournalInspect(rest, stdout, stderr)
	case "export":
		return runJournalExport(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, journalUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "actlet journal: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, journalUsage)
		return 2
	}
}

// parseFileArgs parses flags followed by exactly one file argument.
func parseFileArgs(fs *flag.FlagSet, args []string, stderr io.Writer) (string, int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", 0, false
		}
		return "", 2, false
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "actlet journal %s: expected one journal file\n", fs.Name())
		fs.Usage()
		return "", 2, false
	}
	return fs.Arg(0), 0, true
}

// openJournal opens a journal for reading. The returned function closes it.
func openJournal(path string, opts journal.ReaderOptions) (*journal.Reader, int64, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, nil, err
	}
	r, err := journal.NewReader(f, st.Size(), opts)
	if err != nil {
		_ = f.Close()
		return nil, 0, nil, err
	}
	return r, st.Size(), func() { r.Close(); _ = f.Close() }, nil
}

func runJournalInspect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	summary := fs.Bool("summary", false, "print only the summary, not every batch")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: actlet journal inspect [-summary] <file>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Validate a journal and print its header, batches and committed prefix.")
		fmt.Fprintln(stderr, "Exits 1 if the journal is corrupt or unreadable. An incomplete or damaged")
		fmt.Fprintln(stderr, "final batch, which opening the journal would truncate, is reported but")
		fmt.Fprintln(stderr, "is not an error.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	path, code, ok := parseFileArgs(fs, args, stderr)
	if !ok {
		return code
	}

	r, size, closeJournal, err := openJournal(path, journal.ReaderOptions{})
	var fe *journal.FormatError
	if errors.As(err, &fe) {
		// The file header itself is invalid.
		fmt.Fprintf(stdout, "file:     %s\n", path)
		printInvalid(stdout, path, err, -1)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "actlet journal inspect: %v\n", err)
		return 1
	}
	defer closeJournal()

	h := r.Header()
	fmt.Fprintf(stdout, "file:     %s\n", path)
	fmt.Fprintf(stdout, "size:     %d bytes\n", size)
	fmt.Fprintf(stdout, "format:   version %d\n", h.Version)
	fmt.Fprintf(stdout, "session:  %s\n", h.SessionID)

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	if !*summary {
		fmt.Fprintln(stdout)
		fmt.Fprintln(tw, "batch\toffset\tfirst seq\tlast seq\trecords\tcodec\tstored\traw\t")
	}
	var batches, stored, raw uint64
	var records int64
	var endErr error
	for {
		b, err := r.Next()
		if err != nil {
			endErr = err
			break
		}
		batches++
		records += int64(b.Count)
		stored += uint64(b.FrameLen)
		raw += uint64(b.RawLen)
		if !*summary {
			fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%v\t%d\t%d\t\n",
				batches, b.Offset, b.FirstSeq, b.LastSeq(), b.Count, b.Codec, b.StoredLen, b.RawLen)
		}
	}
	_ = tw.Flush()

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "batches:  %d\n", batches)
	if records > 0 {
		fmt.Fprintf(stdout, "records:  %d (seq 1-%d)\n", records, r.NextSeq()-1)
	} else {
		fmt.Fprintf(stdout, "records:  0\n")
	}
	if raw > 0 {
		fmt.Fprintf(stdout, "payload:  %d bytes stored with framing, %d decoded (%.2fx)\n", stored, raw, float64(raw)/float64(stored))
	}
	fmt.Fprintf(stdout, "valid to: offset %d\n", r.Offset())

	if errors.Is(endErr, io.EOF) {
		fmt.Fprintln(stdout, "status:   ok")
		return 0
	}
	if errors.Is(endErr, journal.ErrTornTail) {
		fmt.Fprintf(stdout, "status:   ok, but opening the journal for writing would truncate the last %d bytes\n", size-r.Offset())
		fmt.Fprintf(stdout, "          (%v)\n", endErr)
		fmt.Fprintln(stdout, "hint:     this is what an interrupted write or a power loss leaves behind: the final")
		fmt.Fprintln(stdout, "          batch never fully reached the disk, so its records were most likely never")
		fmt.Fprintln(stdout, "          acknowledged.")
		return 0
	}

	printInvalid(stdout, path, endErr, records)
	return 1
}

// printInvalid prints the status of an invalid journal, where the problem is
// and what to do about it. intact is the number of records before the
// problem, or -1 if the file header itself is invalid.
func printInvalid(stdout io.Writer, path string, err error, intact int64) {
	fmt.Fprintf(stdout, "status:   INVALID: %v\n", err)
	var fe *journal.FormatError
	if !errors.As(err, &fe) {
		return
	}
	if intact < 0 {
		fmt.Fprintf(stdout, "problem:  in the file header, at offset %d\n", fe.Offset)
	} else {
		plural := "s"
		if intact == 1 {
			plural = ""
		}
		fmt.Fprintf(stdout, "problem:  at offset %d; %d intact record%s before it\n", fe.Offset, intact, plural)
	}
	switch {
	case errors.Is(fe, journal.ErrCorrupt):
		fmt.Fprintln(stdout, "hint:     data that was already synced is damaged, so the journal can't be opened for")
		fmt.Fprintln(stdout, "          writing. Keep a copy of the file before changing it.")
		if intact > 0 {
			fmt.Fprintf(stdout, "          Export the intact records with: actlet journal export --jsonl %s\n", path)
		}
	case errors.Is(fe, journal.ErrLimit):
		fmt.Fprintln(stdout, "hint:     a batch is larger than this build accepts. It may have been written with")
		fmt.Fprintln(stdout, "          larger limits; read it with a build configured the same way.")
	case errors.Is(fe, journal.ErrUnsupported):
		fmt.Fprintln(stdout, "hint:     the journal uses a format version, codec or checksum this build doesn't know.")
		fmt.Fprintln(stdout, "          It was probably written by a newer version of actlet.")
	}
}

// exportRecord is one line of the JSONL export. Data is emitted as a string
// when it is valid UTF-8 and as base64 otherwise.
type exportRecord struct {
	Seq        uint64    `json:"seq"`
	Kind       string    `json:"kind"`
	Time       time.Time `json:"time"`
	Data       *string   `json:"data,omitempty"`
	DataBase64 []byte    `json:"data_base64,omitempty"`
}

func runJournalExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonl := fs.Bool("jsonl", false, "export as JSON Lines, one record per line (required)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: actlet journal export --jsonl <file>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Write every record of the journal's valid prefix to standard output.")
		fmt.Fprintln(stderr, "Each line holds seq, kind, time and data (or data_base64 for binary data).")
		fmt.Fprintln(stderr, "Exits 1 if the journal is corrupt, after exporting the records before")
		fmt.Fprintln(stderr, "the corruption.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	path, code, ok := parseFileArgs(fs, args, stderr)
	if !ok {
		return code
	}
	if !*jsonl {
		fmt.Fprintln(stderr, "actlet journal export: choose an output format: --jsonl")
		return 2
	}

	r, _, closeJournal, err := openJournal(path, journal.ReaderOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "actlet journal export: %v\n", err)
		return 1
	}
	defer closeJournal()

	out := bufio.NewWriter(stdout)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, journal.ErrTornTail) {
			fmt.Fprintf(stderr, "actlet journal export: stopped at an incomplete final batch: %v\n", err)
			break
		}
		if err != nil {
			_ = out.Flush()
			fmt.Fprintf(stderr, "actlet journal export: %v\n", err)
			return 1
		}
		for _, rec := range b.Records {
			line := exportRecord{Seq: rec.Seq, Kind: rec.Kind, Time: rec.Time.UTC()}
			if utf8.Valid(rec.Data) {
				s := string(rec.Data)
				line.Data = &s
			} else {
				line.DataBase64 = rec.Data
			}
			if err := enc.Encode(line); err != nil {
				fmt.Fprintf(stderr, "actlet journal export: %v\n", err)
				return 1
			}
		}
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintf(stderr, "actlet journal export: %v\n", err)
		return 1
	}
	return 0
}
