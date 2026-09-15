// Package wire is the framed protocol between the tree-sitter provider and
// its parser worker subprocess (ruling R8-3): length-prefixed JSON frames
// with a per-frame cap on both sides, so neither process allocates for a
// length it has not agreed to. The worker never sees storage, identities or
// the resolver: it reports byte offsets and names, and the parent validates
// every field against the pinned bytes before anything reaches the sink.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Subcommand is the hidden argv[1] under which the codectx binary runs as a
// parser worker (ruling R8-1); test binaries dispatch on the same argument.
const Subcommand = "__ts-worker"

// Kind tags one frame.
type Kind byte

const (
	// KindHello is the worker's first frame: its identity and fingerprint.
	KindHello Kind = 1
	// KindRequest opens one parse; KindSource carries the raw bytes.
	KindRequest Kind = 2
	KindSource  Kind = 3
	// Fact frames, one record each, then KindDone or KindError.
	KindDecl   Kind = 4
	KindImport Kind = 5
	KindRef    Kind = 6
	KindDone   Kind = 7
	KindError  Kind = 8
)

// Bounds. MaxFactFrameBytes is the per-record cap the parent enforces on
// every frame from the child. It does not bound the sink record a frame
// becomes; the bound that does is the parent's own: the largest fact it can
// build is one node or relation carrying MaxEvidencePerFact (64) evidence
// rows of about 620 bytes of identifiers and positions plus a native key of
// at most MaxNativeKeyBytes (2048), so about 167 KiB, well under the 4 MiB
// max_provider_record_bytes a sink is configured with. MaxSourceBytes is the
// worker's absolute ceiling on one source frame; the parent's configured
// workspace.max_parse_file_bytes is enforced before a request is ever sent.
const (
	MaxFactFrameBytes = 64 << 10
	MaxSourceBytes    = 64 << 20
	headerBytes       = 5
)

const (
	// MaxQualifierBytes bounds Ref.Qualifier. A call's receiver is an
	// arbitrary expression -- `a.b(x).c(y).Scan(&v)` has a receiver hundreds
	// of bytes long -- so the worker reports a qualifier only when it is a
	// name of at most this many bytes and leaves it empty otherwise, rather
	// than sending an unbounded string the parent must refuse. It is at most
	// model.MaxNameBytes; facts.go proves that at compile time.
	MaxQualifierBytes = 512
)

// Hello is the worker's identity: the PID for accounting, the language
// fingerprint the parent must match, and the languages it verified.
type Hello struct {
	PID         int      `json:"pid"`
	Fingerprint string   `json:"fingerprint"`
	Languages   []string `json:"languages"`
}

// Request opens one parse. The source follows in a KindSource frame of
// exactly SourceBytes bytes.
type Request struct {
	Language    string `json:"language"`
	Path        string `json:"path"`
	SourceBytes uint32 `json:"source_bytes"`
	// MaxRecordsPerFile is providers.tree_sitter.max_records_per_file: how
	// many declarations, imports or references (each counted separately) the
	// user wants one file to yield. 0 -- the default -- is unlimited.
	//
	// It replaces three hard-coded ceilings of 20000, 4000 and 60000 that the
	// worker and the parent each held a copy of. A generated file names what it
	// names, and what it yields is bounded by its own size, which
	// workspace.max_parse_file_bytes already bounds, so an unlimited value
	// costs one file's heap and never the repository's.
	//
	// It travels on the request precisely so the worker and the parent read the
	// SAME number: the parent's check exists to stop a misbehaving child from
	// making it buffer more than a healthy one would send, and a parent holding
	// its own constant would kill a healthy worker's output the moment the
	// operator raised the limit. A file that reaches it is reported truncated
	// through Done.Truncated and its structural coverage is partial.
	MaxRecordsPerFile uint64 `json:"max_records_per_file,omitempty"`
}

