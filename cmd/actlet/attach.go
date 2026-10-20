package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tui"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
)

const attachUsage = `Usage: actlet attach [flags]

Attach a terminal viewer to the box running in a session folder: watch its
activity and send the agent notes. Detaching (q) never affects the session.

Flags:
`

func runAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, attachUsage)
		fs.PrintDefaults()
	}
	dir := fs.String("session", envDefault("ACTLET_SESSION", "."), "session folder [ACTLET_SESSION]")
	dump := fs.Bool("dump", false, "print the screen as text once and exit, without a terminal")
	tab := fs.String("tab", "chat", "with -dump: the view to print: chat, session, task, profile, tools or events")
	file := fs.String("file", "", "with -dump: open this file, as the sidebar shows it, in the file viewer")
	separators := fs.Bool("separators", false, "draw lines between turns and tool calls")
	width := fs.Int("width", 120, "with -dump: the screen width")
	height := fs.Int("height", 40, "with -dump: the screen height")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	t, ok := tui.TabByName[*tab]
	if !ok || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	state := sessionfs.Folder{Dir: *dir}.State()
	client, closeClient, err := attach.Dial(state)
	if err != nil {
		fmt.Fprintf(stderr, "actlet attach: %v\n", err)
		return 1
	}
	defer func() { _ = closeClient() }()
	conn := tui.Connection{Kind: "local box", Target: absPath(*dir), Dir: absPath(*dir)}
	if *dump {
		out, err := dumpLive(context.Background(), client, conn, t, *width, *height, *file, *separators)
		if err != nil {
			fmt.Fprintf(stderr, "actlet attach: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, out)
		return 0
	}
	if err := tui.CheckTerminal(); err != nil {
		fmt.Fprintf(stderr, "actlet attach: %v; or use -dump to print the screen as text\n", err)
		return 2
	}
	if err := tui.RunLive(context.Background(), client, conn, viewerName()); err != nil {
		fmt.Fprintf(stderr, "actlet attach: %v\n", err)
		return 1
	}
	return 0
}

// dumpLive renders a box's screen once: what it holds now.
func dumpLive(ctx context.Context, client attachv0.AttachClient, conn tui.Connection, tab, w, h int, file string, separators bool) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := tui.NewLive(conn, nil, "")
	m.SetSize(w, h)
	m.SetTab(tab)
	m.SetSeparators(separators)
	m.SetFileReader(tui.Files(ctx, client, conn.Dir))
	msgs := make(chan tea.Msg, 64)
	errc := make(chan error, 1)
	go func() { errc <- tui.Feed(ctx, client, func(msg tea.Msg) { msgs <- msg }) }()
	// Take what arrives until it goes quiet: the backlog comes at once.
	idle := time.NewTimer(2 * time.Second)
	for {
		select {
		case msg := <-msgs:
			m.Update(msg)
			idle.Reset(300 * time.Millisecond)
		case err := <-errc:
			if err != nil && m.Session().Events == 0 {
				return "", err
			}
			if file != "" {
				m.Open(file)
			}
			return m.Dump(), nil
		case <-idle.C:
			if file != "" {
				m.Open(file)
			}
			return m.Dump(), nil
		}
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// viewerName names a local viewer in the commands it sends.
func viewerName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username + " (local tui)"
	}
	return "local tui"
}
