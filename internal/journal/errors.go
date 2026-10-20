package journal

import (
	"errors"
	"fmt"
)

var (
	// ErrCorrupt reports data in a complete part of the journal that fails
	// validation. Corruption is never skipped: everything after it is
	// unreadable until an operator decides what to do.
	ErrCorrupt = errors.New("journal: corrupt")

	// ErrTornTail reports an incomplete or damaged final batch at the end of
	// the file, left by a write that never fully reached the disk. Recovery
	// truncates it.
	ErrTornTail = errors.New("journal: incomplete trailing batch")

	// ErrLimit reports a length that exceeds the configured limits.
	ErrLimit = errors.New("journal: limit exceeded")

	// ErrUnsupported reports a format version, codec or checksum this build
	// doesn't know.
	ErrUnsupported = errors.New("journal: unsupported")

	// ErrLocked reports that another writer holds the journal.
	ErrLocked = errors.New("journal: locked by another writer")

	// ErrNotDurable reports that an append was not acknowledged as durable.
	// The record may or may not be present when the journal is reopened.
	ErrNotDurable = errors.New("journal: append not durable")

	// ErrWriterFailed reports that the writer stopped after an earlier write
	// or sync failure. The journal must be reopened to continue.
	ErrWriterFailed = errors.New("journal: writer failed")

	// ErrClosed reports use of a closed writer.
	ErrClosed = errors.New("journal: writer closed")

	// errLockUnavailable reports that the platform or filesystem can't lock
	// the journal. The writer warns and continues without the lock.
	errLockUnavailable = errors.New("file locking unavailable")
)

// FormatError describes a problem at a specific offset in a journal file.
// It wraps one of ErrCorrupt, ErrTornTail, ErrLimit or ErrUnsupported.
type FormatError struct {
	Offset int64
	Kind   error
	Reason string
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("%v at offset %d: %s", e.Kind, e.Offset, e.Reason)
}

func (e *FormatError) Unwrap() error { return e.Kind }

func formatErr(off int64, kind error, format string, args ...any) error {
	return &FormatError{Offset: off, Kind: kind, Reason: fmt.Sprintf(format, args...)}
}
