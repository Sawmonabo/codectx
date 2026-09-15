package treesitter

import (
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/source"
)

// A qualifier the worker is allowed to send must fit the name bound the
// parent publishes it under; this does not compile if the two ever diverge.
const _ uint = model.MaxNameBytes - wire.MaxQualifierBytes

// Fact bounds beyond the model's. A doc body is kept well under the per-record
// cap so a declaration's search document never approaches it; evidence per
// fact is the model bound, and occurrences past it are counted, not stored.
const (
	maxDocBytes    = 8 << 10
	capabilityName = "structure"
	searchDomain   = "treesitter-search-v1"
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
	modKey   string
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
	// occurrences past the per-fact evidence bound, calls past the
	// callee-reference bound, and declaration or call-site keys over
	// MaxNativeKeyBytes. Any of it makes the file's coverage partial.
	dropped int

	// maxCallees is tree_sitter.max_callee_references, the bound on the
	// distinct callee reference nodes this file may mint: the callees no
	// single declaration of this file can be. Unlimited by default -- a
	// generated file names what it names -- and bounded in any case by the
	// file's own references. Past a user-set bound the call is counted into
	// dropped and the file reports partial.
	maxCallees config.Limit
}

// declFact is one validated declaration and its resolved identity.
type declFact struct {
	wire.Decl
	kind model.NodeKind
	rng  *model.SourceRange
	sig  string
	doc  string
	res  model.Resolution
	// truncated names the fields this declaration's storage ceilings cut,
	// with the original byte length of each. It is index-time truncation of
	// a stored value and is published as its own attribute, never merged
	// with a result page's transient truncation flag.
	truncated map[string]int

	// body is the search document's text: the attached documentation and the
	// signature, composed and bounded once at extraction so a cut is recorded
	// in truncated before the node's metadata is built.
	body string
}

// truncate bounds one of this declaration's fields and records the cut.
func (d *declFact) truncate(field, value string, max int) string {
	bounded, original := model.TruncateField(value, max)
	if original > len(bounded) {
		if d.truncated == nil {
			d.truncated = map[string]int{}
		}
		d.truncated[field] = original
	}
	return bounded
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
	b.scope, b.modKey = ScopePrefix+b.fv.Path, "module:"+b.fv.Path
	// The file's two path-derived keys are bounded here, once, before any
	// identity is claimed. A snapshot path may be model.MaxPathBytes long while
	// a scope key stops at model.MaxScopeKeyBytes and a native key at
	// model.MaxNativeKeyBytes, so a legal path overflows the file's alias scope
	// and its module node's native key. Neither is truncated — a truncated key
	// is a different, possibly colliding identity claim — and neither may be
	// published over its bound, which model.NodeCandidate.Validate and
	// model.NativeAlias.Validate reject with CTX_ARGUMENT_INVALID, failing the
	// whole unit for one file nothing can name. Without a module node there is
	// no container for a declaration and no scope for an alias, so the file
	// publishes nothing and reports its structural coverage partial /
	// CTX_COVERAGE_INCOMPLETE, exactly as an over-long declaration key does.
	if len(b.scope) > model.MaxScopeKeyBytes || len(b.modKey) > model.MaxNativeKeyBytes {
		b.dropped++
		return nil
	}
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
		// Emptiness and encoding still refuse the fact: a missing name has
		// nothing to store and truncating invalid UTF-8 does not make it
		// valid. Length does not, because generated code clears these
		// ceilings routinely and refusing there publishes nothing for the
		// whole file. The over-long values are cut and flagged below.
		if d.Name == "" || !utf8.ValidString(d.Name) || !utf8.ValidString(d.Qualified) {
			return outputInvalid("declaration name is empty or not UTF-8")
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
		f := declFact{Decl: d, kind: kind, rng: rng}
		f.sig = f.truncate("signature", collapse(string(b.src[d.Start:d.SigEnd])), model.MaxSignatureBytes)
		f.Name = f.truncate("name", d.Name, model.MaxNameBytes)
		f.Qualified = f.truncate("qualified_name", d.Qualified, model.MaxQualifiedNameBytes)
		f.Impl = f.truncate("receiver", d.Impl, model.MaxNameBytes)
		if d.DocEnd > 0 {
			if _, err := b.rangeOf(d.DocStart, d.DocEnd); err != nil {
				return err
			}
			f.doc = f.truncate("doc", cleanDoc(string(b.src[d.DocStart:d.DocEnd])), maxDocBytes)
		}
		// The search body is composed and bounded here, not in searchUnit,
		// because the node's metadata is built before searchUnit runs: a cut
		// made later would never reach truncated_fields. A doc and a
		// signature each at their own ceiling compose to more than one body
		// holds, so this bound really does cut and must disclose it.
		f.body = f.truncate("body", searchBody(f.doc, f.sig), maxDocBytes)
		b.decls = append(b.decls, f)
		b.byName[f.Name] = append(b.byName[f.Name], i)
	}
	return nil
}

