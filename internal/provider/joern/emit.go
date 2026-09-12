package joern

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/source"
)

// maxRangeFileBytes bounds the source a run holds in memory at once: one
// file's bytes, read through the pinned view, to convert Joern's line and
// column coordinates into verified byte ranges. A larger file keeps its
// facts, bound to the file but without a range (Section 9.3: absent, not
// zero-filled).
const maxRangeFileBytes = 4 << 20

// pageSize bounds every keyset page the emitter reads from the scratch.
const pageSize = 512

// emitter turns the staged graph into facts for one unit.
type emitter struct {
	req      provider.UnitRequest
	sink     provider.Sink
	sc       *scratch
	repo     model.RepositoryID
	language string // META_DATA language, for external methods
	// materializationRoot is the absolute directory Joern parsed; an absolute
	// FILENAME is accepted only under it.
	materializationRoot string

	records  uint64
	dropped  uint64 // methods refused: unmatched source path or oversized attribute
	clipped  uint64 // evidence rows over MaxEvidencePerFact
	noRange  uint64 // located facts whose coordinates did not verify
	matched  uint64 // methods resolved to a dependency's declaration
	external uint64
}

// locate resolves every projected method and call site to the pinned file
// and byte range it describes. Items are visited file by file so each file is
// read once; a method whose path is not in the snapshot is dropped (a fact
// bound to the wrong source is worse than no fact), an external method has no
// location by design.
func (e *emitter) locate(ctx context.Context) error {
	if _, err := e.sc.db.ExecContext(ctx, `INSERT OR IGNORE INTO loc(id, ok)
		SELECT from_m, -1 FROM proj UNION SELECT to_m, -1 FROM proj UNION SELECT site, -1 FROM proj`); err != nil {
		return internalErr("joern locate: %v", err)
	}
	after := ""
	for {
		files, err := e.pageStrings(ctx, `SELECT DISTINCT n.filename FROM loc l JOIN nodes n ON n.id = l.id WHERE n.filename > ? ORDER BY n.filename LIMIT ?`, after, pageSize)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return nil
		}
		for _, name := range files {
			if err := e.locateFile(ctx, name); err != nil {
				return err
			}
		}
		after = files[len(files)-1]
	}
}

// located is one item of locateFile's page.
type located struct {
	id, label, code    string
	line, lineEnd, col sql.NullInt64
	isExternal         bool
}

