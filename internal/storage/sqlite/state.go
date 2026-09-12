package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Actor/workflow persistence (Sections 15–17). Manifests, capsules and
// observations are immutable; the session's current-manifest pointer and
// versioned workflow state are the explicitly mutable rows, and every
// transition is a compare-and-swap on state_version in one transaction.

// PutManifest stores an immutable compiled manifest: header, ordered entries,
// slices and exclusions in one transaction. Every entry's node must be visible
// in the manifest's generation and every file must be in its snapshot.
// Storing the same manifest ID again with the same canonical hash is a no-op;
// a different hash under the same ID is CTX_VERSION_CONFLICT.
func (s *Store) PutManifest(ctx context.Context, m model.ContextManifest, requestJSON []byte,
	entries []model.ContextEntry, slices []model.ContextSlice, excluded []model.ExcludedContextEntry) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Binding.AnalysisKey == "" {
		return invalid("manifest binding must carry the generation's analysis key")
	}
	if err := requireJSONObject("manifest.request_json", requestJSON); err != nil {
		return invalid("manifest request payload must be a JSON object")
	}
	if int64(len(requestJSON)) > s.opts.MaxJSONBytes {
		return &model.Error{Code: model.CodeResourceLimit, Message: "manifest request payload exceeds the stored JSON bound"}
	}
	if len(entries) != m.EntryCount || len(slices) != m.SliceCount {
		return invalid("manifest header counts (%d entries, %d slices) disagree with the rows supplied (%d, %d)", m.EntryCount, m.SliceCount, len(entries), len(slices))
	}
	for i, e := range entries {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Ordinal != i {
			return invalid("manifest entries must be ordered 0..n-1; entry %d has ordinal %d", i, e.Ordinal)
		}
	}
	for i, sl := range slices {
		if err := sl.Validate(); err != nil {
			return err
		}
		if sl.Index != i {
			return invalid("manifest slices must be ordered 0..n-1; slice %d has index %d", i, sl.Index)
		}
	}
	for i, x := range excluded {
		if err := x.Validate(); err != nil {
			return err
		}
		if x.Ordinal != i {
			return invalid("excluded entries must be ordered 0..n-1; entry %d has ordinal %d", i, x.Ordinal)
		}
	}
	idRaw, _ := model.DecodeID(string(m.ID))
	snapRaw, _ := model.DecodeID(string(m.Binding.SnapshotID))
	completeness, err := json.Marshal(m.Completeness)
	if err != nil {
		return internal("completeness: " + err.Error())
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRowContext(ctx, `SELECT canonical_hash FROM context_manifests WHERE id = ?`, idRaw).Scan(&stored)
		if err == nil {
			if stored == m.CanonicalHash {
				return nil
			}
			return conflict("manifest %s already exists with a different canonical hash", m.ID)
		}
		if !isNoRows(err) {
			return wrap("context_manifests", err)
		}
		g, err := s.generationRow(ctx, tx, m.Binding.GenerationID, "")
		if err != nil {
			return err
		}
		if g.status != model.GenerationActive && g.status != model.GenerationSuperseded {
			return conflict("manifest binds generation %d which is %s, not published", m.Binding.GenerationID, g.status)
		}
		if string(g.snapshot) != string(snapRaw) || idHex(g.repo) != string(m.Binding.RepositoryID) {
			return invalid("manifest binding does not match generation %d's repository and snapshot", m.Binding.GenerationID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO context_manifests(id, generation_id, snapshot_id, phase, request_hash, policy_version, request_json,
			completeness_json, canonical_hash, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			idRaw, g.id, snapRaw, string(m.Phase), m.RequestHash, m.PolicyVersion, string(requestJSON), string(completeness), m.CanonicalHash, formatTime(m.CreatedAt)); err != nil {
			return wrap("context_manifests", err)
		}
		nodeVisible, err := tx.PrepareContext(ctx, `SELECT 1 FROM node_facts nf JOIN generation_units gu ON gu.unit_id = nf.unit_id WHERE gu.generation_id = ? AND nf.node_id = ? LIMIT 1`)
		if err != nil {
			return wrap("node_facts", err)
		}
		defer nodeVisible.Close()
		fileVisible, err := tx.PrepareContext(ctx, `SELECT 1 FROM snapshot_files WHERE snapshot_id = ? AND file_id = ? AND status <> 'deleted'`)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		defer fileVisible.Close()
		entry, err := tx.PrepareContext(ctx, `INSERT INTO context_entries(manifest_id, ordinal, node_id, file_id, requirement, score_micros, estimated_bytes,
			estimated_tokens, reasons_json, evidence_paths_json) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return wrap("context_entries", err)
		}
		defer entry.Close()
		for _, e := range entries {
			nodeRaw, _ := optionalBlob("node_id", string(e.NodeID))
			fileRaw, _ := optionalBlob("file_id", string(e.FileID))
			var one int
			if nodeRaw != nil {
				if err := nodeVisible.QueryRowContext(ctx, g.id, nodeRaw).Scan(&one); err != nil {
					if isNoRows(err) {
						return &model.Error{Code: model.CodeScopeIncomplete, Message: fmt.Sprintf("manifest entry %d names a node that is not visible in generation %d", e.Ordinal, g.id)}
					}
					return wrap("node_facts", err)
				}
			}
			if fileRaw != nil {
				if err := fileVisible.QueryRowContext(ctx, snapRaw, fileRaw).Scan(&one); err != nil {
					if isNoRows(err) {
						return &model.Error{Code: model.CodeScopeIncomplete, Message: fmt.Sprintf("manifest entry %d names a file that is not in the pinned snapshot", e.Ordinal)}
					}
					return wrap("snapshot_files", err)
				}
			}
			reasons, paths := []byte("[]"), []byte("[]")
			if e.Reasons != nil {
				reasons, _ = json.Marshal(e.Reasons)
			}
			if e.EvidencePaths != nil {
				paths, _ = json.Marshal(e.EvidencePaths)
			}
			if _, err := entry.ExecContext(ctx, idRaw, e.Ordinal, nodeRaw, fileRaw, string(e.Requirement), e.ScoreMicros, e.EstimatedBytes, e.EstimatedTokens, string(reasons), string(paths)); err != nil {
				return wrap("context_entries", err)
			}
		}
		slice, err := tx.PrepareContext(ctx, `INSERT INTO context_slices(manifest_id, slice_index, estimated_bytes, estimated_tokens) VALUES(?, ?, ?, ?)`)
		if err != nil {
			return wrap("context_slices", err)
		}
		defer slice.Close()
		sliceEntry, err := tx.PrepareContext(ctx, `INSERT INTO context_slice_entries(manifest_id, slice_index, position, entry_ordinal) VALUES(?, ?, ?, ?)`)
		if err != nil {
			return wrap("context_slice_entries", err)
		}
		defer sliceEntry.Close()
		for _, sl := range slices {
			if _, err := slice.ExecContext(ctx, idRaw, sl.Index, sl.EstimatedBytes, sl.EstimatedTokens); err != nil {
				return wrap("context_slices", err)
			}
			for pos, ord := range sl.EntryOrdinals {
				if _, err := sliceEntry.ExecContext(ctx, idRaw, sl.Index, pos, ord); err != nil {
					return wrap("context_slice_entries", err)
				}
			}
		}
		excl, err := tx.PrepareContext(ctx, `INSERT INTO excluded_context_entries(manifest_id, ordinal, reference_json, reason) VALUES(?, ?, ?, ?)`)
		if err != nil {
			return wrap("excluded_context_entries", err)
		}
		defer excl.Close()
		for _, x := range excluded {
			ref, _ := json.Marshal(x.Reference)
			if _, err := excl.ExecContext(ctx, idRaw, x.Ordinal, string(ref), x.Reason); err != nil {
				return wrap("excluded_context_entries", err)
			}
		}
		return nil
	})
}

// Manifest reads one manifest header with its binding. Budget is not a stored
// column; it lives in the request payload the compiler owns.
func (s *Store) Manifest(ctx context.Context, id model.ManifestID) (model.ContextManifest, error) {
	raw, err := idBlob("manifest_id", string(id))
	if err != nil {
		return model.ContextManifest{}, err
	}
	var m model.ContextManifest
	err = s.read(ctx, func(tx *sql.Tx) error {
		var repo, snap, key []byte
		var gen int64
		var completeness, created string
		err := tx.QueryRowContext(ctx, `SELECT cm.generation_id, g.repository_id, cm.snapshot_id, g.analysis_key, cm.phase, cm.request_hash, cm.policy_version,
			cm.completeness_json, cm.canonical_hash, cm.created_at,
			(SELECT count(*) FROM context_entries ce WHERE ce.manifest_id = cm.id), (SELECT count(*) FROM context_slices cs WHERE cs.manifest_id = cm.id)
			FROM context_manifests cm JOIN generations g ON g.id = cm.generation_id WHERE cm.id = ?`, raw).
			Scan(&gen, &repo, &snap, &key, &m.Phase, &m.RequestHash, &m.PolicyVersion, &completeness, &m.CanonicalHash, &created, &m.EntryCount, &m.SliceCount)
		if isNoRows(err) {
			return invalid("manifest %s does not exist", id)
		}
		if err != nil {
			return wrap("context_manifests", err)
		}
		m.ID = id
		m.Binding = model.Binding{RepositoryID: model.RepositoryID(idHex(repo)), SnapshotID: model.SnapshotID(idHex(snap)),
			GenerationID: model.GenerationID(gen), AnalysisKey: model.AnalysisKey(idHex(key))}
		if err := json.Unmarshal([]byte(completeness), &m.Completeness); err != nil {
			return corrupt("manifest completeness is not readable")
		}
		m.EstimateMethod = model.EstimateMethodUTF8Bytes
		m.CreatedAt, err = parseTime(created)
		return err
	})
	return m, err
}

// ManifestEntries pages a manifest's entries by ordinal.
func (s *Store) ManifestEntries(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ContextEntry, error) {
	raw, err := idBlob("manifest_id", string(id))
	if err != nil {
		return nil, err
	}
	limit = pageLimit(limit)
	var out []model.ContextEntry
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT ordinal, node_id, file_id, requirement, score_micros, estimated_bytes, estimated_tokens, reasons_json, evidence_paths_json
			FROM context_entries WHERE manifest_id = ? AND ordinal > ? ORDER BY ordinal LIMIT ?`, raw, afterOrdinal, limit)
		if err != nil {
			return wrap("context_entries", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e model.ContextEntry
			var node, file []byte
			var reasons, paths string
			if err := rows.Scan(&e.Ordinal, &node, &file, &e.Requirement, &e.ScoreMicros, &e.EstimatedBytes, &e.EstimatedTokens, &reasons, &paths); err != nil {
				return wrap("context_entries", err)
			}
			if node != nil {
				e.NodeID = model.NodeID(idHex(node))
			}
			if file != nil {
				e.FileID = model.FileID(idHex(file))
			}
			if json.Unmarshal([]byte(reasons), &e.Reasons) != nil || json.Unmarshal([]byte(paths), &e.EvidencePaths) != nil {
				return corrupt("manifest entry payload is not readable")
			}
			out = append(out, e)
		}
		return wrap("context_entries", rows.Err())
	})
	return out, err
}

// ManifestSlices pages a manifest's slices by index, with their entry ordinals.
func (s *Store) ManifestSlices(ctx context.Context, id model.ManifestID, afterIndex int, limit int) ([]model.ContextSlice, error) {
	raw, err := idBlob("manifest_id", string(id))
	if err != nil {
		return nil, err
	}
	limit = pageLimit(limit)
	var out []model.ContextSlice
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT slice_index, estimated_bytes, estimated_tokens FROM context_slices WHERE manifest_id = ? AND slice_index > ? ORDER BY slice_index LIMIT ?`, raw, afterIndex, limit)
		if err != nil {
			return wrap("context_slices", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sl model.ContextSlice
			if err := rows.Scan(&sl.Index, &sl.EstimatedBytes, &sl.EstimatedTokens); err != nil {
				return wrap("context_slices", err)
			}
			out = append(out, sl)
		}
		if err := rows.Err(); err != nil {
			return wrap("context_slices", err)
		}
		for i := range out {
			ords, err := tx.QueryContext(ctx, `SELECT entry_ordinal FROM context_slice_entries WHERE manifest_id = ? AND slice_index = ? ORDER BY position LIMIT ?`,
				raw, out[i].Index, model.MaxRecordsPerResult)
			if err != nil {
				return wrap("context_slice_entries", err)
			}
			for ords.Next() {
				var o int
				if err := ords.Scan(&o); err != nil {
					ords.Close()
					return wrap("context_slice_entries", err)
				}
				out[i].EntryOrdinals = append(out[i].EntryOrdinals, o)
			}
			ords.Close()
			if err := ords.Err(); err != nil {
				return wrap("context_slice_entries", err)
			}
		}
		return nil
	})
	return out, err
}

// ManifestExcluded pages a manifest's exclusions by ordinal.
func (s *Store) ManifestExcluded(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ExcludedContextEntry, error) {
	raw, err := idBlob("manifest_id", string(id))
	if err != nil {
		return nil, err
	}
	limit = pageLimit(limit)
	var out []model.ExcludedContextEntry
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT ordinal, reference_json, reason FROM excluded_context_entries WHERE manifest_id = ? AND ordinal > ? ORDER BY ordinal LIMIT ?`, raw, afterOrdinal, limit)
		if err != nil {
			return wrap("excluded_context_entries", err)
		}
		defer rows.Close()
		for rows.Next() {
			var x model.ExcludedContextEntry
			var ref string
			if err := rows.Scan(&x.Ordinal, &ref, &x.Reason); err != nil {
				return wrap("excluded_context_entries", err)
			}
			if json.Unmarshal([]byte(ref), &x.Reference) != nil {
				return corrupt("excluded entry reference is not readable")
			}
			out = append(out, x)
		}
		return wrap("excluded_context_entries", rows.Err())
	})
	return out, err
}