// resolveModule mints the file's module node: the container every top-level
// declaration is defined by and the subject of imports and exports.
func (b *builder) resolveModule() error {
	res, err := b.resolve(model.NodeCandidate{
		ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: b.modKey,
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
	b.putNode(res, b.fileRng, b.modKey, meta)
	b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: b.modKey, NodeID: res.Node.ID})
	return nil
}

// resolveDecls resolves every declaration, publishes its node, containment,
// export, aliases and search document.
func (b *builder) resolveDecls() error {
	for i := range b.decls {
		d := &b.decls[i]
		res, err := b.resolve(model.NodeCandidate{
			ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: b.identityKey(d), Kind: d.kind, Language: b.lang.Name,
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
		if len(d.truncated) > 0 {
			// Index-time field truncation, reported on the fact itself so an
			// answer built from it says which stored values were cut and how
			// long they were. Distinct from result truncation by name.
			meta["truncated_fields"] = d.truncated
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
		if _, cut := d.truncated["qualified_name"]; !cut {
			b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: d.Qualified, NodeID: res.Node.ID})
		} else {
			// The qualified name was cut to its storage ceiling, so it is no
			// longer an identity: publishing it as an alias would claim that
			// every declaration sharing the first MaxQualifiedNameBytes is
			// the same symbol. It is omitted and counted, exactly as declKey
			// omits an over-long key.
			b.dropped++
		}
		if key := b.declKey(d); key != "" {
			b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: key, NodeID: res.Node.ID})
		} else {
			// The cross-provider key did not fit and was omitted rather than
			// truncated. That is a record this file should have published and
			// did not: without it the semantic provider's declaration for the
			// same function resolves to a second identity. It is counted, so
			// the file reports partial / CTX_COVERAGE_INCOMPLETE instead of
			// claiming structural coverage it does not have.
			b.dropped++
		}
		if d.Parent < 0 {
			pkg, over := b.packageScope()
			switch {
			case pkg != "" && d.truncated["qualified_name"] == 0:
				b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: pkg, NativeKey: d.Qualified, NodeID: res.Node.ID})
			case over:
				// The package alias scope did not fit and was omitted rather
				// than truncated, for the same reason declKey omits an
				// over-long key. This declaration will not merge with the one
				// another file of the same package publishes, so the file's
				// structural coverage is genuinely incomplete and says so.
				b.dropped++
			}
		}
		b.search = append(b.search, b.searchUnit(d))
	}
	return nil
}

