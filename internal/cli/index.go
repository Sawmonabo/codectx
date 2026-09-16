package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

const (
	indexFullFlag    = "full"
	indexRebuildFlag = "rebuild"
	indexWatchFlag   = "watch"
	// indexSCIPIndexFlag names a SCIP index that already exists in the
	// workspace, and indexSCIPInputsFlag the optional manifest describing what
	// produced it. Both are root-relative paths inside the snapshot: the
	// provider reads them through the captured bytes, so an absolute path or
	// one climbing out of the root is refused rather than followed.
	indexSCIPIndexFlag  = "scip-index"
	indexSCIPInputsFlag = "scip-inputs"
	// statusResourcesFlag asks `status` for the Section 23 accounting block.
	// It is opt-in because an ordinary status must stay cheap: sampling a
	// process tree and measuring the store costs more than reporting what the
	// coordinator already knows, and every `status` paying for it would make
	// the cheapest report in the tree one of the most expensive.
	statusResourcesFlag = "resources"
	// statusFollowFlag turns one status report into a live one. It reads and
	// writes nothing but its own output, so it costs a run that is going
	// nothing at all.
	statusFollowFlag = "follow"
)

// statusFollowInterval is how often --follow re-renders. One second is the
// rate a person reading a terminal can actually take in and the rate a script
// tailing the output can keep up with, and a stage worth watching lasts
// seconds. It is a constant and not a setting because a report that re-read
// faster would measure the host more often than the host changes, and one that
// re-read slower would stop being live -- neither is a choice an operator
// gains anything by making.
const statusFollowInterval = time.Second

// indexLockWait is the bounded wait a building command makes for the
// cross-process workspace lock. Section 13.2 offers a second caller exactly
// two answers -- `busy` or a bounded wait -- and this is the wait; nothing
// here ever becomes a second writer.
const indexLockWait = 10 * time.Second

// statusReport is the data payload of `status`: the coordinator's own index
// status, and the managed-toolchain rows the same report renders, which is
// what Task 22 Step 3 owes `status` beside `doctor`. They travel in one
// envelope because they answer one question -- what does this workspace hold
// and what can it run -- and a consumer that had to make two calls could see
// two different moments.
type statusReport struct {
	Index model.IndexStatus `json:"index"`
	Tools toolReport        `json:"tools"`
}

// newIndexCommand builds `codectx index`.
func newIndexCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Capture the workspace and build or refresh its generation",
		Long: "Captures an exact snapshot of the workspace, runs the providers whose " +
			"units are not reusable, and publishes the result as one new generation " +
			"after it validates. Unchanged units are reused rather than rebuilt, so a " +
			"second run over an unchanged tree parses nothing.\n\n" +
			"Work an optional provider defers past the base generation keeps running " +
			"after that generation is active, and each batch that seals is published " +
			"as a further generation before the command exits. Interrupting it keeps " +
			"everything already published and reports how much is still queued; set " +
			"providers.dependence.enabled = false to plan none of it.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := model.IndexRequest{}
			var err error
			if req.Full, err = boolFlag(cmd, indexFullFlag); err != nil {
				return err
			}
			if req.Rebuild, err = boolFlag(cmd, indexRebuildFlag); err != nil {
				return err
			}
			if req.Watch, err = boolFlag(cmd, indexWatchFlag); err != nil {
				return err
			}
			// The incoherent combination is rejected before anything is opened:
			// a rejection that first takes the workspace lock has already made
			// the workspace busy for a command it will not run.
			if err := req.Validate(); err != nil {
				return err
			}
			// The supplied index and its manifest are inputs of the
			// composition, not of the run: they reach the SCIP provider when
			// the workspace is opened, which is why they are resolved here and
			// carried in OpenOptions rather than in the request. scip.New
			// validates both -- a path that is absolute or climbs out of the
			// root, and a manifest with no index to describe, are refused as
			// argument errors by the open itself, so neither is re-checked.
			scipIndex, err := stringFlag(cmd, indexSCIPIndexFlag)
			if err != nil {
				return err
			}
			scipInputs, err := stringFlag(cmd, indexSCIPInputsFlag)
			if err != nil {
				return err
			}
			return runService(cmd, openForBuild(app.OpenOptions{Wait: indexLockWait, Rebuild: req.Rebuild,
				SCIPImport: scipIndex, SCIPManifest: scipInputs}),
				func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
					if req.Rebuild {
						// Section 12.2: the new cache is explicit and the old
						// one is left whole, so the operator is told where both
						// are and that the sessions and receipts they recorded
						// stay in the old one.
						if err := writeText(cmd.ErrOrStderr(),
							"a new cache was created at %s; the previous cache is untouched and still holds its sessions and receipts. "+
								"Point storage.data_dir at the new cache, or remove the old one once you no longer need them.\n",
							ws.DataDir()); err != nil {
							return err
						}
					}
					// Subscribed here and stopped the moment the run returns.
					// The stop is a barrier, so nothing printed below can race
					// a progressive line on the same writer, and a stage that
					// finishes late can never land after the envelope.
					stopStages := progressiveStages(cmd, args, ws)
					result, err := svc.Index(ctx, req)
					stopStages()
					if err != nil {
						return err
					}
					if req.Watch {
						// A watch session emits one envelope, at its end. The
						// base generation is the session's first refresh, not a
						// second result stream of its own.
						if err := emitIndexProgress(cmd, args, result); err != nil {
							return err
						}
						return runWatch(ctx, cmd, build, args, ws)
					}
					return drainIndex(cmd, build, args, ws, result)
				})
		},
	}
	addRepoFlag(cmd)
	cmd.Flags().Bool(indexFullFlag, false, "rebuild every unit of the existing cache instead of reusing sealed ones")
	cmd.Flags().Bool(indexRebuildFlag, false, "build into a new cache beside the configured one; the existing database is left untouched")
	cmd.Flags().Bool(indexWatchFlag, false, "keep running and refresh as the workspace changes")
	cmd.Flags().String(indexSCIPIndexFlag, "", "root-relative path of a SCIP index already present in the workspace, imported instead of being produced")
	cmd.Flags().String(indexSCIPInputsFlag, "", "root-relative path of the input-hash manifest describing that index; needs --"+indexSCIPIndexFlag)
	return cmd
}

