package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// tierKey is where a tier walk stopped: the index entry of the last row a page
// served, in the order of the index the tier reads (idx_nodes_name or
// idx_nodes_qname: the tier column, then unit_id, then node_id). It holds
// storage surrogates, so it never leaves this package.
type tierKey struct {
	key        string
	unit, node int64
}

// WalkTier reads every node f selects, once each, as the row Nodes would
// serve for it (the precedence winner among the node's visible rows that
// match f), and hands them to yield a page of at most limit nodes at a time.
// limit is capped at model.MaxPageItems. The order is the tier index's own,
// not node-id order: it is for a caller that orders the nodes itself (a heap
// or a sort) or does not need an order.
//
// Each page is one statement that seeks the tier index at the previous page's
// last entry and reads forward, keeping a fact row only when no other visible
// row of its node that matches f precedes it (one probe of
// idx_node_facts_id per row). So a page costs its own rows, and a walk reads
// the tier once. Nodes, which pages by node id, must read and sort the whole
// tier on every page, because no index holds the tier in node-id order.
//
// The pages are separate read transactions over the pinned generation, whose
// rows do not change while it is pinned. A page shorter than the bound is the
// end of the tier: the statement applies its bound to nodes, not to fact rows.
func (r *PinnedReader) WalkTier(ctx context.Context, f NodeFilter, limit int,
	yield func([]StoredNode) error) error {
	limit = pageLimit(ctx, limit)
	var after *tierKey
	for {
		query, args, err := r.tierPageStatement(f, after, limit)
		if err != nil {
			return err
		}
		page := make([]StoredNode, 0, limit)
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
		if len(page) > 0 {
			if err := yield(page); err != nil {
				return err
			}
		}
		if len(page) < limit {
			return nil
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
		&n.SourceBinding, &key.key, &key.unit, &key.node); err != nil {
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

// tierPageStatement is one page of WalkTier after the index entry `after`
// (nil: from the tier's start).
//
// The outer rows come from the tier index in its order and the bound applies
// to them. The keyset is a row-value comparison on the index's columns that
// SQLite answers as a seek into the index, not as a filter over the range
// before it: for an equality tier, the equality plus (unit_id, node_id) after
// the last entry; for the prefix tier, (qualified_name, unit_id, node_id) after
// the last entry as the lower bound, with the prefix's upper bound. (A row
// value over all three columns beside the equality, or beside the prefix's
// lower bound, plans as a filter, and every page would read the tier from its
// start up to its own rows.) CROSS JOIN fixes the join
// order (the facts first, then whether the generation selects each fact's
// unit): left to itself, and with statistics, the planner may start from the
// generation's units and read every fact they hold. Inside the NOT EXISTS the
// unary + keeps the node's other rows reached through idx_node_facts_id, by
// node, rather than through the tier index again.
func (r *PinnedReader) tierPageStatement(f NodeFilter, after *tierKey, limit int) (string, []any, error) {
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
	column := "qualified_name"
	if f.Name != "" {
		column = "name"
	}
	args := []any{r.gen}
	var where []string
	var other string
	switch {
	case f.Name != "" || f.QualifiedName != "":
		// An equality tier: the keyset is on the index's columns after the
		// tier column, the form the planner seeks with.
		key := f.Name + f.QualifiedName
		where = append(where, "nf."+column+" = ?2")
		other = "+nf2." + column + " = ?2"
		args = append(args, key)
		if after != nil {
			where = append(where, "(nf.unit_id, nf.node_id) > (?3, ?4)")
			args = append(args, after.unit, after.node)
		}
	default:
		// The prefix tier: past the first page the keyset row value is the
		// range's lower bound (the last entry served is inside the range), so
		// the planner seeks to it rather than to the prefix's start.
		upper := prefixUpperBound(f.QualifiedPrefix)
		other = "+nf2.qualified_name >= ?2 AND +nf2.qualified_name < ?3"
		args = append(args, f.QualifiedPrefix, upper)
		if after == nil {
			where = append(where, "nf.qualified_name >= ?2 AND nf.qualified_name < ?3")
		} else {
			where = append(where, "(nf.qualified_name, nf.unit_id, nf.node_id) > (?4, ?5, ?6) AND nf.qualified_name < ?3")
			args = append(args, after.key, after.unit, after.node)
		}
	}
	marks := make([]string, 0, max(len(f.Kinds), 1))
	kinds := f.Kinds
	op := "IN"
	if len(kinds) == 0 {
		kinds, op = model.NonSymbolKinds(), "NOT IN"
	}
	for _, k := range kinds {
		if !k.Valid() {
			return "", nil, invalid("node filter kind %q is not a known node kind", k)
		}
		args = append(args, string(k))
		marks = append(marks, "?"+itoa(len(args)))
	}
	where = append(where, "ni.kind "+op+" ("+strings.Join(marks, ",")+")")
	args = append(args, limit)
	query := `SELECT ` + nodeColumns + `, nf.` + column + `, nf.unit_id, nf.node_id
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
		ORDER BY nf.` + column + `, nf.unit_id, nf.node_id LIMIT ?` + itoa(len(args))
	return query, args, nil
}
