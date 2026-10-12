package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
)

// The lexical tier's postings live in the embedded search library
// (docs/adr/ADR-0013-search-index.md), in one index directory beside the
// database that this store alone opens, writes and closes. SQLite keeps each
// document's identity row (search_units, keyed by a doc id that is never
// reused), each generation's visible documents (generation_search), the unit's
// pending mark (units.search_pending: documents added to the library and not
// yet known to be in a library commit) and the queue of document deletes the
// library has not applied (search_doc_deletes).
//
// The order of the two commits is the crash contract. The library commits
// first, with the generation id as its payload; SQLite then activates the
// generation, clearing its members' pending marks and the delete rows that
// commit applied. Recover reconciles whatever a crash left between them.

// searchDirSuffix names the index directory beside the database file.
const searchDirSuffix = ".search"

// deletePage bounds the doc ids one DeleteDocs call carries, so a queue of any
// length is applied in pages and never held whole.
const deletePage = 4096

// afterSearchCommit runs between an activation's library commit and its
// SQLite transaction. It is set only by tests (export_test.go), which stop an
// activation there as if the process died.
var afterSearchCommit func() error

// removeStaleSearch removes the index directory beside a database whose
// schema is being created, before anything opens it. A directory found there
// belongs to a database that was removed by hand without it, and adopting it
// would corrupt every search: the new database allocates doc ids from 1 again
// and its units are keyed by content, so the old documents would answer under
// the new database's doc ids and units. The removal is the paced one -- the
// directory is renamed into the to-free set and given back at the pace -- and
// it runs inside the transaction that creates the schema, so no other process
// can open the database and adopt the directory between the two. A removal
// that fails fails that transaction, and the next open tries again.
func (s *Store) removeStaleSearch() error {
	dir := s.path + searchDirSuffix
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return internal("search index directory: " + err.Error())
	}
	if err := paced.RemoveAll(dir); err != nil {
		return internal("remove the search index left beside a removed database: " + err.Error())
	}
	slog.Info("removed the search index directory a removed database left beside this new one", "dir", dir)
	return nil
}

// openTextIndex opens the store's search index. A read-only open of a database
// whose index directory is missing opens none: every search of such a store
// answers CTX_STORAGE_CORRUPT (searchLost), while every other read serves.
func (s *Store) openTextIndex() error {
	o := s.opts.TextIndex
	o.Dir = s.path + searchDirSuffix
	o.Writable = !s.opts.ReadOnly
	if s.opts.ReadOnly {
		if _, err := os.Stat(o.Dir); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return internal("search index directory: " + err.Error())
		}
	}
	x, err := textindex.Open(o)
	if err != nil {
		return err
	}
	s.text = x
	return nil
}

// releaseSearchWriter closes the search index's writer, discarding what it
// has not committed. A store without an index has no writer to close.
func (s *Store) releaseSearchWriter() error {
	if s.text == nil {
		return nil
	}
	return s.text.ReleaseWriter()
}

// searchLost is the answer of a search whose generation the index does not
// hold: the index directory is missing, or its last commit names an older
// generation, so it was lost or replaced behind the database.
func searchLost(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeStorageCorrupt,
		Message:     "the search index does not hold this generation: " + fmt.Sprintf(format, args...),
		Remediation: "run `codectx reset --reindex` to build the workspace again from the repository"}
}

// searchHolds checks that the index's last commit covers generation gen: a
// library commit always precedes the SQLite activation it serves, so a payload
// below an activated generation means the directory is not the one that
// generation was committed into. Zero (no generation) is always held.
func (s *Store) searchHolds(gen int64) error {
	if s.text == nil {
		return searchLost("the index directory %s is missing", s.path+searchDirSuffix)
	}
	if gen == 0 {
		return nil
	}
	payload, ok, err := s.text.Committed()
	if err != nil {
		return err
	}
	if !ok {
		return searchLost("the index holds no commit and generation %d is active", gen)
	}
	committed, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return corrupt("the search index's commit payload %q is not a generation id", payload)
	}
	if committed < gen {
		return searchLost("its last commit is generation %d, behind generation %d", committed, gen)
	}
	return nil
}

