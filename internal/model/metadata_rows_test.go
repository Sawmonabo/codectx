package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestSplitMetadataMovesOnlyWhatDoesNotFit pins the hybrid: a metadata object
// past MaxMetadataBytes moves its largest field to rows and keeps the rest
// inline, and the rows rebuild that field byte for byte, including a long
// scalar (one value row) and a list of equal elements (distinct ordinals).
// Mutation caught: moving every field (a small field lost from the inline
// object), moving the smallest first (the big list stays and the object never
// fits), or rows that collapse equal elements.
func TestSplitMetadataMovesOnlyWhatDoesNotFit(t *testing.T) {
	same := make([]string, 700)
	for i := range same {
		same[i] = "./plugins/"
	}
	long := strings.Repeat("x", MaxMetadataBytes)
	meta, rows, err := SplitMetadata(map[string]any{"exported": true, "fragments": same, "receiver": long})
	if err != nil {
		t.Fatal(err)
	}
	var inline map[string]json.RawMessage
	if err := json.Unmarshal(meta, &inline); err != nil {
		t.Fatalf("metadata %s: %v", meta, err)
	}
	if len(meta) > MaxMetadataBytes || string(inline["exported"]) != "true" {
		t.Fatalf("metadata is %d bytes with exported=%s, want it within the bound and the small field inline", len(meta), inline["exported"])
	}
	byField := map[string][]MetadataRow{}
	for _, r := range rows {
		if err := r.Validate(); err != nil {
			t.Fatalf("row %+v: %v", r, err)
		}
		details, err := r.Details()
		if err != nil || len(details) != 1 {
			t.Fatalf("row %+v renders %d details (%v), want one", r, len(details), err)
		}
		detail := details[0]
		back, ok := ParseMetadataRow(detail)
		if !ok || back.Ordinal != r.Ordinal || string(back.Value) != string(r.Value) {
			t.Fatalf("row %+v does not survive its detail %s", r, detail)
		}
		byField[r.Field] = append(byField[r.Field], back)
	}
	for field, want := range map[string]any{"receiver": long, "fragments": same} {
		if string(inline[MetadataRowsKey(field)]) == "" {
			t.Fatalf("%s is not marked as rows in %s; neither it nor the other fits the bound inline", field, meta)
		}
		got, err := JoinMetadataRows(byField[field])
		if err != nil {
			t.Fatalf("join %s: %v", field, err)
		}
		w, _ := json.Marshal(want)
		if string(got) != string(w) {
			t.Errorf("%s rows rebuild %d bytes, want %d", field, len(got), len(w))
		}
	}
	if n := len(byField["fragments"]); n != len(same) {
		t.Errorf("fragments stored as %d rows, want one per element (%d), equal elements included", n, len(same))
	}
}

// TestJoinMetadataRowsRefusesAGap pins that a list missing an element is an
// error, never a shorter list.
// Mutation caught: joining whatever rows arrived, so a lost row silently
// shortens a fragment list and widens what an open import may match.
func TestJoinMetadataRowsRefusesAGap(t *testing.T) {
	rows := []MetadataRow{
		{Field: "fragments", Ordinal: 0, Shape: RowElement, Value: json.RawMessage(`"a/"`)},
		{Field: "fragments", Ordinal: 2, Shape: RowElement, Value: json.RawMessage(`".h"`)},
	}
	if _, err := JoinMetadataRows(rows); err == nil {
		t.Fatal("rows 0 and 2 joined; want an error for the missing row 1")
	}
}

// TestALongElementIsStoredAsPiecesAndRejoinedWhole protects an element whose
// encoded JSON passes MaxMetadataPieceBytes -- a string literal of megabytes
// used as an import fragment -- which one row would carry past the storage
// batch. It is stored as piece rows, each of bounded width, and rejoins into
// exactly the element; a missing piece is an error, never a shorter element.
//
// Mutations it catches: one detail for the whole element, a piece cut inside
// a rune (its string re-encodes the bytes as U+FFFD and the element changes),
// pieces rejoined out of order, or a gap rejoined silently.
func TestALongElementIsStoredAsPiecesAndRejoinedWhole(t *testing.T) {
	long := strings.Repeat(`é"\x`, 50_000)
	value, err := EncodeMetadataValue([]string{"head", long, "tail"})
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := SplitMetadata(map[string]any{"fragments": []string{"head", long, "tail"}})
	if err != nil {
		t.Fatal(err)
	}
	var stored []MetadataRow
	pieces := 0
	for _, r := range rows {
		details, err := r.Details()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range details {
			if len(d) > 2*MaxMetadataPieceBytes+512 {
				t.Fatalf("a piece detail is %d bytes, past twice the piece bound", len(d))
			}
			back, ok := ParseMetadataRow(d)
			if !ok {
				t.Fatalf("detail %.80s... is not a metadata row", d)
			}
			stored = append(stored, back)
		}
		if len(details) > 1 {
			pieces += len(details)
		}
	}
	if pieces < 2 {
		t.Fatalf("the %d-byte element is stored as one row, want pieces", len(long))
	}
	slices.Reverse(stored)
	got, err := JoinMetadataRows(stored)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if string(got) != string(value) {
		t.Fatalf("the pieces rejoin into %d bytes, want the %d-byte value exactly", len(got), len(value))
	}
	for i, r := range stored {
		if r.Parts != 0 && r.Part == 1 {
			gap := slices.Delete(slices.Clone(stored), i, i+1)
			if _, err := JoinMetadataRows(gap); err == nil {
				t.Fatal("an element with a missing piece rejoined without an error")
			}
			break
		}
	}
}
