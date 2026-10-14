package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TierSpan is a position in an equality tier's content order: a served row's
// path ("" for a node with no file), start byte and end byte, a missing byte
// interval read as start 0 and end -1. The exact search tiers order their hits
// by these keys first (then kind, names, signature and node id), and so does
// the tier index.
type TierSpan struct {
	Path       string
	Start, End int64
}

// tierKey is where a tier walk stopped: the index entry of the last row a page
// served, in the order of the index the tier reads (idx_nodes_name or
// idx_nodes_qname: the tier column, the path, the sort start and end, then
// unit_id and node_id). It holds storage surrogates, so it never leaves this
// package.
type tierKey struct {
	key, path              string
	start, end, unit, node int64
}

// WalkTier reads every node f selects, once each, as the row Nodes would
// serve for it (the precedence winner among the node's visible rows that
// match f), and hands them to yield a page of at most limit nodes at a time.
// limit is capped at model.MaxPageItems. The order is the tier index's own,
// not node-id order: for an exact name or an exact qualified name it is the
// content order (path, start byte, end byte; TierSpan), rows with equal spans
// in no order a caller may rely on; for a qualified-name prefix, the qualified
// name first.
//
// from, which only an exact name or exact qualified name takes, starts the
// walk at the first row whose span is not before it, so a caller resuming
// after a served hit seeks to it and reads only the rows its span shares
// before the ones after it.
//
// A page is one read transaction of a few statements, each over the rows it
// serves rather than over the tier:
//   - a window of the tier index from the previous page's last entry, read
//     from the index alone (each entry's unit and node; no joins), and the
//     index keys of the one entry the page stops at;
//   - the rows of the window's nodes that match f, through idx_node_facts_id,
//     one statement for the whole window;
//   - the precedence of the units those rows belong to that the walk has not
//     seen yet, with whether the generation selects them;
//   - the full rows of the window's winners, with the kind filter.
//
// A window entry is served when its unit is visible and is the best-ranked
// visible unit among its node's rows that match f, the precedence Nodes
// applies (exact-source binding first, then provider id, then unit key; unit
// keys are unique, so it is a total order). The walk remembers each unit's
// precedence, so the units statement reads each unit once per walk: at most
// the units the walked rows belong to. The window is sized from the rows per
// served node seen so far, so a tier whose nodes are published by several
// units still fills a page in about one window.
//
// yield answers the bound of the next page: 0 keeps limit, and a smaller
// positive bound reads only the rows a caller that stops soon still needs.
//
// The pages are separate read transactions over the pinned generation, whose
// rows do not change while it is pinned. A page shorter than its bound is the
// end of the tier, and so may be the last full one: the bound applies to
// served nodes, not to fact rows.
func (r *PinnedReader) WalkTier(ctx context.Context, f NodeFilter, from *TierSpan, limit int,
	yield func([]StoredNode) (int, error)) error {
	w, err := r.newTierWalk(f, from)
	if err != nil {
		return err
	}
	limit = pageLimit(ctx, limit)
	bound := limit
	var after *tierKey
	for {
		page, last, end, err := w.page(ctx, after, bound)
		if err != nil {
			return err
		}
		if end || len(page) < bound {
			if len(page) > 0 {
				_, err = yield(page)
			}
			return err
		}
		next, err := yield(page)
		if err != nil {
			return err
		}
		bound = limit
		if next > 0 && next < limit {
			bound = next
		}
		after = &last
	}
}

// unitRank is one unit's place in the read-time precedence, and whether the
// pinned generation selects it.
type unitRank struct {
	visible  bool
	binding  model.SourceBinding
	provider string
	key      []byte
}

// precedes is nodeOrder over two units: an exact-source binding first, then
// the provider id and the unit key, compared as SQLite compares TEXT and BLOB
// values (bytewise).
func (a unitRank) precedes(b unitRank) bool {
	if av, bv := a.binding == model.SourceBindingVerified, b.binding == model.SourceBindingVerified; av != bv {
		return av
	}
	if a.provider != b.provider {
		return a.provider < b.provider
	}
	return bytes.Compare(a.key, b.key) < 0
}

