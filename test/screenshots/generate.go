//go:build ignore

// generate draws the README's screenshots of the terminal viewer as SVG
// images into a folder, from the demo's recording in test/compat/v0. It
// renders the viewer's screens with the same code as -dump -color, on a
// fixed clock, so the images come out the same every time:
//
//	go run test/screenshots/generate.go docs/images
//
// make screenshots runs it.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/actlet/actlet/internal/ansisvg"
	"github.com/actlet/actlet/internal/tui"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const recording = "test/compat/v0/recording.actrec"

// at is how many of the recording's events the screens show: the
// subagents at work, with a note from the operator.
const at = 60

func main() {
	if len(os.Args) != 2 {
		fail(fmt.Errorf("usage: go run test/screenshots/generate.go <folder>"))
	}
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(err)
	}
	p, err := tui.OpenPlayer(recording)
	if err != nil {
		fail(err)
	}
	defer p.Close()
	evs := p.Events()[:at]
	first, last := evs[0].GetTime().AsTime(), evs[len(evs)-1].GetTime().AsTime()

	// live is the viewer of a running box, after the events.
	live := func(conn tui.Connection, viewers ...*attachv0.Viewer) *tui.Model {
		m := tui.NewLive(conn, nil, "")
		m.SetClock(func() time.Time { return last.Add(2 * time.Second) })
		var msgs []*attachv0.WatchResponse
		for _, ev := range evs {
			msgs = append(msgs, &attachv0.WatchResponse{Msg: &attachv0.WatchResponse_Event{Event: ev}})
		}
		m.Update(tui.InfoMsg{Info: &attachv0.InfoResponse{
			SessionId: m.Session().ID, Epoch: 1, State: gatewayv0.SessionState_SESSION_STATE_RUNNING,
			Stats: &attachv0.Stats{Started: timestamppb.New(first), FolderBytes: 61 << 10, OutputBytes: 1 << 10,
				MemoryBytes: 21 << 20, Cpu: durationpb.New(900 * time.Millisecond), Gateway: "gateway:7400", GatewayConnected: true},
			Viewers: viewers,
		}})
		m.Apply(msgs)
		return m
	}
	since := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(last.Add(-d)) }

	shots := []struct {
		name, title string
		w, h        int
		model       func() *tui.Model
	}{
		{"chat", "actlet run -tui", 120, 36, func() *tui.Model {
			return live(tui.Connection{Kind: "this process"},
				&attachv0.Viewer{Kind: "local", Label: "actlet run", Since: since(40 * time.Second), You: true})
		}},
		{"attached", "actlet attach", 120, 36, func() *tui.Model {
			m := live(tui.Connection{Kind: "local box", Attached: "local socket"},
				&attachv0.Viewer{Kind: "local", Label: "actlet run", Since: since(40 * time.Second)},
				&attachv0.Viewer{Kind: "local", Label: "actlet attach", Since: since(12 * time.Second), You: true},
				&attachv0.Viewer{Kind: "direct", Label: "on-call", Since: since(5 * time.Second)})
			m.SetFilter("root.2", "")
			return m
		}},
		{"keys", "actlet attach · ?", 120, 60, func() *tui.Model {
			m := live(tui.Connection{Kind: "local box", Attached: "local socket"})
			m.ShowHelp(true)
			return m
		}},
		{"profile", "actlet attach · Profile", 120, 36, func() *tui.Model {
			m := live(tui.Connection{Kind: "local box", Attached: "local socket"})
			m.SetTab(tui.TabProfile)
			for range 4 { // from the top
				m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
			}
			return m
		}},
		{"light", "actlet run -tui · light theme", 120, 36, func() *tui.Model {
			m := live(tui.Connection{Kind: "this process"},
				&attachv0.Viewer{Kind: "local", Label: "actlet run", Since: since(40 * time.Second), You: true})
			m.SetTheme(tui.ThemeLight)
			return m
		}},
	}
	for _, s := range shots {
		m := s.model()
		m.SetSize(s.w, s.h)
		th := m.Theme()
		svg := ansisvg.Render(m.DumpColor(), ansisvg.Options{
			FontSize: 14, Padding: 12, Title: s.title,
			Foreground: th.Colors["text"], Background: th.Colors["background"],
		})
		path := filepath.Join(dir, "tui-"+s.name+".svg")
		if err := os.WriteFile(path, svg, 0o644); err != nil {
			fail(err)
		}
		fmt.Println(path)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "generate:", err)
	os.Exit(1)
}
