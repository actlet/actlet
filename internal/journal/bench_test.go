package journal

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// agentCorpus generates deterministic, agent-like records: model prose, tool
// calls, command output, source code and occasional larger context records.
type agentCorpus struct {
	rng *rand.Rand
}

func newAgentCorpus(seed uint64) *agentCorpus {
	return &agentCorpus{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

var (
	corpusWords = strings.Fields(`the a to of and in is for that this with it be on as we
		file function test error value return should will can not need first then update
		change config session worker tool result output check build run package import
		module type struct method field interface handler request response context
		journal batch record sync write read offset length limit recovery crash`)
	corpusPaths = []string{"internal/journal/writer.go", "cmd/actlet/main.go", "go.mod",
		"internal/session/worker.go", "pkg/api/handler.go", "README.md", "Makefile"}
	corpusIdents = []string{"ctx", "err", "w", "r", "batch", "rec", "opts", "seq", "buf", "n"}
)

func (c *agentCorpus) word() string { return corpusWords[c.rng.IntN(len(corpusWords))] }

func (c *agentCorpus) prose(words int) string {
	var b strings.Builder
	for i := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(c.word())
		if c.rng.IntN(12) == 0 {
			b.WriteString(".")
		}
	}
	return b.String()
}

func (c *agentCorpus) code(lines int) string {
	var b strings.Builder
	for range lines {
		id := corpusIdents[c.rng.IntN(len(corpusIdents))]
		switch c.rng.IntN(5) {
		case 0:
			fmt.Fprintf(&b, "\tif %s != nil {\n\t\treturn fmt.Errorf(\"%s %s: %%w\", %s)\n\t}\n", id, c.word(), c.word(), id)
		case 1:
			fmt.Fprintf(&b, "\t%s := %s.%s%s(%d)\n", id, corpusIdents[c.rng.IntN(len(corpusIdents))], strings.ToUpper(c.word()[:1]), c.word(), c.rng.IntN(1000))
		case 2:
			fmt.Fprintf(&b, "\t// %s\n", c.prose(8))
		default:
			fmt.Fprintf(&b, "\tfor i := 0; i < len(%s); i++ {\n\t\t%s[i] = %d\n\t}\n", id, id, c.rng.IntN(100))
		}
	}
	return b.String()
}

func (c *agentCorpus) output(lines int) string {
	var b strings.Builder
	for range lines {
		switch c.rng.IntN(4) {
		case 0:
			fmt.Fprintf(&b, "ok  \tgithub.com/example/project/%s\t%d.%03ds\n", c.word(), c.rng.IntN(5), c.rng.IntN(1000))
		case 1:
			fmt.Fprintf(&b, "%s:%d:%d: %s\n", corpusPaths[c.rng.IntN(len(corpusPaths))], c.rng.IntN(500), c.rng.IntN(80), c.prose(6))
		case 2:
			fmt.Fprintf(&b, "-rw-r--r--  1 user  staff  %6d Oct 20 %02d:%02d %s\n", c.rng.IntN(100000), c.rng.IntN(24), c.rng.IntN(60), corpusPaths[c.rng.IntN(len(corpusPaths))])
		default:
			fmt.Fprintf(&b, "=== RUN   Test%s%s\n--- PASS: Test%s (0.%02ds)\n", strings.ToUpper(c.word()[:1]), c.word(), c.word(), c.rng.IntN(100))
		}
	}
	return b.String()
}

// next returns the kind and data of the next record.
func (c *agentCorpus) next() (string, []byte) {
	id := fmt.Sprintf("%016x", c.rng.Uint64())
	switch n := c.rng.IntN(100); {
	case n < 30:
		return "model.message", fmt.Appendf(nil, `{"id":"%s","role":"assistant","text":%q}`, id, c.prose(20+c.rng.IntN(200)))
	case n < 55:
		return "tool.intent", fmt.Appendf(nil, `{"id":"%s","tool":"shell","args":{"cmd":"go test ./%s/...","cwd":"/workspace"},"worker":"w%d"}`, id, c.word(), c.rng.IntN(4))
	case n < 80:
		return "tool.outcome", fmt.Appendf(nil, `{"id":"%s","exit":%d,"stdout":%q}`, id, c.rng.IntN(2), c.output(2+c.rng.IntN(30)))
	case n < 95:
		return "file.read", fmt.Appendf(nil, `{"id":"%s","path":%q,"content":%q}`, id, corpusPaths[c.rng.IntN(len(corpusPaths))], c.code(5+c.rng.IntN(60)))
	default:
		return "checkpoint", fmt.Appendf(nil, `{"id":"%s","context":%q}`, id, c.prose(1500+c.rng.IntN(1500)))
	}
}

type corpusRecord struct {
	kind string
	data []byte
}

func corpusRecords(n int) ([]corpusRecord, int) {
	c := newAgentCorpus(42)
	recs := make([]corpusRecord, n)
	total := 0
	for i := range recs {
		k, d := c.next()
		recs[i] = corpusRecord{k, d}
		total += len(d)
	}
	return recs, total / n
}

// benchAppend appends b.N records from batchSize concurrent appenders, so
// that each batch fills up to batchSize records, and reports per-batch sync
// cost.
func benchAppend(b *testing.B, batchSize int, sync_ bool) {
	recs, avg := corpusRecords(4096)
	opts := DefaultOptions("bench")
	opts.NoSync = !sync_
	opts.Batch = BatchPolicy{MaxRecords: batchSize}
	if batchSize > 1 {
		// Long enough that each batch fills up with one record from every
		// appender.
		opts.Batch.MaxDelay = 50 * time.Millisecond
	}
	w := mustCreate(b, tempJournal(b), opts)
	defer mustClose(b, w)

	b.SetBytes(int64(avg))
	b.ResetTimer()
	var wg sync.WaitGroup
	per := (b.N + batchSize - 1) / batchSize
	for g := range batchSize {
		wg.Go(func() {
			for i := range per {
				r := recs[(g*per+i)%len(recs)]
				if _, err := w.Append(context.Background(), r.kind, r.data); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()

	s := w.Stats()
	b.ReportMetric(float64(s.Records)/b.Elapsed().Seconds(), "records/s")
	b.ReportMetric(float64(s.Records)/float64(s.Batches), "records/batch")
	if sync_ && s.Batches > 0 {
		b.ReportMetric(float64(s.SyncTime.Microseconds())/float64(s.Batches), "µs-sync/batch")
	}
	b.ReportMetric(float64(s.RawBytes)/float64(s.StoredBytes), "ratio")
}

func BenchmarkAppendDurable(b *testing.B) {
	for _, n := range []int{1, 8, 64, 256} {
		b.Run(fmt.Sprintf("batch=%d", n), func(b *testing.B) { benchAppend(b, n, true) })
	}
}

// BenchmarkAppendNoSync isolates the CPU cost of encoding, compressing and
// writing to the page cache.
func BenchmarkAppendNoSync(b *testing.B) {
	for _, n := range []int{1, 64} {
		b.Run(fmt.Sprintf("batch=%d", n), func(b *testing.B) { benchAppend(b, n, false) })
	}
}

// BenchmarkSync measures a bare fsync after a small append on the benchmark
// filesystem, for comparison with the journal's per-batch cost.
func BenchmarkSync(b *testing.B) {
	f, err := os.Create(tempJournal(b))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 512)
	b.ResetTimer()
	for b.Loop() {
		if _, err := f.Write(buf); err != nil {
			b.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecover scans a large journal, as Open does on startup.
func BenchmarkRecover(b *testing.B) {
	const records = 60000
	recs, _ := corpusRecords(records)
	path := tempJournal(b)
	opts := DefaultOptions("bench")
	opts.NoSync = true
	opts.Batch = BatchPolicy{MaxRecords: 64, MaxDelay: time.Second}
	w := mustCreate(b, path, opts)
	var wg sync.WaitGroup
	for g := range 64 {
		wg.Go(func() {
			for i := g; i < records; i += 64 {
				if _, err := w.Append(context.Background(), recs[i].kind, recs[i].data); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	s := w.Stats()
	mustClose(b, w)
	st, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("journal: %d records, %d batches, %.1f MiB on disk, %.1f MiB raw",
		s.Records, s.Batches, float64(st.Size())/(1<<20), float64(s.RawBytes)/(1<<20))

	for _, skip := range []bool{false, true} {
		name := "decode"
		if skip {
			name = "checksum-only"
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(st.Size())
			for b.Loop() {
				f, err := os.Open(path)
				if err != nil {
					b.Fatal(err)
				}
				res, err := Scan(f, st.Size(), ReaderOptions{SkipRecords: skip})
				_ = f.Close()
				if err != nil || res.LastSeq != records {
					b.Fatalf("Scan: %v, LastSeq %d", err, res.LastSeq)
				}
			}
		})
	}
}

// BenchmarkCompressionRatio reports the stored/raw ratio of agent-like
// batches at several batch sizes and zstd levels, framing included.
func BenchmarkCompressionRatio(b *testing.B) {
	recs, _ := corpusRecords(8192)
	levels := []zstd.EncoderLevel{zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetterCompression}
	for _, size := range []int{1, 16, 64, 256} {
		for _, level := range levels {
			b.Run(fmt.Sprintf("batch=%d/level=%s", size, level), func(b *testing.B) {
				enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = enc.Close() }()
				var raw, stored int
				batch := make([]Record, 0, size)
				var buf, out []byte
				for b.Loop() {
					raw, stored = 0, 0
					for i := 0; i < len(recs); i += size {
						batch = batch[:0]
						for j := i; j < i+size && j < len(recs); j++ {
							batch = append(batch, Record{Seq: uint64(j + 1), Kind: recs[j].kind, Time: time.Unix(1_800_000_000, int64(j)), Data: recs[j].data})
						}
						buf, _ = marshalBatch(buf, batch)
						out = enc.EncodeAll(buf, out[:0])
						raw += len(buf)
						stored += min(len(out), len(buf)) + batchOverhead
					}
				}
				b.SetBytes(int64(raw))
				b.ReportMetric(float64(raw)/float64(stored), "ratio")
			})
		}
	}
}
