package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// Flag spellings for the Section 18.1 query commands. Each command declares
// only the flags its own request type has a field for: `path` carries no page
// and no edge budget, `refs` carries no traversal budget, so offering those
// flags there would be help text that describes a request the command cannot
// build.
const (
	queryDepthFlag      = "depth"
	queryLimitFlag      = "limit"
	queryCursorFlag     = "cursor"
	queryGenerationFlag = "generation"
	queryTimeoutFlag    = "timeout"
	queryVisitedFlag    = "visited"
	queryEdgesFlag      = "edges"
)

// zeroBoundHelp is the one sentence every budget flag ends with. A zero here is
// not "unlimited": the engine resolves it to the configured default, so the
// flag's default must stay 0 rather than repeating a config value the operator
// may have changed (Section 20.1).
const zeroBoundHelp = " (0 uses the configured default)"

// nameArgumentHelp is what these commands accept today. Ruling Q9 resolves a
// name through PinnedReader.Nodes, which is storage rather than search, but no
// accessor reaches a PinnedReader from here: Workspace.Query hands out a
// *graph.Engine, whose every entry point takes an already-resolved NodeID. So a
// name is rejected with the typed argument error rather than silently resolved
// to the first candidate.
const nameArgumentHelp = "Arguments are resolved node ids: 64 lowercase hex characters, as reported by a query that returned the node. A name is rejected rather than guessed at."

// newQueryCommands builds the Section 18.1 query commands. They are returned as
// a slice so the root registers them in one loop and this file never edits the
// command tree it belongs to.
func newQueryCommands(build model.BuildInfo) []*cobra.Command {
	return []*cobra.Command{
		newRefsCommand(build),
		newCallersCommand(build),
		newCalleesCommand(build),
		newPathCommand(build),
		newImpactCommand(build),
	}
}

// newRefsCommand builds `codectx refs`.
func newRefsCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refs <node-id>",
		Short: "List the canonical reference occurrences of one node",
		Long: "Lists the sealed reference occurrences of one node in the pinned generation. " +
			"Section 9.2 separates the two counts this answer carries: several occurrences may " +
			"share one canonical relation, so the summary reports occurrences and distinct " +
			"relations separately and never collapses them.\n\n" +
			"Only canonical facts are answered. There is no flag for the editor overlay and none " +
			"for the implements or type-definition variants; those reach the same engine through " +
			"the MCP surface.\n\n" + nameArgumentHelp,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			nodes, err := startNodes(args)
			if err != nil {
				return err
			}
			page, err := pageRequest(cmd)
			if err != nil {
				return err
			}
			gen, err := generationFlag(cmd)
			if err != nil {
				return err
			}
			req := model.ReferenceRequest{
				GenerationID:   gen,
				NodeID:         nodes[0],
				Operation:      model.ReferenceReferences,
				SemanticSource: model.SemanticCanonical,
				Page:           page,
			}
			// The request is validated before a workspace is opened: a
			// rejection that first opened the database has already paid for
			// work it will not do.
			if err := req.Validate(); err != nil {
				return err
			}
			var result model.Page[model.ReferenceOccurrence]
			if err := runQuery(cmd, gen, func(ctx context.Context, engine *graph.Engine) error {
				result, err = engine.References(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				writeOccurrences(b, result.Items)
			})
		},
	}
	addQueryFlags(cmd)
	addPageFlags(cmd)
	return cmd
}

// newCallersCommand builds `codectx callers`.
func newCallersCommand(build model.BuildInfo) *cobra.Command {
	return newCallCommand(build, "callers", model.DirectionIncoming,
		"Expand the incoming call edges of the given nodes",
		"Walks the `calls` relation inward from each seed, so the answer is who reaches these "+
			"symbols. The direction and the relation are both pinned by the command; there is no "+
			"flag that can contradict them.")
}

// newCalleesCommand builds `codectx callees`.
func newCalleesCommand(build model.BuildInfo) *cobra.Command {
	return newCallCommand(build, "callees", model.DirectionOutgoing,
		"Expand the outgoing call edges of the given nodes",
		"Walks the `calls` relation outward from each seed, so the answer is what these symbols "+
			"reach. The direction and the relation are both pinned by the command; there is no "+
			"flag that can contradict them.")
}

