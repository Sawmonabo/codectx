package admission

import (
	"context"
	"path"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
)

// External tools -- the precise indexers' profile units and the language
// servers -- are reserved from their measured need, the way parser files are
// (ADR-0012 decision 5, "External indexers and language servers"). A tool run's
// work is the bytes of the snapshot files under the project directory it is
// pointed at; its need is the highest anonymous resident set its process tree
// was read at while it ran. The coordinator learns need per work byte in the
// decaying histogram parser files use, keyed per repository, tool, tool payload
// and work-size class, and persisted in the run ledger's observation store.
//
// No figure is a constant of a tool. A run whose key has no observation is
// reserved at the whole allocation: it runs alone among the heavy children,
// once per repository, tool payload, build and size class, because nothing
// derivable says what it will use. Every published source on these tools'
// memory names the project, its dependency closure or the machine itself (a
// managed runtime's heap ceiling is a share of physical memory) as what sets
// it, so no per-tool slope holds across repositories.

// ChildKey names one external tool's need model within a repository. Tool is
// the tool's family and name ("indexer scip-go", "server gopls"); Fingerprint
// is the identity of the payload the lock pinned, so a repinned tool learns
// afresh.
type ChildKey struct {
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
	// probing holds, per key with no observation, the first run reserved at
	// the allocation; it is closed when that run's holding ends. Every other
	// run of the key waits for it before it is planned, so a cold repository's
	// projects of one tool are reserved from the first one's measurement
	// rather than each at the allocation, one after another.
	probing map[childModelKey]chan struct{}
}

type childModelKey struct {
	repository string
	ledger.NeedKey
}

type childModel struct {
	mu     sync.Mutex
	loaded bool
	h      NeedHistogram
}

// ChildNeed is one tool run's memory reservation and what is learned from it.
type ChildNeed struct {
	// Bytes is the memory the run reserves: the model's prediction for its
	// work, or the allocation when nothing is learned yet, and never more
	// than the allocation it was planned against nor less than one byte, so a
	// runner always tracks the child's residency.
	Bytes int64
	// Learned reports a prediction; false is a first run, reserved at the
	// allocation.
	Learned bool
	// WorkBytes is the run's work, the size the need is learned per.
	WorkBytes int64
	halfLife  int64
	mk        childModelKey
	model     *childModel
	run       *ledger.Run
	probe     bool
}

// plan predicts one run's reservation within the allocation l re-derives for
// it. halfLife is counted in the repository's own projects of the tool, so
// the model forgets at the pace the repository in front of it produces
// evidence. A run with no ledger run in its context is still predicted from
// what this process learned, and what it teaches is kept in process only; a
// model the ledger cannot read stands aside and is read again for the next
// run. A run of a key with no observation waits for the key's first run to end
// and is then planned from what it taught; when it taught nothing, this run is
// the next first run. A canceled wait returns CTX_CANCELED.
func (c *ChildNeeds) plan(ctx context.Context, l *Ledger, key ChildKey, workBytes, halfLife int64) (ChildNeed, error) {
	run := ledger.RunFromContext(ctx)
	n := ChildNeed{WorkBytes: workBytes, halfLife: max(halfLife, 1), run: run,
		mk: childModelKey{repository: run.RepositoryID(),
			NeedKey: ledger.NeedKey{Language: key.Tool, Fingerprint: key.Fingerprint, Build: childBuild(),
				SizeClass: SizeClassOf(workBytes)}}}
	c.mu.Lock()
	if c.models == nil {
		c.models, c.probing = map[childModelKey]*childModel{}, map[childModelKey]chan struct{}{}
	}
	m := c.models[n.mk]
	if m == nil {
		m = &childModel{}
		c.models[n.mk] = m
	}
	c.mu.Unlock()
	n.model = m
	for {
		m.mu.Lock()
		if !m.loaded && run != nil {
			if state, ok, err := run.NeedModel(ctx, n.mk.NeedKey); err == nil {
				// A state that does not decode leaves the model empty, and the
				// next observation's state supersedes it.
				if ok {
					_ = m.h.UnmarshalBinary(state)
				}
				m.loaded = true
			}
		}
		need, learned := m.h.Predict(workBytes)
		m.mu.Unlock()
		allocation := max(l.Allocation(), 1)
		if learned {
			n.Bytes, n.Learned = min(max(need, 1), allocation), true
			return n, nil
		}
		c.mu.Lock()
		first := c.probing[n.mk]
		if first == nil {
			c.probing[n.mk] = make(chan struct{})
			c.mu.Unlock()
			n.Bytes, n.probe = allocation, true
			return n, nil
		}
		c.mu.Unlock()
		select {
		case <-first:
		case <-ctx.Done():
			return ChildNeed{}, model.Canceled(ctx.Err())
		}
	}
}

