package secureblob

import (
	"bytes"
	"testing"
)

// TestRiverJobArgumentsRoundTrip covers the encryptor RiverPro uses for job
// arguments. Job payloads are stored in the same table as everything else, so a
// bot token carried in an argument must be sealed and only openable under the same
// purpose label.
func TestRiverJobArgumentsRoundTrip(t *testing.T) {
	t.Parallel()
	cipher, err := NewWithKey(bytes.Repeat([]byte{3}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"token":"777:secret"}`)
	sealed := cipher.Encrypt(plaintext)
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("Encrypt() left the plaintext visible in the ciphertext")
	}
	opened, err := cipher.Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("Decrypt() = %s, want %s", opened, plaintext)
	}
}

// TestRiverJobArgumentsAreBoundToTheirPurpose keeps one ciphertext from being
// opened under another label, so a blob sealed for a bot token cannot be replayed
// as a job argument or vice versa.
func TestRiverJobArgumentsAreBoundToTheirPurpose(t *testing.T) {
	t.Parallel()
	cipher, err := NewWithKey(bytes.Repeat([]byte{3}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipher.Seal("bot-token", []byte("777:secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.Decrypt(sealed); err == nil {
		t.Fatal("Decrypt() accepted a blob sealed under another purpose, want a rejection")
	}
}
