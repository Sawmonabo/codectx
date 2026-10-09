package graphcsv

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

	nodes, aliases int
	external       int // declarations the export marked external
	dropped        int // entities refused: unmatched path or oversized attribute
	noRange        int // located facts whose coordinates did not verify
	clipped        int // evidence rows over the effective per-fact clip
	// truncatedFields counts, by field name, the descriptive storage values
	// this import cut to their model ceiling before writing them to the sink.
	// The model accepts an oversize storage field, so the cut is the
	// producer's to make and the count is what keeps it from being silent.
	truncatedFields map[string]int
}

// cut bounds one descriptive storage field and records the cut by field name.
// Identity fields (ids, paths, scope and native keys) are never cut here: a
// shortened join key names a different thing, so an oversize one drops the
// entity instead.
func (e *emitter) cut(field, value string, max int) string {
	bounded, original := model.TruncateField(value, max)
	if original > len(bounded) {
		if e.truncatedFields == nil {
			e.truncatedFields = map[string]int{}
		}
		e.truncatedFields[field]++
	}
	return bounded
}

// stageFiles records the pinned snapshot manifest so an engine FILENAME can be
// matched to the exact file version its facts must be bound to, marking the
// files the caller handed the engine (Options.Expected) so the ones the export
// has no file node for can be counted. The manifest is streamed into the
// scratch, never held in the heap.
func (e *emitter) stageFiles(ctx context.Context) error {
	return e.opts.Content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		expected := e.opts.Expected != nil && e.opts.Expected(fv)
		return e.sc.putFile(ctx, fv.Path, string(fv.ID), fv.ContentHash, fv.Size, fv.Language, expected)
	})
}

// located is one item of the location pass.
type located struct {
	seq, id            int64
	pathID             sql.NullInt64
	filename           string
	label, code, name  string
	line, lineEnd, col sql.NullInt64
	isExternal         bool
}

// sourceFile is one snapshot file opened for the location pass: its manifest
// row, its bytes when they fit the range budget, and whether the path was in
// the snapshot at all.
type sourceFile struct {
	row    sql.NullInt64
	fv     model.FileVersion
	found  bool
	cursor *source.Cursor
	lines  *lineIndex
	bytes  bool
}

// locate resolves every published entity and every occurrence site to the
// pinned file and byte range it describes. Items are visited in file order
// and, within a file, in line order, so each file is read once and the line
// index only ever scans forward; an entity whose path is not in the snapshot
// is dropped, because a fact bound to the wrong source is worse than no fact.
//
// The item list is built once, in that order, and the locations are appended
// as they are found and then copied once into node order for the phases that
// join on them.
func (e *emitter) locate(ctx context.Context) error {
	// COALESCE keys the rows the export gave no coordinates ahead of every
	// real line, and the id tie-break makes the order total.
	steps := [...]string{
		`CREATE TABLE todo(seq INTEGER PRIMARY KEY, node INTEGER NOT NULL, path INTEGER)`,
		`INSERT INTO todo(node, path) SELECT x.id, a.path FROM (SELECT id FROM ents UNION SELECT site FROM projs) x
			JOIN nodes n ON n.id = x.id JOIN attr a ON a.id = x.id ORDER BY a.path, COALESCE(n.line, -1), x.id`,
		`CREATE TABLE loc_in(seq INTEGER PRIMARY KEY, node INTEGER NOT NULL, ok INTEGER NOT NULL, file INTEGER, range_text TEXT NOT NULL)`,
	}
	for _, q := range steps {
		if err := e.sc.run(ctx, "locate", q); err != nil {
			return err
		}
	}
	var after int64
	var current *sourceFile
	var currentPath sql.NullInt64
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT t.seq, t.node, t.path, COALESCE(p.path, ''), n.label, n.name,
			n.line, n.line_end, n.col, n.is_external,
			COALESCE((SELECT c.code FROM code c WHERE c.id = t.node ORDER BY c.seq LIMIT 1), '')
			FROM todo t LEFT JOIN paths p ON p.id = t.path JOIN nodes n ON n.id = t.node
			WHERE t.seq > ? ORDER BY t.seq LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("import locate: %v", err)
		}
		var page []located
		for rows.Next() {
			var it located
			var ext int64
			if err := rows.Scan(&it.seq, &it.id, &it.pathID, &it.filename, &it.label, &it.name, &it.line, &it.lineEnd, &it.col, &ext, &it.code); err != nil {
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
			break
		}
		for _, it := range page {
			if current == nil || it.pathID != currentPath {
				f, err := e.openSource(ctx, normalizePath(it.filename, e.opts.ProjectRoot, e.opts.UnitRoot))
				if err != nil {
					return err
				}
				current, currentPath = f, it.pathID
			}
			if err := e.locateItem(ctx, it, current); err != nil {
				return err
			}
		}
		after = page[len(page)-1].seq
	}
	for _, q := range [...]string{
		`CREATE TABLE loc(node INTEGER PRIMARY KEY, ok INTEGER NOT NULL, file INTEGER, range_text TEXT NOT NULL)`,
		`INSERT OR IGNORE INTO loc SELECT node, ok, file, range_text FROM loc_in ORDER BY node, seq`,
	} {
		if err := e.sc.run(ctx, "locate", q); err != nil {
			return err
		}
	}
	return nil
}