// activeGenerationID is the store's active generation, zero when none is
// published. One store is one repository, so there is at most one row.
func (s *Store) activeGenerationID(ctx context.Context) (int64, error) {
	var id int64
	err := s.readOwn(ctx, func(tx *sql.Tx) error {
		return wrap("active_generations", tx.QueryRowContext(ctx,
			`SELECT coalesce(max(generation_id), 0) FROM active_generations`).Scan(&id))
	})
	return id, err
}

// applyQueuedDeletes hands every queued document delete to the library, in
// pages, and returns the highest queue rowid it applied (zero: none). The
// deletes are durable only at the next library commit; the caller deletes the
// rows up to the returned rowid in the SQLite transaction that follows it.
func (s *Store) applyQueuedDeletes(ctx context.Context) (int64, error) {
	var last int64
	if err := s.readOwn(ctx, func(tx *sql.Tx) error {
		return wrap("search_doc_deletes", tx.QueryRowContext(ctx,
			`SELECT coalesce(max(rowid), 0) FROM search_doc_deletes`).Scan(&last))
	}); err != nil || last == 0 {
		return 0, err
	}
	var after int64
	ids := make([]uint64, 0, deletePage)
	for after < last {
		ids = ids[:0]
		err := s.readOwn(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT rowid, doc_id FROM search_doc_deletes
				WHERE rowid > ?1 AND rowid <= ?2 ORDER BY rowid LIMIT ?3`, after, last, deletePage)
			if err != nil {
				return wrap("search_doc_deletes", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&after, &id); err != nil {
					return wrap("search_doc_deletes", err)
				}
				ids = append(ids, uint64(id))
			}
			return wrap("search_doc_deletes", rows.Err())
		})
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			break
		}
		if err := s.text.DeleteDocs(ctx, ids); err != nil {
			return 0, err
		}
	}
	return last, nil
}

// commitSearch is an activation's library half: the queued deletes, then the
// commit whose payload is gen, then the proof that every member the commit was
// for holds all of its documents. It returns the queue rowid the commit
// applied. An index that does not hold the active generation refuses: a new
// generation would reuse units whose documents it lacks.
func (s *Store) commitSearch(ctx context.Context, gen, active model.GenerationID) (int64, error) {
	if err := s.searchHolds(int64(active)); err != nil {
		return 0, err
	}
	applied, err := s.applyQueuedDeletes(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.text.Commit(ctx, strconv.FormatInt(int64(gen), 10)); err != nil {
		return 0, err
	}
	// A member whose documents were added in an ingestion group that was then
	// lost holds fewer documents than its mark; activating it would publish a
	// generation that silently misses them.
	err = s.readOwn(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT u.unit_key, u.search_pending FROM generation_units gu
			JOIN units u ON u.id = gu.unit_id WHERE gu.generation_id = ? AND u.search_pending > 0`, int64(gen))
		if err != nil {
			return wrap("units", err)
		}
		defer rows.Close()
		for rows.Next() {
			var key []byte
			var pending int64
			if err := rows.Scan(&key, &pending); err != nil {
				return wrap("units", err)
			}
			held, err := s.text.UnitDocs(idHex(key))
			if err != nil {
				return err
			}
			if held != pending {
				return corrupt("unit %s added %d search documents but the search index commit holds %d", idHex(key), pending, held)
			}
		}
		return wrap("units", rows.Err())
	})
	return applied, err
}

