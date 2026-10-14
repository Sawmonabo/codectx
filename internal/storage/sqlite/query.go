package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// PinnedReader reads exactly one generation. A writer-bearing process pins it
// together with its retention lease in one short write transaction, so the
// generation cannot be collected while the reader is open; a process that
// opened the store read-only holds it by snapshot instead, one deferred read
// transaction per call. Every read joins visible membership through
// generation_units; staging, failed and non-member units are never returned.
// Close releases the lease, if there is one.
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
	r := &PinnedReader{s: s, repo: repoRaw, lease: leaseID}
	// A read-only process cannot take the lease, so it resolves the generation
	// on the reader pool instead: no write transaction, nothing for the writer
	// of another process to be waited on for.
	//
	// Such a pin is a SNAPSHOT, not a lease, and that is enough. Every read it
	// serves runs in one deferred read transaction of its own (Store.read), so
	// within a call the log snapshot is fixed and a collection running
	// concurrently in another process cannot take rows out from under it.
	// Across calls the generation can indeed be collected, and the caller that
	// named it -- a continuation naming the generation its token pins -- is
	// told so by the typed refusal below rather than served a short answer.
	resolve := func(tx *sql.Tx) error {
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
			return generationMissing(id)
		}
		if err != nil {
			return wrap("generations", err)
		}
		if status != model.GenerationActive && status != model.GenerationSuperseded {
			return &model.Error{Code: model.CodeNoActiveGeneration,
				Message: "generation " + string(status) + " has never been published and cannot be pinned"}
		}
		if s.opts.ReadOnly {
			r.lease = ""
			r.gen = id
			r.binding = model.Binding{RepositoryID: repo, SnapshotID: model.SnapshotID(idHex(snapshot)),
				GenerationID: model.GenerationID(id), AnalysisKey: model.AnalysisKey(idHex(key))}
			return nil
		}
		// Through insertLease, never a statement of its own: a query lease is
		// a retention_leases row like any other, and a second write path here
		// is one model.Lease.Validate never sees.
		if err := insertLease(ctx, tx, model.Lease{
			ID:           leaseID,
			GenerationID: model.GenerationID(id),
			SnapshotID:   model.SnapshotID(idHex(snapshot)),
			OwnerKind:    model.LeaseQuery,
			ExpiresAt:    time.Now().Add(ttl),
		}, nil); err != nil {
			return err
		}
		r.gen = id
		r.binding = model.Binding{RepositoryID: repo, SnapshotID: model.SnapshotID(idHex(snapshot)),
			GenerationID: model.GenerationID(id), AnalysisKey: model.AnalysisKey(idHex(key))}
		return nil
	}
	if s.opts.ReadOnly {
		err = s.read(ctx, resolve)
	} else {
		err = s.write(ctx, resolve)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// generationStateDetail marks the refusal a pin gets when the generation it
// names has no row at all: it was collected. The refusal itself stays
// CTX_ARGUMENT_INVALID, which is what an operator who named the generation by
// hand asked for; a caller resuming from a token it was handed did not name it
// by hand, and IsGenerationCollected is how that caller recognises the case and
// answers in the cursor's own family instead.
const generationStateDetail = "generation_state"

func generationMissing(id int64) *model.Error {
	return invalid("generation %d does not exist for this repository", id).
		WithDetail(generationStateDetail, "collected")
}

// IsGenerationCollected reports whether err is a pin refused because the
// generation it named has been collected.
func IsGenerationCollected(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Details[generationStateDetail] == "collected"
}

// Binding is the generation every result from this reader is qualified by.
func (r *PinnedReader) Binding() model.Binding { return r.binding }

// Continuable reports whether a query served through this reader may record a
// retention LEASE. A process that opened the store read-only cannot: it still
// hands back a continuation -- the spool is a filesystem write and the token is
// signed, not stored -- but that continuation names no lease and the state it
// names is reclaimed by the expiry its header carries instead.
func (r *PinnedReader) Continuable() bool { return !r.s.opts.ReadOnly }

// RetainsLeases is Continuable for a caller that holds the lease store rather
// than a pinned reader -- the graph engine, whose continuations are minted far
// from the reader that pinned them. It satisfies pagination.LeaseRetainer, so
// pagination.Leases answers it for the whole process.
func (s *Store) RetainsLeases() bool { return !s.opts.ReadOnly }

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
// so the model.Node here carries no Range or NameRange: Bytes holds the
// node's interval and NameBytes its name token's, absent when none is stored.
// Path is the root-relative path of the node's file, stored beside the fact
// ("" for a node with no file), so a reader of nodes needs no file lookup.
type StoredNode struct {
	Node          model.Node
	Bytes         *model.ByteRange
	NameBytes     *model.ByteRange
	Path          string
	UnitID        model.UnitID
	SourceBinding model.SourceBinding
}

// StoredEvidence is one visible evidence row, with the byte interval separate
// for the same reason as StoredNode.
type StoredEvidence struct {
	Evidence model.Evidence
	Bytes    *model.ByteRange
}

// nodeRefByCanonical and relationRefByCanonical resolve a public 32-byte
// canonical id to its storage-internal surrogate inside the statement itself.
// A reader never holds a surrogate: it hands SQLite the canonical id it was
// given and lets the dictionary's UNIQUE(canonical) autoindex turn it into the
// rowid the fact tables are keyed by, so the outer predicate stays an indexed
// probe on node_ids.id / relation_ids.from_node_id and friends. The aliases are
// deliberately nx/rx so the subquery can never shadow an outer ni/ri.
const (
	nodeRefByCanonical     = `(SELECT nx.id FROM node_ids nx WHERE nx.canonical = ?)`
	relationRefByCanonical = `(SELECT rx.id FROM relation_ids rx WHERE rx.canonical = ?)`
)

// visible restricts a fact table alias to units selected by this generation.
func (r *PinnedReader) visible(alias string) string {
	return ` JOIN generation_units gu ON gu.unit_id = ` + alias + `.unit_id AND gu.generation_id = ?1 JOIN units u ON u.id = gu.unit_id `
}

const nodeColumns = `ni.canonical, ni.kind, nf.language, nf.name, nf.qualified_name, nf.signature, nf.file_id, ui.content_hash,
	nf.start_byte, nf.end_byte, nf.name_start_byte, nf.name_end_byte, nf.metadata_json, u.unit_key, u.source_binding, nf.path`

// nodeOrder is the Section 9.4 attribute precedence as far as storage can
// apply it: exact-source binding first, then provider, then the stable unit
// key. Match basis is not a stored column and precision rank is per evidence.
const nodeOrder = ` ORDER BY CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key`

func scanNode(rows interface{ Scan(...any) error }) (StoredNode, error) {
	var n StoredNode
	var nodeID, fileID, contentHash, unitKey []byte
	var start, end, nameStart, nameEnd sql.NullInt64
	var metadata string
	if err := rows.Scan(&nodeID, &n.Node.Kind, &n.Node.Language, &n.Node.Name, &n.Node.QualifiedName, &n.Node.Signature,
		&fileID, &contentHash, &start, &end, &nameStart, &nameEnd, &metadata, &unitKey, &n.SourceBinding, &n.Path); err != nil {
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
	if nameStart.Valid {
		n.NameBytes = &model.ByteRange{Start: uint64(nameStart.Int64), End: uint64(nameEnd.Int64)}
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
			WHERE ni.canonical = ?2`+nodeOrder+` LIMIT 1`, r.gen, raw)
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
// name prefix (indexed range scan), restricted to Kinds, or, when Kinds is
// empty, to every kind but model.NonSymbolKinds.
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
	query, args, err := r.nodesStatement(f, after, pageLimit(ctx, limit))
	if err != nil {
		return nil, err
	}
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

// nodesStatement is one page of Nodes after the node id `after` ("": from
// the first).
func (r *PinnedReader) nodesStatement(f NodeFilter, after model.NodeID, limit int) (string, []any, error) {
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
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return "", nil, err
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
				return "", nil, invalid("node filter kind %q is not a known node kind", k)
			}
			marks[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "ni.kind IN ("+strings.Join(marks, ",")+")")
	} else {
		skip := model.NonSymbolKinds()
		marks := make([]string, len(skip))
		for i, k := range skip {
			marks[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "ni.kind NOT IN ("+strings.Join(marks, ",")+")")
	}
	if afterRaw != nil {
		where = append(where, "ni.canonical > ?")
		args = append(args, afterRaw)
	}
	args = append(args, limit)
	// CROSS JOIN fixes the join order: the tier's facts first, through its
	// name or qualified-name index, then whether the generation selects each
	// fact's unit. Left to itself, with statistics, the planner starts from
	// the generation's units and reads every fact they hold.
	query := `SELECT ` + nodeColumns + ` FROM node_facts nf
		CROSS JOIN generation_units gu ON gu.unit_id = nf.unit_id AND gu.generation_id = ?1
		CROSS JOIN units u ON u.id = gu.unit_id
		CROSS JOIN node_ids ni ON ni.id = nf.node_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ni.canonical, CASE u.source_binding WHEN 'verified' THEN 0 ELSE 1 END, u.provider_id, u.unit_key LIMIT ?`
	return query, args, nil
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

// Containers pages the visible CONTAINER nodes of the pinned generation by
// keyset on node_id, restricted to kinds. It is the repository map's
// enumeration: Nodes above demands exactly one of name, qualified name or
// qualified-name prefix, and a map has no such seed -- it asks for the
// containers themselves.
//
// One row per node, chosen by nodePrecedence inside the grouped subquery
// exactly as NodesByID does, so the outer LIMIT counts NODES and not facts.
// That is load-bearing rather than tidiness: node_facts holds one row per
// (node, unit), and a container published by two units would otherwise make a
// full page come back short, which the caller reads as the end of the map --
// a repository silently missing its remaining packages.
//
// Visibility is the generation's: memberOf restricts every fact to a unit this
// generation selected, so a failed or unsealed unit's containers are invisible.
func (r *PinnedReader) Containers(ctx context.Context, kinds []model.NodeKind,
	after model.NodeID, limit int) ([]StoredNode, error) {
	if len(kinds) == 0 {
		return nil, invalid("container listing needs at least one node kind")
	}
	if len(kinds) > model.MaxFilterValues {
		return nil, invalid("container listing has %d kinds, limit %d", len(kinds), model.MaxFilterValues)
	}
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	b := newBinder(r.gen)
	marks := make([]string, len(kinds))
	for i, k := range kinds {
		if !k.Valid() {
			return nil, invalid("container listing kind %q is not a known node kind", k)
		}
		marks[i] = b.mark(string(k))
	}
	where := "ni.kind IN (" + strings.Join(marks, ",") + ")"
	if afterRaw != nil {
		where += " AND ni.canonical > " + b.mark(afterRaw)
	}
	limitMark := b.mark(pageLimit(ctx, limit))
	query := `SELECT ` + nodeBatchOuterColumns + ` FROM (
		SELECT ` + nodeBatchColumns + `, ` + nodePrecedence + ` FROM node_facts nf
		JOIN units u ON u.id = nf.unit_id
		JOIN node_ids ni ON ni.id = nf.node_id
		LEFT JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
		WHERE ` + where + memberOf("nf") + ` GROUP BY nf.node_id
	) ORDER BY node_id LIMIT ` + limitMark
	var out []StoredNode
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, b.args...)
		if err != nil {
			return wrap("node_facts", err)
		}
		defer rows.Close()
		for rows.Next() {
			n, err := scanNode(rows)
			if err != nil {
				return err
			}
			out = append(out, n)
		}
		return wrap("node_facts", rows.Err())
	})
	return out, err
}

// Relations pages visible edges touching node in direction, keyset on
// relation_id. Reverse traversal uses the indexed target column.
func (r *PinnedReader) Relations(ctx context.Context, node model.NodeID, direction model.Direction, kinds []model.RelationKind,
	after model.RelationID, limit int) ([]model.Relation, error) {
	limit = pageLimit(ctx, limit)
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
		where = append(where, "ri.from_node_id = "+nodeRefByCanonical)
		args = append(args, nodeRaw)
	case model.DirectionIncoming:
		where = append(where, "ri.to_node_id = "+nodeRefByCanonical)
		args = append(args, nodeRaw)
	default:
		where = append(where, "(ri.from_node_id = "+nodeRefByCanonical+" OR ri.to_node_id = "+nodeRefByCanonical+")")
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
		// The keyset carries the canonical RelationID, never relation_ids.id:
		// a surrogate is meaningful only inside one store and one rebuild, so a
		// cursor that carried it would decode to a different edge after a
		// reindex.
		where = append(where, "ri.canonical > ?")
		args = append(args, afterRaw)
	}
	args = append(args, limit)
	var out []model.Relation
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT ri.canonical, fn.canonical, ri.kind, tn.canonical FROM relation_facts rf`+r.visible("rf")+
			`JOIN relation_ids ri ON ri.id = rf.relation_id
			JOIN node_ids fn ON fn.id = ri.from_node_id JOIN node_ids tn ON tn.id = ri.to_node_id
			WHERE `+strings.Join(where, " AND ")+` ORDER BY ri.canonical LIMIT ?`, args...)
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
	limit = pageLimit(ctx, limit)
	if (node == "") == (relation == "") {
		return nil, invalid("evidence lookup needs exactly one of node_id or relation_id")
	}
	afterRaw, err := optionalEvidenceBlob("after", after)
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
		subject, args = "e.node_id = "+nodeRefByCanonical, append(args, raw)
	} else {
		raw, err := idBlob("relation_id", string(relation))
		if err != nil {
			return nil, err
		}
		subject, args = "e.relation_id = "+relationRefByCanonical, append(args, raw)
	}
	if afterRaw != nil {
		subject += " AND e.id > ?"
		args = append(args, afterRaw)
	}
	args = append(args, limit)
	var out []StoredEvidence
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT e.id, u.unit_key, u.provider_id, u.provider_version, u.origin_run_id, en.canonical, er.canonical, e.precision,
			e.file_id, ui.content_hash, e.start_byte, e.end_byte, nk.key, e.detail, e.dispatch, e.set_size FROM evidence e`+r.visible("e")+
			`JOIN native_keys nk ON nk.id = e.native_key_id
			LEFT JOIN node_ids en ON en.id = e.node_id LEFT JOIN relation_ids er ON er.id = e.relation_id
			LEFT JOIN unit_inputs ui ON ui.unit_id = e.unit_id AND ui.file_id = e.file_id WHERE `+subject+` ORDER BY e.id LIMIT ?`, args...)
		if err != nil {
			return wrap("evidence", err)
		}
		defer rows.Close()
		for rows.Next() {
			var se StoredEvidence
			var id, unitKey, run, nodeID, relID, fileID, contentHash []byte
			var start, end sql.NullInt64
			if err := rows.Scan(&id, &unitKey, &se.Evidence.ProviderID, &se.Evidence.ProviderVersion, &run, &nodeID, &relID, &se.Evidence.Precision,
				&fileID, &contentHash, &start, &end, &se.Evidence.NativeKey, &se.Evidence.Detail,
				&se.Evidence.Dispatch, &se.Evidence.SetSize); err != nil {
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
	limit = pageLimit(ctx, limit)
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

// ChangedFiles is Files with one predicate: only the snapshot rows whose status
// is one of statuses are returned, keyset-paged by file id exactly as Files is.
// It exists so a caller that wants the captured working-tree changes of a
// 250,000-file workspace does not have to read every file row to find them; the
// status column is part of the snapshot_files row the same query already scans,
// so this is a filter, not a second file listing.
//
// An empty statuses is a caller asking for nothing and is answered with nothing,
// never with every row: a status filter that silently becomes "no filter" would
// turn the bounded read this exists to provide back into the full scan.
func (r *PinnedReader) ChangedFiles(ctx context.Context, after model.FileID,
	statuses []model.FileStatus, limit int) ([]model.FileVersion, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	limit = pageLimit(ctx, limit)
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	if afterRaw == nil {
		afterRaw = []byte{}
	}
	args := []any{r.gen, afterRaw, limit}
	placeholders := make([]string, 0, len(statuses))
	for _, st := range statuses {
		args = append(args, string(st))
		placeholders = append(placeholders, "?"+strconv.Itoa(len(args)))
	}
	query := fileQuery + ` AND sf.file_id > ?2 AND sf.status IN (` + strings.Join(placeholders, ",") +
		`) ORDER BY sf.file_id LIMIT ?3`
	var out []model.FileVersion
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
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

// Capabilities returns the generation's stored capability report -- every row
// of it. The bare `LIMIT 256` this once carried silently dropped rows off the
// tail of a large generation's report, and QueryMeta.Completeness is derived
// from what this returns on every answer, so the drop made every answer
// over-claim. The scan streams in one read transaction under the primary key's
// order, but the ANSWER is the whole report and so is the peak: every row is
// decoded into the returned slice and there is no page API here. That is
// deliberate rather than an omission -- the report is written pre-folded. The
// index publishes it through capabilityReport.finish, which collapses the rows
// to one per (provider, capability, scope) in foldCollapsed and bounds what
// survives in boundStates (internal/index/status.go), so a generation's stored
// report is bounded by the provider/capability/scope product and not by the
// repository. A page API would page a set whose size the writer already fixed.
func (r *PinnedReader) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	var out []model.CapabilityState
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT provider_id, capability, scope_key, state, diagnostic_code, remediation, details_json FROM generation_capabilities
			WHERE generation_id = ? ORDER BY provider_id, capability, scope_key`, r.gen)
		if err != nil {
			return wrap("generation_capabilities", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c model.CapabilityState
			var details string
			if err := rows.Scan(&c.ProviderID, &c.Capability, &c.Scope, &c.State, &c.DiagnosticCode,
				&c.Remediation, &details); err != nil {
				return wrap("generation_capabilities", err)
			}
			if details != "" && details != "{}" {
				if err := json.Unmarshal([]byte(details), &c.Details); err != nil {
					return corrupt("generation %d capability %q/%q has unreadable details", r.gen, c.ProviderID, c.Capability)
				}
			}
			// The write path bounds every capability, but the read path must
			// not trust the row: a row written by a foreign or older binary
			// would otherwise hand a caller an unbounded detail map.
			if err := c.Validate(); err != nil {
				return corrupt("generation %d capability %q/%q is not a valid capability state: %v", r.gen, c.ProviderID, c.Capability, err)
			}
			out = append(out, c)
		}
		return wrap("generation_capabilities", rows.Err())
	})
	return out, err
}
