// Package archive writes and extracts archives of directories and transfers them to Azure Blob Storage.
//
// An archive is a Zstandard stream of a tar stream.
// [WriteDirectory] splits the tar stream into chunks of whole entries and compresses each chunk to a separate Zstandard frame, so that [ExtractDirectory] can decompress and extract chunks in parallel.
// A skippable frame before each chunk frame holds the sizes of the chunk.
// The last chunk contains the end-of-archive marker of the tar stream.
package archive

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

const (
	// chunkHeaderMagic is a skippable frame magic number (RFC 8878 section 3.1.2), so that Zstandard decoders that do not know chunk headers skip them.
	chunkHeaderMagic       = 0x184d2a5b
	chunkHeaderPayloadSize = 20
	chunkHeaderSize        = 8 + chunkHeaderPayloadSize
	// chunkTargetSize is the size of the tar stream part in a chunk at which WriteDirectory starts the next chunk.
	// A chunk with a larger regular file is larger.
	chunkTargetSize = 4 << 20
	// chunkLast is the flag of the last chunk of an archive.
	chunkLast = 1
)

type chunkHeader struct {
	// compressedSize is the size of the Zstandard frame that follows the chunk header.
	compressedSize uint64
	// uncompressedSize is the size of the tar stream part in the frame.
	uncompressedSize uint64
	flags            uint32
}

func (h chunkHeader) append(data []byte) []byte {
	data = binary.LittleEndian.AppendUint32(data, chunkHeaderMagic)
	data = binary.LittleEndian.AppendUint32(data, chunkHeaderPayloadSize)
	data = binary.LittleEndian.AppendUint64(data, h.compressedSize)
	data = binary.LittleEndian.AppendUint64(data, h.uncompressedSize)
	return binary.LittleEndian.AppendUint32(data, h.flags)
}

func parseChunkHeader(data []byte) (chunkHeader, error) {
	if binary.LittleEndian.Uint32(data) != chunkHeaderMagic || binary.LittleEndian.Uint32(data[4:]) != chunkHeaderPayloadSize {
		return chunkHeader{}, fmt.Errorf("invalid chunk header")
	}
	h := chunkHeader{
		compressedSize:   binary.LittleEndian.Uint64(data[8:]),
		uncompressedSize: binary.LittleEndian.Uint64(data[16:]),
		flags:            binary.LittleEndian.Uint32(data[24:]),
	}
	switch {
	case h.compressedSize > math.MaxInt || h.uncompressedSize > math.MaxInt:
		return chunkHeader{}, fmt.Errorf("chunk size exceeds %d bytes", math.MaxInt)
	case h.flags&^chunkLast != 0:
		return chunkHeader{}, fmt.Errorf("unknown chunk flags %#x", h.flags)
	}
	return h, nil
}

// chunkBuffer holds the data of a chunk.
type chunkBuffer struct {
	data []byte
}

// Write appends data to the buffer.
// If data starts at the space that spare returned, Write appends it without a copy.
func (b *chunkBuffer) Write(data []byte) (int, error) {
	if free := b.data[len(b.data):cap(b.data)]; len(data) > 0 && len(free) >= len(data) && &free[0] == &data[0] {
		b.data = b.data[:len(b.data)+len(data)]
		return len(data), nil
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

// spare returns size bytes of free space after the data.
func (b *chunkBuffer) spare(size int) []byte {
	b.data = slices.Grow(b.data, size)
	return b.data[len(b.data) : len(b.data)+size]
}

func getBuffer(buffers chan *chunkBuffer, capacity int) *chunkBuffer {
	select {
	case buffer := <-buffers:
		buffer.data = buffer.data[:0]
		return buffer
	default:
		return &chunkBuffer{data: make([]byte, 0, capacity)}
	}
}

func putBuffer(buffers chan *chunkBuffer, buffer *chunkBuffer) {
	select {
	case buffers <- buffer:
	default:
	}
}
