package graph

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// writerlessLeases is the lease store of a process that opened the workspace
// read-only: it declares itself through pagination.LeaseRetainer and FAILS
// every write, exactly as the storage layer does when there is no writer
// connection to take one on.
type writerlessLeases struct{ *fixtureLeases }

func (writerlessLeases) RetainsLeases() bool { return false }

func (writerlessLeases) AcquireLease(context.Context, model.Lease, string) error {
	return &model.Error{Code: model.CodeInternal,
		Message: "this process opened the store read-only; a command that changes the workspace must be composed with the writer"}
}

// TestAWriterlessGraphAnswerRetainsNothing is the disk-leak and refusal proof
// for the four graph continuation sites.
//
// Two failure modes, one test. FIRST: a process that writes nothing asks for a
// cursor lease anyway, the lease write fails, and a graph walk that would have
// answered its first page perfectly well fails outright for the whole length of
// another process's index -- the contention defect. SECOND, and worse: the
// lease is skipped but the spool or state directory is still adopted. A spool's
// reclamation predicate IS its cursor lease (the spool store gates every
// adoption on that lease's expiry), so a directory adopted without one has
// nothing that will ever reclaim it: a disk leak. The answer must therefore be
// served, carry no continuation, SAY that it is truncated, and leave the spool
// root exactly as it found it.
//
// Mutation: make RetainsLeases report true. Both legs then attempt the lease,
// AcquireLease refuses, and the pages fail instead of answering.
func TestAWriterlessGraphAnswerRetainsNothing(t *testing.T) {
	f := newGraphFixture(t)
	widenReferences(t, f, "n-b", 6)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := writerlessLeases{fixtureLeases: newFixtureLeases()}
	spoolRoot := t.TempDir()
	spools, err := pagination.NewSpools(spoolRoot, 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	// One item per page, so every answer below runs past its page and would
	// mint a continuation in a writer-bearing process.
	limits.MaxPageItems = 1
	nodes := make([]model.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	e, err := New(Options{Adjacency: f, Reader: NewMemoryGraph(f.binding, nodes,
		append([]model.Relation(nil), f.relations...)), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	before := spoolEntries(t, spoolRoot)

	walk, err := e.Neighbors(context.Background(), model.GraphRequest{
		GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("a writerless traversal failed instead of answering: %v", err)
	}
	if len(walk.Relations) == 0 {
		t.Fatalf("a writerless traversal served no relation")
	}
	if walk.Meta.NextCursor != "" {
		t.Fatalf("a writerless traversal minted a continuation it cannot retain")
	}
	if !walk.Meta.Truncated {
		t.Fatalf("a writerless traversal served a prefix as the complete answer")
	}

	refs, err := e.References(context.Background(), model.ReferenceRequest{
		NodeID: fixtureNodeID("n-b"), Operation: model.ReferenceReferences,
		SemanticSource: model.SemanticCanonical})
	if err != nil {
		t.Fatalf("a writerless reference query failed instead of answering: %v", err)
	}
	if len(refs.Items) == 0 {
		t.Fatalf("a writerless reference query served no occurrence")
	}
	if refs.Meta.NextCursor != "" {
		t.Fatalf("a writerless reference query minted a continuation it cannot retain")
	}
	if !refs.Meta.Truncated {
		t.Fatalf("a writerless reference query served a prefix as the complete answer")
	}

	if n := store.liveCount(); n != 0 {
		t.Fatalf("a writerless answer holds %d retention leases", n)
	}
	if after := spoolEntries(t, spoolRoot); len(after) != len(before) {
		t.Fatalf("a writerless answer adopted %d spools with no lease to reclaim them: %v",
			len(after)-len(before), after)
	}
}

// spoolEntries is every spool and adopted state directory the spool store
// holds, found the same way its own sweep finds them: a retained entry named
// with the store's prefix, directly under the root. A directory adopted with no
// lease is invisible to a check on the token and visible only here.
func spoolEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the spool root: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "spool-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
