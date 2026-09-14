// Package watch is the filesystem notification half of Section 13.2: it turns
// operating-system change notifications over one workspace into bounded,
// debounced batches of root-relative paths for the index coordinator, and it
// reports honestly when notification does not cover the tree.
//
// Nothing here decides what changed. A notification is a hint: the path in a
// batch says "look here", never "these bytes differ". Source and hash truth
// belong to the capture the coordinator runs against the batch, which is why
// an overflow collapses to one full-reconciliation flag instead of a truncated
// path list -- a truncated list is a claim that the paths it omits did not
// change, and that claim is exactly how an index reports fresh coverage it
// does not have.
//
// The watcher retains no repository-wide file list (Section 6). What it holds
// is the bounded set of watched directories and the bounded pending-path set;
// both have explicit finite caps and the pending set is released the moment a
// batch is emitted.
package watch

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
	"github.com/fsnotify/fsnotify"
)

// Defaults from Section 20.1 `[index]`. A caller that leaves a bound at zero
// gets these; a negative bound is rejected, because no zero or negative
// setting may mean unlimited.
const (
	DefaultDebounce  = 250 * time.Millisecond
	DefaultReconcile = 30 * time.Second
	DefaultMaxPaths  = 10000
	DefaultMaxBytes  = 2 << 20
)

// maxWatchedDirs bounds the directory watches one watcher holds. It is a
// product-owned structural bound, not a configuration knob: it exists so the
// watch set is finite on a pathological tree, and it is deliberately near the
// order of a stock Linux `fs.inotify.max_user_watches` so the product refuses
// before the kernel does and can say so in Coverage.
const maxWatchedDirs = 65536

// Reasons a batch is a full reconciliation. They are operator-facing text on a
// bounded surface: none of them names a path, because Section 20.1 keeps
// source paths out of ordinary logs and these strings reach both a log line
// and a status field.
const (
	ReasonPathsOverflow = "pending path count exceeded index.watch_pending_paths"
	ReasonBytesOverflow = "pending path bytes exceeded index.watch_pending_bytes"
	ReasonQueueOverflow = "the operating system's notification queue overflowed"
	ReasonWatchLimit    = "the operating system refused a directory watch"
	ReasonWatchSetLimit = "the workspace holds more directories than one watcher may watch"
	ReasonUnavailable   = "filesystem notification is unavailable on this host"
	ReasonRescanFailed  = "the workspace could not be traversed to place watches"
	ReasonPeriodic      = "periodic reconciliation"
)

// Batch is one debounced set of changes. Paths are root-relative, sorted and
// deduplicated. Overflow means the batch is a full-reconciliation request:
// Paths is then empty and Reason says why, so a reader can never mistake a
// collapsed batch for a complete list of what changed.
type Batch struct {
	Paths    []string
	Overflow bool
	Reason   string
}

// Options configure one watcher.
type Options struct {
	// Root is the opened workspace. The watcher only reads through it.
	Root workspace.Root
	// Policy is the traversal policy the watch set is derived from: every
	// directory it admits is watched. Its Git hooks (Ignore, ForceInclude,
	// ForceIncludeDir), when supplied, decide which directories are watched and
	// which events are kept, so the watched tree is the tree a capture reads.
	// With nil hooks only the built-in vendor and generated exclusions apply,
	// which watches more than a capture would admit: safe for correctness -- an
	// extra path in a batch costs a hash comparison, a missing one costs a
	// change -- but it puts watches on ignored build trees, so a caller with a
	// Git owner available is expected to supply the hooks.
	Policy workspace.Policy
	// Debounce is index.watch_debounce and Reconcile is
	// index.reconcile_interval.
	Debounce  time.Duration
	Reconcile time.Duration
	// MaxPaths is index.watch_pending_paths and MaxBytes is
	// index.watch_pending_bytes, the latter measured over the retained path
	// bytes themselves: that is the memory the pending set occupies, which is
	// what the bound exists to cap.
	MaxPaths int
	MaxBytes int64
	Logger   *slog.Logger
}

// Watcher turns notifications into batches. One Watcher runs one Run at a
// time; Coverage is safe to call concurrently with Run, which is how status
// reads it while a watch is in flight.
type Watcher struct {
	opts    Options
	dataDir string

	mu             sync.Mutex
	complete       bool
	reason         string
	pending        int
	lastReconciled time.Time
}

