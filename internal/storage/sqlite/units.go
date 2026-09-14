package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Hash domains for the aggregate digests folded into AnalysisKey (Section 9.1).
const (
	domainMembership   = "generation-units-v1"
	domainCapabilities = "generation-capabilities-v1"
)

// BeginGeneration opens a staging generation over snapshot. Staging facts are
// invisible to every reader until Activate publishes the pointer.
//
// ref is the ref this generation is built from and is what retention groups
// by (Section 12.4): keeping the last retain_refs DISTINCT refs is what makes
// switching A -> B -> C -> A find A's units still on disk. Storage never
// interprets it. The coordinator supplies a branch name, the HEAD object id
// when HEAD is detached, or the sentinel "(none)" when the workspace is not a
// Git repository; an empty ref is refused rather than silently grouping every
// non-Git generation under one nameless bucket.
func (s *Store) BeginGeneration(ctx context.Context, repo model.RepositoryID, snapshot model.SnapshotID, semanticConfigHash, ref string) (model.GenerationID, error) {
	repoRaw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return 0, err
	}
	snapRaw, err := idBlob("snapshot_id", string(snapshot))
	if err != nil {
		return 0, err
	}
	if !model.ValidHexID(semanticConfigHash) {
		return 0, invalid("semantic_config_hash must be a %d-character lowercase hex digest", model.IDHexLen)
	}
	// A refname is path-shaped, so MaxPathBytes is its structural ceiling.
	if ref == "" || len(ref) > model.MaxPathBytes {
		return 0, invalid("generation ref is required and bounded to %d bytes", model.MaxPathBytes)
	}
	var id int64
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO generations(repository_id, snapshot_id, ref, analysis_key, semantic_config_hash, status, health, created_at)
			VALUES(?, ?, ?, NULL, ?, ?, ?, ?)`, repoRaw, snapRaw, ref, semanticConfigHash, string(model.GenerationStaging), string(model.HealthFresh), formatTime(time.Now()))
		if err != nil {
			return wrap("generations", err)
		}
		id, err = res.LastInsertId()
		return wrap("generations", err)
	})
	return model.GenerationID(id), err
}

// BeginProviderRun records a running provider invocation for gen and returns
// its random operational ID.
func (s *Store) BeginProviderRun(ctx context.Context, gen model.GenerationID, providerID, providerVersion string) (model.ProviderRunID, error) {
	if providerID == "" || len(providerID) > model.MaxIdentifierBytes || providerVersion == "" || len(providerVersion) > model.MaxIdentifierBytes {
		return "", invalid("provider id and version are required and bounded to %d bytes", model.MaxIdentifierBytes)
	}
	id, err := model.NewRandomID()
	if err != nil {
		return "", err
	}
	raw, _ := model.DecodeID(id)
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := s.generationRow(ctx, tx, gen, model.GenerationStaging); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO provider_runs(id, generation_id, provider_id, provider_version, status, counters_json, diagnostic_code, started_at, completed_at)
			VALUES(?, ?, ?, ?, 'running', '{}', '', ?, NULL)`, raw, int64(gen), providerID, providerVersion, formatTime(time.Now()))
		return wrap("provider_runs", err)
	})
	if err != nil {
		return "", err
	}
	return model.ProviderRunID(id), nil
}

// runCounters is the strict typed shape of provider_runs.counters_json.
type runCounters struct {
	RecordsEmitted uint64 `json:"records_emitted"`
	BytesProcessed uint64 `json:"bytes_processed"`
}

// CompleteProviderRun records a run's terminal state and counters.
func (s *Store) CompleteProviderRun(ctx context.Context, result model.ProviderResult, diagnosticCode string) error {
	if err := result.Validate(); err != nil {
		return err
	}
	if result.State == model.RunRunning {
		return invalid("a completed run cannot be reported as running")
	}
	if len(diagnosticCode) > model.MaxIdentifierBytes {
		return invalid("diagnostic_code exceeds %d bytes", model.MaxIdentifierBytes)
	}
	raw, err := idBlob("run_id", string(result.RunID))
	if err != nil {
		return err
	}
	counters, err := json.Marshal(runCounters{RecordsEmitted: result.RecordsEmitted, BytesProcessed: result.BytesProcessed})
	if err != nil {
		return internal("counters: " + err.Error())
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		return exec1(ctx, tx, conflict("provider run %s is not running", result.RunID),
			`UPDATE provider_runs SET status = ?, counters_json = ?, diagnostic_code = ?, completed_at = ? WHERE id = ? AND status = 'running'`,
			string(result.State), string(counters), diagnosticCode, formatTime(time.Now()), raw)
	})
}

