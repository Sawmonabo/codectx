package scip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// SCIP field numbers and enum values (scip.proto, github.com/scip-code/scip
// v0.10.0). The generated Go bindings are a separate Go module that the
// pinned root module does not contain, so the handful of records this
// provider needs are decoded here with the bounded wire reader; every record
// is size-limited before it is read.
const (
	fieldIndexMetadata        = 1
	fieldIndexDocuments       = 2
	fieldIndexExternalSymbols = 3

	fieldMetadataToolInfo     = 2
	fieldMetadataProjectRoot  = 3
	fieldMetadataTextEncoding = 4
	fieldToolInfoName         = 1
	fieldToolInfoVersion      = 2

	fieldDocumentRelativePath = 1
	fieldDocumentOccurrences  = 2
	fieldDocumentSymbols      = 3
	fieldDocumentLanguage     = 4
	fieldDocumentText         = 5
	fieldDocumentEncoding     = 6

	fieldOccurrenceRange              = 1
	fieldOccurrenceSymbol             = 2
	fieldOccurrenceRoles              = 3
	fieldOccurrenceEnclosingRange     = 7
	fieldOccurrenceSingleRange        = 8
	fieldOccurrenceMultiRange         = 9
	fieldOccurrenceSingleEnclosing    = 10
	fieldOccurrenceMultiEnclosing     = 11
	fieldSymbolInfoSymbol             = 1
	fieldSymbolInfoRelationships      = 4
	fieldSymbolInfoKind               = 5
	fieldSymbolInfoDisplayName        = 6
	fieldSymbolInfoSignature          = 7
	fieldSignatureText                = 5
	fieldRelationshipSymbol           = 1
	fieldRelationshipIsReference      = 2
	fieldRelationshipIsImplementation = 3
	fieldRelationshipIsTypeDefinition = 4
	fieldRelationshipIsDefinition     = 5

	// Only the definition and import roles are read. The read and write
	// access roles exist in the format but are not mapped: `reads` and
	// `writes` belong to the dependence provider (Section 11.6).
	roleDefinition = 0x1
	roleImport     = 0x2

	encodingUnspecified = 0
	encodingUTF8        = 1
	encodingUTF16       = 2
	encodingUTF32       = 3

	// maxRelationships bounds one symbol's relationship list; the record is
	// already size-bounded, this keeps the decoded slice small as well.
	maxRelationships = 4096
)

// metadata is the bounded Index.metadata record.
type metadata struct {
	toolName, toolVersion, projectRoot string
	textEncoding                       int32
}

// occurrence is one decoded Occurrence. rng and enclosing are SCIP ranges of
// three (single line) or four values, zero-based lines and columns in the
// document's position encoding.
type occurrence struct {
	symbol       string
	roles        int32
	rng          []int32
	enclosing    []int32
	hasEnclosing bool
}

// relationship is one SymbolInformation.relationships entry.
type relationship struct {
	symbol string
	flags  int32 // bit 0 reference, 1 implementation, 2 type definition, 3 definition
}

const (
	relReference      = 1
	relImplementation = 2
	relTypeDefinition = 4
	relDefinition     = 8
)

// symbolInfo is one decoded SymbolInformation.
type symbolInfo struct {
	symbol, displayName, signature string
	// signatureCut is the ORIGINAL encoded byte length of a signature the
	// field ceiling shortened, and 0 when nothing was cut. It cannot come
	// from model.TruncateField's second result the way treesitter/facts.go
	// derives it: the decoder never holds the whole value, so the length is
	// taken from the wire, which is the only place it exists.
	signatureCut  int64
	kind          int32
	relationships []relationship
}

// document is the summary the walker reports when a Document record ends:
// everything needed to bind it to a snapshot file. It never carries the text;
// the text is hashed as it streams past.
type document struct {
	index       int64
	path        string
	language    string
	encoding    int32
	hasText     bool
	textHash    string
	occurrences int64
}

// Bound names a decode drop is counted under. They are the wire reader's own
// `what` strings, so a count and the message a class-B refusal would have
// carried name the same bound.
const (
	boundOccurrenceSymbol   = "occurrence symbol"
	boundSymbol             = "symbol"
	boundRelationshipSymbol = "relationship symbol"
	boundDisplayName        = "display name"
	boundSignature          = "signature"
	boundRelationships      = "relationships per symbol"
)

