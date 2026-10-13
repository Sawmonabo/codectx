package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

var _ graph.FlowRoleReader = (*graphReader)(nil)

// RelationFlowRoles reads each relation's call role off its visible evidence
// in one grouped read per batch, as RelationDispatch reads dispatch: whether
// any occurrence is an argument flow and whether any is a return flow (the
// evidence details graph.FlowDetailArgument and graph.FlowDetailReturn).
// Results align with rels; a relation with no visible evidence reads as the
// zero role.
func (g *graphReader) RelationFlowRoles(ctx context.Context, rels []model.RelationID) ([]graph.FlowRole, error) {
	out := make([]graph.FlowRole, len(rels))
	if len(rels) == 0 {
		return out, nil
	}
	at := make(map[model.RelationID][]int, len(rels))
	for i, id := range rels {
		at[id] = append(at[id], i)
	}
	for start := 0; start < len(rels); start += graphBatch {
		chunk := rels[start:min(start+graphBatch, len(rels))]
		raw := make([]string, len(chunk))
		for i, id := range chunk {
			raw[i] = string(id)
		}
		values, err := batchBlobs("relation_id", raw)
		if err != nil {
			return nil, err
		}
		b := newBinder(g.r.gen)
		list := b.markList(values)
		argument, ret := b.mark(graph.FlowDetailArgument), b.mark(graph.FlowDetailReturn)
		query := `SELECT rl.canonical, max(e.detail = ` + argument + `), max(e.detail = ` + ret + `)
			FROM evidence e JOIN relation_ids rl ON rl.id = e.relation_id
			WHERE rl.canonical IN ` + list + memberOf("e") + ` GROUP BY rl.canonical`
		err = g.r.s.read(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, query, b.args...)
			if err != nil {
				return wrap("evidence", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id []byte
				var isArgument, isReturn int64
				if err := rows.Scan(&id, &isArgument, &isReturn); err != nil {
					return wrap("evidence", err)
				}
				for _, i := range at[model.RelationID(idHex(id))] {
					out[i] = graph.FlowRole{Argument: isArgument == 1, Return: isReturn == 1}
				}
			}
			return wrap("evidence", rows.Err())
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
