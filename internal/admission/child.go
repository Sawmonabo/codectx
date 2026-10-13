package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
)

// External tools -- the precise indexers' profile units and the language
// servers -- are reserved from their measured need, the way parser files are
// (ADR-0012 decision 5, "External indexers and language servers"), in both
// dimensions the ledger holds. A tool run's work is the bytes of the snapshot
// files under the project directory it is pointed at. Its memory need is the
// highest anonymous resident set its process tree was read at; its disk need
// is the bytes it staged -- the materialized snapshot, known exactly before it
// starts -- plus the bytes it wrote, measured. The coordinator learns memory
// and written bytes per work byte in the decaying histogram parser files use,
// keyed per repository, kind, tool, tool payload, build and work-size class,
// and persisted in the run ledger's observation store.
//
// No figure is a constant of a tool. A class with no observation is reserved
// at the whole allocation in both dimensions: it runs alone among the heavy
// children, once per repository, tool payload, build and size class, because
// nothing derivable says what it will use. Every published source on these
// tools' memory names the project, its dependency closure or the machine
// itself (a managed runtime's heap ceiling is a share of physical memory) as
// what sets it, so no per-tool slope holds across repositories.

// ChildKind is which external tool a need model is learned for.
type ChildKind uint8

const (
	// ChildIndexer is a precise indexer's profile unit.
	ChildIndexer ChildKind = iota + 1
	// ChildServer is a language server.
	ChildServer
)

// kinds are the observation store's kinds for the two dimensions of k, and
// false for a kind this package does not know.
func (k ChildKind) kinds() (memory, disk ledger.NeedKind, ok bool) {
	switch k {
	case ChildIndexer:
		return ledger.NeedIndexerMemory, ledger.NeedIndexerDisk, true
	case ChildServer:
		return ledger.NeedServerMemory, ledger.NeedServerDisk, true
	}
	return "", "", false
}

// ChildKey names one external tool's need models within a repository: the kind
// of tool, its name, and the identity of the payload the lock pinned, so a
// repinned tool learns afresh.
type ChildKey struct {
	Kind        ChildKind
	Tool        string
	Fingerprint string
}

// childBuild is this binary's build identity: the argument array, the
// environment and the materialization every external tool is run with are
// this build's, so a model learned under another build is not applied.
func childBuild() string {
	b := model.CurrentBuildInfo()
	return b.Version + " " + b.Commit
}

// ChildNeeds is the process's learned need models of external tools, loaded
// from the run ledger on first use. It is safe for concurrent use; the zero
// value is ready. Its maps are bounded by the tools and size classes a process
// runs, never by the repository's files.
type ChildNeeds struct {
	mu     sync.Mutex
	models map[childModelKey]*childModel
	// gates hold, per class whose need no run that succeeded has measured,
	// the one run of the class in flight; a gate is closed when that run's
	// holding ends. Every other run of the class waits for it before it is
	// planned, so a cold repository's projects of one tool are planned from
	// the first one's measurement rather than each at the allocation, and a
	// class known only from failed runs has one run in flight at a time.
	gates map[childModelKey]chan struct{}
}

type childModelKey struct {
	repository string
	ledger.NeedKey
}

// childModel is one dimension's decaying histogram of a class, and whether a
// run that succeeded is among what it learned from. A failed run's reading is
// a lower bound of its need: the run used at least that much before it ended.
type childModel struct {
	mu        sync.Mutex
	loaded    bool
	h         NeedHistogram
	succeeded bool
}

// childState is a child model's encoding in the observation store: the
// histogram's own and the evidence flag beside it.
type childState struct {
	Succeeded bool            `json:"succeeded"`
	Model     json.RawMessage `json:"model"`
}

func (m *childModel) marshal() ([]byte, error) {
	h, err := m.h.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return json.Marshal(childState{Succeeded: m.succeeded, Model: h})
}