// Detail keys the decode drop counts are published under on the unit's
// capability row, and the reason they share.
const (
	detailDroppedRecords = "decode_dropped_records"
	detailDroppedFields  = "decode_dropped_fields"
	detailDropReason     = "decode_drop_reason"
	dropReason           = "value exceeds its field bound"
)

// decodeDrops counts what the decoder discarded because a value exceeded a
// field bound (plan row 26). The split is what the bound costs:
//
//   - An IDENTIFYING field cannot be dropped on its own. A symbol without its
//     symbol string is not a fact about anything, and `decodeSymbolInfo` ends
//     in "symbol information has no symbol", so dropping just the string would
//     turn an over-limit into a malformed abort. The whole RECORD is dropped
//     and counted.
//   - A DECORATIVE field (display name, signature text, the relationships past
//     the pre-allocation bound) is dropped on its own and the record is kept
//     and published.
//
// Neither ever fails the unit: the counts are published as degradation details
// on the unit's capability row, which is where the answer says what it cut.
type decodeDrops struct {
	records map[string]int64
	fields  map[string]int64
}

func (d *decodeDrops) dropRecord(bound string) {
	if d == nil {
		return
	}
	if d.records == nil {
		d.records = map[string]int64{}
	}
	d.records[bound]++
}

func (d *decodeDrops) dropField(bound string) {
	if d == nil {
		return
	}
	if d.fields == nil {
		d.fields = map[string]int64{}
	}
	d.fields[bound]++
}

func (d *decodeDrops) any() bool { return d != nil && (len(d.records) > 0 || len(d.fields) > 0) }

// summary renders one count map as a stable, sorted `bound=n` list so the
// detail value is reproducible across runs of the same index.
func summarizeDrops(counts map[string]int64) string {
	if len(counts) == 0 {
		return ""
	}
	bounds := make([]string, 0, len(counts))
	for b := range counts {
		bounds = append(bounds, b)
	}
	sort.Strings(bounds)
	var b strings.Builder
	for i, name := range bounds {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(counts[name], 10))
	}
	return b.String()
}

// walker streams one index through the wire reader. Callbacks that are nil
// mean the walker skips that record kind without decoding it, which is how
// the binding pre-pass reads only paths and text hashes.
type walker struct {
	limits Limits
	// drops counts the records and fields this walk discarded for exceeding a
	// field bound. It is owned by the caller so one account covers every pass.
	drops *decodeDrops
	// buf holds the record being read; strs holds strings cut from it while
	// it is decoded. They must be distinct: decoding reads from buf.
	buf, strs []byte
	// reserved is the buffer capacity last reported through onGrow.
	reserved int64

	// onGrow is told when the record buffer grows, so the owner can charge
	// the new capacity against the sink's byte pool before it is used.
	onGrow       func(context.Context, int64) error
	onMetadata   func(metadata) error
	onOccurrence func(doc int64, seq int64, o occurrence, recordBytes int64) error
	onSymbol     func(doc int64, s symbolInfo, recordBytes int64) error
	onDocument   func(document) error
	onExternal   func(s symbolInfo, recordBytes int64) error
	// seen tallies the largest figure this walk observed at each counted
	// bound; the importer compares it with the configuration once, at the end.
	seen *limitSeen
}

// record reads one bounded record into the walker's buffer and reports a
// grown buffer to the owner.
func (w *walker) record(ctx context.Context, r *reader, n int64, what string) error {
	var err error
	if w.buf, err = r.bytes(n, w.limits.MaxRecordBytes, what, w.buf); err != nil {
		return err
	}
	if w.onGrow != nil && int64(cap(w.buf)) > w.reserved {
		w.reserved = int64(cap(w.buf))
		return w.onGrow(ctx, w.reserved)
	}
	return nil
}

