package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The structural provider names every call it cannot resolve inside its own
// file by a placeholder: a file-less node carrying the callee as written
// (model.MetaCallee) and no call shape, with the caller's `calls` edge to it.
// The linker then publishes, for the same call site, either the precise
// target, an open-site node, or an external node, each as a `calls` edge from
// the same caller whose evidence covers the same bytes: a link site is cut
// from the same call reference the placeholder's evidence is
// (treesitter/link.go builds both ranges from wire.Ref Start and End). Served
// side by side, one call would be two callees and two impact entries, and the
// placeholder would claim a call the linker resolved is unresolved.
//
// So the packed adjacency leaves out a placeholder edge whose every site some
// other `calls` edge of the same caller covers at exactly the same bytes. The
// match is equality of (file, start, end), never overlap: an inner call's
// range lies inside its outer call's, and an outer call the linker bound says
// nothing about the inner one. A placeholder edge with one site nobody else
// covers -- a proven external call the linker publishes no edge for, a file
// the linker never linked -- is kept whole, with every site it has. Decided
// once per generation here, where visibility is, so every walk, callers and
// callees answer and impact reads one edge per call and pays nothing per
// query.

// placeholderNodesInsert collects the generation's call placeholders: the
// visible node facts with no file whose metadata names a callee and no shape,
// the same test the engine's isCallPlaceholder applies. A file-less node is
// the only kind tested, so the metadata of located nodes is never parsed.
//
// Every join of the pass is a CROSS JOIN, which SQLite never reorders: the
// temp tables carry no statistics, and left to choose, the planner drives
// from the permanent table instead -- a scan of every relation in the store
// and a temporary b-tree to restore caller order. Driven from the temp sets,
// each step is a primary-key or index range under the row before it.
const placeholderNodesInsert = `INSERT OR IGNORE INTO tmp_graph_placeholder(id)
	SELECT nf.node_id FROM tmp_graph_unit u
	CROSS JOIN node_facts nf ON nf.unit_id = u.id
	WHERE nf.file_id IS NULL
	  AND json_type(nf.metadata_json, '$.` + model.MetaCallee + `') IS NOT NULL
	  AND json_type(nf.metadata_json, '$.` + model.MetaShape + `') IS NULL`

// placeholderEdgesInsert collects the visible `calls` edges into a
// placeholder, through idx_relations_to(to_node_id, kind, from_node_id).
const placeholderEdgesInsert = `INSERT INTO tmp_graph_placeholder_rel(id, caller)
	SELECT ri.id, ri.from_node_id FROM tmp_graph_placeholder p
	CROSS JOIN relation_ids ri ON ri.to_node_id = p.id AND ri.kind = ?
	CROSS JOIN tmp_graph_rel t ON t.id = ri.id`

// callerSitesQuery streams every visible `calls` evidence row of every caller
// that has a placeholder edge, caller by caller: the caller set is a temp
// primary key read in order, and each caller's edges and their evidence are
// index ranges under it, so the stream needs no sort and the build holds one
// caller's sites at a time.
func callerSitesQuery() string {
	return `SELECT c.id, ri.id, ph.id IS NOT NULL, e.file_id, e.start_byte, e.end_byte
		FROM tmp_graph_placeholder_caller c
		CROSS JOIN relation_ids ri ON ri.from_node_id = c.id AND ri.kind = ?
		CROSS JOIN tmp_graph_rel t ON t.id = ri.id
		CROSS JOIN evidence e ON e.relation_id = ri.id
		CROSS JOIN tmp_graph_unit u ON u.id = e.unit_id
		LEFT JOIN tmp_graph_placeholder_rel ph ON ph.id = ri.id
		ORDER BY c.id`
}

// callSite is one evidence row's bytes.
type callSite struct {
	file       string
	start, end int64
}

// placeholderTemps are the temp tables supersedeCoveredPlaceholders creates.
var placeholderTemps = []string{"tmp_graph_placeholder", "tmp_graph_placeholder_rel",
	"tmp_graph_placeholder_caller", "tmp_graph_superseded"}