// newCallCommand builds one of the two call-hierarchy commands. They differ
// only in the direction they pin, so a second copy of the same body would be a
// second place for the traversal contract to drift.
func newCallCommand(build model.BuildInfo, name string, direction model.Direction, short, detail string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name + " <node-id> [node-id...]",
		Short: short,
		Long: detail + "\n\nThe walk is bounded: --depth caps the hop count from the nearest seed, " +
			"--visited and --edges cap the distinct nodes and relations it may admit, and each of " +
			"those budgets is cumulative across the pages of one traversal rather than refilled " +
			"per page. Exhausting any of them, or --timeout, reports the answer as truncated with " +
			"the reason rather than presenting it as exhaustive.\n\n" + nameArgumentHelp,
		Args:          cobra.RangeArgs(1, model.MaxStartNodes),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := graphRequest(cmd, args, direction, []model.RelationKind{model.RelCalls})
			if err != nil {
				return err
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var result model.GraphResult
			if err := runQuery(cmd, req.GenerationID, func(ctx context.Context, engine *graph.Engine) error {
				if direction == model.DirectionIncoming {
					result, err = engine.Callers(ctx, req)
				} else {
					result, err = engine.Callees(ctx, req)
				}
				return err
			}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				fmt.Fprintf(b, "direction   %s\nwalked      %d visited, %d %s, depth %d\n",
					result.Direction, result.VisitedCount, result.EdgeCount,
					plural(int(result.EdgeCount), "edge", "edges"), result.MaxDepth)
				writeNodeTable(b, result.Nodes)
				writeRelationTable(b, result.Relations)
			})
		},
	}
	addQueryFlags(cmd)
	addPageFlags(cmd)
	addTraversalFlags(cmd, true)
	return cmd
}

// newPathCommand builds `codectx path`.
func newPathCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path <from-node-id> <to-node-id>",
		Short: "Find the cheapest dependency routes between two nodes",
		Long: "Searches for the cheapest dependency routes from the first node to the second over " +
			"the default relation allowlist, using fixed integer edge costs so the same two nodes " +
			"in the same generation always produce the same routes in the same order.\n\n" +
			"An exhausted --depth, --visited or --timeout is reported as truncation together with " +
			"whatever routes were found; it is never reported as \"no path exists\". A target that " +
			"is genuinely unreachable returns no routes and is not marked truncated.\n\n" +
			"This command takes no --limit, --cursor or --edges: a route set is bounded by the " +
			"reason-path cap, not paged.\n\n" + nameArgumentHelp,
		Args:          cobra.ExactArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			nodes, err := startNodes(args)
			if err != nil {
				return err
			}
			gen, err := generationFlag(cmd)
			if err != nil {
				return err
			}
			depth, err := intFlag(cmd, queryDepthFlag)
			if err != nil {
				return err
			}
			visited, err := intFlag(cmd, queryVisitedFlag)
			if err != nil {
				return err
			}
			req := model.PathRequest{
				GenerationID: gen,
				From:         nodes[0],
				To:           nodes[1],
				MaxDepth:     depth,
				MaxVisited:   visited,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var result model.PathResult
			if err := runQuery(cmd, gen, func(ctx context.Context, engine *graph.Engine) error {
				result, err = engine.ShortestPath(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				fmt.Fprintf(b, "walked      %d visited\n", result.VisitedCount)
				writePaths(b, result.Paths)
				writeNodeTable(b, result.Nodes)
			})
		},
	}
	addQueryFlags(cmd)
	addTraversalFlags(cmd, false)
	return cmd
}

// newImpactCommand builds `codectx impact`.
func newImpactCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "impact <node-id> [node-id...]",
		Short: "Rank the entities a change to the given nodes may affect",
		Long: "Ranks what a change to the seed nodes may affect, walking both directions because " +
			"the direction is the discriminator the answer reports: an incoming entry may need " +
			"modification, an outgoing entry may need reading. Every entry carries at least one " +
			"reason naming the relation kind and direction that made it affected.\n\n" +
			"The answer also carries the package rollup that summarises it: distinct " +
			"package-to-package pairs with their evidence counts. A rollup pair is a summary, " +
			"never a precise symbol-level call.\n\n" +
			"The same bounds as the call commands apply, and a capability that is still building " +
			"is reported as a warning: the result then states that it is not exhaustive rather " +
			"than claiming complete impact.\n\n" + nameArgumentHelp,
		Args:          cobra.RangeArgs(1, model.MaxStartNodes),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// DirectionBoth is the request the ranking needs: Section 14.3
			// makes the per-entry direction the discriminator, which only a
			// two-way walk can populate.
			base, err := graphRequest(cmd, args, model.DirectionBoth, nil)
			if err != nil {
				return err
			}
			req := model.ImpactRequest{
				GenerationID: base.GenerationID,
				Start:        base.Start,
				Direction:    base.Direction,
				MaxDepth:     base.MaxDepth,
				MaxVisited:   base.MaxVisited,
				MaxEdges:     base.MaxEdges,
				Page:         base.Page,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var result model.ImpactResult
			if err := runQuery(cmd, req.GenerationID, func(ctx context.Context, engine *graph.Engine) error {
				result, err = engine.Impact(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				fmt.Fprintf(b, "walked      %d visited, %d %s\n", result.VisitedCount,
					result.EdgeCount, plural(int(result.EdgeCount), "edge", "edges"))
				writeImpactEntries(b, result.Entries)
				writePackageEdges(b, result.Packages)
			})
		},
	}
	addQueryFlags(cmd)
	addPageFlags(cmd)
	addTraversalFlags(cmd, true)
	return cmd
}