func (e *emitter) locateFile(ctx context.Context, filename string) error {
	fv, data, found, err := e.openSource(ctx, filename)
	if err != nil {
		return err
	}
	var cursor *source.Cursor
	var lines *lineIndex
	if data != nil {
		cursor, lines = source.NewCursor(data), &lineIndex{data: data}
	}
	// Items are paged in line order, not id order, so the shared lineIndex
	// only ever scans forward: a page break never sends it back to byte 0.
	// COALESCE keys the NULL lines (nodes the export gave no coordinates)
	// ahead of every real line, and the id tie-break makes the boundary
	// total. The cursor starts below every representable line so that no row
	// can be filtered out of the first page: a line a malformed export made
	// negative must still be visited and refused, never silently skipped.
	afterLine, afterID := int64(math.MinInt64), ""
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT n.id, n.label, n.code, n.line, n.line_end, n.col, n.is_external FROM loc l JOIN nodes n ON n.id = l.id
			WHERE n.filename = ? AND (COALESCE(n.line, -1), n.id) > (?, ?) ORDER BY COALESCE(n.line, -1), n.id LIMIT ?`, filename, afterLine, afterID, pageSize)
		if err != nil {
			return internalErr("joern locate: %v", err)
		}
		var page []located
		for rows.Next() {
			var it located
			var ext int64
			if err := rows.Scan(&it.id, &it.label, &it.code, &it.line, &it.lineEnd, &it.col, &ext); err != nil {
				rows.Close()
				return internalErr("joern locate: %v", err)
			}
			it.isExternal = ext != 0
			page = append(page, it)
		}
		rows.Close()
		if len(page) == 0 {
			return nil
		}
		for _, it := range page {
			ok, rng := int64(1), ""
			switch {
			case it.label == labelMethod && it.isExternal:
				// No location by design; the identity is structural.
				e.external++
				if _, err := e.sc.db.ExecContext(ctx, `UPDATE loc SET ok = 1 WHERE id = ?`, it.id); err != nil {
					return internalErr("joern locate: %v", err)
				}
				continue
			case !found && it.label == labelMethod:
				ok = 0
				e.dropped++
			case !found:
				// A call site in a file the snapshot does not hold: the
				// relation stays, its occurrence carries no source binding.
				e.noRange++
			default:
				if r := e.rangeOf(cursor, lines, it); r != nil {
					raw, _ := json.Marshal(r)
					rng = string(raw)
				} else if data != nil {
					e.noRange++
				}
			}
			var fileID, hash, lang string
			if found {
				fileID, hash, lang = string(fv.ID), fv.ContentHash, fv.Language
			}
			if _, err := e.sc.db.ExecContext(ctx, `UPDATE loc SET ok = ?, file_id = ?, content_hash = ?, language = ?, range_json = ? WHERE id = ?`,
				ok, fileID, hash, lang, rng, it.id); err != nil {
				return internalErr("joern locate: %v", err)
			}
		}
		last := page[len(page)-1]
		afterLine, afterID = -1, last.id
		if last.line.Valid {
			afterLine = last.line.Int64
		}
	}
}

// openSource maps a Joern FILENAME to the snapshot file it names and reads
// its bytes when they fit the range budget. Absolute paths are accepted only
// when they lie under the materialization root; anything that does not
// normalize to a root-relative snapshot path is not found.
func (e *emitter) openSource(ctx context.Context, filename string) (fv model.FileVersion, data []byte, found bool, err error) {
	rel := normalizePath(filename, e.materializationRoot)
	if rel == "" {
		return fv, nil, false, nil
	}
	var size int64
	var id, hash, lang string
	row := e.sc.db.QueryRowContext(ctx, `SELECT file_id, content_hash, size, language FROM files WHERE path = ?`, rel)
	if err := row.Scan(&id, &hash, &size, &lang); err != nil {
		if err == sql.ErrNoRows {
			return fv, nil, false, nil
		}
		return fv, nil, false, internalErr("joern files: %v", err)
	}
	fv = model.FileVersion{ID: model.FileID(id), Path: rel, ContentHash: hash, Size: size, Language: lang}
	if size > maxRangeFileBytes {
		return fv, nil, true, nil
	}
	rc, got, err := e.req.Content.Open(ctx, fv.ID)
	if err != nil {
		return fv, nil, false, err
	}
	defer rc.Close()
	if got.ContentHash != hash {
		return fv, nil, false, &model.Error{Code: model.CodeSnapshotChanged, Message: "the pinned view returned another version of a materialized file"}
	}
	data, err = io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return fv, nil, false, err
	}
	if int64(len(data)) != size {
		return fv, nil, false, &model.Error{Code: model.CodeSourceIntegrity, Message: "retained bytes differ in length from the manifest", Details: map[string]string{"file_id": id}}
	}
	return fv, data, true, nil
}

// normalizePath turns a Joern FILENAME into a slash-separated root-relative
// path, or "" when it cannot be one.
func normalizePath(filename, root string) string {
	if filename == "" || strings.HasPrefix(filename, "<") {
		return ""
	}
	p := filename
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return ""
		}
		p = rel
	}
	p = path.Clean(filepath.ToSlash(p))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return ""
	}
	return p
}

// rangeOf converts one item's Joern coordinates to a verified byte range. A
// METHOD spans whole lines LINE_NUMBER..LINE_NUMBER_END. A CALL site is the
// exact bytes of its CODE at LINE_NUMBER/COLUMN_NUMBER when the source holds
// exactly that text there (columns are tried one-based, then zero-based: the
// match against the bytes is the proof, not the convention), otherwise its
// whole line. Coordinates that do not land on the file yield no range.
func (e *emitter) rangeOf(cursor *source.Cursor, lines *lineIndex, it located) *model.SourceRange {
	if cursor == nil || !it.line.Valid || it.line.Int64 < 1 {
		return nil
	}
	lineStart, _, ok := lines.bounds(it.line.Int64)
	if !ok {
		return nil
	}
	start, end := lineStart, -1
	if it.label == labelCall && it.code != "" && it.col.Valid {
		for _, col := range []int64{it.col.Int64 - 1, it.col.Int64} {
			if col < 0 {
				continue
			}
			s := start + int(col)
			if s+len(it.code) <= len(lines.data) && bytes.Equal(lines.data[s:s+len(it.code)], []byte(it.code)) {
				start, end = s, s+len(it.code)
				break
			}
		}
	}
	if end < 0 {
		last := it.line.Int64
		if it.label == labelMethod && it.lineEnd.Valid && it.lineEnd.Int64 >= last {
			last = it.lineEnd.Int64
		}
		// Resolved without moving the shared cursor: a method's last line is
		// beyond the items that follow it in line order, and parking there
		// would force the next one to rescan from byte 0.
		_, lineEnd, ok := lines.boundsFrom(it.line.Int64, lineStart, last)
		if !ok {
			return nil
		}
		end = lineEnd
	}
	s, err := cursor.PositionAt(uint64(start))
	if err != nil {
		return nil
	}
	en, err := cursor.PositionAt(uint64(end))
	if err != nil {
		return nil
	}
	r := &model.SourceRange{Start: s, End: en}
	if r.Validate("range") != nil {
		return nil
	}
	return r
}

// lineIndex finds the content bounds of a one-based line by scanning forward
// from its last answer. locateFile asks for lines in increasing order and
// rangeOf never parks it on a method's end line, so one file costs one pass;
// an out-of-order request is still correct, it just rescans from byte 0.
type lineIndex struct {
	data  []byte
	line  int64
	start int
}

// bounds resolves line and advances the shared cursor to it.
func (l *lineIndex) bounds(line int64) (start, end int, ok bool) {
	start, end, ok = l.scan(l.line, l.start, line)
	if !ok {
		return 0, 0, false
	}
	l.line, l.start = line, start
	return start, end, true
}

// boundsFrom resolves line from a caller-held position without moving the
// shared cursor.
func (l *lineIndex) boundsFrom(fromLine int64, fromStart int, line int64) (start, end int, ok bool) {
	return l.scan(fromLine, fromStart, line)
}

func (l *lineIndex) scan(fromLine int64, fromStart int, line int64) (start, end int, ok bool) {
	if fromLine == 0 || line < fromLine {
		fromLine, fromStart = 1, 0
	}
	start = fromStart
	for fromLine < line {
		next := bytes.IndexByte(l.data[start:], '\n')
		if next < 0 {
			return 0, 0, false
		}
		start += next + 1
		fromLine++
	}
	if start > len(l.data) || (start == len(l.data) && line > 1 && (len(l.data) == 0 || l.data[len(l.data)-1] == '\n')) {
		return 0, 0, false
	}
	end = len(l.data)
	if next := bytes.IndexByte(l.data[start:], '\n'); next >= 0 {
		end = start + next
	}
	if end > start && l.data[end-1] == '\r' {
		end--
	}
	return start, end, true
}

// identify resolves every kept METHOD to canonical identity through the
// unit's resolver and stages the node fact and alias it publishes. The
// candidate's strong key is the declaration location contract the structural
// provider emits for the same declaration (Ruling R11-3); a hit adopts that
// identity, a miss mints a Joern-local one from the same file and range.
func (e *emitter) identify(ctx context.Context) error {
	after := ""
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT n.id, n.name, n.full_name, n.signature, n.filename, n.line, n.line_end, n.is_external,
			l.file_id, l.content_hash, l.language, l.range_json FROM loc l JOIN nodes n ON n.id = l.id
			WHERE n.label = 'METHOD' AND l.ok = 1 AND n.id > ? ORDER BY n.id LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("joern identify: %v", err)
		}
		type methodRow struct {
			id, name, fullName, signature, filename, fileID, hash, lang, rng string
			line, lineEnd                                                    sql.NullInt64
			external                                                         int64
		}
		var page []methodRow
		for rows.Next() {
			var m methodRow
			if err := rows.Scan(&m.id, &m.name, &m.fullName, &m.signature, &m.filename, &m.line, &m.lineEnd, &m.external, &m.fileID, &m.hash, &m.lang, &m.rng); err != nil {
				rows.Close()
				return internalErr("joern identify: %v", err)
			}
			page = append(page, m)
		}
		rows.Close()
		if len(page) == 0 {
			return nil
		}
		for _, m := range page {
			name := m.name
			if name == "" {
				name = m.fullName
			}
			native := "joern:method:" + m.fullName
			if m.fullName == "" {
				native = "joern:method:" + name
			}
			if name == "" || len(name) > model.MaxNameBytes || len(m.fullName) > model.MaxQualifiedNameBytes ||
				len(m.signature) > model.MaxSignatureBytes || len(native) > model.MaxNativeKeyBytes {
				e.dropped++
				if _, err := e.sc.db.ExecContext(ctx, `UPDATE loc SET ok = 0 WHERE id = ?`, m.id); err != nil {
					return internalErr("joern identify: %v", err)
				}
				continue
			}
			cand := model.NodeCandidate{ProviderID: providerID, ScopeKey: provider.ScopeWorkspace, NativeKey: native, Kind: model.NodeFunction,
				Name: name, QualifiedName: m.fullName, Signature: m.signature, Language: e.language}
			detail := "joern:METHOD external"
			if m.external == 0 {
				rel := normalizePath(m.filename, e.materializationRoot)
				cand.ScopeKey = "file:" + rel
				cand.Language = m.lang
				cand.FileID, cand.ContentHash = model.FileID(m.fileID), m.hash
				if m.rng != "" {
					var r model.SourceRange
					if err := json.Unmarshal([]byte(m.rng), &r); err != nil {
						return internalErr("joern identify: %v", err)
					}
					cand.Range = &r
				}
				// The strong key carries the identifier token exactly as the
				// export wrote it; a method with no NAME has no such token and
				// therefore no strong key (the FULL_NAME fallback used for the
				// candidate's Name would not be the contracted string).
				if m.line.Valid && m.name != "" {
					last := m.line.Int64
					if m.lineEnd.Valid && m.lineEnd.Int64 >= last {
						last = m.lineEnd.Int64
					}
					if key := declarationKey(m.name, rel, m.line.Int64, last); len(key) <= model.MaxNativeKeyBytes {
						cand.StrongKey = key
					}
				}
				detail = "joern:METHOD"
			}
			res, err := e.req.Resolver.Resolve(ctx, cand)
			if err != nil {
				return err
			}
			node := res.Node
			if res.Basis == model.MatchNativeKey {
				e.matched++
			}
			switch {
			case res.Basis == model.MatchUnresolved:
				node.Metadata = json.RawMessage(`{"resolution":"unresolved"}`)
			case m.external != 0:
				node.Metadata = json.RawMessage(`{"joern_external":true}`)
			}
			ev := model.Evidence{UnitID: e.req.Unit.ID, ProviderID: e.req.Unit.ProviderID, ProviderVersion: e.req.Unit.ProviderVersion, OriginRunID: e.req.Run,
				NodeID: node.ID, Precision: model.PrecisionStaticAnalysis, FileID: cand.FileID, ContentHash: cand.ContentHash, Range: cand.Range,
				NativeKey: m.fullName, Detail: detail}
			ev.ID = model.NewEvidenceID(ev)
			fact := model.NodeFact{Node: node, CanonicalKey: res.CanonicalKey, Evidence: []model.Evidence{ev}}
			factJSON, _ := json.Marshal(fact)
			aliasJSON, _ := json.Marshal(model.NativeAlias{ScopeKey: cand.ScopeKey, NativeKey: cand.NativeKey, NodeID: node.ID})
			if _, err := e.sc.db.ExecContext(ctx, `INSERT INTO ident(method, node_id, fact_json, alias_json) VALUES(?, ?, ?, ?)`, m.id, string(node.ID), string(factJSON), string(aliasJSON)); err != nil {
				return internalErr("joern identify: %v", err)
			}
			// Ambiguous alternatives are retained as may_refer_to edges from
			// the chosen identity, never resolved by completion order.
			for _, alt := range res.Ambiguous {
				if err := e.stageRelation(ctx, node.ID, model.RelMayReferTo, alt, cand.FileID, cand.ContentHash, cand.Range, m.fullName, "joern:METHOD ambiguous"); err != nil {
					return err
				}
			}
		}
		after = page[len(page)-1].id
	}
}

// declarationKey is the fixed cross-provider strong key for a declaration
// (controller ruling, fix round 1):
//
//	scope: "file:" + <root-relative slash path>
//	key:   "decl:" + <identifier token as written> + "@" + <root-relative slash path> +
//	       ":" + <start line> + "-" + <end line>
//
// Lines are one-based and the span is inclusive. The identifier is the token
// the producing tool saw, with no case folding, unqualifying or other
// normalization. A structural provider that aliases its declarations under
// this key in scope "file:"+path makes the Joern method resolve to the same
// node.
func declarationKey(name, rel string, line, lineEnd int64) string {
	return "decl:" + name + "@" + rel + ":" + strconv.FormatInt(line, 10) + "-" + strconv.FormatInt(lineEnd, 10)
}

// emitNodes hands every distinct identity to the sink, smallest ID first.
// Two Joern methods that resolved to one identity publish one fact (the one
// from the smaller Joern id), because a unit publishes each node once.
func (e *emitter) emitNodes(ctx context.Context) error {
	after := ""
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT i.node_id, i.fact_json FROM ident i
			WHERE i.node_id > ? AND i.method = (SELECT MIN(method) FROM ident j WHERE j.node_id = i.node_id) ORDER BY i.node_id LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("joern nodes: %v", err)
		}
		var facts []model.NodeFact
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return internalErr("joern nodes: %v", err)
			}
			var f model.NodeFact
			if err := json.Unmarshal([]byte(raw), &f); err != nil {
				rows.Close()
				return internalErr("joern nodes: %v", err)
			}
			facts = append(facts, f)
			after = id
		}
		rows.Close()
		if len(facts) == 0 {
			return nil
		}
		if err := e.sink.PutNodes(ctx, facts); err != nil {
			return err
		}
		e.records += uint64(len(facts))
	}
}

// emitAliases publishes each distinct (scope, native key, node) once.
func (e *emitter) emitAliases(ctx context.Context) error {
	after := ""
	for {
		page, err := e.pageStrings(ctx, `SELECT DISTINCT alias_json FROM ident WHERE alias_json > ? ORDER BY alias_json LIMIT ?`, after, pageSize)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		aliases := make([]model.NativeAlias, 0, len(page))
		for _, raw := range page {
			var a model.NativeAlias
			if err := json.Unmarshal([]byte(raw), &a); err != nil {
				return internalErr("joern aliases: %v", err)
			}
			aliases = append(aliases, a)
		}
		if err := e.sink.PutAliases(ctx, aliases); err != nil {
			return err
		}
		e.records += uint64(len(aliases))
		after = page[len(page)-1]
	}
}

// stageRelations turns projected occurrences whose endpoints both resolved
// into relation identities with one evidence row per occurrence.
func (e *emitter) stageRelations(ctx context.Context) error {
	type key struct{ kind, from, to, site, variable string }
	var after key
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT p.kind, p.from_m, p.to_m, p.site, p.variable, f.node_id, t.node_id,
			s.file_id, s.content_hash, s.range_json, COALESCE(c.method_full_name, '')
			FROM proj p JOIN ident f ON f.method = p.from_m JOIN ident t ON t.method = p.to_m JOIN loc s ON s.id = p.site
			LEFT JOIN nodes c ON c.id = p.site
			WHERE (p.kind, p.from_m, p.to_m, p.site, p.variable) > (?, ?, ?, ?, ?)
			ORDER BY p.kind, p.from_m, p.to_m, p.site, p.variable LIMIT ?`, after.kind, after.from, after.to, after.site, after.variable, pageSize)
		if err != nil {
			return internalErr("joern relations: %v", err)
		}
		type occ struct {
			key
			from, to, fileID, hash, rng, target string
		}
		var page []occ
		for rows.Next() {
			var o occ
			if err := rows.Scan(&o.kind, &o.key.from, &o.key.to, &o.site, &o.variable, &o.from, &o.to, &o.fileID, &o.hash, &o.rng, &o.target); err != nil {
				rows.Close()
				return internalErr("joern relations: %v", err)
			}
			page = append(page, o)
		}
		rows.Close()
		if len(page) == 0 {
			return nil
		}
		for _, o := range page {
			var rng *model.SourceRange
			if o.rng != "" {
				var r model.SourceRange
				if err := json.Unmarshal([]byte(o.rng), &r); err != nil {
					return internalErr("joern relations: %v", err)
				}
				rng = &r
			}
			var kind model.RelationKind
			var detail string
			switch o.kind {
			case "calls":
				kind, detail = model.RelCalls, "joern:CALL"
			case "control_depends_on":
				kind, detail = model.RelControlDependsOn, "joern:CDG"
			case "data_flows_to":
				kind, detail = model.RelDataFlowsTo, "joern:REACHING_DEF intraprocedural"
				if o.variable != "" {
					detail += " var=" + o.variable
				}
			default:
				return internalErr("joern relations: unknown projection %q", o.kind)
			}
			if err := e.stageRelation(ctx, model.NodeID(o.from), kind, model.NodeID(o.to), model.FileID(o.fileID), o.hash, rng, o.target, detail); err != nil {
				return err
			}
		}
		last := page[len(page)-1]
		after = last.key
	}
}