// New validates the options.
func New(o Options) (*Watcher, error) {
	if o.Root.Path == "" {
		return nil, invalid("the watcher needs an opened workspace root")
	}
	if o.Policy.MaxFiles <= 0 {
		return nil, invalid("the watcher needs the traversal file budget; zero would mean unlimited")
	}
	for _, b := range []struct {
		name  string
		value *time.Duration
		def   time.Duration
	}{
		{"watch_debounce", &o.Debounce, DefaultDebounce},
		{"reconcile_interval", &o.Reconcile, DefaultReconcile},
	} {
		if *b.value < 0 {
			return nil, invalid("index." + b.name + " is negative; a bound must not be negative")
		}
		if *b.value == 0 {
			*b.value = b.def
		}
	}
	if o.MaxPaths < 0 || o.MaxBytes < 0 {
		return nil, invalid("index.watch_pending_paths and index.watch_pending_bytes must not be negative")
	}
	if o.MaxPaths == 0 {
		o.MaxPaths = DefaultMaxPaths
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	w := &Watcher{opts: o}
	if o.Policy.DataDir != "" {
		if rel, err := filepath.Rel(o.Root.Path, o.Policy.DataDir); err == nil {
			slashed := filepath.ToSlash(rel)
			if slashed != ".." && !strings.HasPrefix(slashed, "../") {
				w.dataDir = slashed
			}
		}
	}
	return w, nil
}

// Coverage reports whether notification covers every directory the policy
// admits, how many paths are pending in the current debounce window, and when
// the watch set was last re-derived from the filesystem.
//
// complete is not a promise that no event can be missed. It says that every
// directory the traversal admits carries a watch -- whether or not it holds an
// eligible file -- and that none was refused. What it cannot cover is a
// directory that appears between two rescans: its own creation is an event, so
// it joins the batch that closes the current debounce window and the rescan at
// that window's end places its watch, but a file written inside it before that
// rescan produces no event of its own. It is not lost, because the batch
// carrying the directory reaches the coordinator only after the rescan and a
// batch is a hint to re-capture rather than a list of what differs. The
// reconcile interval remains the bound for the changes notification cannot
// describe at all: a queue overflow, a refused watch, an unavailable backend --
// which is what lastReconciled is for, and why a reconciliation batch is a full
// one.
func (w *Watcher) Coverage() (complete bool, pending int, lastReconciled time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.complete, w.pending, w.lastReconciled
}

// Run watches until ctx ends or emit fails. Every batch is delivered to emit
// synchronously: emit is the coordinator's refresh, and events that arrive
// while it runs accumulate into the next batch (Section 13.1), they are not
// dropped and they do not run a second refresh concurrently.
//
// A host with no working notification backend is not a failure. Run reports
// incomplete coverage and falls back to the bounded periodic reconciliation,
// which is the polling path of Section 13.2.
func (w *Watcher) Run(ctx context.Context, emit func(Batch) error) error {
	if emit == nil {
		return invalid("the watcher needs somewhere to deliver batches")
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.setCoverage(false, ReasonUnavailable)
		w.opts.Logger.Warn("filesystem notification unavailable; falling back to periodic reconciliation",
			"component", "index.watch", "error_code", model.CodeProviderUnavailable)
		return w.reconcileOnly(ctx, emit)
	}
	defer fsw.Close()
	return w.loop(ctx, fsw, emit)
}

// reconcileOnly is the polling fallback: no notification, one full
// reconciliation per interval, coverage permanently incomplete.
func (w *Watcher) reconcileOnly(ctx context.Context, emit func(Batch) error) error {
	ticker := time.NewTicker(w.opts.Reconcile)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return model.Canceled(ctx.Err())
		case <-ticker.C:
			w.markReconciled()
			_, reason := w.coverage()
			if err := emit(Batch{Overflow: true, Reason: reason}); err != nil {
				return err
			}
		}
	}
}

