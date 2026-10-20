// Package journal implements the session journal: an append-only file of
// independently compressed, checksummed batches of records that can be
// reopened safely after a crash.
//
// The on-disk format is described in docs/journal-format.md. It is
// experimental (format version 0) and may change incompatibly.
package journal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"unicode/utf8"
)

// FormatVersion is the journal format version this package writes and reads.
const FormatVersion uint16 = 0

// MaxSessionIDLen is the maximum length of a session ID in bytes.
const MaxSessionIDLen = 255

// fileMagic starts every journal file. The leading non-ASCII byte and the
// trailing newline catch text-mode transfers and files that aren't journals.
var fileMagic = [8]byte{0x89, 'A', 'C', 'T', 'L', 'E', 'T', '\n'}

const (
	fileHeaderFixedLen = 14 // magic(8) version(2) flags(2) idLen(2)
	fileHeaderCRCLen   = 4
)

// Batch framing:
//
//	header   32 bytes  (see encodeBatchHeader)
//	payload  storedLen bytes, encoded by the batch codec
//	checksum 4 bytes   over header and payload, algorithm from the header
//	trailer  4 bytes   batchTrailerMagic, marks the batch as complete
const (
	batchHeaderLen   = 32
	batchChecksumLen = 4
	batchTrailerLen  = 4
	batchOverhead    = batchHeaderLen + batchChecksumLen + batchTrailerLen
)

var (
	batchMagic        = [4]byte{'A', 'J', 'B', 'H'}
	batchTrailerMagic = [4]byte{'A', 'J', 'B', 'C'}
)

// crc32c is used for the header CRCs, which are fixed so that lengths can be
// trusted before anything else about a batch is known.
var crc32c = crc32.MakeTable(crc32.Castagnoli)

// Header is the journal file header.
type Header struct {
	Version   uint16
	SessionID string
}

func validateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("journal: session ID is empty")
	}
	if len(id) > MaxSessionIDLen {
		return fmt.Errorf("journal: session ID is %d bytes, max %d", len(id), MaxSessionIDLen)
	}
	if !utf8.ValidString(id) {
		return fmt.Errorf("journal: session ID is not valid UTF-8")
	}
	return nil
}

func encodeFileHeader(h Header) []byte {
	n := fileHeaderFixedLen + len(h.SessionID)
	b := make([]byte, n+fileHeaderCRCLen)
	copy(b, fileMagic[:])
	binary.LittleEndian.PutUint16(b[8:], h.Version)
	binary.LittleEndian.PutUint16(b[10:], 0) // flags
	binary.LittleEndian.PutUint16(b[12:], uint16(len(h.SessionID)))
	copy(b[14:], h.SessionID)
	binary.LittleEndian.PutUint32(b[n:], crc32.Checksum(b[:n], crc32c))
	return b
}

// batchHeader is the fixed-size header in front of every batch payload.
type batchHeader struct {
	codec     CodecID
	checksum  ChecksumID
	flags     uint16 // no flags are defined yet
	firstSeq  uint64
	count     uint32
	storedLen uint32 // payload length on disk
	rawLen    uint32 // payload length after decoding
}

func (h batchHeader) lastSeq() uint64 { return h.firstSeq + uint64(h.count) - 1 }

// frameLen is the total on-disk size of the batch, framing included.
func (h batchHeader) frameLen() int64 { return batchOverhead + int64(h.storedLen) }

// encodeBatchHeader writes h into b[:batchHeaderLen]:
//
//	0  magic      4
//	4  codec      1
//	5  checksum   1
//	6  flags      2  (none defined; must be zero)
//	8  firstSeq   8
//	16 count      4
//	20 storedLen  4
//	24 rawLen     4
//	28 headerCRC  4  CRC-32C of bytes 0..28
func encodeBatchHeader(b []byte, h batchHeader) {
	_ = b[batchHeaderLen-1]
	copy(b, batchMagic[:])
	b[4] = byte(h.codec)
	b[5] = byte(h.checksum)
	binary.LittleEndian.PutUint16(b[6:], h.flags)
	binary.LittleEndian.PutUint64(b[8:], h.firstSeq)
	binary.LittleEndian.PutUint32(b[16:], h.count)
	binary.LittleEndian.PutUint32(b[20:], h.storedLen)
	binary.LittleEndian.PutUint32(b[24:], h.rawLen)
	binary.LittleEndian.PutUint32(b[28:], crc32.Checksum(b[:28], crc32c))
}

// decodeBatchHeader parses b[:batchHeaderLen]. ok is false if the magic or
// header CRC doesn't match; the caller decides whether that is a torn tail or
// corruption.
func decodeBatchHeader(b []byte) (h batchHeader, ok bool) {
	_ = b[batchHeaderLen-1]
	if [4]byte(b[:4]) != batchMagic {
		return h, false
	}
	if binary.LittleEndian.Uint32(b[28:]) != crc32.Checksum(b[:28], crc32c) {
		return h, false
	}
	return batchHeader{
		codec:     CodecID(b[4]),
		checksum:  ChecksumID(b[5]),
		flags:     binary.LittleEndian.Uint16(b[6:]),
		firstSeq:  binary.LittleEndian.Uint64(b[8:]),
		count:     binary.LittleEndian.Uint32(b[16:]),
		storedLen: binary.LittleEndian.Uint32(b[20:]),
		rawLen:    binary.LittleEndian.Uint32(b[24:]),
	}, true
}
