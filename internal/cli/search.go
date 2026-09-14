package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// Flag names shared by the two Section 18.1 query commands. `--repo` is not
// here because it already has one spelling, declared by addRepoFlag, and a
// second would be two contracts for one question.
const (
	queryGenerationFlag = "generation"
	queryLimitFlag      = "limit"
	queryCursorFlag     = "cursor"
	queryTimeoutFlag    = "timeout"
	searchKindFlag      = "kind"
	searchLanguageFlag  = "language"
)

// tableCellWidth bounds one human-table column. The values are indexed from the
// repository, so a single minified line or a pathological identifier would
// otherwise render a table wider than any terminal; --json carries them whole.
const tableCellWidth = 64

// newSearchCommand builds `codectx search`.
func newSearchCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find symbols, paths and lexical matches in one generation",
		Long: "Searches one generation of the index: exact path and symbol lookup, " +
			"qualified-name prefix lookup, and generation-local lexical retrieval. " +
			"The most specific retrieval tier ranks first, and within a tier the " +
			"integer score orders the hits, so the same query over the same " +
			"generation always returns the same page.\n\n" +
			"One generation is pinned for the whole request: --generation reads a " +
			"named one, and without it the active generation is read. A result page " +
			"reports a continuation token; pass it back with --cursor to read the " +
			"next page from the generation the first page was read from, which is " +
			"why --cursor and --generation cannot be combined.\n\n" +
			"Hits carry the matched entity, its path and its ranking, never source " +
			"bodies: read those with the source commands.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := model.SearchRequest{Query: args[0]}
			kinds, err := stringsFlag(cmd, searchKindFlag)
			if err != nil {
				return err
			}
			for _, k := range kinds {
				req.Kinds = append(req.Kinds, model.NodeKind(k))
			}
			if req.Languages, err = stringsFlag(cmd, searchLanguageFlag); err != nil {
				return err
			}
			page, generation, timeout, err := queryFlagValues(cmd)
			if err != nil {
				return err
			}
			req.Page, req.GenerationID = page, generation
			// The whole request -- query text, filters, limit and the
			// cursor/generation pairing -- is rejected before a workspace is
			// opened: Section 14.1 bounds the request before any work, and an
			// open that only precedes a rejection is work nobody asked for.
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
			result, err := ws.Search().Search(ctx, req)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonRequested(cmd, args) {
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), result))
			}
			return writeSearchTable(out, result)
		},
	}
	addQueryFlags(cmd)
	cmd.Flags().StringArray(searchKindFlag, nil, "keep only hits of this node kind; repeat the flag for more than one")
	cmd.Flags().StringArray(searchLanguageFlag, nil, "keep only hits in this language; repeat the flag for more than one")
	return cmd
}

// addQueryFlags declares the flags both Section 18.1 query commands take.
func addQueryFlags(cmd *cobra.Command) {
	addRepoFlag(cmd)
	cmd.Flags().Int64(queryGenerationFlag, 0,
		"generation to read; 0 reads the active generation, and a cursor already pins its own")
	cmd.Flags().Int(queryLimitFlag, 0,
		fmt.Sprintf("most results in one page; 0 uses the endpoint default and %d is the ceiling", model.MaxPageItems))
	cmd.Flags().String(queryCursorFlag, "",
		"continuation token from an earlier page; it pins that page's generation, so --generation is refused with it")
	cmd.Flags().Duration(queryTimeoutFlag, 0,
		"deadline for this query; 0 uses the configured resources.query_timeout")
}

// queryFlagValues reads the shared flags. The request's own Validate bounds the
// limit, the cursor and the generation, so nothing is re-checked here; the
// timeout is the exception, because it is a process-side deadline that never
// reaches a request field and so has no validator of its own.
func queryFlagValues(cmd *cobra.Command) (model.PageRequest, model.GenerationID, time.Duration, error) {
	limit, err := intFlag(cmd, queryLimitFlag)
	if err != nil {
		return model.PageRequest{}, 0, 0, err
	}
	cursor, err := stringFlag(cmd, queryCursorFlag)
	if err != nil {
		return model.PageRequest{}, 0, 0, err
	}
	generation, err := int64Flag(cmd, queryGenerationFlag)
	if err != nil {
		return model.PageRequest{}, 0, 0, err
	}
	timeout, err := durationFlag(cmd, queryTimeoutFlag)
	if err != nil {
		return model.PageRequest{}, 0, 0, err
	}
	if timeout < 0 {
		return model.PageRequest{}, 0, 0, &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s is %s; it must not be negative, and 0 uses the configured query timeout", queryTimeoutFlag, timeout)}
	}
	return model.PageRequest{Limit: limit, Cursor: cursor}, model.GenerationID(generation), timeout, nil
}

