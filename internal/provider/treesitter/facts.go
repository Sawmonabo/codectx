package treesitter

import (
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/source"
)

// Fact bounds beyond the model's. A doc body is kept well under the per-record
// cap so a declaration's search document never approaches it; evidence per
// fact is the model bound, and occurrences past it are counted, not stored.
const (
	maxDocBytes    = 8 << 10
	capabilityName = "structure"
	searchDomain   = "treesitter-search-v1"
	// maxUnresolvedCallees bounds the distinct cross-file callees one file may
	// mint a placeholder for. A generated or minified file can hold tens of
	// thousands of distinct unresolved names, each of which would be a node, a
	// relation and evidence; past the bound the call is counted and the file's
	// structural coverage is reported partial instead.
	maxUnresolvedCallees = 2000
	// maxPutRecords bounds one hand-off to the sink, so a unit's facts stream
	// through it in bounded slices rather than one slice the size of the whole
	// file's output. provider.Sink does not expose the sink's Limits, so this
	// matches the default index.batch_records.
	maxPutRecords = 1000
)

// declKinds is the vocabulary the worker may report; anything else is a
// protocol violation, never a node of a kind this provider does not extract.
var declKinds = map[string]model.NodeKind{
	"function": model.NodeFunction, "method": model.NodeMethod, "class": model.NodeClass,
	"interface": model.NodeInterface, "struct": model.NodeStruct, "enum": model.NodeEnum,
	"field": model.NodeField, "variable": model.NodeVariable, "constant": model.NodeConstant,
	"test": model.NodeTest, "module": model.NodeModule, "namespace": model.NodeNamespace,
}

// builder turns one file's validated extraction into facts. Every byte range
// it publishes is derived from the pinned bytes with source.Cursor; nothing
// the worker said is copied without being checked against them.
type builder struct {
	ctx      context.Context
	req      provider.UnitRequest
	fv       model.FileVersion
	lang     lang.Language
	src      []byte
	cur      *source.Cursor
	ex       *extraction
	scope    string
	fileRng  *model.SourceRange
	decls    []declFact
	byName   map[string][]int
	module   model.Resolution
	nodes    []model.NodeFact
	nodeAt   map[model.NodeID]int
	rels     map[model.RelationID]*model.RelationFact
	relOrder []model.RelationID
	aliases  []model.NativeAlias
	search   []model.SearchUnit
	// dropped counts what this file's bounds kept out of the facts:
	// occurrences past the per-fact evidence bound and calls past the
	// unresolved-callee bound. Any of it makes the file's coverage partial.
	dropped int
}

// declFact is one validated declaration and its resolved identity.
type declFact struct {
	wire.Decl
	kind model.NodeKind
	rng  *model.SourceRange
	sig  string
	doc  string
	res  model.Resolution
}

// outputInvalid is the parent's verdict on a fact the worker sent that does
// not describe the pinned bytes.
func outputInvalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: "parser worker: " + msg, Details: map[string]string{"provider_id": lang.ProviderID}}
}

// build validates the extraction and resolves and assembles every fact. It
// returns the facts in put order: every node before any relation, alias or
// search document that references it.
func (b *builder) build() error {
	b.scope = ScopePrefix + b.fv.Path
	b.rels = map[model.RelationID]*model.RelationFact{}
	b.nodeAt = map[model.NodeID]int{}
	b.byName = map[string][]int{}
	var err error
	if b.fileRng, err = b.rangeOf(0, uint32(len(b.src))); err != nil {
		return err
	}
	if err := b.validateDecls(); err != nil {
		return err
	}
	if err := b.resolveModule(); err != nil {
		return err
	}
	if err := b.resolveDecls(); err != nil {
		return err
	}
	if err := b.imports(); err != nil {
		return err
	}
	return b.refs()
}

