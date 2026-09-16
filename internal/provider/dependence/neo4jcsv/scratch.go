package neo4jcsv

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"modernc.org/sqlite"

	"github.com/Sawmonabo/codectx/internal/config"
	arena "github.com/Sawmonabo/codectx/internal/scratch"
	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
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
	labelMethodRef = "METHOD_REF"
	labelLocal     = "LOCAL"
	labelMember    = "MEMBER"
	labelParamIn   = "METHOD_PARAMETER_IN"
	labelTypeDecl  = "TYPE_DECL"

	// speculatedParent is the namespace the export parks an invented callee
	// under: a method it emitted with no definition anywhere in the graph,
	// so that a call site whose target it could not find still has one. A
	// callee under it is a guess, not a dependency the source states.
	speculatedParent = "<speculatedMethods>"

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
	"METHOD_PARAMETER_OUT": true, "METHOD_RETURN": true, labelMethodRef: true, "LITERAL": true,
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
	id                                              int64
	label, name, fullName, canonicalName, signature string
	filename, code, methodFullName, typeFullName    string
	closureBinding                                  string
	line, lineEnd, col, argIndex                    nullable
	isExternal, speculated                          bool
}

// scratch is the on-disk staging database of one import. Everything the
// import reads lands here first, so an edge that arrives before the nodes it
// joins is resolved by a query, never dropped by a lookup window; and every
// emission is an ordered query, so the facts are a function of the export's
// content and not of the order its files were read.
//
// Every table is written the way a bulk load writes: rows are appended in
// the order they arrive, to a table with no secondary index, and every
// structure a later phase reads in another order is built afterwards in one
// pass -- a sorted copy or an index, each produced by the engine's external
// merge sort and written once, front to back. Nothing updates a row after it
// is written and nothing inserts into the middle of a b-tree larger than the
// page cache. That is what keeps the staging's disk traffic a small constant
// times the export's bytes: a random-keyed insert into a b-tree larger than
// the cache costs a page write per row, and a whole-table update costs the
// table, and the first design of this scratch did both, at thirty to forty
// times the export's bytes (ADR-0009).
type scratch struct {
	db *sql.DB
	// lease is the pooled surface the staging database is written into. It
	// goes back to the pool at its length when the import ends.
	lease *arena.Lease
	// defaultKeys is where the import writes its key file when the caller
	// names none. It sits BESIDE the pool, never inside it: a file in the
	// pool's own directory would be counted as pooled space and read by the
	// sweep that finds surfaces to reuse.
	defaultKeys string
	rows        int64
	// maxRows is the user's `providers.dependence.max_staged_rows`, unlimited for
	// unlimited. It never stops the staging: crossing it sets overRows once,
	// which the import reports.
	maxRows  config.Limit
	overRows bool
	// derivedRows is how many distinct relation occurrences the projection
	// derived. It is a count, never a bound: the projection is not truncated
	// and the unit is not failed for it. Import compares it against the
	// user's `providers.dependence.max_derived_rows` and reports the crossing.
	derivedRows  int64
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

// scratchSchema holds the tables the load phase appends to. Everything else
// is created by the phase that fills it, after the tables it reads are
// complete, so that each is written in one ordered pass.
const scratchSchema = `
CREATE TABLE files(id INTEGER PRIMARY KEY, path TEXT NOT NULL, file_id TEXT NOT NULL, content_hash TEXT NOT NULL,
	size INTEGER NOT NULL, language TEXT NOT NULL);
CREATE UNIQUE INDEX files_by_path ON files(path);
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE node_in(seq INTEGER PRIMARY KEY, id INTEGER NOT NULL, label TEXT NOT NULL, name TEXT NOT NULL,
	full_name TEXT NOT NULL, canonical_name TEXT NOT NULL, signature TEXT NOT NULL, filename TEXT NOT NULL,
	line INTEGER, line_end INTEGER, col INTEGER, arg_index INTEGER, is_external INTEGER NOT NULL,
	method_full_name TEXT NOT NULL, type_full_name TEXT NOT NULL, closure_binding TEXT NOT NULL,
	speculated INTEGER NOT NULL);
CREATE TABLE code(seq INTEGER PRIMARY KEY, id INTEGER NOT NULL, code TEXT NOT NULL);
CREATE TABLE edge_in(seq INTEGER PRIMARY KEY, label TEXT NOT NULL, src INTEGER NOT NULL, dst INTEGER NOT NULL);
`

// defaultCacheKiB is the staging database's page cache when the caller sets
// none. It bounds the memory one import holds for its staging and it is the
// buffer the engine sorts in, so a table smaller than it is sorted without a
// spill file.
const defaultCacheKiB = 256 << 10

// The staging database is a pooled scratch surface (internal/scratch), taken
// for the length of one import and given back holding its bytes.
//
// A slot's file is created once and reused by every later import that takes
// it: its tables are dropped at the start of an import and the engine recycles
// their pages from the file's free list, so the file never shrinks and an
// import frees nothing. Hundreds of megabytes created and deleted per unit is
// what the run must not do: on a host that discards freed blocks into a sparse
// image, a multi-gigabyte free stalls every process on the machine.
//
// The pool is what keeps the reuse correct whether or not imports through one
// provider overlap: a staging database is opened with an exclusive lock, so
// two concurrent imports must have two files, and one given back is taken by
// the next rather than created afresh. It is the arena's pool rather than one
// of this package's own so that the largest surface the product writes is in
// the figure the resources block reports as the disk a run is holding, and so
// that a run which died holding one has it taken over rather than left.

// resetSchema empties the staging database and creates the tables the load
// phase appends to. Every object a previous import left is dropped, which
// returns its pages to the file's free list for this import to write over;
// automatic vacuuming is off, so the file itself never shrinks and nothing is
// freed to the filesystem. Dropping is also what makes the schema creation
// idempotent: every phase of an import creates the structures it fills, and a
// reused file would otherwise already hold them.
func resetSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return internalErr("import scratch reset: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return internalErr("import scratch reset: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return internalErr("import scratch reset: %v", err)
	}
	rows.Close()
	for _, name := range tables {
		// A table's indexes go with it, so nothing else has to be listed.
		if _, err := db.ExecContext(ctx, `DROP TABLE "`+strings.ReplaceAll(name, `"`, `""`)+`"`); err != nil {
			return internalErr("import scratch reset: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, scratchSchema); err != nil {
		return internalErr("import scratch schema: %v", err)
	}
	return nil
}

// openScratch takes the import's staging database from the pool of dir, which
// a previous import may already have written, and empties it. Durability is
// deliberately off: the file is private, single-writer and carries nothing
// across an import. Its pages reach the disk through the paced file system
// every connection of the process uses, so a phase that fills the page cache
// at memory speed hands the disk its pages at the disk's own rate rather than
// in one burst at the commit.
func openScratch(ctx context.Context, dir string, cacheKiB int, maxRows config.Limit) (*scratch, error) {
	if cacheKiB <= 0 {
		cacheKiB = defaultCacheKiB
	}
	if dir == "" {
		dir = os.TempDir()
	}
	lease, err := arena.For(dir).Take(arena.ImportStaging)
	if err != nil {
		return nil, internalErr("import scratch surface: %v", err)
	}
	path := lease.Path()
	defaultKeys := filepath.Join(dir, "keys-"+filepath.Base(path))
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)", "locking_mode(EXCLUSIVE)",
		"cache_size(-" + strconv.Itoa(cacheKiB) + ")"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	if err := pacedvfs.Register(); err != nil {
		lease.Release()
		return nil, internalErr("import scratch: %v", err)
	}
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		lease.Release()
		return nil, internalErr("import scratch connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := resetSchema(ctx, db); err != nil {
		// Emptying the surface is what failed, so the surface itself is the
		// problem -- an image a crash left mid-write, whose DROP TABLE cannot
		// run. Given back it would fail every import after this one, for the
		// life of the data directory; it leaves the pool instead.
		db.Close()
		lease.Unusable()
		return nil, err
	}
	return &scratch{db: db, lease: lease, defaultKeys: defaultKeys, maxRows: maxRows,
		unknown: map[string]uint64{}, stmt: map[string]*sql.Stmt{}}, nil
}

// commitEvery is how many staged rows one staging transaction holds before it
// commits and a new one begins. It bounds the dirty pages the scratch keeps,
// not the rows it can hold.
const commitEvery = 200_000

// close gives the staging surface back to the pool at its length, freeing
// nothing: the next import writes over it.
func (s *scratch) close() error {
	for _, st := range s.stmt {
		st.Close()
	}
	err := s.db.Close()
	if s.lease != nil {
		s.lease.Release()
		s.lease = nil
	}
	return err
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

// run commits the staging transaction and executes one derivation statement
// on its own: a bulk INSERT ... SELECT or a CREATE INDEX, each of which the
// engine runs as one ordered pass.
func (s *scratch) run(ctx context.Context, what, query string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.commit(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return internalErr("import %s: %v", what, err)
	}
	return nil
}

// staged counts one staged row and commits the staging transaction at every
// commitEvery rows.
//
// A row count is a property of the repository's source, so it never fails the
// unit: staging is an on-disk database read back through keyset pages, so the
// rows bound disk, not heap. A user who set `providers.dependence.max_staged_rows`
// is told the import crossed it: Report.StagedRows and Report.OverStagedRows
// carry it out to the provider, which logs it and publishes it on the unit's
// capability rows. This package does no logging of its own.
func (s *scratch) staged(ctx context.Context) error {
	s.rows++
	if s.maxRows.Exceeded(s.rows) {
		s.overRows = true
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
	err := s.exec(ctx, `INSERT INTO node_in(id, label, name, full_name, canonical_name, signature, filename,
		line, line_end, col, arg_index, is_external, method_full_name, type_full_name, closure_binding, speculated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.id, n.label, n.name, n.fullName, n.canonicalName, n.signature, n.filename,
		n.line.value(), n.lineEnd.value(), n.col.value(), n.argIndex.value(),
		boolInt(n.isExternal), n.methodFullName, n.typeFullName, n.closureBinding, boolInt(n.speculated))
	if err != nil {
		return err
	}
	if n.code != "" {
		if err := s.exec(ctx, `INSERT INTO code(id, code) VALUES(?, ?)`, n.id, n.code); err != nil {
			return err
		}
	}
	return s.staged(ctx)
}

func (s *scratch) putEdge(ctx context.Context, src, dst int64, label string) error {
	if !stagedEdgeLabels[label] {
		if !knownEdgeLabels[label] {
			s.countUnknown(label)
		}
		return nil
	}
	if err := s.exec(ctx, `INSERT INTO edge_in(label, src, dst) VALUES(?,?,?)`, label, src, dst); err != nil {
		return err
	}
	return s.staged(ctx)
}

func (s *scratch) putMeta(ctx context.Context, key, value string) error {
	return s.exec(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, key, value)
}

// putFile records one manifest entry; a path the manifest repeats keeps its
// last version. The manifest is small beside the export and arrives in path
// order, so its path index is appended to.
func (s *scratch) putFile(ctx context.Context, path, fileID, hash string, size int64, language string) error {
	return s.exec(ctx, `INSERT OR REPLACE INTO files(path, file_id, content_hash, size, language) VALUES(?,?,?,?,?)`,
		path, fileID, hash, size, language)
}

// order turns the appended export into the structures the projection reads:
// the node table keyed by id, the edge table in (type, source, target) order
// with a reversed copy for the two types read by their target, and the
// label index over the nodes. Every one is written in one pass from the
// engine's sort. A node id the export repeated keeps its first row.
//
// AST edges are kept only where they reach a member, a local or a parameter:
// the export's CONTAINS edges reach statements and expressions but never a
// declaration, so those are the AST edges that attribute a declaration to
// its container, and everything else in AST is structure this import never
// reads.
func (s *scratch) order(ctx context.Context) error {
	steps := [...]struct{ what, query string }{
		{"nodes", `CREATE TABLE nodes(id INTEGER PRIMARY KEY, label TEXT NOT NULL, name TEXT NOT NULL,
			full_name TEXT NOT NULL, canonical_name TEXT NOT NULL, signature TEXT NOT NULL, filename TEXT NOT NULL,
			line INTEGER, line_end INTEGER, col INTEGER, arg_index INTEGER, is_external INTEGER NOT NULL,
			method_full_name TEXT NOT NULL, type_full_name TEXT NOT NULL, closure_binding TEXT NOT NULL,
			speculated INTEGER NOT NULL)`},
		{"nodes", `INSERT OR IGNORE INTO nodes SELECT id, label, name, full_name, canonical_name, signature, filename,
			line, line_end, col, arg_index, is_external, method_full_name, type_full_name, closure_binding, speculated
			FROM node_in ORDER BY id, seq`},
		{"nodes", `CREATE INDEX nodes_by_label ON nodes(label, id)`},
		{"code", `CREATE INDEX code_by_id ON code(id)`},
		{"edges", `CREATE TABLE edges(label TEXT NOT NULL, src INTEGER NOT NULL, dst INTEGER NOT NULL,
			PRIMARY KEY(label, src, dst)) WITHOUT ROWID`},
		{"edges", `INSERT OR IGNORE INTO edges SELECT label, src, dst FROM edge_in
			WHERE label <> '` + edgeAST + `' OR dst IN (SELECT id FROM nodes WHERE label IN ('` +
			labelMember + `', '` + labelLocal + `', '` + labelParamIn + `'))
			ORDER BY label, src, dst, seq`},
		{"edges", `CREATE TABLE edges_rev(label TEXT NOT NULL, dst INTEGER NOT NULL, src INTEGER NOT NULL,
			PRIMARY KEY(label, dst, src)) WITHOUT ROWID`},
		{"edges", `INSERT OR IGNORE INTO edges_rev SELECT label, dst, src FROM edges
			WHERE label IN ('` + edgeContains + `', '` + edgeAST + `') ORDER BY label, dst, src`},
	}
	for _, st := range steps {
		if err := s.run(ctx, st.what, st.query); err != nil {
			return err
		}
	}
	return nil
}

// project derives the published entities, their anchors and every relation
// occurrence that does not need operand-shape analysis (Section 11.6):
//
//   - an entity is a non-operator METHOD or a declaration (LOCAL,
//     METHOD_PARAMETER_IN, MEMBER); operator stubs are lowering machinery and
//     are never published;
//   - a graph node anchors to the entity it stands for: an entity to itself, a
//     non-operator call site to the method it invokes, and an identifier to
//     the declaration its REF edge names. A method reference is the one node
//     whose REF edge names a method rather than a declaration -- it is the
//     method taken as a value -- so it anchors to that method, and every fact
//     derived through it names the method the value carries. Nothing else
//     anchors, so no fact is attributed to a node the export did not bind;
//   - calls: a call site's containing method calls the site's target;
//   - control dependence: on a CDG edge, the dependent node's entity
//     control_depends_on the controlling node's entity, evidenced at the
//     dependent node;
//   - data dependence: a bounded def-use walk from one anchored node through
//     unanchored lowering nodes to the next anchored node. It is
//     reachability the engine computed, never a proven source-to-sink flow,
//     and a walk that leaves the defining method is published as a capture.
//
// Every derived table is keyed by node id and filled in that order, so each
// is one appended pass; a node's attributes live in a table of their own
// rather than in columns updated on the node row.
//
// Reads and writes need the shape of an assignment's written operand and are
// derived in Go (rw.go).
func (s *scratch) project(ctx context.Context) error {
	steps := [...]struct{ what, query string }{
		// Every file name the export mentions, once, so a node's file is one
		// integer in its attribute row.
		{"paths", `CREATE TABLE paths(id INTEGER PRIMARY KEY, path TEXT NOT NULL)`},
		{"paths", `INSERT INTO paths(path) SELECT DISTINCT filename FROM nodes WHERE filename <> '' ORDER BY filename`},
		{"paths", `CREATE UNIQUE INDEX paths_by_path ON paths(path)`},
		// Every node is attributed to the method that contains it; a method
		// owns itself. A node with several containers takes the smallest id
		// so the attribution is a function of the export alone. A member is
		// contained by its type, not by a method; the type is what owns it
		// for attribution and for the fact key.
		{"owners", `CREATE TABLE owners(id INTEGER PRIMARY KEY, owner INTEGER)`},
		{"owners", `INSERT INTO owners SELECT n.id, CASE WHEN n.label = '` + labelMethod + `' THEN n.id ELSE COALESCE(
			(SELECT MIN(k.src) FROM edges_rev k JOIN nodes m ON m.id = k.src AND m.label = '` + labelMethod + `'
				WHERE k.label = '` + edgeContains + `' AND k.dst = n.id),
			CASE WHEN n.label = '` + labelMember + `' THEN
				(SELECT MIN(a.src) FROM edges_rev a JOIN nodes t ON t.id = a.src AND t.label = '` + labelTypeDecl + `'
				WHERE a.label = '` + edgeAST + `' AND a.dst = n.id) END) END
			FROM nodes n ORDER BY n.id`},
		// A local or a parameter is reached by AST, not by CONTAINS: it takes
		// the owner of its syntactic parent (the method itself for a
		// parameter, the method's block for a local). A call site or
		// identifier carries no file of its own; it lies in the file of the
		// method that contains it.
		{"attributes", `CREATE TABLE attr(id INTEGER PRIMARY KEY, owner INTEGER, path INTEGER)`},
		{"attributes", `INSERT INTO attr SELECT x.id, x.owner,
			(SELECT p.id FROM paths p WHERE p.path = COALESCE(NULLIF(x.filename, ''),
				(SELECT m.filename FROM nodes m WHERE m.id = x.owner), ''))
			FROM (SELECT n.id, n.filename, CASE WHEN n.label IN ('` + labelLocal + `', '` + labelParamIn + `') AND o.owner IS NULL
				THEN (SELECT MIN(p.owner) FROM edges_rev a JOIN owners p ON p.id = a.src
					WHERE a.label = '` + edgeAST + `' AND a.dst = n.id AND p.owner IS NOT NULL)
				ELSE o.owner END AS owner
				FROM nodes n JOIN owners o ON o.id = n.id) x ORDER BY x.id`},
		// Only declarations of a published container: the parameters of the
		// operator stubs the frontend invents for lowering are machinery, not
		// entities, and publishing them would be noise with no source.
		{"entities", `CREATE TABLE ents(id INTEGER PRIMARY KEY, kind TEXT NOT NULL, external INTEGER NOT NULL)`},
		{"entities", `INSERT INTO ents SELECT id, '` + kindMethod + `', is_external FROM nodes
			WHERE label = '` + labelMethod + `' AND full_name NOT LIKE '<operator>.%'
			UNION ALL SELECT n.id, '` + kindDecl + `', 0 FROM nodes n JOIN attr a ON a.id = n.id
			WHERE n.label IN ('` + labelLocal + `', '` + labelParamIn + `', '` + labelMember + `')
			AND EXISTS (SELECT 1 FROM nodes m WHERE m.id = a.owner
				AND ((m.label = '` + labelMethod + `' AND m.full_name NOT LIKE '<operator>.%') OR m.label = '` + labelTypeDecl + `'))
			ORDER BY 1`},
		{"anchors", `CREATE TABLE anchors(node INTEGER PRIMARY KEY, target INTEGER NOT NULL)`},
		{"anchors", `INSERT INTO anchors SELECT id, id FROM ents
			UNION ALL SELECT c.id, MIN(e.dst) FROM nodes c
				JOIN edges e ON e.label = '` + edgeCall + `' AND e.src = c.id JOIN ents t ON t.id = e.dst
				WHERE c.label = '` + labelCall + `' AND c.method_full_name NOT LIKE '<operator>.%' GROUP BY c.id
			UNION ALL SELECT i.id, MIN(e.dst) FROM nodes i
				JOIN edges e ON e.label = '` + edgeRef + `' AND e.src = i.id JOIN ents d ON d.id = e.dst
				WHERE (i.label IN ('` + labelIdent + `', '` + labelFieldIdMe + `') AND d.kind = '` + kindDecl + `')
					OR (i.label = '` + labelMethodRef + `' AND d.kind IN ('` + kindDecl + `', '` + kindMethod + `')) GROUP BY i.id
			ORDER BY 1`},
		// The field-access map, built in one pass instead of one join per
		// write site. A type with two members of the same name takes the
		// smallest id, which is the same choice the per-site query made, so
		// the map is a function of the export alone.
		{"members", `CREATE TABLE members(type_full_name TEXT NOT NULL, name TEXT NOT NULL, id INTEGER NOT NULL,
			PRIMARY KEY(type_full_name, name)) WITHOUT ROWID`},
		{"members", `INSERT OR IGNORE INTO members SELECT t.full_name, m.name, MIN(m.id) FROM nodes t
			JOIN edges a ON a.label = '` + edgeAST + `' AND a.src = t.id
			JOIN nodes m ON m.id = a.dst AND m.label = '` + labelMember + `'
			WHERE t.label = '` + labelTypeDecl + `' AND t.full_name <> '' AND m.name <> ''
			GROUP BY t.full_name, m.name`},
		// Occurrences are appended as each derivation produces them; the
		// sorted, de-duplicated copy every later phase reads is built once
		// they are all in (occurrences()).
		{"projection", `CREATE TABLE proj(seq INTEGER PRIMARY KEY, kind TEXT NOT NULL, from_e INTEGER NOT NULL,
			to_e INTEGER NOT NULL, site INTEGER NOT NULL, op TEXT NOT NULL, detail TEXT NOT NULL, target_name TEXT NOT NULL)`},
		{"projection", `INSERT INTO proj(kind, from_e, to_e, site, op, detail, target_name)
			SELECT 'calls', a.owner, an.target, c.id, '',
				CASE WHEN t.speculated = 1 THEN '` + detailCallSpeculated + `' ELSE '` + detailCall + `' END, '' FROM nodes c
			JOIN attr a ON a.id = c.id JOIN anchors an ON an.node = c.id JOIN ents o ON o.id = a.owner
			JOIN nodes t ON t.id = an.target
			WHERE c.label = '` + labelCall + `' AND c.method_full_name NOT LIKE '<operator>.%' AND an.target <> a.owner`},
		{"projection", `INSERT INTO proj(kind, from_e, to_e, site, op, detail, target_name)
			SELECT 'control_depends_on', ad.target, ac.target, e.dst, '', '` + detailCDG + `', '' FROM edges e
			JOIN anchors ac ON ac.node = e.src JOIN anchors ad ON ad.node = e.dst
			WHERE e.label = '` + edgeCDG + `' AND ac.target <> ad.target`},
	}
	for _, st := range steps {
		if err := s.run(ctx, st.what, st.query); err != nil {
			return err
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
// in forty minutes on one unit's quarter of a million def-use edges. Each
// level is read through the depth index, whose entries a level appends, and
// the level's new pairs are inserted in start order, so a leaf of the walk
// is rewritten at most once per level.
func (s *scratch) projectDataFlow(ctx context.Context) error {
	steps := [...]string{
		`CREATE TABLE walk(start INTEGER NOT NULL, node INTEGER NOT NULL, depth INTEGER NOT NULL,
			PRIMARY KEY(start, node)) WITHOUT ROWID`,
		`CREATE INDEX walk_by_depth ON walk(depth, start, node)`,
		`INSERT OR IGNORE INTO walk SELECT a.node, e.dst, 1 FROM anchors a
			JOIN edges e ON e.label = '` + edgeReachingDef + `' AND e.src = a.node`,
	}
	for _, q := range steps {
		if err := s.run(ctx, "projection", q); err != nil {
			return err
		}
	}
	for depth := 2; depth <= maxDataFlowDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO walk(start, node, depth)
			SELECT w.start, e.dst, ? FROM walk w JOIN edges e ON e.label = '`+edgeReachingDef+`' AND e.src = w.node
			WHERE w.depth = ? AND NOT EXISTS (SELECT 1 FROM anchors x WHERE x.node = w.node)`, depth, depth-1)
		if err != nil {
			return internalErr("import projection: %v", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			break
		}
	}
	return s.run(ctx, "projection", `INSERT INTO proj(kind, from_e, to_e, site, op, detail, target_name)
		SELECT 'data_flows_to', a1.target, a2.target, w.node, '',
			CASE WHEN (SELECT owner FROM attr WHERE id = w.start) IS NOT (SELECT owner FROM attr WHERE id = w.node)
				OR EXISTS (SELECT 1 FROM nodes n WHERE n.id IN (a1.target, a2.target) AND n.closure_binding <> '')
			THEN '`+detailReachDefCB+`' ELSE '`+detailReachDef+`' END, ''
		FROM walk w JOIN anchors a1 ON a1.node = w.start JOIN anchors a2 ON a2.node = w.node
		WHERE a1.target <> a2.target`)
}

// occurrences builds the sorted, de-duplicated projection every later phase
// reads: one row per distinct (kind, endpoints, site, operator, target
// name), keeping the first derived detail, in the order the relation staging
// pages it. It records how many rows that is.
//
// An occurrence count is a property of the analysed source, so it never
// refuses the export and never truncates the projection: the occurrences live
// in the same on-disk staging database as the rows they came from and are read
// back one keyset page at a time, so the count bounds disk, not heap. A user
// who set `providers.dependence.max_derived_rows` is told the import crossed
// it -- Report.DerivedRows and Report.OverDerivedRows carry it out to the
// provider, which logs it and publishes it on the unit's capability rows.
func (s *scratch) occurrences(ctx context.Context) error {
	steps := [...]string{
		`CREATE TABLE projs(kind TEXT NOT NULL, from_e INTEGER NOT NULL, to_e INTEGER NOT NULL, site INTEGER NOT NULL,
			op TEXT NOT NULL, target_name TEXT NOT NULL, detail TEXT NOT NULL,
			PRIMARY KEY(kind, from_e, to_e, site, op, target_name)) WITHOUT ROWID`,
		`INSERT OR IGNORE INTO projs SELECT kind, from_e, to_e, site, op, target_name, detail FROM proj
			ORDER BY kind, from_e, to_e, site, op, target_name, seq`,
	}
	for _, q := range steps {
		if err := s.run(ctx, "projection", q); err != nil {
			return err
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM projs`).Scan(&s.derivedRows); err != nil {
		return internalErr("import projection: %v", err)
	}
	return nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
