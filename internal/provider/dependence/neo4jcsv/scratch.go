package neo4jcsv

import (
	"context"
	"database/sql"
	"net/url"
	"strconv"

	"modernc.org/sqlite"
)

// Export labels this import consumes. A mapped label produces facts or is an
// endpoint of an edge that does; a known label is consumed for structure or
// deliberately ignored because it cannot carry a dependence fact; anything
// else is an unknown label, counted and reported (Section 11.6).
const (
	labelMethod    = "METHOD"
	labelCall      = "CALL"
	labelMetaData  = "META_DATA"
	labelIdent     = "IDENTIFIER"
	labelFieldIdMe = "FIELD_IDENTIFIER"
	labelLocal     = "LOCAL"
	labelMember    = "MEMBER"
	labelParamIn   = "METHOD_PARAMETER_IN"
	labelTypeDecl  = "TYPE_DECL"

	edgeCall        = "CALL"
	edgeContains    = "CONTAINS"
	edgeCDG         = "CDG"
	edgeReachingDef = "REACHING_DEF"
	edgeRef         = "REF"
	edgeArgument    = "ARGUMENT"
	edgeAST         = "AST"
)

// stagedNodeLabels are the labels whose rows are staged. Every one of them is
// either published as an entity, anchors to one, or can appear as an endpoint
// of a CDG or REACHING_DEF edge that has to be walked through.
var stagedNodeLabels = map[string]bool{
	labelMethod: true, labelCall: true, labelIdent: true, labelFieldIdMe: true,
	labelLocal: true, labelMember: true, labelParamIn: true, labelTypeDecl: true,
	"METHOD_PARAMETER_OUT": true, "METHOD_RETURN": true, "METHOD_REF": true, "LITERAL": true,
	"BLOCK": true, "RETURN": true, "CONTROL_STRUCTURE": true, "JUMP_TARGET": true,
	"CLOSURE_BINDING": true, "TYPE_REF": true, "UNKNOWN": true,
	"ARRAY_INITIALIZER": true, "TEMPLATE_DOM": true,
}

// knownNodeLabels are labels the import recognizes and deliberately does not
// stage: they carry declarations of shape, modifiers, types or documentation,
// never a control- or data-dependence endpoint.
var knownNodeLabels = map[string]bool{
	labelMetaData: true, "NAMESPACE": true, "NAMESPACE_BLOCK": true, "MODIFIER": true, "FILE": true,
	"BINDING": true, "TYPE": true, "IMPORT": true, "DEPENDENCY": true, "COMMENT": true,
	"ANNOTATION": true, "ANNOTATION_LITERAL": true, "ANNOTATION_PARAMETER": true,
	"ANNOTATION_PARAMETER_ASSIGN": true, "TAG": true, "TAG_NODE_PAIR": true,
	"JUMP_LABEL": true, "KEY_VALUE_PAIR": true, "LOCATION": true,
	"TYPE_ARGUMENT": true, "TYPE_PARAMETER": true,
}

// stagedEdgeLabels are the edge types the projection reads.
var stagedEdgeLabels = map[string]bool{
	edgeCall: true, edgeContains: true, edgeCDG: true, edgeReachingDef: true,
	edgeRef: true, edgeArgument: true, edgeAST: true,
}

// knownEdgeLabels are recognized edge types the projection does not read.
var knownEdgeLabels = map[string]bool{
	"ALIAS_OF": true, "BINDS": true, "CAPTURE": true, "CAPTURED_BY": true, "CFG": true,
	"CONDITION": true, "DOMINATE": true, "EVAL_TYPE": true, "IMPORTS": true,
	"INHERITS_FROM": true, "IS_CALL_FOR_IMPORT": true, "PARAMETER_LINK": true,
	"POINTS_TO": true, "POST_DOMINATE": true, "RECEIVER": true, "SOURCE_FILE": true,
	"TAGGED_BY": true, "TRUE_BODY": true, "FALSE_BODY": true, "FOR_BODY": true,
	"FOR_INIT": true, "FOR_UPDATE": true, "WHILE_BODY": true,
}

// nullable is an optional integer property of a graph node.
type nullable struct {
	Int64 int64
	Valid bool
}

