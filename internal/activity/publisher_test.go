package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func newTest(t *testing.T, queue int, spillMax int64) *Publisher {
	t.Helper()
	return New(Options{
		SessionID: "s1", Epoch: 1, Dir: t.TempDir(),
		QueueLen: queue, SpillMax: spillMax,
		Logger: slog.New(slog.DiscardHandler),
	})
}

func text(s string) *Event {
	return &Event{Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: s}}}
}

func state(to gatewayv0.SessionState) *Event {
	return &Event{Body: &gatewayv0.ActivityEvent_State{State: &gatewayv0.StateChange{To: to}}}
}

func next(t *testing.T, p *Publisher, after uint64, max int) []*Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	evs, err := p.Next(ctx, after, max)
	if err != nil {
		t.Fatalf("Next(%d): %v", after, err)
	}
	return evs
}

// checkContiguous verifies that evs are numbered from first without gaps.
func checkContiguous(t *testing.T, evs []*Event, first uint64) {
	t.Helper()
	for i, ev := range evs {
		want := first + uint64(i)
		if ev.Seq != want || ev.Id != fmt.Sprintf("1.%d", want) || ev.Epoch != 1 {
			t.Fatalf("event %d is %s (seq %d), want seq %d", i, ev.Id, ev.Seq, want)
		}
	}
}

func spillExists(p *Publisher) bool {
	_, err := os.Stat(SpillPath(p.opts.Dir, p.opts.Epoch))
	return err == nil
}

func TestEventsInMemory(t *testing.T) {
	p := newTest(t, 16, 0)
	for i := range 5 {
		p.Emit(text(fmt.Sprint(i)))
	}
	evs := next(t, p, 0, 100)
	if len(evs) != 5 {
		t.Fatalf("got %d events, want 5", len(evs))
	}
	checkContiguous(t, evs, 1)
	if got := next(t, p, 3, 100); len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("Next(3) = %d events", len(got))
	}
	p.Ack(3)
	// Acknowledged events are gone even if asked for again.
	if got := next(t, p, 0, 100); len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("after Ack(3), Next(0) returned %d events from %d", len(got), got[0].Seq)
	}
	if spillExists(p) {
		t.Error("spill file created without overflow")
	}
}

func TestNextWaits(t *testing.T) {
	p := newTest(t, 16, 0)
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.Emit(text("late"))
	}()
	if evs := next(t, p, 0, 10); len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Next(ctx, 1, 10); err == nil {
		t.Fatal("Next returned without events")
	}
}

func TestOverflowSpillsAndReplays(t *testing.T) {
	p := newTest(t, 4, 0)
	for i := range 20 {
		p.Emit(text(fmt.Sprint("event ", i)))
	}
	if !spillExists(p) {
		t.Fatal("no spill file after overflowing the queue")
	}
	evs := next(t, p, 0, 100)
	if len(evs) != 20 {
		t.Fatalf("got %d events, want 20", len(evs))
	}
	checkContiguous(t, evs, 1)
	if got := evs[19].GetText().GetText(); got != "event 19" {
		t.Errorf("last event text %q", got)
	}

	// A reconnect resends from the gateway's position, spill included.
	p.Ack(6)
	evs = next(t, p, 6, 5)
	checkContiguous(t, evs, 7)
	evs = next(t, p, 6, 100)
	if len(evs) != 14 {
		t.Fatalf("resend got %d events, want 14", len(evs))
	}

	// Once everything is acknowledged the spill file goes away, and new
	// events stay in memory again.
	p.Ack(20)
	if spillExists(p) {
		t.Error("spill file kept after everything was acknowledged")
	}
	p.Emit(text("after"))
	if spillExists(p) {
		t.Error("spilled with room in the queue")
	}
	evs = next(t, p, 20, 10)
	checkContiguous(t, evs, 21)
}

