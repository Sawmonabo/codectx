package scip

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/source"
)

// opener yields the index bytes and their size: the snapshot view for a
// supplied index, a private run file for a profile's output.
type opener func(context.Context) (io.ReadCloser, int64, error)

// importer runs one unit. The import is two passes over the index plus one
// binding pre-pass, all streaming:
//
//   - pre-pass: document paths and embedded text hashes decide the source
//     binding before any fact exists, because the binding decides whether an
//     occurrence that does not land on the pinned bytes fails the unit
//     (verified: the index claims to describe these bytes and does not) or
//     is skipped (unverified: discovery over bytes the index never saw);
//   - pass 1: occurrences and symbols are spooled to the scratch database as
//     they stream past; when a document ends its position encoding is known,
//     so its definitions are converted against the pinned bytes, resolved,
//     published and recorded in the on-disk symbol map;
//   - pass 2: references are read back from the spool with the symbol map
//     complete, so a forward or external reference resolves to the same
//     identity a later definition minted; edges are grouped by relation
//     identity on disk and published with one evidence row per distinct
//     occurrence range.
type importer struct {
	p        *Provider
	req      provider.UnitRequest
	sink     provider.Sink
	sc       *scratch
	profile  *Profile
	binding  model.SourceBinding
	unverify bool // binding is unverified: skip, never fail, on a bad position
	meta     metadata
	// ctx is the run context, carried for the walker's document callback,
	// which takes none.
	ctx context.Context

	release    func()
	reserved   int64
	records    uint64
	indexBytes uint64

	skippedDocs, skippedOccurrences, truncatedEdges int64
	partialCode                                     string
}

// location is a pinned file plus a byte range on it: where evidence points.
type location struct {
	file model.FileVersion
	rng  *model.SourceRange
}

// docSource is one document's exact bytes with its position cursor.
type docSource struct {
	doc  docRow
	data []byte
	cur  *source.Cursor
	enc  source.ColumnEncoding
}

// docRow is the scratch record of one document bound to a snapshot file.
type docRow struct {
	idx      int64
	path     string
	language string
	encoding int32
	file     model.FileVersion
}

// run imports one index. open is called once per pass.
func (im *importer) run(ctx context.Context, open opener) error {
	if im.profile == nil && im.p.manifestPath != "" {
		if err := im.loadManifest(ctx); err != nil {
			return err
		}
	}
	binding, meta, err := im.scanBinding(ctx, open)
	if err != nil {
		return err
	}
	im.binding, im.meta = binding, meta
	im.unverify = binding == model.SourceBindingUnverified
	if im.profile != nil {
		if err := checkTool(*im.profile, meta); err != nil {
			return err
		}
	}
	if im.unverify {
		im.partialCode = model.CodeSourceBindingUnverified
	}
	if err := im.passDefinitions(ctx, open); err != nil {
		return err
	}
	if err := im.passReferences(ctx); err != nil {
		return err
	}
	if err := im.passRelationships(ctx); err != nil {
		return err
	}
	return im.emitEdges(ctx)
}

// scanBinding streams the index once, reading only paths and text, and
// decides the Section 11.4 source binding. A codectx-invoked profile analyzed
// a private materialization of exactly these bytes and is verified by
// construction. A supplied index is verified only when every document that
// names a snapshot file proves its bytes, by embedded text whose hash is the
// pinned content hash or by a matching entry in the supplied input-hash
// manifest; anything less is unverified, never upgraded.
func (im *importer) scanBinding(ctx context.Context, open opener) (model.SourceBinding, metadata, error) {
	var meta metadata
	var verified, unverified int64
	w := &walker{limits: im.p.limits, onGrow: im.reserve,
		onMetadata: func(m metadata) error { meta = m; return nil },
		onDocument: func(d document) error {
			fv, ok, err := im.lookup(ctx, d.path)
			if err != nil || !ok {
				return err
			}
			if d.hasText && d.textHash == fv.ContentHash {
				verified++
				return nil
			}
			var hash string
			if found, err := im.sc.row(ctx, `SELECT hash FROM manifest WHERE path = ?`, []any{d.path}, &hash); err != nil {
				return err
			} else if found && hash == fv.ContentHash {
				verified++
				return nil
			}
			unverified++
			return nil
		}}
	if err := im.walk(ctx, open, w); err != nil {
		return "", meta, err
	}
	if im.profile != nil {
		return model.SourceBindingVerified, meta, nil
	}
	if verified > 0 && unverified == 0 {
		return model.SourceBindingVerified, meta, nil
	}
	return model.SourceBindingUnverified, meta, nil
}