func (n nullable) value() any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// graphNode is one decoded node row of interest.
type graphNode struct {
	id, label, name, fullName, canonicalName, signature string
	filename, code, methodFullName, typeFullName        string
	closureBinding                                      string
	line, lineEnd, col, argIndex                        nullable
	isExternal                                          bool
}

// scratch is the on-disk staging database of one import. Everything the
// import reads lands here first, so an edge that arrives before the nodes it
// joins is resolved by a query, never dropped by a lookup window; and every
// emission is an ordered query, so the facts are a function of the export's
// content and not of the order its files were read.
type scratch struct {
	db           *sql.DB
	rows         int64
	unknown      map[string]uint64
	unknownN     uint64
	ignoredFiles int

	// Staging runs inside an explicit transaction committed every
	// commitEvery rows, with every insert a reused prepared statement.
	// Without both, each row is its own transaction and its own compile: a
	// 50 MB export cost millions of page writes and did not finish in forty
	// minutes. The file is private and disposable, so losing an uncommitted
	// transaction to a crash loses only the scratch, which is correct.
	inTx bool
	stmt map[string]*sql.Stmt
}

const scratchSchema = `
CREATE TABLE files(path TEXT PRIMARY KEY, file_id TEXT NOT NULL, content_hash TEXT NOT NULL, size INTEGER NOT NULL, language TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE nodes(id TEXT PRIMARY KEY, label TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', full_name TEXT NOT NULL DEFAULT '',
	canonical_name TEXT NOT NULL DEFAULT '', signature TEXT NOT NULL DEFAULT '', filename TEXT NOT NULL DEFAULT '',
	line INTEGER, line_end INTEGER, col INTEGER, arg_index INTEGER, code TEXT NOT NULL DEFAULT '',
	is_external INTEGER NOT NULL DEFAULT 0, method_full_name TEXT NOT NULL DEFAULT '', type_full_name TEXT NOT NULL DEFAULT '',
	closure_binding TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '') WITHOUT ROWID;
CREATE TABLE edges(label TEXT NOT NULL, src TEXT NOT NULL, dst TEXT NOT NULL, variable TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(label, src, dst, variable)) WITHOUT ROWID;
CREATE INDEX edges_by_dst ON edges(label, dst);
CREATE INDEX nodes_by_label ON nodes(label, id);
CREATE INDEX nodes_by_type ON nodes(label, full_name);
CREATE INDEX nodes_by_position ON nodes(filename, COALESCE(line, -1), id);
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE ents(id TEXT PRIMARY KEY, kind TEXT NOT NULL, external INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID;
CREATE TABLE anchors(node TEXT PRIMARY KEY, target TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE proj(kind TEXT NOT NULL, from_e TEXT NOT NULL, to_e TEXT NOT NULL, site TEXT NOT NULL,
	op TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', target_name TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(kind, from_e, to_e, site, op, target_name)) WITHOUT ROWID;
CREATE TABLE loc(id TEXT PRIMARY KEY, ok INTEGER NOT NULL, path TEXT NOT NULL DEFAULT '', file_id TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL DEFAULT '', language TEXT NOT NULL DEFAULT '', range_json TEXT NOT NULL DEFAULT '') WITHOUT ROWID;
CREATE TABLE ident(ent TEXT PRIMARY KEY, node_id TEXT NOT NULL, fact_json TEXT NOT NULL, alias_json TEXT NOT NULL, key TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE rels(rel_id TEXT NOT NULL, ev_id TEXT NOT NULL, from_id TEXT NOT NULL, kind TEXT NOT NULL, to_id TEXT NOT NULL,
	ev_json TEXT NOT NULL, key TEXT NOT NULL, PRIMARY KEY(rel_id, ev_id)) WITHOUT ROWID;
CREATE TABLE keys(key TEXT PRIMARY KEY, changed INTEGER NOT NULL DEFAULT 1) WITHOUT ROWID;
CREATE TABLE walk(start TEXT NOT NULL, node TEXT NOT NULL, depth INTEGER NOT NULL, PRIMARY KEY(start, node)) WITHOUT ROWID;
`

