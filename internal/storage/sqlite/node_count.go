package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// CountNodes counts the nodes Nodes serves for f over every page: the
// distinct node ids among the visible facts that match f. It is one statement
// over the name or qualified-name index, so the exact count of a name that
// thousands of nodes carry costs one indexed range scan instead of a walk of
// every keyset page. The joins are CROSS JOINs so the name or qualified-name
// index drives the scan whatever the statistics say about the generation's
// units.
func (r *PinnedReader) CountNodes(ctx context.Context, f NodeFilter) (int64, error) {
	query, args, err := r.countNodesStatement(f)
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		return wrap("node_facts", tx.QueryRowContext(ctx, query, args...).Scan(&n))
	})
	return n, err
}

// countNodesStatement is CountNodes' statement and its arguments.
func (r *PinnedReader) countNodesStatement(f NodeFilter) (string, []any, error) {
	where, args, err := nodeFilterWhere(f)
	if err != nil {
		return "", nil, err
	}
	return `SELECT count(DISTINCT nf.node_id) FROM node_facts nf
		CROSS JOIN generation_units gu ON gu.unit_id = nf.unit_id AND gu.generation_id = ?1
		CROSS JOIN units u ON u.id = gu.unit_id
		CROSS JOIN node_ids ni ON ni.id = nf.node_id WHERE ` + where, append([]any{r.gen}, args...), nil
}

// nodeFilterWhere is the predicate Nodes applies for f: exactly one of the
// name, the qualified name or the qualified-name prefix, and the kinds f names
// or, when it names none, every kind but the non-symbol ones. The arguments
// follow the generation argument, which the visibility join reads as ?1.
func nodeFilterWhere(f NodeFilter) (string, []any, error) {
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
	var where []string
	var args []any
	switch {
	case f.Name != "":
		where = append(where, "nf.name = ?")
		args = append(args, f.Name)
	case f.QualifiedName != "":
		where = append(where, "nf.qualified_name = ?")
		args = append(args, f.QualifiedName)
	default:
		where = append(where, "nf.qualified_name >= ? AND nf.qualified_name < ?")
		args = append(args, f.QualifiedPrefix, prefixUpperBound(f.QualifiedPrefix))
	}
	kinds, op := f.Kinds, "IN"
	if len(kinds) == 0 {
		kinds, op = model.NonSymbolKinds(), "NOT IN"
	}
	marks := make([]string, len(kinds))
	for i, k := range kinds {
		if len(f.Kinds) > 0 && !k.Valid() {
			return "", nil, invalid("node filter kind %q is not a known node kind", k)
		}
		marks[i] = "?"
		args = append(args, string(k))
	}
	where = append(where, "ni.kind "+op+" ("+strings.Join(marks, ",")+")")
	return strings.Join(where, " AND "), args, nil
}