// newRefreshCommand builds `codectx refresh`.
func newRefreshCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh [paths...]",
		Short: "Refresh the active generation, optionally only for the named paths",
		Long: "Refreshes the index incrementally. Named paths are notification hints " +
			"only: the refresh always re-reads the workspace and compares content " +
			"hashes, so the answer comes from the captured bytes, never from a hint, " +
			"a modification time or a Git status alone. Use --repo to point the " +
			"command at a repository other than the current directory.\n\n" +
			"Work an optional provider defers past this refresh's generation keeps " +
			"running after that generation is active, and each batch that seals is " +
			"published as a further generation before the command exits. Interrupting " +
			"it keeps everything already published and reports how much is still " +
			"queued; set providers.dependence.enabled = false to plan none of it.",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The named paths are read by nobody on purpose: the refresh
			// re-reads the workspace and compares content hashes either way, so
			// there is no request field to carry them into and a hint that
			// changed the answer would be the thing the help text promises it
			// is not.
			return runService(cmd, openForBuild(app.OpenOptions{Wait: indexLockWait}),
				func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
					result, err := svc.Refresh(ctx, model.IndexRequest{})
					if err != nil {
						return err
					}
					return drainIndex(cmd, build, args, ws, result)
				})
		},
	}
	addRepoFlag(cmd)
	return cmd
}

// newStatusCommand builds `codectx status`.
func newStatusCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the active generation, its coverage and the managed toolchain",
		Long: "Reports what the workspace actually holds: the active snapshot and " +
			"generation, per-capability freshness, whether the generation is still " +
			"coherent with the worktree, the watch coverage of this process's own " +
			"watch loops, and what the managed toolchain has installed. A watch " +
			"session in another process is not visible here: a separate `codectx " +
			"watch` reports as off. Nothing is fetched and nothing is written to " +
			"produce this report, and it takes no workspace lock, so it answers " +
			"while another process is indexing or watching.\n\n" +
			"--resources adds the Section 23 accounting block: parent and worker memory, " +
			"the query, cache and queue reservations, live subprocesses and pending events, " +
			"database, WAL, temporary and content bytes, unit reuse and parse counts, and " +
			"what each heavy analysis unit was reserved, capped and observed to peak at. " +
			"It also reports the run this repository last recorded -- the one still going if " +
			"a run is going, otherwise the one that produced the active generation -- and the " +
			"stages it spent its time in, costliest first, with a stage that is still going " +
			"reported as running with the time it has been going rather than as a finished wall. " +
			"It is not reported by default because measuring it costs more than the rest of " +
			"this report put together. A metric this host cannot measure is reported as " +
			"unavailable, never as zero.\n\n" +
			"--follow reports again every second until it is interrupted, which is the live " +
			"view from a second terminal while a run is going; with --json it emits one " +
			"complete envelope per second, each a whole snapshot rather than a change since " +
			"the last, and with --resources the host is measured again on every one.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resources, err := boolFlag(cmd, statusResourcesFlag)
			if err != nil {
				return err
			}
			follow, err := boolFlag(cmd, statusFollowFlag)
			if err != nil {
				return err
			}
			// Ruling Q1 makes the resource block a request field rather than a
			// second call: one report, one moment. The request is built and
			// screened here, where the command line is.
			req := model.StatusRequest{Resources: resources}
			if err := req.Validate(); err != nil {
				return err
			}
			return runService(cmd, openForReport(),
				func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
					snapshot := func() error { return writeStatus(ctx, cmd, build, args, ws, svc, req) }
					if !follow {
						return snapshot()
					}
					return followStatus(ctx, statusFollowInterval, snapshot)
				})
		},
	}
	addRepoFlag(cmd)
	cmd.Flags().Bool(statusResourcesFlag, false,
		"also report the Section 23 resource accounting block, which an ordinary status does not measure")
	cmd.Flags().Bool(statusFollowFlag, false,
		"re-report every second until interrupted; with --json one complete envelope per second, and with --resources the host is measured again each time")
	return cmd
}

