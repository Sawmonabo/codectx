package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"iter"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Delta import (Section 11.4). A provider that can tell what changed since its
// last run re-emits only the changed facts; the rest of the previous unit is
// carried into the new one by CarryOver. Units stay immutable and
// self-contained: carry-over copies rows, it does not share them. Sharing would
// make a retired predecessor undeletable by its successor's lifetime, and every
// query, every membership check and the single deleteUnit procedure of Section
// 12.4 would have to learn about unit chains. A copy costs one bounded
// INSERT..SELECT per fact table and leaves all of that untouched.
//
// One thing is shared rather than copied, for the same reason node_ids is: a
// lexical document's FTS posting. search_fts is contentless (ADR-0003 §2.1),
// so the indexed text exists ONLY as postings -- no column of this database
// holds it, and it cannot be re-derived from the source either, because a
// producer's Body is not the file's bytes over [start_byte, end_byte)
// (treesitter publishes names, signature and documentation; the filesystem
// provider publishes none). A carried document therefore keeps the posting it
// was indexed into, by copying search_units.doc_id along with the rest of the
// row, and re-indexes nothing. deleteUnit releases a posting only when no
// surviving unit's row still names it, exactly as it releases a node id.
//
// Two retention granularities are supported because the two wave-A importers
// have two:
//
//   - Per path. A fact's retention bucket is the FileID on its evidence:
//     evidence with a FileID belongs to that path's bucket, evidence with none
//     belongs to the unit's single index-level bucket. The SCIP importer's
//     delta is per document, so its applier names the changed and removed
//     paths.
//   - Per fact key. The dependence importer's delta is per fact, and a
//     cross-file edge can disappear while every file holding its evidence is
//     unchanged, so a path bucket cannot express its removals. Its applier
//     names the changed and removed keys, which storage matches against the
//     fact_keys rows each producer supplied through
//     PutKeyedNodes/PutKeyedRelations. A fact is backed by every key that
//     produced it, and is dropped when ANY of them is replaced — the same
//     condition the producer re-emits the whole fact on, so the fresh and
//     carried sets partition exactly. Storage never derives or interprets a
//     key.
//
// Aliases have neither evidence nor a file column, so they are excluded by
// their scope key, which the applier already owns, and are otherwise carried
// only while the node they target is still a fact of the new unit — exactly
// the condition SealUnit enforces.

// deltaStatePart is the size of one stored part of a delta-state artifact.
// An artifact is written and read as a sequence of parts so that a manifest
// or key set the size of a large unit never sits whole in the process: the
// producer streams it from the file it was built in, and a refresh streams it
// back into one. A part is one row's blob, small enough to pass through the
// engine's bind buffer and the writer's cache without spilling on its own.
const deltaStatePart = 1 << 20

// Replaced names everything of the previous unit that the current import has
// already re-emitted or that no longer exists. Everything else is carried.
// Naming the complement rather than the survivors is deliberate: for both
// wave-A importers the replaced set is a handful of entries against tens of
// thousands of survivors.
//
// All three sets are streams, consumed once, and none is ever materialized
// here: the replaced set is small for the refreshes that motivate a delta, but
// a branch switch or a formatting pass names every document of the repository,
// and Section 6 forbids holding that list in the Go heap. A producer that
// cannot stream its set has a whole-repository list of its own, which is the
// same defect one level up.
//
// A producer's stream may read the store — the dependence applier's Files is a
// merge join against Store.UnitInputs — and it is drained from inside
// CarryOver's ingestion call, on the single writer connection. Such a stream
// sees the store as of the last commit: it takes a connection from the reader
// pool (read_connections, default 2) once per page of its own scan, and the
// ingestion group stays open across every one of those round-trips. What a
// stream reads is the predecessor unit, sealed and committed before this run
// began, so the last commit is the state it needs; a stream must never call a
// read that runs on the group's own connection, because the ingestion call
// draining it holds that connection and the two would wait on each other.
// WAL readers never wait on the writer, so the pool reads cannot deadlock; a
// saturated reader pool stalls the carry-over, and the open group blocks
// every other writer in the process for as long as the walk lasts. A producer
// whose stream reads the store must therefore keep those reads bounded and
// proportional to the unit it is replacing, and must not discover this
// nesting by measuring a stall.
type Replaced struct {
	// Files are the per-path evidence buckets the import replaced or dropped.
	// Each must be a valid FileID.
	Files iter.Seq[model.FileID]
	// Scopes are the alias scope keys the import replaced or dropped. Each
	// must be non-empty and within model.MaxScopeKeyBytes.
	Scopes iter.Seq[string]
	// Keys are the producer fact keys the import replaced or dropped.
	// Each key must be a lowercase hex digest (model.ValidHexID); anything
	// else is refused.
	Keys iter.Seq[string]
	// IndexLevel drops the previous unit's index-level bucket: the facts and
	// evidence that name no file. Providers that republish every unlocated
	// fact on every run set it; a provider that republishes them only for the
	// documents it reparsed must not, or edges carried from untouched
	// documents lose their endpoints and the unit cannot seal.
	IndexLevel bool
}