// walk consumes the whole index of size bytes.
func (w *walker) walk(ctx context.Context, src byteSource, size int64) (consumed int64, err error) {
	r := newReader(src, size)
	var docs int64
	for !r.done() {
		if err := ctx.Err(); err != nil {
			return *r.consumed, model.Canceled(err)
		}
		field, wt, err := r.tag()
		if err != nil {
			return *r.consumed, err
		}
		if wt != wireBytes {
			if err := r.skip(wt); err != nil {
				return *r.consumed, err
			}
			continue
		}
		n, err := r.length()
		if err != nil {
			return *r.consumed, err
		}
		switch field {
		case fieldIndexMetadata:
			if w.onMetadata == nil {
				err = r.discardSub(n)
				break
			}
			if err = w.record(ctx, r, n, "metadata record"); err != nil {
				break
			}
			var m metadata
			if m, err = decodeMetadata(w.buf); err == nil {
				err = w.onMetadata(m)
			}
		case fieldIndexDocuments:
			// A document count refuses nothing: documents stream past a
			// bounded record buffer into an on-disk spool, so the figure
			// bounds no allocation. A user-set bound is recorded and
			// reported once the run ends.
			docs++
			w.seen.note(limitDocuments, docs)
			var doc *reader
			if doc, err = r.sub(n); err == nil {
				err = w.document(ctx, doc, docs-1)
			}
		case fieldIndexExternalSymbols:
			if w.onExternal == nil {
				err = r.discardSub(n)
				break
			}
			if err = w.record(ctx, r, n, "symbol record"); err != nil {
				break
			}
			var s symbolInfo
			var dropped bool
			if s, dropped, err = decodeSymbolInfo(w.buf, w.drops); err == nil && !dropped {
				err = w.onExternal(s, n)
			}
		default:
			err = r.discardSub(n)
		}
		if err != nil {
			return *r.consumed, err
		}
	}
	return *r.consumed, nil
}

// document walks one Document record field by field. Occurrence and symbol
// records are read individually within their bound; the text is streamed
// through a hasher and never held.
func (w *walker) document(ctx context.Context, r *reader, index int64) error {
	d := document{index: index}
	hasher := sha256.New()
	var seq int64
	for !r.done() {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		field, wt, err := r.tag()
		if err != nil {
			return err
		}
		if wt != wireBytes {
			if field == fieldDocumentEncoding && wt == wireVarint {
				v, err := r.varint()
				if err != nil {
					return err
				}
				d.encoding = int32(v)
				continue
			}
			if err := r.skip(wt); err != nil {
				return err
			}
			continue
		}
		n, err := r.length()
		if err != nil {
			return err
		}
		switch field {
		case fieldDocumentRelativePath:
			if w.buf, err = r.bytes(n, int64(model.MaxPathBytes), "document path", w.buf); err != nil {
				return err
			}
			d.path = string(w.buf)
		case fieldDocumentLanguage:
			if w.buf, err = r.bytes(n, int64(model.MaxLanguageBytes), "document language", w.buf); err != nil {
				return err
			}
			d.language = string(w.buf)
		case fieldDocumentText:
			d.hasText = true
			if err := r.stream(n, hasher); err != nil {
				return err
			}
		case fieldDocumentOccurrences:
			// Same shape as the document count: spooled, never resident,
			// so the largest per-document figure is reported, not refused.
			d.occurrences++
			w.seen.note(limitOccurrencesPerDocument, d.occurrences)
			if w.onOccurrence == nil {
				if err := r.discardSub(n); err != nil {
					return err
				}
				continue
			}
			if err := w.record(ctx, r, n, "occurrence record"); err != nil {
				return err
			}
			o, dropped, err := w.decodeOccurrence(w.buf)
			if err != nil {
				return err
			}
			if dropped {
				// The occurrence's symbol is over its bound: this occurrence
				// publishes nothing, the rest of the document still does.
				continue
			}
			if err := w.onOccurrence(index, seq, o, n); err != nil {
				return err
			}
			seq++
		case fieldDocumentSymbols:
			if w.onSymbol == nil {
				if err := r.discardSub(n); err != nil {
					return err
				}
				continue
			}
			if err := w.record(ctx, r, n, "symbol record"); err != nil {
				return err
			}
			s, dropped, err := decodeSymbolInfo(w.buf, w.drops)
			if err != nil {
				return err
			}
			if dropped {
				continue
			}
			if err := w.onSymbol(index, s, n); err != nil {
				return err
			}
		default:
			if err := r.discardSub(n); err != nil {
				return err
			}
		}
	}
	if d.path == "" {
		return malformed("document " + strconv.FormatInt(index, 10) + " has no relative_path")
	}
	if d.hasText {
		d.textHash = hex.EncodeToString(hasher.Sum(nil))
	}
	if w.onDocument == nil {
		return nil
	}
	return w.onDocument(d)
}