// openScratch creates the import's staging database. Durability is
// deliberately off: the file is private, single-writer and deleted with the
// import's scratch directory.
func openScratch(ctx context.Context, path string) (*scratch, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)", "locking_mode(EXCLUSIVE)", "cache_size(-32768)"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, internalErr("import scratch connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, scratchSchema); err != nil {
		db.Close()
		return nil, internalErr("import scratch schema: %v", err)
	}
	return &scratch{db: db, unknown: map[string]uint64{}, stmt: map[string]*sql.Stmt{}}, nil
}

// commitEvery is how many staged rows one staging transaction holds before it
// commits and a new one begins. It bounds the dirty pages the scratch keeps,
// not the rows it can hold.
const commitEvery = 200_000

func (s *scratch) close() error {
	for _, st := range s.stmt {
		st.Close()
	}
	return s.db.Close()
}

// exec runs one reused statement inside the staging transaction, opening the
// transaction and compiling the statement on first use.
func (s *scratch) exec(ctx context.Context, query string, args ...any) error {
	if !s.inTx {
		if _, err := s.db.ExecContext(ctx, "BEGIN"); err != nil {
			return internalErr("import scratch: %v", err)
		}
		s.inTx = true
	}
	st, ok := s.stmt[query]
	if !ok {
		var err error
		if st, err = s.db.PrepareContext(ctx, query); err != nil {
			return internalErr("import scratch: %v", err)
		}
		s.stmt[query] = st
	}
	if _, err := st.ExecContext(ctx, args...); err != nil {
		return internalErr("import scratch: %v", err)
	}
	return nil
}

// commit ends the staging transaction. Reads see uncommitted rows anyway —
// the scratch has exactly one connection — so this bounds dirty pages rather
// than publishing anything.
func (s *scratch) commit(ctx context.Context) error {
	if !s.inTx {
		return nil
	}
	s.inTx = false
	if _, err := s.db.ExecContext(ctx, "COMMIT"); err != nil {
		return internalErr("import scratch: %v", err)
	}
	return nil
}

// staged accounts one staged row against the import's row bound and commits
// the staging transaction at every commitEvery rows.
func (s *scratch) staged(ctx context.Context) error {
	s.rows++
	if s.rows > maxStagedRows {
		return resourceLimit("the export stages more than %d rows", maxStagedRows).WithDetail("limit", "max_staged_rows")
	}
	if s.rows%commitEvery != 0 {
		return nil
	}
	return s.commit(ctx)
}

// countUnknown records a label this import does not classify.
func (s *scratch) countUnknown(label string) {
	s.unknownN++
	if _, ok := s.unknown[label]; ok || len(s.unknown) < maxUnknownLabels {
		s.unknown[label]++
	}
}

