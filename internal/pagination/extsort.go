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

// MaxSortFanIn bounds how many runs one merge reads at once, and with it how
// many records the merge holds live. Without a cap the merge heap is one
// record per run and the run count is (records / run buffer) -- proportional
// to the input, which is exactly the property this primitive exists to remove.
// Runs beyond the cap are collapsed in groups into fewer, longer runs first,
// so the depth grows logarithmically while the live set stays constant. The
// value follows the small constant fan-in production external sorts use [S15].
//
// It is exported because it is half of the memory envelope a caller's bound
// assertion is written against: a test must read the design constant rather
// than restate it.
const MaxSortFanIn = 16

// minSortRunBytes floors a byte-budgeted run. A run smaller than this makes
// the run count, and so the number of collapse passes, explode for no memory
// saving worth having.
const minSortRunBytes = 64 << 10

// sortRecordOverheadBytes is what one buffered record costs beyond the bytes
// it reports: the slice or struct header and the allocator's rounding. A run
// of many small records is charged honestly rather than looking free.
const sortRecordOverheadBytes = 48

// SortRunBytes derives one sort's in-memory run budget from the query memory
// admission (resources.query_memory_bytes). A query is admitted against that
// number and a sort is one of several structures it pays for at once -- the
// pinned reader, the page being built and the sort's own merge all draw on it
// -- so a sort takes a quarter of it and never less than minSortRunBytes.
//
// It is a share of the admission rather than a key of its own on purpose: a
// second key could be set so the parts oversubscribe the whole, which is the
// failure the single admission exists to prevent. The floor raises rather than
// refuses: a performance knob must never be the reason a query fails.
func SortRunBytes(queryMemoryBytes int64) int64 {
	if run := queryMemoryBytes / 4; run > minSortRunBytes {
		return run
	}
	return minSortRunBytes
}

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

	// fold, when set, merges two records the comparator reports as equal into
	// the one that survives them. See WithFold for why it runs exactly once.
	fold func(a, b T) (T, error)
	// runBytes and sizeOf are the optional byte budget: a run is spilled when
	// the buffered records reach runBytes, whichever comes first with bufN.
	runBytes int64
	sizeOf   func(T) int64

	buf      []T
	bufBytes int64
	runs     []string
	count    int64
	// peakRecords is the high-water mark of records held in memory at once,
	// across buffering and every merge pass. It is the structural memory
	// assertion: a test asserts it stays within the envelope the run budget
	// and the fan-in cap define as the input grows, which total allocation
	// volume (which scales with the input even for a perfect external sort)
	// cannot show.
	peakRecords int
	err         error
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

// WithFold sets the fold that collapses records the comparator reports as
// equal, and returns s so a caller can chain it onto the constructor.
//
// The fold runs EXACTLY ONCE, left to right, over the fully ordered stream --
// never at spill time and never during a collapse pass. That is what makes it
// byte-identical to folding the same arrivals in a map as they arrive: runs
// are stable-sorted and the merge breaks ties by run index over runs written
// in arrival order, so equal records reach the fold in arrival order. Folding
// at every level instead would require the fold to be associative AND
// commutative, which a fold that takes one side's payload is not.
func (s *ExternalSort[T]) WithFold(fold func(a, b T) (T, error)) *ExternalSort[T] {
	s.fold = fold
	return s
}

// WithRunBytes budgets the run buffer in bytes as well as in records, using
// sizeOf to charge each buffered record. A caller whose records vary in size
// needs this: a record count alone bounds the heap only when every record is
// the same size. Callers derive runBytes with SortRunBytes.
//
// A budget below minSortRunBytes is raised to it rather than refused. A nil
// sizeOf leaves the byte budget OFF and the record count the only bound, which
// is the right answer for a caller whose records are all one size.
func (s *ExternalSort[T]) WithRunBytes(runBytes int64, sizeOf func(T) int64) *ExternalSort[T] {
	if sizeOf == nil {
		return s
	}
	if runBytes < minSortRunBytes {
		runBytes = minSortRunBytes
	}
	s.runBytes, s.sizeOf = runBytes, sizeOf
	return s
}

// PeakLiveRecords reports the largest in-memory working set this sort ever
// held. It is the memory invariant a test asserts on; reading it never changes
// the sort.
func (s *ExternalSort[T]) PeakLiveRecords() int { return s.peakRecords }

