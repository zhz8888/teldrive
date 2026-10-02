// Package secureblob encrypts small secrets at rest with XChaCha20-Poly1305.
//
// TelDrive stores values such as session tokens, bot credentials and Telegram
// session strings in PostgreSQL. This package seals each one under a single
// 32-byte key taken from configuration, binding the value to the column it
// belongs to through an authenticated-data purpose so a ciphertext moved to a
// different column fails to open rather than silently decrypting.
//
// The key is held in memory only; this package never persists or logs it.
package secureblob

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	// ErrInvalidKey reports a key that is missing, not 32 bytes long, or not
	// valid base64. It is also returned when Seal or Open is called without the
	// state they need, such as an empty purpose.
	ErrInvalidKey = errors.New("secure blob key must decode to 32 bytes")

	// ErrInvalidCiphertext reports a value that cannot be opened: truncated,
	// carrying an unknown format version, or authenticated against a different
	// purpose or key. Callers should treat all of these as "not decryptable" and
	// must not try to distinguish them, because doing so would leak information
	// about the key.
	ErrInvalidCiphertext = errors.New("secure blob ciphertext is invalid")
)

// Cipher encrypts sensitive database fields with XChaCha20-Poly1305. Purpose is
// authenticated as associated data so a token, bot credential, login state, or
// Telegram session cannot be replayed in another column.
type Cipher struct {
	// key is the 32-byte XChaCha20-Poly1305 key. It is copied into a fixed-size
	// array so it cannot be mutated through the slice it was built from.
	key [chacha20poly1305.KeySize]byte

	// random is the nonce source. It is crypto/rand in production and an
	// injectable reader in tests, which is the only reason it is a field.
	random io.Reader
}

// New builds a Cipher from a base64-encoded key, accepting either unpadded
// URL-safe or standard base64 so the same value works from configuration files
// and environment variables alike. A key that does not decode to exactly 32
// bytes is rejected with ErrInvalidKey.
func New(base64Key string) (*Cipher, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(base64Key)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(base64Key)
	}
	if err != nil || len(decoded) != chacha20poly1305.KeySize {
		return nil, ErrInvalidKey
	}
	cipher := &Cipher{random: rand.Reader}
	copy(cipher.key[:], decoded)
	return cipher, nil
}

// NewWithKey builds a Cipher from a raw 32-byte key and an explicit nonce
// source.
//
// It exists so tests can seal and open against deterministic nonces; production
// callers should use New, which always draws from crypto/rand. A key of the
// wrong length or a nil random source is rejected.
func NewWithKey(key []byte, random io.Reader) (*Cipher, error) {
	if len(key) != chacha20poly1305.KeySize {
		return nil, ErrInvalidKey
	}
	if random == nil {
		return nil, errors.New("secure blob random source is required")
	}
	cipher := &Cipher{random: random}
	copy(cipher.key[:], key)
	return cipher, nil
}

// Seal encrypts plaintext under the given purpose and returns
// version || nonce || ciphertext.
//
// The purpose is authenticated but not encrypted: opening the result with a
// different purpose fails, which is what ties a value to its column. The version
// byte allows the envelope format to change later without ambiguity. A fresh
// nonce is drawn for every call; reusing a nonce with the same key would be
// catastrophic for this cipher, so callers must never pass a fixed random
// source outside tests.
//
// An empty purpose or an unconfigured Cipher is rejected with ErrInvalidKey
// rather than producing a value that Open could not attribute.
func (c *Cipher) Seal(purpose string, plaintext []byte) ([]byte, error) {
	if c == nil || c.random == nil || purpose == "" {
		return nil, ErrInvalidKey
	}
	aead, err := chacha20poly1305.NewX(c.key[:])
	if err != nil {
		return nil, fmt.Errorf("create secure blob cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(c.random, nonce); err != nil {
		return nil, fmt.Errorf("read secure blob nonce: %w", err)
	}
	out := make([]byte, 1+len(nonce), 1+len(nonce)+len(plaintext)+aead.Overhead())
	out[0] = 1
	copy(out[1:], nonce)
	return aead.Seal(out, nonce, plaintext, []byte(purpose)), nil
}

// Open decrypts a value produced by Seal with the same purpose.
//
// Every failure mode — a short or truncated value, an unknown version byte, a
// modified byte anywhere in the envelope, or a purpose that does not match the
// one used to seal it — is reported as ErrInvalidCiphertext. The single error
// value is deliberate: distinguishing the cases would tell an attacker whether
// a guessed purpose was right.
func (c *Cipher) Open(purpose string, ciphertext []byte) ([]byte, error) {
	if c == nil || purpose == "" {
		return nil, ErrInvalidKey
	}
	aead, err := chacha20poly1305.NewX(c.key[:])
	if err != nil {
		return nil, fmt.Errorf("create secure blob cipher: %w", err)
	}
	if len(ciphertext) < 1+aead.NonceSize()+aead.Overhead() || ciphertext[0] != 1 {
		return nil, ErrInvalidCiphertext
	}
	nonce := ciphertext[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, ciphertext[1+aead.NonceSize():], []byte(purpose))
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

// Encrypt implements RiverPro's job argument encryptor using a dedicated
// authenticated-data purpose. RiverPro's Encryptor contract cannot return an
// error, so encryption failures follow its reference implementation and panic.
func (c *Cipher) Encrypt(plaintext []byte) []byte {
	ciphertext, err := c.Seal("river-job-args", plaintext)
	if err != nil {
		panic(fmt.Sprintf("encrypt River job arguments: %v", err))
	}
	return ciphertext
}

// Decrypt implements RiverPro's job argument decryptor.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return c.Open("river-job-args", ciphertext)
}
