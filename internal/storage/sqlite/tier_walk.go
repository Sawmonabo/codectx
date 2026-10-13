package sqlite

import (
	"context"
	"database/sql"
	"strings"

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
// Each page is one statement that seeks the tier index at the previous page's
// last entry and reads forward, keeping a fact row only when no other visible
// row of its node that matches f precedes it (one probe of
// idx_node_facts_id per row). So a page costs its own rows, and a walk reads
// the tier once. Nodes, which pages by node id, must read and sort the whole
// tier on every page, because no index holds the tier in node-id order.
//
// yield answers the bound of the next page: 0 keeps limit, and a smaller
// positive bound reads only the rows a caller that stops soon still needs.
//
// The pages are separate read transactions over the pinned generation, whose
// rows do not change while it is pinned. A page shorter than its bound is the
// end of the tier: the statement applies its bound to nodes, not to fact rows.
func (r *PinnedReader) WalkTier(ctx context.Context, f NodeFilter, from *TierSpan, limit int,
	yield func([]StoredNode) (int, error)) error {
	limit = pageLimit(ctx, limit)
	bound := limit
	var after *tierKey
	for {
		query, args, err := r.tierPageStatement(f, from, after, bound)
		if err != nil {
			return err
		}
		page := make([]StoredNode, 0, bound)
		var last tierKey
		err = r.s.read(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return wrap("node_facts", err)
			}
			defer rows.Close()
			for rows.Next() {
				n, key, err := scanTierNode(rows)
				if err != nil {
					return err
				}
				page = append(page, n)
				last = key
			}
			return wrap("node_facts", rows.Err())
		})
		if err != nil {
			return err
		}
		if len(page) < bound {
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

// scanTierNode reads one row of a tier page: the node, then the index entry
// the next page resumes after.
func scanTierNode(rows *sql.Rows) (StoredNode, tierKey, error) {
	var n StoredNode
	var key tierKey
	var nodeID, fileID, contentHash, unitKey []byte
	var start, end, nameStart, nameEnd sql.NullInt64
	var metadata string
	if err := rows.Scan(&nodeID, &n.Node.Kind, &n.Node.Language, &n.Node.Name, &n.Node.QualifiedName,
		&n.Node.Signature, &fileID, &contentHash, &start, &end, &nameStart, &nameEnd, &metadata, &unitKey,
		&n.SourceBinding, &key.key, &key.path, &key.start, &key.end, &key.unit, &key.node); err != nil {
		return n, key, wrap("node_facts", err)
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
	n.UnitID = model.UnitID(idHex(unitKey))
	return n, key, nil
}

// tierPageStatement is one page of WalkTier: from the first row at or after
// from (nil: the tier's start), or after the index entry `after` once a page
// has been read.
//
// The outer rows come from the tier index in its order and the bound applies
// to them. The keyset is a row-value comparison on the index's columns that
// SQLite answers as a seek into the index, not as a filter over the range
// before it: for an equality tier, the equality plus the columns after it
// (path, sort start, sort end, unit_id, node_id) after the last entry; for the
// prefix tier, all six columns after the last entry as the lower bound (it is
// inside the range), with the prefix's upper bound. (A row value that repeats
// the equality column beside the equality plans as a filter, and every page
// would read the tier from its start up to its own rows.) CROSS JOIN fixes the
// join order (the facts first, then whether the generation selects each fact's
// unit): left to itself, and with statistics, the planner may start from the
// generation's units and read every fact they hold. Inside the NOT EXISTS the
// unary + keeps the node's other rows reached through idx_node_facts_id, by
// node, rather than through the tier index again.
func (r *PinnedReader) tierPageStatement(f NodeFilter, from *TierSpan, after *tierKey, limit int) (string, []any, error) {
	predicates := 0
	for _, p := range []string{f.Name, f.QualifiedName, f.QualifiedPrefix} {
		if p != "" {
			predicates++
		}
	}
	if predicates != 1 {
		return "", nil, invalid("node filter must name exactly one of name, qualified name or qualified-name prefix")
	}
	if len(f.Kinds) > model.MaxFilterValues {
		return "", nil, invalid("node filter has %d kinds, limit %d", len(f.Kinds), model.MaxFilterValues)
	}
	if from != nil && f.QualifiedPrefix != "" {
		return "", nil, invalid("a qualified-name prefix tier is not in content order and cannot start at a span")
	}
	column := "qualified_name"
	if f.Name != "" {
		column = "name"
	}
	args := []any{r.gen}
	arg := func(v any) string {
		args = append(args, v)
		return "?" + itoa(len(args))
	}
	marks := func(vs ...any) string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = arg(v)
		}
		return strings.Join(out, ", ")
	}
	var where []string
	var other string
	if f.QualifiedPrefix == "" {
		key := arg(f.Name + f.QualifiedName)
		where = append(where, "nf."+column+" = "+key)
		other = "+nf2." + column + " = " + key
		switch {
		case after != nil:
			where = append(where, "(nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id) > ("+
				marks(after.path, after.start, after.end, after.unit, after.node)+")")
		case from != nil:
			where = append(where, "(nf.path, nf.sort_start, nf.sort_end) >= ("+marks(from.Path, from.Start, from.End)+")")
		}
	} else {
		low, high := arg(f.QualifiedPrefix), arg(prefixUpperBound(f.QualifiedPrefix))
		other = "+nf2.qualified_name >= " + low + " AND +nf2.qualified_name < " + high
		if after == nil {
			where = append(where, "nf.qualified_name >= "+low+" AND nf.qualified_name < "+high)
		} else {
			where = append(where, "(nf.qualified_name, nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id) > ("+
				marks(after.key, after.path, after.start, after.end, after.unit, after.node)+") AND nf.qualified_name < "+high)
		}
	}
	kinds := f.Kinds
	op := "IN"
	if len(kinds) == 0 {
		kinds, op = model.NonSymbolKinds(), "NOT IN"
	}
	kindArgs := make([]any, len(kinds))
	for i, k := range kinds {
		if !k.Valid() {
			return "", nil, invalid("node filter kind %q is not a known node kind", k)
		}
		kindArgs[i] = string(k)
	}
	where = append(where, "ni.kind "+op+" ("+marks(kindArgs...)+")")
	bound := arg(limit)
	query := `SELECT ` + nodeColumns + `, nf.` + column + `, nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id
		FROM node_facts nf
		CROSS JOIN generation_units gu ON gu.unit_id = nf.unit_id AND gu.generation_id = ?1
		CROSS JOIN units u ON u.id = gu.unit_id
		CROSS JOIN node_ids ni ON ni.id = nf.node_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE ` + strings.Join(where, " AND ") + `
		AND NOT EXISTS (SELECT 1 FROM node_facts nf2
			CROSS JOIN generation_units gu2 ON gu2.unit_id = nf2.unit_id AND gu2.generation_id = ?1
			CROSS JOIN units u2 ON u2.id = gu2.unit_id
			WHERE nf2.node_id = nf.node_id AND ` + other + `
			AND (CASE u2.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u2.provider_id, u2.unit_key)
			  < (CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key))
		ORDER BY nf.` + column + `, nf.path, nf.sort_start, nf.sort_end, nf.unit_id, nf.node_id LIMIT ` + bound
	return query, args, nil
}
