package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// PinnedReader reads exactly one generation. It was pinned together with its
// retention lease in one short write transaction, so the generation cannot be
// collected while the reader is open. Every read joins visible membership
// through generation_units; staging, failed and non-member units are never
// returned. Close releases the lease.
type PinnedReader struct {
	s       *Store
	binding model.Binding
	repo    []byte
	gen     int64
	lease   string
}

// PinGeneration resolves gen (zero selects the active generation) and acquires
// a query lease for ttl in the same transaction (Section 12.3). A staging or
// failed generation cannot be pinned.
func (s *Store) PinGeneration(ctx context.Context, repo model.RepositoryID, gen model.GenerationID, ttl time.Duration) (*PinnedReader, error) {
	repoRaw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return nil, err
	}
	if gen < 0 {
		return nil, invalid("generation_id must not be negative")
	}
	if ttl <= 0 {
		return nil, invalid("lease ttl must be positive")
	}
	leaseID, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	leaseRaw, _ := model.DecodeID(leaseID)
	r := &PinnedReader{s: s, repo: repoRaw, lease: leaseID}
	err = s.write(ctx, func(tx *sql.Tx) error {
		id := int64(gen)
		if id == 0 {
			if err := activeGeneration(ctx, tx, repoRaw, &id); err != nil {
				return err
			}
		}
		var snapshot, key []byte
		var status model.GenerationStatus
		err := tx.QueryRowContext(ctx, `SELECT snapshot_id, analysis_key, status FROM generations WHERE id = ? AND repository_id = ?`, id, repoRaw).
			Scan(&snapshot, &key, &status)
		if isNoRows(err) {
			return invalid("generation %d does not exist for this repository", id)
		}
		if err != nil {
			return wrap("generations", err)
		}
		if status != model.GenerationActive && status != model.GenerationSuperseded {
			return &model.Error{Code: model.CodeNoActiveGeneration,
				Message: "generation " + string(status) + " has never been published and cannot be pinned"}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO retention_leases(id, generation_id, snapshot_id, owner_kind, expires_at) VALUES(?, ?, ?, ?, ?)`,
			leaseRaw, id, snapshot, string(model.LeaseQuery), formatTime(time.Now().Add(ttl))); err != nil {
			return wrap("retention_leases", err)
		}
		r.gen = id
		r.binding = model.Binding{RepositoryID: repo, SnapshotID: model.SnapshotID(idHex(snapshot)),
			GenerationID: model.GenerationID(id), AnalysisKey: model.AnalysisKey(idHex(key))}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Binding is the generation every result from this reader is qualified by.
func (r *PinnedReader) Binding() model.Binding { return r.binding }

// LeaseID is the retention lease this reader holds; cursors carry it.
func (r *PinnedReader) LeaseID() string { return r.lease }

// Renew extends the reader's lease for a further ttl.
func (r *PinnedReader) Renew(ctx context.Context, ttl time.Duration) error {
	return r.s.RenewLease(ctx, r.lease, time.Now().Add(ttl))
}

// Close releases the lease. It is safe to call more than once.
func (r *PinnedReader) Close() error {
	if r.lease == "" {
		return nil
	}
	err := r.s.ReleaseLease(context.Background(), r.lease)
	r.lease = ""
	return err
}

// StoredNode is one visible node fact. Ranges are stored as bytes only; the
// consumer derives line/column positions from the source's line checkpoints,
// so the model.Node here carries no Range and Bytes holds the interval.
type StoredNode struct {
	Node          model.Node
	Bytes         *model.ByteRange
	UnitID        model.UnitID
	SourceBinding model.SourceBinding
}

// StoredEvidence is one visible evidence row, with the byte interval separate
// for the same reason as StoredNode.
type StoredEvidence struct {
	Evidence model.Evidence
	Bytes    *model.ByteRange
}

// visible restricts a fact table alias to units selected by this generation.
func (r *PinnedReader) visible(alias string) string {
	return ` JOIN generation_units gu ON gu.unit_id = ` + alias + `.unit_id AND gu.generation_id = ?1 JOIN units u ON u.id = gu.unit_id `
}

const nodeColumns = `nf.node_id, ni.kind, nf.language, nf.name, nf.qualified_name, nf.signature, nf.file_id, ui.content_hash,
	nf.start_byte, nf.end_byte, nf.metadata_json, u.unit_key, u.source_binding`

// nodeOrder is the Section 9.4 attribute precedence as far as storage can
// apply it: exact-source binding first, then provider, then the stable unit
// key. Match basis is not a stored column and precision rank is per evidence.
const nodeOrder = ` ORDER BY CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key`

func scanNode(rows interface{ Scan(...any) error }) (StoredNode, error) {
	var n StoredNode
	var nodeID, fileID, contentHash, unitKey []byte
	var start, end sql.NullInt64
	var metadata string
	if err := rows.Scan(&nodeID, &n.Node.Kind, &n.Node.Language, &n.Node.Name, &n.Node.QualifiedName, &n.Node.Signature,
		&fileID, &contentHash, &start, &end, &metadata, &unitKey, &n.SourceBinding); err != nil {
		if isNoRows(err) {
			return n, err
		}
		return n, wrap("node_facts", err)
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
	if metadata != "{}" {
		n.Node.Metadata = []byte(metadata)
	}
	n.UnitID = model.UnitID(idHex(unitKey))
	return n, nil
}

// Node returns the preferred visible fact for id, or CTX_ARGUMENT_INVALID when
// the node is not visible in this generation: a public NodeID is valid only
// within its binding (Section 9.1).
func (r *PinnedReader) Node(ctx context.Context, id model.NodeID) (StoredNode, error) {
	raw, err := idBlob("node_id", string(id))
	if err != nil {
		return StoredNode{}, err
	}
	var n StoredNode
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM node_facts nf`+r.visible("nf")+
			`JOIN node_ids ni ON ni.id = nf.node_id LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
			WHERE nf.node_id = ?2`+nodeOrder+` LIMIT 1`, r.gen, raw)
		var err error
		n, err = scanNode(row)
		if isNoRows(err) {
			return invalid("node %s is not visible in generation %d", id, r.gen)
		}
		return err
	})
	return n, err
}

