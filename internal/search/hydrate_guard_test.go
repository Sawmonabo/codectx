package search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// TestHydrateRefusesAnInvertedSpan pins the guard at the top of hydrate. A span
// whose Start is past its End but whose End is inside the file used to reach
// positionAt, where the window loop clamps every read at rec.Size while w.At()
// never reaches Start: an empty window that passes the length check and
// advances nothing, forever. A typed CTX_ARGUMENT_INVALID is what this owes and
// what the pre-streaming code returned. The watchdog is the point of the test --
// dropping `span.Start > span.End` from the guard hangs it rather than failing
// an assertion.
func TestHydrateRefusesAnInvertedSpan(t *testing.T) {
	ctx := context.Background()
	cas, err := snapshot.OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	body := "package a\n\nfunc A() {}\n"
	rec, err := cas.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	file := model.NewFileID("repo", "a.go")
	h := newHydrator(
		stubFiles{file: {ID: file, Path: "a.go", ContentHash: rec.Hash, Size: rec.Size}},
		stubBlobs{rec.Hash: rec}, cas)

	done := make(chan error, 1)
	go func() {
		_, err := h.hydrate(ctx, file, model.ByteRange{Start: uint64(rec.Size) + 100, End: 5})
		done <- err
	}()
	select {
	case err := <-done:
		var typed *model.Error
		if !errors.As(err, &typed) {
			t.Fatalf("hydrate returned %v, want a typed refusal", err)
		}
		if typed.Code != model.CodeArgumentInvalid {
			t.Fatalf("hydrate returned %s, want %s", typed.Code, model.CodeArgumentInvalid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hydrate did not return within 5s: an inverted span spins in the window loop")
	}
}

// shortTailCAS serves every read faithfully except the one-byte lookahead the
// walk takes when it lands exactly on the offset, which it truncates to
// nothing -- the shape a store that no longer holds the blob it recorded
// presents on that path.
type shortTailCAS struct{ cas *snapshot.CAS }

func (s shortTailCAS) ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error) {
	if r.End-r.Start == 1 {
		return nil, nil
	}
	return s.cas.ReadRange(ctx, rec, r)
}

// TestHydrateFaultsAShortTailRead pins the length check on the COMMON path: the
// lookahead read is taken whenever the walk landed exactly on the offset, which
// is every hit at a checkpointed line start. Unchecked, a short read handed
// PositionAt an empty lookahead and the UTF-8 continuation-byte rejection was
// silently skipped instead of the store being faulted.
func TestHydrateFaultsAShortTailRead(t *testing.T) {
	ctx := context.Background()
	cas, err := snapshot.OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	body := "package a\n\nfunc A() {}\n"
	rec, err := cas.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	file := model.NewFileID("repo", "a.go")
	h := newHydrator(
		stubFiles{file: {ID: file, Path: "a.go", ContentHash: rec.Hash, Size: rec.Size}},
		stubBlobs{rec.Hash: rec}, shortTailCAS{cas: cas})

	// Byte 0 is a line start, so the walk lands on the offset with no window
	// read at all and the lookahead is the only read taken.
	_, err = h.hydrate(ctx, file, model.ByteRange{Start: 0, End: 0})
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Fatalf("hydrate returned %v, want a typed integrity fault", err)
	}
	if typed.Code != model.CodeSourceIntegrity {
		t.Fatalf("hydrate returned %s, want %s", typed.Code, model.CodeSourceIntegrity)
	}
}