// rangeOf derives a located range from two byte offsets the worker reported.
func (b *builder) rangeOf(start, end uint32) (*model.SourceRange, error) {
	if uint64(end) > uint64(len(b.src)) || start > end {
		return nil, outputInvalid("range [" + strconv.Itoa(int(start)) + "," + strconv.Itoa(int(end)) + ") does not fit the " + strconv.Itoa(len(b.src)) + " pinned bytes")
	}
	s, err := b.cur.PositionAt(uint64(start))
	if err != nil {
		return nil, outputInvalid(err.Error())
	}
	e, err := b.cur.PositionAt(uint64(end))
	if err != nil {
		return nil, outputInvalid(err.Error())
	}
	return &model.SourceRange{Start: s, End: e}, nil
}

func (b *builder) validateDecls() error {
	b.decls = make([]declFact, 0, len(b.ex.decls))
	for i, d := range b.ex.decls {
		kind, ok := declKinds[d.Kind]
		if !ok {
			return outputInvalid("declaration kind " + bound(d.Kind, 32) + " is not one this provider extracts")
		}
		if d.ID != i {
			return outputInvalid("declarations are not numbered in order")
		}
		if d.Parent < -1 || d.Parent >= i {
			return outputInvalid("declaration names a parent that does not precede it")
		}
		if d.Name == "" || len(d.Name) > model.MaxNameBytes || !utf8.ValidString(d.Name) ||
			len(d.Qualified) > model.MaxQualifiedNameBytes || !utf8.ValidString(d.Qualified) ||
			len(d.Impl) > model.MaxNameBytes {
			return outputInvalid("declaration name is empty, over its bound or not UTF-8")
		}
		if d.Qualified == "" {
			d.Qualified = d.Name
		}
		if d.SigEnd < d.Start || d.SigEnd > d.End {
			return outputInvalid("declaration signature end lies outside the declaration")
		}
		rng, err := b.rangeOf(d.Start, d.End)
		if err != nil {
			return err
		}
		if d.Parent >= 0 {
			p := b.decls[d.Parent]
			if d.Start < p.Start || d.End > p.End {
				return outputInvalid("nested declaration lies outside its parent")
			}
		}
		// The signature and documentation offsets are cut from the pinned
		// bytes, so they go through rangeOf like every other offset: it is
		// what rejects an offset inside a UTF-8 sequence, which would
		// otherwise put a broken rune in a Signature or a Body.
		if _, err := b.rangeOf(d.Start, d.SigEnd); err != nil {
			return err
		}
		f := declFact{Decl: d, kind: kind, rng: rng, sig: collapse(string(b.src[d.Start:d.SigEnd]), model.MaxSignatureBytes)}
		if d.DocEnd > 0 {
			if _, err := b.rangeOf(d.DocStart, d.DocEnd); err != nil {
				return err
			}
			f.doc = cleanDoc(string(b.src[d.DocStart:d.DocEnd]))
		}
		b.decls = append(b.decls, f)
		b.byName[d.Name] = append(b.byName[d.Name], i)
	}
	return nil
}

// resolveModule mints the file's module node: the container every top-level
// declaration is defined by and the subject of imports and exports.
func (b *builder) resolveModule() error {
	res, err := b.resolve(model.NodeCandidate{
		ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: "module:" + b.fv.Path,
		Kind: model.NodeModule, Language: b.lang.Name, Name: path.Base(b.fv.Path), QualifiedName: b.fv.Path,
		FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: b.fileRng,
	})
	if err != nil {
		return err
	}
	b.module = res
	meta := map[string]any{"path": b.fv.Path}
	if b.ex.done.Package != "" {
		meta["package"] = bound(b.ex.done.Package, model.MaxNameBytes)
	}
	b.putNode(res, b.fileRng, "module:"+b.fv.Path, meta)
	b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: "module:" + b.fv.Path, NodeID: res.Node.ID})
	return nil
}

