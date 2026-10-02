// Package treehash implements the block-wise BLAKE3 tree hash TelDrive stores as
// a file's content fingerprint.
//
// A stream is split into BlockSize chunks, each chunk is hashed on its own, and
// the concatenated chunk digests are hashed once more by ComputeTreeHash. The
// two-level scheme lets an uploader hash chunks as they arrive instead of
// buffering the whole file, and lets the server validate the digest list it is
// handed before trusting the resulting file hash.
package treehash

import (
	"encoding/hex"

	"github.com/zeebo/blake3"
)

const (
	// BlockSize is the fixed block size for tree hashing (16MB)
	BlockSize = 16 * 1024 * 1024

	// DigestSize is the length in bytes of a single BLAKE3 digest, and therefore
	// the stride of the concatenated digest list produced by BlockHasher.Sum.
	DigestSize = 32
)

// Type represents the hash algorithm type
type Type string

const (
	// TypeBlake3 is the only supported hash algorithm (fastest)
	TypeBlake3 Type = "blake3"
)

// BlockHasher processes data in fixed-size blocks and accumulates block hashes
type BlockHasher struct {
	// blockSize is the chunk size the stream is split into. NewBlockHasher sets it
	// to BlockSize; Write relies on it being positive.
	blockSize int64

	// currentHash accumulates the block currently being filled. It is nil until
	// the first Write starts a block.
	currentHash *blake3.Hasher

	// blockHashes holds the digest of every completed block, in stream order.
	blockHashes [][]byte

	// bytesInBlock counts how many bytes of the current block have been written.
	bytesInBlock int64
}

// NewBlockHasher creates a new BlockHasher (always BLAKE3)
func NewBlockHasher() *BlockHasher {
	return &BlockHasher{
		blockSize: BlockSize,
	}
}

// Write implements io.Writer - processes data in BlockSize chunks
func (h *BlockHasher) Write(p []byte) (n int, err error) {
	n = len(p)

	for len(p) > 0 {
		remaining := h.blockSize - h.bytesInBlock
		toWrite := min(int64(len(p)), remaining)

		// Initialize hash if this is a new block
		if h.bytesInBlock == 0 {
			h.currentHash = blake3.New()
		}

		h.currentHash.Write(p[:toWrite])
		h.bytesInBlock += toWrite
		p = p[toWrite:]

		// Block is complete
		if h.bytesInBlock >= h.blockSize {
			h.blockHashes = append(h.blockHashes, h.currentHash.Sum(nil))
			h.bytesInBlock = 0
		}
	}

	return n, nil
}

// Sum returns concatenated block hashes
func (h *BlockHasher) Sum() []byte {
	count := len(h.blockHashes)
	capacity := count * DigestSize
	if h.bytesInBlock > 0 {
		capacity += DigestSize
	}
	result := make([]byte, 0, capacity)
	for _, blockHash := range h.blockHashes {
		result = append(result, blockHash...)
	}
	if h.bytesInBlock > 0 {
		result = append(result, h.currentHash.Sum(nil)...)
	}
	return result
}

// BlockCount returns the number of complete blocks processed
func (h *BlockHasher) BlockCount() int {
	return len(h.blockHashes)
}

// Reset resets the hasher for a new stream
func (h *BlockHasher) Reset() {
	h.blockHashes = nil
	h.bytesInBlock = 0
}

// ComputeTreeHash computes the final tree hash from concatenated block hashes
func ComputeTreeHash(concatenatedBlockHashes []byte) []byte {
	h := blake3.New()
	h.Write(concatenatedBlockHashes)
	return h.Sum(nil)
}

// SumToHex converts bytes to hex string
func SumToHex(data []byte) string {
	return hex.EncodeToString(data)
}