// SessionRecord is one read_sessions row with its generation binding. Readiness
// is computed by the workflow service from this plus Coverage and observations.
type SessionRecord struct {
	ID           model.SessionID
	ActorID      string
	Binding      model.Binding
	ManifestID   model.ManifestID
	Phase        model.Phase
	State        model.WorkflowState
	StateVersion int
	ScopeVersion int
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ClosedAt     *time.Time
}

// sessionFilesSQL derives session_files from the manifest's entries joined to
// the session's own snapshot, keeping the strongest requirement per file. The
// snapshot join is what makes a file from another snapshot impossible here.
const sessionFilesSQL = `INSERT OR IGNORE INTO session_files(session_id, snapshot_id, file_id, content_hash, requirement)
	SELECT ?1, ?2, ce.file_id, sf.content_hash,
		CASE min(CASE ce.requirement WHEN 'required_full' THEN 0 WHEN 'required_symbol' THEN 1 WHEN 'recommended' THEN 2 ELSE 3 END)
			WHEN 0 THEN 'required_full' WHEN 1 THEN 'required_symbol' WHEN 2 THEN 'recommended' ELSE 'optional' END
	FROM context_entries ce JOIN snapshot_files sf ON sf.snapshot_id = ?2 AND sf.file_id = ce.file_id AND sf.content_hash IS NOT NULL
	WHERE ce.manifest_id = ?3 AND ce.file_id IS NOT NULL GROUP BY ce.file_id`