// followStatus re-reports every interval until the operator stops it.
//
// Section 18.2 allows one envelope per answer, and every pass here is a whole
// answer: a --json follow emits one complete envelope per interval, each
// independently parseable, which is what a script tails. A delta would hand
// that script a partial snapshot to reassemble.
//
// The first report goes out before the first wait, because a live view that
// showed nothing for its first interval would be indistinguishable from one
// that had failed to start.
//
// Unlike `watch`, which reports its cancellation as the typed end of a session
// that still owed a final envelope, a follow has already delivered every
// snapshot whole and owes nothing further: the operator stopping it is how it
// ends, not a way it failed.
func followStatus(ctx context.Context, interval time.Duration, snapshot func() error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := snapshot(); err != nil {
			if isCanceled(err) {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// writeStatus reads one status and renders it, which is one whole answer: the
// report a plain `status` emits and one pass of a --follow. It is a function
// rather than a closure over the command because both callers need exactly the
// same bytes -- a live view whose rows differed from the one-shot report would
// be a second surface of the same model.
func writeStatus(ctx context.Context, cmd *cobra.Command, build model.BuildInfo, args []string,
	ws *app.Workspace, svc *app.Services, req model.StatusRequest) error {
	// A provider that could not be constructed publishes no detection row of
	// its own. Those rows are folded in by the coordinator, before the
	// capability report's own bound is applied: appending them here pushed
	// Completeness past model.MaxCapabilityStates, a list
	// IndexStatus.Validate then rejects.
	status, err := svc.IndexStatus(ctx, req)
	if err != nil {
		return err
	}
	// The rows are read from the store, and the whole report path was composed
	// with fetching refused, so nothing here can install the tool it is
	// reporting on -- a report that installed what it reports could only ever
	// say the tool is installed (ledger 159). The toolchain is not a service
	// operation, so this row keeps ws.Resolver()/ws.ToolStore().
	data := statusReport{Index: status, Tools: report(ws.ToolStore(), ws.Resolver().Status(ctx), nil)}
	out := cmd.OutOrStdout()
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), data))
	}
	if err := writeIndexStatus(out, data.Index); err != nil {
		return err
	}
	return writeToolTable(out, data.Tools, false)
}

// newWatchCommand builds `codectx watch`.
func newWatchCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Refresh the index as the workspace changes",
		Long: "Watches the workspace and refreshes the index as it changes, coalescing " +
			"filesystem notifications over index.watch_debounce and reconciling " +
			"against the filesystem every index.reconcile_interval whether or not " +
			"anything was notified. A burst larger than index.watch_pending_paths or " +
			"index.watch_pending_bytes collapses to one full reconciliation rather " +
			"than a partial list of paths, and a host that cannot notify falls back " +
			"to the periodic pass alone. Which of the two is covering the workspace " +
			"is reported by a status call made inside this process; a separate " +
			"`codectx status` process cannot see this session's coverage and always " +
			"reports the watch as off, though `status --resources` and `doctor` do " +
			"report this watch from its persisted heartbeat. Work an optional " +
			"provider deferred keeps " +
			"publishing for the whole session, and the workspace lock is held " +
			"throughout: one writer, never two.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// `watch` is a building command, so it opens through the same
			// runner and the same building opener `index` and `refresh` use:
			// one open dance, one Close, and a cancellation typed once. It
			// declares no --timeout, so no deadline is installed over a session
			// that is meant to run until it is stopped, and the *Services it is
			// handed goes unread because streaming is deliberately not a facade
			// operation (digest 17 Section 4) -- Coordinator is.
			return runService(cmd, openForBuild(app.OpenOptions{Wait: indexLockWait}),
				func(ctx context.Context, ws *app.Workspace, _ *app.Services) error {
					return runWatch(ctx, cmd, build, args, ws)
				})
		},
	}
	addRepoFlag(cmd)
	return cmd
}

// runWatch drives the coordinator's watch loop until it is stopped.
//
// A watch is a stream, and Section 18.2 allows exactly one envelope on stdout,
// so a --json watch reports each refresh as a log line on stderr and emits its
// single envelope when the watch ends -- which, for a session the operator
// interrupts, is the typed cancellation. Human output is progressive, because
// a human watching a terminal is the one consumer a stream is for.
//
// Every progressive write goes through writeText, and a failed one stops the
// watch from inside the callback. Watch's callback cannot report an error, and
// returning one after Watch returns is not enough: Watch only returns when the
// context ends, so `codectx watch | head -1` would keep indexing into a dead
// pipe for the rest of the session -- which is the symptom (Section 18.2: a
// broken stdout pipe fails the command and does not confirm delivery).
func runWatch(ctx context.Context, cmd *cobra.Command, build model.BuildInfo, args []string, ws *app.Workspace) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	machine := jsonRequested(cmd, args)
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var refreshes int64
	var writeErr error
	err := ws.Coordinator().Watch(ctx, func(result model.IndexResult) {
		refreshes++
		if writeErr != nil {
			return
		}
		w, line := out, humanRefreshLine(result)
		if machine {
			w, line = errOut, machineRefreshLine("refreshed", result)
		}
		if err := writeText(w, "%s", line); err != nil {
			writeErr = err
			stop()
		}
	})
	// The write failure is the real cause; the cancellation it triggered is
	// only how the loop was stopped, so it must not mask it.
	if writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	if machine {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd),
			map[string]int64{"refreshes": refreshes}))
	}
	return writeText(out, "watch stopped after %d %s\n", refreshes, plural(int(refreshes), "refresh", "refreshes"))
}

