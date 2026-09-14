package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// shortIDWidth is how much of a canonical identifier the human table shows. The
// full identifier is in the --json envelope; a table that printed seven
// sixty-four-character hex columns would be unreadable, and the prefix is
// enough to tell two candidates apart, which is what this command is for.
const shortIDWidth = 12

// newSymbolCommand builds `codectx symbol`.
func newSymbolCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "symbol <name-or-id>",
		Short: "Resolve a name or canonical ID to the nodes it names",
		Long: "Resolves a symbol name, qualified name or canonical node ID to the " +
			"nodes one generation of the index holds for it. An ambiguous name " +
			"returns every candidate rather than silently picking the first, so " +
			"the caller decides which one it meant.\n\n" +
			"One generation is pinned for the whole request: --generation reads a " +
			"named one, and without it the active generation is read. A result page " +
			"reports a continuation token; pass it back with --cursor to read the " +
			"next page from the generation the first page was read from, which is " +
			"why --cursor and --generation cannot be combined.\n\n" +
			"Only sealed canonical facts are resolved here; nothing is read from a " +
			"dirty worktree and nothing is built.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			page, generation, timeout, err := queryFlagValues(cmd)
			if err != nil {
				return err
			}
			// Section 18.1 gives this command one spelling and no operation
			// flag, so it is the resolve operation over canonical facts. Both
			// are set explicitly because Validate rejects an empty either way,
			// and a zero value would be a default nobody wrote down.
			req := model.SymbolRequest{
				GenerationID:   generation,
				Query:          args[0],
				Operation:      model.SymbolResolve,
				SemanticSource: model.SemanticCanonical,
				Page:           page,
			}
			// Rejected before a workspace is opened, for the reason `search`
			// gives: Section 14.1 bounds the request before any work.
			if err := req.Validate(); err != nil {
				return err
			}
			repo, err := repoFlagValue(cmd)
			if err != nil {
				return err
			}
			ws, err := app.OpenWorkspaceForReport(cmd.Context(), repo)
			if err != nil {
				return err
			}
			defer ws.Close()
			ctx, cancel := queryContext(cmd.Context(), timeout)
			defer cancel()
			// Typed for the reason `search` gives: a query deadline must not
			// reach the operator as an invalid command line.
			result, err := ws.Search().Resolve(ctx, req)
			if err != nil {
				return queryFailure(err)
			}
			out := cmd.OutOrStdout()
			if jsonRequested(cmd, args) {
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), result))
			}
			return writeSymbolTable(out, result)
		},
	}
	addQueryFlags(cmd, true)
	// The Section 18.1 spelling is `codectx symbol <name-or-id> [--limit N]
	// [--cursor TOKEN]`, and the request carries a PageRequest that
	// queryFlagValues reads, so the page flags have to be declared here too.
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	return cmd
}

// writeSymbolTable renders the human page. The candidate columns are the ones
// that separate an ambiguous name's candidates from one another -- which node,
// in which file, at which line -- and the page metadata follows for the reason
// writeSearchTable gives.
func writeSymbolTable(w io.Writer, page model.Page[model.Node]) error {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tKIND\tLANGUAGE\tFILE\tLINE\tNAME\tQUALIFIED NAME")
	for _, node := range page.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(string(node.ID)), tableCell(string(node.Kind)), tableCell(node.Language),
			shortID(string(node.FileID)), rangeLine(node.Range),
			tableCell(node.Name), tableCell(node.QualifiedName))
	}
	if err := flushTable(tw); err != nil {
		return err
	}
	fmt.Fprintf(&b, "\n%d %s\n", len(page.Items), plural(len(page.Items), "candidate", "candidates"))
	writeQueryMeta(&b, page.Meta)
	return writeText(w, "%s", b.String())
}

// shortID renders a canonical identifier's prefix. An absent identifier -- an
// overlay node carries none -- renders as "-" rather than as an empty column.
func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= shortIDWidth {
		return tableCell(id)
	}
	return tableCell(id[:shortIDWidth]) + "..."
}