// tierWalk is one WalkTier call: its statements, which are fixed texts for a
// filter's shape (so the store's prepared handles serve them), the precedence
// of the units it has seen, and the rows per served node it has seen.
type tierWalk struct {
	r *PinnedReader
	// match are the tier arguments: the equality value, or the prefix's
	// bounds. kinds is the kind list as a JSON array.
	match []any
	kinds string
	from  *TierSpan
	// prefix says the tier is the qualified-name prefix tier, whose keyset
	// starts with the qualified name.
	prefix bool
	// The statements: the first window (from the tier's start or from from),
	// the window after an entry, the index keys of one entry, the rows of a
	// window's nodes, the units, and the winners' rows. A window reads only
	// each entry's unit and node; only the entry a page stops at needs its
	// keys, which entryKey reads back by primary key.
	firstWindow, nextWindow, entryKey, probe, units, fetch string
	ranks                                                  map[int64]unitRank
	// entries and served count the window entries read and the nodes served,
	// which size the next window.
	entries, served int
}

func (r *PinnedReader) newTierWalk(f NodeFilter, from *TierSpan) (*tierWalk, error) {
	predicates := 0
	for _, p := range []string{f.Name, f.QualifiedName, f.QualifiedPrefix} {
		if p != "" {
			predicates++
		}
	}
	if predicates != 1 {
		return nil, invalid("node filter must name exactly one of name, qualified name or qualified-name prefix")
	}
	if len(f.Kinds) > model.MaxFilterValues {
		return nil, invalid("node filter has %d kinds, limit %d", len(f.Kinds), model.MaxFilterValues)
	}
	if from != nil && f.QualifiedPrefix != "" {
		return nil, invalid("a qualified-name prefix tier is not in content order and cannot start at a span")
	}
	kinds, op := f.Kinds, "IN"
	if len(kinds) == 0 {
		kinds, op = model.NonSymbolKinds(), "NOT IN"
	}
	names := make([]string, len(kinds))
	for i, k := range kinds {
		if !k.Valid() {
			return nil, invalid("node filter kind %q is not a known node kind", k)
		}
		names[i] = string(k)
	}
	kindJSON, err := json.Marshal(names)
	if err != nil {
		return nil, internal("encode the kind filter: " + err.Error())
	}
	w := &tierWalk{r: r, kinds: string(kindJSON), from: from, ranks: map[int64]unitRank{}}
	order := func(column string) string {
		return ` ORDER BY nf.` + column + `, nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id LIMIT `
	}
	columns := `SELECT nf.unit_id, nf.node_id`
	column := "qualified_name"
	if f.Name != "" {
		column = "name"
	}
	w.entryKey = `SELECT nf.` + column + `, nf.path, nf.sort_start, nf.sort_end FROM node_facts nf
		WHERE nf.unit_id = ?1 AND nf.node_id = ?2`
	if f.QualifiedPrefix == "" {
		w.match = []any{f.Name + f.QualifiedName}
		o := order(column)
		w.firstWindow = columns + ` FROM node_facts nf WHERE nf.` + column + ` = ?1` + o + `?2`
		if from != nil {
			w.firstWindow = columns + ` FROM node_facts nf WHERE nf.` + column + ` = ?1
				AND (nf.path, nf.sort_start, nf.sort_end) >= (?2, ?3, ?4)` + o + `?5`
		}
		w.nextWindow = columns + ` FROM node_facts nf WHERE nf.` + column + ` = ?1
			AND (nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id) > (?2, ?3, ?4, ?5, ?6)` + o + `?7`
		w.probe = `SELECT nf.node_id, nf.unit_id FROM json_each(?1) j
			CROSS JOIN node_facts nf ON nf.node_id = j.value WHERE +nf.` + column + ` = ?2`
	} else {
		w.prefix = true
		w.match = []any{f.QualifiedPrefix, prefixUpperBound(f.QualifiedPrefix)}
		o := order("qualified_name")
		w.firstWindow = columns + ` FROM node_facts nf
			WHERE nf.qualified_name >= ?1 AND nf.qualified_name < ?2` + o + `?3`
		w.nextWindow = columns + ` FROM node_facts nf
			WHERE (nf.qualified_name, nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id) > (?3, ?4, ?5, ?6, ?7, ?8)
			AND nf.qualified_name < ?2` + o + `?9`
		w.probe = `SELECT nf.node_id, nf.unit_id FROM json_each(?1) j
			CROSS JOIN node_facts nf ON nf.node_id = j.value
			WHERE +nf.qualified_name >= ?2 AND +nf.qualified_name < ?3`
	}
	w.units = `SELECT u.id, u.source_binding, u.provider_id, u.unit_key FROM json_each(?2) j
		CROSS JOIN generation_units gu ON gu.generation_id = ?1 AND gu.unit_id = j.value
		CROSS JOIN units u ON u.id = gu.unit_id`
	w.fetch = `SELECT j.key, ni.canonical, ni.kind, nf.language, nf.name, nf.qualified_name, nf.signature, nf.file_id,
			ui.content_hash, nf.start_byte, nf.end_byte, nf.name_start_byte, nf.name_end_byte, nf.metadata_json, nf.path
		FROM json_each(?1) j
		CROSS JOIN node_facts nf ON nf.unit_id = j.value ->> 0 AND nf.node_id = j.value ->> 1
		CROSS JOIN node_ids ni ON ni.id = nf.node_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE ni.kind ` + op + ` (SELECT value FROM json_each(?2))`
	return w, nil
}