// OpenSession creates an actor-specific session over a manifest and populates
// its file scope. With an idempotency key, a retry by the same actor with the
// same open-request hash returns the existing session; the same key with a
// different hash fails (Section 12.4). Identical requests from different
// actors always get separate sessions.
func (s *Store) OpenSession(ctx context.Context, open model.SessionOpen) (model.SessionID, error) {
	if err := open.Validate(); err != nil {
		return "", err
	}
	idRaw, _ := model.DecodeID(string(open.ID))
	manifestRaw, _ := model.DecodeID(string(open.ManifestID))
	var key any
	if open.IdempotencyKey != "" {
		key = open.IdempotencyKey
	}
	result := open.ID
	err := s.write(ctx, func(tx *sql.Tx) error {
		if open.IdempotencyKey != "" {
			var existing []byte
			var hash string
			err := tx.QueryRowContext(ctx, `SELECT id, open_request_hash FROM read_sessions WHERE actor_id = ? AND idempotency_key = ?`, open.ActorID, open.IdempotencyKey).Scan(&existing, &hash)
			if err == nil {
				if hash != open.OpenRequestHash {
					return conflict("idempotency key %q was used by this actor for a different request", open.IdempotencyKey)
				}
				result = model.SessionID(idHex(existing))
				return nil
			}
			if !isNoRows(err) {
				return wrap("read_sessions", err)
			}
		}
		var gen int64
		var snap []byte
		var phase model.Phase
		err := tx.QueryRowContext(ctx, `SELECT generation_id, snapshot_id, phase FROM context_manifests WHERE id = ?`, manifestRaw).Scan(&gen, &snap, &phase)
		if isNoRows(err) {
			return invalid("manifest %s does not exist", open.ManifestID)
		}
		if err != nil {
			return wrap("context_manifests", err)
		}
		state, err := openState(phase)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO read_sessions(id, actor_id, idempotency_key, open_request_hash, manifest_id, generation_id, snapshot_id,
			workflow_state, state_version, scope_version, created_at, expires_at, closed_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, 1, 1, ?, ?, NULL)`,
			idRaw, open.ActorID, key, open.OpenRequestHash, manifestRaw, gen, snap, string(state), formatTime(now), formatTime(open.ExpiresAt)); err != nil {
			return wrap("read_sessions", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_manifests(session_id, ordinal, manifest_id, phase, activated_at) VALUES(?, 0, ?, ?, ?)`,
			idRaw, manifestRaw, string(phase), formatTime(now)); err != nil {
			return wrap("session_manifests", err)
		}
		_, err = tx.ExecContext(ctx, sessionFilesSQL, idRaw, snap, manifestRaw)
		return wrap("session_files", err)
	})
	return result, err
}