// startNodes reads the positional arguments as resolved node ids. Ruling Q9
// would resolve a name through the pinned reader, which nothing reachable from
// here holds, so a name is rejected with the typed argument error instead of
// being silently resolved to one of several candidates.
func startNodes(args []string) ([]model.NodeID, error) {
	nodes := make([]model.NodeID, 0, len(args))
	for _, arg := range args {
		if !model.ValidHexID(arg) {
			return nil, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: fmt.Sprintf("%q is not a resolved node id; this command takes %d lowercase hex characters",
					clip(arg, 64), model.IDHexLen)}).WithDetail("argument", clip(arg, 64))
		}
		nodes = append(nodes, model.NodeID(arg))
	}
	return nodes, nil
}

// graphRequest builds the traversal request shared by the call commands and
// impact. Direction and the relation allowlist are the command's own, never the
// operator's: Section 4b pins them to the operation.
func graphRequest(cmd *cobra.Command, args []string, direction model.Direction,
	relations []model.RelationKind) (model.GraphRequest, error) {
	nodes, err := startNodes(args)
	if err != nil {
		return model.GraphRequest{}, err
	}
	gen, err := generationFlag(cmd)
	if err != nil {
		return model.GraphRequest{}, err
	}
	page, err := pageRequest(cmd)
	if err != nil {
		return model.GraphRequest{}, err
	}
	depth, err := intFlag(cmd, queryDepthFlag)
	if err != nil {
		return model.GraphRequest{}, err
	}
	visited, err := intFlag(cmd, queryVisitedFlag)
	if err != nil {
		return model.GraphRequest{}, err
	}
	edges, err := intFlag(cmd, queryEdgesFlag)
	if err != nil {
		return model.GraphRequest{}, err
	}
	return model.GraphRequest{
		GenerationID: gen,
		Start:        nodes,
		Relations:    relations,
		Direction:    direction,
		MaxDepth:     depth,
		MaxVisited:   visited,
		MaxEdges:     edges,
		Page:         page,
	}, nil
}

// runQuery opens the workspace in report mode, builds the engine for the pinned
// generation and runs one query against it. The lease the engine holds is
// released on every path, including cancellation, and a release failure never
// masks the failure the query itself reported.
func runQuery(cmd *cobra.Command, gen model.GenerationID, fn func(context.Context, *graph.Engine) error) (err error) {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return err
	}
	timeout, err := durationFlag(cmd, queryTimeoutFlag)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if timeout > 0 {
		// Zero leaves the engine's own configured query deadline in charge; a
		// zero deadline set here would expire before the query started.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ws, err := app.OpenWorkspaceForReport(ctx, repo)
	if err != nil {
		return queryFailure(err)
	}
	defer ws.Close()
	engine, release, err := ws.Query(ctx, gen)
	if err != nil {
		return queryFailure(err)
	}
	defer func() {
		if cerr := release(); cerr != nil && err == nil {
			err = queryFailure(cerr)
		}
	}()
	return queryFailure(fn(ctx, engine))
}

// queryFailure maps a bare context failure onto the Section 22 vocabulary. The
// --timeout deadline is this command's own, so nothing below it can type the
// expiry; left untyped it would reach ExitCode as the exit-2 invalid-argument
// class, reporting a deadline the operator set as a command line they typed
// wrong.
func queryFailure(err error) error {
	if err == nil {
		return nil
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &model.Error{Code: model.CodeQueryDeadline,
			Message: "the query did not finish within --" + queryTimeoutFlag}
	case errors.Is(err, context.Canceled):
		return &model.Error{Code: model.CodeCanceled,
			Message: "the query was canceled before it completed"}
	}
	return err
}

