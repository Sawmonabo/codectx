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
			"--operation picks the typed symbol operation. `resolve` (the default), " +
			"`workspace-symbols` and `definition` take the argument as a name or a " +
			"canonical node id; `document-symbols` lists one file, so its argument " +
			"is a canonical FILE id rather than a name.\n\n" +
			"--semantic-source picks where the answer comes from. `canonical` (the " +
			"default) reads sealed facts only: nothing is read from a dirty " +
			"worktree and nothing is built. `lsp` answers from a managed language " +
			"server named by --profile; those rows are labeled overlay results " +
			"bound to their exact input, are never persisted as canonical facts, " +
			"and never grant coverage credit.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			page, generation, timeout, err := queryFlagValues(cmd)
			if err != nil {
				return err
			}
			operation, err := symbolOperationValue(cmd)
			if err != nil {
				return err
			}
			source, profile, err := semanticFlagValues(cmd, false)
			if err != nil {
				return err
			}
			// Both selectors are carried explicitly because Validate rejects an
			// empty either way, and a zero value would be a default nobody
			// wrote down. Their flag defaults are the pre-Task-17 request, so a
			// bare `codectx symbol NAME` builds exactly the request it always
			// did.
			req := model.SymbolRequest{
				GenerationID:   generation,
				Operation:      operation,
				SemanticSource: source,
				Profile:        profile,
				Page:           page,
			}
			// document-symbols lists ONE file and carries no query, so the
			// single positional argument is that file's canonical id. Every
			// other operation reads the argument as the name-or-id the Use line
			// promises. Validate enforces the shape of whichever was filled.
			if operation == model.SymbolDocumentSymbols {
				req.FileID = model.FileID(args[0])
			} else {
				req.Query = args[0]
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
			// Ruling Q13 keeps the canonical query commands on the path they
			// already have -- Task 18 re-points them at the facade -- so the
			// default route is byte-for-byte the one Task 13 built. Only the
			// overlay route goes through Services(), which is the consumer
			// ruling Q14 says drives Symbol for the LSP source.
			//
			// Typed for the reason `search` gives: a query deadline must not
			// reach the operator as an invalid command line.
			var result model.Page[model.Node]
			if source == model.SemanticLSP {
				result, err = ws.Services().Symbol(ctx, req)
			} else {
				result, err = ws.Search().Resolve(ctx, req)
			}
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
	cmd.Flags().String(symbolOperationFlag, string(model.SymbolResolve),
		"typed symbol operation: resolve, document-symbols, workspace-symbols or definition"+
			" (document-symbols takes a canonical file id as its argument)")
	addSemanticFlags(cmd, false)
	return cmd
}

// Flag spellings for the Section 18.1 live-query selectors. They are declared
// here, beside the command that owns every one of them, and shared by query.go:
// flags.go holds the flags EVERY query command has, and refs, callers and
// callees take a strict subset of these four.
const (
	semanticSourceFlag  = "semantic-source"
	semanticProfileFlag = "profile"
	symbolOperationFlag = "operation"
	referenceKindFlag   = "kind"
)

// addSemanticFlags declares --semantic-source and --profile. shared by query.go
//
// canonicalOnly says whether this command can actually answer from the overlay.
// The call-hierarchy commands cannot: model.GraphRequest carries no semantic
// source and ExploreService.Graph is canonical-only, so `--semantic-source lsp`
// there is refused by semanticFlagValues. The flag is still declared, because
// Section 18.1 gives the call-hierarchy commands the selector and because a
// command that silently answered a canonical result to an `lsp` request would
// be the substituted answer Section 11.6 forbids -- but the help text has to
// say so rather than advertise a route the command does not have.
func addSemanticFlags(cmd *cobra.Command, canonicalOnly bool) {
	source := "answer from the sealed canonical index (canonical, the default) or from the managed language server named by --profile (lsp)"
	profile := "managed language server to answer from; required with --" + semanticSourceFlag + " lsp and meaningless without it"
	if canonicalOnly {
		source = "answer from the sealed canonical index; this command has only the canonical route, and `lsp` is refused rather than answered from canonical facts"
		profile = "managed language server to answer from; this command has no overlay route, so naming one is refused"
	}
	cmd.Flags().String(semanticSourceFlag, string(model.SemanticCanonical), source)
	cmd.Flags().String(semanticProfileFlag, "", profile)
}

