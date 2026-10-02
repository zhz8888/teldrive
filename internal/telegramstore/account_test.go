package telegramstore

import (
	"bytes"
	"errors"
	"testing"
)

func TestBoundedWriterRejectsBytesOverBudget(t *testing.T) {
	var buffer bytes.Buffer
	writer := &boundedWriter{writer: &buffer, remaining: 4}
	if _, err := writer.Write([]byte("12345")); !errors.Is(err, ErrProfilePhotoTooLarge) {
		t.Fatalf("Write() error = %v, want ErrProfilePhotoTooLarge", err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("buffer length = %d, want nothing written", buffer.Len())
	}
	if n, err := writer.Write([]byte("1234")); err != nil || n != 4 {
		t.Fatalf("Write() = (%d, %v), want (4, nil)", n, err)
	}
	if buffer.String() != "1234" {
		t.Fatalf("buffer = %q, want %q", buffer.String(), "1234")
	}
}