// Decl is one declaration. Offsets are byte offsets into the source; Parent
// is the ordinal of the containing declaration or -1. The parent recomputes
// lines and columns from the bytes and never trusts a coordinate it did not
// derive.
type Decl struct {
	ID        int    `json:"id"`
	Parent    int    `json:"parent"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Qualified string `json:"qualified"`
	Start     uint32 `json:"start"`
	End       uint32 `json:"end"`
	SigEnd    uint32 `json:"sig_end"`
	DocStart  uint32 `json:"doc_start"`
	DocEnd    uint32 `json:"doc_end"`
	Exported  bool   `json:"exported,omitempty"`
	Test      bool   `json:"test,omitempty"`
	Prototype bool   `json:"prototype,omitempty"`
	Macro     bool   `json:"macro,omitempty"`
	// Impl names the Rust impl target or C++ qualifier a method was defined
	// under when its containing type is not a declaration in this file.
	Impl string `json:"impl,omitempty"`
}

// Import is one import/include/use statement.
type Import struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
	Path  string `json:"path"`
	// Names are the local names the statement introduces, when known.
	Names []string `json:"names,omitempty"`
}

// Ref is one reference occurrence: a call site or a type reference.
type Ref struct {
	// Kind is "call" or "type".
	Kind string `json:"kind"`
	// Start and End bound the whole reference expression: the call
	// expression for a call, the identifier itself for a type reference.
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
	// NameStart and NameEnd bound the callee/type identifier token alone.
	// They are what the Section 11.3 callsite alias is built from, and what
	// a SCIP occurrence covers, so the two providers name the same range;
	// the enclosing expression's range would not join anything.
	NameStart uint32 `json:"name_start"`
	NameEnd   uint32 `json:"name_end"`
	Name      string `json:"name"`
	// Scope is the ordinal of the enclosing declaration or -1 for module level.
	Scope int `json:"scope"`
	// Qualified reports that the call named a receiver/object/scope
	// qualifier; Qualifier is that qualifier when it is a name within
	// MaxQualifierBytes and empty when the receiver is a larger expression,
	// which is a callee this file cannot name rather than a string to
	// truncate. QualifierIsImport reports that the qualifier is a name an
	// import in this file introduced, which makes the target cross-file; it
	// is decided from the receiver's full text, never the bounded copy.
	Qualified         bool   `json:"qualified,omitempty"`
	Qualifier         string `json:"qualifier,omitempty"`
	QualifierIsImport bool   `json:"qualifier_is_import,omitempty"`
}

// Done closes one parse.
type Done struct {
	// Package is the package/module clause when the language has one.
	Package string `json:"package,omitempty"`
	// SyntaxErrors reports that the tree contains ERROR or MISSING nodes.
	SyntaxErrors bool `json:"syntax_errors,omitempty"`
	// Truncated reports that a per-file record bound was reached.
	Truncated bool `json:"truncated,omitempty"`
	// RSSBytes is the worker's resident set after this parse, or 0 when the
	// platform cannot report it (recorded as unavailable, not zero, upstream).
	RSSBytes uint64 `json:"rss_bytes,omitempty"`
}

// Error is a per-request failure that leaves the worker healthy.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrFrameTooLarge reports a frame whose declared length exceeds the cap.
var ErrFrameTooLarge = errors.New("wire: frame exceeds its byte cap")

// Write emits one frame.
func Write(w io.Writer, kind Kind, payload []byte) error {
	if len(payload) > MaxSourceBytes {
		return ErrFrameTooLarge
	}
	var hdr [headerBytes]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)))
	hdr[4] = byte(kind)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSON encodes v and emits it under kind, refusing an encoding larger
// than max.
func WriteJSON(w io.Writer, kind Kind, v any, max int) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > max {
		return fmt.Errorf("%w: %d bytes over the %d-byte cap", ErrFrameTooLarge, len(payload), max)
	}
	return Write(w, kind, payload)
}

// Read reads one frame, refusing to allocate for a length over max. The
// returned payload is freshly allocated and owned by the caller.
func Read(r io.Reader, max int) (Kind, []byte, error) {
	var hdr [headerBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if int64(n) > int64(max) {
		return 0, nil, fmt.Errorf("%w: %d bytes over the %d-byte cap", ErrFrameTooLarge, n, max)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return Kind(hdr[4]), payload, nil
}

// ResidentBytes reports this process's resident set size and false when the
// platform cannot report it. Both sides measure the same way — the worker to
// put its RSS in the Done frame, the parent for its own — so the aggregate
// accounting of Section 22 sums one measurement, not two definitions. A
// caller records an unmeasurable value as unavailable, never as zero.
func ResidentBytes() (int64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || pages < 0 {
		return 0, false
	}
	return pages * int64(os.Getpagesize()), true
}
