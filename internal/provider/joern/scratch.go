package joern

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"modernc.org/sqlite"
)

// Bounds on the import. They are contract ceilings of this adapter, not
// tunables: a Joern export that exceeds one is refused as malformed or over
// budget rather than partially admitted.
const (
	// maxRecordBytes bounds one CSV record and one XML token before the
	// standard decoder sees it. It equals the smallest max_provider_record_bytes
	// the conformance harness uses, so the decode reservation always fits.
	maxRecordBytes = 64 << 10
	// maxFields bounds the columns of one CSV header.
	maxFields = 64
	// maxExportFiles bounds the entries of one export directory; Joern writes
	// two files per label and has well under a hundred labels.
	maxExportFiles = 4096
	// maxXMLDepth bounds GraphML nesting: graphml/graph/node/data is four.
	maxXMLDepth = 16
	// maxLabelBytes bounds a node label or edge type.
	maxLabelBytes = 128
	// maxUnknownLabels bounds the distinct unknown labels reported by name;
	// the rest are counted in aggregate.
	maxUnknownLabels = 32
	// maxDataFlowDepth bounds the intraprocedural REACHING_DEF walk between
	// two anchored call sites.
	maxDataFlowDepth = 8
	// maxDerivedRows bounds the projected relation occurrences of one unit.
	maxDerivedRows = 4_000_000
	// diskCheckEvery is how many staged rows pass between scratch size checks.
	diskCheckEvery = 4096
)

// Joern labels this profile understands. Mapped labels produce facts; known
// labels are consumed for structure or deliberately ignored; anything else is
// an unknown label, counted and reported (Section 11.6).
const (
	labelMethod   = "METHOD"
	labelCall     = "CALL"
	labelMetaData = "META_DATA"

	edgeCall        = "CALL"
	edgeContains    = "CONTAINS"
	edgeCDG         = "CDG"
	edgeReachingDef = "REACHING_DEF"
)

var knownNodeLabels = map[string]bool{
	labelMethod: true, labelCall: true, labelMetaData: true,
	"ANNOTATION": true, "ANNOTATION_LITERAL": true, "ANNOTATION_PARAMETER": true, "ANNOTATION_PARAMETER_ASSIGN": true,
	"ARRAY_INITIALIZER": true, "BINDING": true, "BLOCK": true, "CLOSURE_BINDING": true, "COMMENT": true,
	"CONTROL_STRUCTURE": true, "DEPENDENCY": true, "FIELD_IDENTIFIER": true, "FILE": true, "IDENTIFIER": true,
	"IMPORT": true, "JUMP_LABEL": true, "JUMP_TARGET": true, "KEY_VALUE_PAIR": true, "LITERAL": true, "LOCAL": true,
	"LOCATION": true, "MEMBER": true, "METHOD_PARAMETER_IN": true, "METHOD_PARAMETER_OUT": true, "METHOD_REF": true,
	"METHOD_RETURN": true, "MODIFIER": true, "NAMESPACE": true, "NAMESPACE_BLOCK": true, "RETURN": true, "TAG": true,
	"TAG_NODE_PAIR": true, "TEMPLATE_DOM": true, "TYPE": true, "TYPE_ARGUMENT": true, "TYPE_DECL": true,
	"TYPE_PARAMETER": true, "TYPE_REF": true, "UNKNOWN": true,
}

var knownEdgeLabels = map[string]bool{
	edgeCall: true, edgeContains: true, edgeCDG: true, edgeReachingDef: true,
	"ALIAS_OF": true, "ARGUMENT": true, "AST": true, "BINDS": true, "CAPTURE": true, "CAPTURED_BY": true,
	"CFG": true, "CONDITION": true, "DOMINATE": true, "EVAL_TYPE": true, "IMPORTS": true, "INHERITS_FROM": true,
	"IS_CALL_FOR_IMPORT": true, "PARAMETER_LINK": true, "POINTS_TO": true, "POST_DOMINATE": true, "RECEIVER": true,
	"REF": true, "SOURCE_FILE": true, "TAGGED_BY": true,
}

// scratch is the on-disk staging database of one run. Everything the import
// reads lands here first, so an edge that arrives before the nodes it joins
// is resolved by a query, never dropped by a lookup window; and every
// emission is an ordered query, so the facts are a function of the export's
// content and not of the order its files were read.
type scratch struct {
	db       *sql.DB
	path     string
	budget   int64
	rows     int64
	unknown  map[string]uint64
	unknownN uint64
	dangling uint64
}

