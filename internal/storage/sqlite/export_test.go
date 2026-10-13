package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// Test hooks. They exist so a test can prove behaviour the production
// constants make unreachable on a fixture -- a list that straddles a part
// boundary needs either a repository-sized fixture or a smaller part.

// WithDerivedReaders is opts with the short-read pool sized as the product
// sizes it on this machine under the shipped configuration
// (config.ReadConnections), because Open refuses a store opened without it.
func WithDerivedReaders(opts Options) Options {
	cfg := config.Defaults()
	if opts.ReadConnections == 0 {
		opts.ReadConnections = config.ReadConnections(cfg)
	}
	return opts
}

// SetEdgePartBytes shrinks the edge-stream part size for one test and returns a
// function that restores it. It is not a user setting: part size bounds a
// working set, never what is stored or answered.
func SetEdgePartBytes(n int) func() {
	prev := edgePartBytes
	edgePartBytes = n
	return func() { edgePartBytes = prev }
}

// OutgoingEdgeQuery and IncomingEdgeQuery are the two ordered scans the build
// streams, exported so the query-plan test asserts the SQL that ships.
func OutgoingEdgeQuery() string { return outgoingEdgeQuery() }
func IncomingEdgeQuery() string { return incomingEdgeQuery() }

// PlaceholderNodesInsert, PlaceholderEdgesInsert and CallerSitesQuery are the
// three statements the build's placeholder pass runs over the store.
const (
	PlaceholderNodesInsert = placeholderNodesInsert
	PlaceholderEdgesInsert = placeholderEdgesInsert
)

func CallerSitesQuery() string { return callerSitesQuery() }

// EvidenceCountQuery is the build's third ordered scan.
func EvidenceCountQuery() string { return evidenceCountQuery() }

// ContainerQuery is the build's ordered container scan.
func ContainerQuery() string { return containerQuery() }

// DeltaStateParts counts the stored parts of one unit's delta-state artifact,
// so a test can prove the artifact was written as a sequence of parts rather
// than as one blob.
func (s *Store) DeltaStateParts(ctx context.Context, unit model.UnitID, kind string) (int64, error) {
	key, err := idBlob("unit_id", string(unit))
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.readOwn(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM unit_delta_state ds JOIN units u ON u.id = ds.unit_id
			WHERE u.unit_key = ? AND ds.kind = ?`, key, kind).Scan(&n)
	})
	return n, err
}

// DeltaStatePartBytes is the part size, for the test that sizes its artifact
// to straddle part boundaries.
const DeltaStatePartBytes = deltaStatePart

// WriteRefused and Attribute expose the refused-write error and its settling
// against the disk, so a test can drive the measurement.
func WriteRefused(op string, code int, engineMessage string) *model.Error {
	return writeRefused(op, code, engineMessage)
}

func (s *Store) Attribute(err error) error { return s.attribute(err) }

// SetFreeBytes replaces the disk measurement a refused write is settled
// against and returns a function that restores it.
func (s *Store) SetFreeBytes(fn func(dir string) (uint64, bool)) func() {
	prev := s.freeBytes
	s.freeBytes = fn
	return func() { s.freeBytes = prev }
}

// Commits reports how many ingestion groups the store has committed, so a test
// can prove a long cascade of writes reached the group's commit decision
// between its steps rather than running as one unbounded transaction.
func (s *Store) Commits() int64 { return s.commits.Load() }

// StopAfterSearchCommit makes every activation stop between its search
// index commit and its SQLite transaction, as if the process died there, and
// returns a function that restores the store's behaviour. The stopped
// activation answers ErrStoppedAfterSearchCommit.
func StopAfterSearchCommit() func() {
	prev := afterSearchCommit
	afterSearchCommit = func() error { return ErrStoppedAfterSearchCommit }
	return func() { afterSearchCommit = prev }
}

// ErrStoppedAfterSearchCommit is the activation answer StopAfterSearchCommit
// arranges.
var ErrStoppedAfterSearchCommit = errors.New("stopped between the search index commit and the SQLite activation")

// DiscardSearchAdds closes the search index writer without a commit, as a
// process that died before its next library commit leaves it: every document
// added since the last commit is gone from the library while the SQLite rows
// naming them stay.
func (s *Store) DiscardSearchAdds() error { return s.text.ReleaseWriter() }

// SearchPending is a unit's pending mark, read from the writer's own view.
func (s *Store) SearchPending(ctx context.Context, unit model.UnitID) (int64, error) {
	key, err := idBlob("unit_id", string(unit))
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.readOwn(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT search_pending FROM units WHERE unit_key = ?`, key).Scan(&n)
	})
	return n, err
}

// PendingUnits counts the units still marked pending.
func (s *Store) PendingUnits(ctx context.Context) (int64, error) {
	var n int64
	err := s.readOwn(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM units WHERE search_pending > 0`).Scan(&n)
	})
	return n, err
}

// SearchCommitted is the payload of the search index's last commit, empty when
// it was never committed.
func (s *Store) SearchCommitted() (string, error) {
	payload, _, err := s.text.Committed()
	return payload, err
}