// unmarshal replaces the model with a stored state, and leaves it as it was
// when the state does not decode: the next observation's state supersedes it.
func (m *childModel) unmarshal(state []byte) {
	dec := json.NewDecoder(bytes.NewReader(state))
	dec.DisallowUnknownFields()
	var st childState
	if dec.Decode(&st) != nil {
		return
	}
	var h NeedHistogram
	if h.UnmarshalBinary(st.Model) != nil {
		return
	}
	m.h.mu.Lock()
	m.h.observations, m.h.zero, m.h.buckets = h.observations, h.zero, h.buckets
	m.h.mu.Unlock()
	m.succeeded = st.Succeeded
}

// ChildWork is what one tool run is planned on.
type ChildWork struct {
	// WorkBytes are the bytes of the snapshot files under the project the
	// tool is pointed at: the size its need is learned per.
	WorkBytes int64
	// StagedBytes are the bytes of the snapshot copied for the tool before it
	// starts, held on disk for the whole run.
	StagedBytes int64
	// Projects is the repository's own projects of the tool: the half-life
	// its models are learned under.
	Projects int64
}

// ChildNeed is one tool run's reservation and what is learned from it.
type ChildNeed struct {
	// Bytes is the memory the run reserves: the model's prediction for its
	// work, or the allocation when nothing is learned yet, and never more
	// than the allocation it was planned against nor less than one byte, so a
	// runner always tracks the child's residency.
	Bytes int64
	// DiskBytes is the temporary disk it reserves: the staged bytes plus the
	// predicted written bytes, or at least the whole disk allocation when
	// nothing is learned yet.
	DiskBytes int64
	// Learned reports a memory prediction from a class a run that succeeded
	// was measured in; false is a first run, or a run of a class known only
	// from failed runs. DiskLearned reports a prediction of what the class
	// writes. A run missing either is planned one at a time in its class.
	Learned     bool
	DiskLearned bool
	ChildWork
	memKey, diskKey childModelKey
	mem, disk       *childModel
	run             *ledger.Run
	gated           bool
}

// model is the class model of key, created on first use.
func (c *ChildNeeds) model(key childModelKey) *childModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models == nil {
		c.models, c.gates = map[childModelKey]*childModel{}, map[childModelKey]chan struct{}{}
	}
	m := c.models[key]
	if m == nil {
		m = &childModel{}
		c.models[key] = m
	}
	return m
}

// predict loads m from the run ledger on first use and predicts it for work.
func (m *childModel) predict(ctx context.Context, run *ledger.Run, key ledger.NeedKey, work int64) (int64, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded && run != nil {
		// A model the ledger cannot read stands aside and is read again for
		// the next run.
		if state, ok, err := run.NeedModel(ctx, key); err == nil {
			if ok {
				m.unmarshal(state)
			}
			m.loaded = true
		}
	}
	need, ok := m.h.Predict(work)
	return need, ok, m.succeeded
}

// plan predicts one run's reservation within the allocations l holds for it.
// The memory allocation is re-derived for the admission. The half-life is
// counted in the repository's own projects of the tool, so the models forget
// at the pace the repository in front of it produces evidence. A run with no
// ledger run in its context is still planned from what this process learned,
// and what it teaches is kept in process only. A run of a class no run that
// succeeded has measured waits for the class's run in flight to end and is
// then planned from what it taught. A canceled wait returns CTX_CANCELED.
func (c *ChildNeeds) plan(ctx context.Context, l *Ledger, key ChildKey, w ChildWork) (ChildNeed, error) {
	memKind, diskKind, _ := key.Kind.kinds()
	run := ledger.RunFromContext(ctx)
	class := ledger.NeedKey{Subject: key.Tool, Fingerprint: key.Fingerprint, Build: childBuild(),
		SizeClass: SizeClassOf(w.WorkBytes)}
	n := ChildNeed{ChildWork: w, run: run}
	n.memKey = childModelKey{repository: run.RepositoryID(), NeedKey: class}
	n.memKey.Kind = memKind
	n.diskKey = n.memKey
	n.diskKey.Kind = diskKind
	n.mem, n.disk = c.model(n.memKey), c.model(n.diskKey)
	for {
		memNeed, memOK, proven := n.mem.predict(ctx, run, n.memKey.NeedKey, w.WorkBytes)
		written, diskOK, _ := n.disk.predict(ctx, run, n.diskKey.NeedKey, w.WorkBytes)
		allocation := max(l.Allocation(), 1)
		diskAllocation, _ := l.DiskSnapshot()
		n.Bytes, n.DiskBytes = allocation, max(diskAllocation, w.StagedBytes)
		if memOK {
			n.Bytes = min(max(memNeed, 1), allocation)
		}
		if diskOK {
			n.DiskBytes = saturatingAdd(w.StagedBytes, written)
		}
		n.Learned, n.DiskLearned = memOK && proven, diskOK
		if n.Learned && n.DiskLearned {
			return n, nil
		}
		c.mu.Lock()
		gate := c.gates[n.memKey]
		if gate == nil {
			c.gates[n.memKey] = make(chan struct{})
			c.mu.Unlock()
			n.gated = true
			return n, nil
		}
		c.mu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return ChildNeed{}, model.Canceled(ctx.Err())
		}
	}
}

