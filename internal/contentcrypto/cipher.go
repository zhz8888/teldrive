// Package contentcrypto implements the chunked, seekable encryption format that
// TelDrive uses for stored file content.
//
// An encrypted file is a fixed 34-byte header followed by independently sealed
// blocks:
//
//	0   magic      10 bytes, the text "TELDRIVE" with two trailing NUL bytes
//	10  file nonce 24 bytes, the secretbox nonce of block 0
//	34  block 0    16-byte authentication tag followed by up to 64 KiB of plaintext
//	    block N    the same layout, sealed with the file nonce plus N
//
// A Cipher derives its keys from caller-supplied key material and a per-part
// salt with scrypt, and both the key derivation and the block layout are
// byte-compatible with upstream TelDrive so that content written by either
// implementation decrypts in the other. EncryptData and DecryptData handle whole
// streams, while DecryptDataSeek decrypts a stored range without reading the
// blocks before it, which is what ranged downloads use.
package contentcrypto

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

const (
	// nameCipherBlockSize is the width in bytes of the tweak reserved for
	// file-name encryption, equal to the AES block size. No file-name
	// encryption exists in this package, but the key schedule that Key derives
	// still spans this width.
	nameCipherBlockSize = aes.BlockSize
	// nameKeySize is the width in bytes of the file-name key that upstream
	// TelDrive derives between the data key and the tweak. Content blocks never
	// use it, but the fixed key schedule still derives this many bytes.
	nameKeySize = 32
	// fileMagic is the marker that starts every encrypted file: the ASCII text
	// "TELDRIVE" followed by two NUL bytes.
	fileMagic = "TELDRIVE\x00\x00"
	// fileMagicSize is the length of fileMagic in bytes.
	fileMagicSize = len(fileMagic)
	// fileNonceSize is the size of the per-file nonce in bytes, which is also
	// the nonce width required by secretbox.
	fileNonceSize = 24
	// fileHeaderSize is the size of the fixed file header in bytes: the magic
	// followed by the file nonce.
	fileHeaderSize = fileMagicSize + fileNonceSize
	// blockHeaderSize is the per-block overhead in bytes, the authentication tag
	// that secretbox prepends to the payload of every sealed block.
	blockHeaderSize = secretbox.Overhead
	// blockDataSize is the largest plaintext payload one block can carry, in
	// bytes. Only the final block of a file may be shorter.
	blockDataSize = 64 * 1024
	// blockSize is the buffer size one block needs: a full payload plus its
	// authentication tag. It is an upper bound, because the final block of a
	// file is usually shorter.
	blockSize = blockHeaderSize + blockDataSize
)

var (
	// ErrorEncryptedFileTooShort is returned when stored content is smaller than
	// the encrypted header, so it cannot even carry the magic and the file
	// nonce. Callers must test it with errors.Is.
	ErrorEncryptedFileTooShort = errors.New("file is too short to be encrypted")
	// ErrorEncryptedFileBadHeader is returned when a trailing fragment of the
	// content is no longer than a block authentication tag, so it carries no
	// payload and the recorded size is not a valid encrypted length. Callers
	// must test it with errors.Is.
	ErrorEncryptedFileBadHeader = errors.New("file has truncated block header")
	// ErrorEncryptedBadMagic is returned when the header does not start with
	// fileMagic, which means the content is not a file encrypted by TelDrive.
	// Callers must test it with errors.Is.
	ErrorEncryptedBadMagic = errors.New("not an encrypted file - bad magic string")
	// ErrorFileClosed is returned by reads performed after Close and by every
	// Close call after the first. Callers must test it with errors.Is.
	ErrorFileClosed = errors.New("file already closed")
	// ErrorBadSeek is returned when a seek target lies beyond the block that was
	// fetched for it, which happens when the source returns fewer bytes than the
	// target offset requires. Callers must test it with errors.Is.
	ErrorBadSeek = errors.New("seek beyond end of file")
	// ErrorAuthentication is returned when a block fails secretbox
	// authentication, so the stored bytes were corrupted or tampered with.
	// Callers must test it with errors.Is.
	ErrorAuthentication = errors.New("encrypted block authentication failed")
)