// queryContext applies an operator-supplied deadline to the service call alone.
// The workspace open is deliberately outside it: a slow open is not a query
// that ran out of time, and reporting it as one would name the wrong cause. A
// zero timeout leaves the context alone so resources.query_timeout applies.
func queryContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// writeSearchTable renders the human page: the hits, then the page's own
// metadata. The continuation token and the truncation reason are carried in
// QueryMeta, which --json emits whole; a table that printed only the rows would
// silently drop both, leaving the operator with a short answer and no way to
// tell it was short or to ask for the rest.
func writeSearchTable(w io.Writer, page model.Page[model.SearchHit]) error {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tSCORE\tOCC\tKIND\tLINE\tPATH\tNAME\tREASON")
	for _, hit := range page.Items {
		name := hit.QualifiedName
		if name == "" {
			name = hit.Name
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			tableCell(string(hit.Tier)), hit.ScoreMicros, hit.OccurrenceCount,
			tableCell(string(hit.Kind)), rangeLine(hit.Range), tableCell(hit.Path),
			tableCell(name), reasonCell(hit.Reasons))
	}
	if err := flushTable(tw); err != nil {
		return err
	}
	fmt.Fprintf(&b, "\n%d %s\n", len(page.Items), plural(len(page.Items), "hit", "hits"))
	writeQueryMeta(&b, page.Meta)
	return writeText(w, "%s", b.String())
}

// writeQueryMeta renders the parts of a page's metadata a human table would
// otherwise lose: the generation the answer was read from, the continuation
// token, and the truncation the service reported.
func writeQueryMeta(b *strings.Builder, meta model.QueryMeta) {
	fmt.Fprintf(b, "generation  %d\n", meta.Binding.GenerationID)
	writeCapabilities(b, meta.Completeness)
	if meta.Truncated {
		fmt.Fprintf(b, "warning     result truncated: %s\n", tableCell(meta.TruncationReason))
	}
	if meta.NextCursor != "" {
		fmt.Fprintf(b, "next        --cursor %s\n", meta.NextCursor)
	}
}

// reasonCell renders a hit's bounded explanation. The exact and prefix tiers
// report ScoreMicros 0 and say why in a reason, so a table without this column
// would show those hits as scoring nothing with nothing to explain it. Only the
// first reason fits a column; the rest are counted, and --json carries them all.
func reasonCell(reasons []string) string {
	if len(reasons) == 0 {
		return "-"
	}
	cell := tableCell(reasons[0])
	if extra := len(reasons) - 1; extra > 0 {
		cell += fmt.Sprintf(" (+%d more)", extra)
	}
	return cell
}

// rangeLine renders a hit's one-based start line, or "-" for an entity with no
// source location. An absent range is not rendered as line 0, which would read
// as a real position.
func rangeLine(r *model.SourceRange) string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("%d", r.Start.Line)
}

// tableCell bounds and sanitizes one column value. Unlike the toolchain report,
// these strings are identifiers, paths and signatures read out of the indexed
// repository: Validate bounds their length but not their content, so a control
// character or a newline in a source identifier would otherwise break the table
// apart or rewrite the terminal around it.
func tableCell(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	width := 0
	for _, r := range s {
		if width == tableCellWidth {
			b.WriteString("...")
			break
		}
		switch {
		case r == utf8.RuneError, !unicode.IsPrint(r):
			b.WriteRune('.')
		default:
			b.WriteRune(r)
		}
		width++
	}
	return b.String()
}

func flushTable(tw *tabwriter.Writer) error {
	if err := tw.Flush(); err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "failed to render output: " + err.Error()}
	}
	return nil
}

func intFlag(cmd *cobra.Command, name string) (int, error) {
	v, err := cmd.Flags().GetInt(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

func int64Flag(cmd *cobra.Command, name string) (int64, error) {
	v, err := cmd.Flags().GetInt64(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

func stringFlag(cmd *cobra.Command, name string) (string, error) {
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		return "", &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

func stringsFlag(cmd *cobra.Command, name string) ([]string, error) {
	v, err := cmd.Flags().GetStringArray(name)
	if err != nil {
		return nil, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

func durationFlag(cmd *cobra.Command, name string) (time.Duration, error) {
	v, err := cmd.Flags().GetDuration(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}
