package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
)

// Scope keys. A filesystem or manifest unit is one file (R7-1): its scope is
// "file:"+path and its only input is that file's version, so an unchanged
// file reuses its unit with zero work.
const scopePrefix = "file:"

// ScopeKey is the unit scope for one root-relative path.
func ScopeKey(rel string) string { return scopePrefix + rel }

// PathFromScope recovers the path a file unit was assigned. A scope that is
// not a file scope is a coordinator error, not provider input.
func PathFromScope(scopeKey string) (string, error) {
	rel, ok := strings.CutPrefix(scopeKey, scopePrefix)
	if !ok || rel == "" || rel != path.Clean(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") || rel == ".." {
		return "", &model.Error{Code: model.CodeArgumentInvalid,
			Message: "unit scope is not a normalized file scope", Details: map[string]string{"scope_key": scopeKey}}
	}
	return rel, nil
}

// Native keys of path-identified nodes (R7-2). They are the alias keys a
// dependent unit resolves with, at scope provider.ScopeWorkspace.
const (
	nativeRepository = "repo:"
	nativeDirectory  = "dir:"
	nativeFile       = "file:"
)

// PathCandidate is the candidate for a node identified by a path alone:
// the repository (path "."), a directory, a file, or the document /
// configuration / build_target a recognized file defines. Identity is a
// structural key over the workspace scope, the path and the kind, so every
// unit that mentions the path mints the same node and storage deduplicates
// the identity (R7-2). The candidate deliberately carries no FileID: a unit
// may only name its own inputs, and a document that links to another file
// must still resolve that file's identity.
func PathCandidate(providerID string, kind model.NodeKind, rel string) model.NodeCandidate {
	var prefix string
	switch kind {
	case model.NodeRepository:
		prefix, rel = nativeRepository, "."
	case model.NodeDirectory:
		prefix = nativeDirectory
	case model.NodeFile:
		prefix = nativeFile
	default:
		prefix = string(kind) + ":"
	}
	name := path.Base(rel)
	lang := ""
	if kind != model.NodeRepository && kind != model.NodeDirectory {
		lang = Language(rel)
	}
	return model.NodeCandidate{ProviderID: providerID, ScopeKey: provider.ScopeWorkspace, NativeKey: prefix + rel,
		Kind: kind, Language: lang, Name: name, QualifiedName: rel}
}

// Attrs are the unit-owned attributes and evidence of one fact.
type Attrs struct {
	Precision model.Precision
	// Range is the evidence range in the unit's file, when extractable.
	Range *model.SourceRange
	// Detail is bounded evidence detail, normally a small JSON object.
	Detail string
	// Metadata is the node's strictly typed JSON metadata object.
	Metadata json.RawMessage
	// Located marks a node whose attributes bind it to the unit's file: the
	// node fact carries the file, content hash and Range. Identity is
	// unaffected; attribute ownership stays with the publishing unit.
	Located bool
}

// Emitter builds one unit's facts in the reference order storage requires:
// node facts before any relation, alias or search document that references
// them. It deduplicates node and relation facts within the unit (their
// storage keys are per unit) and merges the evidence of a relation asserted
// more than once, so a dependency listed under two kinds keeps both
// occurrences. Everything it holds is bounded by the caller's caps; Flush
// hands the records to the sink and clears them.
type Emitter struct {
	req  provider.UnitRequest
	sink provider.Sink
	file model.FileVersion

	nodes      []model.NodeFact
	seenNode   map[model.NodeID]int
	relations  []model.RelationFact
	seenRel    map[model.RelationID]int
	aliases    []model.NativeAlias
	search     []model.SearchUnit
	seenSearch map[string]bool

	nodesFlushed bool
	records      uint64
	bytes        uint64
	states       []model.CapabilityState

	// clipped counts evidence occurrences dropped because the fact already
	// carries model.MaxEvidencePerFact of them. The fact itself is still
	// published; the count is what keeps that truncation from being silent,
	// and Result publishes it on every capability state of the unit.
	clipped int
}

// NewEmitter binds an emitter to the unit and the file it indexes.
func NewEmitter(req provider.UnitRequest, sink provider.Sink, file model.FileVersion) *Emitter {
	return &Emitter{req: req, sink: sink, file: file, seenNode: map[model.NodeID]int{}, seenRel: map[model.RelationID]int{}, seenSearch: map[string]bool{}}
}

// File is the unit's input file.
func (e *Emitter) File() model.FileVersion { return e.file }

