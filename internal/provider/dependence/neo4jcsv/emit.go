package neo4jcsv

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/source"
)

// emitter turns the staged graph into the facts of one unit.
type emitter struct {
	sc   *scratch
	res  provider.Resolver
	sink provider.Sink
	opts Options
	// language is the export's own language, used for a node with no file.
	language string

	nodes, relations, aliases int
	external                  int // declarations the export marked external
	dropped                   int // entities refused: unmatched path or oversized attribute
	noRange                   int // located facts whose coordinates did not verify
	unresolved                int // assignment targets published as may_refer_to
	clipped                   int // evidence rows over MaxEvidencePerFact
}

// stageFiles records the pinned snapshot manifest so an engine FILENAME can be
// matched to the exact file version its facts must be bound to. The manifest
// is streamed into the scratch, never held in the heap.
func (e *emitter) stageFiles(ctx context.Context) error {
	return e.opts.Content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		return e.sc.exec(ctx, `INSERT OR REPLACE INTO files(path, file_id, content_hash, size, language) VALUES(?,?,?,?,?)`,
			fv.Path, string(fv.ID), fv.ContentHash, fv.Size, fv.Language)
	})
}

// locate resolves every published entity and every occurrence site to the
// pinned file and byte range it describes. Items are visited file by file so
// each file is read once; an entity whose path is not in the snapshot is
// dropped, because a fact bound to the wrong source is worse than no fact.
func (e *emitter) locate(ctx context.Context) error {
	if err := e.sc.commit(ctx); err != nil {
		return err
	}
	if _, err := e.sc.db.ExecContext(ctx, `INSERT OR IGNORE INTO loc(id, ok)
		SELECT id, -1 FROM ents UNION SELECT site, -1 FROM proj`); err != nil {
		return internalErr("import locate: %v", err)
	}
	after := ""
	for {
		files, err := e.pageStrings(ctx, `SELECT DISTINCT n.filename FROM loc l JOIN nodes n ON n.id = l.id
			WHERE n.filename > ? ORDER BY n.filename LIMIT ?`, after, pageSize)
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
	id, label, code, name string
	line, lineEnd, col    sql.NullInt64
	isExternal            bool
}

func (e *emitter) locateFile(ctx context.Context, filename string) error {
	rel := normalizePath(filename, e.opts.ProjectRoot, e.opts.UnitRoot)
	fv, data, found, err := e.openSource(ctx, rel)
	if err != nil {
		return err
	}
	var cursor *source.Cursor
	var lines *lineIndex
	if data != nil {
		cursor, lines = source.NewCursor(data), &lineIndex{data: data}
	}
	// Items are paged in line order, not id order, so the shared lineIndex
	// only ever scans forward. COALESCE keys the rows the export gave no
	// coordinates ahead of every real line and the id tie-break makes the
	// boundary total; the cursor starts below every representable line so a
	// line a malformed export made negative is still visited and refused.
	afterLine, afterID := int64(math.MinInt64), ""
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT n.id, n.label, n.code, n.name, n.line, n.line_end, n.col, n.is_external
			FROM loc l JOIN nodes n ON n.id = l.id
			WHERE n.filename = ? AND (COALESCE(n.line, -1), n.id) > (?, ?)
			ORDER BY COALESCE(n.line, -1), n.id LIMIT ?`, filename, afterLine, afterID, pageSize)
		if err != nil {
			return internalErr("import locate: %v", err)
		}
		var page []located
		for rows.Next() {
			var it located
			var ext int64
			if err := rows.Scan(&it.id, &it.label, &it.code, &it.name, &it.line, &it.lineEnd, &it.col, &ext); err != nil {
				rows.Close()
				return internalErr("import locate: %v", err)
			}
			it.isExternal = ext != 0
			page = append(page, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import locate: %v", err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, it := range page {
			if err := e.locateItem(ctx, it, rel, fv, found, cursor, lines, data != nil); err != nil {
				return err
			}
		}
		last := page[len(page)-1]
		afterLine, afterID = -1, last.id
		if last.line.Valid {
			afterLine = last.line.Int64
		}
	}
}

func (e *emitter) locateItem(ctx context.Context, it located, rel string, fv model.FileVersion, found bool,
	cursor *source.Cursor, lines *lineIndex, haveBytes bool) error {
	ok, rng := int64(1), ""
	switch {
	case it.label == labelMethod && it.isExternal:
		// A declaration in another unit. It has no location of its own unless
		// the export gave it one; the identity is structural and the
		// reconciler binds it to the real declaration by full name.
		e.external++
	case !found:
		// The path is not in the pinned snapshot. A published entity there
		// cannot be bound to source and is refused; an occurrence site keeps
		// its relation but carries no source binding.
		if _, isEnt := entityLabels[it.label]; isEnt {
			ok = 0
			e.dropped++
		} else {
			e.noRange++
		}
	default:
		if r := e.rangeOf(cursor, lines, it); r != nil {
			raw, _ := json.Marshal(r)
			rng = string(raw)
		} else if haveBytes {
			e.noRange++
		}
	}
	var fileID, hash, lang, storedPath string
	if found {
		fileID, hash, lang, storedPath = string(fv.ID), fv.ContentHash, fv.Language, rel
	}
	if err := e.sc.exec(ctx, `UPDATE loc SET ok = ?, path = ?, file_id = ?, content_hash = ?, language = ?, range_json = ? WHERE id = ?`,
		ok, storedPath, fileID, hash, lang, rng, it.id); err != nil {
		return err
	}
	return e.sc.staged(ctx)
}

// entityLabels are the labels a published entity can carry.
var entityLabels = map[string]struct{}{
	labelMethod: {}, labelLocal: {}, labelParamIn: {}, labelMember: {},
}

// openSource maps a root-relative path to the snapshot file it names and
// reads its bytes when they fit the range budget.
func (e *emitter) openSource(ctx context.Context, rel string) (fv model.FileVersion, data []byte, found bool, err error) {
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
		return fv, nil, false, internalErr("import files: %v", err)
	}
	fv = model.FileVersion{ID: model.FileID(id), Path: rel, ContentHash: hash, Size: size, Language: lang}
	if size > maxRangeFileBytes {
		return fv, nil, true, nil
	}
	rc, got, err := e.opts.Content.Open(ctx, fv.ID)
	if err != nil {
		return fv, nil, false, err
	}
	defer rc.Close()
	if got.ContentHash != hash {
		return fv, nil, false, &model.Error{Code: model.CodeSnapshotChanged,
			Message: "the pinned view returned another version of an analyzed file"}
	}
	data, err = io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return fv, nil, false, err
	}
	if int64(len(data)) != size {
		return fv, nil, false, &model.Error{Code: model.CodeSourceIntegrity,
			Message: "retained bytes differ in length from the manifest", Details: map[string]string{"file_id": id}}
	}
	return fv, data, true, nil
}

// normalizePath turns an engine FILENAME into a slash-separated root-relative
// path, or "" when it cannot be one. A synthetic name the frontend invents
// for a package or an include set is not a path and never matches a file.
func normalizePath(filename, root, unitRoot string) string {
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
	// The export's paths are relative to the directory the engine parsed, so a
	// unit below the repository root carries its own root as the prefix. The
	// result is still checked against the pinned snapshot, which is what
	// refuses a path the repository does not have.
	if unitRoot != "" {
		p = path.Join(unitRoot, p)
	}
	return p
}

// rangeOf converts one item's engine coordinates to a byte range verified
// against the pinned bytes. A method spans whole lines LINE_NUMBER..
// LINE_NUMBER_END; anything else is the exact bytes of its CODE, or of its
// NAME, at LINE_NUMBER/COLUMN_NUMBER when the source holds exactly that text
// there, otherwise its whole line. Columns are tried one-based and then
// zero-based: the match against the bytes is the proof, not the convention,
// and the frontends do not agree on one. Coordinates that do not land on the
// file yield no range.
func (e *emitter) rangeOf(cursor *source.Cursor, lines *lineIndex, it located) *model.SourceRange {
	if cursor == nil || !it.line.Valid || it.line.Int64 < 1 {
		return nil
	}
	lineStart, _, ok := lines.bounds(it.line.Int64)
	if !ok {
		return nil
	}
	start, end := lineStart, -1
	if it.col.Valid && it.label != labelMethod {
		for _, want := range [...]string{it.code, it.name} {
			if want == "" || strings.ContainsRune(want, '\n') {
				continue
			}
			// The engine truncates CODE at engineCodeChars characters. A
			// truncated prefix still matches the pinned bytes at the item's
			// column, so accepting it would publish a range that ends mid
			// expression: a wrong extent, not a missing one, and nothing
			// downstream could tell. Such a value is refused as a range and
			// the item falls through to its name and then to its line.
			if utf8.RuneCountInString(want) >= engineCodeChars {
				continue
			}
			for _, col := range [...]int64{it.col.Int64 - 1, it.col.Int64} {
				if col < 0 {
					continue
				}
				s := lineStart + int(col)
				if s >= 0 && s+len(want) <= len(lines.data) && bytes.Equal(lines.data[s:s+len(want)], []byte(want)) {
					start, end = s, s+len(want)
					break
				}
			}
			if end >= 0 {
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
// from its last answer.
type lineIndex struct {
	data  []byte
	line  int64
	start int
}

func (l *lineIndex) bounds(line int64) (start, end int, ok bool) {
	start, end, ok = l.scan(l.line, l.start, line)
	if !ok {
		return 0, 0, false
	}
	l.line, l.start = line, start
	return start, end, true
}

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

// declarationKey is the fixed cross-provider strong key for a declaration
// (controller ruling R11-3):
//
//	scope: "file:" + <root-relative slash path>
//	key:   "decl:" + <identifier token as written> + "@" + <path> +
//	       ":" + <start line> + "-" + <end line>
//
// Lines are one-based and the span is inclusive. The identifier is the token
// the producing tool saw, with no case folding or other normalization, so a
// structural provider that aliases its declarations under this key resolves
// to the same node this import does.
func declarationKey(name, rel string, line, lineEnd int64) string {
	return "decl:" + name + "@" + rel + ":" + strconv.FormatInt(line, 10) + "-" + strconv.FormatInt(lineEnd, 10)
}

// nativeMethodKey is the unit-independent alias key of a declaration, the one
// an external stub in another unit resolves through.
func nativeMethodKey(fullName string) string { return ProviderID + ":method:" + fullName }

// entity is one published graph entity being identified.
type entity struct {
	id, kind, label                  string
	name, canonical, fullName        string
	signature                        string
	ownerFullName                    string
	fileID, hash, lang, rng, relPath string
	code                             string
	line, lineEnd, col               sql.NullInt64
	external                         bool
}

// identify resolves every kept entity to canonical identity through the
// unit's resolver and stages the node fact, aliases and fact key it
// publishes.
func (e *emitter) identify(ctx context.Context) error {
	after := ""
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT n.id, t.kind, n.label, n.name, n.canonical_name, n.full_name, n.signature, n.line, n.line_end, n.col,
			t.external, l.file_id, l.content_hash, l.language, l.range_json, l.path, COALESCE(o.full_name, ''), COALESCE(n.code, '')
			FROM ents t JOIN nodes n ON n.id = t.id JOIN loc l ON l.id = t.id
			LEFT JOIN nodes o ON o.id = n.owner
			WHERE l.ok = 1 AND t.id > ? ORDER BY t.id LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("import identify: %v", err)
		}
		var page []entity
		for rows.Next() {
			var it entity
			var ext int64
			if err := rows.Scan(&it.id, &it.kind, &it.label, &it.name, &it.canonical, &it.fullName, &it.signature, &it.line, &it.lineEnd, &it.col,
				&ext, &it.fileID, &it.hash, &it.lang, &it.rng, &it.relPath, &it.ownerFullName, &it.code); err != nil {
				rows.Close()
				return internalErr("import identify: %v", err)
			}
			it.external = ext != 0
			page = append(page, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import identify: %v", err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, it := range page {
			if err := e.identifyOne(ctx, it); err != nil {
				return err
			}
		}
		after = page[len(page)-1].id
	}
}

func (e *emitter) identifyOne(ctx context.Context, it entity) error {
	name, kind := it.name, model.NodeFunction
	switch it.label {
	case labelMember:
		kind = model.NodeField
	case labelLocal, labelParamIn:
		kind = model.NodeVariable
	}
	if it.kind == kindUnresolved {
		kind = model.NodeVariable
	}
	if name == "" {
		name = it.canonical
	}
	if name == "" {
		name = truncate(it.code, model.MaxNameBytes)
	}
	qualified := it.fullName
	if qualified == "" && it.ownerFullName != "" && name != "" && it.kind != kindUnresolved {
		qualified = it.ownerFullName + "." + name
	}
	if name == "" {
		name = qualified
	}
	native := nativeMethodKey(qualified)
	switch it.kind {
	case kindDecl:
		native = ProviderID + ":decl:" + qualified + "@" + nullStr(it.line) + ":" + nullStr(it.col)
	case kindUnresolved:
		native = ProviderID + ":unresolved:" + it.ownerFullName + "#" + name
	}
	scope := e.opts.UnitScopeKey
	if it.relPath != "" && it.kind != kindUnresolved {
		scope = "file:" + it.relPath
	}
	if name == "" || len(name) > model.MaxNameBytes || len(qualified) > model.MaxQualifiedNameBytes ||
		len(it.signature) > model.MaxSignatureBytes || len(native) > model.MaxNativeKeyBytes ||
		len(scope) > model.MaxScopeKeyBytes {
		e.dropped++
		return e.sc.exec(ctx, `UPDATE loc SET ok = 0 WHERE id = ?`, it.id)
	}
	cand := model.NodeCandidate{ProviderID: ProviderID, ScopeKey: scope, NativeKey: native, Kind: kind,
		Name: name, QualifiedName: qualified, Signature: it.signature, Language: e.language}
	evFile, evHash, evRange := model.FileID(it.fileID), it.hash, parseRange(it.rng)
	if it.relPath != "" && it.kind != kindUnresolved {
		// A located declaration keys on its exact declaration range, which is
		// how a structural provider and this import mint one identity.
		cand.Language = it.lang
		cand.FileID, cand.ContentHash, cand.Range = evFile, evHash, evRange
		// The strong key is the declaration-location contract the structural
		// providers publish for the same declaration. A declaration with no
		// identifier token or no line has no such key and mints its own
		// identity instead.
		if it.name != "" && it.line.Valid {
			last := it.line.Int64
			if it.lineEnd.Valid && it.lineEnd.Int64 >= last {
				last = it.lineEnd.Int64
			}
			if key := declarationKey(it.name, it.relPath, it.line.Int64, last); len(key) <= model.MaxNativeKeyBytes {
				cand.StrongKey = key
			}
		}
	} else if it.kind == kindUnresolved {
		// A target shape the export did not bind is a provider-local
		// unresolved entity: no qualified name, no location, no merge with
		// another entity by short name (Section 9.4).
		cand.QualifiedName = ""
		qualified = ""
	}
	res, err := e.res.Resolve(ctx, cand)
	if err != nil {
		return err
	}
	node := res.Node
	if meta := resolutionMetadata(res, it.external, it.kind == kindUnresolved); meta != nil {
		node.Metadata = meta
	}
	detail := detailCall
	if it.kind != kindMethod {
		detail = detailAssignment
	}
	ev := e.evidence(node.ID, "", evFile, evHash, evRange, qualified, detail)
	fact := model.NodeFact{Node: node, CanonicalKey: res.CanonicalKey, Evidence: []model.Evidence{ev}}
	factJSON, err := json.Marshal(fact)
	if err != nil {
		return internalErr("import identify: %v", err)
	}
	aliasList := []model.NativeAlias{{ScopeKey: cand.ScopeKey, NativeKey: cand.NativeKey, NodeID: node.ID}}
	if it.kind == kindMethod && qualified != "" && cand.ScopeKey != provider.ScopeWorkspace {
		// The full-name alias is unit independent: it is how a stub in
		// another unit binds to this declaration.
		aliasList = append(aliasList, model.NativeAlias{ScopeKey: provider.ScopeWorkspace, NativeKey: native, NodeID: node.ID})
	}
	aliasJSON, err := json.Marshal(aliasList)
	if err != nil {
		return internalErr("import identify: %v", err)
	}
	label := "node:" + it.kind
	keyOwner := e.keyOwner(it.fullName, it.ownerFullName)
	positional := e.positional(it.fullName, it.ownerFullName, it.rng, it.name, it.signature)
	key := FactKey(label, keyOwner, it.relPath, "", name, positional, "")
	if err := e.sc.exec(ctx, `INSERT INTO ident(ent, node_id, fact_json, alias_json, key) VALUES(?,?,?,?,?)`,
		it.id, string(node.ID), string(factJSON), string(aliasJSON), key); err != nil {
		return err
	}
	if err := e.sc.staged(ctx); err != nil {
		return err
	}
	// Equally supported alternatives are retained as may_refer_to edges from
	// the chosen identity, never resolved by completion order.
	for _, alt := range res.Ambiguous {
		// The key must stay a digest: saveKeys unions relation keys into the
		// key set and LoadKeySet refuses any line that is not one, so a
		// composed key here would write a set the next refresh cannot load.
		altKey := FactKey("rel:may_refer_to", keyOwner, it.relPath, "ambiguous", string(alt), positional,
			string(node.ID)+keySep+string(alt))
		if err := e.stageRelation(ctx, node.ID, model.RelMayReferTo, alt, evFile, evHash, evRange,
			qualified, detailAssignment, altKey); err != nil {
			return err
		}
	}
	return nil
}

// Entity kinds staged in the scratch.
const (
	kindMethod     = "method"
	kindDecl       = "decl"
	kindUnresolved = "unres"
)

func parseRange(raw string) *model.SourceRange {
	if raw == "" {
		return nil
	}
	var r model.SourceRange
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil
	}
	return &r
}

// keyOwner is the fact key's owner component: the full name of the method (or
// type) the fact belongs to.
func (e *emitter) keyOwner(fullName, ownerFullName string) string {
	if fullName != "" {
		return fullName
	}
	return ownerFullName
}

// positional is the fact key's positional component: the ordered byte ranges
// of the fact, or a content digest when the owning method is a synthetic
// initializer whose member order the frontend does not fix.
func (e *emitter) positional(fullName, ownerFullName, rangeJSON string, content ...string) string {
	if isInitializer(fullName) || isInitializer(ownerFullName) {
		return contentDigest(content...)
	}
	return rangeDigest(rangeJSON)
}

func rangeDigest(rangeJSON ...string) string {
	parts := make([]string, 0, len(rangeJSON))
	for _, raw := range rangeJSON {
		if raw == "" {
			parts = append(parts, "-")
			continue
		}
		var r model.SourceRange
		if json.Unmarshal([]byte(raw), &r) != nil {
			parts = append(parts, "-")
			continue
		}
		parts = append(parts, strconv.FormatUint(r.Start.Byte, 10)+"-"+strconv.FormatUint(r.End.Byte, 10))
	}
	return strings.Join(parts, "|")
}

func resolutionMetadata(res model.Resolution, external, unresolved bool) json.RawMessage {
	switch {
	case unresolved || res.Basis == model.MatchUnresolved:
		return json.RawMessage(`{"resolution":"unresolved","candidates":0}`)
	case len(res.Ambiguous) > 0:
		return json.RawMessage(`{"resolution":"ambiguous","candidates":` + strconv.Itoa(len(res.Ambiguous)+1) + `}`)
	case external:
		return json.RawMessage(`{"resolution":"import","candidates":1}`)
	}
	return nil
}

func (e *emitter) evidence(node model.NodeID, rel model.RelationID, file model.FileID, hash string,
	rng *model.SourceRange, nativeKey, detail string) model.Evidence {
	if len(nativeKey) > model.MaxNativeKeyBytes {
		nativeKey = ""
	}
	ev := model.Evidence{UnitID: e.opts.Unit.ID, ProviderID: e.opts.Unit.ProviderID, ProviderVersion: e.opts.Unit.ProviderVersion,
		OriginRunID: e.opts.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionStaticAnalysis,
		FileID: file, ContentHash: hash, Range: rng, NativeKey: nativeKey, Detail: truncate(detail, model.MaxDetailBytes)}
	ev.ID = model.NewEvidenceID(ev)
	return ev
}

// stageRelations turns every projected occurrence whose endpoints both
// resolved into a relation identity with one evidence row per occurrence.
func (e *emitter) stageRelations(ctx context.Context) error {
	if err := e.sc.checkDerived(ctx); err != nil {
		return err
	}
	type key struct{ kind, from, to, site, op, target string }
	var after key
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT p.kind, p.from_e, p.to_e, p.site, p.op, p.target_name, p.detail,
			f.node_id, t.node_id, s.file_id, s.content_hash, s.range_json, s.path,
			COALESCE(o.full_name, ''), COALESCE(c.code, ''), COALESCE(c.name, '')
			FROM proj p JOIN ident f ON f.ent = p.from_e JOIN ident t ON t.ent = p.to_e JOIN loc s ON s.id = p.site
			LEFT JOIN nodes c ON c.id = p.site LEFT JOIN nodes o ON o.id = c.owner
			WHERE (p.kind, p.from_e, p.to_e, p.site, p.op, p.target_name) > (?,?,?,?,?,?)
			ORDER BY p.kind, p.from_e, p.to_e, p.site, p.op, p.target_name LIMIT ?`,
			after.kind, after.from, after.to, after.site, after.op, after.target, pageSize)
		if err != nil {
			return internalErr("import relations: %v", err)
		}
		type occ struct {
			key
			detail, from, to, fileID, hash, rng, relPath, owner, code, siteName string
		}
		var page []occ
		for rows.Next() {
			var o occ
			if err := rows.Scan(&o.kind, &o.key.from, &o.key.to, &o.site, &o.op, &o.key.target, &o.detail,
				&o.from, &o.to, &o.fileID, &o.hash, &o.rng, &o.relPath, &o.owner, &o.code, &o.siteName); err != nil {
				rows.Close()
				return internalErr("import relations: %v", err)
			}
			page = append(page, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import relations: %v", err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, o := range page {
			kind, ok := relationKinds[o.kind]
			if !ok {
				return internalErr("import relations: unknown projection %q", o.kind)
			}
			var rng *model.SourceRange
			if o.rng != "" {
				var r model.SourceRange
				if err := json.Unmarshal([]byte(o.rng), &r); err != nil {
					return internalErr("import relations: %v", err)
				}
				rng = &r
			}
			factKey := FactKey("rel:"+o.kind, o.owner, o.relPath, o.op, o.key.target,
				e.positional("", o.owner, o.rng, o.code, o.siteName, o.key.target), o.from+keySep+o.to)
			if err := e.stageRelation(ctx, model.NodeID(o.from), kind, model.NodeID(o.to), model.FileID(o.fileID),
				o.hash, rng, o.owner, o.detail, factKey); err != nil {
				return err
			}
		}
		after = page[len(page)-1].key
	}
}

