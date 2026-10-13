package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCandidateCursorIsBoundToItsQuery pins that a candidate cursor minted
// for one lookup is refused by another on the same generation.
// Mutation caught: a cursor holding only the generation, step and node id,
// which resumes another name's ranking at a foreign position and silently
// skips or repeats its candidates.
func TestCandidateCursorIsBoundToItsQuery(t *testing.T) {
	var nodes []fixtureDecl
	for i := range 4 {
		for _, name := range []string{"handle", "serve"} {
			nodes = append(nodes, fixtureDecl{Key: fmt.Sprintf("%s_%d", name, i), Name: name, Kind: model.NodeFunction,
				File: fmt.Sprintf("pkg%d/m.py", i), Bytes: [2]uint64{0, 30}, Language: "python"})
		}
	}
	f := newCallFixture("candidate_cursor", fixtureScenario{Nodes: nodes})
	r := f.reader()
	lookup := func(name, cursor string) (model.CandidatePage, error) {
		return Candidates(context.Background(), r, model.CandidateRequest{Name: name, Shape: model.CallShapeBare,
			Language: "python", Page: model.PageRequest{Limit: 1, Cursor: cursor}})
	}
	first, err := lookup("handle", "")
	if err != nil || first.Cursor == "" {
		t.Fatalf("first page of handle: %v, cursor %q", err, first.Cursor)
	}
	if _, err := lookup("handle", first.Cursor); err != nil {
		t.Fatalf("the same lookup refused its own cursor: %v", err)
	}
	_, err = lookup("serve", first.Cursor)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
		t.Fatalf("serve resumed with handle's cursor: err %v, want %s", err, model.CodeCursorInvalid)
	}
}