// writeGenerationSearch stores generation gen's visible documents -- a bitmap
// over doc ids, bit id%8 of byte id/8, least significant first -- clears its
// members' pending marks and deletes the queue rows up to applied. It runs
// inside the activation transaction, after the library commit that made the
// members' documents and those deletes durable.
func writeGenerationSearch(ctx context.Context, tx *sql.Tx, gen, applied int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT su.doc_id FROM generation_units gu
		JOIN search_units su ON su.unit_id = gu.unit_id WHERE gu.generation_id = ?`, gen)
	if err != nil {
		return wrap("search_units", err)
	}
	var visible []byte
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return wrap("search_units", err)
		}
		at := int(id / 8)
		if at >= len(visible) {
			visible = append(visible, make([]byte, at+1-len(visible))...)
		}
		visible[at] |= 1 << (id % 8)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap("search_units", err)
	}
	if visible == nil {
		visible = []byte{}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO generation_search(generation_id, visible) VALUES(?, ?)`, gen, visible); err != nil {
		return wrap("generation_search", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE units SET search_pending = 0 WHERE search_pending > 0
		AND id IN (SELECT unit_id FROM generation_units WHERE generation_id = ?)`, gen); err != nil {
		return wrap("units", err)
	}
	if applied > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_doc_deletes WHERE rowid <= ?`, applied); err != nil {
			return wrap("search_doc_deletes", err)
		}
	}
	return nil
}

// commitSweepDeletes makes the deletes a collection queued durable when no
// activation will: the queue goes to the library, the library commits with
// the active generation as payload, its writer is released and the applied
// rows go. An index that does not hold the active generation is left as it is
// -- a commit would stamp it with a generation it lacks and hide the loss
// searchHolds reports -- and the deletes wait in the queue.
func (s *Store) commitSweepDeletes(ctx context.Context) error {
	var queued bool
	if err := s.readOwn(ctx, func(tx *sql.Tx) error {
		return wrap("search_doc_deletes", tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM search_doc_deletes)`).Scan(&queued))
	}); err != nil || !queued {
		return err
	}
	active, err := s.activeGenerationID(ctx)
	if err != nil {
		return err
	}
	if err := s.searchHolds(active); err != nil {
		slog.Warn("collected search documents stay queued: the search index does not hold the active generation", "error", err)
		return nil
	}
	applied, err := s.applyQueuedDeletes(ctx)
	if err == nil {
		err = s.text.Commit(ctx, strconv.FormatInt(active, 10))
	}
	if err != nil {
		err = errors.Join(err, s.releaseSearchWriter())
		// Another process writing the index holds its writer: the deletes stay
		// queued for the next commit, which is that process's activation or
		// this store's next collection.
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeWorkspaceBusy {
			slog.Warn("collected search documents stay queued: another process is writing the search index", "error", err)
			return nil
		}
		return err
	}
	if err := s.releaseSearchWriter(); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM search_doc_deletes WHERE rowid <= ?`, applied)
		return wrap("search_doc_deletes", err)
	})
}

// commitSearchOnClose makes durable what the search writer still holds when
// the store closes: the documents of units a run sealed but no activation
// committed. The ingestion group they were written in has just committed, so
// every doc id they carry is durably allocated; the documents stay invisible
// until a generation that selects their units activates, and the next run can
// reuse those units instead of rebuilding them. The payload stays the active
// generation, and an index that does not hold it is left uncommitted, so the
// loss searchHolds reports is never hidden.
func (s *Store) commitSearchOnClose() error {
	if s.text == nil || s.opts.ReadOnly || !s.text.Writing() {
		return nil
	}
	// The writer has stopped and its group committed, so the reader pool sees
	// the last activation.
	ctx := context.Background()
	var active int64
	if err := s.read(ctx, func(tx *sql.Tx) error {
		return wrap("active_generations", tx.QueryRowContext(ctx,
			`SELECT coalesce(max(generation_id), 0) FROM active_generations`).Scan(&active))
	}); err != nil {
		return errors.Join(err, s.releaseSearchWriter())
	}
	if err := s.searchHolds(active); err != nil {
		return s.releaseSearchWriter()
	}
	if err := s.text.Commit(ctx, strconv.FormatInt(active, 10)); err != nil {
		return errors.Join(err, s.releaseSearchWriter())
	}
	return s.releaseSearchWriter()
}