func openState(phase model.Phase) (model.WorkflowState, error) {
	switch phase {
	case model.PhaseSweep:
		return model.StateSweepOpen, nil
	case model.PhaseVerify:
		return model.StateVerifyOpen, nil
	}
	return "", invalid("phase %q cannot open a session", phase)
}

// session loads a session for actor and rejects a wrong actor, an expired or
// closed session. Exact actor identity is checked on every session operation.
func (s *Store) session(ctx context.Context, tx *sql.Tx, id model.SessionID, actor string, now time.Time) (SessionRecord, error) {
	raw, err := idBlob("session_id", string(id))
	if err != nil {
		return SessionRecord{}, err
	}
	var rec SessionRecord
	var repo, snap, key, manifest []byte
	var gen int64
	var created, expires string
	var closed sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT rs.actor_id, rs.manifest_id, rs.generation_id, g.repository_id, rs.snapshot_id, g.analysis_key, cm.phase,
		rs.workflow_state, rs.state_version, rs.scope_version, rs.created_at, rs.expires_at, rs.closed_at
		FROM read_sessions rs JOIN generations g ON g.id = rs.generation_id JOIN context_manifests cm ON cm.id = rs.manifest_id WHERE rs.id = ?`, raw).
		Scan(&rec.ActorID, &manifest, &gen, &repo, &snap, &key, &rec.Phase, &rec.State, &rec.StateVersion, &rec.ScopeVersion, &created, &expires, &closed)
	if isNoRows(err) {
		return rec, invalid("session %s does not exist", id)
	}
	if err != nil {
		return rec, wrap("read_sessions", err)
	}
	rec.ID, rec.ManifestID = id, model.ManifestID(idHex(manifest))
	rec.Binding = model.Binding{RepositoryID: model.RepositoryID(idHex(repo)), SnapshotID: model.SnapshotID(idHex(snap)),
		GenerationID: model.GenerationID(gen), AnalysisKey: model.AnalysisKey(idHex(key))}
	if rec.CreatedAt, err = parseTime(created); err != nil {
		return rec, err
	}
	if rec.ExpiresAt, err = parseTime(expires); err != nil {
		return rec, err
	}
	if closed.Valid {
		t, err := parseTime(closed.String)
		if err != nil {
			return rec, err
		}
		rec.ClosedAt = &t
	}
	if actor != "" && rec.ActorID != actor {
		return rec, &model.Error{Code: model.CodeActorMismatch, Message: "session belongs to another actor; coverage and workflow state are never shared"}
	}
	if rec.State == model.StateClosed || !now.Before(rec.ExpiresAt) {
		return rec, &model.Error{Code: model.CodeSessionExpired, Message: "session is closed or expired"}
	}
	return rec, nil
}

// Session reads a session record for its actor. Unlike the mutating
// operations, a closed or expired session is still returned so status can
// report it honestly.
func (s *Store) Session(ctx context.Context, id model.SessionID, actor string) (SessionRecord, error) {
	var rec SessionRecord
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.session(ctx, tx, id, actor, time.Now())
		if typed, ok := err.(*model.Error); ok && typed.Code == model.CodeSessionExpired {
			return nil
		}
		return err
	})
	return rec, err
}

// allowedTransitions is the Section 17.1 graph. Guards beyond reachability
// (resolved seeds, coverage, review) are the workflow service's; the store
// refuses an unreachable transition and a stale state version.
var allowedTransitions = map[model.WorkflowState][]model.WorkflowState{
	model.StateSweepOpen:       {model.StateVerifyOpen, model.StateClosed},
	model.StateVerifyOpen:      {model.StateConsolidateOpen, model.StateClosed},
	model.StateConsolidateOpen: {model.StateComplete, model.StateClosed},
}

// AdvanceSession performs one compare-and-swap workflow transition.
func (s *Store) AdvanceSession(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, error) {
	if err := req.Validate(); err != nil {
		return model.WorkflowStatus{}, err
	}
	var status model.WorkflowStatus
	err := s.write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		rec, err := s.session(ctx, tx, req.SessionID, req.ActorID, now)
		if err != nil {
			return err
		}
		reachable := false
		for _, t := range allowedTransitions[rec.State] {
			reachable = reachable || t == req.Target
		}
		if !reachable {
			return conflict("session cannot move from %s to %s", rec.State, req.Target)
		}
		var closedAt any
		if req.Target == model.StateClosed || req.Target == model.StateComplete {
			closedAt = formatTime(now)
		}
		raw, _ := model.DecodeID(string(req.SessionID))
		if err := exec1(tx, ctx, &model.Error{Code: model.CodeVersionConflict, Message: "session state version has moved; reload and retry"},
			`UPDATE read_sessions SET workflow_state = ?, state_version = state_version + 1, closed_at = ? WHERE id = ? AND state_version = ?`,
			string(req.Target), closedAt, raw, req.ExpectedVersion); err != nil {
			return err
		}
		status = model.WorkflowStatus{SessionID: rec.ID, State: req.Target, StateVersion: rec.StateVersion + 1,
			ScopeVersion: rec.ScopeVersion, ManifestID: rec.ManifestID, Phase: rec.Phase}
		return nil
	})
	return status, err
}

// IncludeManifest points an open sweep/verify session at a recompiled manifest
// of the same generation and snapshot: history is appended, scope and state
// versions advance, and the new manifest's files join the session while
// same-hash coverage already confirmed is retained.
func (s *Store) IncludeManifest(ctx context.Context, req model.IncludeRequest, manifest model.ManifestID) (model.WorkflowStatus, error) {
	if err := req.Validate(); err != nil {
		return model.WorkflowStatus{}, err
	}
	manifestRaw, err := idBlob("manifest_id", string(manifest))
	if err != nil {
		return model.WorkflowStatus{}, err
	}
	var status model.WorkflowStatus
	err = s.write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		rec, err := s.session(ctx, tx, req.SessionID, req.ActorID, now)
		if err != nil {
			return err
		}
		if rec.State != model.StateSweepOpen && rec.State != model.StateVerifyOpen {
			return conflict("scope can be included only in an open sweep or verify session, not %s", rec.State)
		}
		var gen int64
		var snap []byte
		var phase model.Phase
		err = tx.QueryRowContext(ctx, `SELECT generation_id, snapshot_id, phase FROM context_manifests WHERE id = ?`, manifestRaw).Scan(&gen, &snap, &phase)
		if isNoRows(err) {
			return invalid("manifest %s does not exist", manifest)
		}
		if err != nil {
			return wrap("context_manifests", err)
		}
		if model.GenerationID(gen) != rec.Binding.GenerationID || model.SnapshotID(idHex(snap)) != rec.Binding.SnapshotID {
			return &model.Error{Code: model.CodeSessionSuperseded, Message: "manifest generation/snapshot binding cannot change within a session"}
		}
		raw, _ := model.DecodeID(string(req.SessionID))
		if err := exec1(tx, ctx, &model.Error{Code: model.CodeVersionConflict, Message: "session state version has moved; reload and retry"},
			`UPDATE read_sessions SET manifest_id = ?, scope_version = scope_version + 1, state_version = state_version + 1 WHERE id = ? AND state_version = ?`,
			manifestRaw, raw, req.ExpectedVersion); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_manifests(session_id, ordinal, manifest_id, phase, activated_at)
			VALUES(?, (SELECT count(*) FROM session_manifests WHERE session_id = ?), ?, ?, ?)`, raw, raw, manifestRaw, string(phase), formatTime(now)); err != nil {
			return wrap("session_manifests", err)
		}
		if _, err := tx.ExecContext(ctx, sessionFilesSQL, raw, snap, manifestRaw); err != nil {
			return wrap("session_files", err)
		}
		status = model.WorkflowStatus{SessionID: rec.ID, State: rec.State, StateVersion: rec.StateVersion + 1,
			ScopeVersion: rec.ScopeVersion + 1, ManifestID: manifest, Phase: phase}
		return nil
	})
	return status, err
}

