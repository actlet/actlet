package tools

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func writesCall(paths ...string) *Call { return &Call{writes: paths} }

// admitted reports whether Acquire returns at once.
func admitted(t *testing.T, l *Locks, c *Call) (func(), bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	release, err := l.Acquire(ctx, c)
	return release, err == nil
}

func TestLocks(t *testing.T) {
	root := t.TempDir()
	p := func(rel string) string { return filepath.Join(root, rel) }
	l := NewLocks()

	a, ok := admitted(t, l, writesCall(p("a.txt")))
	if !ok {
		t.Fatal("first write waited")
	}
	if _, ok := admitted(t, l, writesCall(p("a.txt"))); ok {
		t.Error("a second write to the same file didn't wait")
	}
	b, ok := admitted(t, l, writesCall(p("b.txt")))
	if !ok {
		t.Error("a write to another file waited")
	}
	if _, ok := admitted(t, l, &Call{}); !ok {
		t.Error("a read-only call waited")
	}
	if _, ok := admitted(t, l, &Call{writesAll: true}); ok {
		t.Error("a call writing everything didn't wait")
	}
	d, ok := admitted(t, l, writesCall(p("dir/x")))
	if !ok {
		t.Fatal("dir/x waited")
	}
	if _, ok := admitted(t, l, writesCall(p("dir"))); ok {
		t.Error("writing a directory didn't wait for a file in it")
	}
	a()
	b()
	d()

	// Waiters go in order: a later call that conflicts with an earlier
	// waiter waits behind it, even if it could run now.
	hold, _ := admitted(t, l, writesCall(p("x")))
	order := make(chan string, 3)
	go func() {
		r, _ := l.Acquire(t.Context(), writesCall(p("x"), p("y")))
		order <- "first"
		time.Sleep(20 * time.Millisecond)
		r()
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		r, _ := l.Acquire(t.Context(), writesCall(p("y")))
		order <- "second"
		r()
	}()
	time.Sleep(20 * time.Millisecond)
	if _, ok := admitted(t, l, writesCall(p("z"))); !ok {
		t.Error("an unrelated write waited behind others")
	}
	hold()
	if got := []string{<-order, <-order}; got[0] != "first" || got[1] != "second" {
		t.Errorf("order %v", got)
	}

	// A cancelled waiter leaves the queue.
	hold, _ = admitted(t, l, writesCall(p("q")))
	if _, ok := admitted(t, l, writesCall(p("q"))); ok {
		t.Fatal("didn't wait")
	}
	hold()
	if r, ok := admitted(t, l, writesCall(p("q"))); !ok {
		t.Error("a cancelled waiter still holds its place")
	} else {
		r()
	}
}

func TestShellWriteSets(t *testing.T) {
	s, _ := session(t, Options{})
	if !s.Has(Shell) {
		t.Skip("no shell")
	}
	for _, tt := range []struct {
		input string
		all   bool
		n     int
	}{
		{`{"command":"make"}`, true, 0},
		{`{"command":"make","writes":["build"]}`, false, 1},
		{`{"command":"ls","writes":[]}`, false, 0},
	} {
		c, err := s.Prepare(Shell, []byte(tt.input))
		if err != nil {
			t.Fatal(err)
		}
		all, paths := c.Writes()
		if all != tt.all || len(paths) != tt.n {
			t.Errorf("%s: writes all %v, %v", tt.input, all, paths)
		}
	}
	c, err := s.Prepare(WriteFile, []byte(`{"path":"f","content":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if all, paths := c.Writes(); all || len(paths) != 1 || filepath.Base(paths[0]) != "f" {
		t.Errorf("write_file writes %v %v", all, paths)
	}
	if c, _ := s.Prepare(ReadFile, []byte(`{"path":"f"}`)); c != nil {
		if all, paths := c.Writes(); all || len(paths) != 0 {
			t.Error("read_file writes")
		}
	}
}
