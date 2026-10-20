package journal

import (
	"fmt"
	"hash/crc32"

	"github.com/klauspost/compress/zstd"
)

// CodecID identifies how a batch payload is encoded on disk.
type CodecID uint8

const (
	CodecNone CodecID = 0
	CodecZstd CodecID = 1
)

func (c CodecID) String() string {
	switch c {
	case CodecNone:
		return "none"
	case CodecZstd:
		return "zstd"
	}
	return fmt.Sprintf("codec(%d)", uint8(c))
}

// codec encodes and decodes batch payloads.
type codec interface {
	// encode appends the encoding of src to dst.
	encode(dst, src []byte) []byte
	// decode appends the decoding of src to dst. It fails if the result
	// isn't exactly rawLen bytes long, and never allocates more than the
	// codec's configured maximum batch size while finding out.
	decode(dst, src []byte, rawLen int) ([]byte, error)
}

type noneCodec struct{}

func (noneCodec) encode(dst, src []byte) []byte { return append(dst, src...) }

func (noneCodec) decode(dst, src []byte, rawLen int) ([]byte, error) {
	if len(src) != rawLen {
		return nil, fmt.Errorf("stored length %d != raw length %d", len(src), rawLen)
	}
	return append(dst, src...), nil
}

// zstdCodec writes one Zstandard frame per batch.
type zstdCodec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// newZstdCodec returns a codec whose decoder refuses frames that would
// decode to more than maxRaw bytes or need a larger window.
func newZstdCodec(maxRaw int) (*zstdCodec, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false), // the batch checksum covers the payload
	)
	if err != nil {
		return nil, err
	}
	window := max(maxRaw, zstd.MinWindowSize)
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maxRaw)),
		zstd.WithDecoderMaxWindow(uint64(window)),
		zstd.WithDecoderLowmem(true),
	)
	if err != nil {
		return nil, err
	}
	return &zstdCodec{enc: enc, dec: dec}, nil
}

func (c *zstdCodec) encode(dst, src []byte) []byte { return c.enc.EncodeAll(src, dst) }

func (c *zstdCodec) decode(dst, src []byte, rawLen int) ([]byte, error) {
	start := len(dst)
	out, err := c.dec.DecodeAll(src, dst)
	if err != nil {
		return nil, err
	}
	if n := len(out) - start; n != rawLen {
		return nil, fmt.Errorf("decoded length %d != raw length %d", n, rawLen)
	}
	return out, nil
}

func (c *zstdCodec) close() { c.dec.Close() }

// codecs holds one instance of each codec for a reader or writer.
type codecs struct {
	zstd *zstdCodec
}

func newCodecs(l Limits) (*codecs, error) {
	z, err := newZstdCodec(l.MaxBatchRaw)
	if err != nil {
		return nil, err
	}
	return &codecs{zstd: z}, nil
}

func (c *codecs) get(id CodecID) (codec, bool) {
	switch id {
	case CodecNone:
		return noneCodec{}, true
	case CodecZstd:
		return c.zstd, true
	}
	return nil, false
}

func (c *codecs) close() { c.zstd.close() }

// ChecksumID identifies the batch checksum algorithm.
type ChecksumID uint8

const ChecksumCRC32C ChecksumID = 1

func (c ChecksumID) String() string {
	if c == ChecksumCRC32C {
		return "crc32c"
	}
	return fmt.Sprintf("checksum(%d)", uint8(c))
}

// batchChecksum computes the checksum of a batch's header and payload.
func batchChecksum(id ChecksumID, header, payload []byte) (uint32, bool) {
	if id != ChecksumCRC32C {
		return 0, false
	}
	return crc32.Update(crc32.Checksum(header, crc32c), crc32c, payload), true
}
