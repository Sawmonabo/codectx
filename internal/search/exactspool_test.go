package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// noLeases is a lease store for spools whose cursors carry no lease: their
// liveness is the expiry stamped in their header, and the store is never asked.
type noLeases struct{}

func (noLeases) AcquireLease(context.Context, model.Lease, string) error {
	return errors.New("not used")
}
func (noLeases) RenewLease(context.Context, string, time.Time) error { return errors.New("not used") }
func (noLeases) ReleaseLease(context.Context, string) error          { return errors.New("not used") }
func (noLeases) LeaseExpiry(context.Context, string) (time.Time, error) {
	return time.Time{}, errors.New("not used")
}

// TestASpooledExactPageReadsOnlyItsRecords proves a page continuing a spooled
// exact phase reads its own records out of the spool and walks no tier: 2 000
// prefix-tier nodes after a position are sorted and spooled past a first page
// of 200, and the next page is the 200 that follow, in served order, with no
// keyset seek and no tier lookup.
//
// Mutation: walk from the position instead of reading the spool (or seek the
// wrong offset) and the page makes keyset seeks (or starts at the wrong node).
func TestASpooledExactPageReadsOnlyItsRecords(t *testing.T) {
	const n, page = 2_001, 200
	ctx := context.Background()
	spools, err := pagination.NewSpools(filepath.Join(t.TempDir(), "spools"), 0, noLeases{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{spools: spools, log: slog.Default()}
	stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "pkg/wide.go"}
	reads := &countingExact{exactReader: stub}
	rule, err := newSearchRule(ctx, reads, "Widget", nil, hitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	pos := searchPosition{phase: phaseExact, rank: model.TierQualifiedNamePrefix.Rank(), node: stub.node(0).Node.ID}
	run, err := s.sortedAfter(ctx, reads, rule, hitFilter{}, pos)
	if err != nil {
		t.Fatalf("sortedAfter: %v", err)
	}
	defer run.Close()
	if run.Len() != n-1 {
		t.Fatalf("the sort holds %d hits after the position, want %d", run.Len(), n-1)
	}
	c := pagination.Cursor{Endpoint: endpointSearch, GenerationID: 1, AnalysisKey: model.AnalysisKey(model.H("a")),
		QueryHash: model.H("q"), ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	id, offset, ok, err := s.spoolRest(run, page, c)
	if err != nil || !ok {
		t.Fatalf("spoolRest: %v, %v", ok, err)
	}
	c.SpoolID = id
	token := searchToken{Cursor: c, Exact: &exactSpoolPos{Offset: offset, Remaining: run.Len() - page,
		Rank: pos.rank, Node: stub.node(page).Node.ID}}

	reads.nodes, reads.among = 0, 0
	var ans searchAnswer
	got, more, err := s.exactPage(ctx, reads, rule, hitFilter{}, pos, page,
		exactSource{spool: &token, now: time.Now()}, &ans)
	if err != nil {
		t.Fatalf("exactPage: %v", err)
	}
	if reads.nodes != 0 || reads.among != 0 {
		t.Fatalf("the spooled page made %d keyset seeks and %d lookups; want none", reads.nodes, reads.among)
	}
	if len(got) != page || !more || ans.spooled == nil || ans.spooled.Remaining != run.Len()-2*page {
		t.Fatalf("the spooled page served %d hits (more %v, state %+v); want %d with %d left", len(got), more,
			ans.spooled, page, run.Len()-2*page)
	}
	for i, e := range got {
		want := stub.node(page + 1 + i)
		if e.Node.Node.ID != want.Node.ID || e.Node.Node.QualifiedName != want.Node.QualifiedName ||
			e.Path != stub.path || e.Tier != model.TierQualifiedNamePrefix || *e.Node.Bytes != *want.Bytes {
			t.Fatalf("hit %d is %s %q at %q, want %s %q: the spool does not hold the served order", i,
				e.Node.Node.ID, e.Node.Node.QualifiedName, e.Tier, want.Node.ID, want.Node.QualifiedName)
		}
	}
}

// widgetDecls is n nodes named "Widget" in one file, so the query "Widget"
// fills the exact_name tier with n hits that page across several cursors.
func widgetDecls(n int) []rowDecl {
	out := make([]rowDecl, n)
	for i := range out {
		key := "W" + strconv.Itoa(i)
		out[i] = rowDecl{scope: "widget-" + key, binding: model.SourceBindingVerified, key: key, name: "Widget",
			qualifiedName: "pkg." + key}
	}
	return out
}

// exactWalk pages "Widget" at limit and answers every node served, in order,
// and how many of its continuations named a spool. Before each continuation
// it calls between with the page number.
func exactWalk(t *testing.T, s *Service, ctx context.Context, limit int, between func(page int)) ([]model.NodeID, int) {
	t.Helper()
	req := model.SearchRequest{Query: "Widget", Page: model.PageRequest{Limit: limit}}
	var out []model.NodeID
	spooled := 0
	for page := 1; ; page++ {
		if page > 100 {
			t.Fatalf("limit %d: the walk did not end", limit)
		}
		got, err := s.Search(ctx, req)
		if err != nil {
			t.Fatalf("limit %d: Search(page %d): %v", limit, page, err)
		}
		for _, h := range got.Items {
			out = append(out, h.NodeID)
		}
		if got.Meta.NextCursor == "" {
			return out, spooled
		}
		tok, err := decodeSearchToken(s.signer, got.Meta.NextCursor, time.Now())
		if err != nil {
			t.Fatalf("limit %d: page %d's cursor: %v", limit, page, err)
		}
		if tok.Exact != nil {
			spooled++
		}
		if between != nil {
			between(page)
		}
		req.Page.Cursor = got.Meta.NextCursor
	}
}

// TestExactPagesThroughASpoolServeTheUnpagedOrder proves an exact phase paged
// through its spool serves exactly the unpaged sequence, that its continuations
// past the second page name the spool, that the spool is released when the
// phase is read to its end, and that a spool removed between pages costs a
// walk and not the answer.
//
// Mutations it catches: a wrong offset or remaining count (a hit served twice
// or skipped); never spooling (no continuation names a spool); leaving the
// spool behind at the end (the directory is not empty); refusing a page whose
// spool is gone (the walk fails with CTX_CURSOR_INVALID).
func TestExactPagesThroughASpoolServeTheUnpagedOrder(t *testing.T) {
	const n = 25
	f := newRowsFixture(t, widgetDecls(n))
	dir := filepath.Join(t.TempDir(), "spools")
	spools, err := pagination.NewSpools(dir, 0, f.opts.Store)
	if err != nil {
		t.Fatal(err)
	}
	opts := f.opts
	opts.Spools = spools
	s := newService(t, opts)
	whole, _ := exactWalk(t, s, f.ctx, n+10, nil)
	if len(whole) != n {
		t.Fatalf("the unpaged answer serves %d hits, want %d", len(whole), n)
	}
	files := func() []string {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			if e.Type().IsRegular() && filepath.Ext(e.Name()) == "" && len(e.Name()) > 6 && e.Name()[:6] == "spool-" {
				names = append(names, e.Name())
			}
		}
		return names
	}
	for _, limit := range []int{1, 3, 7} {
		paged, spooled := exactWalk(t, s, f.ctx, limit, nil)
		if fmt.Sprint(paged) != fmt.Sprint(whole) {
			t.Fatalf("limit %d: the paged answer\n%v\nis not the unpaged one\n%v", limit, paged, whole)
		}
		if pages := (n + limit - 1) / limit; pages > 2 && spooled == 0 {
			t.Fatalf("limit %d: %d pages and no continuation named a spool", limit, pages)
		}
		if left := files(); len(left) != 0 {
			t.Fatalf("limit %d: spools left after the answer was read to its end: %v", limit, left)
		}
	}
	// The spool removed after the second page: the third page walks from its
	// position and the answer is unchanged.
	paged, _ := exactWalk(t, s, f.ctx, 3, func(page int) {
		if page == 2 {
			for _, name := range files() {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	if fmt.Sprint(paged) != fmt.Sprint(whole) {
		t.Fatalf("with the spool removed mid-answer, the paged answer\n%v\nis not the unpaged one\n%v", paged, whole)
	}
}
