package journal

import "fmt"

// Limits bound what a reader accepts and a writer produces. Readers never
// allocate more than these limits allow, whatever the file claims.
type Limits struct {
	// MaxBatchRaw is the maximum decoded size of one batch payload.
	MaxBatchRaw int
	// MaxBatchStored is the maximum on-disk size of one batch payload.
	MaxBatchStored int
	// MaxBatchRecords is the maximum number of records in one batch.
	MaxBatchRecords int
	// MaxRecordData is the maximum size of one record's data.
	MaxRecordData int
	// MaxKindLen is the maximum length of a record kind.
	MaxKindLen int
}

// DefaultLimits returns the limits used when none are configured.
func DefaultLimits() Limits {
	return Limits{
		MaxBatchRaw:     16 << 20,
		MaxBatchStored:  16 << 20,
		MaxBatchRecords: 1 << 16,
		MaxRecordData:   4 << 20,
		MaxKindLen:      128,
	}
}

// recordOverhead bounds the protobuf encoding overhead of one record inside
// a batch, excluding kind and data: tags, varints and length prefixes.
const recordOverhead = 48

func (l Limits) validate() error {
	switch {
	case l.MaxBatchRaw <= 0, l.MaxBatchStored <= 0, l.MaxBatchRecords <= 0,
		l.MaxRecordData <= 0, l.MaxKindLen <= 0:
		return fmt.Errorf("journal: limits must be positive: %+v", l)
	case l.MaxBatchRaw > 1<<30 || l.MaxBatchStored > 1<<30:
		return fmt.Errorf("journal: batch limits must not exceed 1 GiB")
	case l.MaxBatchStored < l.MaxBatchRaw:
		// A batch that doesn't compress is stored as is, so it must fit.
		return fmt.Errorf("journal: MaxBatchStored (%d) is less than MaxBatchRaw (%d)", l.MaxBatchStored, l.MaxBatchRaw)
	case l.MaxRecordData+l.MaxKindLen+recordOverhead > l.MaxBatchRaw:
		return fmt.Errorf("journal: MaxBatchRaw (%d) can't hold one maximal record", l.MaxBatchRaw)
	}
	return nil
}