// saturatingAdd is a + b for two non-negative figures, at the largest int64
// rather than past it.
func saturatingAdd(a, b int64) int64 {
	if a > 1<<63-1-b {
		return 1<<63 - 1
	}
	return a + b
}

// ChildMeasure is what one tool run was read using. A figure that was not read
// is marked so, never zero.
type ChildMeasure struct {
	// MemoryBytes is the highest anonymous resident set the run's tree was
	// read at; MemoryRead is false when no reading was taken.
	MemoryBytes int64
	MemoryRead  bool
	// WrittenBytes are the bytes the run wrote beside what was staged for it;
	// WrittenRead is false when they could not be measured.
	WrittenBytes int64
	WrittenRead  bool
	// Succeeded reports a run that did its work. A failed run's readings are
	// learned as lower bounds of its need, and keep its class gated.
	Succeeded bool
}

// fold folds one dimension's reading into its model and records it in the
// run ledger with the reservation it ran under.
func (n ChildNeed) fold(m *childModel, key childModelKey, reserved, need int64, succeeded bool) {
	if need < 0 || m == nil {
		return
	}
	m.mu.Lock()
	m.h.Observe(need, n.WorkBytes, max(n.Projects, 1))
	m.succeeded = m.succeeded || succeeded
	state, err := m.marshal()
	m.mu.Unlock()
	if err == nil && n.run != nil {
		n.run.ObserveNeed(ledger.NeedObservation{Key: key.NeedKey, ReservedBytes: reserved, NeedBytes: need, State: state})
	}
}

// done ends one run: what it was read using is learned, and a gated run
// releases the runs of its class waiting on it.
func (c *ChildNeeds) done(n ChildNeed, got ChildMeasure) {
	if got.MemoryRead {
		n.fold(n.mem, n.memKey, n.Bytes, got.MemoryBytes, got.Succeeded)
	}
	if got.WrittenRead {
		n.fold(n.disk, n.diskKey, max(n.DiskBytes-n.StagedBytes, 0), got.WrittenBytes, got.Succeeded)
	}
	if n.gated {
		c.mu.Lock()
		if gate := c.gates[n.memKey]; gate != nil {
			delete(c.gates, n.memKey)
			close(gate)
		}
		c.mu.Unlock()
	}
}

// childHold is what a holding of an external tool carries: the plan it was
// admitted on, ended exactly once.
type childHold struct {
	needs *ChildNeeds
	need  ChildNeed
	once  sync.Once
	// succeeded is the ended run's own report, which a later reading of what
	// it wrote is learned under (ObserveWritten). It is written once, by end.
	succeeded atomic.Bool
}

func (ch *childHold) end(got ChildMeasure) {
	ch.once.Do(func() {
		ch.succeeded.Store(got.Succeeded)
		ch.needs.done(ch.need, got)
	})
}

