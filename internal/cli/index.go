package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
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
)

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
					result, err := svc.Index(ctx, req)
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
			"database, WAL, temporary and content bytes, and unit reuse and parse counts. " +
			"It is not reported by default because measuring it costs more than the rest of " +
			"this report put together. A metric this host cannot measure is reported as " +
			"unavailable, never as zero.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resources, err := boolFlag(cmd, statusResourcesFlag)
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
					// A provider that could not be constructed publishes no
					// detection row of its own. Those rows are folded in by the
					// coordinator, before the capability report's own bound is
					// applied: appending them here pushed Completeness past
					// model.MaxCapabilityStates, a list IndexStatus.Validate
					// then rejects.
					status, err := svc.IndexStatus(ctx, req)
					if err != nil {
						return err
					}
					// The rows are read from the store, and the whole report
					// path was composed with fetching refused, so nothing here
					// can install the tool it is reporting on -- a report that
					// installed what it reports could only ever say the tool is
					// installed (ledger 159). The toolchain is not a service
					// operation, so this row keeps ws.Resolver()/ws.ToolStore().
					data := statusReport{Index: status,
						Tools: report(ws.ToolStore(), ws.Resolver().Status(ctx), nil)}
					out := cmd.OutOrStdout()
					if jsonRequested(cmd, args) {
						return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), data))
					}
					if err := writeIndexStatus(out, data.Index); err != nil {
						return err
					}
					return writeToolTable(out, data.Tools, false)
				})
		},
	}
	addRepoFlag(cmd)
	cmd.Flags().Bool(statusResourcesFlag, false,
		"also report the Section 23 resource accounting block, which an ordinary status does not measure")
	return cmd
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
	return writeText(cmd.OutOrStdout(), "%s", b.String())
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
	flushTableInto(tw)
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