// CarryOverStats reports what a carry-over inherited.
// The Section 11.1 bound on evidence per fact is not applied here: SealUnit
// applies it to every unit, delta-built or not, so the two cannot diverge.
type CarryOverStats struct {
	Nodes       int64
	Relations   int64
	Aliases     int64
	SearchUnits int64
	Evidence    int64
}

// CarryOver inherits the sealed unit prev's facts into the unit being built,
// minus everything Replaced names. It must run after the provider's own
// batches and before SealUnit: every insert it issues yields to a row already
// present, so the fresh import always wins over its predecessor.
//
// The whole body is one ingestion call on the single writer connection, and
// draining Replaced's three streams happens inside it: a producer stream that
// reads the store runs nested in this call and on the reader pool, seeing the
// last commit (see Replaced).
//
// Carrying a row asserts that its source has not changed, so the whole call is
// refused unless every file the previous unit located facts in, and that
// Replaced does not name, is declared by this unit with the same content hash.
// A file that was edited but not named, or deleted from the snapshot, is
// therefore a typed refusal and never a stale fact.
func (w *UnitWriter) CarryOver(ctx context.Context, prev model.UnitID, replaced Replaced) (CarryOverStats, error) {
	if w.done {
		return CarryOverStats{}, conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	if prev == w.build.Spec.ID {
		return CarryOverStats{}, invalid("a unit cannot carry over from itself")
	}
	prevKey, err := idBlob("previous unit_id", string(prev))
	if err != nil {
		return CarryOverStats{}, err
	}
	var stats CarryOverStats
	err = w.s.ingest(ctx, func(tx *sql.Tx) error {
		prevRow, err := w.previousUnitRow(ctx, tx, prev, prevKey)
		if err != nil {
			return err
		}
		keyed, err := w.stageReplaced(ctx, tx, replaced)
		if err != nil {
			return err
		}
		if err := w.checkCarriedInputs(ctx, tx, prevRow); err != nil {
			return err
		}
		if keyed > 0 {
			var stored int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM fact_keys WHERE unit_id = ?1`, prevRow).Scan(&stored); err != nil {
				return wrap("fact keys", err)
			}
			if stored == 0 {
				return &model.Error{Code: model.CodeProviderOutputInvalid,
					Message:     fmt.Sprintf("carry-over names %d replaced fact keys but unit %s stored none", keyed, prev),
					Remediation: "the previous unit was written without keys; rebuild it in full before importing a key delta"}
			}
		}
		return w.copyFacts(ctx, tx, prevRow, replaced.IndexLevel, &stats)
	})
	if err != nil {
		return CarryOverStats{}, err
	}
	w.searchDocs += stats.SearchUnits
	return stats, nil
}

// previousUnitRow resolves prev and refuses anything that is not a sealed unit
// of the same provider and scope: a carry-over across scopes would inherit
// facts about files this unit never declared.
func (w *UnitWriter) previousUnitRow(ctx context.Context, tx *sql.Tx, prev model.UnitID, prevKey []byte) (int64, error) {
	var row int64
	var state model.UnitState
	var providerID, scopeKey string
	err := tx.QueryRowContext(ctx, `SELECT id, state, provider_id, scope_key FROM units WHERE unit_key = ?`, prevKey).
		Scan(&row, &state, &providerID, &scopeKey)
	if isNoRows(err) {
		return 0, invalid("previous unit %s does not exist", prev)
	}
	if err != nil {
		return 0, wrap("units", err)
	}
	if state != model.UnitSealed {
		return 0, conflict("previous unit %s is %s; only a sealed unit can be carried over", prev, state)
	}
	if providerID != w.build.Spec.ProviderID || scopeKey != w.build.Spec.ScopeKey {
		return 0, invalid("previous unit %s belongs to provider %q scope %q, not %q/%q",
			prev, providerID, scopeKey, w.build.Spec.ProviderID, w.build.Spec.ScopeKey)
	}
	return row, nil
}

// carryTemp are the staging tables for one carry-over. They live on the single
// writer connection, like the collector's, and are emptied on entry so a
// previous call's rows can never widen this one's exclusions.
var carryTemp = []string{
	`CREATE TEMP TABLE IF NOT EXISTS cx_carry_files(file_id BLOB PRIMARY KEY) WITHOUT ROWID`,
	`CREATE TEMP TABLE IF NOT EXISTS cx_carry_scopes(scope_key TEXT PRIMARY KEY) WITHOUT ROWID`,
	`CREATE TEMP TABLE IF NOT EXISTS cx_carry_keys(fact_key TEXT PRIMARY KEY) WITHOUT ROWID`,
	`DELETE FROM cx_carry_files`,
	`DELETE FROM cx_carry_scopes`,
	`DELETE FROM cx_carry_keys`,
}

// stageReplaced drains the three replaced streams into the staging tables and
// reports how many keys were staged. Each element is validated as it is
// staged, so nothing is held: the sets are the caller's streams, and the only
// materialisation is the temp tables, which live in SQLite's own bounded
// storage and are dropped with the connection.
func (w *UnitWriter) stageReplaced(ctx context.Context, tx *sql.Tx, replaced Replaced) (int64, error) {
	for _, q := range carryTemp {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, wrap("carry-over staging", err)
		}
	}
	// check returns the bound value, not a bool: cx_carry_files is a BLOB
	// column and the other two are TEXT, and binding the wrong Go type would
	// store a value none of the survival predicates can match.
	stage := func(table string, seq iter.Seq[string], check func(string) (any, error)) error {
		if seq == nil {
			return nil
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO `+table+` VALUES(?)`)
		if err != nil {
			return wrap("carry-over staging", err)
		}
		defer stmt.Close()
		var outer error
		for v := range seq {
			raw, err := check(v)
			if err != nil {
				outer = err
				break
			}
			if _, err := stmt.ExecContext(ctx, raw); err != nil {
				outer = wrap("carry-over staging", err)
				break
			}
		}
		return outer
	}
	if err := stage("cx_carry_files(file_id)", stringsOf(replaced.Files), func(f string) (any, error) {
		return idBlob("replaced file_id", f)
	}); err != nil {
		return 0, err
	}
	if err := stage("cx_carry_scopes(scope_key)", replaced.Scopes, func(scope string) (any, error) {
		if scope == "" || len(scope) > model.MaxScopeKeyBytes {
			return nil, invalid("a replaced scope key is empty or exceeds %d bytes", model.MaxScopeKeyBytes)
		}
		return scope, nil
	}); err != nil {
		return 0, err
	}
	var n int64
	if err := stage("cx_carry_keys(fact_key)", replaced.Keys, func(key string) (any, error) {
		if !model.ValidHexID(key) {
			return nil, invalid("a replaced fact key is not a %d-character lowercase hex digest", model.IDHexLen)
		}
		n++
		return key, nil
	}); err != nil {
		return 0, err
	}
	return n, nil
}

// stringsOf adapts a FileID stream to the one stageReplaced validates.
func stringsOf(files iter.Seq[model.FileID]) iter.Seq[string] {
	if files == nil {
		return nil
	}
	return func(yield func(string) bool) {
		for f := range files {
			if !yield(string(f)) {
				return
			}
		}
	}
}

// checkCarriedInputs refuses a carry-over that would inherit a fact about a
// file whose bytes this unit does not declare, which is the one way a delta
// could publish a fact about source that no longer says it. Only the files the
// previous unit actually located facts in are checked: a unit routinely
// declares inputs that carry no facts of their own — the index or export the
// provider read, a build manifest — and those may change freely without
// invalidating a single carried row.
func (w *UnitWriter) checkCarriedInputs(ctx context.Context, tx *sql.Tx, prevRow int64) error {
	var stale int64
	err := tx.QueryRowContext(ctx, `WITH located(file_id) AS (
			SELECT file_id FROM node_facts WHERE unit_id = ?1 AND file_id IS NOT NULL
			UNION SELECT file_id FROM evidence WHERE unit_id = ?1 AND file_id IS NOT NULL
			UNION SELECT file_id FROM search_units WHERE unit_id = ?1)
		SELECT count(*) FROM located l JOIN unit_inputs p ON p.unit_id = ?1 AND p.file_id = l.file_id
		WHERE l.file_id NOT IN (SELECT file_id FROM cx_carry_files)
			AND NOT EXISTS (SELECT 1 FROM unit_inputs n WHERE n.unit_id = ?2 AND n.file_id = l.file_id AND n.content_hash = p.content_hash)`,
		prevRow, w.rowID).Scan(&stale)
	if err != nil {
		return wrap("unit_inputs", err)
	}
	if stale != 0 {
		return &model.Error{Code: model.CodeSnapshotChanged,
			Message:     fmt.Sprintf("carry-over would inherit facts about %d files this unit does not declare with the same content", stale),
			Details:     map[string]string{"unit_id": string(w.build.Spec.ID)},
			Remediation: "name every changed or removed path in Replaced.Files, or rebuild the unit in full"}
	}
	return nil
}

// bucketSurvives is the Section 11.4 retention-bucket predicate over a row
// that carries a file_id: an unnamed path's bucket is inherited, and the
// index-level bucket is inherited unless the caller replaced it.
const bucketSurvives = `((%[1]s.file_id IS NULL AND ?2 = 0)
	OR (%[1]s.file_id IS NOT NULL AND %[1]s.file_id NOT IN (SELECT file_id FROM cx_carry_files)))`

// keySurvives is the Section 11.4 fact-key predicate: a fact is carried unless
// ANY of its keys was replaced. A fact with no keys has no fact_keys row, so
// NOT EXISTS holds and only its retention bucket can replace it. Matching on
// "any" rather than "all" is what makes the carried set the exact complement of
// what the producer re-emitted: the emitter republishes a whole fact as soon as
// one of its occurrence keys changes.
const keySurvives = `NOT EXISTS (SELECT 1 FROM fact_keys fk WHERE fk.unit_id = ?1 AND fk.%[2]s = %[1]s.%[2]s
	AND fk.fact_key IN (SELECT fact_key FROM cx_carry_keys))`

func (w *UnitWriter) copyFacts(ctx context.Context, tx *sql.Tx, prevRow int64, replaceIndexLevel bool, stats *CarryOverStats) error {
	indexArg := boolInt(replaceIndexLevel)
	exec := func(what, query string, args ...any) (int64, error) {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, wrap(what, err)
		}
		n, err := res.RowsAffected()
		return n, wrap(what, err)
	}

	// Nodes first: relations, aliases, search documents and evidence all
	// reference the node facts of this unit through a foreign key.
	n, err := exec("node_facts", `INSERT INTO node_facts(unit_id, node_id, language, name, qualified_name, signature,
		file_id, start_byte, end_byte, metadata_json)
		SELECT ?3, nf.node_id, nf.language, nf.name, nf.qualified_name, nf.signature,
			nf.file_id, nf.start_byte, nf.end_byte, nf.metadata_json
		FROM node_facts nf WHERE nf.unit_id = ?1
			AND `+fmt.Sprintf(bucketSurvives, "nf")+`
			AND `+fmt.Sprintf(keySurvives, "nf", "node_id")+`
		ON CONFLICT(unit_id, node_id) DO NOTHING`, prevRow, indexArg, w.rowID)
	if err != nil {
		return err
	}
	stats.Nodes = n

	// A relation is inherited only while at least one of its occurrences is:
	// SealUnit requires every relation fact to keep evidence.
	if n, err = exec("relation_facts", `INSERT INTO relation_facts(unit_id, relation_id)
		SELECT ?3, rf.relation_id FROM relation_facts rf WHERE rf.unit_id = ?1
			AND `+fmt.Sprintf(keySurvives, "rf", "relation_id")+`
			AND EXISTS (SELECT 1 FROM evidence e WHERE e.unit_id = ?1 AND e.relation_id = rf.relation_id
				AND `+fmt.Sprintf(bucketSurvives, "e")+`)
		ON CONFLICT(unit_id, relation_id) DO NOTHING`, prevRow, indexArg, w.rowID); err != nil {
		return err
	}
	stats.Relations = n

	// The keys of every fact that was carried come with it, or the successor
	// would hold facts no later refresh could ever replace or remove: it would
	// look like a unit its producer never keyed. Only the keys of facts this
	// unit actually holds are copied, and a key the fresh import already
	// recorded for the same fact yields.
	if _, err = exec("fact_keys", `INSERT INTO fact_keys(unit_id, node_id, relation_id, fact_key)
		SELECT ?3, fk.node_id, fk.relation_id, fk.fact_key FROM fact_keys fk WHERE fk.unit_id = ?1
			AND fk.fact_key NOT IN (SELECT fact_key FROM cx_carry_keys)
			AND ((fk.node_id IS NOT NULL AND EXISTS (SELECT 1 FROM node_facts nf WHERE nf.unit_id = ?3 AND nf.node_id = fk.node_id))
			  OR (fk.relation_id IS NOT NULL AND EXISTS (SELECT 1 FROM relation_facts rf WHERE rf.unit_id = ?3 AND rf.relation_id = fk.relation_id)))
		ON CONFLICT DO NOTHING`, prevRow, indexArg, w.rowID); err != nil {
		return err
	}

	// The replaced scope keys arrive as text and the alias rows carry scope
	// surrogates (S-3), so the exclusion is resolved through scope_keys. A
	// replaced scope that was never interned matches no row, which is the same
	// outcome the text comparison had.
	if n, err = exec("native_aliases", `INSERT INTO native_aliases(unit_id, scope_key_id, native_key_id, node_id)
		SELECT ?3, na.scope_key_id, na.native_key_id, na.node_id FROM native_aliases na WHERE na.unit_id = ?1
			AND na.scope_key_id NOT IN (SELECT sk.id FROM scope_keys sk JOIN cx_carry_scopes c ON c.scope_key = sk.key)
			AND EXISTS (SELECT 1 FROM node_facts nf WHERE nf.unit_id = ?3 AND nf.node_id = na.node_id)
		ON CONFLICT(unit_id, scope_key_id, native_key_id, node_id) DO NOTHING`, prevRow, indexArg, w.rowID); err != nil {
		return err
	}
	stats.Aliases = n

	if err := w.copySearchUnits(ctx, tx, prevRow, stats); err != nil {
		return err
	}
	return w.copyEvidence(ctx, tx, prevRow, replaceIndexLevel, stats)
}

// copySearchUnits carries the lexical documents forward. doc_id travels with
// the row, so each carried document keeps the posting its text was indexed
// into and nothing is re-indexed: the copy is one bounded INSERT..SELECT that
// reads and writes no text at all, and it chains through repeated deltas
// because the column it copies is itself a copy. Re-deriving the body instead
// -- from the content store over [start_byte, end_byte) -- would index source
// text no producer ever published as a document body, silently changing every
// carried document's lexical hits, BM25 length and snippets.
func (w *UnitWriter) copySearchUnits(ctx context.Context, tx *sql.Tx, prevRow int64, stats *CarryOverStats) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO search_units(unit_id, search_key, node_id, file_id, path, kind,
		name, qualified_name, signature, start_byte, end_byte, token_count, doc_id)
		SELECT ?3, su.search_key, su.node_id, su.file_id, su.path, su.kind,
			su.name, su.qualified_name, su.signature, su.start_byte, su.end_byte, su.token_count, su.doc_id
		FROM search_units su WHERE su.unit_id = ?1
			AND su.file_id NOT IN (SELECT file_id FROM cx_carry_files)
			AND (su.node_id IS NULL OR EXISTS (SELECT 1 FROM node_facts nf WHERE nf.unit_id = ?3 AND nf.node_id = su.node_id))
		ON CONFLICT(unit_id, search_key) DO NOTHING`, prevRow, 0, w.rowID)
	if err != nil {
		return wrap("search_units", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrap("search_units", err)
	}
	stats.SearchUnits = n
	return nil
}

// carryEvidencePage bounds one page of the evidence copy.
const carryEvidencePage = 2000

// copyEvidence re-identifies and copies the surviving occurrences. Evidence
// identity folds the unit id (Section 9.3), so a copied row is a different row
// with a different primary key; it cannot be copied in SQL, and the only
// honest copy recomputes NewEvidenceID over the new unit. Every other input to
// that derivation is stored, except the producer's assertion of the file's
// content hash, which content_hash_bound records and checkCarriedInputs has
// already proved is this unit's declared hash for that file.
func (w *UnitWriter) copyEvidence(ctx context.Context, tx *sql.Tx, prevRow int64, replaceIndexLevel bool, stats *CarryOverStats) error {
	ins, err := tx.PrepareContext(ctx, `INSERT INTO evidence(id, unit_id, node_id, relation_id, precision, file_id,
		start_byte, end_byte, native_key_id, detail, content_hash_bound)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`)
	if err != nil {
		return wrap("evidence", err)
	}
	defer ins.Close()
	hashes := map[string]string{}
	after := make([]byte, 0, 32)
	for {
		page, err := w.readEvidencePage(ctx, tx, prevRow, replaceIndexLevel, after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		after = page[len(page)-1].raw
		for _, row := range page {
			e := row.evidence
			e.UnitID = w.build.Spec.ID
			if row.bound {
				hash, ok := hashes[string(e.FileID)]
				if !ok {
					if hash, err = w.inputHash(ctx, tx, e.FileID); err != nil {
						return err
					}
					hashes[string(e.FileID)] = hash
				}
				e.ContentHash = hash
			}
			e.ID = model.NewEvidenceID(e)
			idRaw, _ := model.DecodeID(string(e.ID))
			fileRaw, _ := optionalBlob("evidence.file_id", string(e.FileID))
			start, end := rangeBytes(e.Range)
			// The surrogates are rebound as read: node_ids, relation_ids and
			// native_keys are database-wide dictionaries, so the predecessor's
			// refs are already this unit's refs and re-resolving them would
			// only repeat the lookup the page already did.
			res, err := ins.ExecContext(ctx, idRaw, w.rowID, nullNode(row.node), nullRelation(row.rel), string(e.Precision), fileRaw,
				start, end, int64(row.native), e.Detail, boolInt(row.bound))
			if err != nil {
				return wrap("evidence", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return wrap("evidence", err)
			}
			stats.Evidence += n
		}
		if len(page) < carryEvidencePage {
			return nil
		}
	}
}

type carriedEvidence struct {
	evidence model.Evidence
	bound    bool
	raw      []byte
	// node, rel and native are the stored surrogates, carried alongside the
	// canonical values the re-derivation of the evidence id needs: the hash is
	// a function of the canonical identity (Section 9.3), the row is a function
	// of the surrogate, and the copy needs both.
	node   nodeRef
	rel    relRef
	native nativeRef
}

// readEvidencePage reads one keyset page of the previous unit's surviving
// evidence, in primary-key order, so no cursor is open while the copy writes.
// A row is surviving when its bucket is inherited and the fact it supports is
// already a fact of this unit.
func (w *UnitWriter) readEvidencePage(ctx context.Context, tx *sql.Tx, prevRow int64, replaceIndexLevel bool, after []byte) ([]carriedEvidence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.node_id, ni.canonical, e.relation_id, ri.canonical, e.precision, e.file_id,
		e.start_byte, e.end_byte, e.native_key_id, nk.key, e.detail, e.content_hash_bound
		FROM evidence e
		LEFT JOIN node_ids ni ON ni.id = e.node_id
		LEFT JOIN relation_ids ri ON ri.id = e.relation_id
		JOIN native_keys nk ON nk.id = e.native_key_id
		WHERE e.unit_id = ?1 AND e.id > ?4
			AND `+fmt.Sprintf(bucketSurvives, "e")+`
			AND ((e.node_id IS NOT NULL AND EXISTS (SELECT 1 FROM node_facts nf WHERE nf.unit_id = ?3 AND nf.node_id = e.node_id))
			  OR (e.relation_id IS NOT NULL AND EXISTS (SELECT 1 FROM relation_facts rf WHERE rf.unit_id = ?3 AND rf.relation_id = e.relation_id)))
		ORDER BY e.id LIMIT ?5`, prevRow, boolInt(replaceIndexLevel), w.rowID, after, carryEvidencePage)
	if err != nil {
		return nil, wrap("evidence", err)
	}
	defer rows.Close()
	page := make([]carriedEvidence, 0, carryEvidencePage)
	for rows.Next() {
		var id, node, relation, file []byte
		var nodeRow, relRow sql.NullInt64
		var start, end sql.NullInt64
		var native int64
		var bound int
		var row carriedEvidence
		if err := rows.Scan(&id, &nodeRow, &node, &relRow, &relation, &row.evidence.Precision, &file, &start, &end,
			&native, &row.evidence.NativeKey, &row.evidence.Detail, &bound); err != nil {
			return nil, wrap("evidence", err)
		}
		row.raw = id
		row.bound = bound == 1
		row.node = nodeRef(nodeRow.Int64)
		row.rel = relRef(relRow.Int64)
		row.native = nativeRef(native)
		row.evidence.NodeID = model.NodeID(optionalHex(node))
		row.evidence.RelationID = model.RelationID(optionalHex(relation))
		row.evidence.FileID = model.FileID(optionalHex(file))
		if start.Valid && end.Valid {
			row.evidence.Range = &model.SourceRange{
				Start: model.Position{Byte: uint64(start.Int64)},
				End:   model.Position{Byte: uint64(end.Int64)},
			}
		}
		page = append(page, row)
	}
	return page, wrap("evidence", rows.Err())
}

