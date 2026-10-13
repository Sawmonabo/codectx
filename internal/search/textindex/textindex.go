// Package textindex is the embedded search index of the lexical tier
// (docs/adr/ADR-0013-search-index.md): a static library linked through cgo,
// reached through a handful of batch calls. One index holds the documents of
// every retained unit; a generation searches it through a view whose alive set
// is the generation's visible documents.
//
// The package imports nothing else of codectx's storage or search, so the store
// that owns the index directory and the search service that reads pages can
// both depend on it.
//
// Build: the library is built by Cargo before the Go build links it,
//
//	go generate ./internal/search/textindex
//
// which runs `cargo build --release --locked` here and leaves the archive under
// `_build/` (docs/toolchain.md).
package textindex

// The rustflags value is a TOML literal string, so a home directory spelled
// with backslashes, as on Windows, is not read as escapes.
//
//go:generate cargo build --release --locked --manifest-path Cargo.toml --config "build.rustflags=['--remap-path-prefix=$HOME=~']"

import (
	"context"
	"errors"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Options opens an index.
type Options struct {
	// Dir is the index directory. A writable open creates it when absent; a
	// read-only open of a missing directory is refused.
	Dir string
	// Writable lets this process change the index. The writer is opened by the
	// first mutating call, after Reserve admits its memory, and closed by
	// ReleaseWriter or Close.
	Writable bool
	// WriterThreads and WriterMemoryBytes size the writer. WriterMemoryBytes is
	// the budget of all its threads together and is never below
	// WriterMemoryFloor(WriterThreads).
	WriterThreads     int
	WriterMemoryBytes int64
	// MergeThreads runs segment merges; SearchThreads runs one query's
	// segments in parallel.
	MergeThreads  int
	SearchThreads int
	// QuerySlots is how many searches may run at once; a search past it waits
	// for a slot.
	QuerySlots int
	// ViewCacheBytes bounds the cached alive sets of every view this index
	// opened, across generations.
	ViewCacheBytes int64
	// Reserve admits the writer's memory before the writer opens and returns
	// the release that gives it back when the writer closes. Nil reserves
	// nothing.
	Reserve func(ctx context.Context, bytes int64) (release func(), err error)
}

// Document is one search document of a unit. DocID is the store-assigned
// identity that a generation's visible set names; it is never reused.
type Document struct {
	DocID         uint64
	SearchKey     string
	NodeID        model.NodeID
	FileID        model.FileID
	Path          string
	Kind          model.NodeKind
	Name          string
	QualifiedName string
	Signature     string
	Bytes         model.ByteRange
	Body          string
}

// LengthRule is the document length BM25F normalizes by.
type LengthRule uint8

const (
	// LengthTotal is the token count over every column.
	LengthTotal LengthRule = 0
	// LengthWeighted is the sum over columns of the column weight times the
	// column's token count.
	LengthWeighted LengthRule = 1
)

// Request is one page of the lexical tier.
type Request struct {
	Query string
	// Kinds keeps documents of these kinds; empty keeps every kind.
	Kinds []model.NodeKind
	// PathPrefixes keeps documents whose path equals a prefix or lies under
	// it; each is a cleaned root-relative path. Empty keeps every path.
	PathPrefixes []string
	// Depth is how many folded results the page holds at most, best first.
	Depth int
	// After, when set, resumes the ranking after a result already served: the
	// page holds only the results that sort strictly after it.
	After      *Position
	LengthRule LengthRule
	// MaxTerms refuses a query with more terms; zero is unlimited.
	MaxTerms int64
	// MaxBytes bounds what the page's allocations that grow with its depth or
	// its position may hold, the caller's copy of each result included; zero
	// is unbounded. A page the bound ends early holds an exact prefix of the
	// results and says More. ResultBytes is what the caller holds per result
	// beyond the result's strings, and MarkedBound bounds the groups the
	// resume position marks (Stats.NewMarks of the page before).
	MaxBytes    int64
	ResultBytes int64
	MarkedBound int64
	// IgnoreSummaries scores every block, as a segment with no summary is
	// scored. It is the reference the pruned search must equal.
	IgnoreSummaries bool
}

// Position is a place in a view's ranking: a served result, named by its score
// and the doc id of the document it is. The library reads the rest of the
// result's sort key back from that document, so a position is a few bytes
// whatever the result's strings. It is meaningful only for the view, query,
// filters and length rule its result was served under.
type Position struct {
	ScoreMicros int64
	DocID       uint64
}

// Hit is one folded result: the best-scoring document of its result key (the
// node, or the file and start byte of a document with no node).
//
// Results are ranked by score descending, then by the content key: path,
// start byte, end byte, kind, name, qualified name, signature, node id and
// search key. The order is total.
type Hit struct {
	// DocID is the document the result is; with ScoreMicros it is the
	// result's Position.
	DocID         uint64
	SearchKey     string
	NodeID        model.NodeID
	FileID        model.FileID
	Path          string
	Kind          model.NodeKind
	Name          string
	QualifiedName string
	Signature     string
	Bytes         model.ByteRange
	ScoreMicros   int64
	// Occurrences counts the result's matching documents the search visited.
	// OccurrencesExact says the count is exact; otherwise it is a lower bound.
	Occurrences      int64
	OccurrencesExact bool
}

// Stats is the work one search did.
type Stats struct {
	Segments               int64
	SegmentsWithoutSummary int64
	BlocksTotal            int64
	BlocksScored           int64
	DocsScored             int64
	// Heaps is how many segments were scored at once, each holding a heap of
	// Cap places (the request's depth plus one, doubled on every rescoring a
	// marked group forced, and fitted to MaxBytes); Marked is how many groups
	// the resume position marked as served across the segments. NewMarks
	// bounds the groups a request resuming after the page's last hit marks,
	// and Bytes is what the page's growing allocations peaked at, as charged
	// against MaxBytes.
	Heaps    int64
	Cap      int64
	Marked   int64
	NewMarks int64
	Bytes    int64
}

// Page is the answer to one Request.
type Page struct {
	// Terms is the number of scored query terms.
	Terms int
	Hits  []Hit
	// More says results exist past Depth.
	More bool
	// Canceled says the context ended the search; Hits are the best results
	// found before it, not the page's true ranking.
	Canceled bool
	// Matches counts the matching visible documents; MatchesExact says every
	// match was visited, otherwise Matches is a lower bound.
	Matches      int64
	MatchesExact bool
	Stats        Stats
}

// ErrTooManyTerms is what errors.Is matches on the refusal of a query with
// more than Request.MaxTerms terms; errors.As on *TooManyTerms reads the count.
var ErrTooManyTerms = errors.New("the query carries more terms than the limit")

// TooManyTerms is the refusal of a query over Request.MaxTerms.
type TooManyTerms struct{ Terms int64 }

func (e *TooManyTerms) Error() string { return ErrTooManyTerms.Error() }

// Unwrap lets errors.Is match ErrTooManyTerms.
func (e *TooManyTerms) Unwrap() error { return ErrTooManyTerms }

// ErrUnknownPosition is what errors.Is matches when a request's After names no
// document of the view: a position served by another view, or none at all.
var ErrUnknownPosition = errors.New("the resume position names no document of this view")

// At answers the hit's Position.
func (h Hit) At() Position { return Position{ScoreMicros: h.ScoreMicros, DocID: h.DocID} }