// done ends one run: a measured need is folded into its model and recorded in
// the run ledger with the reservation it ran under, and a first run releases
// the runs of its key waiting on it. ok false is a run no reading was taken
// of, which teaches nothing, never a zero.
func (c *ChildNeeds) done(n ChildNeed, needBytes int64, ok bool) {
	if ok && needBytes >= 0 && n.model != nil {
		n.model.mu.Lock()
		n.model.h.Observe(needBytes, n.WorkBytes, n.halfLife)
		state, err := n.model.h.MarshalBinary()
		n.model.mu.Unlock()
		if err == nil && n.run != nil {
			n.run.ObserveNeed(ledger.NeedObservation{Key: n.mk.NeedKey, ReservedBytes: n.Bytes, NeedBytes: needBytes, State: state})
		}
	}
	if n.probe {
		c.mu.Lock()
		if first := c.probing[n.mk]; first != nil {
			delete(c.probing, n.mk)
			close(first)
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
}

func (ch *childHold) end(needBytes int64, ok bool) {
	ch.once.Do(func() { ch.needs.done(ch.need, needBytes, ok) })
}

// HoldChild admits one external tool run -- a precise indexer's profile unit
// or a language server -- at the memory needs predicts for its work, within
// the allocation re-derived for this admission, and the temporary disk it
// states. Everything Hold documents holds: strict first-in-first-out, the
// runs-alone rule, makeRoom and the cancellation that leaks nothing. The
// holding's Observe records what the run used; its Release ends the plan
// either way, so a first run that is refused, canceled or teaches nothing
// never leaves the runs of its key waiting.
func (l *Ledger) HoldChild(ctx context.Context, needs *ChildNeeds, key ChildKey, workBytes, halfLife, diskBytes int64,
	makeRoom func()) (*Holding, error) {
	if key.Tool == "" || key.Fingerprint == "" {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "an external tool's reservation needs the tool and the payload it learns under"}
	}
	n, err := needs.plan(ctx, l, key, workBytes, halfLife)
	if err != nil {
		return nil, err
	}
	ch := &childHold{needs: needs, need: n}
	h, err := l.Hold(ctx, Reservation{MemoryBytes: n.Bytes, DiskBytes: diskBytes}, makeRoom)
	if err != nil {
		ch.end(0, false)
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

// Observe records an external tool run's measured need -- the highest
// anonymous resident set its tree was read at -- against the plan it was
// admitted on. It is recorded once; ok false teaches nothing. A holding that
// is no external tool's records nothing.
func (h *Holding) Observe(needBytes int64, ok bool) {
	if h.child != nil {
		h.child.end(needBytes, ok)
	}
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

// ProjectWork is one tool run's work and the half-life its need is learned
// under, in one streaming pass over the snapshot manifest: the bytes of the
// live files at or under the project directory root (the empty root is the
// workspace root), and how many directories of the snapshot hold one of the
// tool's project markers -- the repository's own projects of that tool.
// Nothing of the manifest is retained but that set of project directories,
// one per project the tool can be run for.
func ProjectWork(ctx context.Context, view model.SnapshotView, root string, markers []string) (workBytes, projects int64, err error) {
	dirs := map[string]struct{}{}
	err = view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		if root == "" || len(fv.Path) > len(root) && fv.Path[:len(root)] == root && fv.Path[len(root)] == '/' {
			workBytes += fv.Size
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
		return 0, 0, err
	}
	return workBytes, int64(len(dirs)), nil
}
