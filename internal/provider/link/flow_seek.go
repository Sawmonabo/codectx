package link

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"io"
	"os"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// A refresh of a flow unit asks the predecessor's contributions the same few
// questions on every iteration of its region (refresh.go): what the
// region's nodes contributed, which entries they contributed to, and what
// every node put into a handful of entries. Each answer is a key range of a
// sorted run, so the runs are written once per refresh to block-indexed
// files (seekFile), as the hierarchy's indexes are, and each question reads
// only the blocks of its keys: an iteration costs the region's keys, never a
// pass over the unit's stored contributions. Only the first record of each
// block is held in memory.

// seekFile is a sorted run on disk with the first record of each block of
// about indexBlockBytes, which a range read starts from.
type seekFile[T any] struct {
	lease *scratch.Lease
	f     *os.File
	size  int64
	c     codec[T]
	first []T
	off   []int64
}

// writeSeek writes run's records, in order, to a seekFile in dir.
func writeSeek[T any](dir string, run *pagination.SortedRun[T], c codec[T]) (*seekFile[T], error) {
	lease, f, err := scratch.For(dir).TakeFile(scratch.SortRun)
	if err != nil {
		return nil, err
	}
	s := &seekFile[T]{lease: lease, f: f, c: c}
	w := bufio.NewWriter(f)
	block := int64(-indexBlockBytes)
	err = run.Each(func(v T) error {
		var b wbuf
		c.put(&b, v)
		if s.size-block >= indexBlockBytes {
			block = s.size
			s.first = append(s.first, v)
			s.off = append(s.off, s.size)
		}
		n, err := w.Write(binary.AppendUvarint(nil, uint64(len(b.b))))
		if err != nil {
			return err
		}
		m, err := w.Write(b.b)
		s.size += int64(n + m)
		return err
	})
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// span yields, in order, every record r with at(r) == 0, given at < 0 for
// every record before them and > 0 for every record after them.
func (s *seekFile[T]) span(at func(T) int, yield func(T) error) error {
	if s == nil || len(s.first) == 0 {
		return nil
	}
	// Every block before the one preceding the first block whose first
	// record is not before the span ends before it.
	i, _ := slices.BinarySearchFunc(s.first, 0, func(v T, _ int) int {
		if at(v) < 0 {
			return -1
		}
		return 1
	})
	i = max(i-1, 0)
	r := bufio.NewReader(io.NewSectionReader(s.f, s.off[i], s.size-s.off[i]))
	var buf []byte
	for {
		n, err := binary.ReadUvarint(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		buf = slices.Grow(buf[:0], int(n))[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		rb := rbuf{b: buf}
		v := s.c.get(&rb)
		if rb.bad || len(rb.b) != 0 {
			return internalErr("a refresh's seek file holds a malformed record")
		}
		switch c := at(v); {
		case c > 0:
			return nil
		case c == 0:
			if err := yield(v); err != nil {
				return err
			}
		}
	}
}

func (s *seekFile[T]) close() {
	if s != nil && s.lease != nil {
		s.lease.Release()
		s.lease = nil
	}
}

// ofNodes yields the stored contributions of every node of set (an Index of
// wholeFile is every node of its member), node by node.
func (fr *flowRefresh) ofNodes(set *pagination.SortedRun[flowNode], yield func(idContribution) error) error {
	var whole model.FileID
	have := false
	return set.Each(func(n flowNode) error {
		if have && n.File == whole {
			return nil
		}
		if n.Index == wholeFile {
			whole, have = n.File, true
			return fr.nodeSeek.span(func(c idContribution) int { return cmp.Compare(c.From.File, n.File) }, yield)
		}
		return fr.nodeSeek.span(func(c idContribution) int { return cmpNode(c.From, n) }, yield)
	})
}

// ofEntries yields every stored contribution into each entry of entries,
// entry by entry.
func (fr *flowRefresh) ofEntries(entries *pagination.SortedRun[idVar], yield func(idContribution) error) error {
	return entries.Each(func(v idVar) error {
		return fr.entrySeek.span(func(c idContribution) int { return cmpIDVar(c.Entry, v) }, yield)
	})
}

// outside adds to add every contribution fill yields whose node set does
// not hold, through a sort by node (what fill yields is bounded by the keys
// it was asked).
func (fr *flowRefresh) outside(set *pagination.SortedRun[flowNode], fill func(yield func(idContribution) error) error,
	add func(idContribution) error) error {
	byNode, err := collect(fr.u.dir, contributionsByNode, nil, fill)
	if err != nil {
		return err
	}
	defer byNode.Close()
	return filterNodes(byNode, set, func(c idContribution) flowNode { return c.From }, false, add)
}

// entriesOf is the entries each contribution fill yields names, each once.
func entriesOf(dir string, fill func(yield func(idContribution) error) error) (*pagination.SortedRun[idVar], error) {
	return collect(dir, idVarCodec, keepFirst[idVar], func(add func(idVar) error) error {
		return fill(func(c idContribution) error { return add(c.Entry) })
	})
}

// contributionsByFrom orders ordinal contributions by their node first.
var contributionsByFrom = codec[contribution]{put: contributionCodec.put, get: contributionCodec.get,
	cmp: func(a, b contribution) int { return cmp.Or(compareRef(a.From, b.From), contributionCodec.cmp(a, b)) }}

// inputs answers a run of the region excluded holds the stored
// contributions of every node outside it (flowInputs): into each variable
// the run reads, one key range each, and every callable's returns, one
// range, read from the contributions translated to this build's ordinals
// once per refresh (ordSeek). release ends it.
func (fr *flowRefresh) inputs(excluded *pagination.SortedRun[flowNode]) (in flowInputs, release func(), err error) {
	dir := fr.u.dir
	ex, err := fr.ordinals(excluded)
	if err != nil {
		return nil, nil, err
	}
	in = func(wants *pagination.SortedRun[variable]) (*pagination.SortedRun[contribution], error) {
		hits, err := collect(dir, contributionsByFrom, nil, func(add func(contribution) error) error {
			err := fr.ordSeek.span(func(c contribution) int { return cmp.Compare(c.Entry.Kind, varReturn) }, add)
			if err != nil {
				return err
			}
			return wants.Each(func(v variable) error {
				if v.Kind == varReturn {
					return nil
				}
				return fr.ordSeek.span(func(c contribution) int { return cmpVar(c.Entry, v) }, add)
			})
		})
		if err != nil {
			return nil, err
		}
		defer hits.Close()
		return collect(dir, contributionCodec, earliest, func(add func(contribution) error) error {
			sel, err := newSelections(ex, splitRef)
			if err != nil {
				return err
			}
			defer sel.stop()
			return hits.Each(func(c contribution) error {
				s, err := sel.of(c.From.File)
				if err != nil || s.has(c.From.Index) {
					return err
				}
				c.From = Ref{}
				return add(c)
			})
		})
	}
	return in, func() { ex.Close() }, nil
}