// stageRelation stages one occurrence of one canonical edge. Occurrences
// with identical evidence identity collapse; distinct ranges stay distinct.
func (e *emitter) stageRelation(ctx context.Context, from model.NodeID, kind model.RelationKind, to model.NodeID,
	file model.FileID, hash string, rng *model.SourceRange, nativeKey, detail string) error {
	rel := model.Relation{From: from, Kind: kind, To: to}
	rel.ID = model.NewRelationID(e.repo, from, kind, to)
	if len(nativeKey) > model.MaxNativeKeyBytes {
		nativeKey = ""
	}
	ev := model.Evidence{UnitID: e.req.Unit.ID, ProviderID: e.req.Unit.ProviderID, ProviderVersion: e.req.Unit.ProviderVersion, OriginRunID: e.req.Run,
		RelationID: rel.ID, Precision: model.PrecisionStaticAnalysis, FileID: file, ContentHash: hash, Range: rng,
		NativeKey: nativeKey, Detail: truncate(detail, model.MaxDetailBytes)}
	ev.ID = model.NewEvidenceID(ev)
	raw, _ := json.Marshal(ev)
	if _, err := e.sc.db.ExecContext(ctx, `INSERT OR IGNORE INTO rels(rel_id, ev_id, from_id, kind, to_id, ev_json) VALUES(?, ?, ?, ?, ?, ?)`,
		string(rel.ID), string(ev.ID), string(from), string(kind), string(to), string(raw)); err != nil {
		return internalErr("joern relations: %v", err)
	}
	return e.sc.staged()
}