func (e *emitter) locateItem(ctx context.Context, it located, f *sourceFile) error {
	ok, rng := int64(1), ""
	switch {
	case it.label == labelMethod && it.isExternal:
		// A declaration in another unit. It has no location of its own unless
		// the export gave it one; the identity is structural and the
		// reconciler binds it to the real declaration by full name.
		e.external++
	case !f.found:
		// The path is not in the pinned snapshot. A published entity there
		// cannot be bound to source and is refused; an occurrence site keeps
		// its relation but carries no source binding.
		if it.label == labelMethod {
			ok = 0
			e.dropped++
		} else {
			e.noRange++
		}
	default:
		if r := e.rangeOf(f.cursor, f.lines, it); r != nil {
			rng = formatRange(r)
		} else if f.bytes {
			e.noRange++
		}
	}
	if err := e.sc.exec(ctx, `INSERT INTO loc_in(node, ok, file, range_text) VALUES(?, ?, ?, ?)`, it.id, ok, f.row, rng); err != nil {
		return err
	}
	return e.sc.staged(ctx)
}

// openSource maps a root-relative path to the snapshot file it names and
// reads its bytes when they fit the range budget.
func (e *emitter) openSource(ctx context.Context, rel string) (*sourceFile, error) {
	f := &sourceFile{}
	if rel == "" {
		return f, nil
	}
	var row, size int64
	var id, hash, lang string
	err := e.sc.db.QueryRowContext(ctx, `SELECT id, file_id, content_hash, size, language FROM files WHERE path = ?`, rel).
		Scan(&row, &id, &hash, &size, &lang)
	if err != nil {
		if err == sql.ErrNoRows {
			return f, nil
		}
		return nil, internalErr("import files: %v", err)
	}
	f.row = sql.NullInt64{Int64: row, Valid: true}
	f.fv = model.FileVersion{ID: model.FileID(id), Path: rel, ContentHash: hash, Size: size, Language: lang}
	f.found = true
	if size > maxRangeFileBytes {
		return f, nil
	}
	rc, got, err := e.opts.Content.Open(ctx, f.fv.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if got.ContentHash != hash {
		return nil, &model.Error{Code: model.CodeSnapshotChanged,
			Message: "the pinned view returned another version of an analyzed file"}
	}
	data, err := io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, &model.Error{Code: model.CodeSourceIntegrity,
			Message: "retained bytes differ in length from the manifest", Details: map[string]string{"file_id": id}}
	}
	f.cursor, f.lines, f.bytes = source.NewCursor(data), &lineIndex{data: data}, true
	return f, nil
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

// declarationKey is the fixed cross-provider strong key for a declaration:
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
	id                               int64
	name, canonical, fullName        string
	signature                        string
	ownerFullName                    string
	fileID, hash, lang, rng, relPath string
	code                             string
	file                             sql.NullInt64
	line, lineEnd                    sql.NullInt64
	external, speculated             bool
}

