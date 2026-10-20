package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/actlet/actlet/internal/tui"
)

const playUsage = `Usage: actlet-cli replay play [flags] <file>

Play an activity recording back in a terminal viewer: the session's
workers, conversation and tool calls as they happened. Seek, change the
speed, filter by worker or tool, and follow a recording that is still
being written. Nothing is run: no network, tools or models.

Keys: space play/pause, left/right seek, +/- speed, f follow, up/down select
a tool call, enter expand it, [ ] worker, t tool, tab views, ? keys, q quit.
The mouse works too: click tabs and tool calls, scroll with the wheel.

Flags:
`

func runReplayPlay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay play", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, playUsage)
		fs.PrintDefaults()
	}
	speed := fs.Float64("speed", 1, "playback speed")
	follow := fs.Bool("follow", false, "start at the end and show events as the recording grows")
	worker := fs.String("worker", "", "show only this worker, such as root.1")
	tool := fs.String("tool", "", "show only calls of this tool")
	at := fs.Int("at", -1, "start after this many events (default: the start, or the end with -dump)")
	dump := fs.Bool("dump", false, "print the screen as text and exit, without a terminal")
	tab := fs.String("tab", "chat", "the view to show: chat, session, task, profile, tools or events")
	expand := fs.Bool("expand", false, "with -dump: expand every tool call")
	file := fs.String("file", "", "with -dump: open this file, as the sidebar shows it, in the file viewer")
	separators := fs.Bool("separators", false, "draw lines between turns and tool calls")
	width := fs.Int("width", 120, "with -dump: the screen width")
	height := fs.Int("height", 40, "with -dump: the screen height")
	path, code, ok := parseFileArgs(fs, args, stderr)
	if !ok {
		return code
	}
	t, ok := tui.TabByName[*tab]
	if !ok || *speed <= 0 {
		fs.Usage()
		return 2
	}
	p, err := tui.OpenPlayer(path)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay play: %v\n", err)
		return 1
	}
	defer p.Close()
	if err := p.Err(); err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay play: the recording is damaged after %d events: %v\n", p.Len(), err)
	}
	start := *at
	if start < 0 && (*dump || *follow) {
		start = p.Len()
	}
	p.Seek(max(start, 0))
	m := tui.NewReplay(p)
	m.SetTab(t)
	m.SetFilter(*worker, *tool)
	m.ExpandAll(*expand)
	m.SetSeparators(*separators)
	if *dump {
		m.SetSize(*width, *height)
		if *file != "" {
			m.Open(*file)
		}
		fmt.Fprintln(stdout, m.Dump())
		return 0
	}
	if err := tui.CheckTerminal(); err != nil {
		fmt.Fprintf(stderr, "actlet-cli replay play: %v; or use -dump to print the screen as text\n", err)
		return 2
	}
	p.SetFollow(*follow)
	if err := tui.RunReplay(context.Background(), m, *speed); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "actlet-cli replay play: %v\n", err)
		return 1
	}
	return 0
}
