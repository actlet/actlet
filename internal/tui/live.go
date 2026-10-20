package tui

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Feed connects a live viewer to a box: it streams the activity and
// refreshes the box's figures, sending them to send, until ctx ends or
// the stream does.
func Feed(ctx context.Context, client attachv0.AttachClient, send func(tea.Msg)) error {
	info, err := client.Info(ctx, &attachv0.InfoRequest{})
	if err != nil {
		return err
	}
	send(InfoMsg{Info: info})
	stream, err := client.Watch(ctx, &attachv0.WatchRequest{})
	if err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if info, err := client.Info(ctx, &attachv0.InfoRequest{}); err == nil {
					send(InfoMsg{Info: info})
				}
			}
		}
	}()
	// Batch what arrives together, so a backlog renders once.
	msgs := make(chan *attachv0.WatchResponse, 256)
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				errc <- err
				close(msgs)
				return
			}
			msgs <- m
		}
	}()
	for m := range msgs {
		batch := EventsMsg{m}
	more:
		for len(batch) < 512 {
			select {
			case m, ok := <-msgs:
				if !ok {
					break more
				}
				batch = append(batch, m)
			default:
				break more
			}
		}
		send(batch)
	}
	err = <-errc
	if errors.Is(err, io.EOF) {
		send(LostMsg{})
		return nil
	}
	if ctx.Err() == nil {
		send(LostMsg{Err: err})
	}
	return err
}

// Commands returns a Commander for the box.
func Commands(ctx context.Context, client attachv0.AttachClient) Commander {
	return func(req *attachv0.CommandRequest) (*gatewayv0.CommandAck, error) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return client.Command(cctx, req)
	}
}

// programs counts the viewers started in this process.
var programs atomic.Int32

// Programs returns how many viewers this process has started, to check
// that a headless run starts none.
func Programs() int { return int(programs.Load()) }

// CheckTerminal reports whether there is a terminal to show a viewer on.
func CheckTerminal() error {
	_, _, closeTerm, err := openTerminal()
	if err != nil {
		return err
	}
	closeTerm()
	return nil
}

// RunLive shows a running box until the viewer quits. Quitting detaches:
// the session carries on.
func RunLive(ctx context.Context, client attachv0.AttachClient, conn Connection, actor string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	in, out, closeTerm, err := openTerminal()
	if err != nil {
		return err
	}
	defer closeTerm()
	m := NewLive(conn, Commands(ctx, client), actor)
	m.SetFileReader(Files(ctx, client))
	programs.Add(1)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	go func() { _ = Feed(ctx, client, p.Send) }()
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// RunReplay plays a recording back at the speed until the viewer quits.
func RunReplay(ctx context.Context, m *Model, speed float64) error {
	in, out, closeTerm, err := openTerminal()
	if err != nil {
		return err
	}
	defer closeTerm()
	m.player.Play(time.Now(), speed)
	programs.Add(1)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