// windowArgs are the arguments of a window of size entries: from the tier's
// start (or from w.from) when after is nil, otherwise after that entry.
func (w *tierWalk) windowArgs(after *tierKey, size int) (string, []any) {
	args := append([]any{}, w.match...)
	switch {
	case after != nil && w.prefix:
		return w.nextWindow, append(args, after.key, after.path, after.start, after.end, after.unit, after.node, size)
	case after != nil:
		return w.nextWindow, append(args, after.path, after.start, after.end, after.unit, after.node, size)
	case w.from != nil:
		return w.firstWindow, append(args, w.from.Path, w.from.Start, w.from.End, size)
	}
	return w.firstWindow, append(args, size)
}

// windowSize is the window that should hold want more served nodes: want
// times the entries read per node served so far, or, while the walk has read
// entries and served none, twice the entries read, so a run of rows that are
// not served costs a few windows, never one per row.
func (w *tierWalk) windowSize(want int) int {
	size := want
	switch {
	case w.served > 0:
		size = (want*w.entries + w.served - 1) / w.served
	case w.entries > 0:
		size = 2 * w.entries
	}
	return min(max(size, want), maxTierWindow)
}

// maxTierWindow bounds one window's entries: a node published by that many
// units is far past any repository's provider count, and a page whose rows
// need more windows still serves them, one window at a time.
const maxTierWindow = 4 * model.MaxPageItems

// tierStatements are the store's handles for one walk's statements, taken
// before the page's read transaction (readStmts.stmt).
type tierStatements struct {
	first, next, key, probe, units, fetch *sql.Stmt
}

func (w *tierWalk) statements(ctx context.Context) (tierStatements, error) {
	var st tierStatements
	for _, s := range []struct {
		dst   **sql.Stmt
		query string
	}{{&st.first, w.firstWindow}, {&st.next, w.nextWindow}, {&st.key, w.entryKey}, {&st.probe, w.probe},
		{&st.units, w.units},
		{&st.fetch, w.fetch}} {
		h, err := w.r.s.reads.stmt(ctx, w.r.s.readers, s.query)
		if err != nil {
			return st, err
		}
		*s.dst = h
	}
	return st, nil
}