func TestSpillCapLeavesGap(t *testing.T) {
	p := newTest(t, 2, 2048)
	for i := range 300 {
		p.Emit(text(strings.Repeat("x", 20) + fmt.Sprint(i)))
	}
	// Events that aren't display-only still fit in the reserve.
	p.Emit(state(gatewayv0.SessionState_SESSION_STATE_PAUSING))

	evs := next(t, p, 0, 1000)
	checkContiguous(t, evs, 1)
	var gaps []*gatewayv0.Gap
	var stored, sawState int
	for _, ev := range evs {
		switch b := ev.Body.(type) {
		case *gatewayv0.ActivityEvent_Gap:
			gaps = append(gaps, b.Gap)
		case *gatewayv0.ActivityEvent_Text:
			stored++
		case *gatewayv0.ActivityEvent_State:
			sawState++
		}
	}
	if len(gaps) != 1 || sawState != 1 {
		t.Fatalf("got %d gap events and %d state events, want 1 and 1", len(gaps), sawState)
	}
	if dropped := int(gaps[0].GetDropped()); stored+dropped != 300 || dropped == 0 {
		t.Fatalf("stored %d and dropped %d of 300 text events", stored, dropped)
	}
	if !gaps[0].GetLast().AsTime().After(gaps[0].GetFirst().AsTime()) && gaps[0].GetDropped() > 1 {
		t.Error("gap time range is empty")
	}
}

// A gap is reported as soon as the sender has caught up, without waiting
// for the next event that fits.
func TestGapReportedWhenCaughtUp(t *testing.T) {
	p := newTest(t, 1, 64)
	for range 10 {
		p.Emit(text(strings.Repeat("y", 50)))
	}
	evs := next(t, p, 0, 100)
	last := evs[len(evs)-1]
	evs = next(t, p, last.Seq, 100)
	if len(evs) != 1 || evs[0].GetGap() == nil {
		t.Fatalf("after catching up got %v, want a gap event", evs)
	}
}

func TestCloseKeepsUnsentEventsForTheNextRun(t *testing.T) {
	p := newTest(t, 4, 0)
	for i := range 10 {
		p.Emit(text(fmt.Sprint(i)))
	}
	p.Ack(3)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	p.Emit(text("ignored after close"))

	backlogs, err := OpenBacklogs(p.opts.Dir, 2, slog.New(slog.DiscardHandler))
	if err != nil || len(backlogs) != 1 {
		t.Fatalf("OpenBacklogs = %d, %v", len(backlogs), err)
	}
	b := backlogs[0]
	if b.Epoch() != 1 {
		t.Fatalf("backlog epoch %d", b.Epoch())
	}
	evs := next(t, b, 0, 100)
	if len(evs) != 7 {
		t.Fatalf("backlog holds %d events, want 7", len(evs))
	}
	checkContiguous(t, evs, 4)
	if b.Done() {
		t.Error("backlog done before acknowledgement")
	}
	if _, err := b.Next(t.Context(), 10, 10); !errors.Is(err, ErrDrained) {
		t.Errorf("Next past the end of a backlog: %v, want ErrDrained", err)
	}
	b.Ack(10)
	if !b.Done() || spillExists(b) {
		t.Error("acknowledged backlog not cleaned up")
	}
	// The current epoch's files are not backlogs.
	if bl, _ := OpenBacklogs(p.opts.Dir, 1, nil); len(bl) != 0 {
		t.Errorf("current epoch treated as backlog")
	}
}

func TestCloseWaitsForAcks(t *testing.T) {
	p := newTest(t, 4, 0)
	p.Emit(text("a"))
	go func() {
		time.Sleep(30 * time.Millisecond)
		p.Ack(1)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("Close didn't return once everything was acknowledged")
	}
	if spillExists(p) {
		t.Error("spill file written although everything was acknowledged")
	}
}

func TestEmitDoesNotBlock(t *testing.T) {
	p := newTest(t, 8, 4096)
	start := time.Now()
	for range 20000 {
		p.Emit(text("burst"))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("20000 emits with nobody reading took %v", d)
	}
}