// HoldChild admits one external tool run -- a precise indexer's profile unit
// or a language server -- at the memory and temporary disk needs predicts for
// its work, within the allocations the ledger holds for this admission.
// Everything Hold documents holds: strict first-in-first-out, the runs-alone
// rule, makeRoom and the cancellation that leaks nothing. The holding's
// Observe records what the run used; its Release ends the plan either way, so
// a gated run that is refused, canceled or teaches nothing never leaves the
// runs of its class waiting.
func (l *Ledger) HoldChild(ctx context.Context, needs *ChildNeeds, key ChildKey, w ChildWork, makeRoom func()) (*Holding, error) {
	if _, _, ok := key.Kind.kinds(); !ok || key.Tool == "" || key.Fingerprint == "" {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "an external tool's reservation needs its kind, the tool and the payload it learns under"}
	}
	n, err := needs.plan(ctx, l, key, w)
	if err != nil {
		return nil, err
	}
	ch := &childHold{needs: needs, need: n}
	h, err := l.Hold(ctx, Reservation{MemoryBytes: n.Bytes, DiskBytes: n.DiskBytes}, makeRoom)
	if err != nil {
		ch.end(ChildMeasure{})
		return nil, err
	}
	h.child = ch
	return h, nil
}

// Need is the plan an external tool's holding was admitted on, and false for
// any other holding.
func (h *Holding) Need() (ChildNeed, bool) {
	if h.child == nil {
		return ChildNeed{}, false
	}
	return h.child.need, true
}

// Observe records an external tool run's readings against the plan it was
// admitted on, once; a holding that is no external tool's records nothing.
//
// Written bytes above what the holding holds of disk raise it at once: they
// are on the disk already. A run planned without a prediction in a dimension
// -- a first run, held at the whole allocations, or a run of a class known
// only from failed runs -- is then lowered in that dimension to what its class
// now predicts for its work, once it is measured there: a language server lives on after its
// measurement, and holding the allocations for its lifetime would keep every
// other child from running beside it. A memory reading above the lowered
// figure raises it again (FollowResidency).
func (h *Holding) Observe(got ChildMeasure) {
	if h.child == nil {
		return
	}
	h.child.end(got)
	n := h.child.need
	if got.WrittenRead {
		if staged := saturatingAdd(n.StagedBytes, got.WrittenBytes); staged > h.DiskBytes() {
			h.AdjustDisk(staged)
		}
	}
	if got.MemoryRead && !n.Learned {
		n.mem.mu.Lock()
		predicted, ok := n.mem.h.Predict(n.WorkBytes)
		n.mem.mu.Unlock()
		if ok && predicted < h.Bytes() {
			h.Adjust(max(predicted, 1))
		}
	}
	if got.WrittenRead && !n.DiskLearned {
		n.disk.mu.Lock()
		written, ok := n.disk.h.Predict(n.WorkBytes)
		n.disk.mu.Unlock()
		if want := saturatingAdd(n.StagedBytes, written); ok && want < h.DiskBytes() {
			h.AdjustDisk(max(want, saturatingAdd(n.StagedBytes, got.WrittenBytes)))
		}
	}
}

// ObserveWritten records written bytes read again after the run's
// measurement -- a language server's work directory when it exits -- into the
// class's disk model, under the run's own success or failure, and raises the
// holding's disk to them when they exceed it. It ends nothing: the run's plan
// ended at Observe.
func (h *Holding) ObserveWritten(written int64) {
	if h.child == nil || written < 0 {
		return
	}
	n := h.child.need
	n.fold(n.disk, n.diskKey, max(h.DiskBytes()-n.StagedBytes, 0), written, h.child.succeeded.Load())
	if staged := saturatingAdd(n.StagedBytes, written); staged > h.DiskBytes() {
		h.AdjustDisk(staged)
	}
}

// WrittenBytes is the apparent size of every regular file under dir, read in
// one walk, less staged: the bytes a tool wrote beside the copy staged for it
// in dir. ok is false when dir could not be walked. A file removed while the
// walk runs is not counted.
func WrittenBytes(dir string, staged int64) (int64, bool) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				return err
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total = saturatingAdd(total, info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, false
	}
	return max(total-staged, 0), true
}

// Admit is how a heavy unit is admitted: the unit's provider plans its
// reservation on the ledger at the moment it is admitted, from what the
// process has learned so far, and returns the holding.
type Admit func(ctx context.Context, l *Ledger) (*Holding, error)