// page reads one page of at most bound served nodes after the entry after
// (nil: from the start). It answers the page, the last index entry it
// consumed, and whether the tier ended.
func (w *tierWalk) page(ctx context.Context, after *tierKey, bound int) ([]StoredNode, tierKey, bool, error) {
	st, err := w.statements(ctx)
	if err != nil {
		return nil, tierKey{}, false, err
	}
	page := make([]StoredNode, 0, bound)
	var last tierKey
	end := false
	err = w.r.s.read(ctx, func(tx *sql.Tx) error {
		cur := after
		for len(page) < bound {
			want := bound - len(page)
			size := w.windowSize(want)
			entries, err := w.window(ctx, tx, st, cur, size)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				end = true
				return nil
			}
			winners, err := w.winners(ctx, tx, st, entries)
			if err != nil {
				return err
			}
			consumed := len(entries) - 1
			var take []tierKey
			for i, e := range entries {
				// A winner is a visible unit; unit ids start at 1, so a
				// node with no winner matches no entry.
				if winners[e.node] != e.unit {
					continue
				}
				take = append(take, e)
				if len(take) == want {
					consumed = i
					break
				}
			}
			served, err := w.rows(ctx, tx, st, take)
			if err != nil {
				return err
			}
			page = append(page, served...)
			w.entries += consumed + 1
			w.served += len(served)
			if consumed == len(entries)-1 && len(entries) < size {
				end = true
				return nil
			}
			if last, err = w.keyOf(ctx, tx, st, entries[consumed]); err != nil {
				return err
			}
			cur = &last
		}
		return nil
	})
	return page, last, end, err
}

// window reads up to size index entries after cur (nil: the walk's start).
func (w *tierWalk) window(ctx context.Context, tx *sql.Tx, st tierStatements, cur *tierKey,
	size int) ([]tierKey, error) {
	query, args := w.windowArgs(cur, size)
	h := st.first
	if query == w.nextWindow {
		h = st.next
	}
	rows, err := tx.StmtContext(ctx, h).QueryContext(ctx, args...)
	if err != nil {
		return nil, wrap("node_facts", err)
	}
	defer rows.Close()
	entries := make([]tierKey, 0, size)
	for rows.Next() {
		var e tierKey
		if err := rows.Scan(&e.unit, &e.node); err != nil {
			return nil, wrap("node_facts", err)
		}
		entries = append(entries, e)
	}
	return entries, wrap("node_facts", rows.Err())
}

// keyOf completes e, a window entry, with its index keys: the tier column, the
// path and the sort start and end.
func (w *tierWalk) keyOf(ctx context.Context, tx *sql.Tx, st tierStatements, e tierKey) (tierKey, error) {
	err := tx.StmtContext(ctx, st.key).QueryRowContext(ctx, e.unit, e.node).Scan(&e.key, &e.path, &e.start, &e.end)
	return e, wrap("node_facts", err)
}

// winners answers, for each node of entries, the visible unit whose row of
// the node wins among the node's rows that match the tier; a node with no
// visible row is absent.
func (w *tierWalk) winners(ctx context.Context, tx *sql.Tx, st tierStatements,
	entries []tierKey) (map[int64]int64, error) {
	if err := w.loadRanks(ctx, tx, st, unseenUnits(w.ranks, entries, func(e tierKey) int64 { return e.unit })); err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(entries))
	nodes := make([]int64, 0, len(entries))
	for _, e := range entries {
		if w.ranks[e.unit].visible && !seen[e.node] {
			seen[e.node] = true
			nodes = append(nodes, e.node)
		}
	}
	best := make(map[int64]int64, len(nodes))
	if len(nodes) == 0 {
		return best, nil
	}
	rows, err := tx.StmtContext(ctx, st.probe).QueryContext(ctx, append([]any{intsJSON(nodes)}, w.match...)...)
	if err != nil {
		return nil, wrap("node_facts", err)
	}
	type fact struct{ node, unit int64 }
	var facts []fact
	for rows.Next() {
		var f fact
		if err := rows.Scan(&f.node, &f.unit); err != nil {
			rows.Close()
			return nil, wrap("node_facts", err)
		}
		facts = append(facts, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, wrap("node_facts", err)
	}
	if err := w.loadRanks(ctx, tx, st, unseenUnits(w.ranks, facts, func(f fact) int64 { return f.unit })); err != nil {
		return nil, err
	}
	for _, f := range facts {
		rank := w.ranks[f.unit]
		if !rank.visible {
			continue
		}
		if held, ok := best[f.node]; !ok || rank.precedes(w.ranks[held]) {
			best[f.node] = f.unit
		}
	}
	return best, nil
}