// identityKey is the native key the declaration's node is resolved under.
// The qualified name is that key while it fits: it is the identity every
// other provider joins on. Once it has been cut to its storage ceiling it is
// a prefix, not an identity, so the file-local declaration key stands in --
// unique within this file and already the cross-provider key. Should that
// not fit either, the cut qualified name is the last resort and the caller's
// b.dropped count is what reports the weakened identity.
func (b *builder) identityKey(d *declFact) string {
	if _, cut := d.truncated["qualified_name"]; !cut {
		return d.Qualified
	}
	if key := b.declKey(d); key != "" {
		return key
	}
	return d.Qualified
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
// The `dependence` provider, whose exported declaration key is byte-for-byte
// this string, publishes the same alias, which is what merges the two
// providers' identities for one function instead of leaving two correct but
// unrelated nodes.
//
// A key over MaxNativeKeyBytes is omitted rather than truncated: a truncated
// key would be a different, possibly colliding identity claim. The
// declaration keeps its own identity and its qualified-name alias either way.
// The omission is counted in b.dropped by the caller, because a declaration
// whose cross-provider key is missing will not merge with the semantic
// provider's node for the same function: the file's structural coverage is
// genuinely incomplete and says so.
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
//
// The Go spelling carries the file's directory, which a legal path can make
// longer than model.MaxScopeKeyBytes on its own. An over-long scope is
// reported over=true so the caller drops and counts that one alias, rather
// than publishing a key model.NativeAlias.Validate rejects — which would fail
// the whole unit — or a truncated one, which would be a colliding scope.
func (b *builder) packageScope() (scope string, over bool) {
	pkg := b.ex.done.Package
	if pkg == "" || len(pkg) > model.MaxNameBytes {
		return "", false
	}
	switch b.lang.Name {
	case "go":
		scope = "pkg:go:" + path.Dir(b.fv.Path) + ":" + pkg
	case "java":
		scope = "pkg:java:" + pkg
	default:
		return "", false
	}
	if len(scope) > model.MaxScopeKeyBytes {
		return "", true
	}
	return scope, false
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
		// An import path is bounded by MaxQualifiedNameBytes, which equals
		// MaxNativeKeyBytes, so the prefix can push the import target's key
		// over its bound. It is dropped and counted like every other key that
		// does not fit: the import edge is lost, never truncated into a
		// different identity and never allowed to fail the unit.
		key := "import:" + b.lang.Name + ":" + imp.Path
		if len(key) > model.MaxNativeKeyBytes {
			b.dropped++
			continue
		}
		name := lastPathSegment(imp.Path)
		res, err := b.resolve(model.NodeCandidate{
			ProviderID: lang.ProviderID, ScopeKey: provider.ScopeWorkspace, NativeKey: key,
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
// (Section 11.1 precision syntax): a call to a name declared exactly once in
// this file names that declaration; every other call names a provider-local
// callee reference node carrying the Section 9.3 resolution and candidates
// attributes, never a guess at another file's symbol, and a name declared
// several times here is additionally retained as may_refer_to edges.
//
// Every call site also publishes the Section 11.3 call-site alias on its
// callee node, so the SCIP importer's alias for the reference occurrence at
// the same bytes joins the syntactic callee to the compiler-resolved symbol
// and the calls relation acquires a precise target.
func (b *builder) refs() error {
	callees := map[string]model.Resolution{}
	for _, r := range b.ex.refs {
		if r.Kind != "call" && r.Kind != "type" {
			return outputInvalid("reference kind " + bound(r.Kind, 32) + " is unknown")
		}
		if r.Name == "" || len(r.Name) > model.MaxNameBytes || !utf8.ValidString(r.Name) {
			return outputInvalid("reference name is empty, over its bound or not UTF-8")
		}
		// The two are checked apart because they fail for different reasons
		// and a single message cost a lane a debugging round: a name is a
		// token the grammar captured, a qualifier is a receiver expression
		// the worker is required to bound before it sends it.
		if len(r.Qualifier) > wire.MaxQualifierBytes || !utf8.ValidString(r.Qualifier) {
			return outputInvalid("reference qualifier is over its bound or not UTF-8")
		}
		if r.Scope < -1 || r.Scope >= len(b.decls) {
			return outputInvalid("reference names a scope that is not a declaration")
		}
		rng, err := b.rangeOf(r.Start, r.End)
		if err != nil {
			return err
		}
		// The identifier range is validated against the pinned bytes like
		// every other offset — rangeOf is what rejects an offset inside a
		// UTF-8 sequence or past the manifest size — and must name a
		// non-empty token inside the reference it belongs to, because the
		// callsite alias is built from it and a range that is not the
		// callee token joins the wrong SCIP occurrence, or none.
		if r.NameEnd <= r.NameStart || r.NameStart < r.Start || r.NameEnd > r.End {
			return outputInvalid("reference identifier range [" + strconv.Itoa(int(r.NameStart)) + "," + strconv.Itoa(int(r.NameEnd)) +
				") is empty or lies outside the reference")
		}
		if _, err := b.rangeOf(r.NameStart, r.NameEnd); err != nil {
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
		var callee model.NodeID
		if len(targets) == 1 {
			// The one declaration of this file the name can mean. The callee
			// node is that declaration, and the calls edge to a located
			// declaration of this same file states that resolution
			// structurally rather than as an attribute on a node that also
			// describes the declaration itself.
			callee = b.decls[targets[0]].res.Node.ID
			b.putRelation(from, model.RelCalls, callee, rng, r.Name, "")
		} else {
			// Nothing in this file is the single callee: the name is declared
			// several times here, is qualified by an imported name, or is not
			// declared here at all.
			//
			// A bare call keys its callee reference by the callee name, a
			// qualified one by receiver and name; the two never collide
			// because a callee name is a single identifier and a qualified
			// key always carries the separating dot. A receiver the worker
			// could not name within its bound leaves the qualifier empty, so
			// the key is "call:." + name: a method of that name on a receiver
			// this file does not name, which is what it is.
			key := "call:" + r.Name
			kind := model.NodeFunction
			if r.Qualified {
				key, kind = "call:"+r.Qualifier+"."+r.Name, model.NodeMethod
			}
			resolution, candidates := calleeResolution(r, targets)
			res, ok := callees[key]
			if !ok {
				if b.maxCallees.Exceeded(int64(len(callees) + 1)) {
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
				callees[key] = res
				b.putNode(res, rng, key, map[string]any{"resolution": resolution, "candidates": candidates, "callee": r.Name})
			} else {
				b.addEvidence(res.Node.ID, rng, key)
			}
			callee = res.Node.ID
			b.putRelation(from, model.RelCalls, callee, rng, key, resolution)
			kept := min(len(targets), model.MaxAmbiguousCandidates)
			if kept < len(targets) {
				// The candidate list is cut to the model's ambiguity bound.
				// Each candidate past it is a `may_refer_to` edge this file
				// should have published and did not, so it is counted like
				// every other loss and the file reports partial rather than
				// claiming complete structural coverage.
				b.dropped += len(targets) - kept
			}
			for _, t := range targets[:kept] {
				b.putRelation(from, model.RelMayReferTo, b.decls[t].res.Node.ID, rng, r.Name, "ambiguous call target")
			}
		}
		if key := b.callsiteKey(r.NameStart, r.NameEnd); key != "" {
			b.aliases = append(b.aliases, model.NativeAlias{ScopeKey: b.scope, NativeKey: key, NodeID: callee})
		} else {
			// The key did not fit and was omitted rather than truncated: a
			// truncated key is a different, possibly colliding identity
			// claim. Without it this call site does not join the SCIP
			// occurrence at the same bytes, so the file's structural
			// coverage is genuinely incomplete and reports partial.
			b.dropped++
		}
	}
	return nil
}

// calleeResolution is the Section 9.3 attribute pair for a call whose callee
// is not one declaration of this file: which basis resolved it and how many
// equally supported candidates were considered.
//
// Only three of the six enum values can be produced by a file-local
// syntactic pass. `exact` needs a compiler binding, which this provider does
// not have; `same_module` describes the resolved case above, which carries
// its resolution structurally as an edge to a located declaration of this
// file rather than as an attribute; `unique_name` would mean resolving a
// name across a scope wider than this file, which this provider never does.
func calleeResolution(r wire.Ref, targets []int) (string, int) {
	switch {
	case len(targets) > 1:
		return "ambiguous", len(targets)
	case r.QualifierIsImport:
		// The qualifier is a name an import of this file introduced, so the
		// callee is known to come through that import and is known not to be
		// in this file: the basis is the import, with no in-file candidate.
		return "import", 0
	default:
		return "unresolved", 0
	}
}

// callsiteKey is the Section 11.3 call-site join key, the one string this
// provider and the SCIP importer both compute for the same call site:
//
//	scope  "file:" + path
//	key    "callsite:" + path + ":" + <first byte> + "-" + <last byte>
//
// The byte range is the callee identifier's, one-based and inclusive, so a
// callee token occupying the half-open UTF-8 byte range [start,end) is
// spelled start+1 "-" end. Byte offsets, never characters or UTF-16 code
// units: the SCIP importer converts its own encoding through internal/source
// before it builds the same key. The path is the snapshot manifest's
// root-relative slash path, the same one this unit's scope key carries.
//
// A key over MaxNativeKeyBytes is omitted rather than truncated, exactly as
// declKey is, and the omission is counted by the caller.
func (b *builder) callsiteKey(start, end uint32) string {
	key := "callsite:" + b.fv.Path + ":" + strconv.FormatUint(uint64(start)+1, 10) + "-" + strconv.FormatUint(uint64(end), 10)
	if len(key) > model.MaxNativeKeyBytes {
		return ""
	}
	return key
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
	body := d.body
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
// collapse folds a declaration's signature bytes onto one line. It does not
// bound the result: the caller truncates through declFact.truncate, so a cut
// signature is flagged in truncated_fields rather than shortened in silence.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
	// Unbounded: the caller truncates through declFact.truncate so the cut is
	// counted. Bounding here is what made an over-long docstring vanish
	// silently.
	return strings.Join(out, "\n")
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

// searchBody composes a declaration's search text: its attached documentation
// above its signature, or the signature alone when it has none. The caller
// bounds the result through declFact.truncate so a cut is flagged.
func searchBody(doc, sig string) string {
	if doc == "" {
		return sig
	}
	return doc + "\n" + sig
}