// NodeFilter selects nodes by exact name, exact qualified name or qualified
// name prefix (indexed range scan), optionally restricted to kinds.
type NodeFilter struct {
	Name            string
	QualifiedName   string
	QualifiedPrefix string
	Kinds           []model.NodeKind
}

// Nodes pages visible node facts matching f by keyset on node_id. One row per
// node: when several units publish the same node the precedence order picks
// one. limit is capped at model.MaxPageItems.
func (r *PinnedReader) Nodes(ctx context.Context, f NodeFilter, after model.NodeID, limit int) ([]StoredNode, error) {
	limit = pageLimit(limit)
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
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	var where []string
	args := []any{r.gen}
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
	if len(f.Kinds) > 0 {
		marks := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			if !k.Valid() {
				return nil, invalid("node filter kind %q is not a known node kind", k)
			}
			marks[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "ni.kind IN ("+strings.Join(marks, ",")+")")
	}
	if afterRaw != nil {
		where = append(where, "nf.node_id > ?")
		args = append(args, afterRaw)
	}
	args = append(args, limit)
	query := `SELECT ` + nodeColumns + ` FROM node_facts nf` + r.visible("nf") +
		`JOIN node_ids ni ON ni.id = nf.node_id LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY nf.node_id, CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key LIMIT ?`
	var out []StoredNode
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

// prefixUpperBound is the smallest string greater than every string with the
// given prefix, for an indexed range scan without a leading wildcard.
func prefixUpperBound(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return prefix + "\xff"
}

func pageLimit(limit int) int {
	if limit <= 0 || limit > model.MaxPageItems {
		return model.MaxPageItems
	}
	return limit
}

// Relations pages visible edges touching node in direction, keyset on
// relation_id. Reverse traversal uses the indexed target column.
func (r *PinnedReader) Relations(ctx context.Context, node model.NodeID, direction model.Direction, kinds []model.RelationKind,
	after model.RelationID, limit int) ([]model.Relation, error) {
	limit = pageLimit(limit)
	nodeRaw, err := idBlob("node_id", string(node))
	if err != nil {
		return nil, err
	}
	if !direction.Valid() {
		return nil, invalid("direction %q is not a known direction", direction)
	}
	if len(kinds) > model.MaxFilterValues {
		return nil, invalid("relation filter has %d kinds, limit %d", len(kinds), model.MaxFilterValues)
	}
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	args := []any{r.gen}
	var where []string
	switch direction {
	case model.DirectionOutgoing:
		where = append(where, "ri.from_node_id = ?")
		args = append(args, nodeRaw)
	case model.DirectionIncoming:
		where = append(where, "ri.to_node_id = ?")
		args = append(args, nodeRaw)
	default:
		where = append(where, "(ri.from_node_id = ? OR ri.to_node_id = ?)")
		args = append(args, nodeRaw, nodeRaw)
	}
	if len(kinds) > 0 {
		marks := make([]string, len(kinds))
		for i, k := range kinds {
			if !k.Valid() {
				return nil, invalid("relation kind %q is not a known relation kind", k)
			}
			marks[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "ri.kind IN ("+strings.Join(marks, ",")+")")
	}
	if afterRaw != nil {
		where = append(where, "ri.id > ?")
		args = append(args, afterRaw)
	}
	args = append(args, limit)
	var out []model.Relation
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT ri.id, ri.from_node_id, ri.kind, ri.to_node_id FROM relation_facts rf`+r.visible("rf")+
			`JOIN relation_ids ri ON ri.id = rf.relation_id WHERE `+strings.Join(where, " AND ")+` ORDER BY ri.id LIMIT ?`, args...)
		if err != nil {
			return wrap("relation_facts", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, from, to []byte
			var rel model.Relation
			if err := rows.Scan(&id, &from, &rel.Kind, &to); err != nil {
				return wrap("relation_facts", err)
			}
			rel.ID, rel.From, rel.To = model.RelationID(idHex(id)), model.NodeID(idHex(from)), model.NodeID(idHex(to))
			out = append(out, rel)
		}
		return wrap("relation_facts", rows.Err())
	})
	return out, err
}

// Evidence pages the visible evidence for one node or one relation, keyset on
// evidence id. Exactly one subject must be given.
func (r *PinnedReader) Evidence(ctx context.Context, node model.NodeID, relation model.RelationID, after model.EvidenceID, limit int) ([]StoredEvidence, error) {
	limit = pageLimit(limit)
	if (node == "") == (relation == "") {
		return nil, invalid("evidence lookup needs exactly one of node_id or relation_id")
	}
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	args := []any{r.gen}
	var subject string
	if node != "" {
		raw, err := idBlob("node_id", string(node))
		if err != nil {
			return nil, err
		}
		subject, args = "e.node_id = ?", append(args, raw)
	} else {
		raw, err := idBlob("relation_id", string(relation))
		if err != nil {
			return nil, err
		}
		subject, args = "e.relation_id = ?", append(args, raw)
	}
	if afterRaw != nil {
		subject += " AND e.id > ?"
		args = append(args, afterRaw)
	}
	args = append(args, limit)
	var out []StoredEvidence
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT e.id, u.unit_key, u.provider_id, u.provider_version, u.origin_run_id, e.node_id, e.relation_id, e.precision,
			e.file_id, ui.content_hash, e.start_byte, e.end_byte, e.native_key, e.detail FROM evidence e`+r.visible("e")+
			`LEFT JOIN unit_inputs ui ON ui.unit_id = e.unit_id AND ui.file_id = e.file_id WHERE `+subject+` ORDER BY e.id LIMIT ?`, args...)
		if err != nil {
			return wrap("evidence", err)
		}
		defer rows.Close()
		for rows.Next() {
			var se StoredEvidence
			var id, unitKey, run, nodeID, relID, fileID, contentHash []byte
			var start, end sql.NullInt64
			if err := rows.Scan(&id, &unitKey, &se.Evidence.ProviderID, &se.Evidence.ProviderVersion, &run, &nodeID, &relID, &se.Evidence.Precision,
				&fileID, &contentHash, &start, &end, &se.Evidence.NativeKey, &se.Evidence.Detail); err != nil {
				return wrap("evidence", err)
			}
			se.Evidence.ID, se.Evidence.UnitID, se.Evidence.OriginRunID = model.EvidenceID(idHex(id)), model.UnitID(idHex(unitKey)), model.ProviderRunID(idHex(run))
			if nodeID != nil {
				se.Evidence.NodeID = model.NodeID(idHex(nodeID))
			}
			if relID != nil {
				se.Evidence.RelationID = model.RelationID(idHex(relID))
			}
			if fileID != nil {
				se.Evidence.FileID, se.Evidence.ContentHash = model.FileID(idHex(fileID)), idHex(contentHash)
			}
			if start.Valid {
				se.Bytes = &model.ByteRange{Start: uint64(start.Int64), End: uint64(end.Int64)}
			}
			out = append(out, se)
		}
		return wrap("evidence", rows.Err())
	})
	return out, err
}