// identify resolves every kept entity to canonical identity through the
// unit's resolver and stages the node, its aliases and its fact key.
// Entities are visited in id order, so the identity and fact tables are
// appended to. When every entity is identified, the identities are grouped:
// one representative entity and the key list per canonical node, in
// representative order, which is the order the fact table holds. An
// ambiguous resolution's alternatives are not staged: the import publishes no
// relation, so no may_refer_to edge and no key for one.
func (e *emitter) identify(ctx context.Context) error {
	steps := [...]string{
		`CREATE TABLE ident(ent INTEGER PRIMARY KEY, node_id TEXT NOT NULL, key TEXT NOT NULL, alias_json TEXT NOT NULL)`,
		`CREATE TABLE facts(ent INTEGER PRIMARY KEY, node_json TEXT NOT NULL, canonical_key TEXT NOT NULL, file INTEGER,
			range_text TEXT NOT NULL, native_key TEXT NOT NULL, detail TEXT NOT NULL)`,
	}
	for _, q := range steps {
		if err := e.sc.run(ctx, "identify", q); err != nil {
			return err
		}
	}
	after := int64(math.MinInt64)
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT t.id, n.name, n.canonical_name, n.full_name, n.signature,
			n.line, n.line_end, t.external, iv.id IS NOT NULL, l.file, COALESCE(f.file_id, ''), COALESCE(f.content_hash, ''), COALESCE(f.language, ''),
			l.range_text, COALESCE(f.path, ''), COALESCE(o.full_name, ''),
			COALESCE((SELECT c.code FROM code c WHERE c.id = t.id ORDER BY c.seq LIMIT 1), '')
			FROM ents t JOIN nodes n ON n.id = t.id JOIN loc l ON l.node = t.id
			LEFT JOIN invented iv ON iv.id = t.id
			LEFT JOIN files f ON f.id = l.file LEFT JOIN attr a ON a.id = t.id LEFT JOIN nodes o ON o.id = a.owner
			WHERE l.ok = 1 AND t.id > ? ORDER BY t.id LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("import identify: %v", err)
		}
		var page []entity
		for rows.Next() {
			var it entity
			var ext, spec int64
			if err := rows.Scan(&it.id, &it.name, &it.canonical, &it.fullName, &it.signature, &it.line, &it.lineEnd,
				&ext, &spec, &it.file, &it.fileID, &it.hash, &it.lang, &it.rng, &it.relPath, &it.ownerFullName, &it.code); err != nil {
				rows.Close()
				return internalErr("import identify: %v", err)
			}
			it.external, it.speculated = ext != 0, spec != 0
			page = append(page, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import identify: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, it := range page {
			if err := e.identifyOne(ctx, it); err != nil {
				return err
			}
		}
		after = page[len(page)-1].id
	}
	grouping := [...]string{
		`CREATE INDEX ident_by_node ON ident(node_id, ent, key)`,
		`CREATE TABLE groups(rep INTEGER PRIMARY KEY, keys TEXT NOT NULL)`,
		`INSERT INTO groups SELECT MIN(ent), group_concat(key, ' ') FROM ident GROUP BY node_id ORDER BY 1`,
	}
	for _, q := range grouping {
		if err := e.sc.run(ctx, "identify", q); err != nil {
			return err
		}
	}
	return nil
}

