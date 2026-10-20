package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/view"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// maxIdle is the longest pause a replay shows between events; longer
// ones are cut short.
const maxIdle = 2 * time.Second

// Player plays an activity recording back: it reads the events, which
// may still be being written, and folds them into a view up to a
// position. It needs no network, tools or models.
type Player struct {
	path      string
	f         *os.File
	r         *journal.Reader
	sessionID string
	events    []*gatewayv0.ActivityEvent
	// end is why reading stopped: io.EOF or a torn tail (the recording
	// may grow), or damage (it can't).
	end error

	pos  int
	sess *view.Session

	playing bool
	follow  bool
	speed   float64
	clock   time.Time // wall time of the last Advance
	cursor  time.Time // recording time reached
}

// OpenPlayer opens a recording and reads what it holds so far.
func OpenPlayer(path string) (*Player, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p := &Player{path: path, f: f, r: r, sessionID: r.Header().SessionID, speed: 1}
	p.read()
	if p.end != nil && !errors.Is(p.end, io.EOF) && !errors.Is(p.end, journal.ErrTornTail) && len(p.events) == 0 {
		p.Close()
		return nil, fmt.Errorf("%s: %w", path, p.end)
	}
	p.sess = p.newView()
	return p, nil
}

func (p *Player) newView() *view.Session {
	s := view.New()
	s.ID = p.sessionID
	return s
}

// Close closes the recording.
func (p *Player) Close() {
	p.r.Close()
	_ = p.f.Close()
}

// read reads the batches available.
func (p *Player) read() {
	for {
		b, err := p.r.Next()
		if err != nil {
			p.end = err
			return
		}
		for _, rec := range b.Records {
			ev := &gatewayv0.ActivityEvent{}
			if err := proto.Unmarshal(rec.Data, ev); err != nil {
				p.end = fmt.Errorf("record %d: %w", rec.Seq, err)
				return
			}
			p.events = append(p.events, ev)
		}
	}
}

// Poll reads what was added to the recording since, and reports whether
// there was anything.
func (p *Player) Poll() bool {
	if p.end != nil && !errors.Is(p.end, io.EOF) && !errors.Is(p.end, journal.ErrTornTail) {
		return false // damaged: nothing more can be read
	}
	st, err := p.f.Stat()
	if err != nil {
		return false
	}
	n := len(p.events)
	p.r.Grow(st.Size())
	p.read()
	return len(p.events) > n
}

// Err returns why the recording couldn't be read to its end, if it is
// damaged.
func (p *Player) Err() error {
	if p.end == nil || errors.Is(p.end, io.EOF) || errors.Is(p.end, journal.ErrTornTail) {
		return nil
	}
	return p.end
}

// Path returns the recording's path.
func (p *Player) Path() string { return p.path }

// Len returns the events read; Pos the events shown.
func (p *Player) Len() int { return len(p.events) }

// Pos returns the number of events shown.
func (p *Player) Pos() int { return p.pos }

// Events returns the events read.
func (p *Player) Events() []*gatewayv0.ActivityEvent { return p.events }

// View returns the view at the position.
func (p *Player) View() *view.Session { return p.sess }

// Seek shows the first n events, and plays on from the last of them.
func (p *Player) Seek(n int) {
	p.show(n)
	if p.pos > 0 {
		p.cursor = p.events[p.pos-1].GetTime().AsTime()
	}
}

// show folds the view to the first n events.
func (p *Player) show(n int) {
	n = max(0, min(n, len(p.events)))
	if n < p.pos {
		p.sess, p.pos = p.newView(), 0
	}
	for ; p.pos < n; p.pos++ {
		p.sess.Apply(p.events[p.pos])
	}
}

// Play starts playing at the given speed.
func (p *Player) Play(now time.Time, speed float64) {
	p.playing, p.clock = true, now
	if speed > 0 {
		p.speed = speed
	}
	if p.pos < len(p.events) && p.cursor.IsZero() {
		p.cursor = p.events[p.pos].GetTime().AsTime()
	}
}

// Toggle pauses or resumes playing.
func (p *Player) Toggle(now time.Time) {
	if p.playing {
		p.playing = false
		return
	}
	p.Play(now, 0)
}

// Faster and Slower change the speed.
func (p *Player) Faster() { p.speed = min(p.speed*2, 64) }

// Slower halves the speed.
func (p *Player) Slower() { p.speed = max(p.speed/2, 0.25) }

// Speed returns the speed.
func (p *Player) Speed() float64 { return p.speed }

// SetFollow makes the player show new events as the recording grows.
func (p *Player) SetFollow(on bool) { p.follow = on }

// Follow reports whether the player follows the recording.
func (p *Player) Follow() bool { return p.follow }

// State describes the player: playing, paused, following or at the end.
func (p *Player) State() string {
	switch {
	case p.follow:
		return "following"
	case p.playing && p.pos < len(p.events):
		return "playing"
	case p.pos >= len(p.events):
		return "at the end"
	}
	return "paused"
}

// Advance moves playback on to now, and reports whether the view
// changed.
func (p *Player) Advance(now time.Time) bool {
	before := p.pos
	if p.follow {
		p.Poll()
		p.Seek(len(p.events))
		return p.pos != before
	}
	if !p.playing {
		return false
	}
	elapsed := time.Duration(float64(now.Sub(p.clock)) * p.speed)
	p.clock = now
	p.cursor = p.cursor.Add(elapsed)
	if p.pos < len(p.events) {
		// Skip long idle stretches.
		if next := p.events[p.pos].GetTime().AsTime(); next.Sub(p.cursor) > maxIdle {
			p.cursor = next.Add(-maxIdle / 4)
		}
	}
	n := p.pos
	for n < len(p.events) && !p.events[n].GetTime().AsTime().After(p.cursor) {
		n++
	}
	// The cursor keeps its place between events, so playing crosses gaps
	// longer than a tick.
	p.show(n)
	return p.pos != before
}