// Node resolves cand and queues its fact, alias and, for an ambiguous
// resolution, may_refer_to edges. A node already queued in this unit gains
// the new evidence instead of a second fact. It returns the canonical node.
func (e *Emitter) Node(ctx context.Context, cand model.NodeCandidate, a Attrs) (model.Node, error) {
	if e.nodesFlushed {
		return model.Node{}, &model.Error{Code: model.CodeInternal, Message: "a node fact was queued after the unit's nodes were flushed"}
	}
	res, err := e.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return model.Node{}, err
	}
	node := res.Node
	if a.Located {
		node.FileID, node.ContentHash, node.Range = e.file.ID, e.file.ContentHash, a.Range
	}
	if res.Basis == model.MatchUnresolved {
		node.Metadata = mergeMetadata(a.Metadata, "resolution", "unresolved")
	} else {
		node.Metadata = a.Metadata
	}
	ev := e.evidence(node.ID, "", a)
	if i, dup := e.seenNode[node.ID]; dup {
		e.nodes[i].Evidence = e.appendEvidence(e.nodes[i].Evidence, ev)
		return node, nil
	}
	e.seenNode[node.ID] = len(e.nodes)
	e.nodes = append(e.nodes, model.NodeFact{Node: node, CanonicalKey: res.CanonicalKey, Evidence: []model.Evidence{ev}})
	e.aliases = append(e.aliases, model.NativeAlias{ScopeKey: cand.ScopeKey, NativeKey: cand.NativeKey, NodeID: node.ID})
	for _, other := range res.Ambiguous {
		if err := e.Relation(node.ID, model.RelMayReferTo, other, Attrs{Precision: a.Precision, Range: a.Range}); err != nil {
			return model.Node{}, err
		}
	}
	return node, nil
}

// Relation queues one canonical edge with one occurrence of evidence. Both
// endpoints must be node facts of this unit or of a sealed dependency.
func (e *Emitter) Relation(from model.NodeID, kind model.RelationKind, to model.NodeID, a Attrs) error {
	if e.nodesFlushed {
		return &model.Error{Code: model.CodeInternal, Message: "a relation was queued after the unit's facts were flushed"}
	}
	rel := model.Relation{ID: model.NewRelationID(e.req.Binding.RepositoryID, from, kind, to), From: from, Kind: kind, To: to}
	ev := e.evidence("", rel.ID, a)
	if i, dup := e.seenRel[rel.ID]; dup {
		e.relations[i].Evidence = e.appendEvidence(e.relations[i].Evidence, ev)
		return nil
	}
	e.seenRel[rel.ID] = len(e.relations)
	e.relations = append(e.relations, model.RelationFact{Relation: rel, Evidence: []model.Evidence{ev}})
	return nil
}

// Symbol queues a name-only search document for a node of this unit: names
// and signature, never a body (Section 11.2). rng is the node's source range,
// or nil when the parser could not place the node; model.SearchUnit.Bytes is
// a required value field, so "no range" is written as the empty range [0,0)
// and consumers must read it as "this document has no byte range" rather than
// as the first zero bytes of the file.
func (e *Emitter) Symbol(node model.Node, rng *model.SourceRange) {
	doc := model.SearchUnit{NodeID: node.ID, FileID: e.file.ID, Path: e.file.Path, Kind: node.Kind,
		Name: node.Name, QualifiedName: node.QualifiedName, Signature: node.Signature}
	if rng != nil {
		doc.Bytes = model.ByteRange{Start: rng.Start.Byte, End: rng.End.Byte}
	}
	doc.ID = model.H("search-symbol-v1", string(node.ID), string(e.file.ID), e.file.ContentHash)
	e.Search(doc)
}

// Search queues a caller-built search document. The unit's search keys are
// unique, so a document already queued under the same key is not repeated.
func (e *Emitter) Search(doc model.SearchUnit) {
	if e.seenSearch[doc.ID] {
		return
	}
	e.seenSearch[doc.ID] = true
	e.search = append(e.search, doc)
}

// Flush hands every queued record to the sink in reference order and clears
// the queues. After the first Flush no further node facts may be queued, so
// a later relation or search document can never precede its node.
func (e *Emitter) Flush(ctx context.Context) error {
	if len(e.nodes) > 0 {
		if err := e.sink.PutNodes(ctx, e.nodes); err != nil {
			return err
		}
		e.records += uint64(len(e.nodes))
		e.nodes = nil
	}
	e.nodesFlushed = true
	if len(e.relations) > 0 {
		if err := e.sink.PutRelations(ctx, e.relations); err != nil {
			return err
		}
		e.records += uint64(len(e.relations))
		e.relations = nil
	}
	if len(e.aliases) > 0 {
		if err := e.sink.PutAliases(ctx, e.aliases); err != nil {
			return err
		}
		e.records += uint64(len(e.aliases))
		e.aliases = nil
	}
	if len(e.search) > 0 {
		if err := e.sink.PutSearchUnits(ctx, e.search); err != nil {
			return err
		}
		e.records += uint64(len(e.search))
		e.search = nil
	}
	return nil
}

