package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// kindFork is the first record of a forked session.
const kindFork = "session.fork"

// Checkpoint describes a checkpoint in a session's journal.
type Checkpoint struct {
	// Seq is the checkpoint record's sequence number.
	Seq uint64
	// Turn is the root worker's model turns, and Workers the workers.
	Turn    uint64
	Workers int
	Time    time.Time
	// Packed says that the checkpoint was too large for one record.
	Packed bool

	records []journal.Record // its parts, if packed, then itself
}

// ForkOptions choose the checkpoint to fork from, and how.
type ForkOptions struct {
	// Seq picks the checkpoint record with this sequence number.
	Seq uint64
	// Turn, if Seq is 0, picks the latest checkpoint at this root worker
	// turn. With neither, the latest checkpoint is forked.
	Turn uint64
	// SessionID is the fork's session ID; empty makes a new one.
	SessionID string
	// CopyWorkspace copies the parent's inputs/ and workspace/ as they are
	// now, not as they were at the checkpoint.
	CopyWorkspace bool
}

// Checkpoints lists the checkpoints in a session's journal, oldest first.
// A journal cut short by a crash is read up to the damage.
func Checkpoints(f sessionfs.Folder) ([]Checkpoint, error) {
	var out []Checkpoint
	_, _, err := scanCheckpoints(f, func(c Checkpoint) bool {
		c.records = nil
		out = append(out, c)
		return false
	})
	return out, err
}

// scanCheckpoints calls keep with each checkpoint of the journal, with its
// records, and returns the last one keep wanted, or the latest if keep is
// nil.
func scanCheckpoints(f sessionfs.Folder, keep func(Checkpoint) bool) (*Checkpoint, string, error) {
	file, err := os.Open(f.Journal())
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	r, err := journal.NewReader(file, st.Size(), journal.ReaderOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("session journal: %w", err)
	}
	defer r.Close()
	var kept *Checkpoint
	var parts []journal.Record
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return kept, r.Header().SessionID, nil // a crash's torn tail ends the journal
		}
		if err != nil {
			return nil, "", fmt.Errorf("session journal: %w", err)
		}
		for _, jr := range b.Records {
			if !agent.IsRecord(jr.Kind) {
				continue
			}
			jr.Data = append([]byte(nil), jr.Data...)
			switch jr.Kind {
			case agent.KindCheckpointPart:
				parts = append(parts, jr)
				continue
			case agent.KindCheckpoint:
				cp, err := agent.ReadCheckpoint(parts, jr)
				if err != nil {
					return nil, "", fmt.Errorf("checkpoint %d: %w", jr.Seq, err)
				}
				c := Checkpoint{Seq: jr.Seq, Turn: agent.RootTurn(cp), Workers: len(cp.GetWorkers()), Time: jr.Time,
					Packed: len(parts) > 0, records: append(append([]journal.Record(nil), parts...), jr)}
				if keep == nil || keep(c) {
					kept = &c
				}
			}
			parts = nil
		}
	}
}

// Fork starts a new session in the folder child from a checkpoint of the
// session in the folder parent: a journal that holds the fork's lineage
// and the checkpoint, so that the child's first run carries on from the
// checkpoint's conversation. The parent is only read. Calls in flight at
// the checkpoint are recovered as after a crash; nothing after it is
// copied. The child gets the parent's task.json and, with
// CopyWorkspace, its inputs and workspace as they are now.
func Fork(parent, child sessionfs.Folder, opts ForkOptions) (*gatewayv0.Lineage, string, error) {
	var keep func(Checkpoint) bool
	switch {
	case opts.Seq > 0:
		keep = func(c Checkpoint) bool { return c.Seq == opts.Seq }
	case opts.Turn > 0:
		keep = func(c Checkpoint) bool { return c.Turn == opts.Turn }
	}
	cp, parentID, err := scanCheckpoints(parent, keep)
	if err != nil {
		return nil, "", err
	}
	if cp == nil {
		switch {
		case opts.Seq > 0:
			return nil, "", fmt.Errorf("no checkpoint at journal record %d; actlet-cli fork --list shows them", opts.Seq)
		case opts.Turn > 0:
			return nil, "", fmt.Errorf("no checkpoint at turn %d; actlet-cli fork --list shows them", opts.Turn)
		}
		return nil, "", errors.New("the session has no checkpoint")
	}
	if entries, err := os.ReadDir(child.Dir); err == nil && len(entries) > 0 {
		return nil, "", fmt.Errorf("%s isn't empty", child.Dir)
	}
	id := opts.SessionID
	if id == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		id = hex.EncodeToString(b[:])
	}
	for _, d := range []string{child.Inputs(), child.Workspace(), child.Output(), child.State()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, "", err
		}
	}
	if err := copyFile(parent.TaskFile(), child.TaskFile()); err != nil {
		return nil, "", fmt.Errorf("task.json: %w", err)
	}
	if opts.CopyWorkspace {
		for _, d := range [][2]string{{parent.Inputs(), child.Inputs()}, {parent.Workspace(), child.Workspace()}} {
			if err := copyTree(d[0], d[1]); err != nil {
				return nil, "", err
			}
		}
	}

	lineage := &gatewayv0.Lineage{ParentSessionId: parentID, CheckpointSeq: cp.Seq, Turn: cp.Turn, ForkedAt: timestamppb.Now()}
	j, err := journal.Create(child.Journal(), journal.DefaultOptions(id))
	if err != nil {
		return nil, "", err
	}
	data, err := proto.Marshal(&sessionpb.Fork{Lineage: lineage})
	if err != nil {
		return nil, "", err
	}
	if err := appendRecord(j, kindFork, data); err != nil {
		_ = j.Close()
		return nil, "", err
	}
	for _, r := range cp.records {
		if err := appendRecord(j, r.Kind, r.Data); err != nil {
			_ = j.Close()
			return nil, "", err
		}
	}
	if err := j.Close(); err != nil {
		return nil, "", err
	}
	return lineage, id, nil
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

// copyTree copies a directory's files, directories and symbolic links.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == from {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil
	})
}

func appendRecord(j *journal.Writer, kind string, data []byte) error {
	_, err := j.Append(context.Background(), kind, data)
	return err
}