// Allocation re-derives the memory allocation from the kernel's figure, as
// every admission does, and returns it: the ceiling a reservation planned now
// is held within.
func (l *Ledger) Allocation() int64 {
	l.mu.Lock()
	rederive := l.rederive
	l.mu.Unlock()
	if rederive != nil {
		rederive()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allocation
}

// Bytes is the memory the holding holds now, adjustments included.
func (h *Holding) Bytes() int64 {
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	return h.w.bytes
}

type holdingKey struct{}

// WithHolding hands a holding to the work it was admitted for, so the child
// that work starts can be read against it and raised to what it uses.
func WithHolding(ctx context.Context, h *Holding) context.Context {
	return context.WithValue(ctx, holdingKey{}, h)
}

// HoldingFrom is the holding WithHolding handed over, or nil.
func HoldingFrom(ctx context.Context) *Holding {
	h, _ := ctx.Value(holdingKey{}).(*Holding)
	return h
}

// Residency follows one running child's anonymous resident set against the
// holding it was admitted with. A reading above what the holding holds raises
// the holding to it at once (Holding.Adjust): the memory is already in use,
// waiting cannot give it back, and the raised figure is what holds the next
// reservation back while the child runs. A reading below never lowers it,
// because a child that has shrunk can grow again within its reservation.
type Residency struct {
	h    *Holding
	read func() (int64, bool)
	stop chan struct{}
	done chan struct{}
	once sync.Once
	// mu orders the readings: the follower's and Peak's.
	mu   sync.Mutex
	peak int64
	seen bool
}

// FollowResidency starts following read, the child's live anonymous resident
// set, every period: the sampling period of the runner that produces the
// reading, since a faster poll reads the same figure again.
func FollowResidency(h *Holding, read func() (int64, bool), period time.Duration) *Residency {
	r := &Residency{h: h, read: read, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			r.sample()
			select {
			case <-r.stop:
				return
			case <-t.C:
			}
		}
	}()
	return r
}

// sample takes one reading and raises the holding to it.
func (r *Residency) sample() {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.read()
	if !ok {
		return
	}
	if !r.seen || b > r.peak {
		r.peak, r.seen = b, true
	}
	if b > r.h.Bytes() {
		r.h.Adjust(b)
	}
}

// Peak takes a reading now and returns the highest reading taken so far, and
// false when none was ever taken: the need of a child that keeps running, read
// at a moment its caller knows is past its load.
func (r *Residency) Peak() (int64, bool) {
	r.sample()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak, r.seen
}

// Stop joins the follower, takes one last reading, and returns the highest
// reading taken: the run's need, and false when no reading was ever taken.
// It may be called more than once and returns the same figure.
func (r *Residency) Stop() (int64, bool) {
	r.once.Do(func() {
		close(r.stop)
		<-r.done
		r.sample()
	})
	return r.peak, r.seen
}

// ProjectWork is what one tool run is planned on, in one streaming pass over
// the snapshot manifest: the bytes of the live files at or under the project
// directory root (the empty root is the workspace root); the bytes of every
// live file, which is what a whole-snapshot copy stages, or at most
// maxStagedBytes when that is positive; and how many directories of the
// snapshot hold one of the tool's project markers -- the repository's own
// projects of that tool. Nothing of the manifest is retained but that set of
// project directories, one per project the tool can be run for.
func ProjectWork(ctx context.Context, view model.SnapshotView, root string, markers []string, maxStagedBytes int64) (ChildWork, error) {
	var w ChildWork
	dirs := map[string]struct{}{}
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		w.StagedBytes = saturatingAdd(w.StagedBytes, fv.Size)
		if root == "" || len(fv.Path) > len(root) && fv.Path[:len(root)] == root && fv.Path[len(root)] == '/' {
			w.WorkBytes = saturatingAdd(w.WorkBytes, fv.Size)
		}
		dir, base := path.Split(fv.Path)
		for _, m := range markers {
			if base == m {
				dirs[dir] = struct{}{}
				break
			}
		}
		return nil
	})
	if err != nil {
		return ChildWork{}, err
	}
	if maxStagedBytes > 0 {
		w.StagedBytes = min(w.StagedBytes, maxStagedBytes)
	}
	w.Projects = int64(len(dirs))
	return w, nil
}