// IssueChunk persists an issued, not yet confirmed, chunk. The chunk must name
// a file and content hash in the session's pinned scope; the schema's foreign
// key to session_files makes another snapshot's bytes unrepresentable.
func (s *Store) IssueChunk(ctx context.Context, c model.IssuedChunk) error {
	if err := c.Validate(); err != nil {
		return err
	}
	idRaw, _ := model.DecodeID(c.ID)
	sessionRaw, _ := model.DecodeID(string(c.SessionID))
	fileRaw, _ := model.DecodeID(string(c.FileID))
	hashRaw, _ := model.DecodeID(c.ContentHash)
	return s.write(ctx, func(tx *sql.Tx) error {
		var size int64
		err := tx.QueryRowContext(ctx, `SELECT sf.size_bytes FROM session_files s JOIN snapshot_files sf ON sf.snapshot_id = s.snapshot_id AND sf.file_id = s.file_id AND sf.content_hash = s.content_hash
			WHERE s.session_id = ? AND s.file_id = ? AND s.content_hash = ?`, sessionRaw, fileRaw, hashRaw).Scan(&size)
		if isNoRows(err) {
			return &model.Error{Code: model.CodeScopeIncomplete, Message: "file and content hash are not in this session's pinned scope",
				Details: map[string]string{"file_id": string(c.FileID)}}
		}
		if err != nil {
			return wrap("session_files", err)
		}
		if c.Bytes.End > uint64(size) {
			return invalid("chunk [%d,%d) exceeds the %d-byte source", c.Bytes.Start, c.Bytes.End, size)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO issued_chunks(id, session_id, file_id, content_hash, start_byte, end_byte, expires_at, confirmed_at) VALUES(?, ?, ?, ?, ?, ?, ?, NULL)`,
			idRaw, sessionRaw, fileRaw, hashRaw, int64(c.Bytes.Start), int64(c.Bytes.End), formatTime(c.ExpiresAt))
		return wrap("issued_chunks", err)
	})
}

// UnconfirmedChunks counts a session's issued but unconfirmed, unexpired
// chunks so the coverage service can enforce its per-session cap.
func (s *Store) UnconfirmedChunks(ctx context.Context, session model.SessionID) (int64, error) {
	raw, err := idBlob("session_id", string(session))
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.read(ctx, func(tx *sql.Tx) error {
		return wrap("issued_chunks", tx.QueryRowContext(ctx, `SELECT count(*) FROM issued_chunks WHERE session_id = ? AND confirmed_at IS NULL AND expires_at > ?`,
			raw, formatTime(time.Now())).Scan(&n))
	})
	return n, err
}

// ConfirmChunks marks issued chunks confirmed and merges their intervals into
// served_ranges in one transaction (Section 16.3). Duplicate confirmations are
// idempotent; an unknown, expired or foreign chunk fails the whole batch.
func (s *Store) ConfirmChunks(ctx context.Context, session model.SessionID, actor string, chunkIDs []string) error {
	if len(chunkIDs) == 0 || len(chunkIDs) > model.MaxReceiptsPerConfirmation {
		return invalid("confirmation needs between 1 and %d chunk ids", model.MaxReceiptsPerConfirmation)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		rec, err := s.session(ctx, tx, session, actor, now)
		if err != nil {
			return err
		}
		sessionRaw, _ := model.DecodeID(string(rec.ID))
		for _, id := range chunkIDs {
			idRaw, err := idBlob("chunk_id", id)
			if err != nil {
				return err
			}
			var file, hash []byte
			var start, end int64
			var expires string
			var confirmed sql.NullString
			err = tx.QueryRowContext(ctx, `SELECT file_id, content_hash, start_byte, end_byte, expires_at, confirmed_at FROM issued_chunks WHERE id = ? AND session_id = ?`, idRaw, sessionRaw).
				Scan(&file, &hash, &start, &end, &expires, &confirmed)
			if isNoRows(err) {
				return &model.Error{Code: model.CodeCursorInvalid, Message: "receipt does not name a chunk issued to this session"}
			}
			if err != nil {
				return wrap("issued_chunks", err)
			}
			if confirmed.Valid {
				continue
			}
			expiry, err := parseTime(expires)
			if err != nil {
				return err
			}
			if !now.Before(expiry) {
				return &model.Error{Code: model.CodeCursorInvalid, Message: "receipt has expired; read the chunk again"}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issued_chunks SET confirmed_at = ? WHERE id = ?`, formatTime(now), idRaw); err != nil {
				return wrap("issued_chunks", err)
			}
			if start == end {
				// A zero-length EOF receipt confirms an empty file was opened; it
				// merges no bytes and is recorded on the chunk row alone.
				continue
			}
			if err := mergeServedRange(ctx, tx, sessionRaw, file, hash, start, end); err != nil {
				return err
			}
		}
		return nil
	})
}

