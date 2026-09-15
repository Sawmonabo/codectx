package cli

import (
	"context"
	"fmt"
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
	searchKindFlag     = "kind"
	searchLanguageFlag = "language"
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
			page, generation, err := queryFlagValues(cmd)
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
			// One call path: the facade's Search. runService owns the open, the
			// close, the --timeout deadline and the typing of a bare context
			// failure, so nothing here repeats them.
			var result model.Page[model.SearchHit]
			if err := runService(cmd, openForReport(),
				func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
					var err error
					result, err = svc.Search(ctx, req)
					return err
				}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				writeSearchTable(b, result.Items)
			})
		},
	}
	addQueryFlags(cmd, true)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	cmd.Flags().StringArray(searchKindFlag, nil, "keep only hits of this node kind; repeat the flag for more than one")
	cmd.Flags().StringArray(searchLanguageFlag, nil, "keep only hits in this language; repeat the flag for more than one")
	return cmd
}

// queryFlagValues reads the request-shaped flags the two Section 18.1 query
// commands share. The request's own Validate bounds the limit, the cursor and
// the generation, so nothing is re-checked here.
//
// --timeout is not read here at all: runService applies it, and durationFlag
// refuses a negative one for every duration flag in the tree, so a second read
// here would be the same command line checked twice.
func queryFlagValues(cmd *cobra.Command) (model.PageRequest, model.GenerationID, error) {
	limit, err := intFlag(cmd, queryLimitFlag)
	if err != nil {
		return model.PageRequest{}, 0, err
	}
	cursor, err := stringFlag(cmd, queryCursorFlag)
	if err != nil {
		return model.PageRequest{}, 0, err
	}
	generation, err := int64Flag(cmd, queryGenerationFlag)
	if err != nil {
		return model.PageRequest{}, 0, err
	}
	return model.PageRequest{Limit: limit, Cursor: cursor}, model.GenerationID(generation), nil
}

// queryContext applies an operator-supplied deadline to the service call alone.
// The workspace open is deliberately outside it: a slow open is not a query
// that ran out of time, and reporting it as one would name the wrong cause.
//
// A zero `--timeout` installs NO deadline and leaves the configured
// resources.query_timeout in charge, which itself defaults to unlimited: the
// command returns the complete answer unless the operator asked for a bound.
// context.WithTimeout is never called with zero, which would expire the call
// immediately -- the opposite of what "no limit" means.
func queryContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// writeSearchTable renders the hits a page admitted, in the order the service
// ranked them: the tier orders the groups and the integer score orders the hits
// within one, so re-sorting here would show an order the --json consumer never
// sees. Every hit the page carries is printed -- the page is already bounded by
// the request's limit, and the continuation token, the truncation reason and
// the capability notes are rendered around this block by emitQuery, so the
// operator is never handed a short answer with nothing saying it was short.
func writeSearchTable(b *strings.Builder, hits []model.SearchHit) {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tSCORE\tOCC\tKIND\tLINE\tPATH\tNAME\tREASON")
	for _, hit := range hits {
		name := hit.QualifiedName
		if name == "" {
			name = hit.Name
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			tableCell(string(hit.Tier)), hit.ScoreMicros, hit.OccurrenceCount,
			tableCell(string(hit.Kind)), hitRangeCell(hit), tableCell(hit.Path),
			tableCell(name), reasonCell(hit))
	}
	flushTableInto(tw)
	fmt.Fprintf(b, "\n%d %s\n", len(hits), plural(len(hits), "hit", "hits"))
}

// flushTableInto flushes a table whose sink is a strings.Builder, which is what
// every renderResult block emitQuery calls gets. The error is dropped
// deliberately: Builder.Write never returns one, so returning it would put an
// unreachable branch in each caller. A table written to a file or a pipe -- the
// envelope on stdout, for one -- must not use this.
func flushTableInto(tw *tabwriter.Writer) {
	_ = tw.Flush()
}

// reasonCell renders a hit's bounded explanation. The exact and prefix tiers
// report ScoreMicros 0 and say why in a reason, so a table without this column
// would show those hits as scoring nothing with nothing to explain it. Only the
// first reason fits a column; the rest are counted, and --json carries them all.
// It also carries the per-hit fault the JSON answer reports in
// `unresolved_fields`: a hit whose range the content store could not supply
// stays in the answer, so the reason it lost its position must reach an
// operator reading the table and not only one reading --json.
func reasonCell(hit model.SearchHit) string {
	reasons := hit.Reasons
	if why := hit.UnresolvedFields[model.SearchHitFieldRange]; why != "" {
		// A fresh slice: the hit's own Reasons must not grow a rendering
		// artifact that a later --json of the same page would then publish.
		reasons = append([]string{"range unresolved: " + why}, reasons...)
	}
	if len(reasons) == 0 {
		return "-"
	}
	cell := tableCell(reasons[0])
	if extra := len(reasons) - 1; extra > 0 {
		cell += fmt.Sprintf(" (+%d more)", extra)
	}
	return cell
}

// hitRangeCell renders a search hit's LINE column. It separates the two ways a
// hit can reach the table with no position: an entity that simply has no source
// location renders "-", while a hit whose range the content store could not
// supply renders "!" and states why in the REASON column. Rendering both as "-"
// would present a lost location as "this hit has none", which is the silent
// degradation the per-hit fault flag exists to prevent.
func hitRangeCell(hit model.SearchHit) string {
	if hit.Range == nil {
		if _, unresolved := hit.UnresolvedFields[model.SearchHitFieldRange]; unresolved {
			return "!"
		}
	}
	return rangeLine(hit.Range)
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

func int64Flag(cmd *cobra.Command, name string) (int64, error) {
	v, err := cmd.Flags().GetInt64(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
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