// PutSearch hands one search document straight to the sink. It is for the
// streamed source chunks, whose node fact has already been flushed.
func (e *Emitter) PutSearch(ctx context.Context, doc model.SearchUnit) error {
	if !e.nodesFlushed {
		return &model.Error{Code: model.CodeInternal, Message: "a search document was put before the unit's nodes were flushed"}
	}
	if err := e.sink.PutSearchUnits(ctx, []model.SearchUnit{doc}); err != nil {
		return err
	}
	e.records++
	return nil
}

// AddBytes counts input bytes the provider processed.
func (e *Emitter) AddBytes(n uint64) { e.bytes += n }

// Capability records the state of one capability at this unit's scope. A
// later report for the same capability replaces the earlier one, so a
// provider may claim fresh first and downgrade when it learns more.
func (e *Emitter) Capability(capability string, state model.CapabilityStateValue, code string) {
	cs := model.CapabilityState{ProviderID: e.req.Unit.ProviderID, Capability: capability, Scope: e.req.Unit.ScopeKey, State: state, DiagnosticCode: code}
	for i := range e.states {
		if e.states[i].Capability == capability {
			e.states[i] = cs
			return
		}
	}
	e.states = append(e.states, cs)
}

// Result is the succeeded result for this unit with its counters and
// per-file capability states.
func (e *Emitter) Result() model.ProviderResult {
	states := e.states
	if e.clipped > 0 {
		// Occurrences were cut by the per-fact evidence bound. Every
		// capability this unit publishes carries the count, so the answer
		// built from these facts can say how much evidence it is missing.
		states = make([]model.CapabilityState, len(e.states))
		for i, cs := range e.states {
			states[i] = cs.WithDetail("evidence_clipped", strconv.Itoa(e.clipped))
		}
	}
	return model.ProviderResult{RunID: e.req.Run, State: model.RunSucceeded, Capabilities: states, RecordsEmitted: e.records, BytesProcessed: e.bytes}
}

// evidence builds one evidence row for this unit and run. Every row binds
// to the unit's file; a range is present only when the caller extracted one.
func (e *Emitter) evidence(node model.NodeID, rel model.RelationID, a Attrs) model.Evidence {
	ev := model.Evidence{UnitID: e.req.Unit.ID, ProviderID: e.req.Unit.ProviderID, ProviderVersion: e.req.Unit.ProviderVersion,
		OriginRunID: e.req.Run, NodeID: node, RelationID: rel, Precision: a.Precision,
		FileID: e.file.ID, ContentHash: e.file.ContentHash, Range: a.Range, Detail: a.Detail}
	ev.ID = model.NewEvidenceID(ev)
	return ev
}

// appendEvidence adds ev unless an identical occurrence is already present
// or the fact's evidence is at its bound. A duplicate is not a loss and is
// not counted; an occurrence cut by the bound is, so the unit reports how
// many occurrences it did not store instead of dropping them in silence.
func (e *Emitter) appendEvidence(list []model.Evidence, ev model.Evidence) []model.Evidence {
	for _, have := range list {
		if have.ID == ev.ID {
			return list
		}
	}
	if len(list) >= model.MaxEvidencePerFact {
		e.clipped++
		return list
	}
	return append(list, ev)
}

// Metadata renders a strictly typed metadata object with sorted keys. A
// payload over the bound is replaced by one that says so rather than
// truncated into invalid JSON.
func Metadata(fields map[string]any) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	raw := marshal(fields)
	if raw == nil || len(raw) > model.MaxMetadataBytes {
		raw = marshal(map[string]any{"truncated": true})
	}
	return raw
}

// marshal renders JSON with sorted keys and without HTML escaping, so a
// requirement such as ">=2" is stored as written.
func marshal(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Detail renders bounded evidence detail as a small JSON object from
// alternating key/value pairs. Values are cut so the whole stays under the
// detail bound; the cut never splits a rune.
func Detail(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	fields := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			fields[kv[i]] = kv[i+1]
		}
	}
	if len(fields) == 0 {
		return ""
	}
	raw := marshal(fields)
	if len(raw) <= model.MaxDetailBytes {
		return string(raw)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	budget := max((model.MaxDetailBytes-64)/len(keys), 1)
	for _, k := range keys {
		fields[k] = cut(fields[k], budget)
	}
	return string(marshal(fields))
}

// cut shortens s to at most limit bytes on a rune boundary.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && !isRuneStart(s[i]) {
		i--
	}
	return s[:i]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// mergeMetadata sets one string field on a metadata object.
func mergeMetadata(raw json.RawMessage, key, value string) json.RawMessage {
	fields := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &fields)
	}
	fields[key] = value
	return Metadata(fields)
}
