package jobs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// TestJobUserIDHidesJobsWithoutAUsableOwner is the filter every tenant-scoped
// listing depends on: a job whose arguments carry no positive integer user_id
// belongs to no single user and must stay invisible to them.
func TestJobUserIDHidesJobsWithoutAUsableOwner(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		args map[string]json.RawMessage
		want int64
		ok   bool
	}{
		{name: "positive", args: map[string]json.RawMessage{"user_id": json.RawMessage(`1001`)}, want: 1001, ok: true},
		{name: "absent", args: map[string]json.RawMessage{}},
		{name: "nil map", args: nil},
		{name: "zero", args: map[string]json.RawMessage{"user_id": json.RawMessage(`0`)}},
		{name: "negative", args: map[string]json.RawMessage{"user_id": json.RawMessage(`-5`)}},
		{name: "not a number", args: map[string]json.RawMessage{"user_id": json.RawMessage(`"1001"`)}},
		{name: "object", args: map[string]json.RawMessage{"user_id": json.RawMessage(`{"id":1001}`)}},
		{name: "malformed", args: map[string]json.RawMessage{"user_id": json.RawMessage(`{`)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Job{Args: testCase.args}.UserID()
			if ok != testCase.ok || got != testCase.want {
				t.Fatalf("UserID() = %d, %t, want %d, %t", got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

// TestDecodeCursorRejectsAnythingEncodeCursorWouldNotProduce pins the paging
// contract: a blank cursor starts at the newest job, a real one round-trips, and
// anything a client could have tampered with is reported as a client error rather
// than being silently ignored.
func TestDecodeCursorRejectsAnythingEncodeCursorWouldNotProduce(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{"", "   ", "\t\n"} {
		id, err := decodeCursor(blank)
		if err != nil || id != 0 {
			t.Fatalf("decodeCursor(%q) = %d, %v, want 0, nil", blank, id, err)
		}
	}
	for _, id := range []int64{1, 42, 9007199254740993} {
		cursor := encodeCursor(id)
		// The cursor travels in a URL. RawURLEncoding rejects the standard-base64
		// alphabet, so a cursor that decodes cleanly is already safe to embed in a
		// query parameter without escaping.
		if _, err := base64.RawURLEncoding.DecodeString(cursor); err != nil {
			t.Fatalf("encodeCursor(%d) = %q, which is not URL-safe base64: %v", id, cursor, err)
		}
		got, err := decodeCursor(cursor)
		if err != nil || got != id {
			t.Fatalf("decodeCursor(%q) = %d, %v, want %d, nil", cursor, got, err, id)
		}
	}
	for _, bad := range []string{
		"not base64!!",
		base64.RawURLEncoding.EncodeToString([]byte("abc")), // not an integer
		base64.RawURLEncoding.EncodeToString([]byte("0")),   // not a real job id
		base64.RawURLEncoding.EncodeToString([]byte("-3")),  // negative
	} {
		if _, err := decodeCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("decodeCursor(%q) error = %v, want ErrInvalidCursor", bad, err)
		}
	}
}
