package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

const resetReindexFlag = "reindex"

// resetNotice is what an operator is told beside what a reset removed, for the
// reason gcNotice gives: the space goes back a window at a time because a
// burst of frees stalls some virtual hosts, and a large data directory can
// stall one even so.
const resetNotice = "Freeing a large amount of space can stall some virtual hosts for about a minute, " +
	"a short while after this command returns. The data directory is given back a window at a time " +
	"to keep that as small as it can be, so a large workspace takes a while to reset."

// newResetCommand builds `codectx reset`.
//
// It is the one recovery for a workspace whose data cannot be trusted or read
// -- a corrupt database, one written by another schema, a damaged search index
// -- and it is safe because nothing in the data directory is a source of
// truth: the repository is, and the next index produces all of it again.
func newResetCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset [path]",
		Short: "Remove the workspace's data directory so the next index builds from nothing",
		Long: "Empties the data directory (storage.data_dir) of the named repository, or of the " +
			"current directory when no path is given: the database and its side files, the search " +
			"index, the content store, spools, scratch pools and the run ledger. Nothing in it is a " +
			"source of truth -- the repository is -- so the next `codectx index` builds all of it " +
			"again. Open sessions, cursors and receipts go with it: they cite generations that no " +
			"longer exist.\n\n" +
			"The database is never opened, so a store that is corrupt or of another schema is " +
			"removed like any other. The workspace lock is taken first and held until everything " +
			"is gone; a running index, refresh or watch beat holding it is named in the " +
			"CTX_WORKSPACE_BUSY refusal and nothing is removed. A watch or MCP server open on the " +
			"workspace refuses the reset the same way, idle or not: stop it, then reset.\n\n" +
			"Only the resolved data directory is emptied. A symbolic link inside it is removed as " +
			"a link and its target is left alone, and a data directory that is, or holds, the " +
			"repository, the home directory or the filesystem root is refused. The two lock files " +
			"stay, empty, for the next holders to take. A cache `codectx index --rebuild` " +
			"made beside the data directory is not the data directory and is not touched.\n\n" +
			resetNotice + "\n\n" +
			"--reindex runs a full index straight afterwards; when that index fails, what was removed " +
			"is still reported, then the failure. --json prints every entry removed " +
			"with the bytes its removal gave back. The repository is named as a positional path, " +
			"or with --repo, and naming it both ways is refused.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReset(cmd, args, build)
		},
	}
	addRepoFlag(cmd)
	cmd.Flags().Bool(resetReindexFlag, false, "run a full index once the data directory has been emptied")
	return cmd
}

func runReset(cmd *cobra.Command, args []string, build model.BuildInfo) error {
	repo, err := repoTarget(cmd, args)
	if err != nil {
		return err
	}
	reindex, err := boolFlag(cmd, resetReindexFlag)
	if err != nil {
		return err
	}
	opts := app.ResetOptions{Reindex: reindex}
	if reindex {
		// The index waits for a holder that is getting somewhere, as
		// `codectx index` does, and says what it is behind while it waits.
		opts.OnWaiting = waitingForHolder(cmd, args)
	}
	report, err := app.ResetWorkspace(cmd.Context(), repo, opts)
	out := cmd.OutOrStdout()
	if err != nil && report.Removed != nil {
		// The removal happened and the index after it failed: what went is
		// reported, then the failure.
		failure := queryFailure(err)
		var typed *model.Error
		if !errors.As(failure, &typed) {
			return failure
		}
		if jsonRequested(cmd, args) {
			return &failedWithData{err: typed, data: report}
		}
		if werr := writeResetReport(out, report); werr != nil {
			return werr
		}
		return failure
	}
	if err != nil {
		return queryFailure(err)
	}
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), report, resetNotice))
	}
	if err := writeResetReport(out, report); err != nil {
		return err
	}
	if report.Index != nil {
		return emitIndexProgress(cmd, args, *report.Index)
	}
	return nil
}

// writeResetReport prints what went, entry by entry, and the notice.
func writeResetReport(w io.Writer, r model.WorkspaceReset) error {
	var b strings.Builder
	if len(r.Removed) == 0 {
		fmt.Fprintf(&b, "%s holds nothing to remove\n", r.DataDir)
		return writeText(w, "%s", b.String())
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, e := range r.Removed {
		fmt.Fprintf(tw, "removed %s\t%d bytes freed\n", e.Path, e.Bytes)
	}
	fmt.Fprintf(tw, "total\t%d bytes freed\n", r.FreedBytes)
	flushTableInto(tw)
	b.WriteString("\n" + resetNotice + "\n")
	return writeText(w, "%s", b.String())
}