// unseenUnits answers the distinct units of items whose precedence the walk
// has not read.
func unseenUnits[T any](ranks map[int64]unitRank, items []T, unit func(T) int64) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, it := range items {
		u := unit(it)
		if _, ok := ranks[u]; !ok && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// loadRanks reads the precedence of units the walk has not seen; a unit the
// generation does not select is remembered as not visible.
func (w *tierWalk) loadRanks(ctx context.Context, tx *sql.Tx, st tierStatements, units []int64) error {
	if len(units) == 0 {
		return nil
	}
	for _, u := range units {
		w.ranks[u] = unitRank{}
	}
	rows, err := tx.StmtContext(ctx, st.units).QueryContext(ctx, w.r.gen, intsJSON(units))
	if err != nil {
		return wrap("units", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		rank := unitRank{visible: true}
		if err := rows.Scan(&id, &rank.binding, &rank.provider, &rank.key); err != nil {
			return wrap("units", err)
		}
		w.ranks[id] = rank
	}
	return wrap("units", rows.Err())
}

// rows reads the full rows of take, in take's order, keeping those whose node
// passes the kind filter.
func (w *tierWalk) rows(ctx context.Context, tx *sql.Tx, st tierStatements, take []tierKey) ([]StoredNode, error) {
	if len(take) == 0 {
		return nil, nil
	}
	pairs := make([]byte, 0, 24*len(take))
	pairs = append(pairs, '[')
	for i, e := range take {
		if i > 0 {
			pairs = append(pairs, ',')
		}
		pairs = append(pairs, '[')
		pairs = strconv.AppendInt(pairs, e.unit, 10)
		pairs = append(pairs, ',')
		pairs = strconv.AppendInt(pairs, e.node, 10)
		pairs = append(pairs, ']')
	}
	pairs = append(pairs, ']')
	rows, err := tx.StmtContext(ctx, st.fetch).QueryContext(ctx, string(pairs), w.kinds)
	if err != nil {
		return nil, wrap("node_facts", err)
	}
	defer rows.Close()
	got := make([]*StoredNode, len(take))
	for rows.Next() {
		var at int
		var n StoredNode
		var nodeID, fileID, contentHash []byte
		var start, end, nameStart, nameEnd sql.NullInt64
		var metadata string
		if err := rows.Scan(&at, &nodeID, &n.Node.Kind, &n.Node.Language, &n.Node.Name, &n.Node.QualifiedName,
			&n.Node.Signature, &fileID, &contentHash, &start, &end, &nameStart, &nameEnd, &metadata, &n.Path); err != nil {
			return nil, wrap("node_facts", err)
		}
		if at < 0 || at >= len(take) {
			return nil, internal("a tier row names an entry outside its batch")
		}
		n.Node.ID = model.NodeID(idHex(nodeID))
		n.Node.SemanticSource = model.SemanticCanonical
		if fileID != nil {
			n.Node.FileID = model.FileID(idHex(fileID))
			n.Node.ContentHash = idHex(contentHash)
		}
		if start.Valid {
			n.Bytes = &model.ByteRange{Start: uint64(start.Int64), End: uint64(end.Int64)}
		}
		if nameStart.Valid {
			n.NameBytes = &model.ByteRange{Start: uint64(nameStart.Int64), End: uint64(nameEnd.Int64)}
		}
		if metadata != "{}" {
			n.Node.Metadata = []byte(metadata)
		}
		rank := w.ranks[take[at].unit]
		n.UnitID = model.UnitID(idHex(rank.key))
		n.SourceBinding = rank.binding
		got[at] = &n
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("node_facts", err)
	}
	out := make([]StoredNode, 0, len(take))
	for _, n := range got {
		if n != nil {
			out = append(out, *n)
		}
	}
	return out, nil
}

// intsJSON renders ids as a JSON array for json_each.
func intsJSON(ids []int64) string {
	out := make([]byte, 0, 8*len(ids)+2)
	out = append(out, '[')
	for i, id := range ids {
		if i > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendInt(out, id, 10)
	}
	return string(append(out, ']'))
}