// loop is the notification path.
func (w *Watcher) loop(ctx context.Context, fsw *fsnotify.Watcher, emit func(Batch) error) error {
	watched := make(map[string]bool, 64)
	w.rescan(ctx, fsw, watched)

	pending := make(map[string]struct{}, 64)
	var pendingBytes int64
	var overflow bool
	var overflowReason string
	// emitted records whether a batch reached the coordinator since the last
	// reconciliation tick. A refresh re-derived truth from the filesystem, so
	// the periodic pass right behind it would be a second capture of the same
	// state; with incomplete coverage the tick always fires, because there the
	// notification stream is not trustworthy on its own.
	emitted := false
	needRescan := false
	// armed tracks the debounce window explicitly rather than inferring it
	// from the pending count: a window that was released by a reconciliation
	// leaves a stopped timer behind, and a count-based guess would then never
	// re-arm and the next events would sit in the set forever.
	armed := false

	debounce := time.NewTimer(time.Hour)
	stopTimer(debounce)
	defer debounce.Stop()
	ticker := time.NewTicker(w.opts.Reconcile)
	defer ticker.Stop()

	arm := func() {
		if armed {
			return
		}
		armed = true
		stopTimer(debounce)
		debounce.Reset(w.opts.Debounce)
	}
	disarm := func() {
		armed = false
		stopTimer(debounce)
	}
	release := func() Batch {
		b := Batch{Overflow: overflow, Reason: overflowReason}
		if !overflow {
			b.Paths = make([]string, 0, len(pending))
			for p := range pending {
				b.Paths = append(b.Paths, p)
			}
			slices.Sort(b.Paths)
		}
		pending = make(map[string]struct{}, 64)
		pendingBytes, overflow, overflowReason = 0, false, ""
		w.setPending(0)
		return b
	}

	for {
		select {
		case <-ctx.Done():
			return model.Canceled(ctx.Err())

		case ev, ok := <-fsw.Events:
			if !ok {
				w.setCoverage(false, ReasonUnavailable)
				return w.reconcileOnly(ctx, emit)
			}
			// A rename arrives as two events -- Rename carrying the old path
			// and Create carrying the new one -- so both sides of a move reach
			// the batch from Name alone. (v1.10.1 keeps the renamedFrom
			// correlation unexported, and it is a hint either way.)
			{
				name := ev.Name
				// Directory-ness decides two things: whether a rescan is owed,
				// and which question the policy's ignore hook is asked. The
				// capture's hook answers them differently ("is anything under
				// this directory known" against "is this file known"), so an
				// event about a directory must not be classified as a file.
				dir := ev.Has(fsnotify.Create) && isDir(name)
				rel, ok := w.relevant(name, dir)
				if !ok {
					continue
				}
				if dir {
					// A directory appeared inside the watched tree: its own
					// children produce no events until it carries a watch.
					needRescan = true
				}
				if overflow {
					continue
				}
				if _, dup := pending[rel]; dup {
					continue
				}
				if len(pending)+1 > w.opts.MaxPaths {
					overflow, overflowReason = true, ReasonPathsOverflow
				} else if pendingBytes+int64(len(rel)) > w.opts.MaxBytes {
					overflow, overflowReason = true, ReasonBytesOverflow
				}
				if overflow {
					// The collapse frees the accumulated list on purpose: the
					// batch no longer claims anything about individual paths,
					// so retaining them would be memory held for a claim that
					// is no longer made.
					pending = make(map[string]struct{})
					pendingBytes = 0
					w.setPending(0)
					w.opts.Logger.Info("watch overflow; collapsing to full reconciliation",
						"component", "index.watch", "reason", overflowReason)
					arm()
					continue
				}
				pending[rel] = struct{}{}
				pendingBytes += int64(len(rel))
				w.setPending(len(pending))
				arm()
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				w.setCoverage(false, ReasonUnavailable)
				return w.reconcileOnly(ctx, emit)
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// The kernel dropped events it never told us the names of.
				// Anything short of a full reconciliation here is a guess.
				if !overflow {
					overflow, overflowReason = true, ReasonQueueOverflow
					pending = make(map[string]struct{})
					pendingBytes = 0
					w.setPending(0)
					arm()
				}
				continue
			}
			w.opts.Logger.Warn("filesystem notification error",
				"component", "index.watch", "error", err.Error())

		case <-debounce.C:
			armed = false
			if needRescan {
				w.rescan(ctx, fsw, watched)
				needRescan = false
			}
			if len(pending) == 0 && !overflow {
				continue
			}
			if err := emit(release()); err != nil {
				return err
			}
			emitted = true

		case <-ticker.C:
			w.rescan(ctx, fsw, watched)
			needRescan = false
			complete, reason := w.coverage()
			if emitted && complete {
				emitted = false
				continue
			}
			emitted = false
			if !complete {
				reason = reason + "; " + ReasonPeriodic
			} else {
				reason = ReasonPeriodic
			}
			// A pending window is subsumed by a full reconciliation, so it is
			// released rather than emitted twice.
			disarm()
			pending = make(map[string]struct{}, 64)
			pendingBytes, overflow, overflowReason = 0, false, ""
			w.setPending(0)
			if err := emit(Batch{Overflow: true, Reason: reason}); err != nil {
				return err
			}
		}
	}
}