const scratchSchema = `
CREATE TABLE files(path TEXT PRIMARY KEY, file_id TEXT NOT NULL, content_hash TEXT NOT NULL, size INTEGER NOT NULL, language TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE nodes(id TEXT PRIMARY KEY, label TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', full_name TEXT NOT NULL DEFAULT '',
	signature TEXT NOT NULL DEFAULT '', filename TEXT NOT NULL DEFAULT '', line INTEGER, line_end INTEGER, col INTEGER,
	code TEXT NOT NULL DEFAULT '', is_external INTEGER NOT NULL DEFAULT 0, method_full_name TEXT NOT NULL DEFAULT '') WITHOUT ROWID;
CREATE TABLE edges(src TEXT NOT NULL, dst TEXT NOT NULL, label TEXT NOT NULL, variable TEXT NOT NULL DEFAULT '', PRIMARY KEY(src, dst, label, variable)) WITHOUT ROWID;
CREATE INDEX edges_by_dst ON edges(dst, label);
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE anchors(node TEXT PRIMARY KEY, method TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE proj(kind TEXT NOT NULL, from_m TEXT NOT NULL, to_m TEXT NOT NULL, site TEXT NOT NULL, variable TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(kind, from_m, to_m, site, variable)) WITHOUT ROWID;
CREATE TABLE loc(id TEXT PRIMARY KEY, ok INTEGER NOT NULL, file_id TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL DEFAULT '',
	language TEXT NOT NULL DEFAULT '', range_json TEXT NOT NULL DEFAULT '') WITHOUT ROWID;
CREATE TABLE ident(method TEXT PRIMARY KEY, node_id TEXT NOT NULL, fact_json TEXT NOT NULL, alias_json TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE rels(rel_id TEXT NOT NULL, ev_id TEXT NOT NULL, from_id TEXT NOT NULL, kind TEXT NOT NULL, to_id TEXT NOT NULL, ev_json TEXT NOT NULL,
	PRIMARY KEY(rel_id, ev_id)) WITHOUT ROWID;
`

// openScratch creates the run's staging database. Durability is deliberately
// off: the file is private, single-writer and deleted with the run directory.
func openScratch(ctx context.Context, path string, budget int64) (*scratch, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)", "locking_mode(EXCLUSIVE)"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, internalErr("joern scratch connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, scratchSchema); err != nil {
		db.Close()
		return nil, internalErr("joern scratch schema: %v", err)
	}
	return &scratch{db: db, path: path, budget: budget, unknown: map[string]uint64{}}, nil
}

func (s *scratch) close() error { return s.db.Close() }

// staged accounts one staged row and checks the disk budget periodically.
func (s *scratch) staged() error {
	s.rows++
	if s.rows%diskCheckEvery != 0 {
		return nil
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return internalErr("joern scratch: %v", err)
	}
	if info.Size() > s.budget {
		return resourceLimit("the staged Joern graph exceeds the profile's %d-byte disk budget", s.budget).WithDetail("limit", "disk_budget_bytes")
	}
	return nil
}

// countUnknown records a label the profile does not know.
func (s *scratch) countUnknown(label string) {
	s.unknownN++
	if _, ok := s.unknown[label]; ok || len(s.unknown) < maxUnknownLabels {
		s.unknown[label]++
	}
}

// graphNode is one decoded node row of interest.
type graphNode struct {
	id, label, name, fullName, signature, filename, code, methodFullName string
	line, lineEnd, col                                                   sql.NullInt64
	isExternal                                                           bool
}