// decodeOccurrence decodes one bounded Occurrence record. The deprecated
// packed range fields and the typed single/multi-line ranges are both
// accepted; a record carrying neither has no range and is malformed.
//
// A record may carry both forms. The typed range wins whatever order the
// fields arrive in: which coordinates this provider converts must not depend
// on a writer's field order, or the same index would yield different evidence
// bytes from one encoder to the next. Both forms are still consumed, so the
// record stays in sync.
// The second result reports that the occurrence was DROPPED: its symbol -- the
// only field that identifies what the occurrence is about -- was over its
// bound, so there is nothing to publish. The index is not malformed and the
// walk continues.
func (w *walker) decodeOccurrence(buf []byte) (occurrence, bool, error) {
	r := newReader(bytes.NewReader(buf), int64(len(buf)))
	var o occurrence
	var legacyRange, legacyEnclosing []int32
	var rangeTyped, enclosingTyped, enclosingSeen bool
	for !r.done() {
		field, wt, err := r.tag()
		if err != nil {
			return o, false, err
		}
		switch field {
		case fieldOccurrenceRange:
			if legacyRange, err = r.ints(wt, legacyRange[:0], 4); err != nil {
				return o, false, err
			}
		case fieldOccurrenceEnclosingRange:
			enclosingSeen = true
			if legacyEnclosing, err = r.ints(wt, legacyEnclosing[:0], 4); err != nil {
				return o, false, err
			}
		case fieldOccurrenceSingleRange, fieldOccurrenceMultiRange, fieldOccurrenceSingleEnclosing, fieldOccurrenceMultiEnclosing:
			if wt != wireBytes {
				return o, false, malformed("typed range has an unexpected wire type")
			}
			n, err := r.length()
			if err != nil {
				return o, false, err
			}
			body, err := r.sub(n)
			if err != nil {
				return o, false, err
			}
			vals, err := decodeTypedRange(body, field == fieldOccurrenceSingleRange || field == fieldOccurrenceSingleEnclosing)
			if err != nil {
				return o, false, err
			}
			if field == fieldOccurrenceSingleRange || field == fieldOccurrenceMultiRange {
				o.rng, rangeTyped = vals, true
			} else {
				o.enclosing, enclosingTyped, enclosingSeen = vals, true, true
			}
		case fieldOccurrenceSymbol:
			if wt != wireBytes {
				return o, false, malformed("occurrence symbol has an unexpected wire type")
			}
			var dropped bool
			if o.symbol, w.strs, _, dropped, err = r.strBounded(model.MaxNativeKeyBytes, boundOccurrenceSymbol, w.strs); err != nil {
				return o, false, err
			} else if dropped {
				w.drops.dropRecord(boundOccurrenceSymbol)
				return o, true, nil
			}
		case fieldOccurrenceRoles:
			if wt != wireVarint {
				return o, false, malformed("occurrence roles have an unexpected wire type")
			}
			v, err := r.varint()
			if err != nil {
				return o, false, err
			}
			o.roles = int32(v)
		default:
			if err := r.skip(wt); err != nil {
				return o, false, err
			}
		}
	}
	if !rangeTyped {
		o.rng = legacyRange
	}
	if !enclosingTyped {
		o.enclosing = legacyEnclosing
	}
	o.hasEnclosing = enclosingSeen
	if len(o.rng) != 3 && len(o.rng) != 4 {
		return o, false, malformed("occurrence range has " + strconv.Itoa(len(o.rng)) + " values, want 3 or 4")
	}
	if o.hasEnclosing && len(o.enclosing) != 3 && len(o.enclosing) != 4 {
		return o, false, malformed("enclosing range has " + strconv.Itoa(len(o.enclosing)) + " values, want 3 or 4")
	}
	return o, false, nil
}