// File returns the pinned snapshot's manifest row for id.
func (r *PinnedReader) File(ctx context.Context, id model.FileID) (model.FileVersion, error) {
	raw, err := idBlob("file_id", string(id))
	if err != nil {
		return model.FileVersion{}, err
	}
	var fv model.FileVersion
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fileQuery+` AND sf.file_id = ?2 LIMIT 1`, r.gen, raw)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		defer rows.Close()
		if !rows.Next() {
			return invalid("file %s is not in the pinned snapshot", id)
		}
		fv, err = scanFile(rows)
		return err
	})
	return fv, err
}

// Files pages the pinned manifest by keyset on file_id. This is the one
// paginated source of truth for public file listings (Section 10.1).
func (r *PinnedReader) Files(ctx context.Context, after model.FileID, limit int) ([]model.FileVersion, error) {
	limit = pageLimit(limit)
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	if afterRaw == nil {
		afterRaw = []byte{}
	}
	var out []model.FileVersion
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fileQuery+` AND sf.file_id > ?2 ORDER BY sf.file_id LIMIT ?3`, r.gen, afterRaw, limit)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		defer rows.Close()
		for rows.Next() {
			fv, err := scanFile(rows)
			if err != nil {
				return err
			}
			out = append(out, fv)
		}
		return wrap("snapshot_files", rows.Err())
	})
	return out, err
}