// ProviderRun reads one run's provenance. GenerationID is zero once the
// creating generation has been deleted.
func (s *Store) ProviderRun(ctx context.Context, id model.ProviderRunID) (model.ProviderRun, error) {
	raw, err := idBlob("run_id", string(id))
	if err != nil {
		return model.ProviderRun{}, err
	}
	var run model.ProviderRun
	err = s.read(ctx, func(tx *sql.Tx) error {
		var gen sql.NullInt64
		var counters, started string
		var completed sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT generation_id, provider_id, provider_version, status, counters_json, diagnostic_code, started_at, completed_at
			FROM provider_runs WHERE id = ?`, raw).Scan(&gen, &run.ProviderID, &run.ProviderVersion, &run.State, &counters, &run.DiagnosticCode, &started, &completed)
		if isNoRows(err) {
			return invalid("provider run %s does not exist", id)
		}
		if err != nil {
			return wrap("provider_runs", err)
		}
		run.ID = id
		run.GenerationID = model.GenerationID(gen.Int64)
		var c runCounters
		if err := json.Unmarshal([]byte(counters), &c); err != nil {
			return corrupt("provider run counters are not readable")
		}
		run.RecordsEmitted, run.BytesProcessed = c.RecordsEmitted, c.BytesProcessed
		if run.StartedAt, err = parseTime(started); err != nil {
			return err
		}
		if completed.Valid {
			t, err := parseTime(completed.String)
			if err != nil {
				return err
			}
			run.CompletedAt = &t
		}
		return nil
	})
	return run, err
}

// generation is the subset of a generations row the write paths consult.
type generation struct {
	id       int64
	repo     []byte
	snapshot []byte
	semantic string
	status   model.GenerationStatus
}

// generationRow loads gen and, when want is nonempty, requires that status.
func (s *Store) generationRow(ctx context.Context, tx *sql.Tx, gen model.GenerationID, want model.GenerationStatus) (generation, error) {
	g := generation{id: int64(gen)}
	err := tx.QueryRowContext(ctx, `SELECT repository_id, snapshot_id, semantic_config_hash, status FROM generations WHERE id = ?`, g.id).
		Scan(&g.repo, &g.snapshot, &g.semantic, &g.status)
	if isNoRows(err) {
		return g, invalid("generation %d does not exist", gen)
	}
	if err != nil {
		return g, wrap("generations", err)
	}
	if want != "" && g.status != want {
		return g, conflict("generation %d is %s, want %s", gen, g.status, want)
	}
	return g, nil
}

// UnitWriter receives one unit's facts in bounded batches while the unit is
// building. Only the store hands one out; providers reach it through the Task
// 6 sink and never see SQL.
type UnitWriter struct {
	s       *Store
	rowID   int64
	build   model.UnitBuild
	unitKey []byte
	repo    model.RepositoryID
	repoRaw []byte
	gen     model.GenerationID
	ftsDocs int64
	done    bool

	// evidenceClipped records what SealUnit dropped to hold the evidence
	// bound, so a caller can report the truncation instead of it being silent.
	evidenceClipped int64
}

// UnitID is the immutable key of the unit being written.
func (w *UnitWriter) UnitID() model.UnitID { return w.build.Spec.ID }

// EvidenceClipped is the number of occurrence rows SealUnit dropped to hold
// the Section 11.1 bound on evidence per fact. It is zero until the unit is
// sealed. A nonzero count is not a failure; it is what keeps the truncation
// from being silent, and SealUnit itself logs one bounded warning naming the
// unit and the count, so the drop is on the record even for a caller that
// never reads this.
func (w *UnitWriter) EvidenceClipped() int64 { return w.evidenceClipped }

// BeginUnit opens an immutable unit for gen. It recomputes the unit identity,
// requires every declared dependency to be sealed, streams and hashes the
// inputs in bounded batches against gen's snapshot, and rejects the unit if
// the streamed inputs do not reproduce Spec.InputHash. inputs must yield in
// ascending FileID order. A sealed unit with the same key cannot be rebuilt;
// use AttachUnit to reuse it.
func (s *Store) BeginUnit(ctx context.Context, gen model.GenerationID, build model.UnitBuild,
	inputs func(yield func(model.UnitInput) error) error) (*UnitWriter, error) {
	if err := build.Validate(); err != nil {
		return nil, err
	}
	unitKey, _ := model.DecodeID(string(build.Spec.ID))
	runRaw, _ := model.DecodeID(string(build.OriginRunID))
	w := &UnitWriter{s: s, build: build, unitKey: unitKey, gen: gen}
	var snapshot []byte
	err := s.write(ctx, func(tx *sql.Tx) error {
		g, err := s.generationRow(ctx, tx, gen, model.GenerationStaging)
		if err != nil {
			return err
		}
		snapshot, w.repoRaw = g.snapshot, g.repo
		w.repo = model.RepositoryID(idHex(g.repo))
		var runGen sql.NullInt64
		var runProvider, runVersion string
		err = tx.QueryRowContext(ctx, `SELECT generation_id, provider_id, provider_version FROM provider_runs WHERE id = ? AND status = 'running'`, runRaw).
			Scan(&runGen, &runProvider, &runVersion)
		if isNoRows(err) {
			return invalid("origin run %s is not a running provider run", build.OriginRunID)
		}
		if err != nil {
			return wrap("provider_runs", err)
		}
		if runGen.Int64 != g.id || runProvider != build.Spec.ProviderID || runVersion != build.Spec.ProviderVersion {
			return invalid("origin run belongs to another generation or provider")
		}
		var existing int64
		var state model.UnitState
		err = tx.QueryRowContext(ctx, `SELECT id, state FROM units WHERE unit_key = ?`, unitKey).Scan(&existing, &state)
		switch {
		case err == nil && state == model.UnitSealed:
			return conflict("unit %s is already sealed; attach it instead of rebuilding", build.Spec.ID)
		case err == nil:
			// An earlier attempt left an unsealed unit; it was never visible.
			if err := s.deleteUnit(ctx, tx, existing); err != nil {
				return err
			}
		case !isNoRows(err):
			return wrap("units", err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO units(unit_key, provider_id, provider_version, scope_key, input_hash, dependency_hash, origin_run_id, state, source_binding)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, unitKey, build.Spec.ProviderID, build.Spec.ProviderVersion, build.Spec.ScopeKey,
			build.Spec.InputHash, build.Spec.DependencyHash, runRaw, string(model.UnitBuilding), string(build.SourceBinding))
		if err != nil {
			return wrap("units", err)
		}
		if w.rowID, err = res.LastInsertId(); err != nil {
			return wrap("units", err)
		}
		for _, dep := range build.Dependencies {
			depKey, _ := model.DecodeID(string(dep))
			var depRow int64
			var depState model.UnitState
			err := tx.QueryRowContext(ctx, `SELECT id, state FROM units WHERE unit_key = ?`, depKey).Scan(&depRow, &depState)
			if isNoRows(err) || (err == nil && depState != model.UnitSealed) {
				return invalid("dependency %s is not a sealed unit; providers resolve only against completed dependencies", dep)
			}
			if err != nil {
				return wrap("units", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO unit_dependencies(unit_id, dependency_id) VALUES(?, ?)`, w.rowID, depRow); err != nil {
				return wrap("unit_dependencies", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := w.streamInputs(ctx, snapshot, inputs); err != nil {
		// The unit row and any inputs already written must not linger as a
		// building unit; a failure to remove them is reported alongside the
		// cause rather than hidden behind it.
		return nil, errors.Join(err, w.Fail(ctx))
	}
	return w, nil
}

// streamInputs writes unit_inputs in bounded batches, checking each row against
// the generation's snapshot and folding the canonical input digest.
func (w *UnitWriter) streamInputs(ctx context.Context, snapshot []byte, inputs func(yield func(model.UnitInput) error) error) error {
	hasher := model.NewUnitInputHasher()
	batch := make([]model.UnitInput, 0, w.s.opts.BatchRecords)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := w.s.write(ctx, func(tx *sql.Tx) error {
			check, err := tx.PrepareContext(ctx, `SELECT 1 FROM snapshot_files WHERE snapshot_id = ? AND file_id = ? AND content_hash = ? AND executable = ? AND status <> 'deleted'`)
			if err != nil {
				return wrap("snapshot_files", err)
			}
			defer check.Close()
			ins, err := tx.PrepareContext(ctx, `INSERT INTO unit_inputs(unit_id, file_id, content_hash, executable) VALUES(?, ?, ?, ?)`)
			if err != nil {
				return wrap("unit_inputs", err)
			}
			defer ins.Close()
			for _, in := range batch {
				fileRaw, _ := model.DecodeID(string(in.FileID))
				hashRaw, _ := model.DecodeID(in.ContentHash)
				var one int
				if err := check.QueryRowContext(ctx, snapshot, fileRaw, hashRaw, boolInt(in.Executable)).Scan(&one); err != nil {
					if isNoRows(err) {
						return &model.Error{Code: model.CodeSnapshotChanged,
							Message: "unit input is not the selected snapshot's version of that file", Details: map[string]string{"file_id": string(in.FileID)}}
					}
					return wrap("snapshot_files", err)
				}
				if _, err := ins.ExecContext(ctx, w.rowID, fileRaw, hashRaw, boolInt(in.Executable)); err != nil {
					return wrap("unit_inputs", err)
				}
			}
			return nil
		})
		batch = batch[:0]
		return err
	}
	err := inputs(func(in model.UnitInput) error {
		if err := hasher.Add(in); err != nil {
			return err
		}
		batch = append(batch, in)
		if len(batch) == w.s.opts.BatchRecords {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return err
	}
	if got := hasher.Sum(); got != w.build.Spec.InputHash {
		return invalid("unit_spec.input_hash %q does not match the streamed inputs (%q)", w.build.Spec.InputHash, got)
	}
	return nil
}

// checkBatch enforces the record and byte caps on one handed-off batch. An
// oversized batch is a typed limit failure, not a bypass (Section 11.1).
func (s *Store) checkBatch(records int, bytes int64) error {
	if records > s.opts.BatchRecords || bytes > s.opts.BatchBytes {
		return &model.Error{Code: model.CodeResourceLimit,
			Message: fmt.Sprintf("batch of %d records / %d bytes exceeds the configured %d records / %d bytes", records, bytes, s.opts.BatchRecords, s.opts.BatchBytes)}
	}
	return nil
}

const recordOverhead = 128

func evidenceBytes(list []model.Evidence) int64 {
	var n int64
	for _, e := range list {
		n += recordOverhead + int64(len(e.NativeKey)+len(e.Detail))
	}
	return n
}

// checkEvidence enforces that every evidence row belongs to this unit and its
// run: identity is producer-owned, provenance is not.
func (w *UnitWriter) checkEvidence(list []model.Evidence) error {
	for _, e := range list {
		if e.UnitID != w.build.Spec.ID || e.ProviderID != w.build.Spec.ProviderID ||
			e.ProviderVersion != w.build.Spec.ProviderVersion || e.OriginRunID != w.build.OriginRunID {
			return &model.Error{Code: model.CodeProviderOutputInvalid,
				Message: "evidence names another unit, provider or run than the one being written"}
		}
	}
	return nil
}

// PutNodes stores node facts with their evidence, recording no delta keys.
func (w *UnitWriter) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	return w.PutKeyedNodes(ctx, facts, nil)
}

// PutKeyedNodes stores node facts with their evidence in one transaction. Each
// fact's identity is recomputed from the repository, its kind and its
// canonical key (Section 9.1) and registered in the node_ids dictionary before
// the fact row references it; a fact whose ID does not derive, or whose kind
// disagrees with the identity already registered under that ID, is malformed
// provider output.
//
// keys is either empty or parallel to facts: keys[i] is every producer key
// backing facts[i] — the keys its next incremental run will classify as
// changed, unchanged or removed (Section 11.4) — sorted, without duplicates,
// each a lowercase hex digest. A fact is backed by more than one key whenever
// the producer's keys are finer than the identities they resolve to, which is
// ordinary: a canonical edge is published once and derived from N occurrences.
// Storage never derives or interprets a key; it records every one so CarryOver
// can drop the fact when ANY of them is replaced, which is the same condition
// the producer re-emits the whole fact on.
//
// A repeated node identity within one unit is not an error while it is the
// same fact: the same declaration can be observed by more than one worker, by
// two parts of a subdivided unit or, under a delta, by both the fresh import
// and the carried predecessor, and its keys accumulate. A repeat whose stored
// columns DIVERGE from the row already written is refused
// (CTX_PROVIDER_OUTPUT_INVALID) naming the node and the first differing
// column: the row would otherwise keep whichever description arrived first,
// and a delta-built unit would keep a different one from a full re-import of
// the same source. Nothing of a refused fact is stored, its evidence included.
func (w *UnitWriter) PutKeyedNodes(ctx context.Context, facts []model.NodeFact, keys [][]string) error {
	if err := checkFactKeys(len(facts), keys); err != nil {
		return err
	}
	if w.done {
		return conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	var bytes int64
	for _, f := range facts {
		if err := f.Validate(); err != nil {
			return err
		}
		if f.Node.SemanticSource == model.SemanticLSP {
			return &model.Error{Code: model.CodeProviderOutputInvalid, Message: "an lsp overlay node is never a canonical fact"}
		}
		if model.NewNodeID(w.repo, f.Node.Kind, f.CanonicalKey) != f.Node.ID {
			return &model.Error{Code: model.CodeProviderOutputInvalid,
				Message: "node id does not derive from its repository, kind and canonical key", Details: map[string]string{"node_id": string(f.Node.ID)}}
		}
		if err := w.checkEvidence(f.Evidence); err != nil {
			return err
		}
		n := f.Node
		bytes += recordOverhead + int64(len(n.Name)+len(n.QualifiedName)+len(n.Signature)+len(n.Language)+len(n.Metadata)+len(f.CanonicalKey)) + evidenceBytes(f.Evidence)
	}
	bytes += factKeyBytes(keys)
	if err := w.s.checkBatch(len(facts), bytes); err != nil {
		return err
	}
	return w.s.write(ctx, func(tx *sql.Tx) error {
		return w.providerWrite(func() error {
			ids, err := tx.PrepareContext(ctx, `INSERT INTO node_ids(id, repository_id, kind, canonical_key) VALUES(?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`)
			if err != nil {
				return err
			}
			defer ids.Close()
			kindOf, err := tx.PrepareContext(ctx, `SELECT kind FROM node_ids WHERE id = ?`)
			if err != nil {
				return err
			}
			defer kindOf.Close()
			ins, err := tx.PrepareContext(ctx, `INSERT INTO node_facts(unit_id, node_id, language, name, qualified_name, signature, file_id, start_byte, end_byte, metadata_json)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(unit_id, node_id) DO NOTHING`)
			if err != nil {
				return err
			}
			defer ins.Close()
			putKey, err := w.prepareFactKeys(ctx, tx)
			if err != nil {
				return err
			}
			defer putKey.Close()
			for i, f := range facts {
				n := f.Node
				nodeRaw, _ := model.DecodeID(string(n.ID))
				if _, err := ids.ExecContext(ctx, nodeRaw, w.repoRaw, string(n.Kind), f.CanonicalKey); err != nil {
					return err
				}
				var kind model.NodeKind
				if err := kindOf.QueryRowContext(ctx, nodeRaw).Scan(&kind); err != nil {
					return err
				}
				if kind != n.Kind {
					return &model.Error{Code: model.CodeProviderOutputInvalid,
						Message: "node kind disagrees with the identity already registered under this id", Details: map[string]string{"node_id": string(n.ID)}}
				}
				fileRaw, err := w.inputFile(ctx, tx, n.FileID, n.ContentHash)
				if err != nil {
					return err
				}
				start, end := rangeBytes(n.Range)
				metadata := "{}"
				if len(n.Metadata) > 0 {
					if err := requireJSONObject("node.metadata", n.Metadata); err != nil {
						return err
					}
					metadata = string(n.Metadata)
				}
				res, err := ins.ExecContext(ctx, w.rowID, nodeRaw, n.Language, n.Name, n.QualifiedName, n.Signature, fileRaw, start, end, metadata)
				if err != nil {
					return err
				}
				if err := w.checkRepeatedNode(ctx, tx, res, n, nodeRaw,
					[]string{n.Language, n.Name, n.QualifiedName, n.Signature, blobText(fileRaw), intText(start), intText(end), metadata}); err != nil {
					return err
				}
				if err := w.storeFactKeys(ctx, putKey, nodeRaw, nil, keysAt(keys, i)); err != nil {
					return err
				}
				if err := w.insertEvidence(ctx, tx, f.Evidence); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// PutRelations stores canonical edges with their occurrence evidence,
// recording no delta keys.
func (w *UnitWriter) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	return w.PutKeyedRelations(ctx, facts, nil)
}

// PutKeyedRelations stores canonical edges with their occurrence evidence.
// Relation identity is recomputed; both endpoints must already be registered
// identities. keys carries every producer key backing each fact, exactly as
// PutKeyedNodes documents. A relation_facts row stores nothing but its
// identity, so a repeat cannot diverge the way a node fact can; it adds its
// keys to the ones already recorded and is otherwise a no-op.
func (w *UnitWriter) PutKeyedRelations(ctx context.Context, facts []model.RelationFact, keys [][]string) error {
	if err := checkFactKeys(len(facts), keys); err != nil {
		return err
	}
	if w.done {
		return conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	var bytes int64
	for _, f := range facts {
		if err := f.Validate(); err != nil {
			return err
		}
		r := f.Relation
		if model.NewRelationID(w.repo, r.From, r.Kind, r.To) != r.ID {
			return &model.Error{Code: model.CodeProviderOutputInvalid, Message: "relation id does not derive from its endpoints and kind"}
		}
		if err := w.checkEvidence(f.Evidence); err != nil {
			return err
		}
		bytes += recordOverhead + evidenceBytes(f.Evidence)
	}
	bytes += factKeyBytes(keys)
	if err := w.s.checkBatch(len(facts), bytes); err != nil {
		return err
	}
	return w.s.write(ctx, func(tx *sql.Tx) error {
		return w.providerWrite(func() error {
			ids, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO relation_ids(id, repository_id, from_node_id, kind, to_node_id) VALUES(?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer ids.Close()
			ins, err := tx.PrepareContext(ctx, `INSERT INTO relation_facts(unit_id, relation_id) VALUES(?, ?) ON CONFLICT(unit_id, relation_id) DO NOTHING`)
			if err != nil {
				return err
			}
			defer ins.Close()
			putKey, err := w.prepareFactKeys(ctx, tx)
			if err != nil {
				return err
			}
			defer putKey.Close()
			for i, f := range facts {
				r := f.Relation
				relRaw, _ := model.DecodeID(string(r.ID))
				fromRaw, _ := model.DecodeID(string(r.From))
				toRaw, _ := model.DecodeID(string(r.To))
				if _, err := ids.ExecContext(ctx, relRaw, w.repoRaw, fromRaw, string(r.Kind), toRaw); err != nil {
					return err
				}
				if _, err := ins.ExecContext(ctx, w.rowID, relRaw); err != nil {
					return err
				}
				if err := w.storeFactKeys(ctx, putKey, nil, relRaw, keysAt(keys, i)); err != nil {
					return err
				}
				if err := w.insertEvidence(ctx, tx, f.Evidence); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// PutAliases stores scoped native aliases. The target must be a registered
// identity now and a visible node in the unit/dependency closure at seal.
func (w *UnitWriter) PutAliases(ctx context.Context, aliases []model.NativeAlias) error {
	if w.done {
		return conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	var bytes int64
	for _, a := range aliases {
		if err := a.Validate(); err != nil {
			return err
		}
		bytes += recordOverhead + int64(len(a.ScopeKey)+len(a.NativeKey))
	}
	if err := w.s.checkBatch(len(aliases), bytes); err != nil {
		return err
	}
	return w.s.write(ctx, func(tx *sql.Tx) error {
		return w.providerWrite(func() error {
			stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO native_aliases(unit_id, scope_key, native_key, node_id) VALUES(?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, a := range aliases {
				nodeRaw, _ := model.DecodeID(string(a.NodeID))
				if _, err := stmt.ExecContext(ctx, w.rowID, a.ScopeKey, a.NativeKey, nodeRaw); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// PutSearchUnits stores lexical documents and their FTS index rows in the same
// transaction. The FTS insert is explicit: the external-content table is never
// maintained by cascade or by row counts (Section 12.2).
//
// token_count is computed here with the index's own unicode61 tokenizer over
// exactly the columns search_fts indexes, so the Section 12.4 length
// statistics agree with the index. SearchUnit.TokenCount is informational on
// input and ignored; on read it is the stored, authoritative value.
func (w *UnitWriter) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	if w.done {
		return conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	var bytes int64
	for _, d := range docs {
		if err := d.Validate(); err != nil {
			return err
		}
		bytes += recordOverhead + int64(len(d.Path)+len(d.Name)+len(d.QualifiedName)+len(d.Signature)+len(d.Body))
	}
	if err := w.s.checkBatch(len(docs), bytes); err != nil {
		return err
	}
	tokenCounts, err := w.s.countTokens(ctx, docs)
	if err != nil {
		return err
	}
	var inserted int64
	err = w.s.write(ctx, func(tx *sql.Tx) error {
		return w.providerWrite(func() error {
			content, err := tx.PrepareContext(ctx, `INSERT INTO search_units(unit_id, search_key, node_id, file_id, path, kind, name, qualified_name, signature, start_byte, end_byte, body, token_count)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer content.Close()
			index, err := tx.PrepareContext(ctx, `INSERT INTO search_fts(rowid, name, qualified_name, signature, path, body) VALUES(?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer index.Close()
			for i, d := range docs {
				keyRaw, _ := model.DecodeID(d.ID)
				fileRaw, _ := model.DecodeID(string(d.FileID))
				nodeRaw, _ := optionalBlob("search_unit.node_id", string(d.NodeID))
				res, err := content.ExecContext(ctx, w.rowID, keyRaw, nodeRaw, fileRaw, d.Path, string(d.Kind), d.Name, d.QualifiedName, d.Signature,
					int64(d.Bytes.Start), int64(d.Bytes.End), d.Body, tokenCounts[i])
				if err != nil {
					return err
				}
				rowid, err := res.LastInsertId()
				if err != nil {
					return err
				}
				if _, err := index.ExecContext(ctx, rowid, d.Name, d.QualifiedName, d.Signature, d.Path, d.Body); err != nil {
					return err
				}
				inserted++
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	w.ftsDocs += inserted
	return nil
}

// providerWrite maps a constraint failure inside a provider batch to
// CTX_PROVIDER_OUTPUT_INVALID: an unregistered endpoint, an input file the
// unit did not declare or a duplicated fact is malformed provider output, not
// a caller argument error.
func (w *UnitWriter) providerWrite(fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	wrapped := wrap("unit write", err)
	if typed, ok := wrapped.(*model.Error); ok && typed.Code == model.CodeArgumentInvalid {
		return &model.Error{Code: model.CodeProviderOutputInvalid, Message: typed.Message,
			Details: map[string]string{"unit_id": string(w.build.Spec.ID)}}
	}
	return wrapped
}

// inputFile resolves an optional file reference to its unit_inputs row and
// checks the asserted content hash against the declared input.
func (w *UnitWriter) inputFile(ctx context.Context, tx *sql.Tx, file model.FileID, contentHash string) (any, error) {
	if file == "" {
		return nil, nil
	}
	fileRaw, _ := model.DecodeID(string(file))
	var stored []byte
	err := tx.QueryRowContext(ctx, `SELECT content_hash FROM unit_inputs WHERE unit_id = ? AND file_id = ?`, w.rowID, fileRaw).Scan(&stored)
	if isNoRows(err) {
		return nil, &model.Error{Code: model.CodeProviderOutputInvalid,
			Message: "fact names a file the unit did not declare as an input", Details: map[string]string{"file_id": string(file)}}
	}
	if err != nil {
		return nil, wrap("unit_inputs", err)
	}
	if contentHash != "" && idHex(stored) != contentHash {
		return nil, &model.Error{Code: model.CodeSourceBindingUnverified,
			Message: "fact asserts a content hash that is not the unit's declared input for that file", Details: map[string]string{"file_id": string(file)}}
	}
	return fileRaw, nil
}

func (w *UnitWriter) insertEvidence(ctx context.Context, tx *sql.Tx, list []model.Evidence) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO evidence(id, unit_id, node_id, relation_id, precision, file_id, start_byte, end_byte, native_key, detail, content_hash_bound)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range list {
		idRaw, _ := model.DecodeID(string(e.ID))
		nodeRaw, _ := optionalBlob("evidence.node_id", string(e.NodeID))
		relRaw, _ := optionalBlob("evidence.relation_id", string(e.RelationID))
		fileRaw, err := w.inputFile(ctx, tx, e.FileID, e.ContentHash)
		if err != nil {
			return err
		}
		start, end := rangeBytes(e.Range)
		if _, err := stmt.ExecContext(ctx, idRaw, w.rowID, nodeRaw, relRaw, string(e.Precision), fileRaw, start, end, e.NativeKey, e.Detail,
			boolInt(e.ContentHash != "")); err != nil {
			return err
		}
	}
	return nil
}

// canonicalDetails renders a capability's diagnostic pairs as the stored JSON
// object. Keys are emitted in ascending order (encoding/json sorts map keys),
// so the text a generation stores, and therefore the digest folded into its
// AnalysisKey, is a function of the pairs and never of the order a publisher
// happened to add them. An empty map is the empty object, so a state with no
// details hashes identically whether Details was nil or allocated.
func canonicalDetails(details map[string]string) (string, error) {
	if len(details) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return "", internal("capability details: " + err.Error())
	}
	return string(raw), nil
}

// checkFactKeys enforces the shape of a keyed batch: either no fact carries
// keys or every fact carries at least one, each a lowercase hex digest, sorted
// and without duplicates. A partly keyed batch would silently leave rows a
// later delta could neither replace nor remove; an unsorted or duplicated key
// list is producer state storage cannot repair, and the sorted order is what
// makes the producer's own key set and the stored one comparable.
func checkFactKeys(facts int, keys [][]string) error {
	if len(keys) == 0 {
		return nil
	}
	if len(keys) != facts {
		return invalid("a keyed batch carries %d key lists for %d facts", len(keys), facts)
	}
	for i, list := range keys {
		if len(list) == 0 {
			return invalid("%s is empty; a keyed batch keys every fact or none", indexedField("keys", i))
		}
		for j, k := range list {
			if !model.ValidHexID(k) {
				return invalid("%s is not a %d-character lowercase hex digest", indexedField(indexedField("keys", i), j), model.IDHexLen)
			}
			if j > 0 && k <= list[j-1] {
				return invalid("%s is not sorted or repeats a key", indexedField("keys", i))
			}
		}
	}
	return nil
}

// factKeyBytes is the retained-byte contribution of a batch's keys.
func factKeyBytes(keys [][]string) int64 {
	var n int64
	for _, list := range keys {
		for _, k := range list {
			n += int64(len(k))
		}
	}
	return n
}

func keysAt(keys [][]string, i int) []string {
	if i < len(keys) {
		return keys[i]
	}
	return nil
}

func indexedField(name string, i int) string { return name + "[" + strconv.Itoa(i) + "]" }

// prepareFactKeys prepares the fact_keys insert. A key already recorded for
// this fact is not an error: two workers, two parts of a subdivided unit or a
// carried predecessor can publish the same fact under the same keys.
func (w *UnitWriter) prepareFactKeys(ctx context.Context, tx *sql.Tx) (*sql.Stmt, error) {
	return tx.PrepareContext(ctx, `INSERT INTO fact_keys(unit_id, node_id, relation_id, fact_key) VALUES(?, ?, ?, ?) ON CONFLICT DO NOTHING`)
}

func (w *UnitWriter) storeFactKeys(ctx context.Context, stmt *sql.Stmt, node, relation any, keys []string) error {
	for _, k := range keys {
		if _, err := stmt.ExecContext(ctx, w.rowID, node, relation, k); err != nil {
			return err
		}
	}
	return nil
}

// nodeFactColumns names the stored columns of a node fact, in the order
// checkRepeatedNode compares them, so the column it reports is a function of
// the divergence and not of map order.
var nodeFactColumns = []string{"language", "name", "qualified_name", "signature", "file_id", "start_byte", "end_byte", "metadata_json"}

// checkRepeatedNode refuses a repeated node identity whose stored columns
// differ from the row already written for it. The insert yields to the row
// that is present, so without this check the unit would silently keep whichever
// description arrived first and discard the rest — and because CarryOver runs
// after the provider's own batches, a delta-built unit would keep the fresh
// description where a full re-import keeps the first-written one. Two units
// over the same source would then hold different rows, which is the one thing
// Section 11.4 requires a delta not to do. An identical repeat costs one
// comparison and is free.
func (w *UnitWriter) checkRepeatedNode(ctx context.Context, tx *sql.Tx, res sql.Result, n model.Node, raw []byte, incoming []string) error {
	affected, err := res.RowsAffected()
	if err != nil || affected != 0 {
		return err
	}
	var language, name, qualified, signature, metadata string
	var file []byte
	var start, end sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT language, name, qualified_name, signature, file_id, start_byte, end_byte, metadata_json
		FROM node_facts WHERE unit_id = ? AND node_id = ?`, w.rowID, raw).Scan(&language, &name, &qualified, &signature, &file, &start, &end, &metadata); err != nil {
		return err
	}
	stored := []string{language, name, qualified, signature, optionalHex(file), nullIntText(start), nullIntText(end), metadata}
	for i, col := range nodeFactColumns {
		if stored[i] == incoming[i] {
			continue
		}
		return &model.Error{Code: model.CodeProviderOutputInvalid,
			Message:     "a repeated node identity describes the fact differently than the row already written",
			Details:     map[string]string{"node_id": string(n.ID), "column": col, "stored": stored[i], "published": incoming[i]},
			Remediation: "publish one description per node identity; a repeat must be identical to the fact already published"}
	}
	return nil
}

// blobText and intText render the values bound to an insert so they compare
// against the columns read back.
func blobText(v any) string {
	raw, ok := v.([]byte)
	if !ok {
		return ""
	}
	return idHex(raw)
}

func intText(v any) string {
	n, ok := v.(int64)
	if !ok {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func nullIntText(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func rangeBytes(r *model.SourceRange) (start, end any) {
	if r == nil {
		return nil, nil
	}
	return int64(r.Start.Byte), int64(r.End.Byte)
}

// requireJSONObject enforces the strict typed shape of a JSON column: an
// object, not an arbitrary value.
func requireJSONObject(field string, raw []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return &model.Error{Code: model.CodeProviderOutputInvalid, Message: field + " must be a JSON object"}
	}
	return nil
}

// Fail discards a building unit and every row it wrote. Unsealed output is
// deleted, never quarantined into visibility.
func (w *UnitWriter) Fail(ctx context.Context) error {
	if w.done {
		return nil
	}
	w.done = true
	return w.s.write(ctx, func(tx *sql.Tx) error {
		var state model.UnitState
		err := tx.QueryRowContext(ctx, `SELECT state FROM units WHERE id = ?`, w.rowID).Scan(&state)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return wrap("units", err)
		}
		if state != model.UnitBuilding {
			return conflict("unit %s is %s and cannot be failed", w.build.Spec.ID, state)
		}
		return w.s.deleteUnit(ctx, tx, w.rowID)
	})
}

// closureCTE selects the unit row and every transitive dependency.
const closureCTE = `WITH RECURSIVE closure(id) AS (
	SELECT ?1 UNION SELECT ud.dependency_id FROM unit_dependencies ud JOIN closure c ON ud.unit_id = c.id)`

// SealUnit validates the whole unit and makes it immutable and a member of the
// generation it was begun for (Section 12.2 invariants beyond DDL): evidence
// behind every fact, ranges inside their source, relation endpoints and alias
// targets visible through the unit/dependency closure, an acyclic dependency
// graph, and the exact FTS document count its transactions emitted.
func (s *Store) SealUnit(ctx context.Context, w *UnitWriter) error {
	if w.done {
		return conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := w.clipEvidence(ctx, tx); err != nil {
			return err
		}
		checks := []struct {
			what  string
			query string
		}{
			{"node facts without evidence", `SELECT count(*) FROM node_facts nf WHERE nf.unit_id = ?1
				AND NOT EXISTS (SELECT 1 FROM evidence e WHERE e.unit_id = nf.unit_id AND e.node_id = nf.node_id)`},
			{"relation facts without evidence", `SELECT count(*) FROM relation_facts rf WHERE rf.unit_id = ?1
				AND NOT EXISTS (SELECT 1 FROM evidence e WHERE e.unit_id = rf.unit_id AND e.relation_id = rf.relation_id)`},
			{"node ranges past the end of their source", `SELECT count(*) FROM node_facts nf JOIN unit_inputs ui ON ui.unit_id = nf.unit_id AND ui.file_id = nf.file_id
				JOIN blobs b ON b.hash = ui.content_hash WHERE nf.unit_id = ?1 AND nf.end_byte > b.size_bytes`},
			{"evidence ranges past the end of their source", `SELECT count(*) FROM evidence e JOIN unit_inputs ui ON ui.unit_id = e.unit_id AND ui.file_id = e.file_id
				JOIN blobs b ON b.hash = ui.content_hash WHERE e.unit_id = ?1 AND e.end_byte > b.size_bytes`},
			{"search documents past the end of their source", `SELECT count(*) FROM search_units su JOIN unit_inputs ui ON ui.unit_id = su.unit_id AND ui.file_id = su.file_id
				JOIN blobs b ON b.hash = ui.content_hash WHERE su.unit_id = ?1 AND su.end_byte > b.size_bytes`},
			{"relation endpoints with no visible node fact", closureCTE + ` SELECT count(*) FROM relation_facts rf JOIN relation_ids ri ON ri.id = rf.relation_id
				WHERE rf.unit_id = ?1 AND (
					NOT EXISTS (SELECT 1 FROM node_facts nf WHERE nf.node_id = ri.from_node_id AND nf.unit_id IN (SELECT id FROM closure))
				 OR NOT EXISTS (SELECT 1 FROM node_facts nf WHERE nf.node_id = ri.to_node_id AND nf.unit_id IN (SELECT id FROM closure)))`},
			{"alias targets with no visible node fact", closureCTE + ` SELECT count(*) FROM native_aliases na WHERE na.unit_id = ?1
				AND NOT EXISTS (SELECT 1 FROM node_facts nf WHERE nf.node_id = na.node_id AND nf.unit_id IN (SELECT id FROM closure))`},
			{"dependency cycles", `SELECT count(*) FROM unit_dependencies ud WHERE ud.dependency_id = ?1 AND ud.unit_id IN (` +
				`WITH RECURSIVE closure(id) AS (SELECT ?1 UNION SELECT d.dependency_id FROM unit_dependencies d JOIN closure c ON d.unit_id = c.id) SELECT id FROM closure)`},
		}
		for _, c := range checks {
			var n int64
			if err := tx.QueryRowContext(ctx, c.query, w.rowID).Scan(&n); err != nil {
				return wrap(c.what, err)
			}
			if n != 0 {
				return &model.Error{Code: model.CodeProviderOutputInvalid,
					Message: fmt.Sprintf("unit cannot seal: %d %s", n, c.what), Details: map[string]string{"unit_id": string(w.build.Spec.ID)}}
			}
		}
		var docs int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM search_units WHERE unit_id = ?`, w.rowID).Scan(&docs); err != nil {
			return wrap("search_units", err)
		}
		if docs != w.ftsDocs {
			return corrupt("unit %s has %d search documents but emitted %d index rows", w.build.Spec.ID, docs, w.ftsDocs)
		}
		if err := exec1(ctx, tx, conflict("unit %s is no longer building", w.build.Spec.ID),
			`UPDATE units SET state = ? WHERE id = ? AND state = ?`, string(model.UnitSealed), w.rowID, string(model.UnitBuilding)); err != nil {
			return err
		}
		return s.attach(ctx, tx, w.gen, w.rowID, w.build.Spec.ProviderID, w.build.Spec.ScopeKey, false, Carry{})
	})
	if err == nil {
		w.done = true
		if w.evidenceClipped != 0 {
			// One bounded line per sealed unit, and only when the bound
			// actually truncated something: the clip is legitimate but it
			// silently drops occurrences a caller may be counting on, so the
			// unit and the count are on the record. No source, no native keys.
			slog.Warn("evidence occurrences were dropped to hold the per-fact bound",
				"unit_id", string(w.build.Spec.ID), "dropped", w.evidenceClipped, "limit", model.MaxEvidencePerFact)
		}
	}
	return err
}

// attach inserts the generation membership row for a unit. carried and its
// distance are zero for every fresh or reused member; only AttachCarried
// passes a nonzero carry (Section 13.3).
func (s *Store) attach(ctx context.Context, tx *sql.Tx, gen model.GenerationID, unitRow int64, providerID, scopeKey string, carried bool, c Carry) error {
	if _, err := s.generationRow(ctx, tx, gen, model.GenerationStaging); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO generation_units(generation_id, provider_id, scope_key, unit_id, carried, distance_generations, distance_files)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, int64(gen), providerID, scopeKey, unitRow, boolInt(carried), c.DistanceGenerations, c.DistanceFiles)
	if err != nil {
		wrapped := wrap("generation_units", err)
		if typed, ok := wrapped.(*model.Error); ok && typed.Code == model.CodeArgumentInvalid {
			return conflict("generation %d already selects a unit for provider %q scope %q", gen, providerID, scopeKey)
		}
		return wrapped
	}
	return nil
}

// AttachUnit reuses a sealed unit in gen. Reuse is admitted only when every
// declared input is exactly the selected snapshot's version of that file
// (content hash and mode); otherwise CTX_SNAPSHOT_CHANGED. Membership of the
// unit's dependencies is validated at Activate.
func (s *Store) AttachUnit(ctx context.Context, gen model.GenerationID, unit model.UnitID) error {
	key, err := idBlob("unit_id", string(unit))
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		g, err := s.generationRow(ctx, tx, gen, model.GenerationStaging)
		if err != nil {
			return err
		}
		var row int64
		var state model.UnitState
		var providerID, scopeKey string
		err = tx.QueryRowContext(ctx, `SELECT id, state, provider_id, scope_key FROM units WHERE unit_key = ?`, key).Scan(&row, &state, &providerID, &scopeKey)
		if isNoRows(err) {
			return invalid("unit %s does not exist", unit)
		}
		if err != nil {
			return wrap("units", err)
		}
		if state != model.UnitSealed {
			return conflict("unit %s is %s; only sealed units can be reused", unit, state)
		}
		var mismatched int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM unit_inputs ui WHERE ui.unit_id = ? AND NOT EXISTS (
			SELECT 1 FROM snapshot_files sf WHERE sf.snapshot_id = ? AND sf.file_id = ui.file_id AND sf.content_hash = ui.content_hash
			AND sf.executable = ui.executable AND sf.status <> 'deleted')`, row, g.snapshot).Scan(&mismatched); err != nil {
			return wrap("unit_inputs", err)
		}
		if mismatched != 0 {
			return &model.Error{Code: model.CodeSnapshotChanged,
				Message: fmt.Sprintf("unit cannot be reused: %d of its inputs differ from the selected snapshot", mismatched),
				Details: map[string]string{"unit_id": string(unit)}}
		}
		return s.attach(ctx, tx, gen, row, providerID, scopeKey, false, Carry{})
	})
}

// Activate validates the frozen membership, computes the AnalysisKey and runs
// exactly the Section 12.3 pointer transaction. expectedActive is the
// generation the caller believes is published (zero: none); it is compared
// with active_generations inside the same immediate transaction and a
// mismatch is CTX_VERSION_CONFLICT, so a coordinator never publishes over a
// pointer it has not seen. Any failure rolls back every status and pointer
// change.
func (s *Store) Activate(ctx context.Context, gen, expectedActive model.GenerationID, health model.GenerationHealth,
	capabilities []model.CapabilityState, normalizationVersion string) (model.Binding, error) {
	if !health.Valid() {
		return model.Binding{}, invalid("generation health %q is not a known health", health)
	}
	if normalizationVersion == "" || len(normalizationVersion) > model.MaxIdentifierBytes {
		return model.Binding{}, invalid("normalization version is required and bounded to %d bytes", model.MaxIdentifierBytes)
	}
	if len(capabilities) > model.MaxCapabilityStates {
		return model.Binding{}, invalid("capability report has %d entries, limit %d", len(capabilities), model.MaxCapabilityStates)
	}
	for _, c := range capabilities {
		if err := c.Validate(); err != nil {
			return model.Binding{}, err
		}
	}
	var binding model.Binding
	err := s.write(ctx, func(tx *sql.Tx) error {
		g, err := s.generationRow(ctx, tx, gen, model.GenerationStaging)
		if err != nil {
			return err
		}
		var current int64
		if err := activeGeneration(ctx, tx, g.repo, &current); err != nil {
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeNoActiveGeneration {
				return err
			}
		}
		if current != int64(expectedActive) {
			return &model.Error{Code: model.CodeVersionConflict,
				Message:     fmt.Sprintf("active generation is %d, not the expected %d; another activation intervened", current, expectedActive),
				Remediation: "re-read the active generation and decide again whether to publish"}
		}
		checks := []struct {
			what  string
			query string
		}{
			{"members that are not sealed", `SELECT count(*) FROM generation_units gu JOIN units u ON u.id = gu.unit_id WHERE gu.generation_id = ?1 AND u.state <> 'sealed'`},
			{"member dependencies that are not members", `SELECT count(*) FROM generation_units gu JOIN unit_dependencies ud ON ud.unit_id = gu.unit_id
				WHERE gu.generation_id = ?1 AND NOT EXISTS (SELECT 1 FROM generation_units g2 WHERE g2.generation_id = ?1 AND g2.unit_id = ud.dependency_id)`},
			// A carried member is exempt from input equality and from it alone:
			// Section 13.3 carries the previous sealed unit precisely because
			// its inputs no longer match the snapshot. What still holds for it
			// is that its inputs exist — a unit whose files were deleted is
			// never carried — so the carried rows are checked for existence
			// instead, and both checks run over the frozen membership.
			//
			// Neither check is reachable from the store's own writers, and no
			// test guards either: AttachUnit and AttachCarried apply the same
			// two predicates against the same generation's snapshot, and a
			// snapshot's snapshot_files rows are immutable once PutSnapshot
			// returns, so the set cannot move between attach and activation.
			// They are defence in depth over the one moment that publishes
			// facts to every reader, and they are deliberately symmetric: do
			// not "simplify" either away believing a test protects it, and do
			// not delete one without the other.
			{"member inputs that differ from the snapshot", `SELECT count(*) FROM generation_units gu JOIN unit_inputs ui ON ui.unit_id = gu.unit_id
				WHERE gu.generation_id = ?1 AND gu.carried = 0 AND NOT EXISTS (SELECT 1 FROM snapshot_files sf JOIN generations g ON g.snapshot_id = sf.snapshot_id
				WHERE g.id = ?1 AND sf.file_id = ui.file_id AND sf.content_hash = ui.content_hash AND sf.executable = ui.executable AND sf.status <> 'deleted')`},
			{"carried member inputs that no longer exist in the snapshot", `SELECT count(*) FROM generation_units gu JOIN unit_inputs ui ON ui.unit_id = gu.unit_id
				WHERE gu.generation_id = ?1 AND gu.carried = 1 AND NOT EXISTS (SELECT 1 FROM snapshot_files sf JOIN generations g ON g.snapshot_id = sf.snapshot_id
				WHERE g.id = ?1 AND sf.file_id = ui.file_id AND sf.status <> 'deleted')`},
			{"relation endpoints with no visible node fact", `SELECT count(*) FROM generation_units gu JOIN relation_facts rf ON rf.unit_id = gu.unit_id
				JOIN relation_ids ri ON ri.id = rf.relation_id WHERE gu.generation_id = ?1 AND (
				NOT EXISTS (SELECT 1 FROM node_facts nf JOIN generation_units g2 ON g2.unit_id = nf.unit_id WHERE g2.generation_id = ?1 AND nf.node_id = ri.from_node_id)
				OR NOT EXISTS (SELECT 1 FROM node_facts nf JOIN generation_units g2 ON g2.unit_id = nf.unit_id WHERE g2.generation_id = ?1 AND nf.node_id = ri.to_node_id))`},
		}
		for _, c := range checks {
			var n int64
			if err := tx.QueryRowContext(ctx, c.query, g.id).Scan(&n); err != nil {
				return wrap(c.what, err)
			}
			if n != 0 {
				return &model.Error{Code: model.CodeProviderOutputInvalid,
					Message: fmt.Sprintf("generation %d cannot activate: %d %s", gen, n, c.what)}
			}
		}
		var members int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM generation_units WHERE generation_id = ?`, g.id).Scan(&members); err != nil {
			return wrap("generation_units", err)
		}
		if members == 0 {
			return conflict("generation %d has no units; nothing to publish", gen)
		}
		var violations int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check('generation_units')`).Scan(&violations); err != nil {
			return wrap("foreign_key_check", err)
		}
		if violations != 0 {
			return corrupt("generation_units has %d foreign key violations", violations)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM generation_capabilities WHERE generation_id = ?`, g.id); err != nil {
			return wrap("generation_capabilities", err)
		}
		capStmt, err := tx.PrepareContext(ctx, `INSERT INTO generation_capabilities(generation_id, provider_id, capability, scope_key, state, diagnostic_code, details_json)
			VALUES(?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return wrap("generation_capabilities", err)
		}
		defer capStmt.Close()
		for _, c := range capabilities {
			details, err := canonicalDetails(c.Details)
			if err != nil {
				return err
			}
			if _, err := capStmt.ExecContext(ctx, g.id, c.ProviderID, c.Capability, c.Scope, string(c.State), c.DiagnosticCode, details); err != nil {
				return wrap("generation_capabilities", err)
			}
		}

		// The membership digest folds each member's staleness with its key.
		// Folding the key alone would make a generation that carries a stale
		// unit byte-identical to one that rebuilt it over the same snapshot,
		// so the two would share an AnalysisKey and a stale answer could not
		// be told from a fresh one (Section 20.2 determinism).
		membership := model.NewHasher(domainMembership)
		if err := foldColumn(ctx, tx, membership, `SELECT lower(hex(u.unit_key)) || char(0) || gu.carried || char(0) || gu.distance_generations || char(0) || gu.distance_files
			FROM generation_units gu JOIN units u ON u.id = gu.unit_id WHERE gu.generation_id = ? ORDER BY u.unit_key`, g.id); err != nil {
			return err
		}
		capsHash := model.NewHasher(domainCapabilities)
		if err := foldColumn(ctx, tx, capsHash, `SELECT provider_id || char(0) || capability || char(0) || scope_key || char(0) || state || char(0) || diagnostic_code
			|| char(0) || details_json
			FROM generation_capabilities WHERE generation_id = ? ORDER BY provider_id, capability, scope_key`, g.id); err != nil {
			return err
		}
		snapshotID := model.SnapshotID(idHex(g.snapshot))
		key := model.NewAnalysisKey(snapshotID, Fingerprint, membership.Sum(), capsHash.Sum(), normalizationVersion, g.semantic)
		keyRaw, _ := model.DecodeID(string(key))
		if err := exec1(ctx, tx, conflict("generation %d left staging during activation", gen),
			`UPDATE generations SET analysis_key = ?, health = ? WHERE id = ? AND status = 'staging'`, keyRaw, string(health), g.id); err != nil {
			return err
		}

		// Section 12.3, verbatim in effect: exactly one row must flip to active.
		now := formatTime(time.Now())
		if err := exec1(ctx, tx, conflict("generation %d could not be activated: not staging or no analysis key", gen),
			`UPDATE generations SET status='active', activated_at=? WHERE id=? AND repository_id=? AND status='staging' AND analysis_key IS NOT NULL`,
			now, g.id, g.repo); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE generations SET status='superseded'
			WHERE id=(SELECT generation_id FROM active_generations WHERE repository_id=?) AND id<>?`, g.repo, g.id); err != nil {
			return wrap("supersede", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO active_generations(repository_id, generation_id) VALUES(?, ?)
			ON CONFLICT(repository_id) DO UPDATE SET generation_id=excluded.generation_id`, g.repo, g.id); err != nil {
			return wrap("active_generations", err)
		}
		binding = model.Binding{RepositoryID: model.RepositoryID(idHex(g.repo)), SnapshotID: snapshotID, GenerationID: gen, AnalysisKey: key}
		return nil
	})
	return binding, err
}

// foldColumn streams one text column into h; the query must already be sorted.
func foldColumn(ctx context.Context, tx *sql.Tx, h *model.Hasher, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return wrap("fold", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return wrap("fold", err)
		}
		h.AddString(v)
	}
	return wrap("fold", rows.Err())
}

// Abort fails a staging generation: its status and health become failed, its
// still-running provider runs are marked failed, and the unsealed units those
// runs were building are deleted. Sealed units stay reusable and the active
// pointer is untouched.
func (s *Store) Abort(ctx context.Context, gen model.GenerationID) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := exec1(ctx, tx, conflict("generation %d is not staging", gen),
			`UPDATE generations SET status = 'failed', health = 'failed' WHERE id = ? AND status = 'staging'`, int64(gen)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE provider_runs SET status = 'failed', completed_at = ? WHERE generation_id = ? AND status = 'running'`,
			formatTime(time.Now()), int64(gen))
		return wrap("provider_runs", err)
	})
	if err != nil {
		return err
	}
	return s.collectUnits(ctx, `SELECT u.id FROM units u JOIN provider_runs pr ON pr.id = u.origin_run_id WHERE pr.generation_id = ?1 AND u.state = 'building' LIMIT ?2`, int64(gen))
}

// ActiveGeneration returns the published generation for repo, or
// CTX_NO_ACTIVE_GENERATION.
func (s *Store) ActiveGeneration(ctx context.Context, repo model.RepositoryID) (model.GenerationID, error) {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return 0, err
	}
	var gen int64
	err = s.read(ctx, func(tx *sql.Tx) error {
		return activeGeneration(ctx, tx, raw, &gen)
	})
	return model.GenerationID(gen), err
}

func activeGeneration(ctx context.Context, tx *sql.Tx, repo []byte, gen *int64) error {
	err := tx.QueryRowContext(ctx, `SELECT generation_id FROM active_generations WHERE repository_id = ?`, repo).Scan(gen)
	if isNoRows(err) {
		return &model.Error{Code: model.CodeNoActiveGeneration, Message: "no generation has been published for this repository",
			Remediation: "run `codectx index`"}
	}
	return wrap("active_generations", err)
}

// GenerationStatus reads one generation's lifecycle status.
func (s *Store) GenerationStatus(ctx context.Context, gen model.GenerationID) (model.GenerationStatus, error) {
	var status model.GenerationStatus
	err := s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT status FROM generations WHERE id = ?`, int64(gen)).Scan(&status)
		if isNoRows(err) {
			return invalid("generation %d does not exist", gen)
		}
		return wrap("generations", err)
	})
	return status, err
}

// UnitOrigin reports the run that produced a retained unit. A reused unit keeps
// its original run; a rerun never rewrites it (Section 9.3).
func (s *Store) UnitOrigin(ctx context.Context, unit model.UnitID) (model.ProviderRunID, error) {
	key, err := idBlob("unit_id", string(unit))
	if err != nil {
		return "", err
	}
	var origin []byte
	err = s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT origin_run_id FROM units WHERE unit_key = ?`, key).Scan(&origin)
		if isNoRows(err) {
			return invalid("unit %s does not exist", unit)
		}
		return wrap("units", err)
	})
	return model.ProviderRunID(idHex(origin)), err
}