func (e *emitter) identifyOne(ctx context.Context, it entity) error {
	name := it.name
	if name == "" {
		name = it.canonical
	}
	if name == "" {
		name = e.cut("name", it.code, model.MaxNameBytes)
	}
	qualified := it.fullName
	if qualified == "" && it.ownerFullName != "" && name != "" {
		qualified = it.ownerFullName + "." + name
	}
	if name == "" {
		name = qualified
	}
	native := nativeMethodKey(qualified)
	scope := e.opts.UnitScopeKey
	if it.relPath != "" {
		scope = "file:" + it.relPath
	}
	// Only the identity half still refuses the entity. native and scope are
	// join keys — a shortened one merges this entity with nothing, or with the
	// wrong thing — and they are derived from the untruncated name and
	// qualified name above, so the cuts below must come after they are minted.
	// A refused entity has no identity row, so no occurrence joins to it.
	if name == "" || len(native) > model.MaxNativeKeyBytes || len(scope) > model.MaxScopeKeyBytes {
		e.dropped++
		return nil
	}
	// The three descriptive fields are cut to their ceiling and counted, into
	// the candidate only: a clipped name or signature still answers most of
	// what a reader asks of it, where dropping the declaration answers
	// nothing. The local name and qualified name stay whole below, because
	// they are read there as key material — the fact key and the evidence
	// native key — which a cut would silently re-mint.
	cand := model.NodeCandidate{ProviderID: ProviderID, ScopeKey: scope, NativeKey: native, Kind: model.NodeFunction,
		Name:      e.cut("name", name, model.MaxNameBytes),
		Signature: e.cut("signature", it.signature, model.MaxSignatureBytes),
		Language:  e.language}
	cand.QualifiedName = e.cut("qualified_name", qualified, model.MaxQualifiedNameBytes)
	evFile, evHash := model.FileID(it.fileID), it.hash
	if it.relPath != "" {
		// A located declaration keys on its exact declaration range, which is
		// how a structural provider and this import mint one identity.
		cand.Language = it.lang
		cand.FileID, cand.ContentHash, cand.Range = evFile, evHash, parseRange(it.rng)
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
	}
	res, err := e.res.Resolve(ctx, cand)
	if err != nil {
		return err
	}
	node := res.Node
	if meta := resolutionMetadata(res, it.external, it.speculated); meta != nil {
		node.Metadata = meta
	}
	// The fact is stored as the node plus the fields its one evidence row is
	// rebuilt from at emission; the evidence itself repeats the unit, run and
	// file identities on every row and is minted again from the same fields.
	nodeJSON, err := json.Marshal(node)
	if err != nil {
		return internalErr("import identify: %v", err)
	}
	aliasList := []model.NativeAlias{{ScopeKey: cand.ScopeKey, NativeKey: cand.NativeKey, NodeID: node.ID}}
	if qualified != "" && cand.ScopeKey != provider.ScopeWorkspace {
		// The full-name alias is unit independent: it is how a stub in
		// another unit binds to this declaration.
		aliasList = append(aliasList, model.NativeAlias{ScopeKey: provider.ScopeWorkspace, NativeKey: native, NodeID: node.ID})
	}
	aliasJSON, err := json.Marshal(aliasList)
	if err != nil {
		return internalErr("import identify: %v", err)
	}
	keyOwner := e.keyOwner(it.fullName, it.ownerFullName)
	positional := e.positional(it.fullName, it.ownerFullName, it.rng, it.name, it.signature)
	key := FactKey(factNodeMethod, keyOwner, it.relPath, "", name, positional, "")
	if err := e.sc.exec(ctx, `INSERT INTO ident(ent, node_id, key, alias_json) VALUES(?,?,?,?)`,
		it.id, string(node.ID), key, string(aliasJSON)); err != nil {
		return err
	}
	if err := e.sc.exec(ctx, `INSERT INTO facts(ent, node_json, canonical_key, file, range_text, native_key, detail) VALUES(?,?,?,?,?,?,?)`,
		it.id, string(nodeJSON), res.CanonicalKey, it.file, it.rng, e.boundedNativeKey(qualified), e.cut("detail", detailCall, model.MaxDetailBytes)); err != nil {
		return err
	}
	return e.sc.staged(ctx)
}

// factNodeMethod is the fact-key label of a published method.
const factNodeMethod = "node:method"

// formatRange is the staged form of a verified range: its six coordinates
// in decimal, which is a fifth of the JSON form and is parsed without one.
func formatRange(r *model.SourceRange) string {
	return strconv.FormatUint(r.Start.Byte, 10) + ":" + strconv.FormatUint(uint64(r.Start.Line), 10) + ":" +
		strconv.FormatUint(uint64(r.Start.Column), 10) + ":" + strconv.FormatUint(r.End.Byte, 10) + ":" +
		strconv.FormatUint(uint64(r.End.Line), 10) + ":" + strconv.FormatUint(uint64(r.End.Column), 10)
}

