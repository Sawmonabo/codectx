package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"maps"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Test hooks. They exist so a test can prove behaviour the production
// constants make unreachable on a fixture -- a list that straddles a part
// boundary needs either a repository-sized fixture or a smaller part.

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

// The scans the packed lexical structure streams, exported so the query-plan
// test asserts the SQL that ships: the activation's one pass over its
// documents, which resolves its segment set with them; a reader's walk of that
// set; and the two the compaction adds -- the walk of the documents an input
// segment still holds and the re-point of an absorbed segment's rows.
func GenerationDocumentQuery() string { return generationDocumentQuery }
func GenerationLexicalQuery() string  { return generationLexicalQuery }
func SegmentDocumentQuery() string    { return segmentDocumentQuery }
func RepointSegmentStatement() string { return repointSegmentStatement }

// SetLexicalMergeRatio shrinks the geometric partitioning's ratio for one test
// and returns a function that restores it. It is not a user setting: the ratio
// decides how often already-packed bytes are rewritten, never what is stored or
// answered. LexicalMergeRatio is the ratio that ships, so a test can drive a
// merge on the real constant.
func SetLexicalMergeRatio(r int) func() {
	prev := lexMergeRatio
	lexMergeRatio = r
	return func() { lexMergeRatio = prev }
}

func LexicalMergeRatio() int { return lexMergeRatio }

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

// SegmentPostingDocuments decodes the document rowids one segment's posting
// lists actually carry, so a test can prove a document is GONE from the packed
// bytes rather than merely hidden by a bitmap. It reassembles the segment's
// term directory and posting stream, which is why it is a test hook: a
// production read holds a bounded window of parts, never a whole stream.
func SegmentPostingDocuments(db *sql.DB, segment int64) ([]int64, error) {
	stream := func(name string) ([]byte, error) {
		rows, err := db.Query(`SELECT bytes FROM lexical_segment_parts
			WHERE segment_id = ? AND stream = ? ORDER BY part`, segment, name)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []byte
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
		return out, rows.Err()
	}
	dir, err := stream(streamTermDir)
	if err != nil {
		return nil, err
	}
	lists, err := stream(streamPostList)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for off := 0; off+termEntryBytes <= len(dir); off += termEntryBytes {
		e := dir[off:]
		listOff := int64(binary.LittleEndian.Uint64(e[termEntryListOff:]))
		listLen := int64(binary.LittleEndian.Uint32(e[termEntryListLen:]))
		if listOff+listLen > int64(len(lists)) {
			return nil, corrupt("segment %d term at %d points past its posting stream", segment, off)
		}
		c := &postingCursor{raw: lists[listOff : listOff+listLen], segment: segment}
		for {
			live, err := c.next()
			if err != nil {
				return nil, err
			}
			if !live {
				break
			}
			seen[c.doc] = true
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}