// observe records a live-set high-water mark.
func (s *ExternalSort[T]) observe(records int) {
	if records > s.peakRecords {
		s.peakRecords = records
	}
}

// Add offers one record. It spills a run whenever the buffer is full, so the
// caller's heap never grows with the number of records added.
func (s *ExternalSort[T]) Add(v T) error {
	if s.err != nil {
		return s.err
	}
	s.buf = append(s.buf, v)
	s.count++
	s.observe(len(s.buf))
	if s.sizeOf != nil {
		s.bufBytes += s.sizeOf(v) + sortRecordOverheadBytes
		if s.bufBytes >= s.runBytes {
			return s.spill()
		}
	}
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
	s.buf, s.bufBytes = s.buf[:0], 0
	return nil
}

// maxSortRecordBytes bounds one encoded sort record before its buffer is
// allocated on read (a pre-allocation guard, not a work limit): a run file is
// trusted only as far as its header, and a corrupt length must not allocate.
// It equals the spool's chunk size; a planner input record is a few hundred
// bytes, so the bound is never the reason a sort fails.
const maxSortRecordBytes = maxSpoolChunkBytes

func (s *ExternalSort[T]) writeRecord(w *bufio.Writer, v T) error {
	b, err := s.encode(v)
	if err != nil {
		return err
	}
	if len(b) > maxSortRecordBytes {
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
//
// On the ERROR path the run files are not always removed: a failure before the
// merge (spill, collapse, output creation) leaves the spilled runs on disk, so
// the caller keeps its obligation to Close this sort whether Sorted succeeded
// or failed. Every caller already defers Close, and that is the contract, not
// an accident: Sorted owns removal only on the path that consumed the runs.
func (s *ExternalSort[T]) Sorted() (*SortedRun[T], error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.runs) == 0 {
		slices.SortStableFunc(s.buf, s.compare)
		buf, err := s.foldBuffer(s.buf)
		if err != nil {
			return nil, err
		}
		return &SortedRun[T]{mem: buf, count: int64(len(buf))}, nil
	}
	if err := s.spill(); err != nil {
		return nil, err
	}
	if err := s.collapse(); err != nil {
		return nil, err
	}
	out, err := os.CreateTemp(s.dir, s.prefix+"sorted-*")
	if err != nil {
		s.removeRuns()
		return nil, internalErr("external sort output: " + err.Error())
	}
	n, err := s.merge(out, s.runs, s.fold != nil)
	closeErr := out.Close()
	s.removeRuns()
	if err == nil && closeErr != nil {
		err = internalErr("external sort output: " + closeErr.Error())
	}
	if err != nil {
		os.Remove(out.Name())
		return nil, err
	}
	return &SortedRun[T]{path: out.Name(), decode: s.decode, count: n}, nil
}

// foldBuffer collapses equal neighbours of an already-ordered buffer. It is
// the no-spill path's half of the fold, and folds left to right exactly as the
// merge does, so a sort that fit its buffer and one that spilled answer the
// same records.
func (s *ExternalSort[T]) foldBuffer(buf []T) ([]T, error) {
	if s.fold == nil || len(buf) < 2 {
		return buf, nil
	}
	out := buf[:1]
	for _, v := range buf[1:] {
		last := out[len(out)-1]
		if s.compare(last, v) == 0 {
			folded, err := s.fold(last, v)
			if err != nil {
				return nil, err
			}
			out[len(out)-1] = folded
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// collapse merges runs in groups until one merge pass can read them all at
// once, so the final merge never holds more than MaxSortFanIn records live
// however many runs the input produced.
//
// Each group's input runs are removed as soon as THAT group's merge completes,
// not after the whole pass: removing them at the end of a pass would hold a
// full second copy of the run set on disk for the length of the pass, so peak
// temporary disk would be twice the input against a shared byte budget that
// continuation spools also draw on. Removing per group keeps the peak at the
// run set plus the one run currently being written.
func (s *ExternalSort[T]) collapse() error {
	for len(s.runs) > MaxSortFanIn {
		next := make([]string, 0, (len(s.runs)+MaxSortFanIn-1)/MaxSortFanIn)
		for i := 0; i < len(s.runs); i += MaxSortFanIn {
			group := s.runs[i:min(i+MaxSortFanIn, len(s.runs))]
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}
			merged, err := s.mergeToRun(group)
			if err != nil {
				// Everything still on disk stays on s.runs so removeRuns and
				// Close clean it up: the merged outputs so far, and the groups
				// this pass has not reached.
				s.runs = append(next, s.runs[i:]...)
				return err
			}
			next = append(next, merged)
		}
		s.runs = next
	}
	return nil
}

// mergeToRun merges one group into a single new run and removes the group.
// A collapse pass never folds: the fold runs exactly once, over the fully
// ordered final stream (WithFold).
func (s *ExternalSort[T]) mergeToRun(group []string) (string, error) {
	f, err := os.CreateTemp(s.dir, s.prefix+"run-*")
	if err != nil {
		return "", internalErr("external sort run: " + err.Error())
	}
	_, err = s.merge(f, group, false)
	closeErr := f.Close()
	if err == nil && closeErr != nil {
		err = internalErr("external sort run: " + closeErr.Error())
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	for _, name := range group {
		os.Remove(name)
	}
	return f.Name(), nil
}

// merge is the k-way merge over runs: one open reader and one heap entry per
// run, so the merge holds len(runs) records and len(runs) read blocks whatever
// the runs hold, and collapse keeps len(runs) at MaxSortFanIn. It returns how
// many records it wrote, which is fewer than it read when fold is set.
func (s *ExternalSort[T]) merge(out *os.File, runs []string, fold bool) (int64, error) {
	readers := make([]*runReader[T], 0, len(runs))
	defer func() {
		for _, r := range readers {
			r.file.Close()
		}
	}()
	h := &mergeHeap[T]{compare: s.compare}
	for i, name := range runs {
		f, err := os.Open(name)
		if err != nil {
			return 0, internalErr("external sort run: " + err.Error())
		}
		r := &runReader[T]{file: f, br: bufio.NewReaderSize(f, mergeBlockBytes), decode: s.decode, run: i}
		readers = append(readers, r)
		v, ok, err := r.next()
		if err != nil {
			return 0, err
		}
		if ok {
			h.items = append(h.items, mergeItem[T]{value: v, run: i})
		}
	}
	heap.Init(h)
	s.observe(len(h.items))
	w := bufio.NewWriterSize(out, mergeBlockBytes)
	var n int64
	for h.Len() > 0 {
		v, err := s.pop(h, readers)
		if err != nil {
			return 0, err
		}
		if fold {
			// Equal records reach here in arrival order, so this reproduces
			// folding the same arrivals left to right as they were offered.
			for h.Len() > 0 && s.compare(v, h.items[0].value) == 0 {
				dup, err := s.pop(h, readers)
				if err != nil {
					return 0, err
				}
				if v, err = s.fold(v, dup); err != nil {
					return 0, err
				}
			}
		}
		if err := s.writeRecord(w, v); err != nil {
			return 0, err
		}
		n++
	}
	if err := w.Flush(); err != nil {
		return 0, internalErr("external sort output: " + err.Error())
	}
	return n, nil
}

// pop takes the heap's head and refills its run's slot.
func (s *ExternalSort[T]) pop(h *mergeHeap[T], readers []*runReader[T]) (T, error) {
	it := h.items[0]
	v, ok, err := readers[it.run].next()
	if err != nil {
		var zero T
		return zero, err
	}
	if ok {
		h.items[0] = mergeItem[T]{value: v, run: it.run}
		heap.Fix(h, 0)
	} else {
		heap.Remove(h, 0)
	}
	return it.value, nil
}

func (s *ExternalSort[T]) removeRuns() {
	for _, name := range s.runs {
		os.Remove(name)
	}
	s.runs = nil
	s.buf, s.bufBytes = nil, 0
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
	if n > maxSortRecordBytes {
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

// SortDir is the directory a sort should spill into so that a query's runs sit
// beside the continuation spools of the same store rather than in a second
// place an operator has to know about.
//
// Run bytes are NOT charged against the store's shared reservation, and that
// is a decision rather than an oversight. resources.max_temp_bytes bounds
// continuation state: bytes that outlive the request that wrote them and pin a
// generation against retention until a lease expires. A sort run is sized by
// the match count and is created and removed inside one request, so charging
// it there would silently repurpose the key -- one query's ranking could
// exhaust the budget another query's continuation needs, and an operator
// raising it to hold more pages would instead be raising how wide a query a
// single request may rank. Sweep and diskBytes still see the files, so runs
// are reported and reclaimed as real disk exactly as a spool is.
func (s *Spools) SortDir() string { return s.dir }
