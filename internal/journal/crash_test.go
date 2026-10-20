//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const crashHelperEnv = "ACTLET_JOURNAL_CRASH_HELPER"

// TestCrashHelper is the writer process for TestKillDuringWrites. It only
// runs when the environment variable names a journal path.
func TestCrashHelper(t *testing.T) {
	path := os.Getenv(crashHelperEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	opts := DefaultOptions("crash-test")
	opts.Batch.MaxRecords = 32
	w, err := Open(path, opts)
	if errors.Is(err, os.ErrNotExist) {
		w, err = Create(path, opts)
	}
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println("ready", w.LastSeq())

	// Several appenders, so batches of different sizes are in flight when
	// the process is killed. Each acknowledged seq is printed.
	var mu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	for g := range 8 {
		go func() {
			for i := 0; ; i++ {
				data := crashData(g, i)
				seq, err := w.Append(context.Background(), "test.crash", data)
				if err != nil {
					fmt.Println("error", err)
					os.Exit(1)
				}
				mu.Lock()
				fmt.Fprintf(out, "ack %d %s\n", seq, data[:16])
				_ = out.Flush()
				mu.Unlock()
			}
		}()
	}
	select {}
}

// crashData is a record payload of varying size identifying its writer.
func crashData(g, i int) []byte {
	b := fmt.Appendf(nil, "g%03d-i%010d|", g, i)
	return append(b, strings.Repeat("payload ", (g*7+i)%300)...)
}

// TestKillDuringWrites repeatedly SIGKILLs a writer process while it appends
// and checks that reopening recovers a contiguous prefix holding every
// acknowledged record.
func TestKillDuringWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	path := tempJournal(t)
	acked := map[uint64]string{} // seq -> data prefix, across all rounds
	var maxAcked uint64

	for round := range 5 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.v=false")
		cmd.Env = append(os.Environ(), crashHelperEnv+"="+path)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		sc := bufio.NewScanner(stdout)
		if !sc.Scan() || !strings.HasPrefix(sc.Text(), "ready ") {
			_ = cmd.Process.Kill()
			t.Fatalf("round %d: helper didn't start: %q", round, sc.Text())
		}

		// While the helper holds the journal, this process can't open it.
		if _, err := Open(path, testOptions()); !errors.Is(err, ErrLocked) {
			_ = cmd.Process.Kill()
			t.Fatalf("round %d: Open while helper runs: err = %v, want ErrLocked", round, err)
		}

		// Let it acknowledge a random number of records, then kill it at a
		// random moment, usually with batches in flight.
		target := 50 + rand.IntN(400)
		n := 0
		for n < target && sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "error") {
				t.Fatalf("round %d: helper: %s", round, line)
			}
			f := strings.Fields(line)
			seq, _ := strconv.ParseUint(f[1], 10, 64)
			acked[seq] = f[2]
			maxAcked = max(maxAcked, seq)
			n++
		}
		time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		// Acks printed before the kill still count.
		for sc.Scan() {
			if f := strings.Fields(sc.Text()); len(f) == 3 && f[0] == "ack" {
				seq, _ := strconv.ParseUint(f[1], 10, 64)
				acked[seq] = f[2]
				maxAcked = max(maxAcked, seq)
			}
		}
		_ = cmd.Wait()

		// The kill released the lock. Every acknowledged record is in the
		// recovered prefix, with the content that was acknowledged.
		w := mustOpen(t, path, DefaultOptions("crash-test"))
		rec := w.Recovery()
		mustClose(t, w)
		if rec.LastSeq < maxAcked {
			t.Fatalf("round %d: recovered up to seq %d, but %d was acknowledged", round, rec.LastSeq, maxAcked)
		}
		recs := readAll(t, path)
		if uint64(len(recs)) != rec.LastSeq {
			t.Fatalf("round %d: read %d records, recovery says %d", round, len(recs), rec.LastSeq)
		}
		for i, r := range recs {
			if r.Seq != uint64(i+1) {
				t.Fatalf("round %d: record %d has seq %d", round, i, r.Seq)
			}
			if want, ok := acked[r.Seq]; ok && string(r.Data[:16]) != want {
				t.Fatalf("round %d: seq %d holds %q, acknowledged %q", round, r.Seq, r.Data[:16], want)
			}
		}
		t.Logf("round %d: %d acked, recovered %d records in %d batches, tail %v",
			round, n, rec.LastSeq, rec.Batches, rec.Tail)
	}
}
