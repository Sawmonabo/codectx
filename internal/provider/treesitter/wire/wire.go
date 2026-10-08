// Package wire is the framed protocol between the tree-sitter provider and
// its parser worker subprocess. Every message -- a hello, a request, a source
// file, one fact record, one function's dependence facts, a done or an
// error -- travels as one or more
// length-prefixed frames of at most ChunkBytes each; a message longer than one
// frame continues in frames of the same kind, and the reader reassembles it
// whole. No message has a size limit of its own: a source file is bounded by
// workspace.max_parse_file_bytes before it is ever sent, and a record by the
// file it was extracted from. The worker never sees storage, identities or
// the resolver: it reports byte offsets and names, and the parent validates
// every field against the pinned bytes before anything reaches the sink.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// Subcommand is the hidden argv[1] under which the codectx binary runs as a
// parser worker; test binaries dispatch on the same argument.
const Subcommand = "__ts-worker"

// Kind tags one message and every frame it is sent in.
type Kind byte

const (
	// KindHello is the worker's first message: its identity and fingerprint.
	KindHello Kind = 1
	// KindRequest opens one parse; KindSource carries the raw bytes as one
	// message of exactly Request.SourceBytes bytes.
	KindRequest Kind = 2
	KindSource  Kind = 3
	// Fact messages, one record each, then KindDone or KindError. A
	// file's answer is every KindDecl, KindImport and KindRef message, then
	// one KindFunction message per callable in the lowering's preorder, then
	// KindDone.
	KindDecl   Kind = 4
	KindImport Kind = 5
	KindRef    Kind = 6
	KindDone   Kind = 7
	KindError  Kind = 8
	// KindFunction carries one callable's dependence facts in the binary
	// encoding of AppendFunction, never JSON.
	KindFunction Kind = 9
)

// ChunkBytes is the transport unit, not a limit on anything carried: the
// largest payload one frame holds. A message of any length is sent as
// ceil(length/ChunkBytes) frames, every one but the last flagged to continue,
// and a frame is never allocated for a length it did not declare within this
// unit, so a corrupt header costs the reader at most one unit before it is
// refused as malformed. What a reader holds for one message is what the
// writer actually sent, grown as the frames arrive.
//
// That is the whole bound on the parent's heap for one file's answer: the
// records the worker extracted from that one file, which is a function of the
// file's bytes and therefore of workspace.max_parse_file_bytes. No record is
// ever refused or dropped for its size on the wire. A record the sink is
// later handed over a user-set resources.max_provider_record_bytes is
// admitted and counted as a degradation there (package provider, Sink).
const ChunkBytes = 64 << 10

// MaxSourceOffset is the widest byte offset the parser addresses and the
// records carry (uint32). It is the width of the parser's coordinates, not a
// size policy: a file longer than it cannot be addressed at all, so the parent
// reports such a file unavailable rather than send it and publish wrapped
// offsets. workspace.max_parse_file_bytes is the only size policy.
const MaxSourceOffset = math.MaxUint32

