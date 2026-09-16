package graph

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

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

// TestAWriterlessGraphAnswerPagesAndIsReclaimable is the paging and disk-leak
// proof for the graph continuation sites.
//
// Two failure modes, one test. FIRST: a process that writes nothing serves one
// page and mints no token, so a person walking the graph while another process
// indexes sees a prefix of the answer with no way to reach the rest -- and a
// process that instead asks for a cursor lease anyway fails the page outright
// on the lease write. SECOND, and worse: the token is minted and the spool or
// state directory it names has nothing that will ever reclaim it -- a disk
// leak in the one directory resources.max_temp_bytes bounds. What the answer
// must do is page, hold no lease, and leave behind only entries a sweep in ANY
// process reclaims on their own recorded expiry.
//
// Mutation: restore the retention test in firstReferencePage's guard
// (`|| !e.leases.Retains()`) and the reference leg stops after one page.
// Second mutation: restore `|| e.leases == nil` in resumedReferencePage's
// guard and the engine with no lease store refuses its own token.
func TestAWriterlessGraphAnswerPagesAndIsReclaimable(t *testing.T) {
	ctx := context.Background()
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
	// One item per page, so every answer below runs past its page and must
	// mint a continuation.
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

	walkReq := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	walk, err := e.Neighbors(ctx, walkReq)
	if err != nil {
		t.Fatalf("a writerless traversal failed instead of answering: %v", err)
	}
	if len(walk.Relations) == 0 {
		t.Fatalf("a writerless traversal served no relation")
	}
	if walk.Meta.NextCursor == "" {
		t.Fatalf("a writerless traversal stopped after one page of a longer walk")
	}
	walkReq.Page, walkReq.GenerationID = model.PageRequest{Cursor: walk.Meta.NextCursor}, 0
	if _, err := e.Neighbors(ctx, walkReq); err != nil {
		t.Fatalf("a writerless traversal continuation failed instead of serving page two: %v", err)
	}

	refReq := model.ReferenceRequest{NodeID: fixtureNodeID("n-b"),
		Operation: model.ReferenceReferences, SemanticSource: model.SemanticCanonical}
	refs, err := e.References(ctx, refReq)
	if err != nil {
		t.Fatalf("a writerless reference query failed instead of answering: %v", err)
	}
	if len(refs.Items) == 0 {
		t.Fatalf("a writerless reference query served no occurrence")
	}
	if refs.Meta.NextCursor == "" {
		t.Fatalf("a writerless reference query stopped after one page of a longer answer")
	}
	refReq.Page = model.PageRequest{Cursor: refs.Meta.NextCursor}
	second, err := e.References(ctx, refReq)
	if err != nil {
		t.Fatalf("a writerless reference continuation failed instead of serving page two: %v", err)
	}
	if len(second.Items) == 0 {
		t.Fatalf("page two of a writerless reference answer served no occurrence")
	}

	// An engine composed with NO lease store at all -- the read handle a
	// serving process gives its exploration tools -- mints the same leaseless
	// continuation, and must honour it. A page one that mints a token page two
	// refuses is worse than a page one that mints nothing.
	leaseless, err := New(Options{Adjacency: f, Reader: NewMemoryGraph(f.binding, nodes,
		append([]model.Relation(nil), f.relations...)), Signer: signer, Spools: spools,
		Limits: limits})
	if err != nil {
		t.Fatalf("new engine without a lease store: %v", err)
	}
	noLease := model.ReferenceRequest{NodeID: fixtureNodeID("n-b"),
		Operation: model.ReferenceReferences, SemanticSource: model.SemanticCanonical}
	firstNoLease, err := leaseless.References(ctx, noLease)
	if err != nil {
		t.Fatalf("an engine with no lease store failed the query instead of answering: %v", err)
	}
	if firstNoLease.Meta.NextCursor == "" {
		t.Fatalf("an engine with no lease store stopped after one page of a longer answer")
	}
	noLease.Page = model.PageRequest{Cursor: firstNoLease.Meta.NextCursor}
	if _, err := leaseless.References(ctx, noLease); err != nil {
		t.Fatalf("an engine with no lease store refused the continuation it had just minted: %v", err)
	}

	if n := store.liveCount(); n != 0 {
		t.Fatalf("a writerless answer holds %d retention leases", n)
	}
	retained := spoolEntries(t, spoolRoot)
	if len(retained) == 0 {
		t.Fatalf("a paged writerless answer retained no continuation state at all")
	}
	// The sweep of ANOTHER process: its own store over the same root, holding
	// no reservation and no lease row for any of these entries. Past the
	// cursor TTL every one of them must be gone, or the leases these answers
	// could not take have become a leak.
	other, err := pagination.NewSpools(spoolRoot, 64<<20, newFixtureLeases())
	if err != nil {
		t.Fatalf("new spools (second process): %v", err)
	}
	if live, err := other.Sweep(ctx, time.Now().Add(limits.CursorTTL+time.Minute)); err != nil || live != 0 {
		t.Fatalf("another process's sweep left %d live bytes of leaseless state %v; entries were %v",
			live, err, retained)
	}
	if after := spoolEntries(t, spoolRoot); len(after) != 0 {
		t.Fatalf("leaseless continuation state survived a sweep past its recorded expiry: %v", after)
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
