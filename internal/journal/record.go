package journal

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/journal/journalpb"
)

// Record is one journal entry.
type Record struct {
	// Seq is assigned by the writer: contiguous, starting at 1.
	Seq uint64
	// Kind names the record type. The storage layer doesn't interpret it.
	Kind string
	// Time is when the record was appended. The writer sets it if zero.
	Time time.Time
	// Data is the opaque record payload.
	Data []byte
}

// encodedSizeBound returns an upper bound for the record's encoded size
// inside a batch.
func (r *Record) encodedSizeBound() int { return len(r.Kind) + len(r.Data) + recordOverhead }

func marshalBatch(buf []byte, recs []Record) ([]byte, error) {
	b := &journalpb.Batch{Records: make([]*journalpb.Record, len(recs))}
	for i := range recs {
		b.Records[i] = &journalpb.Record{
			Seq:          recs[i].Seq,
			Kind:         recs[i].Kind,
			TimeUnixNano: recs[i].Time.UnixNano(),
			Data:         recs[i].Data,
		}
	}
	return proto.MarshalOptions{}.MarshalAppend(buf[:0], b)
}

// unmarshalBatch decodes a batch payload and checks that it holds exactly
// count records numbered from firstSeq.
func unmarshalBatch(raw []byte, h batchHeader, l Limits) ([]Record, error) {
	var b journalpb.Batch
	if err := proto.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode records: %w", err)
	}
	if len(b.Records) != int(h.count) {
		return nil, fmt.Errorf("batch holds %d records, header says %d", len(b.Records), h.count)
	}
	recs := make([]Record, len(b.Records))
	for i, r := range b.Records {
		if want := h.firstSeq + uint64(i); r.Seq != want {
			return nil, fmt.Errorf("record %d has seq %d, want %d", i, r.Seq, want)
		}
		if len(r.Kind) > l.MaxKindLen || len(r.Data) > l.MaxRecordData {
			return nil, fmt.Errorf("record seq %d exceeds record limits", r.Seq)
		}
		recs[i] = Record{Seq: r.Seq, Kind: r.Kind, Time: time.Unix(0, r.TimeUnixNano), Data: r.Data}
	}
	return recs, nil
}