var (
	// fileMagicBytes is fileMagic in slice form, used both to write the header
	// and to validate it during decryption.
	fileMagicBytes = []byte(fileMagic)
)

// ReadSeekCloser is the seekable plaintext view returned by DecryptDataSeek.
// The caller owns the reader and must close it to release the range reader the
// decrypter opened.
type ReadSeekCloser interface {
	// Reader streams decrypted plaintext from the current position.
	io.Reader
	// Seeker repositions the stream; offset is a plaintext byte offset and only
	// io.SeekStart is supported.
	io.Seeker
	// Closer releases the underlying reader. A second Close returns
	// ErrorFileClosed, and reads issued after Close fail with the same sentinel.
	io.Closer
}

// OpenRangeSeek opens a byte range of stored content for a decrypter. offset and
// limit are measured in stored (still encrypted) bytes from the start of the
// content, and a negative limit means "to the end". Every call must return a
// fresh reader, because seeking closes the range the decrypter currently holds
// as it replaces it, so a shared handle would be closed while still in use.
type OpenRangeSeek func(ctx context.Context, offset, limit int64) (io.ReadCloser, error)

// readFill reads from r until buf is full or r reports an error, retrying short
// reads. It returns the number of bytes copied and the error of the final read,
// which is normally io.EOF when the stream ended early. Callers must examine n
// before err: a full buffer is returned together with io.EOF when the reader
// delivers the last bytes and the end marker in the same call.
func readFill(r io.Reader, buf []byte) (n int, err error) {
	var nn int
	for n < len(buf) && err == nil {
		nn, err = r.Read(buf[n:])
		n += nn
	}
	return n, err
}

// Cipher encrypts and decrypts file content under the keys derived from a
// password and a salt. Once its keys are set it may be shared: each stream keeps
// its own nonce and block index, every pool operation is safe for concurrent
// use, and the Cipher holds no per-stream state, so one instance can serve many
// simultaneous transfers. Key replaces the key material on the receiver and must
// therefore run before the Cipher is shared. Every stream of a given file needs
// the same salt that was used when its content was written.
type Cipher struct {
	// dataKey is the secretbox key that protects block payloads.
	dataKey [32]byte
	// buffers pools the two blockSize scratch buffers that every stream borrows,
	// so transfers do not allocate tens of kilobytes per block.
	buffers sync.Pool
	// cryptoRand is the entropy source for file nonces. Production ciphers use
	// crypto/rand.Reader; tests inject a deterministic reader to pin the output.
	cryptoRand io.Reader
}

// NewCipher derives a cipher from password and salt, drawing file nonces from
// crypto/rand.Reader. The salt must be the per-part salt stored with the
// content, because a different salt derives different keys and the content can
// no longer be decrypted. It returns an error only when key derivation fails.
func NewCipher(password, salt string) (*Cipher, error) {
	return NewCipherWithRand(password, salt, rand.Reader)
}

