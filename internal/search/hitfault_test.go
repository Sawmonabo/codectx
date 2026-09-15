package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// TestHydrateMissingBlobFlagsOneHit proves the per-hit fault contract: when the
// content store no longer holds ONE hit's blob, that hit comes back flagged
// with a nil Range and every other hit on the page still carries its real
// resolved range. Failing the whole page for one unreadable blob -- which is
// what hydratePage did before blobFault -- costs the caller every good hit in
// the answer.
func TestHydrateMissingBlobFlagsOneHit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cas, err := snapshot.OpenCAS(dir)
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}

	goodBody := "package a\n\nfunc Good() int { return 1 }\n"
	goodRec, err := cas.Put(ctx, strings.NewReader(goodBody))
	if err != nil {
		t.Fatalf("Put(good): %v", err)
	}
	goneBody := "package b\n\nfunc Gone() int { return 2 }\n"
	goneRec, err := cas.Put(ctx, strings.NewReader(goneBody))
	if err != nil {
		t.Fatalf("Put(gone): %v", err)
	}
	// The blob row survives -- the index still records it -- but the bytes are
	// gone from the store, which is exactly the fault a half-pruned or
	// externally-damaged data directory presents.
	if err := os.Remove(filepath.Join(dir, goneRec.Hash[:2], goneRec.Hash)); err != nil {
		t.Fatalf("remove the blob: %v", err)
	}

	goodFile := model.NewFileID("repo", "pkg/good.go")
	goneFile := model.NewFileID("repo", "pkg/gone.go")
	h := newHydrator(
		stubFiles{
			goodFile: {ID: goodFile, Path: "pkg/good.go", ContentHash: goodRec.Hash, Size: goodRec.Size},
			goneFile: {ID: goneFile, Path: "pkg/gone.go", ContentHash: goneRec.Hash, Size: goneRec.Size},
		},
		stubBlobs{goodRec.Hash: goodRec, goneRec.Hash: goneRec},
		cas)

	// The unreadable hit sits BETWEEN two readable ones, so a fault that
	// aborted the walk would be visible as a hit that never got hydrated.
	target := "func Good() int { return 1 }"
	start := uint64(strings.Index(goodBody, target))
	goodSpan := model.ByteRange{Start: start, End: start + uint64(len(target))}
	hits := []model.SearchHit{
		{FileID: goodFile, Path: "pkg/good.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS},
		{FileID: goneFile, Path: "pkg/gone.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS},
		{FileID: goodFile, Path: "pkg/good.go", Kind: model.NodeFile, Tier: model.TierLexicalFTS},
	}
	spans := []model.ByteRange{goodSpan, {Start: 0, End: uint64(len(goneBody))}, {Start: 0, End: 0}}
	if err := h.hydratePage(ctx, hits, spans); err != nil {
		t.Fatalf("hydratePage: %v: one unreadable blob must not fail the whole answer", err)
	}

	// The affected hit: present, flagged, no range.
	faulted := hits[1]
	if faulted.Range != nil {
		t.Errorf("the unreadable hit carries Range %+v, want nil", faulted.Range)
	}
	reason, ok := faulted.UnresolvedFields[model.SearchHitFieldRange]
	if !ok {
		t.Fatalf("the unreadable hit carries %v, want a %q entry", faulted.UnresolvedFields, model.SearchHitFieldRange)
	}
	if !strings.HasPrefix(reason, model.CodeSourceIntegrity+": ") {
		t.Errorf("the flag reads %q, want it to name the %s fault", reason, model.CodeSourceIntegrity)
	}
	if !strings.Contains(reason, "missing from the content-addressed store") {
		t.Errorf("the flag reads %q, want it to say the blob is missing", reason)
	}
	if err := faulted.Validate(); err != nil {
		t.Errorf("a flagged hit does not validate: %v", err)
	}

	// The other hits: intact, with correct ranges and no flag.
	want := model.SourceRange{
		Start: model.Position{Byte: goodSpan.Start, Line: 3, Column: 0},
		End:   model.Position{Byte: goodSpan.End, Line: 3, Column: uint32(len(target))},
	}
	if hits[0].Range == nil || *hits[0].Range != want {
		t.Errorf("the readable hit resolved to %+v, want %+v", hits[0].Range, want)
	}
	if hits[2].Range == nil || hits[2].Range.Start.Line != 1 {
		t.Errorf("the second readable hit resolved to %+v, want line 1", hits[2].Range)
	}
	for _, i := range []int{0, 2} {
		if hits[i].UnresolvedFields != nil {
			t.Errorf("hit %d is flagged %v, but its blob is readable", i, hits[i].UnresolvedFields)
		}
	}
}

// TestHydrateFaultClassification pins the narrow classification: a cancelled
// context and a corrupt index still fail the whole answer. Only an integrity
// fault of the content store becomes a per-hit flag.
func TestHydrateFaultClassification(t *testing.T) {
	ctx := context.Background()
	cas, err := snapshot.OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	rec, err := cas.Put(ctx, strings.NewReader("package a\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	file := model.NewFileID("repo", "pkg/a.go")
	h := newHydrator(
		stubFiles{file: {ID: file, Path: "pkg/a.go", ContentHash: rec.Hash, Size: rec.Size}},
		stubBlobs{rec.Hash: rec},
		cas)

	// A span past the end of the file is a corrupt index, not a clamped range.
	hits := []model.SearchHit{{FileID: file, Path: "pkg/a.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS}}
	err = h.hydratePage(ctx, hits, []model.ByteRange{{Start: 0, End: uint64(rec.Size) + 1}})
	assertCode(t, "a document past the end of its file", err, model.CodeArgumentInvalid)
	if hits[0].UnresolvedFields != nil {
		t.Errorf("a corrupt index was flagged per hit as %v, want a failed answer", hits[0].UnresolvedFields)
	}

	// Cancellation is never a per-hit footnote on an otherwise complete answer.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	hits = []model.SearchHit{{FileID: file, Path: "pkg/a.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS}}
	err = h.hydratePage(canceled, hits, []model.ByteRange{{Start: 0, End: 0}})
	assertCode(t, "a cancelled hydration", err, model.CodeCanceled)
	if hits[0].UnresolvedFields != nil {
		t.Errorf("cancellation was flagged per hit as %v, want a failed answer", hits[0].UnresolvedFields)
	}
}