// rescan re-derives the watch set from the traversal policy and reconciles the
// held watches with it.
//
// The set is every directory the policy admits -- derived by running the one
// traversal the product owns (workspace.WalkDirs) rather than by
// re-implementing its exclusion rules here. Admitted, not populated: a
// directory holding no eligible file today still carries a watch, so the first
// file to appear in it is a notification rather than something the next
// reconciliation tick discovers up to index.reconcile_interval later.
//
// The excluded trees stay unwatched, which is what keeps that affordable: with
// the capture's Git hooks in place an ignored build directory is pruned by the
// same predicate that keeps it out of the snapshot, so an `npm install` under
// an excluded `node_modules` produces no watches, no events and no full
// reconciliation of a tree that is not indexed at all.
func (w *Watcher) rescan(ctx context.Context, fsw *fsnotify.Watcher, watched map[string]bool) {
	want := make(map[string]bool, len(watched)+16)
	// The root is wanted even when the traversal cannot start: a workspace
	// whose root is watched and whose coverage is reported incomplete is
	// strictly better than one with no watch at all.
	want["."] = true
	overLimit := false
	err := workspace.WalkDirs(ctx, w.opts.Root, w.opts.Policy, func(dir string) error {
		if want[dir] {
			return nil
		}
		if len(want) >= maxWatchedDirs {
			overLimit = true
			return errWatchSetFull
		}
		want[dir] = true
		return nil
	})
	// A traversal that stops part way still yields the directories it reached.
	// Placing those watches and reporting coverage incomplete is strictly
	// better than placing none: one unreadable directory -- a scratch tree
	// being rewritten underneath the walk, say -- must not leave the whole
	// workspace unwatched. The removal pass is skipped in that case, because a
	// partial set is not evidence that the directories it omits are gone.
	partial := err != nil && !errors.Is(err, errWatchSetFull)
	if partial {
		w.opts.Logger.Warn("the workspace could not be fully traversed to place watches",
			"component", "index.watch", "error", err.Error())
	}

	refused := false
	for dir := range want {
		if watched[dir] {
			continue
		}
		abs := w.opts.Root.Path
		if dir != "." {
			abs = filepath.Join(abs, filepath.FromSlash(dir))
		}
		if err := fsw.Add(abs); err != nil {
			if isWatchLimit(err) {
				refused = true
				w.opts.Logger.Warn("the operating system refused a directory watch",
					"component", "index.watch", "watched", len(watched))
				break
			}
			// A directory that vanished between the traversal and the watch is
			// not a coverage failure: its removal is itself a change the next
			// batch carries.
			continue
		}
		watched[dir] = true
	}
	for dir := range watched {
		if want[dir] || partial {
			continue
		}
		abs := w.opts.Root.Path
		if dir != "." {
			abs = filepath.Join(abs, filepath.FromSlash(dir))
		}
		_ = fsw.Remove(abs)
		delete(watched, dir)
	}

	switch {
	case refused:
		w.setCoverage(false, ReasonWatchLimit)
	case overLimit:
		w.setCoverage(false, ReasonWatchSetLimit)
	case partial:
		w.setCoverage(false, ReasonRescanFailed)
	default:
		w.setCoverage(true, "")
	}
	w.markReconciled()
}

// errWatchSetFull stops the traversal once the watch set is at its bound.
var errWatchSetFull = errors.New("watch set full")

// relevant maps an absolute event path to the root-relative path a batch
// carries, or reports that the event is not about repository source. The
// unconditional exclusions are the ones workspace.Walk applies (the Git
// directory and the data directory, neither of which is source and the second
// of which the indexer writes itself); the policy hooks are applied when the
// caller supplied them.
func (w *Watcher) relevant(name string, isDir bool) (string, bool) {
	if name == "" {
		return "", false
	}
	rel, err := filepath.Rel(w.opts.Root.Path, name)
	if err != nil {
		return "", false
	}
	slashed := filepath.ToSlash(rel)
	if slashed == "." || slashed == ".." || strings.HasPrefix(slashed, "../") {
		return "", false
	}
	if slashed == ".git" || strings.HasPrefix(slashed, ".git/") {
		return "", false
	}
	if w.dataDir != "" && (slashed == w.dataDir || strings.HasPrefix(slashed, w.dataDir+"/")) {
		return "", false
	}
	if len(slashed) > model.MaxPathBytes {
		return "", false
	}
	if w.opts.Policy.ForceInclude != nil && w.opts.Policy.ForceInclude(slashed) {
		return slashed, true
	}
	if w.opts.Policy.Ignore != nil && w.opts.Policy.Ignore(slashed, isDir) {
		return "", false
	}
	return slashed, true
}

func (w *Watcher) setCoverage(complete bool, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.complete, w.reason = complete, reason
}

func (w *Watcher) coverage() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.complete, w.reason
}

func (w *Watcher) setPending(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = n
}

func (w *Watcher) markReconciled() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastReconciled = time.Now()
}

func isDir(name string) bool {
	info, err := os.Lstat(name)
	return err == nil && info.IsDir()
}

// isWatchLimit reports the operating system refusing another watch. These are
// the conditions Section 13.2 names as "OS watch limits": the inotify watch
// and instance caps and the per-process descriptor cap.
func isWatchLimit(err error) bool {
	return errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE)
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

func invalid(msg string) error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}
