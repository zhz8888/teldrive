package contentcrypto

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"runtime/debug"
	"testing"
)

const compatibilityCiphertextHex = "54454c44524956450000000102030405060708090a0b0c0d0e0f1011121314151617c09ad6a7579e2df65e379814cd4fd464c48776cff0caf5195a4016a3f5a43d2199a5ac9caa146a6c72fde00dab47"

func TestCipherMatchesOriginalTelDriveVector(t *testing.T) {
	t.Parallel()

	nonce := []byte{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11,
		12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23,
	}
	cipher, err := NewCipherWithRand("compat-password", "compat-salt", bytes.NewReader(nonce))
	if err != nil {
		t.Fatalf("NewCipherWithRand() error = %v", err)
	}
	encrypted, err := cipher.EncryptData(bytes.NewReader([]byte("TelDrive compatibility payload")))
	if err != nil {
		t.Fatalf("EncryptData() error = %v", err)
	}
	got, err := io.ReadAll(encrypted)
	if err != nil {
		t.Fatalf("read encrypted data: %v", err)
	}
	if hex.EncodeToString(got) != compatibilityCiphertextHex {
		t.Fatalf("ciphertext changed:\n got %x\nwant %s", got, compatibilityCiphertextHex)
	}

	decryptCipher, err := NewCipher("compat-password", "compat-salt")
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	decrypted, err := decryptCipher.DecryptData(io.NopCloser(bytes.NewReader(got)))
	if err != nil {
		t.Fatalf("DecryptData() error = %v", err)
	}
	plain, err := io.ReadAll(decrypted)
	if err != nil {
		t.Fatalf("read decrypted data: %v", err)
	}
	if string(plain) != "TelDrive compatibility payload" {
		t.Fatalf("plaintext = %q", plain)
	}
}

