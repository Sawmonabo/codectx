package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

const (
	indexFullFlag    = "full"
	indexRebuildFlag = "rebuild"
	indexWatchFlag   = "watch"
)

// indexLockWait is the bounded wait a building command makes for the
// cross-process workspace lock. Section 13.2 offers a second caller exactly
// two answers -- `busy` or a bounded wait -- and this is the wait; nothing
// here ever becomes a second writer.
const indexLockWait = 10 * time.Second

// statusLockWait is zero on purpose. A report must not queue behind a build:
// telling the operator the workspace is busy is an answer, blocking a status
// call for ten seconds behind someone else's index is not.
const statusLockWait = 0

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
			"second run over an unchanged tree parses nothing.",
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
			ws, err := openWorkspace(cmd, indexLockWait)
			if err != nil {
				return err
			}
			defer ws.Close()
			if req.Watch {
				result, err := ws.Coordinator().Index(cmd.Context(), req)
				if err != nil {
					return err
				}
				if err := emitIndexResult(cmd, build, args, result); err != nil {
					return err
				}
				return runWatch(cmd, build, args, ws)
			}
			result, err := ws.Coordinator().Index(cmd.Context(), req)
			if err != nil {
				return err
			}
			return emitIndexResult(cmd, build, args, result)
		},
	}
	addRepoFlag(cmd)
	cmd.Flags().Bool(indexFullFlag, false, "rebuild every unit of the existing cache instead of reusing sealed ones")
	cmd.Flags().Bool(indexRebuildFlag, false, "create a new cache; the existing database is left untouched")
	cmd.Flags().Bool(indexWatchFlag, false, "keep running and refresh as the workspace changes")
	return cmd
}

// newRefreshCommand builds `codectx refresh`.
func newRefreshCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh [paths...]",
		Short: "Refresh the active generation, optionally only for the named paths",
		Long: "Refreshes the index incrementally. Named paths narrow the work to the " +
			"units those files belong to; with no paths the whole workspace is " +
			"reconciled. Either way the answer comes from the captured bytes, never " +
			"from a modification time or a Git status alone.",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := openWorkspace(cmd, indexLockWait)
			if err != nil {
				return err
			}
			defer ws.Close()
			result, err := ws.Coordinator().Refresh(cmd.Context(), args)
			if err != nil {
				return err
			}
			return emitIndexResult(cmd, build, args, result)
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
			"coherent with the worktree, watch coverage, and what the managed " +
			"toolchain has installed. Nothing is fetched to produce this report.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoFlagValue(cmd)
			if err != nil {
				return err
			}
			ws, err := app.OpenWorkspaceForReport(cmd.Context(), repo, statusLockWait)
			if err != nil {
				return err
			}
			defer ws.Close()
			status, err := ws.Coordinator().Status(cmd.Context())
			if err != nil {
				return err
			}
			// A provider that could not be constructed publishes no detection
			// row of its own; without these the report would simply not mention
			// a capability the operator asked for, which reads as "fine".
			status.Completeness = append(status.Completeness, ws.States()...)
			// The rows are read from the store, and the whole report path was
			// composed with fetching refused, so nothing here can install the
			// tool it is reporting on -- a report that installed what it
			// reports could only ever say the tool is installed (ledger 159).
			data := statusReport{Index: status,
				Tools: report(ws.ToolStore(), ws.Resolver().Status(cmd.Context()), nil)}
			out := cmd.OutOrStdout()
			if jsonRequested(cmd, args) {
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, cmd.Name(), data))
			}
			if err := writeIndexStatus(out, data.Index); err != nil {
				return err
			}
			return writeToolTable(out, data.Tools)
		},
	}
	addRepoFlag(cmd)
	return cmd
}

// newWatchCommand builds `codectx watch`.
func newWatchCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Refresh the index as the workspace changes",
		Long: "Watches the workspace and refreshes the index as it changes, coalescing " +
			"events over a debounce window and reconciling against the filesystem " +
			"periodically. A burst larger than the pending bounds collapses to one " +
			"full reconciliation rather than a partial list of paths, and the " +
			"workspace lock is held for the whole session: one writer, never two.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := openWorkspace(cmd, indexLockWait)
			if err != nil {
				return err
			}
			defer ws.Close()
			return runWatch(cmd, build, args, ws)
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
func runWatch(cmd *cobra.Command, build model.BuildInfo, args []string, ws *app.Workspace) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	machine := jsonRequested(cmd, args)
	var refreshes int64
	err := ws.Coordinator().Watch(cmd.Context(), func(result model.IndexResult) {
		refreshes++
		if machine {
			fmt.Fprintf(errOut, "refreshed generation=%d health=%s reused=%d built=%d carried=%d invalidated=%d\n",
				result.Binding.GenerationID, result.Health,
				result.UnitsReused, result.UnitsBuilt, result.UnitsCarried, result.UnitsInvalidated)
			return
		}
		fmt.Fprintf(out, "%s  generation %d  %s  reused %d  built %d  carried %d  invalidated %d\n",
			result.CompletedAt.Format(time.RFC3339), result.Binding.GenerationID, result.Health,
			result.UnitsReused, result.UnitsBuilt, result.UnitsCarried, result.UnitsInvalidated)
	})
	if err != nil {
		return err
	}
	if machine {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, cmd.Name(),
			map[string]int64{"refreshes": refreshes}))
	}
	return writeText(out, "watch stopped after %d %s\n", refreshes, plural(int(refreshes), "refresh", "refreshes"))
}

// openWorkspace opens the workspace a building command was pointed at.
func openWorkspace(cmd *cobra.Command, wait time.Duration) (*app.Workspace, error) {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return nil, err
	}
	return app.OpenWorkspace(cmd.Context(), repo, wait)
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

// emitIndexResult renders one completed run under the single-envelope rule.
func emitIndexResult(cmd *cobra.Command, build model.BuildInfo, args []string, result model.IndexResult) error {
	out := cmd.OutOrStdout()
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, cmd.Name(), result))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "generation  %d\nsnapshot    %s\nhealth      %s (%s)\n",
		result.Binding.GenerationID, result.Binding.SnapshotID, result.Health, result.Status)
	fmt.Fprintf(&b, "units       %d reused, %d built, %d carried stale, %d invalidated\n",
		result.UnitsReused, result.UnitsBuilt, result.UnitsCarried, result.UnitsInvalidated)
	fmt.Fprintf(&b, "files       %d captured, %d parsed\nelapsed     %s\n",
		result.FilesCaptured, result.FilesParsed, result.CompletedAt.Sub(result.StartedAt).Round(time.Millisecond))
	writeCapabilities(&b, result.Completeness)
	return writeText(out, "%s", b.String())
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
	b.WriteString("\n")
	return writeText(w, "%s", b.String())
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