// relationKinds maps a projection kind to its published relation.
var relationKinds = map[string]model.RelationKind{
	"calls":              model.RelCalls,
	"control_depends_on": model.RelControlDependsOn,
	"data_flows_to":      model.RelDataFlowsTo,
	"reads":              model.RelReads,
	"writes":             model.RelWrites,
	"may_refer_to":       model.RelMayReferTo,
}

// stageRelation stages one occurrence of one canonical edge. Occurrences with
// identical evidence identity collapse; distinct ranges stay distinct.
func (e *emitter) stageRelation(ctx context.Context, from model.NodeID, kind model.RelationKind, to model.NodeID,
	file model.FileID, hash string, rng *model.SourceRange, nativeKey, detail, factKey string) error {
	rel := model.Relation{From: from, Kind: kind, To: to}
	rel.ID = model.NewRelationID(e.opts.Repository, from, kind, to)
	ev := e.evidence("", rel.ID, file, hash, rng, nativeKey, detail)
	raw, err := json.Marshal(ev)
	if err != nil {
		return internalErr("import relations: %v", err)
	}
	if err := e.sc.exec(ctx, `INSERT OR IGNORE INTO rels(rel_id, ev_id, from_id, kind, to_id, ev_json, key)
		VALUES(?,?,?,?,?,?,?)`, string(rel.ID), string(ev.ID), string(from), string(kind), string(to), string(raw), factKey); err != nil {
		return err
	}
	return e.sc.staged(ctx)
}