func TestCipherRangeDecryptAcrossBlocks(t *testing.T) {
	t.Parallel()

	plain := bytes.Repeat([]byte("0123456789abcdef"), 9000)
	cipher, err := NewCipherWithRand("password", "salt", bytes.NewReader(bytes.Repeat([]byte{7}, fileNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedReader, err := cipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := io.ReadAll(encryptedReader)
	if err != nil {
		t.Fatal(err)
	}

	decryptCipher, err := NewCipher("password", "salt")
	if err != nil {
		t.Fatal(err)
	}
	offset := int64(blockDataSize - 17)
	limit := int64(80)
	rangeReader, err := decryptCipher.DecryptDataSeek(context.Background(), func(_ context.Context, offset, limit int64) (io.ReadCloser, error) {
		end := int64(len(encrypted))
		if limit >= 0 && offset+limit < end {
			end = offset + limit
		}
		return io.NopCloser(bytes.NewReader(encrypted[offset:end])), nil
	}, offset, limit)
	if err != nil {
		t.Fatalf("DecryptDataSeek() error = %v", err)
	}
	decrypted, err := io.ReadAll(rangeReader)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if !bytes.Equal(decrypted, plain[offset:offset+limit]) {
		t.Fatalf("range mismatch: got %d bytes", len(decrypted))
	}
}

func TestCipherRejectsTampering(t *testing.T) {
	t.Parallel()

	cipher, err := NewCipherWithRand("password", "salt", bytes.NewReader(bytes.Repeat([]byte{3}, fileNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedReader, err := cipher.EncryptData(bytes.NewReader([]byte("authenticated payload")))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := io.ReadAll(encryptedReader)
	if err != nil {
		t.Fatal(err)
	}
	encrypted[len(encrypted)-1] ^= 0xff

	decryptCipher, err := NewCipher("password", "salt")
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := decryptCipher.DecryptData(io.NopCloser(bytes.NewReader(encrypted)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(decrypted)
	if !errors.Is(err, ErrorAuthentication) {
		t.Fatalf("tampered ciphertext error = %v, want ErrorAuthentication", err)
	}
}

func TestEncryptedSizeRoundTrip(t *testing.T) {
	t.Parallel()

	for _, size := range []int64{0, 1, blockDataSize - 1, blockDataSize, blockDataSize + 1, 10*blockDataSize + 123} {
		encrypted := EncryptedSize(size)
		decrypted, err := DecryptedSize(encrypted)
		if err != nil {
			t.Fatalf("DecryptedSize(%d): %v", encrypted, err)
		}
		if decrypted != size {
			t.Fatalf("size %d round trip = %d", size, decrypted)
		}
	}
}

func TestNewCipherWithRandRejectsNilSource(t *testing.T) {
	t.Parallel()
	if _, err := NewCipherWithRand("password", "salt", nil); err == nil {
		t.Fatal("expected nil random source error")
	}
}

func TestDecryptSeekAfterEOFAndClose(t *testing.T) {
	t.Parallel()
	plain := bytes.Repeat([]byte("seekable-content-"), 10000)
	cipher, err := NewCipherWithRand("seek-password", "seek-salt", bytes.NewReader(bytes.Repeat([]byte{8}, fileNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedReader, err := cipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if err := encryptedReader.Close(); err != nil {
		t.Fatalf("encrypter Close() error = %v", err)
	}
	encrypted, err := io.ReadAll(encryptedReader)
	if err != nil {
		t.Fatal(err)
	}
	decryptCipher, err := NewCipher("seek-password", "seek-salt")
	if err != nil {
		t.Fatal(err)
	}
	open := func(_ context.Context, offset, limit int64) (io.ReadCloser, error) {
		end := int64(len(encrypted))
		if limit >= 0 && offset+limit < end {
			end = offset + limit
		}
		return io.NopCloser(bytes.NewReader(encrypted[offset:end])), nil
	}
	reader, err := decryptCipher.DecryptDataSeek(context.Background(), open, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("initial ReadAll() error = %v", err)
	}
	position, err := reader.Seek(10, io.SeekStart)
	if err != nil || position != 10 {
		t.Fatalf("Seek() = %d, %v", position, err)
	}
	got := make([]byte, 32)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	if !bytes.Equal(got, plain[10:42]) {
		t.Fatalf("seek content mismatch: %q", got)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := reader.Close(); !errors.Is(err, ErrorFileClosed) {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestDecryptRejectsShortAndBadMagic(t *testing.T) {
	t.Parallel()
	cipher, err := NewCipher("password", "salt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.DecryptData(io.NopCloser(bytes.NewReader([]byte("short")))); !errors.Is(err, ErrorEncryptedFileTooShort) {
		t.Fatalf("short ciphertext error = %v", err)
	}
	badMagic := make([]byte, fileHeaderSize+blockHeaderSize+1)
	if _, err := cipher.DecryptData(io.NopCloser(bytes.NewReader(badMagic))); !errors.Is(err, ErrorEncryptedBadMagic) {
		t.Fatalf("bad magic error = %v", err)
	}
}

func TestDecryptSeekRejectsUnsupportedWhence(t *testing.T) {
	t.Parallel()
	plain := []byte("seek")
	cipher, err := NewCipherWithRand("password", "salt", bytes.NewReader(bytes.Repeat([]byte{2}, fileNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedReader, err := cipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := io.ReadAll(encryptedReader)
	if err != nil {
		t.Fatal(err)
	}
	decryptCipher, _ := NewCipher("password", "salt")
	reader, err := decryptCipher.DecryptDataSeek(context.Background(), func(_ context.Context, offset, limit int64) (io.ReadCloser, error) {
		end := int64(len(encrypted))
		if limit >= 0 && offset+limit < end {
			end = offset + limit
		}
		return io.NopCloser(bytes.NewReader(encrypted[offset:end])), nil
	}, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(0, io.SeekCurrent); err == nil {
		t.Fatal("expected unsupported whence error")
	}
}

// TestDecryptSeekRejectsNegativeOffset pins that a negative offset is reported as
// ErrorBadSeek by both entry points instead of panicking on the negative buffer
// index it used to produce, and that a rejected seek leaves the reader where it
// was rather than poisoning it.
func TestDecryptSeekRejectsNegativeOffset(t *testing.T) {
	t.Parallel()
	plain := []byte("negative-offset")
	cipher, err := NewCipherWithRand("negative-password", "negative-salt", bytes.NewReader(bytes.Repeat([]byte{9}, fileNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedReader, err := cipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := io.ReadAll(encryptedReader)
	if err != nil {
		t.Fatal(err)
	}
	decryptCipher, err := NewCipher("negative-password", "negative-salt")
	if err != nil {
		t.Fatal(err)
	}
	open := func(_ context.Context, offset, limit int64) (io.ReadCloser, error) {
		end := int64(len(encrypted))
		if limit >= 0 && offset+limit < end {
			end = offset + limit
		}
		return io.NopCloser(bytes.NewReader(encrypted[offset:end])), nil
	}
	if _, err := decryptCipher.DecryptDataSeek(context.Background(), open, -1, -1); !errors.Is(err, ErrorBadSeek) {
		t.Fatalf("DecryptDataSeek() with a negative offset error = %v, want ErrorBadSeek", err)
	}
	reader, err := decryptCipher.DecryptDataSeek(context.Background(), open, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(-1, io.SeekStart); !errors.Is(err, ErrorBadSeek) {
		t.Fatalf("Seek() with a negative offset error = %v, want ErrorBadSeek", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() after a rejected seek error = %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("content after a rejected seek = %q, want %q", got, plain)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// closeCountingReader counts Close calls so a test can tell whether the
// decrypter released a range it replaced.
type closeCountingReader struct {
	reader io.Reader
	closes int
}

// Read forwards to the wrapped reader.
func (r *closeCountingReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

// Close records the call and succeeds.
func (r *closeCountingReader) Close() error {
	r.closes++
	return nil
}

func TestDecryptSeekClosesReplacedRanges(t *testing.T) {
	t.Parallel()

	plain := bytes.Repeat([]byte("seekable-content-"), 10000)
	encryptCipher, err := NewCipherWithRand("seek-close-password", "seek-close-salt", bytes.NewReader(bytes.Repeat([]byte{5}, fileNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedReader, err := encryptCipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := io.ReadAll(encryptedReader)
	if err != nil {
		t.Fatal(err)
	}

	decryptCipher, err := NewCipher("seek-close-password", "seek-close-salt")
	if err != nil {
		t.Fatal(err)
	}
	var readers []*closeCountingReader
	open := func(_ context.Context, offset, limit int64) (io.ReadCloser, error) {
		end := int64(len(encrypted))
		if limit >= 0 && offset+limit < end {
			end = offset + limit
		}
		reader := &closeCountingReader{reader: bytes.NewReader(encrypted[offset:end])}
		readers = append(readers, reader)
		return reader, nil
	}

	// Starting inside the content makes the decrypter open the header range
	// first and then replace it with the range holding the target block.
	reader, err := decryptCipher.DecryptDataSeek(context.Background(), open, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(readers) != 2 {
		t.Fatalf("opened %d ranges, want 2", len(readers))
	}
	if readers[0].closes != 1 {
		t.Fatalf("header range closed %d times, want 1", readers[0].closes)
	}

	if _, err := reader.Seek(int64(blockDataSize)+7, io.SeekStart); err != nil {
		t.Fatalf("Seek() error = %v", err)
	}
	if len(readers) != 3 {
		t.Fatalf("opened %d ranges after seek, want 3", len(readers))
	}
	if readers[1].closes != 1 {
		t.Fatalf("replaced range closed %d times, want 1", readers[1].closes)
	}
	if readers[2].closes != 0 {
		t.Fatalf("current range closed %d times before Close, want 0", readers[2].closes)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if readers[2].closes != 1 {
		t.Fatalf("current range closed %d times, want 1", readers[2].closes)
	}
}

func TestEncrypterReturnsPoolBuffersWhenNonceReadFails(t *testing.T) {
	// Not parallel: the assertion inspects the cipher's private pool, and a
	// garbage collection would legitimately clear it, so collection stays off for
	// the critical section. The previous setting is restored when the test ends.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	cipher, err := NewCipher("pool-password", "pool-salt")
	if err != nil {
		t.Fatal(err)
	}
	allocations := 0
	cipher.buffers.New = func() any {
		allocations++
		return new([blockSize]byte)
	}
	cipher.cryptoRand = bytes.NewReader([]byte("short"))

	if _, err := cipher.EncryptData(bytes.NewReader([]byte("payload"))); err == nil {
		t.Fatal("expected nonce read error")
	}
	if allocations != 2 {
		t.Fatalf("buffers allocated during the failed construction = %d, want 2", allocations)
	}

	if raceDetectorEnabled() {
		// The race runtime changes when sync.Pool hands a stored entry back to a
		// later Get, so the two borrows below can miss buffers that were returned
		// moments earlier. The retention itself is covered by a run without -race
		// (just test-unit), which is where this assertion is meaningful.
		t.Log("skipping the pool retention assertion in a -race build")
		return
	}

	cipher.getBlock()
	cipher.getBlock()
	if allocations != 2 {
		t.Fatalf("pool buffers were not returned: allocations = %d, want 2", allocations)
	}
}

// raceDetectorEnabled reports whether this test binary was built with -race, which
// it reads from the build settings the toolchain stamps into the executable.
func raceDetectorEnabled() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, setting := range info.Settings {
		if setting.Key == "-race" {
			return setting.Value == "true"
		}
	}
	return false
}
