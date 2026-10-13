package graph

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
)

// An impact route is stored as a parent pointer per admission, never as a list
// carried with the walk. Every admitted state takes the next row of the route
// table -- the admission it was reached from and the canonical relation that
// reached it -- and a served route is rebuilt by following the pointers back to
// a seed and reversing them: the predecessor-array reconstruction of a
// breadth-first search, which stores one pointer per vertex and reads a path in
// time linear in its length. So the walk's per-state cost is one row whatever
// the route's length, and a route is stored whole however long it is.
//
// The pointer is keyed by ADMISSION, not by node. One node may be admitted more
// than once (a dispatch step, a dependence lift), and the fold keeps the
// cheapest of a node's records; a pointer keyed by node would rebuild the
// parent's WINNING route, which need not be the prefix the child was reached
// by. An admission's row is written once and never changes, so following it is
// exact.
//
// The table is fixed-width and addressed by id, so a pointer is one positioned
// read: row id-1 at (id-1)*routeRowBytes. Id 0 is no admission, a seed's: the
// route ends there.
const (
	routeTableFile = "routes"
	// routeRowBytes is one row: the parent admission, then the via relation
	// in viaSlotBytes.
	routeRowBytes = 8 + viaSlotBytes
	// viaSlotBytes holds a relation id. A canonical id (64 lowercase hex) is
	// stored as its 32 bytes under tag 0; any other id is stored as its own
	// bytes under tag 1 with a length byte, up to viaRawMax. Canonical ids are
	// what every production generation carries; the second form keeps the
	// table exact for a fixture's short ids.
	viaSlotBytes = 1 + 32
	viaRawMax    = 31
)

// routeHash is a route's identity: hash(parent route's hash, via), seeded with
// the zero hash at a seed. It is derived from canonical ids alone, so a fresh
// index and a delta-built one give the same route the same hash, and two
// routes compare in O(1) wherever the fold has to tell them apart. 128 bits
// make a collision between two routes of one answer negligible.
type routeHash [16]byte

// extend is the hash of the route r followed by via.
func (r routeHash) extend(via model.RelationID) routeHash {
	h := sha256.New()
	h.Write(r[:])
	h.Write([]byte(via))
	var out routeHash
	copy(out[:], h.Sum(nil))
	return out
}

// compare orders two hashes bytewise.
func (r routeHash) compare(o routeHash) int { return bytes.Compare(r[:], o[:]) }

// MarshalText and UnmarshalText spell the hash as hex in a JSON record.
func (r routeHash) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(r[:])), nil }

func (r *routeHash) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*r = routeHash{}
		return nil
	}
	if hex.DecodedLen(len(b)) != len(r) {
		return errors.New("route hash has the wrong length")
	}
	_, err := hex.Decode(r[:], b)
	return err
}

// routeTable is the append side of the table, held by a walk while it admits.
type routeTable struct {
	home *dirMaker
	f    *os.File
	w    *bufio.Writer
	// rows is how many rows the file holds, so the next id is rows+1.
	rows uint64
}

func openRouteTable(home *dirMaker) *routeTable { return &routeTable{home: home} }

// open opens the file for append and counts its rows. A partial trailing row is
// what a crash between two writes leaves; it belongs to no admission that was
// committed, and is cut off.
func (t *routeTable) open() error {
	if t.w != nil {
		return nil
	}
	dir, err := t.home.ensure()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, routeTableFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return internalErr("graph: the route table: " + err.Error())
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return internalErr("graph: the route table: " + err.Error())
	}
	whole := st.Size() / routeRowBytes * routeRowBytes
	if whole != st.Size() {
		if err := f.Truncate(whole); err != nil {
			_ = f.Close()
			return internalErr("graph: the route table: " + err.Error())
		}
	}
	if _, err := f.Seek(whole, io.SeekStart); err != nil {
		_ = f.Close()
		return internalErr("graph: the route table: " + err.Error())
	}
	t.f, t.w, t.rows = f, bufio.NewWriter(paced.NewWriter(f)), uint64(whole/routeRowBytes)
	return nil
}

// admit appends the row of one admission reached from parent over via and
// answers its id.
func (t *routeTable) admit(parent uint64, via model.RelationID) (uint64, error) {
	if err := t.open(); err != nil {
		return 0, err
	}
	if parent > t.rows {
		return 0, internalErr("graph: an admission names a parent the route table does not hold")
	}
	var row [routeRowBytes]byte
	binary.LittleEndian.PutUint64(row[:8], parent)
	if err := putVia(row[8:], via); err != nil {
		return 0, err
	}
	if _, err := t.w.Write(row[:]); err != nil {
		return 0, internalErr("graph: the route table: " + err.Error())
	}
	t.rows++
	return t.rows, nil
}

// sync makes every row written so far durable. A level's admitted states name
// their rows, so the rows are synced before the level that names them is.
func (t *routeTable) sync() error {
	if t.w == nil {
		return nil
	}
	if err := t.w.Flush(); err != nil {
		return internalErr("graph: the route table: " + err.Error())
	}
	if err := t.f.Sync(); err != nil {
		return internalErr("graph: the route table: " + err.Error())
	}
	return nil
}

func (t *routeTable) close() error {
	if t.f == nil {
		return nil
	}
	err := t.w.Flush()
	if cerr := t.f.Close(); err == nil {
		err = cerr
	}
	t.f, t.w = nil, nil
	if err != nil {
		return internalErr("graph: the route table: " + err.Error())
	}
	return nil
}

// path is the table's file in dir.
func routeTablePath(dir string) string { return filepath.Join(dir, routeTableFile) }