// reconcileSearch brings the library and SQLite back to one state after a
// crash (ADR-0013 Decision 4). It runs first in Recover, under the indexing
// lock, before anything reads the staging the crash left:
//
//  1. the queued deletes are applied;
//  2. every library document whose doc id SQLite never durably allocated --
//     its rows were in an ingestion group the crash lost -- is deleted;
//  3. every unit still marked pending whose library documents differ from its
//     mark is failed, with every unit whose rows name one of the documents it
//     added (a carry-over shares them) and every unit depending on a failed
//     one; their memberships of generations that never activated go, so
//     collection removes them and the planner rebuilds them like any unit
//     that is not sealed. A unit a published generation selects is reported,
//     not failed;
//  4. the library commits with the active generation as payload, its writer
//     is released, and the applied delete rows and the surviving pending
//     marks are cleared.
//
// An index that does not hold the active generation is not touched: its loss
// is what searchHolds reports to every search, and a commit here would hide
// it. Recover does not fail on it.
func (s *Store) reconcileSearch(ctx context.Context) error {
	active, err := s.activeGenerationID(ctx)
	if err != nil {
		return err
	}
	if err := s.searchHolds(active); err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeStorageCorrupt {
			slog.Warn("the search index is not reconciled: it does not hold the active generation", "error", err)
			return nil
		}
		return err
	}
	if err := s.reconcileSearchIndex(ctx, active); err != nil {
		return errors.Join(err, s.releaseSearchWriter())
	}
	return nil
}

func (s *Store) reconcileSearchIndex(ctx context.Context, active int64) error {
	applied, err := s.applyQueuedDeletes(ctx)
	if err != nil {
		return err
	}
	var next int64
	if err := s.readOwn(ctx, func(tx *sql.Tx) error {
		return wrap("search_sequence", tx.QueryRowContext(ctx,
			`SELECT next_doc_id FROM search_sequence WHERE singleton = 1`).Scan(&next))
	}); err != nil {
		return err
	}
	if err := s.text.DeleteDocsFrom(ctx, uint64(next)); err != nil {
		return err
	}
	failed, err := s.unreconciledUnits(ctx)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		if err := s.failUnreconciled(ctx, failed); err != nil {
			return err
		}
	}
	if err := s.text.Commit(ctx, strconv.FormatInt(active, 10)); err != nil {
		return err
	}
	if err := s.releaseSearchWriter(); err != nil {
		return err
	}
	// Every unit still pending now holds exactly its documents in a commit.
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_doc_deletes WHERE rowid <= ?`, applied); err != nil {
			return wrap("search_doc_deletes", err)
		}
		_, err := tx.ExecContext(ctx, `UPDATE units SET search_pending = 0 WHERE search_pending > 0`)
		return wrap("units", err)
	})
}

// unreconciledUnits pages the units marked pending, keyset by row id, and
// returns those whose committed library documents differ from their mark. Only
// a crash produces one, so the answer is the crashed run's broken units.
func (s *Store) unreconciledUnits(ctx context.Context) ([]int64, error) {
	const page = 1000
	var failed []int64
	var after int64
	for {
		type pending struct {
			id    int64
			key   string
			count int64
		}
		var batch []pending
		err := s.readOwn(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT id, unit_key, search_pending FROM units
				WHERE search_pending > 0 AND id > ? ORDER BY id LIMIT ?`, after, page)
			if err != nil {
				return wrap("units", err)
			}
			defer rows.Close()
			for rows.Next() {
				var p pending
				var key []byte
				if err := rows.Scan(&p.id, &key, &p.count); err != nil {
					return wrap("units", err)
				}
				p.key = idHex(key)
				batch = append(batch, p)
			}
			return wrap("units", rows.Err())
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return failed, nil
		}
		for _, p := range batch {
			held, err := s.text.UnitDocs(p.key)
			if err != nil {
				return nil, err
			}
			if held != p.count {
				failed = append(failed, p.id)
			}
		}
		after = batch[len(batch)-1].id
	}
}