// mergeServedRange folds [start,end) into the stored disjoint intervals for one
// session file, coalescing overlap and adjacency. The stored set is always
// minimal, so the rows read here are bounded by the file's gap count.
func mergeServedRange(ctx context.Context, tx *sql.Tx, session, file, hash []byte, start, end int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT start_byte, end_byte FROM served_ranges WHERE session_id = ? AND file_id = ? AND content_hash = ? AND end_byte >= ? AND start_byte <= ?`,
		session, file, hash, start, end)
	if err != nil {
		return wrap("served_ranges", err)
	}
	type interval struct{ start, end int64 }
	var touching []interval
	for rows.Next() {
		var iv interval
		if err := rows.Scan(&iv.start, &iv.end); err != nil {
			rows.Close()
			return wrap("served_ranges", err)
		}
		touching = append(touching, iv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap("served_ranges", err)
	}
	for _, iv := range touching {
		start, end = min(start, iv.start), max(end, iv.end)
		if _, err := tx.ExecContext(ctx, `DELETE FROM served_ranges WHERE session_id = ? AND file_id = ? AND content_hash = ? AND start_byte = ? AND end_byte = ?`,
			session, file, hash, iv.start, iv.end); err != nil {
			return wrap("served_ranges", err)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO served_ranges(session_id, file_id, content_hash, start_byte, end_byte) VALUES(?, ?, ?, ?, ?)`, session, file, hash, start, end)
	return wrap("served_ranges", err)
}

// Coverage pages the session's honest per-file coverage: pinned hash, size,
// confirmed bytes (the disjoint served union), requirement, state and waiver.
// A nonempty file is full_served only when the union is exactly [0,size); an
// empty file requires a confirmed zero-length EOF receipt.
func (s *Store) Coverage(ctx context.Context, session model.SessionID, actor string, after model.FileID, limit int) ([]model.FileCoverage, error) {
	limit = pageLimit(limit)
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	if afterRaw == nil {
		afterRaw = []byte{}
	}
	var out []model.FileCoverage
	err = s.read(ctx, func(tx *sql.Tx) error {
		rec, err := s.session(ctx, tx, session, actor, time.Now())
		if err != nil {
			if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeSessionExpired {
				return err
			}
		}
		sessionRaw, _ := model.DecodeID(string(rec.ID))
		rows, err := tx.QueryContext(ctx, `SELECT s.file_id, s.content_hash, sf.size_bytes, s.requirement,
			(SELECT coalesce(sum(r.end_byte - r.start_byte), 0) FROM served_ranges r WHERE r.session_id = s.session_id AND r.file_id = s.file_id AND r.content_hash = s.content_hash),
			(SELECT count(*) FROM issued_chunks ic WHERE ic.session_id = s.session_id AND ic.file_id = s.file_id AND ic.content_hash = s.content_hash AND ic.confirmed_at IS NOT NULL AND ic.start_byte = ic.end_byte),
			EXISTS (SELECT 1 FROM coverage_waivers w WHERE w.session_id = s.session_id AND w.file_id = s.file_id AND w.content_hash = s.content_hash)
			FROM session_files s JOIN snapshot_files sf ON sf.snapshot_id = s.snapshot_id AND sf.file_id = s.file_id AND sf.content_hash = s.content_hash
			WHERE s.session_id = ? AND s.file_id > ? ORDER BY s.file_id LIMIT ?`, sessionRaw, afterRaw, limit)
		if err != nil {
			return wrap("session_files", err)
		}
		defer rows.Close()
		for rows.Next() {
			var fc model.FileCoverage
			var file, hash []byte
			var eofReceipts int64
			if err := rows.Scan(&file, &hash, &fc.Size, &fc.Requirement, &fc.ConfirmedBytes, &eofReceipts, &fc.Waived); err != nil {
				return wrap("session_files", err)
			}
			fc.FileID, fc.ContentHash = model.FileID(idHex(file)), idHex(hash)
			switch {
			case fc.Size == 0 && eofReceipts > 0, fc.Size > 0 && fc.ConfirmedBytes == fc.Size:
				fc.State = model.CoverageFullServed
			case fc.ConfirmedBytes > 0:
				fc.State = model.CoveragePartialServed
			default:
				fc.State = model.CoverageUnserved
			}
			out = append(out, fc)
		}
		return wrap("session_files", rows.Err())
	})
	return out, err
}