// putNode stages one node. Only METHOD and CALL nodes carry attributes the
// projection needs; other known labels are counted and skipped, unknown ones
// counted and reported. Duplicate ids keep the first row (the CSV export is
// read before the GraphML one).
func (s *scratch) putNode(ctx context.Context, n graphNode) error {
	switch n.label {
	case labelMethod, labelCall:
	case labelMetaData:
		return nil
	default:
		if !knownNodeLabels[n.label] {
			s.countUnknown(n.label)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO nodes(id, label, name, full_name, signature, filename, line, line_end, col, code, is_external, method_full_name)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, n.id, n.label, n.name, n.fullName, n.signature, n.filename, n.line, n.lineEnd, n.col, n.code, boolInt(n.isExternal), n.methodFullName)
	if err != nil {
		return internalErr("joern scratch node: %v", err)
	}
	return s.staged()
}

// putEdge stages one edge of a mapped label; other known labels are skipped
// and unknown ones counted. "DDG" is the PDG export's spelling of REACHING_DEF.
func (s *scratch) putEdge(ctx context.Context, src, dst, label, variable string) error {
	if label == "DDG" {
		label = edgeReachingDef
	}
	switch label {
	case edgeCall, edgeContains, edgeCDG, edgeReachingDef:
	default:
		if !knownEdgeLabels[label] {
			s.countUnknown(label)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO edges(src, dst, label, variable) VALUES(?, ?, ?, ?)`, src, dst, label, variable); err != nil {
		return internalErr("joern scratch edge: %v", err)
	}
	return s.staged()
}

func (s *scratch) putFile(ctx context.Context, fv model.FileVersion) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO files(path, file_id, content_hash, size, language) VALUES(?, ?, ?, ?, ?)`,
		fv.Path, string(fv.ID), fv.ContentHash, fv.Size, fv.Language); err != nil {
		return internalErr("joern scratch file: %v", err)
	}
	return nil
}

func (s *scratch) putMeta(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, key, value); err != nil {
		return internalErr("joern scratch meta: %v", err)
	}
	return nil
}

// project derives the method-level relation occurrences (Section 11.6):
//
//   - calls: a CALL edge from a call site to a METHOD, attributed to the
//     METHOD that CONTAINS the site;
//   - control dependence: a CDG edge between two call sites whose targets are
//     distinct methods; the dependent target control_depends_on the
//     controlling target, evidenced at the dependent site;
//   - data dependence: a REACHING_DEF walk of bounded depth from one call
//     site through non-call nodes (operator calls, identifiers) to another
//     call site with a distinct target; the source target data_flows_to the
//     destination target, evidenced at the destination site. It is
//     intraprocedural def-use reachability, never a proven source-to-sink
//     flow.
//
// Anchors map a call site to the method it invokes; a site with several
// targets takes the smallest id so the projection is deterministic. The
// dangling count records edges that name a node the export never defined.
func (s *scratch) project(ctx context.Context) error {
	steps := []string{
		// A CALL node carries no FILENAME of its own; it lies in the file of
		// the METHOD that CONTAINS it.
		`UPDATE nodes SET filename = COALESCE((SELECT m.filename FROM edges k JOIN nodes m ON m.id = k.src AND m.label = 'METHOD'
			WHERE k.label = 'CONTAINS' AND k.dst = nodes.id ORDER BY m.id LIMIT 1), '') WHERE label = 'CALL' AND filename = ''`,
		`INSERT INTO anchors(node, method) SELECT c.id, MIN(e.dst) FROM nodes c JOIN edges e ON e.src = c.id AND e.label = 'CALL'
			JOIN nodes t ON t.id = e.dst AND t.label = 'METHOD'
			WHERE c.label = 'CALL' AND c.method_full_name NOT LIKE '<operator>.%' GROUP BY c.id`,
		`INSERT OR IGNORE INTO proj(kind, from_m, to_m, site, variable)
			SELECT 'calls', k.src, e.dst, c.id, '' FROM edges e JOIN nodes c ON c.id = e.src AND c.label = 'CALL'
			JOIN nodes t ON t.id = e.dst AND t.label = 'METHOD'
			JOIN edges k ON k.label = 'CONTAINS' AND k.dst = c.id JOIN nodes m ON m.id = k.src AND m.label = 'METHOD'
			WHERE e.label = 'CALL'`,
		`INSERT OR IGNORE INTO proj(kind, from_m, to_m, site, variable)
			SELECT 'control_depends_on', a2.method, a1.method, e.dst, '' FROM edges e
			JOIN anchors a1 ON a1.node = e.src JOIN anchors a2 ON a2.node = e.dst
			WHERE e.label = 'CDG' AND a1.method <> a2.method`,
		`INSERT OR IGNORE INTO proj(kind, from_m, to_m, site, variable)
			WITH RECURSIVE walk(start, node, depth, variable) AS (
				SELECT a.node, e.dst, 1, e.variable FROM anchors a JOIN edges e ON e.src = a.node AND e.label = 'REACHING_DEF'
				UNION
				SELECT w.start, e.dst, w.depth + 1, e.variable FROM walk w JOIN edges e ON e.src = w.node AND e.label = 'REACHING_DEF'
				WHERE w.depth < ` + strconv.Itoa(maxDataFlowDepth) + ` AND NOT EXISTS (SELECT 1 FROM anchors x WHERE x.node = w.node))
			SELECT 'data_flows_to', a1.method, a2.method, w.node, w.variable FROM walk w
			JOIN anchors a1 ON a1.node = w.start JOIN anchors a2 ON a2.node = w.node WHERE a1.method <> a2.method
			LIMIT ` + strconv.Itoa(maxDerivedRows+1),
	}
	for _, q := range steps {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return internalErr("joern projection: %v", err)
		}
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM proj`).Scan(&n); err != nil {
		return internalErr("joern projection: %v", err)
	}
	if n > maxDerivedRows {
		return resourceLimit("the Joern export projects to more than %d relation occurrences", maxDerivedRows).WithDetail("limit", "max_derived_rows")
	}
	// Only the call structure has endpoints that must be staged nodes: a CALL
	// edge joins a CALL site to a METHOD and a CONTAINS edge starts at one.
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM edges e WHERE (e.label = 'CALL'
		AND (NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.src) OR NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.dst)))
		OR (e.label = 'CONTAINS' AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.src))`).Scan(&s.dangling)
	if err != nil {
		return internalErr("joern projection: %v", err)
	}
	return nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func internalErr(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}

func outputInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: fmt.Sprintf(format, args...)}
}