// parseRange reads a staged range; an absent or malformed one is no range.
func parseRange(raw string) *model.SourceRange {
	if raw == "" {
		return nil
	}
	var v [6]uint64
	for i := range v {
		part, rest, _ := strings.Cut(raw, ":")
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil
		}
		v[i], raw = n, rest
	}
	return &model.SourceRange{Start: model.Position{Byte: v[0], Line: uint32(v[1]), Column: uint32(v[2])},
		End: model.Position{Byte: v[3], Line: uint32(v[4]), Column: uint32(v[5])}}
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
func (e *emitter) positional(fullName, ownerFullName, rangeText string, content ...string) string {
	if isInitializer(fullName) || isInitializer(ownerFullName) {
		return contentDigest(content...)
	}
	return rangeDigest(rangeText)
}

func rangeDigest(ranges ...string) string {
	parts := make([]string, 0, len(ranges))
	for _, raw := range ranges {
		r := parseRange(raw)
		if r == nil {
			parts = append(parts, "-")
			continue
		}
		parts = append(parts, strconv.FormatUint(r.Start.Byte, 10)+"-"+strconv.FormatUint(r.End.Byte, 10))
	}
	return strings.Join(parts, "|")
}

// resolutionMetadata is the node metadata that tells a consumer of the graph
// answers what kind of identity this is: one the resolver could not bind, one
// it bound several ways, one the export invented, or one that lives outside
// the unit. Nothing else carries that distinction to a caller, because a
// traversal answers with nodes and relations and never with the evidence
// behind them.
func resolutionMetadata(res model.Resolution, external, speculated bool) json.RawMessage {
	switch {
	case res.Basis == model.MatchUnresolved:
		return json.RawMessage(`{"resolution":"unresolved","candidates":0}`)
	case len(res.Ambiguous) > 0:
		return json.RawMessage(`{"resolution":"ambiguous","candidates":` + strconv.Itoa(len(res.Ambiguous)+1) + `}`)
	case speculated:
		// The export invented this callee, so it is not an import: nothing in
		// the graph defines it and nothing says the source depends on it. The
		// flag is the export's own stub signal, derived over the whole graph
		// (scratch.go, the invented table), never one parent namespace: an
		// invented callee the engine parked elsewhere would otherwise publish
		// here as a real dependency of the source.
		return json.RawMessage(`{"resolution":"speculated","candidates":1}`)
	case external:
		return json.RawMessage(`{"resolution":"import","candidates":1}`)
	}
	return nil
}

// boundedNativeKey is the evidence native key for a qualified name: the name
// itself, or nothing when it is over the model's key ceiling. A shortened
// key would name another declaration, so an oversize one is left off.
func (e *emitter) boundedNativeKey(qualified string) string {
	if len(qualified) > model.MaxNativeKeyBytes {
		return ""
	}
	return qualified
}

// evidence builds one evidence row from fields already bounded to the model's
// ceilings. It is called once when an occurrence is staged, to mint the id
// the occurrence sorts under, and once more when it is emitted, from the same
// stored fields, so the two agree byte for byte.
func (e *emitter) evidence(node model.NodeID, rel model.RelationID, file model.FileID, hash string,
	rng *model.SourceRange, nativeKey, detail string) model.Evidence {
	ev := model.Evidence{UnitID: e.opts.Unit.ID, ProviderID: e.opts.Unit.ProviderID, ProviderVersion: e.opts.Unit.ProviderVersion,
		OriginRunID: e.opts.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionStaticAnalysis,
		FileID: file, ContentHash: hash, Range: rng, NativeKey: nativeKey, Detail: detail}
	ev.ID = model.NewEvidenceID(ev)
	return ev
}