func (s *scratch) putNode(ctx context.Context, n graphNode) error {
	if !stagedNodeLabels[n.label] {
		if !knownNodeLabels[n.label] {
			s.countUnknown(n.label)
		}
		return nil
	}
	err := s.exec(ctx, `INSERT OR IGNORE INTO nodes(id, label, name, full_name, canonical_name, signature, filename,
		line, line_end, col, arg_index, code, is_external, method_full_name, type_full_name, closure_binding)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.id, n.label, n.name, n.fullName, n.canonicalName, n.signature, n.filename,
		n.line.value(), n.lineEnd.value(), n.col.value(), n.argIndex.value(), n.code,
		boolInt(n.isExternal), n.methodFullName, n.typeFullName, n.closureBinding)
	if err != nil {
		return err
	}
	return s.staged(ctx)
}

func (s *scratch) putEdge(ctx context.Context, src, dst, label, variable string) error {
	if !stagedEdgeLabels[label] {
		if !knownEdgeLabels[label] {
			s.countUnknown(label)
		}
		return nil
	}
	if err := s.exec(ctx, `INSERT OR IGNORE INTO edges(label, src, dst, variable) VALUES(?,?,?,?)`,
		label, src, dst, variable); err != nil {
		return err
	}
	return s.staged(ctx)
}

func (s *scratch) putMeta(ctx context.Context, key, value string) error {
	return s.exec(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, key, value)
}

// project derives the published entities, their anchors and every relation
// occurrence that does not need operand-shape analysis (Section 11.6):
//
//   - an entity is a non-operator METHOD or a declaration (LOCAL,
//     METHOD_PARAMETER_IN, MEMBER); operator stubs are lowering machinery and
//     are never published;
//   - a graph node anchors to the entity it stands for: an entity to itself, a
//     non-operator call site to the method it invokes, and an identifier to
//     the declaration its REF edge names. Nothing else anchors, so no fact is
//     attributed to a node the export did not bind;
//   - calls: a call site's containing method calls the site's target;
//   - control dependence: on a CDG edge, the dependent node's entity
//     control_depends_on the controlling node's entity, evidenced at the
//     dependent node;
//   - data dependence: a bounded def-use walk from one anchored node through
//     unanchored lowering nodes to the next anchored node. It is
//     reachability the engine computed, never a proven source-to-sink flow,
//     and a walk that leaves the defining method is published as a capture.
//
// Reads and writes need the shape of an assignment's written operand and are
// derived in Go (rw.go).
func (s *scratch) project(ctx context.Context) error {
	if err := s.commit(ctx); err != nil {
		return err
	}
	steps := []string{
		// AST is staged only to attribute a declaration to its container:
		// the export's CONTAINS edges reach statements and expressions but
		// never a local, a parameter or a member. Everything else in AST is
		// structure this import never reads and is dropped before it costs a
		// join.
		`DELETE FROM edges WHERE label = 'AST' AND NOT EXISTS(SELECT 1 FROM nodes d
			WHERE d.id = edges.dst AND d.label IN ('MEMBER', 'LOCAL', 'METHOD_PARAMETER_IN'))`,
		// Every node is attributed to the method that contains it; a method
		// owns itself. A node with several containers takes the smallest id so
		// the attribution is a function of the export alone.
		`UPDATE nodes SET owner = COALESCE((SELECT MIN(k.src) FROM edges k JOIN nodes m ON m.id = k.src AND m.label = 'METHOD'
			WHERE k.label = 'CONTAINS' AND k.dst = nodes.id), '')`,
		`UPDATE nodes SET owner = id WHERE label = 'METHOD'`,
		// A member is contained by its type, not by a method; the type is
		// what owns it for attribution and for the fact key.
		`UPDATE nodes SET owner = COALESCE((SELECT MIN(a.src) FROM edges a JOIN nodes t ON t.id = a.src AND t.label = 'TYPE_DECL'
			WHERE a.label = 'AST' AND a.dst = nodes.id), '') WHERE label = 'MEMBER' AND owner = ''`,
		// A local or a parameter is reached by AST, not by CONTAINS: it takes
		// the owner of its syntactic parent (the method itself for a
		// parameter, the method's block for a local).
		`UPDATE nodes SET owner = COALESCE((SELECT MIN(p.owner) FROM edges a JOIN nodes p ON p.id = a.src
			WHERE a.label = 'AST' AND a.dst = nodes.id AND p.owner <> ''), '')
			WHERE label IN ('LOCAL', 'METHOD_PARAMETER_IN') AND owner = ''`,
		// A call site or identifier carries no FILENAME of its own; it lies in
		// the file of the method that contains it.
		`UPDATE nodes SET filename = COALESCE((SELECT m.filename FROM nodes m WHERE m.id = nodes.owner), '')
			WHERE filename = '' AND owner <> ''`,
		`INSERT OR IGNORE INTO ents(id, kind, external) SELECT id, 'method', is_external FROM nodes
			WHERE label = 'METHOD' AND full_name NOT LIKE '<operator>.%'`,
		// Only declarations of a published container: the parameters of the
		// operator stubs the frontend invents for lowering are machinery, not
		// entities, and publishing them would be noise with no source.
		`INSERT OR IGNORE INTO ents(id, kind, external) SELECT n.id, 'decl', 0 FROM nodes n
			WHERE n.label IN ('LOCAL', 'METHOD_PARAMETER_IN', 'MEMBER')
			AND (EXISTS (SELECT 1 FROM ents m WHERE m.id = n.owner)
				OR EXISTS (SELECT 1 FROM nodes t WHERE t.id = n.owner AND t.label = 'TYPE_DECL'))`,
		`INSERT OR IGNORE INTO anchors(node, target) SELECT id, id FROM ents`,
		`INSERT OR IGNORE INTO anchors(node, target) SELECT c.id, MIN(e.dst) FROM nodes c
			JOIN edges e ON e.label = 'CALL' AND e.src = c.id JOIN ents t ON t.id = e.dst
			WHERE c.label = 'CALL' AND c.method_full_name NOT LIKE '<operator>.%' GROUP BY c.id`,
		`INSERT OR IGNORE INTO anchors(node, target) SELECT i.id, MIN(e.dst) FROM nodes i
			JOIN edges e ON e.label = 'REF' AND e.src = i.id JOIN ents d ON d.id = e.dst AND d.kind = 'decl'
			WHERE i.label IN ('IDENTIFIER', 'FIELD_IDENTIFIER', 'METHOD_REF') GROUP BY i.id`,
		`INSERT OR IGNORE INTO proj(kind, from_e, to_e, site, op, detail, target_name)
			SELECT 'calls', c.owner, a.target, c.id, '', '` + detailCall + `', '' FROM nodes c
			JOIN anchors a ON a.node = c.id JOIN ents o ON o.id = c.owner
			WHERE c.label = 'CALL' AND c.method_full_name NOT LIKE '<operator>.%' AND a.target <> c.owner`,
		`INSERT OR IGNORE INTO proj(kind, from_e, to_e, site, op, detail, target_name)
			SELECT 'control_depends_on', ad.target, ac.target, e.dst, '', '` + detailCDG + `', '' FROM edges e
			JOIN anchors ac ON ac.node = e.src JOIN anchors ad ON ad.node = e.dst
			WHERE e.label = 'CDG' AND ac.target <> ad.target`,
	}
	for _, q := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return internalErr("import projection: %v", err)
		}
	}
	return s.projectDataFlow(ctx)
}

// projectDataFlow walks the export's def-use edges from every anchored node to
// the next anchored node, through the lowering nodes in between (operator
// calls, literals, method returns) but never past a second anchor.
//
// The walk is iterated one bounded level at a time rather than written as a
// recursive CTE. The frontier of each level is only the unanchored nodes the
// previous level reached, which is a small set, whereas the recursive form
// re-evaluates its stop condition for every row it queues and did not finish
// in forty minutes on one unit's quarter of a million def-use edges.
func (s *scratch) projectDataFlow(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO walk(start, node, depth)
		SELECT a.node, e.dst, 1 FROM anchors a JOIN edges e ON e.label = 'REACHING_DEF' AND e.src = a.node`); err != nil {
		return internalErr("import projection: %v", err)
	}
	for depth := 2; depth <= maxDataFlowDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO walk(start, node, depth)
			SELECT w.start, e.dst, ? FROM walk w JOIN edges e ON e.label = 'REACHING_DEF' AND e.src = w.node
			WHERE w.depth = ? AND NOT EXISTS (SELECT 1 FROM anchors x WHERE x.node = w.node)`, depth, depth-1)
		if err != nil {
			return internalErr("import projection: %v", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			break
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO proj(kind, from_e, to_e, site, op, detail, target_name)
		SELECT 'data_flows_to', a1.target, a2.target, w.node, '',
			CASE WHEN (SELECT owner FROM nodes WHERE id = w.start) <> (SELECT owner FROM nodes WHERE id = w.node)
				OR EXISTS (SELECT 1 FROM nodes n WHERE n.id IN (a1.target, a2.target) AND n.closure_binding <> '')
			THEN '`+detailReachDefCB+`' ELSE '`+detailReachDef+`' END, ''
		FROM walk w JOIN anchors a1 ON a1.node = w.start JOIN anchors a2 ON a2.node = w.node
		WHERE a1.target <> a2.target LIMIT `+strconv.Itoa(maxDerivedRows+1))
	if err != nil {
		return internalErr("import projection: %v", err)
	}
	return nil
}

// checkDerived refuses an export that projects past the occurrence bound.
func (s *scratch) checkDerived(ctx context.Context) error {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM proj`).Scan(&n); err != nil {
		return internalErr("import projection: %v", err)
	}
	if n > maxDerivedRows {
		return resourceLimit("the export projects to more than %d relation occurrences", maxDerivedRows).WithDetail("limit", "max_derived_rows")
	}
	return nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
