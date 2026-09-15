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

	"github.com/Sawmonabo/codectx/internal/config"
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
	// ReasonPathTooLong is an event about a path longer than
	// model.MaxPathBytes. The path cannot be carried in a batch, so
	// notification coverage over it is reported incomplete rather than the
	// event being dropped in silence.
	ReasonPathTooLong  = "a changed path is longer than the representable path limit"
	ReasonUnavailable  = "filesystem notification is unavailable on this host"
	ReasonRescanFailed = "the workspace could not be traversed to place watches"
	ReasonPeriodic     = "periodic reconciliation"
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
	// MaxWatchedDirs is index.watch_max_directories: how many directories the
	// user allows one watcher to watch. Unlimited by default -- a repository's
	// directory count is a property of the repository, and the kernel's own
	// per-user watch limit is the real ceiling, which is refused and reported
	// as ReasonWatchLimit when it is reached. A user-set bound stops the
	// traversal at that many directories and reports coverage incomplete with
	// ReasonWatchSetLimit, so what it costs is never silent.
	MaxWatchedDirs config.Limit
	Logger         *slog.Logger
}

// Watcher turns notifications into batches. One Watcher runs one Run at a
// time; Coverage is safe to call concurrently with Run, which is how status
// reads it while a watch is in flight.
type Watcher struct {
	opts    Options
	dataDir string
	// longPathSeen makes the over-long-path warning one-shot per watcher.
	longPathSeen bool

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
	// The watch set is directories, so workspace.max_files never bounds it --
	// but a zero-valued Policy is a caller that never built one, and watching a
	// tree with none of the capture's exclusions applied is what this refuses.
	// DataDir is the field the check reads: Config.TraversalPolicy always fills
	// it and snapshot.Builder.validate already requires it to be absolute, so
	// it is present in every real policy. MaxFiles cannot serve as the check:
	// with unlimited the default, a real policy's MaxFiles is zero.
	if o.Policy.DataDir == "" {
		return nil, invalid("the watcher needs the capture's traversal policy, not a zero value")
	}
	if o.Policy.MaxFiles < 0 {
		return nil, invalid("workspace.max_files is negative; zero means unlimited")
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
	if o.MaxWatchedDirs < 0 {
		return nil, invalid("index.watch_max_directories is negative; zero means unlimited")
	}
	if o.MaxPaths < 0 || o.MaxBytes < 0 {
		return nil, invalid("index.watch_pending_paths and index.watch_pending_bytes must not be negative")
	}
	// These two stay reservations with a finite default, and 0 means "use the
	// default" rather than "unlimited". That is the documented behaviour for a
	// class-A bound, not an oversight of the "0 = unlimited" rule:
	//
	//   - The pending set is a heap-resident structure whose size is the number
	//     of paths changed between two debounce ticks. On a monorepo a branch
	//     switch or a generated-code run changes every file at once, so an
	//     unlimited pending set IS the repository-sized heap allocation the
	//     scale ruling forbids. "Unlimited" is never "load everything into
	//     heap"; here the two rules point in opposite directions and the memory
	//     one wins.
	//   - Nothing is lost when the reservation is exceeded. Overflow does not
	//     drop a path: it clears the pending set and forces a FULL
	//     reconciliation, which is strictly more work over strictly more paths,
	//     and it is reported three ways (Overflow, the batch Reason, Coverage).
	//     A bound that can only cost time, never fidelity, has no reason to be
	//     unlimited.
	//
	// So a user who writes 0 is asking for the product's reservation, and the
	// product must have one; an operator who wants a larger window sets a
	// larger number. Ruling applied: H-L0 report deviation 2, over the plan's
	// row-12 sentence.
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
// eligible file -- and that none was refused. Two windows it does not cover:
//
// The first is a directory that appears between two rescans: its own creation
// is an event, so it joins the batch that closes the current debounce window
// and the rescan at that window's end places its watch, but a file written
// inside it before that rescan produces no event of its own. It is not lost,
// because the batch carrying the directory reaches the coordinator only after
// the rescan and a batch is a hint to re-capture rather than a list of what
// differs.
//
// The second is the traversal policy itself, which every rescan re-applies but
// none re-derives. The caller builds it once, when the workspace is opened, and
// its Git ignore predicate is a snapshot of the ignored roots taken at that
// moment. A .gitignore edited mid-session therefore does not move the watch
// set: a tree it newly un-ignores stays unwatched while coverage still reports
// complete, and a tree it newly ignores keeps its watches. Each generation's
// own traversal recomputes the set, so the index is right either way, and the
// changes are seen at the next reconciliation rather than on notification.
//
// The reconcile interval remains the bound for the changes notification cannot
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
					// collapsed batch claims nothing about individual paths,
					// so retaining them would be memory held for a claim
					// nobody makes.
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
// the capture's Git ignore predicate in place an ignored build directory is
// pruned by the same predicate that keeps it out of the snapshot, so an
// `npm install` under
// an excluded `node_modules` produces no watches, no events and no full
// reconciliation of a tree that is not indexed at all.
// Peak RSS: `want` is one entry per ADMITTED DIRECTORY, never one per file,
// and the loop below stops at a user-set index.watch_max_directories, so the
// map holds one short relative path per admitted directory -- a 300 000-file
// monorepo at a realistic 10 files per directory wants ~30 000 entries, a few
// megabytes. Unlimited is the default: the kernel's own per-user watch limit
// is the real ceiling, and reaching it is refused and reported rather than
// pre-empted here. A tree that exceeds a user-set bound stops at it, reports
// coverage incomplete and keeps working.
// It is therefore already bounded by watch-set size rather than repository
// size, and a streamed diff against a spooled directory list would trade a
// bounded map for a temp file and buy nothing. (Row 12b asks for a diff
// against a SQLite watched-directory table; no such table exists and
// schema.sql is frozen this wave -- but on this bound the diff is not the
// memory fix it would be for a per-file structure.)
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
		// The question is whether admitting THIS directory would cross the
		// bound, so the set holds exactly the configured number, root
		// included -- not one more.
		if w.opts.MaxWatchedDirs.Exceeded(int64(len(want)) + 1) {
			overLimit = true
			return errWatchSetFull
		}
		want[dir] = true
		return nil
	})
	// A cancelled traversal is the session ending, not a workspace that cannot
	// be read: the operator pressed Ctrl-C, or Run's own context was cancelled
	// by the coordinator. Warning about it, placing watches the process is
	// about to drop, and recording incomplete coverage for a watcher with no
	// future would all be noise about a shutdown.
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return
	}
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
		// Neither this batch nor the capture can name this path, so the honest
		// answer is that notification coverage is not complete. Dropping it
		// silently -- what this replaces -- left a file that is never
		// reindexed and nothing saying so.
		//
		// This reports rather than arming a reconciliation, unlike the pending
		// overflow channel beside it, because a reconciliation would change
		// nothing: workspace.Walk skips the same path for the same reason, so
		// the path is unreachable to the indexer by any route. The next rescan
		// resets coverage to complete; that is a known hole, and the warning
		// below is what survives it.
		//
		// The one-shot flag and the coverage call are both reached only from
		// the single event-loop goroutine that owns relevant's only call site.
		if !w.longPathSeen {
			w.longPathSeen = true
			w.opts.Logger.Warn("watch event about an unrepresentable path",
				"component", "index.watch", "reason", ReasonPathTooLong, "path_bytes", len(slashed))
		}
		w.setCoverage(false, ReasonPathTooLong)
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