// emitQuery writes the one answer. A --json request emits exactly one envelope
// on stdout and nothing else; a human request gets the bounded block that
// renderResult builds, preceded by the binding and followed by the same
// warnings the envelope carries, so neither consumer learns less than the other.
func emitQuery[T any](cmd *cobra.Command, build model.BuildInfo, args []string, data T,
	meta model.QueryMeta, renderResult func(*strings.Builder)) error {
	out := cmd.OutOrStdout()
	warnings := queryWarnings(meta)
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), data, warnings...))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "generation  %d\nsnapshot    %s\n", meta.Binding.GenerationID, meta.Binding.SnapshotID)
	renderResult(&b)
	writeCapabilities(&b, meta.Completeness)
	for _, warning := range warnings {
		fmt.Fprintf(&b, "warning     %s\n", warning)
	}
	if meta.NextCursor != "" {
		fmt.Fprintf(&b, "next        --%s %s\n", queryCursorFlag, meta.NextCursor)
	}
	return writeText(out, "%s", b.String())
}

// queryWarnings is the bounded set of notes about work the answer does not
// cover: the truncation the engine reported, and every capability it could not
// read. Both reach the operator on both output paths -- an answer that is not
// exhaustive and does not say so is the failure Section 13.3 names.
func queryWarnings(meta model.QueryMeta) []string {
	var warnings []string
	if meta.Truncated {
		reason := meta.TruncationReason
		if reason == "" {
			reason = "a traversal budget was exhausted"
		}
		note := "this answer is not exhaustive: " + reason
		if meta.NextCursor == "" {
			note += "; no continuation is offered"
		}
		warnings = append(warnings, note)
	}
	for _, state := range meta.Completeness {
		if state.State != model.CapabilityUnavailable && state.State != model.CapabilityFailed {
			continue
		}
		note := fmt.Sprintf("%s/%s is %s", state.ProviderID, state.Capability, state.State)
		if reason := state.Details["reason"]; reason != "" {
			note += " (" + reason + ")"
		}
		if units := state.Details["units"]; units != "" {
			note += fmt.Sprintf(", %s %s still building", units, plural(atoiOrZero(units), "unit", "units"))
		}
		warnings = append(warnings, note)
	}
	return warnings
}

// writeNodeTable renders the nodes an answer admitted. The list is already
// bounded by the result contract, so every row is printed; only the per-cell
// width is clipped, which is a display decision and not a dropped fact.
func writeNodeTable(b *strings.Builder, nodes []model.Node) {
	if len(nodes) == 0 {
		b.WriteString("nodes       none\n")
		return
	}
	fmt.Fprintf(b, "nodes       %d\n", len(nodes))
	for _, n := range nodes {
		name := n.QualifiedName
		if name == "" {
			name = n.Name
		}
		fmt.Fprintf(b, "  %s  %-12s %s\n", n.ID, clip(string(n.Kind), 12), clip(name, 80))
	}
}

// writeRelationTable renders the edges an answer admitted, in the keyset order
// the engine returned them.
func writeRelationTable(b *strings.Builder, relations []model.Relation) {
	if len(relations) == 0 {
		b.WriteString("relations   none\n")
		return
	}
	fmt.Fprintf(b, "relations   %d\n", len(relations))
	for _, r := range relations {
		fmt.Fprintf(b, "  %s  %-18s %s -> %s\n", r.ID, clip(string(r.Kind), 18), r.From, r.To)
	}
}

// writeOccurrences renders a reference page, keeping the Section 9.2
// distinction: several occurrences may share one canonical relation, so both
// counts are stated and neither stands in for the other.
func writeOccurrences(b *strings.Builder, items []model.ReferenceOccurrence) {
	distinct := map[model.RelationID]struct{}{}
	for _, o := range items {
		distinct[o.RelationID] = struct{}{}
	}
	fmt.Fprintf(b, "occurrences %d in %d distinct %s\n", len(items), len(distinct),
		plural(len(distinct), "relation", "relations"))
	for _, o := range items {
		location := o.Path
		if o.Range != nil {
			location = fmt.Sprintf("%s:%d", o.Path, o.Range.Start.Line)
		}
		fmt.Fprintf(b, "  %-18s %-10s %s\n", clip(string(o.Kind), 18), clip(string(o.Precision), 10), clip(location, 100))
	}
}

// writePaths renders the routes a path query found, cheapest first, with the
// evidence each route carries.
func writePaths(b *strings.Builder, paths []model.RelationPath) {
	if len(paths) == 0 {
		b.WriteString("paths       none\n")
		return
	}
	fmt.Fprintf(b, "paths       %d\n", len(paths))
	for i, p := range paths {
		fmt.Fprintf(b, "  #%d cost %d, %d %s, %d evidence\n", i+1, p.CostUnits,
			len(p.Relations), plural(len(p.Relations), "edge", "edges"), len(p.Evidence))
		for _, id := range p.Relations {
			fmt.Fprintf(b, "    %s\n", id)
		}
	}
}