// decodeTypedRange reads a SingleLineRange {line, start, end} or a
// MultiLineRange {start_line, start_char, end_line, end_char} into the
// deprecated flat form so one conversion path serves both.
func decodeTypedRange(r *reader, single bool) ([]int32, error) {
	vals := make([]int32, 4)
	for !r.done() {
		field, wt, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wt != wireVarint || field < 1 || field > 4 {
			if err := r.skip(wt); err != nil {
				return nil, err
			}
			continue
		}
		v, err := r.varint()
		if err != nil {
			return nil, err
		}
		vals[field-1] = int32(v)
	}
	if single {
		return vals[:3], nil
	}
	return vals, nil
}

// decodeSymbolInfo decodes one bounded SymbolInformation record: symbol,
// kind, display name, signature text and relationships. Documentation is
// skipped; it is not a fact this provider publishes.
// The second result reports that the record was DROPPED: its `symbol` was over
// the native-key bound, so the record cannot be published under any identity.
func decodeSymbolInfo(buf []byte, drops *decodeDrops) (symbolInfo, bool, error) {
	r := newReader(bytes.NewReader(buf), int64(len(buf)))
	var s symbolInfo
	var scratch []byte
	for !r.done() {
		field, wt, err := r.tag()
		if err != nil {
			return s, false, err
		}
		switch {
		case field == fieldSymbolInfoKind && wt == wireVarint:
			v, err := r.varint()
			if err != nil {
				return s, false, err
			}
			s.kind = int32(v)
		case field == fieldSymbolInfoSymbol && wt == wireBytes:
			var dropped bool
			if s.symbol, scratch, _, dropped, err = r.strBounded(model.MaxNativeKeyBytes, boundSymbol, scratch); err != nil {
				return s, false, err
			} else if dropped {
				drops.dropRecord(boundSymbol)
				return s, true, nil
			}
		case field == fieldSymbolInfoDisplayName && wt == wireBytes:
			// Decorative: the record keeps its identity without it.
			var dropped bool
			if s.displayName, scratch, _, dropped, err = r.strBounded(model.MaxNameBytes, boundDisplayName, scratch); err != nil {
				return s, false, err
			} else if dropped {
				drops.dropField(boundDisplayName)
			}
		case field == fieldSymbolInfoSignature && wt == wireBytes:
			n, err := r.length()
			if err != nil {
				return s, false, err
			}
			sig, err := r.sub(n)
			if err != nil {
				return s, false, err
			}
			if s.signature, s.signatureCut, err = decodeSignature(sig); err != nil {
				return s, false, err
			} else if s.signatureCut > 0 {
				drops.dropField(boundSignature)
			}
		case field == fieldSymbolInfoRelationships && wt == wireBytes:
			n, err := r.length()
			if err != nil {
				return s, false, err
			}
			body, err := r.sub(n)
			if err != nil {
				return s, false, err
			}
			// The class-B pre-allocation bound stays; what changes is that the
			// relationships past it are discarded and counted rather than
			// aborting the import of a deep type hierarchy.
			if len(s.relationships) >= maxRelationships {
				if err := body.discard(); err != nil {
					return s, false, err
				}
				drops.dropField(boundRelationships)
				continue
			}
			rel, dropped, err := decodeRelationship(body, drops)
			if err != nil {
				return s, false, err
			}
			if dropped {
				continue
			}
			s.relationships = append(s.relationships, rel)
		default:
			if err := r.skip(wt); err != nil {
				return s, false, err
			}
		}
	}
	if s.symbol == "" {
		return s, false, malformed("symbol information has no symbol")
	}
	return s, false, nil
}