const fileQuery = `SELECT sf.file_id, f.path, sf.status, sf.size_bytes, sf.content_hash, sf.git_object_id, sf.language, sf.executable
	FROM generations g JOIN snapshot_files sf ON sf.snapshot_id = g.snapshot_id JOIN files f ON f.id = sf.file_id WHERE g.id = ?1`

func scanFile(rows *sql.Rows) (model.FileVersion, error) {
	var fv model.FileVersion
	var id, hash []byte
	var exec int64
	if err := rows.Scan(&id, &fv.Path, &fv.Status, &fv.Size, &hash, &fv.GitObjectID, &fv.Language, &exec); err != nil {
		return fv, wrap("snapshot_files", err)
	}
	fv.ID, fv.Executable = model.FileID(idHex(id)), exec == 1
	if hash != nil {
		fv.ContentHash = idHex(hash)
	}
	return fv, nil
}

// Capabilities returns the generation's stored capability report.
func (r *PinnedReader) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	var out []model.CapabilityState
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT provider_id, capability, scope_key, state, diagnostic_code, details_json FROM generation_capabilities
			WHERE generation_id = ? ORDER BY provider_id, capability, scope_key LIMIT ?`, r.gen, model.MaxCapabilityStates)
		if err != nil {
			return wrap("generation_capabilities", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c model.CapabilityState
			var details string
			if err := rows.Scan(&c.ProviderID, &c.Capability, &c.Scope, &c.State, &c.DiagnosticCode, &details); err != nil {
				return wrap("generation_capabilities", err)
			}
			if details != "" && details != "{}" {
				if err := json.Unmarshal([]byte(details), &c.Details); err != nil {
					return corrupt("generation %d capability %q/%q has unreadable details", r.gen, c.ProviderID, c.Capability)
				}
			}
			out = append(out, c)
		}
		return wrap("generation_capabilities", rows.Err())
	})
	return out, err
}

// Lexical primitives for Task 13's generation-scoped BM25 (Section 12.4). The
// store exposes membership-restricted rowids and aggregates; ranking is not
// implemented here.

// SearchStats returns the visible document count and total token length, the
// corpus statistics a generation-local BM25 needs.
func (r *PinnedReader) SearchStats(ctx context.Context) (documents, tokens int64, err error) {
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		return wrap("search_units", tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(su.token_count), 0) FROM search_units su`+r.visible("su"), r.gen).Scan(&documents, &tokens))
	})
	return documents, tokens, err
}

