package link

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// A member's projection is what the hierarchy is built from of its table:
// the table without its sites, values, handlers, dynamic imports and dropped
// count, which only the member's own resolution reads, but with the argument
// each routing mount passes (mountArgument), and with the offsets
// of the Java anonymous bodies those named (javaAnonymous), so the bodies
// keep their ranks. buildHierarchy reads each table once, keeps every
// member's projection beside the hierarchy, and reads the projections in its
// second pass; the state keeps them, so a build whose layout broke reads the
// tables of the members that moved or are new and the stored projections of
// the others. A stored projection is the one its member's table had when its
// digest was last read, or one of the same interface (InterfaceDigest), which
// is all a hierarchy is built from (refresh.layout), so the hierarchy built
// from it is the hierarchy a full build builds.

// projection is one member's projection as the hierarchy keeps it: its FileID
// and its projected table in the table's stored form (Table.Encode).
type projection struct {
	File model.FileID
	Raw  string
}

func compareProjection(a, b projection) int { return cmp.Compare(a.File, b.File) }

func encodeProjection(p projection) ([]byte, error) {
	return appendString(appendString(nil, string(p.File)), p.Raw), nil
}

func decodeProjection(raw []byte) (projection, error) {
	r := byteReader{b: raw}
	p := projection{File: model.FileID(r.str()), Raw: r.str()}
	if r.err != nil || len(r.b) != 0 || p.File == "" {
		return projection{}, errMalformedProjection
	}
	return p, nil
}

var errMalformedProjection = errors.New("link: malformed member projection")

func newProjectionSort(dir string) (*pagination.ExternalSort[projection], error) {
	return pagination.NewExternalSort(dir, 0, encodeProjection, decodeProjection, compareProjection)
}

// projected is a projection's stored form: the projected table's fields and
// the anonymous body offsets its dropped parts named.
type projected struct {
	*Table
	Anonymous []uint64 `json:"anonymous_bodies,omitempty"`
}

// project is t's projection, sharing t's kept fields.
func (t *Table) project() *Table {
	c := *t
	c.Sites, c.Values, c.Handlers, c.DynamicImports, c.Dropped = nil, nil, nil, nil, 0
	// A mount's argument (mountArgument) is the one part of a site the
	// module index reads: the projection keeps it as the one argument of a
	// site of its own.
	if len(t.Routing.Mounts) > 0 {
		c.Routing.Mounts = slices.Clone(t.Routing.Mounts)
		for i := range c.Routing.Mounts {
			m := &c.Routing.Mounts[i]
			if m.Join != JoinNone {
				// A Go mount is read through its call (MountCall).
				m.Site = -1
				continue
			}
			arg, ok := mountArgument(t, m)
			if !ok {
				if m.Module == "" {
					m.Site = -1
				}
				continue
			}
			m.Site, m.Arg = int32(len(c.Sites)), 0
			c.Sites = append(c.Sites, Site{Caller: -1, Args: []wire.Type{arg}})
		}
	}
	if a := anonymousNames(t); a.rank != nil {
		c.anonymous = make([]uint64, 0, len(a.rank))
		for n := range a.rank {
			c.anonymous = append(c.anonymous, n)
		}
		slices.Sort(c.anonymous)
	}
	return &c
}

// encodeProjected is the stored form of t, a projection (project).
func (t *Table) encodeProjected() (string, error) {
	b, err := json.Marshal(projected{Table: t, Anonymous: t.anonymous})
	return string(b), err
}

// decodeProjected reads a projection encodeProjected stored.
func decodeProjected(raw string) (*Table, error) {
	p := projected{Table: &Table{}}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, stateCorrupt("a member's projection: " + err.Error())
	}
	p.Table.anonymous = p.Anonymous
	return p.Table, nil
}

// heldProjections is what a refresh's hierarchy reads in place of tables:
// prev, the predecessor's projections, for the members kept holds, those
// whose table digest held.
type heldProjections struct {
	prev *pagination.SortedRun[projection]
	kept *pagination.SortedRun[model.FileID]
}

// open returns held, which answers, member by member in ascending order, the
// stored projection of a member whose digest held and its stored form (nil
// for any other member), and the function that stops it. A nil from holds
// nothing.
func (from *heldProjections) open() (held func(model.FileID) (*Table, string, error), stop func(), err error) {
	if from == nil {
		return func(model.FileID) (*Table, string, error) { return nil, "", nil }, func() {}, nil
	}
	kept, err := newCursor(from.kept)
	if err != nil {
		return nil, nil, err
	}
	next, stopPrev := pull(from.prev)
	stop = func() {
		kept.stop()
		stopPrev()
	}
	cur, ok, err := next()
	if err != nil {
		stop()
		return nil, nil, err
	}
	held = func(id model.FileID) (*Table, string, error) {
		has, err := kept.has(id)
		if err != nil || !has {
			return nil, "", err
		}
		for ok && cur.File < id {
			if cur, ok, err = next(); err != nil {
				return nil, "", err
			}
		}
		if !ok || cur.File != id {
			return nil, "", nil
		}
		t, err := decodeProjected(cur.Raw)
		return t, cur.Raw, err
	}
	return held, stop, nil
}