// writeImpactEntries renders the ranked entries in the order the engine ranked
// them, each with the direction that made it affected and its reasons.
func writeImpactEntries(b *strings.Builder, entries []model.ImpactEntry) {
	if len(entries) == 0 {
		b.WriteString("entries     none\n")
		return
	}
	fmt.Fprintf(b, "entries     %d\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(b, "  %s  %-9s depth %d  score %d  %s\n", e.NodeID, e.Direction, e.Depth,
			e.ScoreMicros, clip(e.Path, 70))
		for _, reason := range e.Reasons {
			fmt.Fprintf(b, "    %s\n", clip(reason, 100))
		}
	}
}

// writePackageEdges renders the rollup. It is labelled as a summary of pairs
// and evidence counts, because Section 14.3 forbids presenting an aggregated
// pair as a precise symbol-level call.
func writePackageEdges(b *strings.Builder, edges []model.PackageEdge) {
	if len(edges) == 0 {
		return
	}
	fmt.Fprintf(b, "packages    %d aggregated %s (pair counts, not individual calls)\n",
		len(edges), plural(len(edges), "pair", "pairs"))
	for _, e := range edges {
		fmt.Fprintf(b, "  %-40s -> %-40s %d %s, %d evidence\n", clip(e.FromPath, 40), clip(e.ToPath, 40),
			e.PairCount, plural(int(e.PairCount), "pair", "pairs"), e.EvidenceCount)
	}
}

// addQueryFlags declares the flags every query command shares. --repo reuses
// the one spelling the rest of the tree already has.
func addQueryFlags(cmd *cobra.Command) {
	addRepoFlag(cmd)
	cmd.Flags().Int64(queryGenerationFlag, 0, "answer from this generation instead of the active one (0 pins the active generation; not combinable with --cursor)")
	cmd.Flags().Duration(queryTimeoutFlag, 0, "give up after this much wall clock"+zeroBoundHelp)
}

// addPageFlags declares the page flags, for the commands whose request has a
// page. `path` has none: its routes are bounded by the reason-path cap.
func addPageFlags(cmd *cobra.Command) {
	cmd.Flags().Int(queryLimitFlag, 0, "items in one page"+zeroBoundHelp)
	cmd.Flags().String(queryCursorFlag, "", "continue a previous answer from the token it printed as next; the continuation stays on that answer's generation")
}

// addTraversalFlags declares the walk budgets. withEdges is false for `path`,
// whose request carries no edge budget: a flag with no field behind it would be
// help text describing a request the command cannot build.
func addTraversalFlags(cmd *cobra.Command, withEdges bool) {
	cmd.Flags().Int(queryDepthFlag, 0, "maximum hops from the nearest start node"+zeroBoundHelp)
	cmd.Flags().Int(queryVisitedFlag, 0, "maximum distinct nodes the walk may admit, cumulative across pages"+zeroBoundHelp)
	if withEdges {
		cmd.Flags().Int(queryEdgesFlag, 0, "maximum distinct relations the walk may admit, cumulative across pages"+zeroBoundHelp)
	}
}

// pageRequest reads the page flags of a command that declares them.
func pageRequest(cmd *cobra.Command) (model.PageRequest, error) {
	limit, err := intFlag(cmd, queryLimitFlag)
	if err != nil {
		return model.PageRequest{}, err
	}
	cursor, err := stringFlag(cmd, queryCursorFlag)
	if err != nil {
		return model.PageRequest{}, err
	}
	return model.PageRequest{Limit: limit, Cursor: cursor}, nil
}

func generationFlag(cmd *cobra.Command) (model.GenerationID, error) {
	v, err := cmd.Flags().GetInt64(queryGenerationFlag)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return model.GenerationID(v), nil
}

func intFlag(cmd *cobra.Command, name string) (int, error) {
	v, err := cmd.Flags().GetInt(name)
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

func durationFlag(cmd *cobra.Command, name string) (time.Duration, error) {
	v, err := cmd.Flags().GetDuration(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	if v < 0 {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: "--" + name + " must not be negative"}
	}
	return v, nil
}

// clip bounds one display cell. It never shortens an identifier a caller has to
// be able to copy back into a command: only names, paths and reasons pass
// through it.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// atoiOrZero reads a capability detail that the publisher writes as a number.
// A detail map is free-form strings, so an unparsable value renders as the
// plural form rather than failing the answer that carries it.
func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