// SearchUnitRowIDs pages the visible search document rowids by keyset.
func (r *PinnedReader) SearchUnitRowIDs(ctx context.Context, after int64, limit int) ([]int64, error) {
	limit = pageLimit(limit)
	var out []int64
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT su.rowid FROM search_units su`+r.visible("su")+` WHERE su.rowid > ?2 ORDER BY su.rowid LIMIT ?3`, r.gen, after, limit)
		if err != nil {
			return wrap("search_units", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return wrap("search_units", err)
			}
			out = append(out, id)
		}
		return wrap("search_units", rows.Err())
	})
	return out, err
}

// Match returns visible rowids whose indexed columns match the FTS5 expression,
// keyset by rowid. The caller owns query encoding: pass an expression built by
// the literal-text encoder, never raw user text (Section 14.2).
func (r *PinnedReader) Match(ctx context.Context, expression string, after int64, limit int) ([]int64, error) {
	limit = pageLimit(limit)
	if strings.TrimSpace(expression) == "" || len(expression) > model.MaxQueryTextBytes*2 {
		return nil, invalid("fts expression is empty or exceeds its bound")
	}
	var out []int64
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT f.rowid FROM search_fts f WHERE f.search_fts MATCH ?2 AND f.rowid > ?3
			AND EXISTS (SELECT 1 FROM search_units su JOIN generation_units gu ON gu.unit_id = su.unit_id WHERE su.rowid = f.rowid AND gu.generation_id = ?1)
			ORDER BY f.rowid LIMIT ?4`, r.gen, expression, after, limit)
		if err != nil {
			return wrap("search_fts", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return wrap("search_fts", err)
			}
			out = append(out, id)
		}
		return wrap("search_fts", rows.Err())
	})
	return out, err
}

// SearchUnit reads one visible document by rowid, including its bounded body.
func (r *PinnedReader) SearchUnit(ctx context.Context, rowid int64) (model.SearchUnit, error) {
	var d model.SearchUnit
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		var key, node, file []byte
		var start, end int64
		err := tx.QueryRowContext(ctx, `SELECT su.search_key, su.node_id, su.file_id, su.path, su.kind, su.name, su.qualified_name, su.signature,
			su.start_byte, su.end_byte, su.body, su.token_count FROM search_units su`+r.visible("su")+` WHERE su.rowid = ?2`, r.gen, rowid).
			Scan(&key, &node, &file, &d.Path, &d.Kind, &d.Name, &d.QualifiedName, &d.Signature, &start, &end, &d.Body, &d.TokenCount)
		if isNoRows(err) {
			return invalid("search document %d is not visible in generation %d", rowid, r.gen)
		}
		if err != nil {
			return wrap("search_units", err)
		}
		d.ID, d.FileID = idHex(key), model.FileID(idHex(file))
		if node != nil {
			d.NodeID = model.NodeID(idHex(node))
		}
		d.Bytes = model.ByteRange{Start: uint64(start), End: uint64(end)}
		return nil
	})
	return d, err
}

