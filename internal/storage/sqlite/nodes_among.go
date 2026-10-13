package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// NodesAmong answers, for each of ids that some visible row matching f
// lists, the row Nodes would serve for it: the precedence winner among the
// node's visible rows that match f. The answer is in ascending node-id order,
// one row per node; an id no matching row lists is absent. It is the batched
// form of seeking Nodes' keyset at each id, so a caller asking about a page of
// nodes pays one statement instead of one per node.
//
// At most model.MaxPageItems ids are asked at once; the caller splits a larger
// set. The ids drive the statement: each is one probe of node_ids' canonical
// index, then node_facts' (node_id, unit_id) index, then the generation's
// selection of each fact's unit, and f's name or qualified name is tested on
// the rows those reach (the unary + keeps the planner from reaching the facts
// through the name index instead, which would cost the tier's size rather than
// the ids').
func (r *PinnedReader) NodesAmong(ctx context.Context, f NodeFilter, ids []model.NodeID) ([]StoredNode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > model.MaxPageItems {
		return nil, invalid("a node lookup names %d ids, limit %d", len(ids), model.MaxPageItems)
	}
	query, args, err := r.nodesAmongStatement(f, ids)
	if err != nil {
		return nil, err
	}
	out := make([]StoredNode, 0, len(ids))
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("node_facts", err)
		}
		defer rows.Close()
		var last model.NodeID
		for rows.Next() {
			n, err := scanNode(rows)
			if err != nil {
				return err
			}
			if n.Node.ID == last {
				continue
			}
			last = n.Node.ID
			out = append(out, n)
		}
		return wrap("node_facts", rows.Err())
	})
	return out, err
}

// nodesAmongStatement is NodesAmong's statement and its arguments.
func (r *PinnedReader) nodesAmongStatement(f NodeFilter, ids []model.NodeID) (string, []any, error) {
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
	args := []any{r.gen}
	marks := make([]string, len(ids))
	for i, id := range ids {
		raw, err := idBlob("node_id", string(id))
		if err != nil {
			return "", nil, err
		}
		marks[i] = "?"
		args = append(args, raw)
	}
	where := []string{"ni.canonical IN (" + strings.Join(marks, ",") + ")"}
	switch {
	case f.Name != "":
		where = append(where, "+nf.name = ?")
		args = append(args, f.Name)
	case f.QualifiedName != "":
		where = append(where, "+nf.qualified_name = ?")
		args = append(args, f.QualifiedName)
	default:
		where = append(where, "+nf.qualified_name >= ? AND +nf.qualified_name < ?")
		args = append(args, f.QualifiedPrefix, prefixUpperBound(f.QualifiedPrefix))
	}
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			if !k.Valid() {
				return "", nil, invalid("node filter kind %q is not a known node kind", k)
			}
			kinds[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "ni.kind IN ("+strings.Join(kinds, ",")+")")
	}
	// CROSS JOIN fixes the join order (SQLite's manual plan control): the asked
	// ids first, then their facts, then whether the generation selects each
	// fact's unit. Left to itself the planner may start from the generation's
	// units and read every fact they hold.
	query := `SELECT ` + nodeColumns + ` FROM node_ids ni CROSS JOIN node_facts nf ON nf.node_id = ni.id
		CROSS JOIN generation_units gu ON gu.unit_id = nf.unit_id AND gu.generation_id = ?1
		CROSS JOIN units u ON u.id = gu.unit_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ni.canonical, CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key`
	return query, args, nil
}
