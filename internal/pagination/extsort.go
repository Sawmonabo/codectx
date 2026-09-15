package pagination

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"slices"
)

// External sort — a disk-backed sort for a sequence too long to hold in heap.
//
// It is a new primitive rather than a use of Spools because a Spools spool is
// bound to a lease, a generation and a query hash: it is continuation state for
// a paginated ANSWER. This sorts an INDEX-TIME sequence that belongs to no
// query and outlives no request, so it borrows the spool file conventions (0600
// files, length-prefixed records, a per-record ceiling so a reader never
// allocates from an untrusted length) and none of its lease machinery.
//
// The shape is the classic external merge sort: fill a fixed-size run buffer,
// sort it, write it out as a sorted run, and merge the runs with one k-way
// heap. Peak RSS is the run buffer plus one bufio block per run in the merge --
// a function of the buffer and the fan-in, never of the input length.

// RunBufferRecords is how many records one run buffer holds. It is the sort's
// memory knob: peak heap is this many records plus mergeBlockBytes per run.
const RunBufferRecords = 1 << 16

// mergeBlockBytes is the read buffer held per run during the merge, so the
// merge's own cost is fan-in x this.
const mergeBlockBytes = 1 << 16

// ExternalSort accumulates records and answers them in sorted order. Records
// are encoded with Encode and compared with Compare; both must be pure, and
// Compare must be a total order, or the merged output is not sorted.
//
// It is not safe for concurrent use: one sort belongs to one producer.
type ExternalSort[T any] struct {
	dir     string
	prefix  string
	encode  func(T) ([]byte, error)
	decode  func([]byte) (T, error)
	compare func(a, b T) int
	bufN    int

	buf   []T
	runs  []string
	count int64
	err   error
}

// NewExternalSort opens a sort whose spill files live under dir (created 0700)
// and are named with prefix. bufRecords is the run buffer size; zero takes
// RunBufferRecords.
func NewExternalSort[T any](dir, prefix string, bufRecords int,
	encode func(T) ([]byte, error), decode func([]byte) (T, error), compare func(a, b T) int,
) (*ExternalSort[T], error) {
	if dir == "" || encode == nil || decode == nil || compare == nil {
		return nil, internalErr("an external sort needs a directory, a codec and a comparator")
	}
	if bufRecords <= 0 {
		bufRecords = RunBufferRecords
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, internalErr("external sort directory: " + err.Error())
	}
	return &ExternalSort[T]{dir: dir, prefix: prefix, encode: encode, decode: decode,
		compare: compare, bufN: bufRecords, buf: make([]T, 0, bufRecords)}, nil
}

// Add offers one record. It spills a run whenever the buffer is full, so the
// caller's heap never grows with the number of records added.
func (s *ExternalSort[T]) Add(v T) error {
	if s.err != nil {
		return s.err
	}
	s.buf = append(s.buf, v)
	s.count++
	if len(s.buf) < s.bufN {
		return nil
	}
	return s.spill()
}

// Len is how many records have been added.
func (s *ExternalSort[T]) Len() int64 { return s.count }