// maxQueryTokens is the ceiling on tokens one query can yield. Query text is
// bounded to MaxQueryTextBytes and every token occupies at least one byte, so
// this LIMIT can never truncate; Section 20.1's resources.max_query_terms (32)
// is enforced by the search service on top.
const maxQueryTokens = model.MaxQueryTextBytes

// tokenizerDDL declares a throwaway FTS5 table with exactly search_fts's
// indexed columns and tokenizer, plus the vocabulary view that exposes each
// token instance. It lives on the private in-memory connection and holds no
// data between calls.
var tokenizerDDL = []string{
	`CREATE VIRTUAL TABLE IF NOT EXISTS tok USING fts5(name, qualified_name, signature, path, body, tokenize='unicode61', detail='full')`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS tok_vocab USING fts5vocab(tok, 'instance')`,
}

// withTokenizer runs fn in a transaction on the tokenizer connection that is
// always rolled back, so inserted documents never outlive the call.
func (s *Store) withTokenizer(ctx context.Context, fn func(tx *sql.Tx) error) error {
	conn, err := s.tokenizer.Conn(ctx)
	if err != nil {
		return wrap("tokenizer", err)
	}
	defer conn.Close()
	for _, ddl := range tokenizerDDL {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return wrap("tokenizer", err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return wrap("tokenizer", err)
	}
	defer tx.Rollback()
	return fn(tx)
}

// Tokenize splits text with the exact unicode61 tokenizer search_fts uses.
// Tokens are returned in document order, duplicates included, so the caller
// can count term and phrase occurrences without reimplementing Unicode
// tokenization.
func (s *Store) Tokenize(ctx context.Context, text string) ([]string, error) {
	if len(text) > model.MaxQueryTextBytes {
		return nil, invalid("query text exceeds %d bytes", model.MaxQueryTextBytes)
	}
	var terms []string
	err := s.withTokenizer(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tok(rowid, body) VALUES(1, ?)`, text); err != nil {
			return wrap("tokenizer", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT term FROM tok_vocab WHERE doc = 1 ORDER BY offset LIMIT ?`, maxQueryTokens)
		if err != nil {
			return wrap("tokenizer", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return wrap("tokenizer", err)
			}
			terms = append(terms, t)
		}
		return wrap("tokenizer", rows.Err())
	})
	return terms, err
}

// countTokens returns, for each document, the number of token instances the
// index tokenizer produces across every indexed column. It is the one source
// of search_units.token_count.
func (s *Store) countTokens(ctx context.Context, docs []model.SearchUnit) ([]int64, error) {
	counts := make([]int64, len(docs))
	if len(docs) == 0 {
		return counts, nil
	}
	err := s.withTokenizer(ctx, func(tx *sql.Tx) error {
		ins, err := tx.PrepareContext(ctx, `INSERT INTO tok(rowid, name, qualified_name, signature, path, body) VALUES(?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return wrap("tokenizer", err)
		}
		defer ins.Close()
		for i, d := range docs {
			if _, err := ins.ExecContext(ctx, int64(i+1), d.Name, d.QualifiedName, d.Signature, d.Path, d.Body); err != nil {
				return wrap("tokenizer", err)
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT doc, count(*) FROM tok_vocab GROUP BY doc`)
		if err != nil {
			return wrap("tokenizer", err)
		}
		defer rows.Close()
		for rows.Next() {
			var doc, n int64
			if err := rows.Scan(&doc, &n); err != nil {
				return wrap("tokenizer", err)
			}
			if doc < 1 || doc > int64(len(docs)) {
				return corrupt("tokenizer reported document %d outside the batch", doc)
			}
			counts[doc-1] = n
		}
		return wrap("tokenizer", rows.Err())
	})
	return counts, err
}
