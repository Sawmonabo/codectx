package search

import (
	"context"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// fakePostings is a lexicalSource over an in-memory corpus. It counts how many
// posting statements the walk opens, which is the invariant this file guards:
// one per token for the whole candidate walk, not one per refill.
type fakePostings struct {
	docs     int64           // number of documents, rowids 1..docs
	perDoc   int64           // instances of the term in each document
	opened   map[string]int  // TermOccurrences calls per term
	sessions int             // OpenPostings calls
	live     map[string]bool // streams still open
	docCalls int             // SearchDocuments calls
	docRows  int             // rowids asked for across those calls
}

func newFakePostings(docs, perDoc int64) *fakePostings {
	return &fakePostings{docs: docs, perDoc: perDoc, opened: map[string]int{}, live: map[string]bool{}}
}

func (f *fakePostings) SearchStats(context.Context) (int64, int64, error) {
	return f.docs, f.docs * f.perDoc, nil
}

func (f *fakePostings) DocumentFrequency(_ context.Context, terms []string) ([]int64, error) {
	out := make([]int64, len(terms))
	for i := range terms {
		out[i] = f.docs
	}
	return out, nil
}

func (f *fakePostings) Match(_ context.Context, _ string, after int64, limit int) ([]int64, error) {
	var out []int64
	for id := after + 1; id <= f.docs && len(out) < limit; id++ {
		out = append(out, id)
	}
	return out, nil
}

func (f *fakePostings) SearchDocuments(_ context.Context, rowids []int64) ([]sqlite.SearchDocument, error) {
	f.docCalls++
	f.docRows += len(rowids)
	out := make([]sqlite.SearchDocument, 0, len(rowids))
	for _, id := range rowids {
		out = append(out, sqlite.SearchDocument{RowID: id, TokenCount: f.perDoc,
			Path: "pkg/f" + strconv.FormatInt(id, 10) + ".go", Kind: "function",
			Name: "n" + strconv.FormatInt(id, 10)})
	}
	return out, nil
}

func (f *fakePostings) OpenPostings(context.Context) (postingSession, error) {
	f.sessions++
	return f, nil
}

func (f *fakePostings) Close() error { return nil }

func (f *fakePostings) TermOccurrences(_ context.Context, term string) (occurrenceStream, error) {
	f.opened[term]++
	f.live[term] = true
	return &fakeStream{src: f, term: term}, nil
}

// fakeStream replays the corpus in document order in page-sized refills.
type fakeStream struct {
	src  *fakePostings
	term string
	next int64
}

func (s *fakeStream) Next(_ context.Context, limit int) ([]sqlite.TermOccurrence, error) {
	var out []sqlite.TermOccurrence
	for len(out) < limit && s.next < s.src.docs {
		s.next++
		offsets := make([]int64, 0, s.src.perDoc)
		for i := int64(0); i < s.src.perDoc; i++ {
			offsets = append(offsets, i)
		}
		out = append(out, sqlite.TermOccurrence{RowID: s.next, Column: sqlite.ColumnBody,
			Count: s.src.perDoc, Offsets: offsets})
	}
	return out, nil
}

func (s *fakeStream) Close() error {
	s.src.live[s.term] = false
	return nil
}

// fakeTokenizer splits on spaces; the corpus terms are already normalized.
type fakeTokenizer struct{}

func (fakeTokenizer) Tokenize(_ context.Context, text string) ([]string, error) {
	var out []string
	word := ""
	for _, r := range text {
		if r == ' ' {
			if word != "" {
				out = append(out, word)
			}
			word = ""
			continue
		}
		word += string(r)
	}
	if word != "" {
		out = append(out, word)
	}
	return out, nil
}

// TestWalkHoldsOnePostingStatementPerToken is the regression guard for the
// per-refill re-issue: a walk long enough to need many refills must still open
// exactly one posting statement per query token, and must close them all.
func TestWalkHoldsOnePostingStatementPerToken(t *testing.T) {
	// Enough documents to force many refills at occurrencePageSize.
	const docs = 20 * occurrencePageSize
	src := newFakePostings(docs, 3)
	tier := newLexicalTier(fakeTokenizer{}, config.Limit(0), 1<<20)

	var hits int
	outcome, err := tier.search(context.Background(), src, "key", "alpha beta", func(lexicalHit) error {
		hits++
		return nil
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if outcome.Truncated {
		t.Fatalf("unexpected truncation: %q", outcome.Reason)
	}
	if hits != docs {
		t.Fatalf("scored %d documents, want %d", hits, docs)
	}
	if src.sessions != 1 {
		t.Fatalf("opened %d posting sessions, want 1", src.sessions)
	}
	for _, term := range []string{"alpha", "beta"} {
		if got := src.opened[term]; got != 1 {
			t.Fatalf("term %q opened %d statements for one walk, want 1", term, got)
		}
	}
	for term, open := range src.live {
		if open {
			t.Fatalf("term %q stream left open after the walk", term)
		}
	}
}

// TestOneDocumentReadPerRowidPage is the regression guard for the duplicated
// hydration: the tier and its consumer used to fetch the SAME rowid page from
// SearchDocuments twice, once for TokenCount and once for path/kind/name. One
// read per rowid page must now carry both, so the walk issues exactly as many
// document reads as it has candidate pages, and the emitted hit carries the
// document's servable facts as well as the length its score was computed from.
func TestOneDocumentReadPerRowidPage(t *testing.T) {
	const docs = 3 * matchPageSize
	src := newFakePostings(docs, 2)
	tier := newLexicalTier(fakeTokenizer{}, config.Limit(0), 1<<20)

	var hits []lexicalHit
	if _, err := tier.search(context.Background(), src, "key", "alpha", func(h lexicalHit) error {
		hits = append(hits, h)
		return nil
	}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != docs {
		t.Fatalf("scored %d documents, want %d", len(hits), docs)
	}
	// docs is an exact multiple of matchPageSize, so the walk asks for one
	// trailing empty page to learn it is done: 3 full pages + 1 empty, and the
	// empty one short-circuits before any read.
	if src.docCalls != 3 {
		t.Fatalf("issued %d document reads for 3 rowid pages, want 3", src.docCalls)
	}
	if src.docRows != docs {
		t.Fatalf("document reads asked for %d rowids, want %d", src.docRows, docs)
	}
	for i, h := range hits {
		want := int64(i + 1)
		if h.RowID != want || h.Doc.RowID != want {
			t.Fatalf("hit %d is rowid %d / doc %d, want %d", i, h.RowID, h.Doc.RowID, want)
		}
		if h.Doc.TokenCount != 2 {
			t.Fatalf("hit %d carries token count %d, want 2", i, h.Doc.TokenCount)
		}
		if h.Doc.Path != "pkg/f"+strconv.FormatInt(want, 10)+".go" || h.Doc.Kind != "function" ||
			h.Doc.Name != "n"+strconv.FormatInt(want, 10) {
			t.Fatalf("hit %d carries path %q kind %q name %q, want the document's own",
				i, h.Doc.Path, h.Doc.Kind, h.Doc.Name)
		}
	}
}
