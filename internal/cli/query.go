package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// Flag spellings for the Section 18.1 query commands. Each command declares
// only the flags its own request type has a field for: `path` carries no page
// and no edge budget, `refs` carries no traversal budget, so offering those
// flags there would be help text that describes a request the command cannot
// build.
const (
	queryDepthFlag   = "depth"
	queryVisitedFlag = "visited"
	queryEdgesFlag   = "edges"
)

// zeroBoundHelp is the one sentence every budget flag ends with. A zero here is
// not "unlimited": the engine resolves it to the configured default, so the
// flag's default must stay 0 rather than repeating a config value the operator
// may have changed (Section 20.1).
const zeroBoundHelp = " (0 uses the configured default)"

// nameArgumentHelp describes the `<name-or-id>` argument. Ruling Q9: a name is
// resolved through Workspace.ResolveNodes, which reads PinnedReader.Nodes --
// storage rather than search, so this path does not make the graph depend on
// search. A name several nodes carry is rejected with the candidates rather
// than silently resolved to the first of them.
const nameArgumentHelp = "Each argument is either a resolved node id (64 lowercase hex characters) or a symbol name. A name is matched against the exact name first and the exact qualified name second, in the pinned generation; a name that several nodes carry is rejected with the candidate ids rather than guessed at."

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
			"--kind picks the evidence asked for: references (the default), implements or " +
			"type-definition. --semantic-source picks where it comes from: canonical (the " +
			"default) answers sealed facts only, and lsp answers from the managed language " +
			"server named by --profile as labeled overlay rows that are never persisted as " +
			"canonical facts and never grant coverage credit.\n\n" + nameArgumentHelp,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The arguments and the flags are screened before a workspace is
			// opened: a rejection that first opened the database has already
			// paid for work it will not do. The request itself can only be
			// built once a name argument has been resolved against the pinned
			// generation, so its own validation happens inside the callback.
			if err := checkNodeArgs(args); err != nil {
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
			operation, err := referenceKindValue(cmd)
			if err != nil {
				return err
			}
			source, profile, err := semanticFlagValues(cmd, false)
			if err != nil {
				return err
			}
			var result model.Page[model.ReferenceOccurrence]
			if err := runService(cmd, openForReport(), func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
				// Names are resolved against the same generation the answer
				// will pin, so a name and an id name the same node in one
				// invocation. An argument that is already an id passes through.
				nodes, err := ws.ResolveNodes(ctx, gen, args)
				if err != nil {
					return err
				}
				// Both semantic sources are one facade call: References routes
				// canonical to the sealed index and lsp to the overlay, so the
				// command chooses evidence, never a call path.
				result, err = svc.References(ctx, model.ReferenceRequest{
					GenerationID:   gen,
					NodeID:         nodes[0],
					Operation:      operation,
					SemanticSource: source,
					Profile:        profile,
					Page:           page,
				})
				return err
			}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				writeOccurrences(b, result.Items)
			})
		},
	}
	addQueryFlags(cmd, true)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	cmd.Flags().String(referenceKindFlag, string(model.ReferenceReferences),
		"evidence to answer: references, implements or type-definition")
	addSemanticFlags(cmd, false)
	return cmd
}

