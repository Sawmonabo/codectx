package search

import (
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCandidateCodecRoundTripsEveryField is what pays for a packed codec being
// able to drift from the struct: it fills EVERY field of scored with a
// distinct value, round-trips it, and compares deeply. A field added to scored
// (or to model.SearchHit) and forgotten in encodeScored fails the reflective
// completeness check below before it can silently blank a served hit.
func TestCandidateCodecRoundTripsEveryField(t *testing.T) {
	full := scored{
		ranked: ranked{
			Tier: model.TierLexicalFTS, ScoreMicros: -4321, Path: "pkg/a/b.go", StartByte: 1 << 40,
			NodeID: "n-1", SearchKey: "sk-1", RowID: 987654321, Occurrences: 7,
		},
		Reasons: []string{"matched the body text", "matched the name"},
		Hit: model.SearchHit{
			NodeID: "n-1", FileID: "f-1", Path: "pkg/a/b.go", Kind: "function", Name: "Do",
			QualifiedName: "pkg/a.Do", Signature: "func Do(ctx context.Context) error",
			Tier: model.TierLexicalFTS, ScoreMicros: -4321,
			Range: &model.SourceRange{
				Start: model.Position{Byte: 10, Line: 2, Column: 3},
				End:   model.Position{Byte: 4_000_000_000, Line: 99, Column: 1},
			},
			OccurrenceCount:  7,
			Reasons:          []string{"matched the body text"},
			UnresolvedFields: map[string]string{model.SearchHitFieldRange: "the blob is gone", "other": "x"},
		},
		Span:   &model.ByteRange{Start: 10, End: 4_000_000_000},
		Folded: 9_000_000,
	}
	assertNoZeroField(t, "scored", reflect.ValueOf(full))
	assertNoZeroField(t, "scored.Hit", reflect.ValueOf(full.Hit))

	for _, tc := range []struct {
		name string
		in   scored
	}{
		{"every field set", full},
		// The zero candidate: nil slices, nil maps and nil pointers must come
		// back nil, not as empty-but-non-nil values.
		{"zero", scored{}},
		// A pointer to the zero range is NOT the same as no range.
		{"zero range present", scored{Hit: model.SearchHit{Range: &model.SourceRange{}}, Span: &model.ByteRange{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := encodeScored(tc.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := decodeScored(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.in) {
				t.Fatalf("round trip changed the candidate:\n got %+v\nwant %+v", got, tc.in)
			}
		})
	}

	// A truncated record is reported as a corrupt run, never as a short hit.
	b, err := encodeScored(full)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeScored(b[:len(b)-1]); err == nil {
		t.Fatal("a truncated record decoded without an error")
	}
	if _, err := decodeScored(append(b, 0)); err == nil {
		t.Fatal("a record with trailing bytes decoded without an error")
	}
}

// assertNoZeroField fails when any exported field of v is still its zero
// value, which is how the fixture above stays exhaustive as the struct grows.
func assertNoZeroField(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.Anonymous {
			assertNoZeroField(t, path+"."+f.Name, v.Field(i))
			continue
		}
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Fatalf("%s.%s is zero in the codec fixture: fill it, or the round trip does not test it", path, f.Name)
		}
	}
}
