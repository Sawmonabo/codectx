package neo4jcsv

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"modernc.org/sqlite"
)

// DedupeSink drops a record an earlier Import already handed to the same
// sink.
//
// A unit is normally one import, and an import never publishes one identity
// twice. A subdivided unit is the exception: the provider runs the engine on
// each part and imports each export into the one sink storage opened for the
// unit, and two parts legitimately describe the same entity — above all the
// external stub of a callee both parts reference. Storage keys a node fact by
// (unit, node) and admits a repeat of that key without failing the unit, but
// it does not compare the two rows: the second row's differing columns are
// dropped with nothing said. This sink drops the repeat here instead, where
// the identity is known to have been published by an earlier part of the same
// unit, so the caller wraps its sink in one DedupeSink for the unit's
// lifetime and passes it to every Import.
//
// The seen set is on disk, not in the Go heap: a large unit publishes
// hundreds of thousands of identities. Dropping a duplicate never weakens the
// put-order rule, because the identity it names was already written by the
// call that published it first.
type DedupeSink struct {
	dst provider.Sink
	db  *sql.DB
	// ins is compiled once: the seen set is probed once per record, and
	// recompiling the statement per record is the per-row cost the scratch
	// staging was rewritten to avoid.
	ins *sql.Stmt
}

// NewDedupeSink wraps dst, keeping its seen set in a private database under
// dir. Close removes it.
func NewDedupeSink(dst provider.Sink, dir string) (*DedupeSink, error) {
	if dst == nil {
		return nil, argumentInvalid("a dedupe sink needs a destination")
	}
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "locking_mode(EXCLUSIVE)"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "emitted.db"), RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, internalErr("dedupe sink connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS seen(key TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		db.Close()
		return nil, internalErr("dedupe sink schema: %v", err)
	}
	ins, err := db.PrepareContext(context.Background(), `INSERT OR IGNORE INTO seen(key) VALUES(?)`)
	if err != nil {
		db.Close()
		return nil, internalErr("dedupe sink schema: %v", err)
	}
	return &DedupeSink{dst: dst, db: db, ins: ins}, nil
}

// Close releases the seen set. It does not close the destination sink.
func (s *DedupeSink) Close() error {
	s.ins.Close()
	return s.db.Close()
}

// first reports whether key is new, recording it when it is.
func (s *DedupeSink) first(ctx context.Context, key string) (bool, error) {
	res, err := s.ins.ExecContext(ctx, key)
	if err != nil {
		return false, internalErr("dedupe sink: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, internalErr("dedupe sink: %v", err)
	}
	return n > 0, nil
}

func (s *DedupeSink) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	out := facts[:0]
	for _, f := range facts {
		ok, err := s.first(ctx, "n\x00"+string(f.Node.ID))
		if err != nil {
			return err
		}
		if ok {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return s.dst.PutNodes(ctx, out)
}

func (s *DedupeSink) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	out := facts[:0]
	for _, f := range facts {
		kept := f.Evidence[:0]
		for _, ev := range f.Evidence {
			ok, err := s.first(ctx, "e\x00"+string(ev.ID))
			if err != nil {
				return err
			}
			if ok {
				kept = append(kept, ev)
			}
		}
		if len(kept) == 0 {
			continue
		}
		f.Evidence = kept
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return s.dst.PutRelations(ctx, out)
}

func (s *DedupeSink) PutAliases(ctx context.Context, aliases []model.NativeAlias) error {
	out := aliases[:0]
	for _, a := range aliases {
		ok, err := s.first(ctx, "a\x00"+a.ScopeKey+"\x00"+a.NativeKey+"\x00"+string(a.NodeID))
		if err != nil {
			return err
		}
		if ok {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return s.dst.PutAliases(ctx, out)
}

func (s *DedupeSink) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	return s.dst.PutSearchUnits(ctx, docs)
}
