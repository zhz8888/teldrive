// Package dbtypes converts between plain Go values and the nullable pgx types
// used by the generated query layer.
//
// The generated code expresses SQL NULL as the Valid flag of a pgtype value.
// These helpers keep that translation in one place so call sites can say
// "this value is always present" or "this value may be NULL" without repeating
// the struct literals.
package dbtypes

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// UUID converts v into a non-NULL pgtype.UUID.
func UUID(v uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: v, Valid: true}
}

// OptionalUUID converts v into a pgtype.UUID, mapping a nil pointer to SQL NULL
// (Valid == false).
func OptionalUUID(v *uuid.UUID) pgtype.UUID {
	if v == nil {
		return pgtype.UUID{}
	}
	return UUID(*v)
}

// Time converts v into a non-NULL pgtype.Timestamptz. The instant is preserved;
// PostgreSQL renders it in the session time zone when it is read back.
func Time(v time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: v, Valid: true}
}

// OptionalTime converts v into a pgtype.Timestamptz, mapping a nil pointer to
// SQL NULL.
func OptionalTime(v *time.Time) pgtype.Timestamptz {
	if v == nil {
		return pgtype.Timestamptz{}
	}
	return Time(*v)
}

// Text converts v into a non-NULL pgtype.Text. The empty string is a stored
// value, not NULL; use OptionalText when "absent" must be distinguishable.
func Text(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: true}
}

// OptionalText converts v into a pgtype.Text, mapping a nil pointer to SQL NULL.
// A pointer to an empty string still produces a non-NULL empty string.
func OptionalText(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return Text(*v)
}

// Int4 converts v into a non-NULL pgtype.Int4.
func Int4(v int32) pgtype.Int4 {
	return pgtype.Int4{Int32: v, Valid: true}
}

// OptionalInt4 converts v into a pgtype.Int4, mapping a nil pointer to SQL NULL.
func OptionalInt4(v *int32) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return Int4(*v)
}

// Int8 converts v into a non-NULL pgtype.Int8.
func Int8(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: true}
}

// OptionalInt8 converts v into a pgtype.Int8, mapping a nil pointer to SQL NULL.
func OptionalInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return Int8(*v)
}

// GoogleUUID converts a pgtype.UUID back into a uuid.UUID. The boolean result is
// false when the column held NULL, in which case the zero UUID is returned.
func GoogleUUID(v pgtype.UUID) (uuid.UUID, bool) {
	if !v.Valid {
		return uuid.Nil, false
	}
	return uuid.UUID(v.Bytes), true
}
