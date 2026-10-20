package sdk

import (
	"time"

	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Checkpoint is a checkpoint in a session folder's journal, to fork from.
type Checkpoint struct {
	// Seq is the checkpoint's journal record sequence number.
	Seq uint64
	// Turn is the session's agent's model turns at the checkpoint, and
	// Workers its workers, subagents included.
	Turn    uint64
	Workers int
	Time    time.Time
	// Packed says it was too large for one journal record.
	Packed bool
}

// SessionCheckpoints lists the checkpoints of the session in folder dir,
// oldest first.
func SessionCheckpoints(dir string) ([]Checkpoint, error) {
	cps, err := session.Checkpoints(sessionfs.Folder{Dir: dir})
	if err != nil {
		return nil, err
	}
	out := make([]Checkpoint, len(cps))
	for i, c := range cps {
		out[i] = Checkpoint{Seq: c.Seq, Turn: c.Turn, Workers: c.Workers, Time: c.Time, Packed: c.Packed}
	}
	return out, nil
}

// ForkOptions choose the checkpoint a fork starts from, and how.
type ForkOptions struct {
	// Seq picks the checkpoint with this journal sequence number.
	Seq uint64
	// Turn, without Seq, picks the latest checkpoint at this model turn
	// of the session's agent. With neither, the latest checkpoint.
	Turn uint64
	// SessionID is the fork's session ID; empty makes a new one.
	SessionID string
	// CopyWorkspace copies the parent's inputs/ and workspace/ into the
	// fork, as they are now: Actlet keeps no snapshots of files, so they
	// may hold changes made after the checkpoint.
	CopyWorkspace bool
}

// Fork is a session forked from another's checkpoint.
type Fork struct {
	SessionID string
	Lineage   *gatewayv0.Lineage
}

// ForkSession forks the session in folder parent from one of its
// checkpoints into folder child, which must be empty or not exist: a new
// session, with its own ID, whose first run continues from the
// checkpoint's conversation. Its worker reports the lineage when it
// registers (Session.Lineage) and in its session information. The parent
// is only read. Calls in flight at the checkpoint are recovered as after
// a crash; nothing after the checkpoint is copied.
func ForkSession(parent, child string, opts ForkOptions) (*Fork, error) {
	lineage, id, err := session.Fork(sessionfs.Folder{Dir: parent}, sessionfs.Folder{Dir: child}, session.ForkOptions{
		Seq: opts.Seq, Turn: opts.Turn, SessionID: opts.SessionID, CopyWorkspace: opts.CopyWorkspace,
	})
	if err != nil {
		return nil, err
	}
	return &Fork{SessionID: id, Lineage: lineage}, nil
}
