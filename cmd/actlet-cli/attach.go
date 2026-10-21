package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/tui"
	"github.com/actlet/actlet/internal/view"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
)

const attachUsage = `Usage: actlet-cli attach -gateway <addr> [-session <id>] [flags]
       actlet-cli attach -direct <addr> [flags]

Attach the terminal viewer to a running session: watch its activity, open
its files, and send the agent notes or the commands you may. Detaching (q)
never affects the session.

  -gateway  through the orchestrator's attach service. Without -session,
            pick one of its sessions.
  -direct   straight to a box's attach port, for example one forwarded
            with: kubectl port-forward pod/<pod> 7600:7600

The token is the one the orchestrator or the box expects; prefer
-token-file or ACTLET_ATTACH_TOKEN to -token, which other users can see in
the process list.

Flags:
`

// stdin is where the session picker reads the choice.
var stdin io.Reader = os.Stdin

func runAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, attachUsage)
		fs.PrintDefaults()
	}
	gateway := fs.String("gateway", "", "the orchestrator's attach service, host:port")
	direct := fs.String("direct", "", "a box's attach port, host:port")
	sessionID := fs.String("session", "", "with -gateway: the session to attach to")
	token := fs.String("token", os.Getenv("ACTLET_ATTACH_TOKEN"), "bearer token [ACTLET_ATTACH_TOKEN]")
	tokenFile := fs.String("token-file", os.Getenv("ACTLET_ATTACH_TOKEN_FILE"),
		"file holding the bearer token; a trailing newline is ignored [ACTLET_ATTACH_TOKEN_FILE]")
	useTLS := fs.Bool("tls", false, "connect with TLS")
	caFile := fs.String("tls-ca", "", "PEM file of CA certificates to trust for TLS")
	dump := fs.Bool("dump", false, "print the screen as text once and exit, without a terminal")
	tab := fs.String("tab", "chat", "with -dump: the view to print: chat, session, task, profile, tools or events")
	file := fs.String("file", "", "with -dump: open this file, as the sidebar shows it, in the file viewer")
	var separators tui.BoolFlag
	fs.Var(&separators, "separators", "draw lines between turns and tool calls; on unless the profile says otherwise [ACTLET_TUI_SEPARATORS]")
	name := fs.String("name", "actlet-cli", "how this viewer is named to the session's other viewers")
	theme := fs.String("theme", "", "the colours: dark, light, plain, a theme file's name or its path [ACTLET_THEME]")
	color := fs.Bool("color", false, "with -dump: keep the colours, as ANSI escape sequences")
	keys := fs.Bool("keys", false, "with -dump: show the help panel, with the viewer's keys")
	width := fs.Int("width", 120, "with -dump: the screen width")
	height := fs.Int("height", 40, "with -dump: the screen height")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	t, ok := tui.TabByName[*tab]
	if !ok || fs.NArg() > 0 || (*gateway == "") == (*direct == "") {
		if ok && fs.NArg() == 0 {
			fmt.Fprintln(stderr, "actlet-cli attach: give one of -gateway and -direct")
		}
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "actlet-cli attach: %v\n", err)
		return 1
	}
	if !*dump {
		if err := tui.CheckTerminal(); err != nil {
			fmt.Fprintf(stderr, "actlet-cli attach: %v; or use -dump to print the screen as text\n", err)
			return 2
		}
	}
	if *token != "" && *tokenFile != "" {
		fmt.Fprintln(stderr, "actlet-cli attach: give a token or a token file, not both: -token or ACTLET_ATTACH_TOKEN, -token-file or ACTLET_ATTACH_TOKEN_FILE")
		return 2
	}
	if *tokenFile != "" {
		if _, err := gwclient.ReadToken(*tokenFile); err != nil {
			return fail(err)
		}
	}
	opts := attach.RemoteOptions{Token: *token, TokenFile: *tokenFile, TLS: *useTLS, CAFile: *caFile, Session: *sessionID}
	addr := *direct
	if *gateway != "" {
		addr = *gateway
		if opts.Session == "" {
			id, err := pickSession(addr, opts, *dump, stderr)
			if err != nil {
				return fail(err)
			}
			opts.Session = id
		}
	}
	client, closeClient, err := attach.DialRemote(addr, opts)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = closeClient() }()
	conn := tui.Connection{Kind: "box " + addr, Attached: "box " + addr, Viewer: *name}
	if *gateway != "" {
		conn.Kind = fmt.Sprintf("orchestrator %s · session %s", addr, shortID(opts.Session))
		conn.Attached = "orchestrator " + addr
	}
	look := tui.ResolveLook(*theme, separators.Value(), os.Getenv)
	ctx := context.Background()
	if *dump {
		out, err := tui.DumpLive(ctx, client, conn, tui.DumpOptions{Tab: t, Width: *width, Height: *height, File: *file, Look: look, Color: *color, Keys: *keys})
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, out)
		return 0
	}
	// Fail early, with the reason, rather than in the viewer.
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = client.Info(ictx, &attachv0.InfoRequest{})
	cancel()
	if err != nil {
		return fail(err)
	}
	if err := tui.RunLive(ctx, client, conn, viewerName(), look); err != nil {
		return fail(err)
	}
	return 0
}

// pickSession chooses one of the orchestrator's sessions: the only one,
// or the one the user picks from a list.
func pickSession(addr string, opts attach.RemoteOptions, noPrompt bool, stderr io.Writer) (string, error) {
	client, closeClient, err := attach.DialRemote(addr, opts)
	if err != nil {
		return "", err
	}
	defer func() { _ = closeClient() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := client.Sessions(ctx, &attachv0.SessionsRequest{})
	if err != nil {
		return "", err
	}
	sessions := list.GetSessions()
	switch {
	case len(sessions) == 0:
		return "", errors.New("the orchestrator has no sessions to attach to")
	case len(sessions) == 1:
		return sessions[0].GetSessionId(), nil
	}
	lines := make([]string, len(sessions))
	for i, s := range sessions {
		conn := "not connected"
		if s.GetConnected() {
			conn = "connected"
		}
		lines[i] = fmt.Sprintf("%3d  %s  worker %s  run %d  %s, %s", i+1, view.Sanitize(s.GetSessionId()), view.Sanitize(s.GetWorkerId()),
			s.GetEpoch(), view.StateName(s.GetState()), conn)
	}
	if noPrompt {
		return "", fmt.Errorf("choose a session with -session:\n%s", strings.Join(lines, "\n"))
	}
	fmt.Fprintf(stderr, "Sessions:\n%s\nAttach to which? ", strings.Join(lines, "\n"))
	line, _ := bufio.NewReader(io.LimitReader(stdin, 256)).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(sessions) {
		return "", errors.New("no session chosen")
	}
	return sessions[n-1].GetSessionId(), nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// viewerName names the viewer in the commands it sends.
func viewerName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username + " (actlet-cli)"
	}
	return "actlet-cli"
}