// emitNodes hands every distinct identity to the sink. Two entities that
// resolved to one identity publish one fact -- the smallest entity's --
// because a unit publishes each node once, and that one fact carries the
// fact keys of every entity behind it. Storage drops a carried fact when ANY
// of its keys is replaced (Section 11.4), so a key left off here would strand
// the row it backs: the next refresh would classify that key as changed,
// storage would keep the old row because the key it kept the row under is
// not this fact's, and the unit would hold two descriptions of one identity.
//
// The groups were written in the order of their representative entity, which
// is the order the fact table holds, so the emission reads both front to
// back.
func (e *emitter) emitNodes(ctx context.Context) error {
	if err := e.sc.commit(ctx); err != nil {
		return err
	}
	after := int64(math.MinInt64)
	for {
		rows, err := e.sc.db.QueryContext(ctx, `SELECT g.rep, g.keys, f.node_json, f.canonical_key, COALESCE(fl.file_id, ''),
			COALESCE(fl.content_hash, ''), f.range_text, f.native_key, f.detail
			FROM groups g JOIN facts f ON f.ent = g.rep LEFT JOIN files fl ON fl.id = f.file
			WHERE g.rep > ? ORDER BY g.rep LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("import nodes: %v", err)
		}
		var facts []model.NodeFact
		var keys [][]string
		var last int64
		for rows.Next() {
			var raw, keyList, canonical, fileID, hash, rng, nativeKey, detail string
			if err := rows.Scan(&last, &keyList, &raw, &canonical, &fileID, &hash, &rng, &nativeKey, &detail); err != nil {
				rows.Close()
				return internalErr("import nodes: %v", err)
			}
			var node model.Node
			if err := json.Unmarshal([]byte(raw), &node); err != nil {
				rows.Close()
				return internalErr("import nodes: %v", err)
			}
			ev := e.evidence(node.ID, "", model.FileID(fileID), hash, parseRange(rng), nativeKey, detail)
			facts = append(facts, model.NodeFact{Node: node, CanonicalKey: canonical, Evidence: []model.Evidence{ev}})
			keys = append(keys, sortedKeys(strings.Fields(keyList)))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import nodes: %v", err)
		}
		if len(facts) == 0 {
			return nil
		}
		if err := e.putNodes(ctx, facts, keys); err != nil {
			return err
		}
		e.nodes += len(facts)
		after = last
	}
}

// sortedKeys is one fact's stored key list in the order a keyed put requires:
// ascending and without duplicates. Duplicates are ordinary — two entities of
// one identity, or two occurrences of one edge, can share a key — and storage
// refuses a list that holds one. There is deliberately no per-fact bound:
// dropping a key would blind removal detection for that fact, and every key is
// one of the export's rows, so a fact's key list is bounded by what the export
// holds — the staged rows for a node fact's keys, the projected occurrences for a
// relation's (51 keys on one relation was the measured peak on this
// repository).
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

// emitAliases publishes each distinct (scope, native key, node) once. The
// distinct sorted list is one pass of the engine's sort, streamed: the loop
// touches nothing but the sink, so the one scratch connection stays on the
// stream.
func (e *emitter) emitAliases(ctx context.Context) error {
	if err := e.sc.commit(ctx); err != nil {
		return err
	}
	rows, err := e.sc.db.QueryContext(ctx, `SELECT DISTINCT alias_json FROM ident ORDER BY alias_json`)
	if err != nil {
		return internalErr("import aliases: %v", err)
	}
	defer rows.Close()
	var aliases []model.NativeAlias
	flush := func() error {
		if len(aliases) == 0 {
			return nil
		}
		if err := e.sink.PutAliases(ctx, aliases); err != nil {
			return err
		}
		e.aliases += len(aliases)
		aliases = nil
		return nil
	}
	for n := 0; rows.Next(); n++ {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return internalErr("import aliases: %v", err)
		}
		var a []model.NativeAlias
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return internalErr("import aliases: %v", err)
		}
		aliases = append(aliases, a...)
		if n%pageSize == pageSize-1 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return internalErr("import aliases: %v", err)
	}
	return flush()
}
