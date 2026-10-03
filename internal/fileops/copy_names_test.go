package fileops

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestSplitCopyNameKeepsTheRealExtension pins where a duplicate name is split.
// The dot has to be the last one, and a leading or trailing dot is not an
// extension: a file called ".gitignore" must not become ".gitignore" plus an empty
// extension, or the generated "name (1)" would lose its suffix entirely.
func TestSplitCopyNameKeepsTheRealExtension(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		input     string
		stem      string
		extension string
	}{
		{name: "normal file", input: "report.pdf", stem: "report", extension: ".pdf"},
		{name: "two dots", input: "archive.tar.gz", stem: "archive.tar", extension: ".gz"},
		{name: "no extension", input: "README", stem: "README"},
		{name: "leading dot", input: ".gitignore", stem: ".gitignore"},
		{name: "trailing dot", input: "weird.", stem: "weird."},
		{name: "dotfile with inner dot", input: ".config.json", stem: ".config", extension: ".json"},
		{name: "empty", input: "", stem: ""},
		{name: "only a dot", input: ".", stem: "."},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			stem, extension := splitCopyName(testCase.input)
			if stem != testCase.stem || extension != testCase.extension {
				t.Fatalf("splitCopyName(%q) = %q, %q, want %q, %q",
					testCase.input, stem, extension, testCase.stem, testCase.extension)
			}
		})
	}
}

// TestOptionalInt32KeepsNullDistinctFromZero is what lets a caller tell "this
// column was not set" from "this column is zero". Conflating the two would send a
// zero page size where the caller meant "use the default".
func TestOptionalInt32KeepsNullDistinctFromZero(t *testing.T) {
	t.Parallel()
	if got := optionalInt32(pgtype.Int4{}); got != nil {
		t.Fatalf("optionalInt32(NULL) = %d, want nil", *got)
	}
	zero := optionalInt32(pgtype.Int4{Int32: 0, Valid: true})
	if zero == nil || *zero != 0 {
		t.Fatalf("optionalInt32(zero) = %v, want a pointer to 0", zero)
	}
	value := optionalInt32(pgtype.Int4{Int32: 42, Valid: true})
	if value == nil || *value != 42 {
		t.Fatalf("optionalInt32(42) = %v, want a pointer to 42", value)
	}
	// A negative value is still a value: the caller's own validation rejects it,
	// and silently dropping it here would hide the bad input.
	negative := optionalInt32(pgtype.Int4{Int32: -1, Valid: true})
	if negative == nil || *negative != -1 {
		t.Fatalf("optionalInt32(-1) = %v, want a pointer to -1", negative)
	}
}