func optionalHex(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return idHex(raw)
}

func (w *UnitWriter) inputHash(ctx context.Context, tx *sql.Tx, file model.FileID) (string, error) {
	fileRaw, _ := model.DecodeID(string(file))
	var stored []byte
	err := tx.QueryRowContext(ctx, `SELECT content_hash FROM unit_inputs WHERE unit_id = ? AND file_id = ?`, w.rowID, fileRaw).Scan(&stored)
	if isNoRows(err) {
		return "", &model.Error{Code: model.CodeProviderOutputInvalid,
			Message: "carried evidence names a file the unit did not declare as an input", Details: map[string]string{"file_id": string(file)}}
	}
	if err != nil {
		return "", wrap("unit_inputs", err)
	}
	return idHex(stored), nil
}

// clipEvidence enforces the Section 11.1 bound on evidence per fact over the
// whole unit, and runs for every unit at seal rather than only after a
// carry-over. NodeFact.Validate bounds the evidence of one handed-off batch,
// which is not the same thing: a producer that publishes the same fact in two
// batches, or a delta that merges fresh occurrences with carried ones, ends up
// over the bound either way. Clipping only the delta path would make the row
// set depend on how the unit was built, which is precisely the determinism a
// delta has to preserve.
//
// The surplus is chosen by evidence id, which is a function of the
// occurrence's own content, so the retained set is the same for the same union
// however it was assembled. It does not prefer fresh rows to carried ones:
// both are valid occurrences of the same fact in the same unit, and preferring
// one would reintroduce the dependence on assembly order.
func (w *UnitWriter) clipEvidence(ctx context.Context, tx *sql.Tx) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM evidence WHERE unit_id = ?1 AND id IN (
		SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY node_id, relation_id ORDER BY id) AS rank
			FROM evidence WHERE unit_id = ?1) WHERE rank > ?2)`, w.rowID, w.s.opts.MaxEvidencePerFact)
	if err != nil {
		return wrap("evidence", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrap("evidence", err)
	}
	w.evidenceClipped = n
	return nil
}

// PutDeltaState stores one provider-owned incremental-refresh artifact with
// the unit it describes: the SCIP DocumentManifest, the dependence KeySet, or
// whatever a later provider needs to diff its next run without recomputing the
// previous one. The payload is opaque to storage and read from src in parts,
// so an artifact is never held whole; there is no bound on its size. It
// shares the unit's lifetime exactly, so a retired unit takes its artifact
// with it and no refresh can diff against state whose facts were collected.
// A second call under the same kind replaces the first.
func (w *UnitWriter) PutDeltaState(ctx context.Context, kind string, src io.Reader) error {
	if w.done {
		return conflict("unit %s is no longer building", w.build.Spec.ID)
	}
	if kind == "" || len(kind) > model.MaxIdentifierBytes {
		return invalid("delta state kind is required and bounded to %d bytes", model.MaxIdentifierBytes)
	}
	return w.s.ingest(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM unit_delta_state WHERE unit_id = ? AND kind = ?`, w.rowID, kind); err != nil {
			return wrap("unit_delta_state", err)
		}
		buf := make([]byte, deltaStatePart)
		for part := int64(0); ; part++ {
			n, err := io.ReadFull(src, buf)
			if n > 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO unit_delta_state(unit_id, kind, part, payload) VALUES(?, ?, ?, ?)`,
					w.rowID, kind, part, buf[:n]); err != nil {
					return wrap("unit_delta_state", err)
				}
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if part == 0 && n == 0 {
					return invalid("delta state %q is empty", kind)
				}
				return nil
			}
			if err != nil {
				return internal("delta state " + kind + ": " + err.Error())
			}
		}
	})
}

// DeltaState streams what a sealed unit stored under kind into dst, part by
// part, in one read transaction so the artifact is the one the unit sealed. A
// unit that stored none is CTX_ARGUMENT_INVALID with reason not_found, so a
// refresh can tell "no previous state, import in full" from a failure.
func (s *Store) DeltaState(ctx context.Context, unit model.UnitID, kind string, dst io.Writer) error {
	key, err := idBlob("unit_id", string(unit))
	if err != nil {
		return err
	}
	return s.readOwn(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT ds.part, ds.payload FROM unit_delta_state ds JOIN units u ON u.id = ds.unit_id
			WHERE u.unit_key = ? AND ds.kind = ? ORDER BY ds.part`, key, kind)
		if err != nil {
			return wrap("unit_delta_state", err)
		}
		defer rows.Close()
		var next int64
		for rows.Next() {
			var part int64
			var payload []byte
			if err := rows.Scan(&part, &payload); err != nil {
				return wrap("unit_delta_state", err)
			}
			if part != next {
				return corrupt("unit %s delta state %q: part %d follows part %d", unit, kind, part, next-1)
			}
			next++
			if _, err := dst.Write(payload); err != nil {
				return internal("delta state " + kind + ": " + err.Error())
			}
		}
		if err := rows.Err(); err != nil {
			return wrap("unit_delta_state", err)
		}
		if next == 0 {
			return notFound("unit %s stored no delta state of kind %q", unit, kind)
		}
		return nil
	})
}