// walk opens the index and streams it through w.
func (im *importer) walk(ctx context.Context, open opener, w *walker) error {
	rc, size, err := open(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	if size > im.p.limits.MaxIndexBytes {
		return overLimit("index bytes", size, im.p.limits.MaxIndexBytes)
	}
	consumed, err := w.walk(ctx, bufio.NewReaderSize(rc, 64<<10), size)
	im.indexBytes += uint64(consumed)
	return err
}

// reserve charges the walker's record buffer against the sink's pool when
// the sink supports reservations (the production BatchSink does), releasing
// the previous charge first so exactly one reservation is outstanding.
func (im *importer) reserve(ctx context.Context, n int64) error {
	r, ok := im.sink.(interface {
		Reserve(context.Context, int64) (func(), error)
	})
	if !ok || n <= im.reserved {
		return nil
	}
	release, err := r.Reserve(ctx, n)
	if err != nil {
		return err
	}
	if im.release != nil {
		im.release()
	}
	im.release, im.reserved = release, n
	return nil
}

func (im *importer) close() {
	if im.release != nil {
		im.release()
		im.release = nil
	}
}

// lookup finds the snapshot file at a document's relative path.
func (im *importer) lookup(ctx context.Context, p string) (model.FileVersion, bool, error) {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > model.MaxPathBytes {
		return model.FileVersion{}, false, nil
	}
	var out model.FileVersion
	var found bool
	err := im.req.Content.EachFile(ctx, model.FileSelection{Paths: []string{p}}, func(fv model.FileVersion) error {
		if fv.Status != model.FileDeleted {
			out, found = fv, true
		}
		return nil
	})
	return out, found, err
}

// loadManifest streams the supplied input-hash manifest (`<sha256>  <path>`
// lines) into the scratch table. Its lines are bounded and the file is a
// snapshot input like the index itself.
func (im *importer) loadManifest(ctx context.Context) error {
	fv, ok, err := im.lookup(ctx, im.p.manifestPath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if fv.Size > im.p.limits.MaxManifestBytes {
		return overLimit("manifest bytes", fv.Size, im.p.limits.MaxManifestBytes)
	}
	rc, _, err := im.req.Content.Open(ctx, fv.ID)
	if err != nil {
		return err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 4096), model.IDHexLen+2+model.MaxPathBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		hash, rest, ok := strings.Cut(line, " ")
		if !ok || !model.ValidHexID(hash) {
			return malformed("input-hash manifest line is not `<sha256>  <path>`")
		}
		if err := im.sc.charge(int64(len(line))); err != nil {
			return err
		}
		if err := im.sc.exec(ctx, `INSERT OR REPLACE INTO manifest(path, hash) VALUES(?, ?)`, strings.TrimLeft(rest, " "), hash); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return malformed("input-hash manifest cannot be read: " + err.Error())
	}
	return nil
}

// passDefinitions is pass 1: spool occurrences and symbols, bind each
// document to its snapshot file when it ends, and publish its definitions.
func (im *importer) passDefinitions(ctx context.Context, open opener) error {
	w := &walker{limits: im.p.limits, onGrow: im.reserve,
		onOccurrence: func(doc, seq int64, o occurrence, n int64) error {
			if err := im.sc.charge(n); err != nil {
				return err
			}
			r := pad(o.rng)
			var e [4]any
			if o.hasEnclosing {
				enc := pad(o.enclosing)
				e = [4]any{enc[0], enc[1], enc[2], enc[3]}
			}
			return im.sc.exec(ctx, `INSERT INTO occ(doc, seq, symbol, roles, r0, r1, r2, r3, e0, e1, e2, e3) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				doc, seq, o.symbol, o.roles, r[0], r[1], r[2], r[3], e[0], e[1], e[2], e[3])
		},
		onSymbol: func(doc int64, s symbolInfo, n int64) error {
			if err := im.sc.charge(n); err != nil {
				return err
			}
			return im.recordSymbol(ctx, doc, s)
		},
		onExternal: func(s symbolInfo, n int64) error {
			if err := im.sc.charge(n); err != nil {
				return err
			}
			return im.recordSymbol(ctx, -1, s)
		},
		onDocument: im.endDocument,
	}
	return im.walk(ctx, open, w)
}

// pad widens a three-value single-line range to four values.
func pad(r []int32) [4]int32 {
	if len(r) == 3 {
		return [4]int32{r[0], r[1], r[0], r[2]}
	}
	return [4]int32{r[0], r[1], r[2], r[3]}
}

// symKey scopes a symbol in the map: a local symbol to its document, a
// global one to itself (Section 9.4: SCIP locals are document scoped).
func symKey(doc int64, sym symbol) string {
	if sym.local {
		return "l:" + strconv.FormatInt(doc, 10) + ":" + sym.raw
	}
	return "g:" + sym.raw
}

// recordSymbol upserts a SymbolInformation record. Fields already known are
// kept, so a later duplicate can add a kind or name but never erase the node
// a definition assigned.
func (im *importer) recordSymbol(ctx context.Context, doc int64, s symbolInfo) error {
	sym, err := parseSymbol(s.symbol)
	if err != nil {
		return err
	}
	key := symKey(doc, sym)
	name := s.displayName
	if name == "" {
		name = sym.lastName
	}
	if err := im.sc.exec(ctx, `INSERT INTO sym(key, symbol, kind, name, sig) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET kind = CASE WHEN sym.kind = 0 THEN excluded.kind ELSE sym.kind END,
		name = CASE WHEN sym.name = '' THEN excluded.name ELSE sym.name END,
		sig = CASE WHEN sym.sig = '' THEN excluded.sig ELSE sym.sig END`, key, s.symbol, s.kind, name, s.signature); err != nil {
		return err
	}
	for _, rel := range s.relationships {
		if err := im.sc.exec(ctx, `INSERT OR IGNORE INTO rel(key, target, flags) VALUES(?, ?, ?)`, key, rel.symbol, rel.flags); err != nil {
			return err
		}
	}
	return nil
}

// endDocument binds a finished document to its snapshot file and publishes
// its definitions. A document the snapshot does not hold, or whose encoding
// the indexer left unspecified, or whose file exceeds the source bound, is
// skipped and counted: nothing is guessed about it.
func (im *importer) endDocument(d document) error {
	ctx := im.ctx
	fv, ok, err := im.lookup(ctx, d.path)
	if err != nil {
		return err
	}
	switch {
	case !ok:
		im.skippedDocs++
		return nil
	case columnEncoding(d.encoding) == "":
		im.skippedDocs++
		im.degrade(model.CodeProviderOutputInvalid)
		return nil
	case fv.Size > im.p.limits.MaxSourceFileBytes:
		im.skippedDocs++
		im.degrade(model.CodeResourceLimit)
		return nil
	}
	row := docRow{idx: d.index, path: d.path, language: d.language, encoding: d.encoding, file: fv}
	if err := im.sc.exec(ctx, `INSERT INTO docs(idx, path, lang, enc, file_id, content_hash, size) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		row.idx, row.path, row.language, row.encoding, string(fv.ID), fv.ContentHash, fv.Size); err != nil {
		return err
	}
	ds, err := im.loadSource(ctx, row)
	if err != nil {
		return err
	}
	return im.sc.each(ctx, `SELECT seq, symbol, roles, r0, r1, r2, r3, e0, e1, e2, e3 FROM occ WHERE doc = ? AND (roles & ?) <> 0 ORDER BY seq`,
		[]any{row.idx, roleDefinition}, func(scan func(...any) error) error {
			var seq int64
			var symbolText string
			var roles int32
			var r [4]int32
			var e [4]*int32
			if err := scan(&seq, &symbolText, &roles, &r[0], &r[1], &r[2], &r[3], &e[0], &e[1], &e[2], &e[3]); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			if symbolText == "" {
				return nil
			}
			var enclosing *[4]int32
			if e[0] != nil && e[1] != nil && e[2] != nil && e[3] != nil {
				enclosing = &[4]int32{*e[0], *e[1], *e[2], *e[3]}
			}
			return im.defineSymbol(ctx, ds, symbolText, r, enclosing)
		})
}

// degrade records the first reason the unit's capabilities are partial. An
// unverified binding outranks every later reason.
func (im *importer) degrade(code string) {
	if im.partialCode == "" {
		im.partialCode = code
	}
}

// columnEncoding maps a SCIP position encoding to the shared source
// vocabulary; unspecified maps to "" and is never guessed (Section 9.3).
func columnEncoding(enc int32) source.ColumnEncoding {
	switch enc {
	case encodingUTF8:
		return source.UTF8
	case encodingUTF16:
		return source.UTF16
	case encodingUTF32:
		return source.UTF32
	}
	return ""
}

// loadSource reads one document's exact pinned bytes through the snapshot
// view. The file is bounded by MaxSourceFileBytes, checked before the read.
func (im *importer) loadSource(ctx context.Context, d docRow) (*docSource, error) {
	rc, _, err := im.req.Content.Open(ctx, d.file.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data := make([]byte, d.file.Size)
	if _, err := io.ReadFull(rc, data); err != nil {
		return nil, readError(err)
	}
	if _, err := rc.Read(make([]byte, 1)); err != io.EOF {
		return nil, &model.Error{Code: model.CodeSourceIntegrity, Message: "pinned file is longer than its manifest size", Details: map[string]string{"path": d.path}}
	}
	return &docSource{doc: d, data: data, cur: source.NewCursor(data), enc: columnEncoding(d.encoding)}, nil
}

// rangeOf converts a SCIP range against the document's bytes. A coordinate
// that does not land on these bytes is a wrong-source error under a verified
// binding and a skipped occurrence under an unverified one; it is never
// adjusted.
func (im *importer) rangeOf(ds *docSource, r [4]int32) (*model.SourceRange, error) {
	if r[0] < 0 || r[1] < 0 || r[2] < 0 || r[3] < 0 {
		return nil, im.badRange(ds, "occurrence range has a negative coordinate")
	}
	rng, err := ds.cur.SourceRange(uint32(r[0])+1, uint32(r[1]), uint32(r[2])+1, uint32(r[3]), ds.enc)
	if err != nil {
		return nil, im.badRange(ds, err.Error())
	}
	return &rng, nil
}

// badRange is the typed outcome of a coordinate that misses the pinned
// bytes: nil under an unverified binding (the caller skips and counts),
// otherwise CTX_PROVIDER_OUTPUT_INVALID that fails the unit.
func (im *importer) badRange(ds *docSource, msg string) error {
	if im.unverify {
		im.skippedOccurrences++
		return nil
	}
	return (&model.Error{Code: model.CodeProviderOutputInvalid, Message: "scip occurrence does not describe the pinned source bytes: " + msg}).
		WithDetail("path", ds.doc.path)
}

// defineSymbol publishes one definition occurrence: the node, its scoped
// alias, any may_refer_to alternatives, its containment extent for pass 2
// and its entry in the symbol map.
func (im *importer) defineSymbol(ctx context.Context, ds *docSource, symbolText string, r [4]int32, enclosing *[4]int32) error {
	sym, err := parseSymbol(symbolText)
	if err != nil {
		return err
	}
	rng, err := im.rangeOf(ds, r)
	if err != nil || rng == nil {
		return err
	}
	extent := rng
	if enclosing != nil {
		if extent, err = im.rangeOf(ds, *enclosing); err != nil {
			return err
		}
		if extent == nil || extent.Start.Byte > rng.Start.Byte || extent.End.Byte < rng.End.Byte {
			extent = rng
		}
	}
	key := symKey(ds.doc.idx, sym)
	var kind int32
	var name, sig, existing string
	if _, err := im.sc.row(ctx, `SELECT kind, name, sig, node FROM sym WHERE key = ?`, []any{key}, &kind, &name, &sig, &existing); err != nil {
		return err
	}
	if name == "" {
		name = sym.lastName
	}
	qualified := sym.descriptors
	if sym.local {
		qualified = ""
	}
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: sym.scopeKey(ds.doc.path), NativeKey: sym.raw, Kind: sym.nodeKind(kind),
		Language: ds.doc.language, Name: name, QualifiedName: qualified, Signature: sig,
		FileID: ds.doc.file.ID, ContentHash: ds.doc.file.ContentHash, Range: rng}
	res, err := im.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return err
	}
	loc := location{file: ds.doc.file, rng: rng}
	if err := im.putNode(ctx, res, sym.raw, "definition", loc, false); err != nil {
		return err
	}
	if err := im.sink.PutAliases(ctx, []model.NativeAlias{{ScopeKey: cand.ScopeKey, NativeKey: sym.raw, NodeID: res.Node.ID}}); err != nil {
		return err
	}
	im.records++
	for _, other := range res.Ambiguous {
		if err := im.addEdge(ctx, res.Node.ID, model.RelMayReferTo, other, loc, sym.raw, "may_refer_to"); err != nil {
			return err
		}
	}
	if err := im.sc.exec(ctx, `INSERT OR IGNORE INTO defs(doc, start, end, node) VALUES(?, ?, ?, ?)`,
		ds.doc.idx, int64(extent.Start.Byte), int64(extent.End.Byte), string(res.Node.ID)); err != nil {
		return err
	}
	// The first definition of a symbol is the one references bind to; a
	// later one keeps its own located identity but does not take over.
	return im.sc.exec(ctx, `INSERT INTO sym(key, symbol, kind, name, sig, node, def_doc, def_start, def_end) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET node = CASE WHEN sym.node = '' THEN excluded.node ELSE sym.node END,
		def_doc = CASE WHEN sym.node = '' THEN excluded.def_doc ELSE sym.def_doc END,
		def_start = CASE WHEN sym.node = '' THEN excluded.def_start ELSE sym.def_start END,
		def_end = CASE WHEN sym.node = '' THEN excluded.def_end ELSE sym.def_end END`,
		key, sym.raw, kind, name, sig, string(res.Node.ID), ds.doc.idx, int64(rng.Start.Byte), int64(rng.End.Byte))
}

// putNode publishes a resolved node with one evidence row at loc. Node
// metadata carries the binding when it is unverified and marks an entity
// that has no definition in this index as external.
func (im *importer) putNode(ctx context.Context, res model.Resolution, nativeKey, detail string, loc location, external bool) error {
	node := res.Node
	meta := map[string]any{}
	if im.unverify {
		meta["source_binding"] = string(model.SourceBindingUnverified)
	}
	if external {
		meta["scip_external"] = true
	}
	if len(meta) > 0 {
		raw, err := json.Marshal(meta)
		if err != nil {
			return internal("scip metadata: " + err.Error())
		}
		node.Metadata = raw
	}
	ev := im.evidence(node.ID, "", loc, nativeKey, detail)
	if err := im.sink.PutNodes(ctx, []model.NodeFact{{Node: node, CanonicalKey: res.CanonicalKey, Evidence: []model.Evidence{ev}}}); err != nil {
		return err
	}
	im.records++
	return nil
}

// evidence builds one evidence row for this unit and run with compiler
// precision, the SCIP precision class of Section 9.3.
func (im *importer) evidence(node model.NodeID, rel model.RelationID, loc location, nativeKey, detail string) model.Evidence {
	e := model.Evidence{UnitID: im.req.Unit.ID, ProviderID: im.req.Unit.ProviderID, ProviderVersion: im.req.Unit.ProviderVersion,
		OriginRunID: im.req.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionCompiler,
		FileID: loc.file.ID, ContentHash: loc.file.ContentHash, Range: loc.rng, NativeKey: nativeKey, Detail: detail}
	e.ID = model.NewEvidenceID(e)
	return e
}

// addEdge records one occurrence of a canonical edge for grouped emission.
// The same edge at the same range is recorded once; a distinct range is a
// distinct occurrence (Section 11.4).
func (im *importer) addEdge(ctx context.Context, from model.NodeID, kind model.RelationKind, to model.NodeID, loc location, symbolText, detail string) error {
	if err := im.sc.charge(int64(len(from) + len(to) + len(symbolText) + 192)); err != nil {
		return err
	}
	rng := loc.rng
	return im.sc.exec(ctx, `INSERT OR IGNORE INTO edges(frm, kind, dst, file_id, content_hash, start, end, sl, sc, el, ec, symbol, detail) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(from), string(kind), string(to), string(loc.file.ID), loc.file.ContentHash, int64(rng.Start.Byte), int64(rng.End.Byte),
		rng.Start.Line, rng.Start.Column, rng.End.Line, rng.End.Column, symbolText, detail)
}

// passReferences is pass 2 over the spooled occurrences: every non-definition
// occurrence becomes one occurrence of an edge from its innermost enclosing
// definition (or the document's file node) to the referenced symbol's node.
func (im *importer) passReferences(ctx context.Context) error {
	var after int64 = -1
	for {
		var page []docRow
		err := im.sc.each(ctx, `SELECT idx, path, lang, enc, file_id, content_hash, size FROM docs WHERE idx > ? ORDER BY idx LIMIT 256`, []any{after},
			func(scan func(...any) error) error {
				var d docRow
				var id, hash string
				if err := scan(&d.idx, &d.path, &d.language, &d.encoding, &id, &hash, &d.file.Size); err != nil {
					return internal("scip scratch read: " + err.Error())
				}
				d.file.ID, d.file.ContentHash, d.file.Path = model.FileID(id), hash, d.path
				page = append(page, d)
				return nil
			})
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, d := range page {
			if err := im.referencesOf(ctx, d); err != nil {
				return err
			}
			after = d.idx
		}
	}
}

func (im *importer) referencesOf(ctx context.Context, d docRow) error {
	ds, err := im.loadSource(ctx, d)
	if err != nil {
		return err
	}
	return im.sc.each(ctx, `SELECT symbol, roles, r0, r1, r2, r3 FROM occ WHERE doc = ? AND (roles & ?) = 0 ORDER BY seq`,
		[]any{d.idx, roleDefinition}, func(scan func(...any) error) error {
			var symbolText string
			var roles int32
			var r [4]int32
			if err := scan(&symbolText, &roles, &r[0], &r[1], &r[2], &r[3]); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			if symbolText == "" {
				return nil
			}
			sym, err := parseSymbol(symbolText)
			if err != nil {
				return err
			}
			rng, err := im.rangeOf(ds, r)
			if err != nil || rng == nil {
				return err
			}
			loc := location{file: d.file, rng: rng}
			to, err := im.targetNode(ctx, symKey(d.idx, sym), sym, loc)
			if err != nil {
				return err
			}
			from, err := im.enclosingNode(ctx, ds, rng)
			if err != nil {
				return err
			}
			kind, detail := model.RelReferences, "reference"
			switch {
			case roles&roleImport != 0:
				kind, detail = model.RelImports, "import"
			case roles&roleWriteAccess != 0:
				kind, detail = model.RelWrites, "write"
			case roles&roleReadAccess != 0:
				kind, detail = model.RelReads, "read"
			}
			return im.addEdge(ctx, from, kind, to, loc, sym.raw, detail)
		})
}

// targetNode returns the node a referenced symbol resolves to: the node its
// definition minted, or, for a symbol this index never defines, an entity
// minted once from the symbol alone (structural key over its package scope
// and qualified name) with loc as its evidence. It is recorded in the map so
// every later reference reuses it.
func (im *importer) targetNode(ctx context.Context, key string, sym symbol, loc location) (model.NodeID, error) {
	var kind int32
	var name, sig, node string
	if _, err := im.sc.row(ctx, `SELECT kind, name, sig, node FROM sym WHERE key = ?`, []any{key}, &kind, &name, &sig, &node); err != nil {
		return "", err
	}
	if node != "" {
		return model.NodeID(node), nil
	}
	if name == "" {
		name = sym.lastName
	}
	qualified := sym.descriptors
	if sym.local {
		qualified = ""
	}
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: sym.scopeKey(loc.file.Path), NativeKey: sym.raw, Kind: sym.nodeKind(kind),
		Name: name, QualifiedName: qualified, Signature: sig}
	res, err := im.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return "", err
	}
	if err := im.putNode(ctx, res, sym.raw, "reference", loc, true); err != nil {
		return "", err
	}
	for _, other := range res.Ambiguous {
		if err := im.addEdge(ctx, res.Node.ID, model.RelMayReferTo, other, loc, sym.raw, "may_refer_to"); err != nil {
			return "", err
		}
	}
	return res.Node.ID, im.sc.exec(ctx, `INSERT INTO sym(key, symbol, kind, name, sig, node) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET node = excluded.node WHERE sym.node = ''`, key, sym.raw, kind, name, sig, string(res.Node.ID))
}

// enclosingNode is the innermost definition whose extent contains rng, or
// the document's own file node when the occurrence is at file level.
func (im *importer) enclosingNode(ctx context.Context, ds *docSource, rng *model.SourceRange) (model.NodeID, error) {
	var node string
	found, err := im.sc.row(ctx, `SELECT node FROM defs WHERE doc = ? AND start <= ? AND end >= ? ORDER BY (end - start), start, node LIMIT 1`,
		[]any{ds.doc.idx, int64(rng.Start.Byte), int64(rng.End.Byte)}, &node)
	if err != nil {
		return "", err
	}
	if found {
		return model.NodeID(node), nil
	}
	if _, err := im.sc.row(ctx, `SELECT file_node FROM docs WHERE idx = ?`, []any{ds.doc.idx}, &node); err != nil {
		return "", err
	}
	if node != "" {
		return model.NodeID(node), nil
	}
	// The file node this provider owns for a document: keyed on the path and
	// the pinned file, with the whole file as its evidence range.
	end, err := ds.cur.PositionAt(uint64(len(ds.data)))
	if err != nil {
		return "", err
	}
	whole := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1, Column: 0}, End: end}
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: "file:" + ds.doc.path, NativeKey: "document:" + ds.doc.path, Kind: model.NodeFile,
		Language: ds.doc.language, Name: path.Base(ds.doc.path), QualifiedName: ds.doc.path, FileID: ds.doc.file.ID, ContentHash: ds.doc.file.ContentHash}
	res, err := im.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return "", err
	}
	if err := im.putNode(ctx, res, cand.NativeKey, "document", location{file: ds.doc.file, rng: whole}, false); err != nil {
		return "", err
	}
	return res.Node.ID, im.sc.exec(ctx, `UPDATE docs SET file_node = ? WHERE idx = ?`, string(res.Node.ID), ds.doc.idx)
}

// passRelationships turns SymbolInformation relationships of defined symbols
// into edges: is_implementation → implements, is_reference and
// is_type_definition → references; is_definition names the symbol's own
// definition and is not an edge. Evidence is the source symbol's definition.
func (im *importer) passRelationships(ctx context.Context) error {
	type relRow struct {
		key, target, node string
		flags             int32
		docIdx            int64
		start, end        int64
	}
	var rows []relRow
	var after string
	for {
		rows = rows[:0]
		err := im.sc.each(ctx, `SELECT r.key, r.target, r.flags, s.node, s.def_doc, s.def_start, s.def_end FROM rel r JOIN sym s ON s.key = r.key
			WHERE s.node <> '' AND s.def_doc >= 0 AND r.key || char(0) || r.target > ? ORDER BY r.key, r.target LIMIT 256`, []any{after},
			func(scan func(...any) error) error {
				var r relRow
				if err := scan(&r.key, &r.target, &r.flags, &r.node, &r.docIdx, &r.start, &r.end); err != nil {
					return internal("scip scratch read: " + err.Error())
				}
				rows = append(rows, r)
				return nil
			})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, r := range rows {
			after = r.key + "\x00" + r.target
			d, ok, err := im.docByIndex(ctx, r.docIdx)
			if err != nil {
				return err
			}
			if !ok {
				return internal("scip scratch names a document that was never recorded")
			}
			ds, err := im.loadSource(ctx, d)
			if err != nil {
				return err
			}
			start, err := ds.cur.PositionAt(uint64(r.start))
			if err != nil {
				return err
			}
			end, err := ds.cur.PositionAt(uint64(r.end))
			if err != nil {
				return err
			}
			loc := location{file: d.file, rng: &model.SourceRange{Start: start, End: end}}
			target, err := parseSymbol(r.target)
			if err != nil {
				return err
			}
			to, err := im.targetNode(ctx, symKey(r.docIdx, target), target, loc)
			if err != nil {
				return err
			}
			if r.flags&relImplementation != 0 {
				if err := im.addEdge(ctx, model.NodeID(r.node), model.RelImplements, to, loc, target.raw, "implementation"); err != nil {
					return err
				}
			}
			if r.flags&(relReference|relTypeDefinition) != 0 {
				if err := im.addEdge(ctx, model.NodeID(r.node), model.RelReferences, to, loc, target.raw, "relationship"); err != nil {
					return err
				}
			}
		}
	}
}

func (im *importer) docByIndex(ctx context.Context, idx int64) (docRow, bool, error) {
	var d docRow
	var id, hash string
	found, err := im.sc.row(ctx, `SELECT idx, path, lang, enc, file_id, content_hash, size FROM docs WHERE idx = ?`, []any{idx},
		&d.idx, &d.path, &d.language, &d.encoding, &id, &hash, &d.file.Size)
	d.file.ID, d.file.ContentHash, d.file.Path = model.FileID(id), hash, d.path
	return d, found, err
}

// emitEdges publishes the grouped edges: one RelationFact per canonical
// relation with one evidence row per distinct occurrence range, in a fixed
// order. Occurrences beyond MaxEvidencePerFact on one edge are counted and
// reported through the partial capability state, never silently dropped.
func (im *importer) emitEdges(ctx context.Context) error {
	var current *model.RelationFact
	var overflow int64
	flush := func() error {
		if current == nil {
			return nil
		}
		fact := *current
		current = nil
		if err := im.sink.PutRelations(ctx, []model.RelationFact{fact}); err != nil {
			return err
		}
		im.records++
		return nil
	}
	err := im.sc.each(ctx, `SELECT frm, kind, dst, symbol, detail, start, end, sl, sc, el, ec, file_id, content_hash
		FROM edges ORDER BY frm, kind, dst, file_id, start, end, detail`, nil,
		func(scan func(...any) error) error {
			var frm, kind, dst, symbolText, detail, fileID, hash string
			var rng model.SourceRange
			if err := scan(&frm, &kind, &dst, &symbolText, &detail, &rng.Start.Byte, &rng.End.Byte, &rng.Start.Line, &rng.Start.Column, &rng.End.Line, &rng.End.Column, &fileID, &hash); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			rel := model.Relation{From: model.NodeID(frm), Kind: model.RelationKind(kind), To: model.NodeID(dst)}
			rel.ID = model.NewRelationID(im.req.Binding.RepositoryID, rel.From, rel.Kind, rel.To)
			if current != nil && current.Relation.ID != rel.ID {
				if err := flush(); err != nil {
					return err
				}
			}
			if current == nil {
				current = &model.RelationFact{Relation: rel}
			}
			loc := location{file: model.FileVersion{ID: model.FileID(fileID), ContentHash: hash}, rng: &rng}
			if len(current.Evidence) >= model.MaxEvidencePerFact {
				overflow++
				return nil
			}
			current.Evidence = append(current.Evidence, im.evidence("", rel.ID, loc, symbolText, detail))
			return nil
		})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if overflow > 0 {
		im.truncatedEdges = overflow
		im.degrade(model.CodeResourceLimit)
	}
	return nil
}

// result reports the run: succeeded with every capability fresh, or with
// each capability partial under the first degradation reason (an unverified
// binding, an unprocessable document, or truncated occurrence evidence).
func (im *importer) result() model.ProviderResult {
	state := model.CapabilityFresh
	if im.partialCode != "" {
		state = model.CapabilityPartial
	}
	r := model.ProviderResult{RunID: im.req.Run, State: model.RunSucceeded, RecordsEmitted: im.records, BytesProcessed: im.indexBytes}
	for _, c := range capabilities {
		r.Capabilities = append(r.Capabilities, model.CapabilityState{ProviderID: ID, Capability: c, Scope: im.req.Unit.ScopeKey, State: state, DiagnosticCode: im.partialCode})
	}
	return r
}