// AcknowledgeFile records the client's separate full-file statement. It fails
// unless the file is already fully served for this session and hash; an
// acknowledgment never substitutes for receipts (Section 16.2).
func (s *Store) AcknowledgeFile(ctx context.Context, session model.SessionID, actor string, file model.FileID) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		rec, err := s.session(ctx, tx, session, actor, time.Now())
		if err != nil {
			return err
		}
		cov, err := s.fileCoverage(ctx, tx, rec, file)
		if err != nil {
			return err
		}
		if cov.State != model.CoverageFullServed {
			return &model.Error{Code: model.CodeCoverageIncomplete,
				Message: fmt.Sprintf("file has %d of %d bytes confirmed; a file acknowledgment requires full confirmed coverage", cov.ConfirmedBytes, cov.Size)}
		}
		sessionRaw, _ := model.DecodeID(string(rec.ID))
		fileRaw, _ := model.DecodeID(string(file))
		hashRaw, _ := model.DecodeID(cov.ContentHash)
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO source_acknowledgements(session_id, file_id, content_hash, acknowledged_at) VALUES(?, ?, ?, ?)`,
			sessionRaw, fileRaw, hashRaw, formatTime(time.Now()))
		return wrap("source_acknowledgements", err)
	})
}

// fileCoverage is Coverage for one file inside a transaction.
func (s *Store) fileCoverage(ctx context.Context, tx *sql.Tx, rec SessionRecord, file model.FileID) (model.FileCoverage, error) {
	fileRaw, err := idBlob("file_id", string(file))
	if err != nil {
		return model.FileCoverage{}, err
	}
	sessionRaw, _ := model.DecodeID(string(rec.ID))
	var fc model.FileCoverage
	var hash []byte
	var eof int64
	err = tx.QueryRowContext(ctx, `SELECT s.content_hash, sf.size_bytes, s.requirement,
		(SELECT coalesce(sum(r.end_byte - r.start_byte), 0) FROM served_ranges r WHERE r.session_id = s.session_id AND r.file_id = s.file_id AND r.content_hash = s.content_hash),
		(SELECT count(*) FROM issued_chunks ic WHERE ic.session_id = s.session_id AND ic.file_id = s.file_id AND ic.content_hash = s.content_hash AND ic.confirmed_at IS NOT NULL AND ic.start_byte = ic.end_byte)
		FROM session_files s JOIN snapshot_files sf ON sf.snapshot_id = s.snapshot_id AND sf.file_id = s.file_id AND sf.content_hash = s.content_hash
		WHERE s.session_id = ? AND s.file_id = ?`, sessionRaw, fileRaw).Scan(&hash, &fc.Size, &fc.Requirement, &fc.ConfirmedBytes, &eof)
	if isNoRows(err) {
		return fc, &model.Error{Code: model.CodeScopeIncomplete, Message: "file is not in this session's scope"}
	}
	if err != nil {
		return fc, wrap("session_files", err)
	}
	fc.FileID, fc.ContentHash = file, idHex(hash)
	switch {
	case fc.Size == 0 && eof > 0, fc.Size > 0 && fc.ConfirmedBytes == fc.Size:
		fc.State = model.CoverageFullServed
	case fc.ConfirmedBytes > 0:
		fc.State = model.CoveragePartialServed
	default:
		fc.State = model.CoverageUnserved
	}
	return fc, nil
}

// Waive records an audited exception for one scoped file. It changes no
// coverage and never grants strict readiness (Section 17.3).
func (s *Store) Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, error) {
	if err := req.Validate(); err != nil {
		return model.WaiverRecord{}, err
	}
	var rec model.WaiverRecord
	err := s.write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		sess, err := s.session(ctx, tx, req.SessionID, req.ActorID, now)
		if err != nil {
			return err
		}
		cov, err := s.fileCoverage(ctx, tx, sess, req.FileID)
		if err != nil {
			return err
		}
		sessionRaw, _ := model.DecodeID(string(sess.ID))
		fileRaw, _ := model.DecodeID(string(req.FileID))
		hashRaw, _ := model.DecodeID(cov.ContentHash)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO coverage_waivers(session_id, file_id, content_hash, reason, created_at) VALUES(?, ?, ?, ?, ?)`,
			sessionRaw, fileRaw, hashRaw, req.Reason, formatTime(now)); err != nil {
			return wrap("coverage_waivers", err)
		}
		rec = model.WaiverRecord{SessionID: sess.ID, ActorID: sess.ActorID, FileID: req.FileID, ContentHash: cov.ContentHash, Reason: req.Reason, CreatedAt: now.UTC()}
		return nil
	})
	return rec, err
}

// observationPayload is the strict typed shape of session_observations.references_json.
type observationPayload struct {
	References []model.ClaimReference `json:"references"`
	Review     *model.ScopeReview     `json:"review,omitempty"`
}

// PutObservation appends an immutable observation. Its ID is derived from its
// content, so a resubmission is idempotent; an existing observation is never
// rewritten. The scope version must be the session's current one.
func (s *Store) PutObservation(ctx context.Context, o model.Observation) error {
	if err := o.Validate(); err != nil {
		return err
	}
	idRaw, _ := model.DecodeID(string(o.ID))
	sessionRaw, _ := model.DecodeID(string(o.SessionID))
	payload, err := json.Marshal(observationPayload{References: o.References, Review: o.Review})
	if err != nil {
		return internal("observation payload: " + err.Error())
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		rec, err := s.session(ctx, tx, o.SessionID, o.ActorID, now)
		if err != nil {
			return err
		}
		if o.ScopeVersion != rec.ScopeVersion {
			return &model.Error{Code: model.CodeScopeChanged, Message: fmt.Sprintf("observation names scope version %d but the session is at %d", o.ScopeVersion, rec.ScopeVersion)}
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_observations(id, session_id, scope_version, kind, references_json, note, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			idRaw, sessionRaw, o.ScopeVersion, string(o.Kind), string(payload), o.Note, formatTime(now))
		return wrap("session_observations", err)
	})
}