const (
	headerBytes = 5
	// more flags a frame whose message continues in the next frame.
	more = 0x80
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
// fingerprint and the build the parent must match, and the languages it
// verified.
type Hello struct {
	PID         int      `json:"pid"`
	Fingerprint string   `json:"fingerprint"`
	Build       string   `json:"build"`
	Languages   []string `json:"languages"`
	// Memory is the worker's standing memory once it has started and before
	// its first file: BaseBytes and AnonBytes, never NeedBytes.
	Memory Memory `json:"memory"`
}

// Build is this binary's build identity as a worker states it in its hello
// and the parent keys what it learns of a worker's files by: the version, the
// commit and the toolchain it was built from. The parent refuses a worker of
// another build, so a need model learned under one build is only ever applied
// to workers of that build.
func Build() string {
	b := model.CurrentBuildInfo()
	return b.Version + " " + b.Commit + " " + b.Toolchain
}

// Memory is one file-boundary reading the worker takes of itself. Every
// field is absent (nil) when the platform cannot supply it, never zero.
type Memory struct {
	// NeedBytes is the file's need: the worker's resident peak over the file
	// less the base it started the file from. It is absent where the platform
	// offers no resettable per-process peak, and then nothing is learned from
	// the file.
	NeedBytes *uint64 `json:"need_bytes,omitempty"`
	// BaseBytes is the worker's resident set once this file's memory has been
	// returned and its peak reset: what the worker holds between files, and
	// the base the next file's need is measured from.
	BaseBytes *uint64 `json:"base_bytes,omitempty"`
	// AnonBytes is the anonymous part of that resident set, the memory that
	// is this worker's alone rather than pages of the executable it shares
	// with every other worker. It is what the parent holds a worker's base at
	// and counts as the product's own residency.
	AnonBytes *uint64 `json:"anon_bytes,omitempty"`
}

// Request opens one parse. The source follows as one KindSource message of
// exactly SourceBytes bytes.
type Request struct {
	Language    string `json:"language"`
	Path        string `json:"path"`
	SourceBytes uint64 `json:"source_bytes"`
	// Fallback is the grammar a header whose parse with Language has errors
	// is parsed with once more, the parse with fewer error bytes being kept.
	// It is empty for every file that is not a header.
	Fallback string `json:"fallback,omitempty"`
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
	// NameStart and NameEnd bound the declaration's name token alone, inside
	// [Start, End). A variable the dependence pass declares at that token is
	// this declaration, not a second entity.
	NameStart uint32 `json:"name_start"`
	NameEnd   uint32 `json:"name_end"`
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
	// Truncated reports that the query cursor exceeded its match limit, so
	// the query did not see every match in the tree.
	Truncated bool `json:"truncated,omitempty"`
	// Header is a header's grammar choice: the grammar each parse used, which
	// was kept and why. It is nil for a request that named no fallback.
	Header *lang.HeaderChoice `json:"header,omitempty"`
	// DependenceFailure is why the file's dependence pass produced no
	// function message at all: a failure outside every function's analysis,
	// such as the tree failing to flatten. It is empty when the pass ran, and
	// a function whose own analysis failed is disclosed by its message
	// instead (Function.Failed). It never fails the file's structural facts.
	DependenceFailure string `json:"dependence_failure,omitempty"`
	// Memory is the worker's reading of itself at the end of this file.
	Memory Memory `json:"memory"`
}

// Error is a per-request failure that leaves the worker healthy.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrMalformedFrame reports a frame no writer of this protocol produces: a
// payload longer than ChunkBytes, an unknown continuation, or a continuation
// of a different kind than the frame it continues.
var ErrMalformedFrame = errors.New("wire: malformed frame")

// WriteMessage emits payload under kind as ceil(len/ChunkBytes) frames, or
// one empty frame when payload is empty.
func WriteMessage(w io.Writer, kind Kind, payload []byte) error {
	for {
		n := min(len(payload), ChunkBytes)
		var hdr [headerBytes]byte
		binary.BigEndian.PutUint32(hdr[:4], uint32(n))
		hdr[4] = byte(kind)
		if n < len(payload) {
			hdr[4] |= more
		}
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if n > 0 {
			if _, err := w.Write(payload[:n]); err != nil {
				return err
			}
		}
		payload = payload[n:]
		if len(payload) == 0 {
			return nil
		}
	}
}

// WriteJSON encodes v and emits it as one message under kind.
func WriteJSON(w io.Writer, kind Kind, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteMessage(w, kind, payload)
}

// ReadMessage reads one message, reassembling its continuation frames. The
// returned payload is freshly allocated and owned by the caller. sizeHint is
// the length the caller expects, when it knows it from a peer it trusts (the
// worker reading a source the parent announced); it only presizes the buffer
// and bounds nothing. Zero means unknown, and the buffer grows with what
// arrives.
func ReadMessage(r io.Reader, sizeHint int) (Kind, []byte, error) {
	kind, cont, payload, err := readFrame(r)
	if err != nil || !cont {
		return kind, payload, err
	}
	msg := make([]byte, 0, max(sizeHint, 2*len(payload)))
	msg = append(msg, payload...)
	for cont {
		var k Kind
		if k, cont, payload, err = readFrame(r); err != nil {
			return 0, nil, err
		}
		if k != kind {
			return 0, nil, fmt.Errorf("%w: a kind %d frame continues a kind %d message", ErrMalformedFrame, k, kind)
		}
		msg = append(msg, payload...)
	}
	return kind, msg, nil
}

// readFrame reads one frame: its kind, whether the message continues, and its
// payload.
func readFrame(r io.Reader) (Kind, bool, []byte, error) {
	var hdr [headerBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, false, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n > ChunkBytes {
		return 0, false, nil, fmt.Errorf("%w: a %d-byte frame is longer than the %d-byte transport unit", ErrMalformedFrame, n, ChunkBytes)
	}
	cont := hdr[4]&more != 0
	if cont && n < ChunkBytes {
		return 0, false, nil, fmt.Errorf("%w: a %d-byte frame continues its message before filling the transport unit", ErrMalformedFrame, n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, false, nil, err
	}
	return Kind(hdr[4] &^ more), cont, payload, nil
}