// NewCipherWithRand creates a compatible TelDrive cipher using the supplied
// entropy source. Production callers should use NewCipher; the explicit source
// makes compatibility tests deterministic without weakening production nonce generation.
func NewCipherWithRand(password, salt string, random io.Reader) (*Cipher, error) {
	if random == nil {
		return nil, errors.New("random source is required")
	}
	c := &Cipher{
		cryptoRand: random,
	}
	c.buffers.New = func() any {
		return new([blockSize]byte)
	}
	err := c.Key(password, salt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Key derives this cipher's key material from password and salt with scrypt
// (N=16384, r=8, p=1) and stores the data key in the receiver. The derived bytes
// are split in order into the 32-byte dataKey, the 32-byte nameKey and the
// 16-byte nameTweak that upstream TelDrive uses to encrypt file names; content
// blocks only use dataKey, but the full width is still derived because the
// parameters and the split are fixed by the stored format and changing either
// makes existing content undecryptable. It returns an error when scrypt fails.
func (c *Cipher) Key(password, salt string) error {
	const keySize = len(c.dataKey) + nameKeySize + nameCipherBlockSize
	saltBytes := []byte(salt)
	key, err := scrypt.Key([]byte(password), saltBytes, 16384, 8, 1, keySize)
	if err != nil {
		return err
	}

	copy(c.dataKey[:], key)
	return nil
}

// getBlock borrows a blockSize scratch buffer from the cipher's pool. The
// caller owns the buffer until it hands it back to putBlock.
func (c *Cipher) getBlock() *[blockSize]byte {
	return c.buffers.Get().(*[blockSize]byte)
}

// putBlock returns a buffer previously obtained from getBlock to the pool so
// another stream can reuse it. The caller must not touch buf afterwards, and
// passing nil is a no-op.
func (c *Cipher) putBlock(buf *[blockSize]byte) {
	c.buffers.Put(buf)
}

// nonce is the 24-byte secretbox nonce of one encrypted file. Block N is sealed
// with the file nonce plus N, so a reader that knows the block index can derive
// the nonce of any block without reading the blocks before it, which is what
// makes ranged decryption possible. A nonce must never be reused for a
// different block under the same key.
type nonce [fileNonceSize]byte

// pointer returns the nonce as a pointer to a fixed-size array, the form that
// secretbox.Seal and secretbox.Open expect.
func (n *nonce) pointer() *[fileNonceSize]byte {
	return (*[fileNonceSize]byte)(n)
}

// fromReader fills the nonce from in, which is how a new file gets its random
// file nonce. It returns an error when in yields fewer than fileNonceSize bytes.
func (n *nonce) fromReader(in io.Reader) error {
	read, err := readFill(in, (*n)[:])
	if read != fileNonceSize {
		return fmt.Errorf("short read of nonce: %w", err)
	}
	return nil
}

// fromBuf copies the first fileNonceSize bytes of buf into the nonce, which is
// how the file nonce is recovered from the header. It returns an error when buf
// is shorter than the nonce.
func (n *nonce) fromBuf(buf []byte) error {
	read := copy((*n)[:], buf)

	if read != fileNonceSize {
		return errors.New("buffer too short to read nonce")
	}
	return nil
}

// carry adds one to the byte at index i of the nonce and propagates the carry
// towards the more significant bytes, treating the array as a little-endian
// integer. It stops at the first byte that does not wrap around, so the common
// case costs one byte. Keeping the nonce equal to the file nonce plus the block
// index is what lets a reader jump straight to any block.
func (n *nonce) carry(i int) {
	for ; i < len(*n); i++ {
		digit := (*n)[i]
		newDigit := digit + 1
		(*n)[i] = newDigit
		if newDigit >= digit {
			// exit if no carry
			break
		}
	}
}

// increment advances the nonce by one so that the next block is sealed with a
// different nonce, keeping it equal to the file nonce plus the block index.
func (n *nonce) increment() {
	n.carry(0)
}

// add increases the nonce by x, treating its first eight bytes as a
// little-endian integer, and propagates any overflow into the remaining bytes.
// Seeking uses it to jump directly to the nonce of a target block instead of
// incrementing once per skipped block.
func (n *nonce) add(x uint64) {
	carry := uint16(0)
	for i := range 8 {
		digit := (*n)[i]
		xDigit := byte(x)
		x >>= 8
		carry += uint16(digit) + uint16(xDigit)
		(*n)[i] = byte(carry)
		carry >>= 8
	}
	if carry != 0 {
		n.carry(8)
	}
}

// encrypter turns a plaintext reader into its encrypted representation. It
// delivers the file header first and then one sealed block at a time, and it is
// a stream rather than a random-access writer: mu serialises Read calls, so one
// encrypter must not be read concurrently.
type encrypter struct {
	mu sync.Mutex
	// in is the plaintext source, consumed in blockDataSize chunks.
	in io.Reader
	c  *Cipher
	// nonce is the nonce that seals the block currently held in buf; it advances
	// after every block.
	nonce nonce
	// buf holds the bytes waiting to be delivered: the header before the first
	// Read, then the sealed block currently being consumed.
	buf *[blockSize]byte
	// readBuf stages one plaintext chunk before it is sealed into buf.
	readBuf *[blockSize]byte
	// bufIndex is the offset of the next byte to deliver from buf.
	bufIndex int
	// bufSize is the number of valid bytes in buf. It starts at fileHeaderSize so
	// that the header is delivered before any block.
	bufSize int
	// err is the terminal error of the stream; once set, Read returns it without
	// touching the source again.
	err error
}

// newEncrypter creates the encrypted stream for in. A nil nonce makes it draw a
// fresh file nonce from the cipher's entropy source, while a supplied nonce
// pins the output, which compatibility tests rely on. The returned encrypter has
// already staged the header in its buffer, so the first Read emits the magic and
// the nonce before any block. It returns an error when the nonce cannot be read,
// and it hands the buffers it borrowed back to the pool before doing so.
func (c *Cipher) newEncrypter(in io.Reader, nonce *nonce) (*encrypter, error) {
	fh := &encrypter{
		in:      in,
		c:       c,
		buf:     c.getBlock(),
		readBuf: c.getBlock(),
		bufSize: fileHeaderSize,
	}

	if nonce != nil {
		fh.nonce = *nonce
	} else {
		err := fh.nonce.fromReader(c.cryptoRand)
		if err != nil {
			// Release the buffers borrowed above, as the decrypter does on its
			// header failure paths, so a failed construction leaks nothing.
			fh.finish(err)
			return nil, err
		}
	}

	copy((*fh.buf)[:], fileMagicBytes)

	copy((*fh.buf)[fileMagicSize:], fh.nonce[:])
	return fh, nil
}

// Read seals and returns the next bytes of the encrypted stream, emitting the
// file header first. When the staged bytes are exhausted it reads up to
// blockDataSize plaintext bytes and seals them, so a Read that starts a new
// block returns at most one block worth of data. It ends with io.EOF, or with
// the source's own error, once the source yields no more data; that terminal
// error is repeated by every later call.
func (fh *encrypter) Read(p []byte) (n int, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()

	if fh.err != nil {
		return 0, fh.err
	}
	if fh.bufIndex >= fh.bufSize {

		readBuf := (*fh.readBuf)[:blockDataSize]
		n, err = readFill(fh.in, readBuf)
		if n == 0 {
			return fh.finish(err)
		}

		secretbox.Seal((*fh.buf)[:0], readBuf[:n], fh.nonce.pointer(), &fh.c.dataKey)
		fh.bufIndex = 0
		fh.bufSize = blockHeaderSize + n
		fh.nonce.increment()
	}
	n = copy(p, (*fh.buf)[fh.bufIndex:fh.bufSize])
	fh.bufIndex += n
	return n, nil
}

// finish records err as the terminal error of the stream, hands both borrowed
// buffers back to the cipher's pool and clears the pointers so that the released
// buffers can never be touched again. The first call wins: later calls return
// the stored error and release nothing a second time.
func (fh *encrypter) finish(err error) (int, error) {
	if fh.err != nil {
		return 0, fh.err
	}
	fh.err = err
	fh.c.putBlock(fh.buf)
	fh.buf = nil
	fh.c.putBlock(fh.readBuf)
	fh.readBuf = nil
	return 0, err
}

// Close satisfies io.ReadCloser and deliberately does nothing: the encrypter
// owns no operating-system resources, and its buffers go back to the pool when
// the source reaches EOF. Closing early therefore does not end the stream, and
// reads may still be issued afterwards.
func (fh *encrypter) Close() error {
	return nil
}

// EncryptData returns a reader over the encrypted form of in: the header first,
// then sealed blocks. The reader borrows buffers from the cipher's pool and
// releases them once it reaches EOF, so callers that stop early simply abandon
// it. It returns an error only when the file nonce cannot be drawn from the
// cipher's entropy source.
func (c *Cipher) EncryptData(in io.Reader) (io.ReadCloser, error) {
	return c.newEncrypter(in, nil)
}

// decrypter authenticates and decrypts an encrypted stream supplied by an
// io.ReadCloser. It is the reading counterpart of encrypter and, when it was
// created through newDecrypterSeek, it can also reopen the source at a block
// boundary and resume there. Its methods are serialised by mu, so it must not be
// read and seeked concurrently.
type decrypter struct {
	mu sync.Mutex
	// rc is the stored content currently being read. A seek closes the range it
	// replaces and installs a freshly opened one, so Close always releases the
	// current range and no range the decrypter opened stays open.
	rc io.ReadCloser
	// nonce is the nonce of the block currently held in buf.
	nonce nonce
	// initialNonce is the file nonce taken from the header. Seeking recomputes a
	// block nonce by adding the block index to it.
	initialNonce nonce
	c            *Cipher
	// buf holds the authenticated plaintext of the current block.
	buf *[blockSize]byte
	// readBuf stages the raw encrypted block read from rc before it is opened.
	readBuf *[blockSize]byte
	// bufIndex is the offset of the next plaintext byte to deliver from buf.
	bufIndex int
	// bufSize is the number of valid plaintext bytes in buf.
	bufSize int
	// err is the terminal error of the stream. Once set, Read returns it; io.EOF
	// records a clean end of stream and is the one value RangeSeek can revive.
	err error
	// limit is the number of plaintext bytes still to deliver, or -1 for no
	// limit. When it reaches zero the next Read finishes with io.EOF, which is
	// how a ranged download stops at its requested length.
	limit int64
	// open reopens the source at a stored byte range. It is nil unless the
	// decrypter came from newDecrypterSeek, and RangeSeek refuses to run without
	// it.
	open OpenRangeSeek
	// ctx is the context RangeSeek passes to open when Seek reopens the source.
	ctx context.Context
}

// newDecrypter reads and validates the encrypted header of rc, then returns a
// reader positioned at the first block. It returns ErrorEncryptedFileTooShort
// when rc ends before the header is complete, ErrorEncryptedBadMagic when the
// magic does not match, and any other read error unchanged. On those header
// failures it closes rc before returning, so the caller only owns rc when the
// returned error is nil.
func (c *Cipher) newDecrypter(rc io.ReadCloser) (*decrypter, error) {
	fh := &decrypter{
		rc:      rc,
		c:       c,
		buf:     c.getBlock(),
		readBuf: c.getBlock(),
		limit:   -1,
	}

	readBuf := (*fh.readBuf)[:fileHeaderSize]
	n, err := readFill(fh.rc, readBuf)
	if n < fileHeaderSize && err == io.EOF {

		return nil, fh.finishAndClose(ErrorEncryptedFileTooShort)
	} else if err != io.EOF && err != nil {
		return nil, fh.finishAndClose(err)
	}

	if !bytes.Equal(readBuf[:fileMagicSize], fileMagicBytes) {
		return nil, fh.finishAndClose(ErrorEncryptedBadMagic)
	}

	err = fh.nonce.fromBuf(readBuf[fileMagicSize:])
	if err != nil {
		return nil, fh.finishAndClose(err)
	}
	fh.initialNonce = fh.nonce
	return fh, nil
}

// newDecrypterSeek returns a decrypter over the plaintext range that begins at
// offset and is at most limit bytes long, where a negative limit means "to the
// end". It asks open for as little as possible: the whole content when the range
// is unbounded, one header-plus-payload range when the range starts at byte 0,
// and otherwise only the header, after which RangeSeek reopens the content at
// the target block. ctx is retained and reused by later Seek calls. On success
// the returned reader owns every range it opens, so callers close the reader
// rather than the individual ranges; if any step fails, the ranges opened so far
// are closed. A negative offset is rejected with ErrorBadSeek before anything is
// opened or mutated.
func (c *Cipher) newDecrypterSeek(ctx context.Context, open OpenRangeSeek, offset, limit int64) (fh *decrypter, err error) {
	if offset < 0 {
		return nil, ErrorBadSeek
	}
	var rc io.ReadCloser
	doRangeSeek := false
	setLimit := false

	if offset == 0 && limit < 0 {

		rc, err = open(ctx, 0, -1)
	} else if offset == 0 {

		_, underlyingLimit, _, _ := calculateUnderlying(offset, limit)
		rc, err = open(ctx, 0, int64(fileHeaderSize)+underlyingLimit)
		setLimit = true
	} else {

		rc, err = open(ctx, 0, int64(fileHeaderSize))
		doRangeSeek = true
	}
	if err != nil {
		return nil, err
	}

	fh, err = c.newDecrypter(rc)
	if err != nil {
		return nil, err
	}
	fh.open = open
	fh.ctx = ctx
	if doRangeSeek {
		_, err = fh.RangeSeek(ctx, offset, io.SeekStart, limit)
		if err != nil {
			fh.Close()
			return nil, err
		}
	}
	if setLimit {
		fh.limit = limit
	}
	return fh, nil
}

// fillBuffer reads the next encrypted block from rc, authenticates it and
// exposes its plaintext through buf. It returns ErrorEncryptedFileBadHeader when
// the remaining bytes are no longer than the authentication tag, and
// ErrorAuthentication when secretbox cannot verify the block, zeroing the bytes
// it read into the output buffer before returning that error. A clean end of
// stream returns the source's io.EOF. On success the buffer bounds are reset to
// the plaintext and the nonce advances to the following block.
func (fh *decrypter) fillBuffer() (err error) {

	readBuf := fh.readBuf
	n, err := readFill(fh.rc, (*readBuf)[:])
	if n == 0 {
		return err
	}

	if n <= blockHeaderSize {
		if err != nil && err != io.EOF {
			return err
		}
		return ErrorEncryptedFileBadHeader
	}

	_, ok := secretbox.Open((*fh.buf)[:0], (*readBuf)[:n], fh.nonce.pointer(), &fh.c.dataKey)
	if !ok {
		for i := range (*fh.buf)[:n] {
			(*fh.buf)[i] = 0
		}
		return ErrorAuthentication
	}
	fh.bufIndex = 0
	fh.bufSize = n - blockHeaderSize
	fh.nonce.increment()
	return nil
}

// Read returns decrypted plaintext from the current block, fetching and
// authenticating the next block when the buffer is exhausted. When a limit is in
// force and its last byte has been delivered, Read finishes the stream with
// io.EOF. A failure is sticky: every later call repeats that error until
// RangeSeek revives the reader.
func (fh *decrypter) Read(p []byte) (n int, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()

	if fh.err != nil {
		return 0, fh.err
	}
	if fh.bufIndex >= fh.bufSize {
		err = fh.fillBuffer()
		if err != nil {
			return 0, fh.finish(err)
		}
	}
	toCopy := fh.bufSize - fh.bufIndex
	if fh.limit >= 0 && fh.limit < int64(toCopy) {
		toCopy = int(fh.limit)
	}
	n = copy(p, (*fh.buf)[fh.bufIndex:fh.bufIndex+toCopy])
	fh.bufIndex += n
	if fh.limit >= 0 {
		fh.limit -= int64(n)
		if fh.limit == 0 {
			return n, fh.finish(io.EOF)
		}
	}
	return n, nil
}

// calculateUnderlying maps a plaintext offset and limit onto the stored bytes
// that hold them. It returns the stored offset to open at, the number of stored
// bytes covering the range or -1 when limit is negative, the plaintext bytes to
// discard inside the first block, and the index of that first block, which
// doubles as the amount by which the file nonce must be advanced.
func calculateUnderlying(offset, limit int64) (underlyingOffset, underlyingLimit, discard, blocks int64) {

	blocks, discard = offset/blockDataSize, offset%blockDataSize

	underlyingOffset = int64(fileHeaderSize) + blocks*(blockHeaderSize+blockDataSize)

	underlyingLimit = int64(-1)
	if limit >= 0 {

		bytesToRead := limit - (blockDataSize - discard)

		blocksToRead := int64(1)

		if bytesToRead > 0 {

			extraBlocksToRead, endBytes := bytesToRead/blockDataSize, bytesToRead%blockDataSize
			if endBytes != 0 {

				extraBlocksToRead++
			}
			blocksToRead += extraBlocksToRead
		}

		underlyingLimit = blocksToRead * (blockHeaderSize + blockDataSize)
	}
	return
}

// RangeSeek repositions the reader to a plaintext offset and caps the remaining
// output at limit bytes, where -1 means no cap; it returns the new offset. Only
// io.SeekStart is accepted. It reopens the content through the callback supplied
// at construction, closing the range it replaces, recomputes the block nonce as
// initialNonce plus the block index so that no earlier block has to be
// decrypted, and discards the bytes preceding the offset inside the first block.
// A reader that already ended at io.EOF is revived, while any other stored error
// stays permanent. A negative offset is rejected with ErrorBadSeek before any
// reader state changes, so it neither walks the nonce backwards nor leaves a
// negative buffer index behind. It returns ErrorBadSeek as well when the reopened
// range ends before the requested block, and it refuses to run at all unless the
// reader came from newDecrypterSeek.
func (fh *decrypter) RangeSeek(ctx context.Context, offset int64, whence int, limit int64) (int64, error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()

	if fh.open == nil {
		return 0, fh.finish(errors.New("can't seek - not initialised with newDecrypterSeek"))
	}
	if whence != io.SeekStart {
		return 0, fh.finish(errors.New("can only seek from the start"))
	}
	if offset < 0 {
		return 0, ErrorBadSeek
	}

	if fh.err == io.EOF {
		fh.unFinish()
	} else if fh.err != nil {
		return 0, fh.err
	}

	underlyingOffset, underlyingLimit, discard, blocks := calculateUnderlying(offset, limit)

	fh.nonce = fh.initialNonce
	fh.nonce.add(uint64(blocks))

	rc, err := fh.open(ctx, underlyingOffset, underlyingLimit)
	if err != nil {
		return 0, fh.finish(fmt.Errorf("couldn't reopen file with offset and limit: %w", err))
	}

	// Release the range being replaced. A callback that hands back the reader it
	// already owns keeps it open, because the two are then the same object.
	if fh.rc != nil && fh.rc != rc {
		_ = fh.rc.Close()
	}
	fh.rc = rc

	err = fh.fillBuffer()
	if err != nil {
		return 0, fh.finish(err)
	}

	if int(discard) > fh.bufSize {
		return 0, fh.finish(ErrorBadSeek)
	}
	fh.bufIndex = int(discard)

	fh.limit = limit

	return offset, nil
}

// Seek implements io.Seeker by delegating to RangeSeek with no length cap,
// reusing the context that DecryptDataSeek stored on the reader. Only
// io.SeekStart is accepted, and it fails unless the reader came from
// DecryptDataSeek.
func (fh *decrypter) Seek(offset int64, whence int) (int64, error) {
	ctx := fh.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return fh.RangeSeek(ctx, offset, whence, -1)
}

// finish stores err as the sticky stream error, hands both borrowed buffers back
// to the cipher's pool and clears the pointers so that nothing touches them
// again. The first error wins, and later calls return it without releasing
// anything a second time.
func (fh *decrypter) finish(err error) error {
	if fh.err != nil {
		return fh.err
	}
	fh.err = err
	fh.c.putBlock(fh.buf)
	fh.buf = nil
	fh.c.putBlock(fh.readBuf)
	fh.readBuf = nil
	return err
}

// unFinish revives a reader that stopped at io.EOF so that RangeSeek can reuse
// it: it clears the terminal error and borrows fresh buffers from the pool,
// leaving the block buffer empty for RangeSeek to refill. The limit is not reset
// here, because RangeSeek sets it once the new position is established.
func (fh *decrypter) unFinish() {

	fh.err = nil

	fh.buf = fh.c.getBlock()
	fh.readBuf = fh.c.getBlock()

	fh.bufIndex = 0
	fh.bufSize = 0
}

// Close releases the reader: it makes sure the pooled buffers have gone back to
// the pool, marks the stream closed and closes the current range reader. Ranges
// that a seek replaced were already closed when they were replaced. Calling it
// again returns ErrorFileClosed without touching the source, and reads after
// Close fail with the same sentinel. The error of the underlying reader's Close
// is returned.
func (fh *decrypter) Close() error {
	fh.mu.Lock()
	defer fh.mu.Unlock()

	if fh.err == ErrorFileClosed {
		return fh.err
	}

	if fh.err == nil {
		fh.finish(io.EOF)
	}

	fh.err = ErrorFileClosed
	if fh.rc == nil {
		return nil
	}
	return fh.rc.Close()
}

// finishAndClose releases the reader and then closes it, returning err unchanged
// so that the header-validation failures of newDecrypter can be reported in one
// expression without leaking the source.
func (fh *decrypter) finishAndClose(err error) error {
	fh.finish(err)
	fh.Close()
	return err
}

// DecryptData validates the header of rc and returns a reader over its
// plaintext. It returns ErrorEncryptedFileTooShort or ErrorEncryptedBadMagic for
// content that is not a TelDrive encrypted file, and in that case rc has already
// been closed. Block authentication happens as the reader is consumed, so a
// corrupted or tampered block surfaces as ErrorAuthentication from Read rather
// than from this call.
func (c *Cipher) DecryptData(rc io.ReadCloser) (io.ReadCloser, error) {
	out, err := c.newDecrypter(rc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecryptDataSeek returns a seekable reader over the plaintext range that starts
// at offset and is at most limit bytes long, where -1 means "to the end". Only
// the stored ranges needed for that window are fetched through open, and later
// Seek calls reuse ctx to reopen the content. Header errors match DecryptData;
// when the source cannot supply the block holding offset, the call fails instead
// of returning a short reader, and a negative offset fails with ErrorBadSeek
// before open is called. The same error comes back from a later Seek to a
// negative offset, which leaves the reader where it was.
func (c *Cipher) DecryptDataSeek(ctx context.Context, open OpenRangeSeek, offset, limit int64) (ReadSeekCloser, error) {
	out, err := c.newDecrypterSeek(ctx, open, offset, limit)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EncryptedSize returns how many stored bytes a plaintext of size bytes occupies
// in the encrypted format: the header, one full block for every complete
// blockDataSize chunk and one short block for the remainder. It is the value to
// record as the stored size of an uploaded part, and it is exact for every
// non-negative size, including zero.
func EncryptedSize(size int64) int64 {
	blocks, residue := size/blockDataSize, size%blockDataSize
	encryptedSize := int64(fileHeaderSize) + blocks*(blockHeaderSize+blockDataSize)
	if residue != 0 {
		encryptedSize += blockHeaderSize + residue
	}
	return encryptedSize
}

// DecryptedSize is the inverse of EncryptedSize: it returns the plaintext length
// held by an encrypted file of size bytes, which callers use to recover the
// plaintext size of stored content. It returns ErrorEncryptedFileTooShort when
// size does not even cover the header, and ErrorEncryptedFileBadHeader when the
// final block has room only for its authentication tag.
func DecryptedSize(size int64) (int64, error) {
	size -= int64(fileHeaderSize)
	if size < 0 {
		return 0, ErrorEncryptedFileTooShort
	}
	blocks, residue := size/blockSize, size%blockSize
	decryptedSize := blocks * blockDataSize
	if residue != 0 {
		residue -= blockHeaderSize
		if residue <= 0 {
			return 0, ErrorEncryptedFileBadHeader
		}
	}
	decryptedSize += residue
	return decryptedSize, nil
}