// Observations pages a session's observations by ID, optionally one kind and
// one scope version (zero means every version).
func (s *Store) Observations(ctx context.Context, session model.SessionID, actor string, kind model.ObservationKind, scopeVersion int,
	after model.ObservationID, limit int) ([]model.Observation, error) {
	limit = pageLimit(limit)
	afterRaw, err := optionalBlob("after", string(after))
	if err != nil {
		return nil, err
	}
	if afterRaw == nil {
		afterRaw = []byte{}
	}
	var out []model.Observation
	err = s.read(ctx, func(tx *sql.Tx) error {
		rec, err := s.session(ctx, tx, session, actor, time.Now())
		if err != nil {
			if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeSessionExpired {
				return err
			}
		}
		sessionRaw, _ := model.DecodeID(string(rec.ID))
		rows, err := tx.QueryContext(ctx, `SELECT id, scope_version, kind, references_json, note, created_at FROM session_observations
			WHERE session_id = ? AND (? = '' OR kind = ?) AND (? = 0 OR scope_version = ?) AND id > ? ORDER BY id LIMIT ?`,
			sessionRaw, string(kind), string(kind), scopeVersion, scopeVersion, afterRaw, limit)
		if err != nil {
			return wrap("session_observations", err)
		}
		defer rows.Close()
		for rows.Next() {
			o := model.Observation{SessionID: rec.ID, ActorID: rec.ActorID}
			var id []byte
			var payload, created string
			if err := rows.Scan(&id, &o.ScopeVersion, &o.Kind, &payload, &o.Note, &created); err != nil {
				return wrap("session_observations", err)
			}
			var p observationPayload
			if json.Unmarshal([]byte(payload), &p) != nil {
				return corrupt("observation payload is not readable")
			}
			o.ID, o.References, o.Review = model.ObservationID(idHex(id)), p.References, p.Review
			if o.CreatedAt, err = parseTime(created); err != nil {
				return err
			}
			out = append(out, o)
		}
		return wrap("session_observations", rows.Err())
	})
	return out, err
}

// PutCapsule stores the deterministic completion record once. A second call
// returns the first stored capsule unchanged (Section 17.3).
func (s *Store) PutCapsule(ctx context.Context, c model.Capsule) (model.Capsule, error) {
	if err := c.Validate(); err != nil {
		return model.Capsule{}, err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return model.Capsule{}, internal("capsule: " + err.Error())
	}
	if int64(len(payload)) > s.opts.MaxJSONBytes {
		return model.Capsule{}, &model.Error{Code: model.CodeResourceLimit, Message: "capsule exceeds the stored JSON bound",
			Remediation: "export the capsule paginated; raise context.max_capsule_bytes only with sufficient reservations"}
	}
	stored := c
	err = s.write(ctx, func(tx *sql.Tx) error {
		rec, err := s.session(ctx, tx, c.SessionID, c.ActorID, time.Now())
		if err != nil {
			if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeSessionExpired {
				return err
			}
		}
		if rec.Binding != c.Binding {
			return invalid("capsule binding does not match its session's generation and snapshot")
		}
		sessionRaw, _ := model.DecodeID(string(c.SessionID))
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT capsule_json FROM context_capsules WHERE session_id = ?`, sessionRaw).Scan(&existing)
		if err == nil {
			if json.Unmarshal([]byte(existing), &stored) != nil {
				return corrupt("stored capsule is not readable")
			}
			return nil
		}
		if !isNoRows(err) {
			return wrap("context_capsules", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO context_capsules(session_id, canonical_hash, capsule_json, created_at) VALUES(?, ?, ?, ?)`,
			sessionRaw, c.CanonicalHash, string(payload), formatTime(c.CreatedAt))
		return wrap("context_capsules", err)
	})
	return stored, err
}

// Capsule reads a session's stored capsule for its actor.
func (s *Store) Capsule(ctx context.Context, session model.SessionID, actor string) (model.Capsule, error) {
	var c model.Capsule
	err := s.read(ctx, func(tx *sql.Tx) error {
		rec, err := s.session(ctx, tx, session, actor, time.Now())
		if err != nil {
			if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeSessionExpired {
				return err
			}
		}
		sessionRaw, _ := model.DecodeID(string(rec.ID))
		var payload string
		err = tx.QueryRowContext(ctx, `SELECT capsule_json FROM context_capsules WHERE session_id = ?`, sessionRaw).Scan(&payload)
		if isNoRows(err) {
			return invalid("session %s has no capsule", session)
		}
		if err != nil {
			return wrap("context_capsules", err)
		}
		if json.Unmarshal([]byte(payload), &c) != nil {
			return corrupt("stored capsule is not readable")
		}
		return nil
	})
	return c, err
}

// ExpireSessions closes open sessions whose TTL has passed, in one bounded
// batch, and reports how many it closed. Closing stops mutations; the audit
// artifacts stay until PruneSessions.
func (s *Store) ExpireSessions(ctx context.Context, now time.Time, limit int) (int64, error) {
	limit = pageLimit(limit)
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE read_sessions SET workflow_state = 'closed', closed_at = ?, state_version = state_version + 1
			WHERE id IN (SELECT id FROM read_sessions WHERE expires_at <= ? AND workflow_state NOT IN ('closed', 'complete') LIMIT ?)`,
			formatTime(now), formatTime(now), limit)
		if err != nil {
			return wrap("read_sessions", err)
		}
		n, err = res.RowsAffected()
		return wrap("read_sessions", err)
	})
	return n, err
}

// PruneSessions deletes closed or complete sessions whose closed_at is older
// than retention, in one bounded batch. Their chunks, ranges, observations and
// capsules cascade; their manifests become collectable with their generation.
func (s *Store) PruneSessions(ctx context.Context, now time.Time, retention time.Duration, limit int) (int64, error) {
	limit = pageLimit(limit)
	if retention < 0 {
		return 0, invalid("closed-session retention must not be negative")
	}
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM read_sessions WHERE id IN (
			SELECT id FROM read_sessions WHERE workflow_state IN ('closed', 'complete') AND closed_at IS NOT NULL AND closed_at <= ? LIMIT ?)`,
			formatTime(now.Add(-retention)), limit)
		if err != nil {
			return wrap("read_sessions", err)
		}
		n, err = res.RowsAffected()
		return wrap("read_sessions", err)
	})
	return n, err
}