// decodeSignature reads Signature.text, bounded at the signature ceiling. A
// longer signature is TRUNCATED and flagged, not discarded: a 4 096-byte
// prefix of a signature answers most questions about the symbol, and this
// producer yielding nothing where treesitter/facts.go:220 yields a prefix was
// the whole of F26. Only the prefix is ever read, so a signature of any size
// costs the ceiling and not itself.
//
// The second result is the ORIGINAL encoded length when the text was cut, and
// 0 when it was not; the caller records it on the symbol and counts one field
// truncation.
func decodeSignature(r *reader) (string, int64, error) {
	var text string
	var cut int64
	for !r.done() {
		field, wt, err := r.tag()
		if err != nil {
			return "", 0, err
		}
		if field != fieldSignatureText || wt != wireBytes {
			if err := r.skip(wt); err != nil {
				return "", 0, err
			}
			continue
		}
		n, err := r.length()
		if err != nil {
			return "", 0, err
		}
		keep := n
		if max := int64(model.MaxSignatureBytes); n > max {
			keep, cut = max, n
		}
		buf, err := r.bytes(keep, keep, boundSignature, nil)
		if err != nil {
			return "", 0, err
		}
		if keep < n {
			if err := r.discardSub(n - keep); err != nil {
				return "", 0, err
			}
		}
		// The prefix is cut at a byte offset, so the last rune of it may be
		// half a UTF-8 sequence; TruncateField is what removes it.
		text, _ = model.TruncateField(string(buf), model.MaxSignatureBytes)
	}
	return text, cut, nil
}

// The second result reports that the relationship was DROPPED: its symbol was
// over the native-key bound, so it names no target.
func decodeRelationship(r *reader, drops *decodeDrops) (relationship, bool, error) {
	var rel relationship
	var scratch []byte
	for !r.done() {
		field, wt, err := r.tag()
		if err != nil {
			return rel, false, err
		}
		switch {
		case field == fieldRelationshipSymbol && wt == wireBytes:
			var dropped bool
			if rel.symbol, scratch, _, dropped, err = r.strBounded(model.MaxNativeKeyBytes, boundRelationshipSymbol, scratch); err != nil {
				return rel, false, err
			} else if dropped {
				drops.dropRecord(boundRelationshipSymbol)
				if err := r.discard(); err != nil {
					return rel, false, err
				}
				return rel, true, nil
			}
		case wt == wireVarint && field >= fieldRelationshipIsReference && field <= fieldRelationshipIsDefinition:
			v, err := r.varint()
			if err != nil {
				return rel, false, err
			}
			if v != 0 {
				rel.flags |= 1 << (field - fieldRelationshipIsReference)
			}
		default:
			if err := r.skip(wt); err != nil {
				return rel, false, err
			}
		}
	}
	if rel.symbol == "" {
		return rel, false, malformed("relationship has no symbol")
	}
	return rel, false, nil
}

// decodeMetadata reads the tool identity and text encoding of the index.
func decodeMetadata(buf []byte) (metadata, error) {
	r := newReader(bytes.NewReader(buf), int64(len(buf)))
	var m metadata
	var scratch []byte
	for !r.done() {
		field, wt, err := r.tag()
		if err != nil {
			return m, err
		}
		switch {
		case field == fieldMetadataTextEncoding && wt == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			m.textEncoding = int32(v)
		case field == fieldMetadataProjectRoot && wt == wireBytes:
			if m.projectRoot, scratch, err = r.str(model.MaxPathBytes, "project root", scratch); err != nil {
				return m, err
			}
		case field == fieldMetadataToolInfo && wt == wireBytes:
			n, err := r.length()
			if err != nil {
				return m, err
			}
			sub, err := r.sub(n)
			if err != nil {
				return m, err
			}
			for !sub.done() {
				f, w, err := sub.tag()
				if err != nil {
					return m, err
				}
				switch {
				case f == fieldToolInfoName && w == wireBytes:
					if m.toolName, scratch, err = sub.str(model.MaxIdentifierBytes, "tool name", scratch); err != nil {
						return m, err
					}
				case f == fieldToolInfoVersion && w == wireBytes:
					if m.toolVersion, scratch, err = sub.str(model.MaxIdentifierBytes, "tool version", scratch); err != nil {
						return m, err
					}
				default:
					if err := sub.skip(w); err != nil {
						return m, err
					}
				}
			}
		default:
			if err := r.skip(wt); err != nil {
				return m, err
			}
		}
	}
	return m, nil
}
