package agent

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/journal"
)

// A checkpoint too large for one journal record is packed: compressed,
// written in parts, and then named by a small checkpoint record. A crash
// before that record leaves parts that nothing names, and recovery starts
// from the checkpoint before.

// Limits of packed checkpoints.
var (
	// maxCheckpointRecord is the largest checkpoint written as is, and
	// the largest part.
	maxCheckpointRecord = journal.DefaultLimits().MaxRecordData - 1024
	// maxCheckpoint bounds a checkpoint's encoding. A larger one isn't
	// written; recovery replays from the one before.
	maxCheckpoint = 256 << 20
)

// errCheckpointTooLarge reports a checkpoint over maxCheckpoint.
var errCheckpointTooLarge = errors.New("the workers' state is too large to checkpoint")

// pack returns the records that store a checkpoint: the checkpoint itself
// when it fits in one, else its parts and then a packed checkpoint that
// names them.
func pack(cp *agentpb.Checkpoint) ([]proto.Message, error) {
	size := proto.Size(cp)
	if size <= maxCheckpointRecord {
		return []proto.Message{cp}, nil
	}
	if size > maxCheckpoint {
		return nil, fmt.Errorf("%w: %d bytes", errCheckpointTooLarge, size)
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(cp)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	packed := buf.Bytes()
	sum := sha256.Sum256(packed)
	var out []proto.Message
	for i := 0; len(packed) > 0; i++ {
		n := min(len(packed), maxCheckpointRecord)
		out = append(out, &agentpb.CheckpointPart{Sha256: sum[:], Index: uint32(i), Data: packed[:n]})
		packed = packed[n:]
	}
	head := &agentpb.Checkpoint{Packed: &agentpb.PackedCheckpoint{Parts: uint32(len(out)), Sha256: sum[:], Size: uint64(len(data))}}
	return append(out, head), nil
}

// parts collects the parts of a packed checkpoint during recovery: those
// of the latest one begun.
type parts struct {
	sum  []byte
	data [][]byte
	size int
}

// add adds a part. Parts that don't follow on are dropped: only a packed
// checkpoint whose parts are missing fails recovery, in unpack.
func (p *parts) add(m *agentpb.CheckpointPart) {
	if !bytes.Equal(m.GetSha256(), p.sum) || m.GetIndex() == 0 {
		*p = parts{sum: m.GetSha256()}
	}
	if int(m.GetIndex()) != len(p.data) || p.size+len(m.GetData()) > maxCheckpoint {
		*p = parts{}
		return
	}
	p.size += len(m.GetData())
	p.data = append(p.data, m.GetData())
}

// unpack returns the checkpoint a packed one names, from its parts.
func (p *parts) unpack(head *agentpb.PackedCheckpoint) (*agentpb.Checkpoint, error) {
	if !bytes.Equal(head.GetSha256(), p.sum) || len(p.data) != int(head.GetParts()) {
		return nil, fmt.Errorf("a packed checkpoint's %d parts are missing", head.GetParts())
	}
	if head.GetSize() > uint64(maxCheckpoint) {
		return nil, fmt.Errorf("%w: %d bytes", errCheckpointTooLarge, head.GetSize())
	}
	packed := bytes.Join(p.data, nil)
	if sum := sha256.Sum256(packed); !bytes.Equal(sum[:], head.GetSha256()) {
		return nil, errors.New("a packed checkpoint's parts don't match its checksum")
	}
	zr := flate.NewReader(bytes.NewReader(packed))
	data, err := io.ReadAll(io.LimitReader(zr, int64(head.GetSize())+1))
	if err != nil {
		return nil, fmt.Errorf("decompressing a packed checkpoint: %w", err)
	}
	if uint64(len(data)) != head.GetSize() {
		return nil, fmt.Errorf("a packed checkpoint is %d bytes, not %d", len(data), head.GetSize())
	}
	cp := new(agentpb.Checkpoint)
	if err := proto.Unmarshal(data, cp); err != nil {
		return nil, fmt.Errorf("a packed checkpoint: %w", err)
	}
	*p = parts{}
	return cp, nil
}