// emitRelations streams the staged occurrences grouped by relation, in
// relation then evidence order, and hands each canonical edge with its
// bounded evidence list to the sink.
func (e *emitter) emitRelations(ctx context.Context) error {
	var afterRel, afterEv string
	var current *model.RelationFact
	var batch []model.RelationFact
	flush := func() error {
		if current != nil {
			batch = append(batch, *current)
			current = nil
		}
		if len(batch) == 0 {
			return nil
		}
		if err := e.sink.PutRelations(ctx, batch); err != nil {
			return err
		}
		e.records += uint64(len(batch))
		batch = nil
		return nil
	}
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT rel_id, ev_id, from_id, kind, to_id, ev_json FROM rels
			WHERE (rel_id, ev_id) > (?, ?) ORDER BY rel_id, ev_id LIMIT ?`, afterRel, afterEv, pageSize)
		if err != nil {
			return internalErr("joern relations: %v", err)
		}
		type row struct{ rel, ev, from, kind, to, raw string }
		var page []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rel, &r.ev, &r.from, &r.kind, &r.to, &r.raw); err != nil {
				rows.Close()
				return internalErr("joern relations: %v", err)
			}
			page = append(page, r)
		}
		rows.Close()
		if len(page) == 0 {
			return flush()
		}
		for _, r := range page {
			if current == nil || string(current.Relation.ID) != r.rel {
				if current != nil {
					batch = append(batch, *current)
				}
				current = &model.RelationFact{Relation: model.Relation{ID: model.RelationID(r.rel), From: model.NodeID(r.from), Kind: model.RelationKind(r.kind), To: model.NodeID(r.to)}}
			}
			if len(current.Evidence) >= model.MaxEvidencePerFact {
				e.clipped++
				continue
			}
			var ev model.Evidence
			if err := json.Unmarshal([]byte(r.raw), &ev); err != nil {
				return internalErr("joern relations: %v", err)
			}
			current.Evidence = append(current.Evidence, ev)
		}
		afterRel, afterEv = page[len(page)-1].rel, page[len(page)-1].ev
		// The relation still open may continue on the next page; everything
		// before it is complete.
		if len(batch) >= pageSize {
			open := current
			current = nil
			if err := flush(); err != nil {
				return err
			}
			current = open
		}
	}
}

// pageStrings runs a single-column keyset query.
func (e *emitter) pageStrings(ctx context.Context, query string, after string, limit int) ([]string, error) {
	rows, err := e.sc.db.QueryContext(ctx, query, after, limit)
	if err != nil {
		return nil, internalErr("joern scratch: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, internalErr("joern scratch: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("joern scratch: %v", err)
	}
	return out, nil
}

// truncate cuts s to at most limit bytes on a rune boundary.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