// referenceKindValue reads --kind. `refs` spells the reference operation as a
// kind because that is what the three values name -- which evidence about the
// node is wanted -- and because --operation is `symbol`'s flag over a different
// enum; one spelling over two enums would make the two commands look
// interchangeable when they are not.
func referenceKindValue(cmd *cobra.Command) (model.ReferenceOperation, error) {
	raw, err := stringFlag(cmd, referenceKindFlag)
	if err != nil {
		return "", err
	}
	op := model.ReferenceOperation(raw)
	if !op.Valid() {
		return "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s %q is not a known reference operation", referenceKindFlag, clip(raw, 64))}).
			WithRemediation("pass one of: " + string(model.ReferenceReferences) + ", " +
				string(model.ReferenceImplements) + ", " + string(model.ReferenceTypeDefinition))
	}
	return op, nil
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
			"the reason rather than presenting it as exhaustive.\n\n" +
			"The walk is answered from sealed canonical facts. --semantic-source is accepted so " +
			"that asking for the overlay is refused explicitly rather than answered from canonical " +
			"facts under an lsp label; there is no overlay call hierarchy in this build.\n\n" +
			nameArgumentHelp,
		Args:          cobra.RangeArgs(1, model.MaxStartNodes),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkNodeArgs(args); err != nil {
				return err
			}
			gen, err := generationFlag(cmd)
			if err != nil {
				return err
			}
			// Screened for its refusal, not for a value: the call hierarchy has
			// only the canonical route, so this rejects `--semantic-source lsp`
			// explicitly instead of answering it from canonical facts.
			if _, _, err := semanticFlagValues(cmd, true); err != nil {
				return err
			}
			var result model.GraphResult
			if err := runService(cmd, openForReport(), func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
				nodes, err := ws.ResolveNodes(ctx, gen, args)
				if err != nil {
					return err
				}
				// Direction and the `calls` allowlist are pinned here, by the
				// command, so the single bounded traversal the facade exposes
				// answers both spellings; a second operation selector would be
				// the same contract pinned in two places.
				req, err := graphRequest(cmd, nodes, direction, []model.RelationKind{model.RelCalls})
				if err != nil {
					return err
				}
				result, err = svc.Graph(ctx, req)
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
	addQueryFlags(cmd, true)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	addTraversalFlags(cmd, true, true)
	addSemanticFlags(cmd, true)
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
			if err := checkNodeArgs(args); err != nil {
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
			var result model.PathResult
			if err := runService(cmd, openForReport(), func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
				nodes, err := ws.ResolveNodes(ctx, gen, args)
				if err != nil {
					return err
				}
				result, err = svc.Path(ctx, model.PathRequest{
					GenerationID: gen,
					From:         nodes[0],
					To:           nodes[1],
					MaxDepth:     depth,
					MaxVisited:   visited,
				})
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
	addQueryFlags(cmd, false)
	addTraversalFlags(cmd, false, false)
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
			if err := checkNodeArgs(args); err != nil {
				return err
			}
			gen, err := generationFlag(cmd)
			if err != nil {
				return err
			}
			var result model.ImpactResult
			if err := runService(cmd, openForReport(), func(ctx context.Context, ws *app.Workspace, svc *app.Services) error {
				nodes, err := ws.ResolveNodes(ctx, gen, args)
				if err != nil {
					return err
				}
				base, err := graphRequest(cmd, nodes, model.DirectionBoth, nil)
				if err != nil {
					return err
				}
				// Impact returns the whole model.ImpactResult, not a page of
				// entries: the package rollup and the visited/edge accounting
				// this command renders have no home in model.Page.
				result, err = svc.Impact(ctx, model.ImpactRequest{
					GenerationID: base.GenerationID,
					Start:        base.Start,
					Direction:    base.Direction,
					MaxDepth:     base.MaxDepth,
					MaxVisited:   base.MaxVisited,
					MaxEdges:     base.MaxEdges,
					Page:         base.Page,
				})
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
	addQueryFlags(cmd, true)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	addTraversalFlags(cmd, true, true)
	return cmd
}

// checkNodeArgs screens the positional arguments before any database is opened.
// It cannot resolve a name -- that needs the pinned generation the service
// call opens -- but it can reject an argument that is neither a resolved id nor a name worth
// looking up, so an obviously wrong command line never pays for a workspace.
func checkNodeArgs(args []string) error {
	for _, arg := range args {
		if model.ValidHexID(arg) {
			continue
		}
		if arg == "" || len(arg) > model.MaxIdentifierBytes || strings.ContainsAny(arg, "\x00\n\r\t") {
			return (&model.Error{Code: model.CodeArgumentInvalid,
				Message: fmt.Sprintf("%q is neither a resolved node id (%d lowercase hex characters) nor a symbol name",
					clip(arg, 64), model.IDHexLen)}).WithDetail("argument", clip(arg, 64))
		}
	}
	return nil
}

// graphRequest builds the traversal request shared by the call commands and
// impact. Direction and the relation allowlist are the command's own, never the
// operator's: Section 4b pins them to the operation.
func graphRequest(cmd *cobra.Command, nodes []model.NodeID, direction model.Direction,
	relations []model.RelationKind) (model.GraphRequest, error) {
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
		// Deliberately neutral: runService carries every command that reaches
		// the facade, including `index --watch`, whose cancellation is a
		// stopped session rather than an abandoned query.
		return &model.Error{Code: model.CodeCanceled,
			Message: "the command was canceled before it completed"}
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
		// Every row carries the three things that tell one occurrence from
		// another and lead the operator to it: which end of the edge is not the
		// node being asked about, where the site is, and the evidence identity.
		// Without them a node called twelve times from one file renders as
		// twelve identical lines, which is an answer the operator cannot act on.
		//
		// References deliberately never sets Path -- resolving it would cost one
		// file read per distinct file on a port with no batched file lookup --
		// so the location is the file id, refined to :line:column when the
		// occurrence carries a range (the language server overlay's rows do).
		location := string(o.FileID)
		if o.Path != "" {
			location = o.Path
		}
		if o.Range != nil {
			location = fmt.Sprintf("%s:%d:%d", location, o.Range.Start.Line, o.Range.Start.Column)
		}
		fmt.Fprintf(b, "  %-18s %-16s %s at %s  evidence %s\n", clip(string(o.Kind), 18),
			clip(string(o.Precision), 16), occurrenceOrigin(o), clip(location, 100), occurrenceEvidence(o))
	}
}

// occurrenceOrigin names the end of the edge the caller did not ask about: the
// referencing node for a reference or an implementation, the referenced type
// for a type definition. It is printed as a full node id because that is what
// the operator feeds straight back to `refs` or `symbol` to see where the site
// is; a clipped id would name nothing they can ask about. An overlay row that
// sealed no canonical edge has no such id and says so rather than printing an
// empty column that reads as a node whose id is unknown.
func occurrenceOrigin(o model.ReferenceOccurrence) string {
	switch {
	case o.FromNodeID != "":
		return "from " + string(o.FromNodeID)
	case o.ToNodeID != "":
		return "to   " + string(o.ToNodeID)
	default:
		return "from (overlay row; no canonical node)"
	}
}

// occurrenceEvidence is the identity that separates two occurrences of the same
// relation in the same file. It is clipped: it is an identity to compare
// against the --json answer, not an argument any command takes.
func occurrenceEvidence(o model.ReferenceOccurrence) string {
	if o.EvidenceID == "" {
		return "(none sealed)"
	}
	return clip(string(o.EvidenceID), 16)
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
		// ImpactEntry.Path carries the entry's QUALIFIED NAME: model.Node has no
		// file path, and the hydration fills this field from QualifiedName. The
		// column is labelled for what it holds, so a reader does not take it
		// for a file path sitting next to a FileID.
		fmt.Fprintf(b, "  %s  %-9s depth %d  score %d  name %s\n", e.NodeID, e.Direction, e.Depth,
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
		// Silence here read as "this renderer emitted nothing". The rollup is
		// built from containment edges, and a generation whose providers sealed
		// none has no packages to aggregate -- which is an answer about the
		// generation, not a missing section of the report.
		b.WriteString("packages    none (no containment edges in this generation)\n")
		return
	}
	fmt.Fprintf(b, "packages    %d aggregated %s (pair counts, not individual calls)\n",
		len(edges), plural(len(edges), "pair", "pairs"))
	for _, e := range edges {
		fmt.Fprintf(b, "  %-40s -> %-40s %d %s, %d evidence\n", clip(e.FromPath, 40), clip(e.ToPath, 40),
			e.PairCount, plural(int(e.PairCount), "pair", "pairs"), e.EvidenceCount)
	}
}

// addTraversalFlags declares the walk budgets. The two switches are separate
// because the commands differ on both axes: `path` carries no edge budget at
// all and issues no continuation, while `impact` carries one and pages its
// ranked list. A flag with no field behind it would describe a request the
// command cannot build, and "cumulative across pages" on a command that issues
// no continuation would describe a workflow it cannot perform.
func addTraversalFlags(cmd *cobra.Command, edges, acrossPages bool) {
	cmd.Flags().Int(queryDepthFlag, 0, "maximum hops from the nearest start node"+zeroBoundHelp)
	cumulative := ""
	if acrossPages {
		cumulative = ", cumulative across pages"
	}
	cmd.Flags().Int(queryVisitedFlag, 0, "maximum distinct nodes the walk may admit"+cumulative+zeroBoundHelp)
	if edges {
		cmd.Flags().Int(queryEdgesFlag, 0,
			"maximum distinct relations the walk may admit"+cumulative+zeroBoundHelp)
	}
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