// resolveDecls resolves every declaration, publishes its node, containment,
// export, aliases and search document.
func (b *builder) resolveDecls() error {
	for i := range b.decls {
		d := &b.decls[i]
		res, err := b.resolve(model.NodeCandidate{
			ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: d.Qualified, Kind: d.kind, Language: b.lang.Name,
			Name: d.Name, QualifiedName: d.Qualified, Signature: d.sig,
			FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: d.rng,
		})
		if err != nil {
			return err
		}
		d.res = res
		meta := map[string]any{}
		if d.Exported {
			meta["exported"] = true
		}
		if d.Prototype {
			meta["prototype"] = true
		}
		if d.Macro {
			meta["macro"] = true
		}
		if d.Impl != "" {
			meta["receiver"] = d.Impl
		}
		b.putNode(res, d.rng, d.Qualified, meta)
		b.ambiguous(res, d.rng)

		parent := b.module.Node.ID
		kind := model.RelDefines
		if d.Parent >= 0 {
			parent, kind = b.decls[d.Parent].res.Node.ID, model.RelContains
		}
		b.putRelation(parent, kind, res.Node.ID, d.rng, d.Qualified, "")
		if d.Exported && d.Parent < 0 {
			b.putRelation(b.module.Node.ID, model.RelExports, res.Node.ID, d.rng, d.Qualified, "")
		}
		b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: d.Qualified, NodeID: res.Node.ID})
		if key := b.declKey(d); key != "" {
			b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: key, NodeID: res.Node.ID})
		}
		if pkg := b.packageScope(); pkg != "" && d.Parent < 0 {
			b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: pkg, NativeKey: d.Qualified, NodeID: res.Node.ID})
		}
		b.search = append(b.search, b.searchUnit(d))
	}
	return nil
}

// declKey is the cross-provider declaration key of the controller's ruling,
// the one string a semantic provider and this one both compute for the same
// declaration:
//
//	scope  "file:" + path
//	key    "decl:" + <identifier token as written> + "@" + path + ":" + <first line> + "-" + <last line>
//
// Lines are one-based and the span is inclusive, so the last line is the line
// the declaration's final byte lies on, not the half-open end position's.
// Nothing is normalized: the identifier is the token the source spells, the
// path is the same root-relative slash path the unit's scope key carries.
// Joern's exported METHOD key is byte-for-byte this string, so publishing it
// as an alias is what merges the two providers' identities for one function
// instead of leaving two correct but unrelated nodes.
//
// A key over MaxNativeKeyBytes is omitted rather than truncated: a truncated
// key would be a different, possibly colliding identity claim. The
// declaration keeps its own identity and its qualified-name alias either way.
func (b *builder) declKey(d *declFact) string {
	endLine := d.rng.End.Line
	if d.End > d.Start && b.src[d.End-1] == '\n' {
		// The half-open end sits at the start of the next line; the
		// declaration's last line is the one before it.
		endLine--
	}
	key := "decl:" + d.Name + "@" + b.fv.Path + ":" + strconv.FormatUint(uint64(d.rng.Start.Line), 10) + "-" + strconv.FormatUint(uint64(endLine), 10)
	if len(key) > model.MaxNativeKeyBytes {
		return ""
	}
	return key
}

// packageScope is the language's package-level alias scope for top-level
// declarations, when the language has a declared package clause whose
// members are visible across files without an import: Go and Java.
func (b *builder) packageScope() string {
	pkg := b.ex.done.Package
	if pkg == "" || len(pkg) > model.MaxNameBytes {
		return ""
	}
	switch b.lang.Name {
	case "go":
		return "pkg:go:" + path.Dir(b.fv.Path) + ":" + pkg
	case "java":
		return "pkg:java:" + pkg
	}
	return ""
}

// imports publishes one module node per import target (a structural
// identity: the same import path from two files mints the same node) and an
// imports relation from this file's module.
func (b *builder) imports() error {
	for _, imp := range b.ex.imports {
		if imp.Path == "" || len(imp.Path) > model.MaxQualifiedNameBytes || !utf8.ValidString(imp.Path) {
			return outputInvalid("import path is empty, over its bound or not UTF-8")
		}
		rng, err := b.rangeOf(imp.Start, imp.End)
		if err != nil {
			return err
		}
		name := lastPathSegment(imp.Path)
		res, err := b.resolve(model.NodeCandidate{
			ProviderID: lang.ProviderID, ScopeKey: provider.ScopeWorkspace, NativeKey: "import:" + b.lang.Name + ":" + imp.Path,
			Kind: model.NodeModule, Language: b.lang.Name, Name: name, QualifiedName: imp.Path,
		})
		if err != nil {
			return err
		}
		b.putNode(res, rng, "import:"+imp.Path, map[string]any{"import_path": imp.Path})
		b.putRelation(b.module.Node.ID, model.RelImports, res.Node.ID, rng, imp.Path, "")
	}
	return nil
}