// failUnreconciled fails the units whose documents never reached a library
// commit, closed over the units that share their doc ids and the units that
// depend on any of them, in one transaction.
func (s *Store) failUnreconciled(ctx context.Context, units []int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		steps := []string{
			`CREATE TEMP TABLE IF NOT EXISTS cx_search_failed(id INTEGER PRIMARY KEY)`,
			`DELETE FROM cx_search_failed`,
		}
		for _, q := range steps {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return wrap("reconcile", err)
			}
		}
		for _, id := range units {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cx_search_failed(id) VALUES(?)`, id); err != nil {
				return wrap("reconcile", err)
			}
		}
		// A unit's OWN documents are the ones no older unit names: a carry-over
		// copies a predecessor's rows into a unit begun after it, so the unit
		// that added a document is always the lowest-numbered one naming it.
		// Only the units naming a failed unit's own documents lost them; a unit
		// sharing a document the failed unit merely carried shares one that
		// reached a commit under its adder.
		closure := []string{
			`INSERT OR IGNORE INTO cx_search_failed(id) SELECT s2.unit_id FROM cx_search_failed f
				JOIN search_units su ON su.unit_id = f.id JOIN search_units s2 ON s2.doc_id = su.doc_id
				WHERE s2.unit_id <> f.id
				AND NOT EXISTS (SELECT 1 FROM search_units s3 WHERE s3.doc_id = su.doc_id AND s3.unit_id < f.id)`,
			`INSERT OR IGNORE INTO cx_search_failed(id) SELECT ud.unit_id FROM unit_dependencies ud
				WHERE ud.dependency_id IN (SELECT id FROM cx_search_failed)`,
		}
		for {
			var grew int64
			for _, q := range closure {
				res, err := tx.ExecContext(ctx, q)
				if err != nil {
					return wrap("reconcile", err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					return wrap("reconcile", err)
				}
				grew += n
			}
			if grew == 0 {
				break
			}
		}
		// A unit a published generation selects stays as it is: failing it would
		// leave that generation with a member that is not sealed. Its missing
		// documents are reported instead; only a rebuild into a new cache
		// restores them.
		var published int64
		if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT f.id) FROM cx_search_failed f
			JOIN generation_units gu ON gu.unit_id = f.id JOIN generations g ON g.id = gu.generation_id
			WHERE g.status IN ('active', 'superseded')`).Scan(&published); err != nil {
			return wrap("reconcile", err)
		}
		if published != 0 {
			slog.Warn("the search index lacks documents of units a published generation selects; their searches miss them until the workspace is built again with `codectx reset --reindex`",
				"units", published)
		}
		final := []string{
			`DELETE FROM cx_search_failed WHERE id IN (SELECT gu.unit_id FROM generation_units gu
				JOIN generations g ON g.id = gu.generation_id WHERE g.status IN ('active', 'superseded'))`,
			`UPDATE units SET state = 'failed', search_pending = 0 WHERE id IN (SELECT id FROM cx_search_failed)`,
			`DELETE FROM generation_units WHERE unit_id IN (SELECT id FROM cx_search_failed)
				AND generation_id IN (SELECT id FROM generations WHERE status IN ('staging', 'failed'))`,
			`DELETE FROM cx_search_failed`,
		}
		for _, q := range final {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return wrap("reconcile", err)
			}
		}
		return nil
	})
}

// SearchView opens the pinned generation's view of the search index: the
// library's latest commit searched through the generation's visible
// documents. The caller closes it.
func (r *PinnedReader) SearchView(ctx context.Context) (*textindex.View, error) {
	if err := r.s.searchHolds(r.gen); err != nil {
		return nil, err
	}
	var visible []byte
	err := r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT visible FROM generation_search WHERE generation_id = ?`, r.gen).Scan(&visible)
		if isNoRows(err) {
			return corrupt("generation %d has no visible search documents recorded", r.gen)
		}
		return wrap("generation_search", err)
	})
	if err != nil {
		return nil, err
	}
	return r.s.text.OpenView(model.GenerationID(r.gen), visible)
}
