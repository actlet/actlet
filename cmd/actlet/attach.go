package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tui"
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
	var separators tui.BoolFlag
	fs.Var(&separators, "separators", "draw lines between turns and tool calls; on unless the profile says otherwise [ACTLET_TUI_SEPARATORS]")
	theme := fs.String("theme", "", "the colours: dark, light, plain, a theme file's name or its path [ACTLET_THEME]")
	color := fs.Bool("color", false, "with -dump: keep the colours, as ANSI escape sequences")
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
	conn := tui.Connection{Kind: "local box", Target: absPath(*dir), Dir: absPath(*dir), Attached: "local socket"}
	look := tui.ResolveLook(*theme, separators.Value(), os.Getenv)
	if *dump {
		out, err := tui.DumpLive(context.Background(), client, conn, tui.DumpOptions{
			Tab: t, Width: *width, Height: *height, File: *file, Look: look, Color: *color,
		})
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
	if err := tui.RunLive(context.Background(), client, conn, viewerName(), look); err != nil {
		fmt.Fprintf(stderr, "actlet attach: %v\n", err)
		return 1
	}
	return 0
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