// refs publishes call sites and type references. Resolution is file-local
// (Section 11.1 precision syntax): a name declared once in this file is the
// target; a name declared several times is retained as may_refer_to edges; a
// name declared nowhere in this file, or qualified by an imported name, is an
// honest unresolved placeholder, never a guess at another file's symbol.
func (b *builder) refs() error {
	unresolved := map[string]model.Resolution{}
	for _, r := range b.ex.refs {
		if r.Kind != "call" && r.Kind != "type" {
			return outputInvalid("reference kind " + bound(r.Kind, 32) + " is unknown")
		}
		if r.Name == "" || len(r.Name) > model.MaxNameBytes || !utf8.ValidString(r.Name) || len(r.Qualifier) > model.MaxNameBytes {
			return outputInvalid("reference name is empty, over its bound or not UTF-8")
		}
		if r.Scope < -1 || r.Scope >= len(b.decls) {
			return outputInvalid("reference names a scope that is not a declaration")
		}
		rng, err := b.rangeOf(r.Start, r.End)
		if err != nil {
			return err
		}
		from := b.module.Node.ID
		if r.Scope >= 0 {
			from = b.decls[r.Scope].res.Node.ID
		}
		targets := b.targets(r)
		if r.Kind == "type" {
			// A type reference to a name this file does not declare is left to a
			// provider that can see the other file; no placeholder is minted.
			for _, t := range targets {
				b.putRelation(from, model.RelReferences, b.decls[t].res.Node.ID, rng, r.Name, "")
			}
			continue
		}
		switch {
		case len(targets) == 1:
			b.putRelation(from, model.RelCalls, b.decls[targets[0]].res.Node.ID, rng, r.Name, "")
		case len(targets) > 1:
			for _, t := range targets[:min(len(targets), model.MaxAmbiguousCandidates)] {
				b.putRelation(from, model.RelMayReferTo, b.decls[t].res.Node.ID, rng, r.Name, "ambiguous call target")
			}
		default:
			key := "call:" + r.Name
			kind := model.NodeFunction
			if r.Qualified {
				key, kind = "call:"+r.Qualifier+"."+r.Name, model.NodeMethod
			}
			res, ok := unresolved[key]
			if !ok {
				if len(unresolved) >= maxUnresolvedCallees {
					// Past the bound the call is counted, not minted: a file
					// with more distinct cross-file callees than this is
					// reported partial rather than allowed to publish an
					// unbounded number of placeholder nodes and relations.
					b.dropped++
					continue
				}
				if res, err = b.resolve(model.NodeCandidate{ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: key,
					Kind: kind, Language: b.lang.Name, Name: r.Name}); err != nil {
					return err
				}
				unresolved[key] = res
				b.putNode(res, rng, key, map[string]any{"resolution": "unresolved", "callee": r.Name})
			} else {
				b.addEvidence(res.Node.ID, rng, key)
			}
			b.putRelation(from, model.RelCalls, res.Node.ID, rng, key, "unresolved")
		}
	}
	return nil
}