// unitInputsPage bounds one page of a UnitInputs scan. The iterator reads a
// page per read transaction so no cursor is held open across the caller's
// work, and the page size is the bound Section 6 requires on the traversal.
const unitInputsPage = 1000

// UnitInputs streams the sealed unit's declared inputs in ascending FileID
// order — the same order BeginUnit required when they were written — so a
// caller can merge-join them against a fresh input stream without holding
// either list. It refuses a unit that is not sealed: a building unit's
// unit_inputs rows are still arriving, and a delta that diffed against a
// partial set would name too few replaced buckets and carry a stale fact.
//
// The first failure ends the sequence: it is yielded with a zero UnitInput and
// nothing follows it.
//
// Each page is one read transaction on the reader pool, so the stream sees
// the last commit and not the ingestion group in progress: the unit it reads
// is a predecessor, sealed and committed before the run that reads it began.
// The dependence delta applier walks this from inside UnitWriter.CarryOver's
// ingestion call, which holds the group's connection, so those reads are
// nested inside the writer's and compete for reader connections with the
// rest of the process; see Replaced.
func (s *Store) UnitInputs(ctx context.Context, unit model.UnitID) iter.Seq2[model.UnitInput, error] {
	return func(yield func(model.UnitInput, error) bool) {
		fail := func(err error) { yield(model.UnitInput{}, err) }
		key, err := idBlob("unit_id", string(unit))
		if err != nil {
			fail(err)
			return
		}
		var row int64
		if err := s.read(ctx, func(tx *sql.Tx) error {
			var state model.UnitState
			err := tx.QueryRowContext(ctx, `SELECT id, state FROM units WHERE unit_key = ?`, key).Scan(&row, &state)
			if isNoRows(err) {
				return notFound("unit %s does not exist", unit)
			}
			if err != nil {
				return wrap("units", err)
			}
			if state != model.UnitSealed {
				return conflict("unit %s is %s; only a sealed unit's inputs can be read", unit, state)
			}
			return nil
		}); err != nil {
			fail(err)
			return
		}
		after := make([]byte, 0, 32)
		page := make([]model.UnitInput, 0, unitInputsPage)
		for {
			var last []byte
			if err := s.read(ctx, func(tx *sql.Tx) error {
				page = page[:0]
				last = nil
				rows, err := tx.QueryContext(ctx, `SELECT file_id, content_hash, executable FROM unit_inputs
					WHERE unit_id = ?1 AND file_id > ?2 ORDER BY file_id LIMIT ?3`, row, after, unitInputsPage)
				if err != nil {
					return wrap("unit_inputs", err)
				}
				defer rows.Close()
				for rows.Next() {
					var file, hash []byte
					var executable int
					if err := rows.Scan(&file, &hash, &executable); err != nil {
						return wrap("unit_inputs", err)
					}
					last = file
					page = append(page, model.UnitInput{FileID: model.FileID(idHex(file)),
						ContentHash: idHex(hash), Executable: executable == 1})
				}
				return wrap("unit_inputs", rows.Err())
			}); err != nil {
				fail(err)
				return
			}
			for _, in := range page {
				if !yield(in, nil) {
					return
				}
			}
			if len(page) < unitInputsPage {
				return
			}
			after = last
		}
	}
}

// SelectedUnit reports the unit a generation selects for one provider and
// scope, which is how a refresh finds the predecessor to carry over from: the
// previous generation's unit for the same work. A generation that selects none
// is CTX_ARGUMENT_INVALID with reason not_found.
func (s *Store) SelectedUnit(ctx context.Context, gen model.GenerationID, providerID, scopeKey string) (model.UnitID, error) {
	var raw []byte
	err := s.readOwn(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT u.unit_key FROM generation_units gu JOIN units u ON u.id = gu.unit_id
			WHERE gu.generation_id = ? AND gu.provider_id = ? AND gu.scope_key = ?`, int64(gen), providerID, scopeKey).Scan(&raw)
		if isNoRows(err) {
			return notFound("generation %d selects no unit for provider %q scope %q", gen, providerID, scopeKey)
		}
		return wrap("generation_units", err)
	})
	if err != nil {
		return "", err
	}
	return model.UnitID(idHex(raw)), nil
}