// emitNodes hands every distinct identity to the sink, smallest entity first.
// Two entities that resolved to one identity publish one fact, because a unit
// publishes each node once — and that one fact carries the fact keys of every
// entity behind it. Storage drops a carried fact when ANY of its keys is
// replaced (Section 11.4), so a key left off here would strand the row it
// backs: the next refresh would classify that key as changed, storage would
// keep the old row because the key it kept the row under is not this fact's,
// and the unit would hold two descriptions of one identity.
func (e *emitter) emitNodes(ctx context.Context) error {
	var afterNode, afterEnt string
	var open *model.NodeFact
	var openNode string
	var openKeys []string
	var facts []model.NodeFact
	var keys [][]string
	closeGroup := func() {
		if open == nil {
			return
		}
		facts = append(facts, *open)
		keys = append(keys, sortedKeys(openKeys))
		open, openNode, openKeys = nil, "", nil
	}
	flush := func() error {
		closeGroup()
		if len(facts) == 0 {
			return nil
		}
		if err := e.putNodes(ctx, facts, keys); err != nil {
			return err
		}
		e.nodes += len(facts)
		facts, keys = nil, nil
		return nil
	}
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT node_id, ent, fact_json, key FROM ident
			WHERE (node_id, ent) > (?, ?) ORDER BY node_id, ent LIMIT ?`, afterNode, afterEnt, pageSize)
		if err != nil {
			return internalErr("import nodes: %v", err)
		}
		type row struct{ node, ent, raw, key string }
		var page []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.node, &r.ent, &r.raw, &r.key); err != nil {
				rows.Close()
				return internalErr("import nodes: %v", err)
			}
			page = append(page, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import nodes: %v", err)
		}
		if len(page) == 0 {
			return flush()
		}
		for _, r := range page {
			if open == nil || openNode != r.node {
				closeGroup()
				var f model.NodeFact
				if err := json.Unmarshal([]byte(r.raw), &f); err != nil {
					return internalErr("import nodes: %v", err)
				}
				open, openNode = &f, r.node
			}
			if r.key != "" {
				openKeys = append(openKeys, r.key)
			}
		}
		afterNode, afterEnt = page[len(page)-1].node, page[len(page)-1].ent
		// The identity still open may continue on the next page; everything
		// before it is complete.
		if len(facts) >= pageSize {
			hold, holdNode, holdKeys := open, openNode, openKeys
			open, openNode, openKeys = nil, "", nil
			if err := flush(); err != nil {
				return err
			}
			open, openNode, openKeys = hold, holdNode, holdKeys
		}
	}
}

// sortedKeys is one fact's key list in the order a keyed put requires:
// ascending and without duplicates. Duplicates are ordinary — two entities of
// one identity, or two occurrences of one edge, can share a key — and storage
// refuses a list that holds one.
func sortedKeys(keys []string) []string {
	slices.Sort(keys)
	return slices.Compact(keys)
}

// putNodes hands one batch to the sink with its keys, or without them when the
// sink cannot carry keys at all. A keyed put to a sink whose destination is
// not a provider.DeltaSink is refused by the sink rather than silently
// dropping the keys, so the fallback is on the sink's own interface, not on
// what its destination turns out to be.
func (e *emitter) putNodes(ctx context.Context, facts []model.NodeFact, keys [][]string) error {
	if d, ok := e.sink.(provider.DeltaSink); ok {
		return d.PutKeyedNodes(ctx, facts, keys)
	}
	return e.sink.PutNodes(ctx, facts)
}

// putRelations is putNodes for relation facts.
func (e *emitter) putRelations(ctx context.Context, facts []model.RelationFact, keys [][]string) error {
	if d, ok := e.sink.(provider.DeltaSink); ok {
		return d.PutKeyedRelations(ctx, facts, keys)
	}
	return e.sink.PutRelations(ctx, facts)
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
		var aliases []model.NativeAlias
		for _, raw := range page {
			var a []model.NativeAlias
			if err := json.Unmarshal([]byte(raw), &a); err != nil {
				return internalErr("import aliases: %v", err)
			}
			aliases = append(aliases, a...)
		}
		if err := e.sink.PutAliases(ctx, aliases); err != nil {
			return err
		}
		e.aliases += len(aliases)
		after = page[len(page)-1]
	}
}

// emitRelations streams the staged occurrences grouped by relation and hands
// each canonical edge, with its bounded evidence list and the fact keys of
// every occurrence behind it, to the sink. Node facts are always emitted,
// because they are the identities the kept rows still reference.
//
// A delta import filters whole relations, not occurrences: an edge is emitted
// if ANY of its occurrences carries a key the previous run did not publish,
// and it is then emitted entire. Filtering occurrences would publish an edge
// whose evidence held only the occurrences that moved, so the delta-built
// unit would differ from the same unit built in full — and storage, which
// drops a fact when any of its keys is replaced, would have nothing to carry
// the omitted evidence on.
//
// deltaFilter, not this function, decides whether filtering is allowed at all.
func (e *emitter) emitRelations(ctx context.Context, delta bool) error {
	filter := ""
	if delta {
		filter = ` AND EXISTS (SELECT 1 FROM rels o JOIN keys k ON k.key = o.key
			WHERE o.rel_id = rels.rel_id AND k.changed = 1)`
	}
	query := `SELECT rel_id, ev_id, from_id, kind, to_id, ev_json, key FROM rels
		WHERE (rel_id, ev_id) > (?, ?)` + filter + ` ORDER BY rel_id, ev_id LIMIT ?`
	var afterRel, afterEv string
	var current *model.RelationFact
	var currentKeys []string
	var batch []model.RelationFact
	var keys [][]string
	closeGroup := func() {
		if current == nil {
			return
		}
		batch = append(batch, *current)
		keys = append(keys, sortedKeys(currentKeys))
		current, currentKeys = nil, nil
	}
	flush := func() error {
		closeGroup()
		if len(batch) == 0 {
			return nil
		}
		if err := e.putRelations(ctx, batch, keys); err != nil {
			return err
		}
		e.relations += len(batch)
		batch, keys = nil, nil
		return nil
	}
	for {
		rows, err := e.sc.db.QueryContext(ctx, query, afterRel, afterEv, pageSize)
		if err != nil {
			return internalErr("import relations: %v", err)
		}
		type row struct{ rel, ev, from, kind, to, raw, key string }
		var page []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rel, &r.ev, &r.from, &r.kind, &r.to, &r.raw, &r.key); err != nil {
				rows.Close()
				return internalErr("import relations: %v", err)
			}
			page = append(page, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import relations: %v", err)
		}
		if len(page) == 0 {
			return flush()
		}
		for _, r := range page {
			if current == nil || string(current.Relation.ID) != r.rel {
				closeGroup()
				current = &model.RelationFact{Relation: model.Relation{ID: model.RelationID(r.rel),
					From: model.NodeID(r.from), Kind: model.RelationKind(r.kind), To: model.NodeID(r.to)}}
			}
			// The key is collected even for an occurrence past the evidence
			// bound: the occurrence still backs this edge, and a key left off
			// would leave storage carrying the previous row when that
			// occurrence alone changes.
			if r.key != "" {
				currentKeys = append(currentKeys, r.key)
			}
			if len(current.Evidence) >= model.MaxEvidencePerFact {
				e.clipped++
				continue
			}
			var ev model.Evidence
			if err := json.Unmarshal([]byte(r.raw), &ev); err != nil {
				return internalErr("import relations: %v", err)
			}
			current.Evidence = append(current.Evidence, ev)
		}
		afterRel, afterEv = page[len(page)-1].rel, page[len(page)-1].ev
		// The relation still open may continue on the next page; everything
		// before it is complete.
		if len(batch) >= pageSize {
			open, openKeys := current, currentKeys
			current, currentKeys = nil, nil
			if err := flush(); err != nil {
				return err
			}
			current, currentKeys = open, openKeys
		}
	}
}

// pageStrings runs a single-column keyset query.
func (e *emitter) pageStrings(ctx context.Context, query, after string, limit int) ([]string, error) {
	rows, err := e.sc.db.QueryContext(ctx, query, after, limit)
	if err != nil {
		return nil, internalErr("import scratch: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, internalErr("import scratch: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("import scratch: %v", err)
	}
	return out, nil
}

func nullStr(n sql.NullInt64) string {
	if !n.Valid {
		return ""
	}
	return strconv.FormatInt(n.Int64, 10)
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