// targets are the declarations in this file a reference can name. A
// qualified call (receiver.name, Type::name) names methods; a bare call names
// functions, tests or macros; a type reference names type-like declarations.
// Definitions are preferred over prototypes when both exist. A qualifier that
// is an imported name makes the target cross-file, so nothing here.
func (b *builder) targets(r wire.Ref) []int {
	if r.QualifierIsImport {
		return nil
	}
	var out, protos []int
	for _, i := range b.byName[r.Name] {
		d := &b.decls[i]
		ok := false
		switch r.Kind {
		case "type":
			ok = d.kind == model.NodeClass || d.kind == model.NodeStruct || d.kind == model.NodeInterface || d.kind == model.NodeEnum
		case "call":
			if r.Qualified {
				ok = d.kind == model.NodeMethod
			} else {
				ok = d.kind == model.NodeFunction || d.kind == model.NodeTest || d.kind == model.NodeClass || d.kind == model.NodeStruct
			}
		}
		if !ok {
			continue
		}
		if d.Prototype {
			protos = append(protos, i)
		} else {
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		return protos
	}
	return out
}

// resolve asks the unit's resolver; identity never comes from anywhere else.
func (b *builder) resolve(c model.NodeCandidate) (model.Resolution, error) {
	res, err := b.req.Resolver.Resolve(b.ctx, c)
	if err != nil {
		return model.Resolution{}, err
	}
	return res, nil
}

// evidence builds one evidence row for this unit and run.
func (b *builder) evidence(node model.NodeID, rel model.RelationID, rng *model.SourceRange, nativeKey, detail string) model.Evidence {
	e := model.Evidence{UnitID: b.req.Unit.ID, ProviderID: b.req.Unit.ProviderID, ProviderVersion: b.req.Unit.ProviderVersion,
		OriginRunID: b.req.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionSyntax,
		FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: rng, NativeKey: bound(nativeKey, model.MaxNativeKeyBytes), Detail: detail}
	e.ID = model.NewEvidenceID(e)
	return e
}

func (b *builder) putNode(res model.Resolution, rng *model.SourceRange, nativeKey string, meta map[string]any) {
	node := res.Node
	if len(meta) > 0 {
		if raw, err := json.Marshal(meta); err == nil && len(raw) <= model.MaxMetadataBytes {
			node.Metadata = raw
		}
	}
	b.nodes = append(b.nodes, model.NodeFact{Node: node, CanonicalKey: res.CanonicalKey,
		Evidence: []model.Evidence{b.evidence(node.ID, "", rng, nativeKey, "")}})
	// The index makes a repeat occurrence of an identity a map lookup. Without
	// it a file whose every call is unresolved rescans the published facts per
	// occurrence, which is quadratic in the file's references.
	b.nodeAt[node.ID] = len(b.nodes) - 1
}

// addEvidence records another occurrence of a node this builder published.
func (b *builder) addEvidence(id model.NodeID, rng *model.SourceRange, nativeKey string) {
	i, ok := b.nodeAt[id]
	if !ok {
		return
	}
	if len(b.nodes[i].Evidence) >= model.MaxEvidencePerFact {
		b.dropped++
		return
	}
	b.nodes[i].Evidence = append(b.nodes[i].Evidence, b.evidence(b.nodes[i].Node.ID, "", rng, nativeKey, ""))
}

// ambiguous retains the equally supported identities a resolution reported
// as may_refer_to edges instead of silently picking the first.
func (b *builder) ambiguous(res model.Resolution, rng *model.SourceRange) {
	for _, alt := range res.Ambiguous {
		b.putRelation(res.Node.ID, model.RelMayReferTo, alt, rng, res.CanonicalKey, "ambiguous alias match")
	}
}

// putRelation adds one occurrence of a canonical edge; several occurrences
// share the relation and each carries its own range-bearing evidence.
func (b *builder) putRelation(from model.NodeID, kind model.RelationKind, to model.NodeID, rng *model.SourceRange, nativeKey, detail string) {
	id := model.NewRelationID(b.req.Binding.RepositoryID, from, kind, to)
	f, ok := b.rels[id]
	if !ok {
		f = &model.RelationFact{Relation: model.Relation{ID: id, From: from, Kind: kind, To: to}}
		b.rels[id] = f
		b.relOrder = append(b.relOrder, id)
	}
	if len(f.Evidence) >= model.MaxEvidencePerFact {
		b.dropped++
		return
	}
	f.Evidence = append(f.Evidence, b.evidence("", id, rng, nativeKey, detail))
}

// searchUnit is the declaration's lexical document: names, signature and its
// attached documentation, never the body (Section 11.2 stores bodies once, in
// the filesystem unit).
func (b *builder) searchUnit(d *declFact) model.SearchUnit {
	body := d.sig
	if d.doc != "" {
		body = d.doc + "\n" + d.sig
	}
	body = bound(body, maxDocBytes)
	return model.SearchUnit{
		ID: model.H(searchDomain, string(b.fv.ID), string(d.res.Node.ID)), NodeID: d.res.Node.ID, FileID: b.fv.ID, Path: b.fv.Path,
		Kind: d.res.Node.Kind, Name: d.Name, QualifiedName: d.Qualified, Signature: d.sig,
		Bytes: model.ByteRange{Start: uint64(d.Start), End: uint64(d.End)}, Body: body, TokenCount: int64(len(strings.Fields(body))),
	}
}

// emit hands the facts to the sink in reference order: every node before any
// relation, alias or search document that names it. Each kind streams in
// slices of at most maxPutRecords, and the builder drops its reference to
// each group once it is handed over, so the unit's output is not held in the
// heap a second time while the sink persists it.
func (b *builder) emit(sink provider.Sink) (uint64, error) {
	rels := make([]model.RelationFact, 0, len(b.relOrder))
	for _, id := range b.relOrder {
		rels = append(rels, *b.rels[id])
	}
	b.rels, b.relOrder = nil, nil
	records := uint64(len(b.nodes) + len(rels) + len(b.aliases) + len(b.search))

	nodes, aliases, search := b.nodes, b.aliases, b.search
	b.nodes, b.nodeAt, b.aliases, b.search = nil, nil, nil, nil
	if err := putChunked(b.ctx, nodes, sink.PutNodes); err != nil {
		return 0, err
	}
	if err := putChunked(b.ctx, rels, sink.PutRelations); err != nil {
		return 0, err
	}
	if err := putChunked(b.ctx, aliases, sink.PutAliases); err != nil {
		return 0, err
	}
	if err := putChunked(b.ctx, search, sink.PutSearchUnits); err != nil {
		return 0, err
	}
	return records, nil
}

// putChunked hands items to one Put in bounded slices, keeping their order.
// Each chunk is capped so the sink cannot append into the next chunk's bytes,
// and no chunk is read or written again once it is handed over: ownership of
// a handed-off slice belongs to the sink (Section 11.1).
func putChunked[T any](ctx context.Context, items []T, put func(context.Context, []T) error) error {
	for len(items) > 0 {
		n := min(len(items), maxPutRecords)
		if err := put(ctx, items[:n:n]); err != nil {
			return err
		}
		items = items[n:]
	}
	return nil
}

// collapse trims s, folds runs of whitespace into one space and bounds it.
func collapse(s string, max int) string {
	return bound(strings.Join(strings.Fields(s), " "), max)
}

// cleanDoc strips comment syntax from an attached comment or docstring and
// keeps its lines, bounded.
func cleanDoc(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(s, q) && strings.HasSuffix(s, q) && len(s) >= 2*len(q) {
			s = s[len(q) : len(s)-len(q)]
		}
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		for _, m := range []string{"/**", "/*!", "/*", "*/", "///", "//!", "//", "#", "*"} {
			l = strings.TrimSpace(strings.TrimPrefix(l, m))
		}
		l = strings.TrimSpace(strings.TrimSuffix(l, "*/"))
		if l == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return bound(strings.Join(out, "\n"), maxDocBytes)
}

// bound truncates s to at most max bytes on a rune boundary.
func bound(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// lastPathSegment is the display name of an import path in any of the
// languages' spellings: "a/b/c", "a.b.c", "a::b::c", "<x/y.h>", "./x".
func lastPathSegment(p string) string {
	p = strings.TrimSuffix(strings.Trim(p, `"'<>`), "/")
	switch {
	case strings.ContainsAny(p, "/:"):
		p = p[strings.LastIndexAny(p, "/:")+1:]
	case strings.HasSuffix(p, ".h"), strings.HasSuffix(p, ".hpp"), strings.HasSuffix(p, ".hh"):
		// A header's extension is not a package separator.
	default:
		p = p[strings.LastIndex(p, ".")+1:]
	}
	if p == "" {
		return "."
	}
	return bound(p, model.MaxNameBytes)
}