// supersedeCoveredPlaceholders removes from tmp_graph_rel every placeholder
// edge whose every visible site another visible `calls` edge of the same
// caller covers at the same bytes. It runs after tmp_graph_rel and
// tmp_graph_unit are filled and before anything is packed.
func supersedeCoveredPlaceholders(ctx context.Context, tx *sql.Tx) error {
	for _, ddl := range []string{
		`CREATE TEMP TABLE tmp_graph_placeholder(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE tmp_graph_placeholder_rel(id INTEGER PRIMARY KEY, caller INTEGER NOT NULL)`,
		`CREATE TEMP TABLE tmp_graph_placeholder_caller(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE tmp_graph_superseded(id INTEGER PRIMARY KEY)`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return wrap("placeholder edges", err)
		}
	}
	calls := string(model.RelCalls)
	if _, err := tx.ExecContext(ctx, placeholderNodesInsert); err != nil {
		return wrap("placeholder edges", err)
	}
	if _, err := tx.ExecContext(ctx, placeholderEdgesInsert, calls); err != nil {
		return wrap("placeholder edges", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tmp_graph_placeholder_caller(id)
		SELECT caller FROM tmp_graph_placeholder_rel`); err != nil {
		return wrap("placeholder edges", err)
	}
	insert, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO tmp_graph_superseded(id) VALUES(?)`)
	if err != nil {
		return wrap("placeholder edges", err)
	}
	defer insert.Close()

	rows, err := tx.QueryContext(ctx, callerSitesQuery(), calls)
	if err != nil {
		return wrap("placeholder edges", err)
	}
	defer rows.Close()
	var (
		caller  int64 = -1
		covered       = map[callSite]bool{}
		// sites is each placeholder edge's located sites; blind marks an edge
		// with a site that has no bytes, which nothing can cover.
		sites = map[int64][]callSite{}
		blind = map[int64]bool{}
	)
	flush := func() error {
		for rel, list := range sites {
			if blind[rel] || len(list) == 0 {
				continue
			}
			all := true
			for _, s := range list {
				if !covered[s] {
					all = false
					break
				}
			}
			if all {
				if _, err := insert.ExecContext(ctx, rel); err != nil {
					return wrap("placeholder edges", err)
				}
			}
		}
		clear(covered)
		clear(sites)
		clear(blind)
		return nil
	}
	for rows.Next() {
		var own, rel int64
		var placeholder bool
		var file []byte
		var start, end sql.NullInt64
		if err := rows.Scan(&own, &rel, &placeholder, &file, &start, &end); err != nil {
			return wrap("placeholder edges", err)
		}
		if own != caller {
			if err := flush(); err != nil {
				return err
			}
			caller = own
		}
		located := file != nil && start.Valid && end.Valid
		site := callSite{file: string(file), start: start.Int64, end: end.Int64}
		switch {
		case placeholder && !located:
			blind[rel] = true
		case placeholder:
			sites[rel] = append(sites[rel], site)
		case located:
			covered[site] = true
		}
	}
	if err := rows.Err(); err != nil {
		return wrap("placeholder edges", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_graph_rel WHERE id IN (SELECT id FROM tmp_graph_superseded)`); err != nil {
		return wrap("placeholder edges", err)
	}
	return nil
}

// A call the linker could not bind is an open site: a node located at the
// call, whose metadata gives the reason and the call's shape (model
// OpenSiteMetadata), and the caller's `calls` edge to it, its evidence keyed
// by the call-site key of Section 11.3. A SCIP index that resolved the same
// call publishes `calls` from the same caller with a `compiler` row at the
// same key. Served side by side, the call would be listed twice, once as a
// call whose target is unknown, beside the target the compiler named.
//
// So the packed adjacency leaves out an open-site edge whose every visible
// evidence row some other visible `calls` edge of the same caller carries a
// `compiler` row for at the same native key. A linker target that differs
// from the compiler's is not an open site and stays: two relations for one
// call, which say the two analyses disagree.

// openSiteNodesInsert collects the generation's open sites: the visible
// located node facts whose metadata names a reason and a shape. Most node
// facts carry no metadata, and the empty object is compared before any is
// parsed.
const openSiteNodesInsert = `INSERT OR IGNORE INTO tmp_graph_opensite(id)
	SELECT nf.node_id FROM tmp_graph_unit u
	CROSS JOIN node_facts nf ON nf.unit_id = u.id
	WHERE nf.file_id IS NOT NULL AND nf.metadata_json <> '{}'
	  AND json_type(nf.metadata_json, '$.` + model.MetaReason + `') IS NOT NULL
	  AND json_type(nf.metadata_json, '$.` + model.MetaShape + `') IS NOT NULL`

// resolvedOpenSitesDelete removes the open-site edges a compiler row
// resolves. Each step is an index range under the row before it: the open
// site's incoming `calls` (idx_relations_to), each edge's evidence
// (idx_evidence_relation), and the rows sharing its native key
// (idx_evidence_native), whose relation must leave the same caller.
const resolvedOpenSitesDelete = `DELETE FROM tmp_graph_rel WHERE id IN (
	SELECT ri.id FROM tmp_graph_opensite o
	CROSS JOIN relation_ids ri ON ri.to_node_id = o.id AND ri.kind = ?1
	CROSS JOIN tmp_graph_rel t ON t.id = ri.id
	WHERE EXISTS (SELECT 1 FROM evidence e CROSS JOIN tmp_graph_unit u ON u.id = e.unit_id WHERE e.relation_id = ri.id)
	  AND NOT EXISTS (
		SELECT 1 FROM evidence e CROSS JOIN tmp_graph_unit u ON u.id = e.unit_id
		WHERE e.relation_id = ri.id AND NOT EXISTS (
			SELECT 1 FROM evidence c
			CROSS JOIN tmp_graph_unit cu ON cu.id = c.unit_id
			CROSS JOIN relation_ids rc ON rc.id = c.relation_id
			CROSS JOIN tmp_graph_rel ct ON ct.id = rc.id
			WHERE c.native_key_id = e.native_key_id AND c.precision = ?2
			  AND rc.from_node_id = ri.from_node_id AND rc.kind = ?1 AND rc.id <> ri.id)))`

// openSiteTemps are the temp tables supersedeResolvedOpenSites creates.
var openSiteTemps = []string{"tmp_graph_opensite"}

// supersedeResolvedOpenSites removes from tmp_graph_rel every open-site edge
// a compiler row of the same caller resolves at the same call-site key. It
// runs after tmp_graph_rel and tmp_graph_unit are filled and before anything
// is packed.
func supersedeResolvedOpenSites(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_graph_opensite(id INTEGER PRIMARY KEY)`); err != nil {
		return wrap("open-site edges", err)
	}
	if _, err := tx.ExecContext(ctx, openSiteNodesInsert); err != nil {
		return wrap("open-site edges", err)
	}
	if _, err := tx.ExecContext(ctx, resolvedOpenSitesDelete, string(model.RelCalls), string(model.PrecisionCompiler)); err != nil {
		return wrap("open-site edges", err)
	}
	return nil
}
