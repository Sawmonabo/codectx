package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/scratch"
	"github.com/spf13/cobra"
)

// gcNotice is what an operator is told before the space goes, and it is the
// reason this is a command rather than something a run does on its own.
//
// Freeing is the expensive act on the hosts this product is run on. A burst of
// frees on a machine whose disk is a sparse image on its host leaves the host
// owing work it never reports: every disk request in flight about a minute
// later waits, for as long as the backlog takes, and nothing inside the machine
// can observe or wait for it. This command gives the space back a window at a
// time for that reason, which is slow on purpose, and even so a very large
// collection can leave such a host stalled.
const gcNotice = "Freeing a large amount of space can stall some virtual hosts for about a minute, " +
	"a short while after this command returns. The space is given back a window at a time to keep " +
	"that as small as it can be, so a large collection takes a while."

// newGCCommand builds `codectx gc`.
//
// The product never frees its working files on its own: it writes over them
// next time, because creating and freeing them per use is what hands the host
// the burst above. So the disk a workspace holds only ever grows towards its
// working set, and `scratch_bytes` in the resources block says how much that
// is. This is the request that gives it back.
//
// It has no timer and no threshold. A threshold would be this product deciding
// on the operator's behalf that now is a good moment to stall their host, which
// is precisely the decision it has no way to make.
func newGCCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Give the workspace's pooled scratch space back to the filesystem",
		Long: "Empties the scratch pools of the named repository's workspace: the sort runs, " +
			"staging databases, search state, blob staging surfaces and engine temporaries " +
			"the product keeps and writes over instead of freeing, including those left by " +
			"runs that are no longer running.\n\n" +
			"What the pools hold is printed first, by what each surface was taken for, so the " +
			"space can be judged before it is given back.\n\n" + gcNotice + "\n\n" +
			"There is no timer and no threshold: the pools are emptied when this is run and " +
			"at no other time.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGC(cmd, args, build)
		},
	}
	addRepoFlag(cmd)
	return cmd
}

func runGC(cmd *cobra.Command, args []string, build model.BuildInfo) error {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return err
	}
	ws, err := app.OpenWorkspaceForReport(cmd.Context(), repo)
	if err != nil {
		return queryFailure(err)
	}
	defer ws.Close()

	report := model.ScratchCollection{Pools: []model.ScratchPool{}}
	for _, a := range gcArenas(ws.DataDir()) {
		pool := model.ScratchPool{Directory: a.Root(), HeldByPurpose: map[string]uint64{}}
		byPurpose, err := a.BytesByPurpose()
		if err != nil {
			return &model.Error{Code: model.CodeInternal,
				Message: "reading the scratch pool " + a.Root() + ": " + err.Error()}
		}
		for p, n := range byPurpose {
			if n > 0 {
				pool.HeldByPurpose[string(p)] = uint64(n)
				pool.HeldBytes += uint64(n)
			}
		}
		collected, err := a.Empty()
		if err != nil {
			if errors.Is(err, scratch.ErrInUse) {
				return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
					Message:     "the scratch pool " + a.Root() + " is in use: " + err.Error(),
					Remediation: "Another operation of this process holds a scratch surface. Run this again when it has finished."}
			}
			return &model.Error{Code: model.CodeInternal,
				Message: "emptying the scratch pool " + a.Root() + ": " + err.Error()}
		}
		pool.FreedBytes = uint64(max(collected.FreedBytes, 0))
		for _, stuck := range collected.Stuck {
			pool.StuckFrees = append(pool.StuckFrees,
				model.StuckFree{Entry: stuck.Entry, Reason: stuck.Reason})
		}
		report.HeldBytes += pool.HeldBytes
		report.FreedBytes += pool.FreedBytes
		report.Pools = append(report.Pools, pool)
	}
	sort.Slice(report.Pools, func(i, j int) bool { return report.Pools[i].Directory < report.Pools[j].Directory })

	out := cmd.OutOrStdout()
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), report, gcNotice))
	}
	return writeGCReport(out, report)
}

// gcArenas is every scratch pool of this workspace: the ones this process has
// opened, and every one on the disk under the data directory, which is where a
// run that is no longer running left its own.
//
// Reading the disk rather than the process's own list is the point. A pool
// belongs to the directory it serves -- the data directory, the continuation
// store's, a provider's work directory -- and a `gc` process opens almost none
// of them, so a command that emptied only what it had opened would report a
// collection and leave the largest pools untouched.
func gcArenas(dataDir string) []*scratch.Arena {
	seen := map[string]*scratch.Arena{}
	add := func(a *scratch.Arena) {
		if _, ok := seen[a.Root()]; !ok {
			seen[a.Root()] = a
		}
	}
	for _, a := range scratch.All() {
		add(a)
	}
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if scratch.Dir(filepath.Dir(path)) == path {
			add(scratch.For(filepath.Dir(path)))
			return fs.SkipDir
		}
		return nil
	})
	out := make([]*scratch.Arena, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	return out
}

// writeGCReport prints what was held and what went, pool by pool.
func writeGCReport(w io.Writer, r model.ScratchCollection) error {
	var b strings.Builder
	if len(r.Pools) == 0 {
		b.WriteString("no scratch pools\n")
		return writeText(w, "%s", b.String())
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, p := range r.Pools {
		fmt.Fprintf(tw, "%s\t%d bytes held\t%d bytes freed\n", p.Directory, p.HeldBytes, p.FreedBytes)
		for _, purpose := range slices.Sorted(maps.Keys(p.HeldByPurpose)) {
			fmt.Fprintf(tw, "  %s\t%d bytes\t\n", purpose, p.HeldByPurpose[purpose])
		}
		// A removal the filesystem refused is why freed can fall short of
		// held; unnamed it would read as this command having quietly done
		// less than it said.
		for _, stuck := range p.StuckFrees {
			fmt.Fprintf(tw, "  could not free %s\t%s\t\n", stuck.Entry, stuck.Reason)
		}
	}
	fmt.Fprintf(tw, "total\t%d bytes held\t%d bytes freed\n", r.HeldBytes, r.FreedBytes)
	flushTableInto(tw)
	b.WriteString("\n" + gcNotice + "\n")
	return writeText(w, "%s", b.String())
}
