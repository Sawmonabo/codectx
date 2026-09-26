package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// newRepoMapCommand builds `codectx repo-map`.
//
// Unlike `doctor`, this IS a runService caller, and the asymmetry is the point:
// a workspace that cannot be opened has no repository map, so the open failure
// must fail the command. Answering it with an empty page would render an
// unopenable workspace as a repository that contains no packages, which is the
// one thing a map must never say.
func newRepoMapCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo-map [path]",
		Short: "Report the bounded repository, package, module and language map",
		Long: "Reports the containers of the pinned generation -- repository, modules, " +
			"packages and directories -- with the file, symbol and byte totals aggregated " +
			"under each one. Nothing is materialized whole: the answer is a bounded page " +
			"of container metadata, and a repository larger than one page is continued " +
			"through the token the answer prints rather than truncated.\n\n" +
			"--depth bounds how far down the container tree the map goes. The repository " +
			"is named as a positional path, or with --repo, and naming it both ways is " +
			"refused rather than resolved to one of them.\n\n" +
			"A continuation is the whole question again: pass --cursor TOKEN alongside the " +
			"same --depth and --limit the first page was asked with. A token is bound to " +
			"the request that minted it, so a page asked with different ones is refused.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoTarget(cmd, args)
			if err != nil {
				return err
			}
			depth, err := intFlag(cmd, queryDepthFlag)
			if err != nil {
				return err
			}
			// The shared reader for the three flags every paged command
			// declares. The request's own Validate bounds all three, including
			// the Section 14.1 refusal of a cursor beside a generation, so
			// nothing is re-checked here.
			page, gen, err := queryFlagValues(cmd)
			if err != nil {
				return err
			}
			req := model.OverviewRequest{GenerationID: gen, Depth: depth, Page: page}
			var result model.Page[model.OverviewItem]
			if err := runService(cmd, repoMapOpener(repo),
				func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
					result, err = svc.Overview(ctx, req)
					return err
				}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				writeOverviewTable(b, result.Items)
			})
		},
	}
	addQueryFlags(cmd, true)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	cmd.Flags().Int(queryDepthFlag, 0, "container depth to report, counted from the repository root"+zeroBoundHelp)
	return cmd
}

// repoMapOpener is the read-only opener, closed over the repository this
// invocation resolved. The runner's own repo argument is ignored because it is
// the --repo flag alone, and the Section 18.1 spelling of this command also
// accepts the repository as a positional path; resolving that in one place and
// closing over the answer keeps runService the single runner without writing a
// value back into a flag the operator never typed.
func repoMapOpener(repo string) opener {
	return func(ctx context.Context, _ string) (*app.Workspace, error) {
		return app.OpenWorkspaceForQuery(ctx, repo)
	}
}

// repoTarget resolves which repository the command is about, for the two
// Section 18.1 commands spelled positionally -- `repo-map [path]` and
// `doctor [path]`. Every other workspace command spells the same question
// --repo, so both spellings are accepted here. Naming both is refused
// instead of resolved: either precedence silently ignores something the
// operator typed, and the one they meant is the one that gets ignored half the
// time.
func repoTarget(cmd *cobra.Command, args []string) (string, error) {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		return repo, nil
	}
	if cmd.Flags().Changed(toolsRepoFlag) {
		return "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "the repository is named twice: once as a positional path and once with --" + toolsRepoFlag}).
			WithRemediation("Give the path either way, not both.")
	}
	return args[0], nil
}

// writeOverviewTable renders one page of the map. Paths, names and languages
// are read out of the indexed repository, so each passes through tableCell: a
// container named with a control character would otherwise break the table
// apart or rewrite the terminal around it.
//
// Every item the page carries is printed. The page is already bounded by the
// request's limit, and emitQuery renders the continuation token, the truncation
// reason and the capability notes around this block, so a short answer is never
// handed over with nothing saying it was short.
func writeOverviewTable(b *strings.Builder, items []model.OverviewItem) {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEPTH\tKIND\tPATH\tNAME\tLANGUAGE\tFILES\tSYMBOLS\tBYTES")
	var files, symbols, sourceBytes int64
	for _, item := range items {
		files += item.FileCount
		symbols += item.SymbolCount
		sourceBytes += item.SourceBytes
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%d\t%d\n", item.Depth, tableCell(string(item.Kind)),
			tableCell(item.Path), tableCell(item.Name), tableCell(languageCell(item.Language)),
			item.FileCount, item.SymbolCount, item.SourceBytes)
	}
	flushTableInto(tw)
	// The totals are of this page, and say so: they are sums of what was
	// printed, not a repository-wide aggregate nothing here computed.
	fmt.Fprintf(b, "\n%d %s on this page: %d %s, %d %s, %d bytes\n",
		len(items), plural(len(items), "container", "containers"),
		files, plural(int(files), "file", "files"),
		symbols, plural(int(symbols), "symbol", "symbols"), sourceBytes)
}

// languageCell renders a container that carries no language. A repository or a
// directory node has none, and an empty column would read as a language whose
// name failed to render.
func languageCell(language string) string {
	if language == "" {
		return "-"
	}
	return language
}