// spill sorts the run buffer and writes it out as one sorted run.
//
// The sort is stable so that the merge below, which breaks ties by run order
// and then by position within a run, reproduces exactly the order a single
// stable in-memory sort of the whole input would have produced. Callers whose
// keys are unique get the same answer either way; callers whose keys are not
// get a defined one.
func (s *ExternalSort[T]) spill() error {
	if len(s.buf) == 0 {
		return nil
	}
	slices.SortStableFunc(s.buf, s.compare)
	f, err := os.CreateTemp(s.dir, s.prefix+"run-*")
	if err != nil {
		s.err = internalErr("external sort run: " + err.Error())
		return s.err
	}
	w := bufio.NewWriterSize(f, mergeBlockBytes)
	for _, v := range s.buf {
		if err := s.writeRecord(w, v); err != nil {
			f.Close()
			os.Remove(f.Name())
			s.err = err
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(f.Name())
		s.err = internalErr("external sort run: " + err.Error())
		return s.err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		s.err = internalErr("external sort run: " + err.Error())
		return s.err
	}
	s.runs = append(s.runs, f.Name())
	s.buf = s.buf[:0]
	return nil
}

func (s *ExternalSort[T]) writeRecord(w *bufio.Writer, v T) error {
	b, err := s.encode(v)
	if err != nil {
		return err
	}
	if len(b) > maxSpoolRecordBytes {
		return internalErr("external sort record exceeds the record ceiling")
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	if _, err := w.Write(hdr[:n]); err != nil {
		return internalErr("external sort run: " + err.Error())
	}
	if _, err := w.Write(b); err != nil {
		return internalErr("external sort run: " + err.Error())
	}
	return nil
}

// Sorted ends the sort and answers a re-iterable sorted sequence. Nothing was
// spilled when the whole input fit one run buffer, and then the answer is that
// buffer -- a repository small enough to sort in heap pays no file at all.
// Otherwise the runs are merged once into a single sorted file and the run
// files are removed. Close the result to remove that file.
//
// Add must not be called afterwards.
func (s *ExternalSort[T]) Sorted() (*SortedRun[T], error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.runs) == 0 {
		slices.SortStableFunc(s.buf, s.compare)
		return &SortedRun[T]{mem: s.buf, count: s.count}, nil
	}
	if err := s.spill(); err != nil {
		return nil, err
	}
	out, err := os.CreateTemp(s.dir, s.prefix+"sorted-*")
	if err != nil {
		s.removeRuns()
		return nil, internalErr("external sort output: " + err.Error())
	}
	err = s.merge(out)
	closeErr := out.Close()
	s.removeRuns()
	if err == nil && closeErr != nil {
		err = internalErr("external sort output: " + closeErr.Error())
	}
	if err != nil {
		os.Remove(out.Name())
		return nil, err
	}
	return &SortedRun[T]{path: out.Name(), decode: s.decode, count: s.count}, nil
}

// merge is the k-way merge: one open reader and one heap entry per run, so the
// merge holds fan-in records and fan-in read blocks, whatever the runs hold.
func (s *ExternalSort[T]) merge(out *os.File) error {
	readers := make([]*runReader[T], 0, len(s.runs))
	defer func() {
		for _, r := range readers {
			r.file.Close()
		}
	}()
	h := &mergeHeap[T]{compare: s.compare}
	for i, name := range s.runs {
		f, err := os.Open(name)
		if err != nil {
			return internalErr("external sort run: " + err.Error())
		}
		r := &runReader[T]{file: f, br: bufio.NewReaderSize(f, mergeBlockBytes), decode: s.decode, run: i}
		readers = append(readers, r)
		v, ok, err := r.next()
		if err != nil {
			return err
		}
		if ok {
			h.items = append(h.items, mergeItem[T]{value: v, run: i})
		}
	}
	heap.Init(h)
	w := bufio.NewWriterSize(out, mergeBlockBytes)
	for h.Len() > 0 {
		it := h.items[0]
		if err := s.writeRecord(w, it.value); err != nil {
			return err
		}
		v, ok, err := readers[it.run].next()
		if err != nil {
			return err
		}
		if ok {
			h.items[0] = mergeItem[T]{value: v, run: it.run}
			heap.Fix(h, 0)
			continue
		}
		heap.Remove(h, 0)
	}
	if err := w.Flush(); err != nil {
		return internalErr("external sort output: " + err.Error())
	}
	return nil
}

func (s *ExternalSort[T]) removeRuns() {
	for _, name := range s.runs {
		os.Remove(name)
	}
	s.runs = nil
	s.buf = nil
}

// Close releases everything the sort holds without producing an answer. It is
// safe after Sorted, which already removed the runs.
func (s *ExternalSort[T]) Close() error {
	s.removeRuns()
	return nil
}

// runReader streams one sorted run back.
type runReader[T any] struct {
	file   *os.File
	br     *bufio.Reader
	decode func([]byte) (T, error)
	run    int
}

func (r *runReader[T]) next() (T, bool, error) {
	var zero T
	n, err := binary.ReadUvarint(r.br)
	if errors.Is(err, io.EOF) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, internalErr("external sort run: " + err.Error())
	}
	if n > maxSpoolRecordBytes {
		return zero, false, internalErr("external sort run record exceeds the record ceiling")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r.br, b); err != nil {
		return zero, false, internalErr("external sort run: " + err.Error())
	}
	v, err := r.decode(b)
	if err != nil {
		return zero, false, err
	}
	return v, true, nil
}

// mergeItem is one run's current record. run is both the reader index and the
// tie-break: equal records leave the heap in run order, and runs were written
// in arrival order, so the merge reproduces a stable sort of the whole input.
type mergeItem[T any] struct {
	value T
	run   int
}

type mergeHeap[T any] struct {
	items   []mergeItem[T]
	compare func(a, b T) int
}

func (h *mergeHeap[T]) Len() int { return len(h.items) }
func (h *mergeHeap[T]) Less(i, j int) bool {
	if c := h.compare(h.items[i].value, h.items[j].value); c != 0 {
		return c < 0
	}
	return h.items[i].run < h.items[j].run
}
func (h *mergeHeap[T]) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *mergeHeap[T]) Push(x any)    { h.items = append(h.items, x.(mergeItem[T])) }
func (h *mergeHeap[T]) Pop() any {
	n := len(h.items) - 1
	v := h.items[n]
	h.items = h.items[:n]
	return v
}

// SortedRun is the sorted answer: a slice when the input fit one run buffer, a
// file otherwise. Each may be called any number of times -- a file-backed run
// reopens the file per call -- which is what lets one caller fold a digest over
// the sequence and another stream the same sequence again later.
type SortedRun[T any] struct {
	mem    []T
	path   string
	decode func([]byte) (T, error)
	count  int64
	closed bool
}

// Len is how many records the run holds.
func (r *SortedRun[T]) Len() int64 {
	if r == nil {
		return 0
	}
	return r.count
}

// Each yields every record in sorted order. It stops and returns the callback's
// error unchanged, so a caller may end the walk with a typed error of its own.
func (r *SortedRun[T]) Each(yield func(T) error) error {
	if r == nil {
		return nil
	}
	// A closed run must never read as an EMPTY one. Its records fold a unit's
	// identity digest, and the empty digest is the same whatever the snapshot
	// holds -- an identity that reuses forever no matter what changed, which is
	// exactly the degradation the planner refuses by hand elsewhere. A lifetime
	// mistake is therefore a typed error, never a silent zero-record walk.
	if r.closed {
		return internalErr("this sorted run was closed; its records were read after the value that owns it was released")
	}
	if r.path == "" {
		for _, v := range r.mem {
			if err := yield(v); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := os.Open(r.path)
	if err != nil {
		return internalErr("external sort output: " + err.Error())
	}
	defer f.Close()
	rr := &runReader[T]{file: f, br: bufio.NewReaderSize(f, mergeBlockBytes), decode: r.decode}
	for {
		v, ok, err := rr.next()
		if err != nil || !ok {
			return err
		}
		if err := yield(v); err != nil {
			return err
		}
	}
}

// Close removes the run's backing file. A run held in heap has none.
func (r *SortedRun[T]) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	r.mem = nil
	path := r.path
	r.path = ""
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return internalErr("external sort output: " + err.Error())
	}
	return nil
}