func putVia(slot []byte, via model.RelationID) error {
	if model.ValidHexID(string(via)) {
		slot[0] = 0
		_, err := hex.Decode(slot[1:33], []byte(via))
		return err
	}
	if len(via) == 0 || len(via) > viaRawMax {
		return internalErr("graph: a route relation id does not fit the route table")
	}
	slot[0] = 1
	slot[1] = byte(len(via))
	copy(slot[2:], via)
	return nil
}

func getVia(slot []byte) (model.RelationID, error) {
	switch slot[0] {
	case 0:
		return model.RelationID(hex.EncodeToString(slot[1:33])), nil
	case 1:
		n := int(slot[1])
		if n == 0 || n > viaRawMax {
			return "", routeCorrupt()
		}
		return model.RelationID(slot[2 : 2+n]), nil
	default:
		return "", routeCorrupt()
	}
}

func routeCorrupt() error {
	return &model.Error{Code: model.CodeStorageCorrupt, Message: "graph: the route table is not readable"}
}

// routeReader rebuilds routes from a table file, one positioned read per hop.
type routeReader struct {
	f *os.File
}

// openRouteReader opens the table at path for reading. A table that was never
// written holds no admission, which is what a walk of seeds alone leaves.
func openRouteReader(path string) (*routeReader, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &routeReader{}, nil
	}
	if err != nil {
		return nil, internalErr("graph: the route table: " + err.Error())
	}
	return &routeReader{f: f}, nil
}

func (r *routeReader) close() error {
	if r == nil || r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// route answers the relations from a seed to the admission adm, in walk order,
// followed by via, the relation a record took out of that admission. Every
// parent precedes its child in the table, so each step moves to a smaller id
// and the read ends; a row that pointed forward would be damage, and is
// reported as such rather than followed.
func (r *routeReader) route(adm uint64, via model.RelationID) ([]model.RelationID, error) {
	var out []model.RelationID
	if via != "" {
		out = append(out, via)
	}
	var row [routeRowBytes]byte
	for adm != 0 {
		if r.f == nil {
			return nil, routeCorrupt()
		}
		if _, err := r.f.ReadAt(row[:], int64(adm-1)*routeRowBytes); err != nil {
			return nil, routeCorrupt()
		}
		parent := binary.LittleEndian.Uint64(row[:8])
		if parent >= adm {
			return nil, routeCorrupt()
		}
		rel, err := getVia(row[8:])
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
		adm = parent
	}
	slices.Reverse(out)
	return out, nil
}

// adoptRoutes moves the walk's route table into a directory of its own and
// adopts it under next's lease, so the answer's later pages can read the routes
// of the records they serve after the walk's own state is gone. The directory
// is the answer's: it also records which views have been served whole
// (finishView), so finished names the views the minting page completed. A walk
// whose every record left a seed directly wrote no row, and its directory holds
// no table.
func (e *Engine) adoptRoutes(next traversalCursor, w *retainedWalk, finished []model.ImpactView) (string, error) {
	home, err := pendingDir(e.walkScratchDir())
	if err != nil {
		return "", err
	}
	dir, err := home.ensure()
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = paced.RemoveAll(dir)
		return "", err
	}
	if w.routes != nil && w.home.exists() {
		if err := w.routes.close(); err != nil {
			return fail(err)
		}
		from := routeTablePath(w.home.path)
		if err := os.Rename(from, routeTablePath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(internalErr("graph: the route table: " + err.Error()))
		}
	}
	for _, v := range finished {
		if err := markViewDone(dir, v); err != nil {
			return fail(err)
		}
	}
	id, err := e.spools.AdoptDir(next.spoolCursor(), dir)
	if err != nil {
		return fail(err)
	}
	return id, nil
}

func viewDonePath(dir string, v model.ImpactView) string {
	return filepath.Join(dir, "done-"+string(v))
}

func markViewDone(dir string, v model.ImpactView) error {
	f, err := os.OpenFile(viewDonePath(dir, v), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return internalErr("graph: the answer state: " + err.Error())
	}
	return f.Close()
}

// finishView records that view of the answer c reads has been served whole, and
// releases the answer -- its spool, its route table and its lease -- once every
// view has been. An answer whose client reads only some views is reclaimed by
// its lease's expiry instead, as an answer paged only part of the way is.
func (e *Engine) finishView(ctx context.Context, c traversalCursor, view model.ImpactView) {
	if c.RouteID == "" {
		// The answer listed no entity: its only view is the entries view.
		e.releaseConsumed(ctx, c.SpoolID, c.LeaseID)
		return
	}
	rc := c.spoolCursor()
	rc.SpoolID = c.RouteID
	dir, err := e.spools.OpenDir(ctx, rc, e.now())
	if err != nil {
		return
	}
	if markViewDone(dir, view) != nil {
		return
	}
	for _, v := range model.ImpactViews {
		if _, err := os.Stat(viewDonePath(dir, v)); err != nil {
			return
		}
	}
	e.releaseConsumed(ctx, c.RouteID, "")
	e.releaseConsumed(ctx, c.SpoolID, c.LeaseID)
}

// openRankedRoutes opens the route table a ranked continuation names. A
// continuation that names none serves records that name no route.
func (e *Engine) openRankedRoutes(ctx context.Context, c traversalCursor) (*routeReader, error) {
	if c.RouteID == "" {
		return &routeReader{}, nil
	}
	rc := c.spoolCursor()
	rc.SpoolID = c.RouteID
	dir, err := e.spools.OpenDir(ctx, rc, e.now())
	if err != nil {
		return nil, err
	}
	return openRouteReader(routeTablePath(dir))
}