// drainIndex publishes the generation a one-shot command has just produced and
// then runs the deferred queue to empty, which is what makes
// providers.dependence.enabled = "auto" its documented self in a one-shot run
// (Section 11.6, ruling Q9). Both one-shot building commands end here --
// `codectx index` with the generation its Index call activated and `codectx
// refresh` with the one its Refresh call activated -- because the lifetime that
// makes the abandonment possible is the same in both: the process exits as soon
// as that generation is active.
//
// Without this the coordinator is closed the moment that generation activates,
// and every unit it deferred is abandoned unbuilt and unreported: the shipped
// default silently produced no dependence fact at all. There is no new flag,
// because "do not plan that work" already has a spelling:
// providers.dependence.enabled = false.
//
// Section 18.2's one envelope still holds across the whole run: a --json
// invocation reports that first generation and each publication on stderr and
// emits its single envelope, carrying the last generation published, at the
// end. An interruption is not a failure here -- the first generation is active
// and every published generation stands -- so the command succeeds and says in
// a warning how much work is still queued.
func drainIndex(cmd *cobra.Command, build model.BuildInfo, args []string, ws *app.Workspace, base model.IndexResult) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	machine := jsonRequested(cmd, args)
	if err := emitIndexProgress(cmd, args, base); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(cmd.Context())
	defer stop()
	latest := base
	var writeErr error
	drainErr := ws.Coordinator().Drain(ctx, func(result model.IndexResult) {
		latest = result
		if writeErr != nil {
			return
		}
		w, line := out, humanRefreshLine(result)
		if machine {
			w, line = errOut, machineRefreshLine("published", result)
		}
		if err := writeText(w, "%s", line); err != nil {
			writeErr = err
			stop()
		}
	})
	if writeErr != nil {
		return writeErr
	}
	var warnings []string
	if pending := ws.Coordinator().Pending(); pending.Units > 0 {
		warnings = append(warnings, fmt.Sprintf("%d deferred %s not built; run `codectx watch` to finish them",
			pending.Units, plural(pending.Units, "unit is", "units are")))
	} else if drainErr != nil && !isCanceled(drainErr) {
		warnings = append(warnings, "the deferred queue stopped early: "+diagnostic(drainErr))
	}
	if machine {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), latest, warnings...))
	}
	for _, warning := range warnings {
		if err := writeText(out, "warning     %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

// isCanceled reports whether an error is the operator stopping the work, which
// on the drain path is an expected end and not a defect.
func isCanceled(err error) bool {
	var typed *model.Error
	return errors.Is(err, context.Canceled) || (errors.As(err, &typed) && typed.Code == model.CodeCanceled)
}

// diagnostic is the product-authored text of a typed failure. Section 20.1
// keeps source bodies, secrets, environment and raw analyzer output out of
// ordinary output; a model.Error message is none of those, and a bare code
// cannot tell an operator what happened.
func diagnostic(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.Code + ": " + typed.Message
	}
	return err.Error()
}

func humanRefreshLine(result model.IndexResult) string {
	return fmt.Sprintf("%s  generation %d  %s  reused %d  built %d  carried %d  invalidated %d\n",
		result.CompletedAt.Format(time.RFC3339), result.Binding.GenerationID, result.Health,
		result.UnitsReused, result.UnitsBuilt, result.UnitsCarried, result.UnitsInvalidated)
}

func machineRefreshLine(event string, result model.IndexResult) string {
	return fmt.Sprintf("%s generation=%d health=%s reused=%d built=%d carried=%d invalidated=%d\n",
		event, result.Binding.GenerationID, result.Health,
		result.UnitsReused, result.UnitsBuilt, result.UnitsCarried, result.UnitsInvalidated)
}

// addRepoFlag declares `--repo` on one command. The flag name is the one
// `tools` already uses: a second spelling of "which repository is this about"
// would be two contracts for one question.
func addRepoFlag(cmd *cobra.Command) {
	cmd.Flags().String(toolsRepoFlag, ".", "repository whose resolved configuration and workspace this command is about")
}

func repoFlagValue(cmd *cobra.Command) (string, error) {
	repo, err := cmd.Flags().GetString(toolsRepoFlag)
	if err != nil {
		return "", &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return repo, nil
}

func boolFlag(cmd *cobra.Command, name string) (bool, error) {
	v, err := cmd.Flags().GetBool(name)
	if err != nil {
		return false, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

// progressiveStages prints one line per top-level stage of the run as it
// finishes, so a long index says what it is doing while it does it instead of
// staying silent until the completion block.
//
// The rows are the ones the run ledger recorded, reached through the workspace:
// nothing here measures anything, and nothing here opens a ledger -- the file
// has a single writer, and a second would contend with the run these lines
// describe. A workspace that composed no ledger prints nothing.
//
// Only the run's own top-level stages are printed. A unit nested inside a stage
// is already counted in it, and a line per unit would bury the stage the time
// actually went to. No share of the run is printed either: the run's own wall
// is not known until it ends, and a share computed from nothing would be a
// number the operator could not trust.
//
// The returned stop is called before ANY other output of this command, and is
// a barrier: when it returns, no progressive line is being written and none
// will start, whatever the ledger goes on publishing. That is what keeps these
// lines and the result off each other on one writer, and keeps a stage that
// finishes late out of the single --json envelope.
func progressiveStages(cmd *cobra.Command, args []string, ws *app.Workspace) (stop func()) {
	machine := jsonRequested(cmd, args)
	// A --json consumer's stdout carries the one envelope and nothing else, so
	// the progressive lines take the stderr channel the refresh lines already
	// take for the same reason.
	w := cmd.OutOrStdout()
	if machine {
		w = cmd.ErrOrStderr()
	}
	// The mutex, and not a flag, is what makes stop a barrier: a flag would
	// stop the NEXT line and leave one already being written racing the
	// result below on the same writer.
	var mu sync.Mutex
	var done bool
	ws.Spans(func(row model.StageRecord) {
		mu.Lock()
		defer mu.Unlock()
		if done || row.ParentSeq != nil {
			return
		}
		if machine {
			writeText(w, "stage %s wall_ms=%d outcome=%s in=%d out=%d\n", //nolint:errcheck // a progress line lost to a closed pipe must not fail the run; the result below reports the same failure.
				row.Stage, row.WallMS, row.Outcome, row.ItemsIn, row.ItemsOut)
			return
		}
		writeText(w, "stage       %s %s%s, %s, in %d, out %d\n", //nolint:errcheck // as above.
			row.Stage, stageProgressScope(row), wallMetric(row.WallMS, row.Running, row.FinishedAt),
			stageOutcome(row), row.ItemsIn, row.ItemsOut)
	})
	return func() {
		mu.Lock()
		done = true
		mu.Unlock()
	}
}

// stageProgressScope is the scope a progressive line names, with the separator
// it needs, and nothing at all for a stage that has no scope: a bare "-" in
// running prose reads as a missing value rather than as a stage that is simply
// not about one scope.
func stageProgressScope(row model.StageRecord) string {
	if row.ScopeKey == "" {
		return ""
	}
	return row.ScopeKey + " "
}

// emitIndexProgress renders one completed run as progress rather than as the
// result: human output on stdout, and for a --json consumer one log line on
// stderr, because the envelope that run belongs to has not been written yet.
func emitIndexProgress(cmd *cobra.Command, args []string, result model.IndexResult) error {
	if jsonRequested(cmd, args) {
		return writeText(cmd.ErrOrStderr(), "%s", machineRefreshLine("indexed", result))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "generation  %d\nsnapshot    %s\nhealth      %s (%s)\n",
		result.Binding.GenerationID, result.Binding.SnapshotID, result.Health, result.Status)
	fmt.Fprintf(&b, "units       %d reused, %d built, %d carried stale, %d invalidated\n",
		result.UnitsReused, result.UnitsBuilt, result.UnitsCarried, result.UnitsInvalidated)
	fmt.Fprintf(&b, "files       %d captured, %d parsed\nelapsed     %s\n",
		result.FilesCaptured, result.FilesParsed, result.CompletedAt.Sub(result.StartedAt).Round(time.Millisecond))
	writeCapabilities(&b, result.Completeness)
	writeIndexRunLedger(&b, result.Run, result.Stages, result.StagesOmitted)
	return writeText(cmd.OutOrStdout(), "%s", b.String())
}

// writeIndexRunLedger appends what the run cost and which of its stages that
// cost went to, in this block's label-and-value idiom.
//
// Only the run's own top-level stages are listed, and each with its share of
// the run. A unit nested under a stage is already counted inside it, so listing
// both would report shares that add up to more than the run and leave the
// operator unable to see which stage the time actually went to.
func writeIndexRunLedger(b *strings.Builder, run *model.RunRecord, stages []model.StageRecord, omitted int64) {
	if run == nil {
		return
	}
	fmt.Fprintf(b, "run         %s in %s, %d planned, %d succeeded, %d failed, %d subdivided\n",
		run.Outcome, wallMetric(run.WallMS, run.Outcome == runOutcomeRunning, run.FinishedAt),
		run.UnitsPlanned, run.UnitsSucceeded, run.UnitsFailed, run.UnitsSubdivided)
	if run.EventsDropped > 0 {
		fmt.Fprintf(b, "incomplete  %d accounting %s dropped; the stages below are not the whole run\n",
			run.EventsDropped, plural(int(run.EventsDropped), "event was", "events were"))
	}
	for _, stage := range topLevelStagesByWall(stages) {
		fmt.Fprintf(b, "stage       %s %s, %s of the run, %s, in %d, out %d\n",
			stage.Stage, wallMetric(stage.WallMS, stage.Running, stage.FinishedAt),
			shareMetric(stage.ShareOfWall), stageOutcome(stage), stage.ItemsIn, stage.ItemsOut)
	}
	// The run recorded more stages than one result carries. Saying how many is
	// what keeps the lines above a page of the run's accounting rather than a
	// silently short list read as the whole of it.
	if omitted > 0 {
		fmt.Fprintf(b, "omitted     %d further %s beyond this result's page\n",
			omitted, plural(int(omitted), "stage was recorded", "stages were recorded"))
	}
}

// topLevelStagesByWall is the run's own stages, costliest first, with the run's
// ordinal breaking a tie so two runs of the same shape print the same lines. It
// sorts a copy: the result it is given is the one the envelope carries, and
// reordering that would make the human block and the --json rows two orders of
// one list.
func topLevelStagesByWall(stages []model.StageRecord) []model.StageRecord {
	top := make([]model.StageRecord, 0, len(stages))
	for _, stage := range stages {
		if stage.ParentSeq == nil {
			top = append(top, stage)
		}
	}
	slices.SortStableFunc(top, func(a, b model.StageRecord) int {
		if order := cmp.Compare(b.WallMS, a.WallMS); order != 0 {
			return order
		}
		return cmp.Compare(a.Seq, b.Seq)
	})
	return top
}

// shareMetric renders a stage's share of its run. A share of zero is not a
// measured zero: it is what a run whose own wall was never measured leaves
// behind, and printing "0%" beside a stage with a real wall would claim the
// stage took none of a run it plainly took time out of.
func shareMetric(share float64) string {
	if share <= 0 {
		return metricUnavailable
	}
	return fmt.Sprintf("%.0f%%", share*100)
}

// writeIndexStatus renders the human status block. Every value is one the
// coordinator produced -- identifiers, enum spellings and counts -- so nothing
// here needs display sanitization beyond the warnings, which are the product's
// own bounded strings.
func writeIndexStatus(w io.Writer, s model.IndexStatus) error {
	var b strings.Builder
	fmt.Fprintf(&b, "generation  %d\nsnapshot    %s\nhealth      %s\ncoherence   %s\ncapture     %s\n",
		s.Binding.GenerationID, s.Binding.SnapshotID, s.Health, s.Coherence, s.CaptureConsistency)
	fmt.Fprintf(&b, "source      %d files, %d bytes\n", s.FileCount, s.SourceBytes)
	if s.ActivatedAt != nil {
		fmt.Fprintf(&b, "activated   %s\n", s.ActivatedAt.Format(time.RFC3339))
	}
	watchState := "off"
	if s.WatchActive {
		// "complete" is a statement about notification coverage, not about
		// freshness: an incomplete watch is why the reconciliation time beside
		// it matters, so the two are always printed together.
		watchState = "active, coverage incomplete"
		if s.WatchComplete {
			watchState = "active, coverage complete"
		}
	}
	fmt.Fprintf(&b, "watch       %s, %d pending %s\n", watchState, s.PendingPaths, plural(int(s.PendingPaths), "path", "paths"))
	if s.LastReconciledAt != nil {
		fmt.Fprintf(&b, "reconciled  %s\n", s.LastReconciledAt.Format(time.RFC3339))
	}
	writeCapabilities(&b, s.Completeness)
	for _, warning := range s.Warnings {
		fmt.Fprintf(&b, "warning     %s\n", warning)
	}
	// Absent unless --resources asked for it, which is the same thing the
	// --json consumer sees: the field is omitted rather than rendered empty.
	if s.Resources != nil {
		writeResources(&b, *s.Resources)
	}
	b.WriteString("\n")
	return writeText(w, "%s", b.String())
}

// metricUnavailable is how an unmeasured metric renders. Section 22 requires an
// unavailable metric be recorded as unavailable and never as zero, and every
// field of model.ResourceReport is a pointer for exactly that reason: nil means
// this host or this platform did not measure it, while 0 is a real measurement
// of zero. Printing a nil as 0 would report a process using no memory, which is
// the one reading an operator would act on and the one that is never true.
const metricUnavailable = "unavailable"

// writeResources renders the Section 23 accounting block. Every field of the
// report is listed, present or not: a row that disappeared when its metric did
// would leave the operator unable to tell "this build does not report that" from
// "this host cannot measure it".
func writeResources(b *strings.Builder, r model.ResourceReport) {
	b.WriteString("\nresources\n")
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	for _, row := range []struct{ label, value string }{
		{"parent rss", byteMetric(r.ParentRSSBytes)},
		{"peak parent rss", byteMetric(r.PeakParentRSSBytes)},
		{"base worker rss", byteMetric(r.BaseWorkerRSSBytes)},
		{"go managed", byteMetric(r.GoManagedBytes)},
		{"native worker", byteMetric(r.NativeWorkerBytes)},
		{"query reservation", byteMetric(r.QueryReservationBytes)},
		{"cache reservation", byteMetric(r.CacheReservationBytes)},
		{"queue reservation", byteMetric(r.QueueReservationBytes)},
		{"database", byteMetric(r.DatabaseBytes)},
		{"wal", byteMetric(r.WALBytes)},
		{"temp", byteMetric(r.TempBytes)},
		{"content store", byteMetric(r.CASBytes)},
		{"freed this run", byteMetric(r.FreedBytes)},
		{"scratch held", byteMetric(r.ScratchBytes)},
		{"awaiting freeing", byteMetric(r.PendingFreeBytes)},
		{"live subprocesses", countMetric(r.LiveSubprocesses)},
		{"pending events", countMetric(r.PendingEvents)},
		{"units reused", countMetric(r.UnitsReused)},
		{"units parsed", countMetric(r.UnitsParsed)},
	} {
		fmt.Fprintf(tw, "  %s\t%s\n", row.label, row.value)
	}
	// What the freeing was for, under the figure it breaks down. Sorted so
	// two runs of the same shape print the same lines.
	for _, purpose := range slices.Sorted(maps.Keys(r.FreedByPurpose)) {
		n := r.FreedByPurpose[purpose]
		fmt.Fprintf(tw, "    freed for %s\t%s\n", purpose, byteMetric(&n))
	}
	// A removal that could not be made holds its space in "awaiting freeing"
	// and will go on holding it, so it is named under that figure rather than
	// left to look like a backlog the pace is still working through.
	for _, stuck := range r.StuckFrees {
		fmt.Fprintf(tw, "    stuck %s\t%s\n", stuck.Entry, stuck.Reason)
	}
	// One line per heavy analysis unit: what it was admitted against, the cap
	// it ran under, and what its process tree actually reached. Reading the
	// three together is the whole point -- a peak far under the cap says the
	// unit was serialized behind memory it never used.
	for _, u := range r.AnalyzerUnits {
		fmt.Fprintf(tw, "    unit %s\treserved %d bytes, cap %d bytes (export %d bytes), allocation %s, observed peak %s\n",
			u.ScopeKey, u.ReservationBytes, u.HeapCapBytes, u.ExportHeapCapBytes,
			byteMetric(u.AllocationBytes), byteMetric(u.ObservedPeakBytes))
	}
	flushTableInto(tw)
	writeRunLedger(b, r.Run, r.Stages, r.StagesOmitted)
}

// writeRunLedger renders the recorded run and its stages: what the run cost,
// and where it spent that cost. The run row comes first because a stage's
// share of a run means nothing without the run it is a share of, and the
// stages follow in the order the report assembled them -- wall descending --
// so the table and the --json rows are the same rows in the same order.
//
// A stage that is still going is rendered as running with the time it has been
// going, never as a finished wall. "This is taking a long time" and "this took
// a long time" are different facts, and a live stage whose elapsed time printed
// like a measurement would tell an operator that a stalled stage had finished
// fast.
func writeRunLedger(b *strings.Builder, run *model.RunRecord, stages []model.StageRecord, omitted int64) {
	if run == nil {
		return
	}
	b.WriteString("\nrun\n")
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "  stage\tscope\twall\tcpu\tpeak\tin\tout\toutcome\n")
	// The run's own row carries the counts the run keeps for itself: the files
	// it took in and the units it got out. Its processor time is not a figure
	// anything measures for the process as a whole, so the column is
	// unavailable here rather than a sum of the stages that would silently
	// omit every stage whose time could not be attributed.
	fmt.Fprintf(tw, "  run\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
		runScope(run), wallMetric(run.WallMS, run.Outcome == runOutcomeRunning, run.FinishedAt),
		metricUnavailable, byteMetric(run.ProcessPeakRSSBytes),
		run.FileCount, run.UnitsSucceeded, run.Outcome)
	for _, stage := range stages {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			stage.Stage, stageScope(stage), wallMetric(stage.WallMS, stage.Running, stage.FinishedAt),
			cpuMetric(stage), byteMetric(stage.PeakRSSBytes),
			stage.ItemsIn, stage.ItemsOut, stageOutcome(stage))
	}
	// Why a stage or a unit reached no output, under the row that reports it:
	// a table column cannot hold it, and an outcome with the reason only in
	// --json would leave the operator reading the table to guess. The outcome
	// is repeated rather than assumed, because a unit whose tool is absent is
	// unavailable and not failed, and its scope is named because a provider
	// can have many units unavailable for different reasons at once.
	for _, stage := range stages {
		if stage.Failure != "" {
			fmt.Fprintf(tw, "    %s %s %s\t%s\n", stage.Stage, stageScope(stage), stage.Outcome, stage.Failure)
		}
	}
	flushTableInto(tw)
	// The run recorded more stages than one page carries. A table that did not
	// say how many it dropped would present a page of the run's accounting as
	// the whole of it, and an operator reading it would draw the shares and the
	// costliest stage from a list that is missing rows.
	if omitted > 0 {
		fmt.Fprintf(b, "  omitted     %d further %s beyond this page\n",
			omitted, plural(int(omitted), "stage was recorded", "stages were recorded"))
	}
	// Accounting the bounded bus refused rather than made the run wait for it.
	// Above zero the stages above are known to be an incomplete account of the
	// run, and a table that did not say so would read as the whole of it.
	if run.EventsDropped > 0 {
		fmt.Fprintf(b, "  incomplete  %d accounting %s dropped; the stages above are not the whole run\n",
			run.EventsDropped, plural(int(run.EventsDropped), "event was", "events were"))
	}
}

// runOutcomeRunning is the one outcome spelling a renderer has to recognise:
// it is what makes a run's elapsed time elapsed rather than measured.
const runOutcomeRunning = "running"

// wallMetric renders a span's or a run's time: its measurement once it has
// finished, its elapsed time so far while it is running, and unavailable for
// one that ended without anything measuring it. The last is a real case -- a
// run whose process died leaves no finish, and rendering its zero as "0s"
// would present the run an operator is investigating as one that took no time.
func wallMetric(wallMS int64, running bool, finishedAt *time.Time) string {
	switch {
	case running:
		return "running " + millisMetric(wallMS)
	case finishedAt == nil:
		return metricUnavailable
	}
	return millisMetric(wallMS)
}

// millisMetric renders a measured millisecond count as a duration, in the same
// idiom every other duration in this package is rendered in.
func millisMetric(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

// cpuMetric renders a stage's processor time. The two halves are measured
// together or not at all, so they are summed; a stage with neither says why it
// has neither, because "the platform does not sample this" and "this stage ran
// beside other work, so the process counters do not measure it" are different
// answers and only the second means the number could never exist.
func cpuMetric(stage model.StageRecord) string {
	if stage.CPUUserMS == nil && stage.CPUSysMS == nil {
		if stage.CPUUnattributed != "" {
			return metricUnavailable + " (" + stage.CPUUnattributed + ")"
		}
		return metricUnavailable
	}
	var total int64
	if stage.CPUUserMS != nil {
		total += *stage.CPUUserMS
	}
	if stage.CPUSysMS != nil {
		total += *stage.CPUSysMS
	}
	return millisMetric(total)
}

// runScope names what the run produced. A run that never reached a generation
// says so rather than printing a zero that would read as generation zero.
func runScope(run *model.RunRecord) string {
	if run.GenerationID == nil {
		return "no generation"
	}
	return fmt.Sprintf("generation %d", *run.GenerationID)
}

// stageScope names what a stage was working on. Both parts are optional -- a
// whole-run stage has no scope and an in-process one has no provider -- so a
// stage with neither prints a placeholder rather than an empty column that
// would run into the next one.
func stageScope(stage model.StageRecord) string {
	switch {
	case stage.ScopeKey != "" && stage.Provider != "":
		return stage.ScopeKey + " (" + stage.Provider + ")"
	case stage.ScopeKey != "":
		return stage.ScopeKey
	case stage.Provider != "":
		return stage.Provider
	}
	return "-"
}

// stageOutcome renders a stage's outcome with the diagnostic code that names
// the failure class, where there is one.
func stageOutcome(stage model.StageRecord) string {
	if stage.DiagnosticCode != "" {
		return stage.Outcome + " (" + stage.DiagnosticCode + ")"
	}
	return stage.Outcome
}

func byteMetric(v *uint64) string {
	if v == nil {
		return metricUnavailable
	}
	return fmt.Sprintf("%d bytes", *v)
}

func countMetric(v *int64) string {
	if v == nil {
		return metricUnavailable
	}
	return fmt.Sprintf("%d", *v)
}

// writeCapabilities renders per-capability freshness. A capability with no row
// is not printed as fresh and is not invented: the absence of a row is itself
// what "this provider published nothing" looks like, and Section 13.3 forbids
// reading available-with-no-units as fresh coverage.
func writeCapabilities(b *strings.Builder, states []model.CapabilityState) {
	if len(states) == 0 {
		b.WriteString("capabilities none reported\n")
		return
	}
	counts := map[string]int{}
	for _, st := range states {
		counts[string(st.State)]++
	}
	parts := make([]string, 0, len(counts))
	for _, state := range []model.CapabilityStateValue{model.CapabilityFresh, model.CapabilityPartial,
		model.CapabilityStale, model.CapabilityUnavailable, model.CapabilityFailed} {
		if n := counts[string(state)]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state))
		}
	}
	fmt.Fprintf(b, "capabilities %s\n", strings.Join(parts, ", "))
}