// semanticFlagValues reads and screens the two selectors before any workspace
// is opened, for the reason `search` gives: a rejection that first opened the
// database has already paid for work it will not do. shared by query.go
//
// An unknown source is the caller's typo. A `lsp` request with no profile is
// rejected here rather than passed on, because nothing downstream may guess
// which of the six supported servers was meant -- a root marker is a hint, not
// a choice (Section 11.5) -- and a profile named beside `canonical` is a
// command line that would silently do nothing. The profile name itself is NOT
// checked against the supported set here: that list belongs to
// internal/provider/lsp, which the facade exists to keep out of the command
// tree, and SymbolRequest.Validate already bounds the string.
func semanticFlagValues(cmd *cobra.Command, canonicalOnly bool) (model.SemanticSource, string, error) {
	raw, err := stringFlag(cmd, semanticSourceFlag)
	if err != nil {
		return "", "", err
	}
	source := model.SemanticSource(raw)
	if !source.Valid() {
		return "", "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s %q is not a known semantic source", semanticSourceFlag, clip(raw, 64))}).
			WithRemediation("pass --" + semanticSourceFlag + " " + string(model.SemanticCanonical) +
				" or --" + semanticSourceFlag + " " + string(model.SemanticLSP))
	}
	profile, err := stringFlag(cmd, semanticProfileFlag)
	if err != nil {
		return "", "", err
	}
	if canonicalOnly && source == model.SemanticLSP {
		// Section 11.6's explicit unavailable answer, not a silently
		// substituted canonical one. CTX_PROVIDER_UNAVAILABLE is the provider
		// exit class (5), which is where "this route cannot run" belongs; it is
		// not the exit-2 class, because the command line is well formed and the
		// capability is what is missing.
		return "", "", (&model.Error{Code: model.CodeProviderUnavailable,
			Message: "the call hierarchy is answered from canonical facts only; there is no " +
				string(model.SemanticLSP) + " route for this command"}).
			WithDetail("semantic_source", string(model.SemanticLSP)).
			WithRemediation("drop --" + semanticSourceFlag + ", or ask the overlay for symbols or references instead")
	}
	if source == model.SemanticLSP && profile == "" {
		return "", "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "--" + semanticSourceFlag + " " + string(model.SemanticLSP) +
				" needs --" + semanticProfileFlag + ": the language server to answer from is never guessed"}).
			WithRemediation("name one of the supported servers, for example --" + semanticProfileFlag + " gopls")
	}
	if source == model.SemanticCanonical && profile != "" {
		// The remediation has to differ on the canonical-only commands:
		// telling their caller to add `--semantic-source lsp` would send them
		// straight into the refusal above, which is advice that cannot be
		// followed.
		remediation := "add --" + semanticSourceFlag + " " + string(model.SemanticLSP) +
			", or drop --" + semanticProfileFlag
		if canonicalOnly {
			remediation = "drop --" + semanticProfileFlag + ": this command has no overlay route to name a server for"
		}
		return "", "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "--" + semanticProfileFlag + " names a language server, which only the " +
				string(model.SemanticLSP) + " semantic source answers from"}).
			WithRemediation(remediation)
	}
	return source, profile, nil
}

// symbolOperationValue reads --operation. The enum is the model's, so an
// unknown value is rejected with the same vocabulary the request would use.
func symbolOperationValue(cmd *cobra.Command) (model.SymbolOperation, error) {
	raw, err := stringFlag(cmd, symbolOperationFlag)
	if err != nil {
		return "", err
	}
	op := model.SymbolOperation(raw)
	if !op.Valid() {
		return "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s %q is not a known symbol operation", symbolOperationFlag, clip(raw, 64))}).
			WithRemediation("pass one of: " + string(model.SymbolResolve) + ", " +
				string(model.SymbolDocumentSymbols) + ", " + string(model.SymbolWorkspaceSymbols) +
				", " + string(model.SymbolDefinition))
	}
	return op, nil
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
