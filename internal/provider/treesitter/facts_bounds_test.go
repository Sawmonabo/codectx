package treesitter

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/source"
)

// TestOverlongDeclarationNamesAreTruncatedNotRefused pins the ruling that a
// storage ceiling on a field truncates and flags rather than failing the unit.
// Generated code clears MaxQualifiedNameBytes routinely, and the old guard
// turned one such declaration into CTX_PROVIDER_OUTPUT_INVALID, which publishes
// nothing for the whole file. Restoring the length arms of that guard makes
// this fail, which is the mutation that proves it.
func TestOverlongDeclarationNamesAreTruncatedNotRefused(t *testing.T) {
	src := []byte("func f() {}\n")
	qualified := strings.Repeat("a", 3000)
	b := &builder{
		fv:     model.FileVersion{Path: "gen/api.go"},
		src:    src,
		cur:    source.NewCursor(src),
		byName: map[string][]int{},
		ex: &extraction{decls: []wire.Decl{{
			ID: 0, Parent: -1, Kind: "function", Name: "f", Qualified: qualified,
			Start: 0, End: uint32(len(src)), SigEnd: 9,
		}}},
	}
	if err := b.validateDecls(); err != nil {
		t.Fatalf("an over-long qualified name must not fail the unit: %v", err)
	}
	d := b.decls[0]
	if len(d.Qualified) > model.MaxQualifiedNameBytes {
		t.Fatalf("qualified name stored at %d bytes, ceiling %d", len(d.Qualified), model.MaxQualifiedNameBytes)
	}
	if got := d.truncated["qualified_name"]; got != len(qualified) {
		t.Fatalf("truncated_fields records original length %d, want %d", got, len(qualified))
	}
	// The cut value is a prefix, not an identity: the native key falls back to
	// the file-local declaration key so two generated symbols sharing a
	// 2048-byte prefix do not collide into one node.
	if key := b.identityKey(&d); key == d.Qualified || !strings.HasPrefix(key, "decl:f@gen/api.go:") {
		t.Fatalf("identity key %q is the truncated qualified name", key)
	}
}
