package treesitter

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// capabilities is the descriptor's capability list in its published order:
// the file's structure, then the four file-local dependence families, each
// named by the relation kind it publishes.
var capabilities = []string{capabilityName, string(model.RelControlDependsOn), string(model.RelDataFlowsTo),
	string(model.RelReads), string(model.RelWrites)}

// The capability details a file's dependence rows disclose.
const (
	// detailFailedFunctions is the count of functions whose analysis the
	// worker recovered from, a space, and the first one's byte range
	// "start-end".
	detailFailedFunctions = "failed_functions"
	// detailFailureCause is the first failed function's recovered text, or
	// the file's dependence failure.
	detailFailureCause = "failure_cause"
	// detailUnresolvedJumps is the count of jumps whose target names no open
	// frame or label.
	detailUnresolvedJumps = "unresolved_jumps"
)

// The evidence details of the four families (docs/providers-treesitter.md,
// Published shape).
const (
	detailControl       = "cdg"
	detailFlow          = "reaching_def"
	detailUse           = "use"
	detailDefinition    = "definition"
	detailMayDefinition = "may_definition"
)

// functions validates every function message against the pinned bytes and
// publishes its variables and its control_depends_on, data_flows_to, reads
// and writes facts. A failed function publishes nothing; it is disclosed on
// the file's dependence rows (capabilities). A variable is one node per
// declaring identifier in the file: a closure's message that names a
// variable an enclosing function declared names the same node.
func (b *builder) functions() error {
	if len(b.ex.functions) == 0 {
		return nil
	}
	if b.ex.done.DependenceFailure != "" {
		return outputInvalid("a file whose dependence pass failed also sent function messages")
	}
	byStart := b.declsByStart()
	vars := map[wire.Span]model.NodeID{}
	for i := range b.ex.functions {
		f := &b.ex.functions[i]
		if f.Span.End <= f.Span.Start {
			return outputInvalid("a function span [" + strconv.Itoa(int(f.Span.Start)) + "," + strconv.Itoa(int(f.Span.End)) + ") is empty")
		}
		if _, err := b.rangeOf(f.Span.Start, f.Span.End); err != nil {
			return err
		}
		if f.Failed {
			continue
		}
		owner, ownerKey, ownerQualified := b.owner(byStart, f.Span)
		ids := make([]model.NodeID, len(f.Vars))
		for j, v := range f.Vars {
			// A variable's identifier need not lie inside its function's
			// span: a Java compact constructor's parameters are declared on
			// the record header.
			if v.End <= v.Start {
				return outputInvalid("a variable range [" + strconv.Itoa(int(v.Start)) + "," + strconv.Itoa(int(v.End)) + ") is empty")
			}
			rng, err := b.rangeOf(v.Start, v.End)
			if err != nil {
				return err
			}
			id, seen := vars[v]
			if !seen {
				if id, err = b.variable(v, rng, ownerQualified); err != nil {
					return err
				}
				vars[v] = id
			}
			ids[j] = id
		}
		nodes := make([]*model.SourceRange, len(f.Nodes))
		for j, n := range f.Nodes {
			rng, err := b.rangeOf(n.Start, n.End)
			if err != nil {
				return err
			}
			nodes[j] = rng
		}
		for _, p := range f.Control {
			b.dependsOn(ids[p.From], model.RelControlDependsOn, ids[p.To], nodes[p.Node], ownerKey, detailControl)
		}
		for _, p := range f.Flows {
			b.dependsOn(ids[p.From], model.RelDataFlowsTo, ids[p.To], nodes[p.Node], ownerKey, detailFlow)
		}
		for _, a := range f.Reads {
			b.dependsOn(owner, model.RelReads, ids[a.Var], nodes[a.Node], ownerKey, detailUse)
		}
		for _, a := range f.Writes {
			detail := detailDefinition
			if a.May {
				detail = detailMayDefinition
			}
			b.dependsOn(owner, model.RelWrites, ids[a.Var], nodes[a.Node], ownerKey, detail)
		}
	}
	return nil
}

// dependsOn publishes one occurrence of a dependence fact, or counts it when
// an endpoint is a variable whose key did not fit.
func (b *builder) dependsOn(from model.NodeID, kind model.RelationKind, to model.NodeID, rng *model.SourceRange, ownerKey, detail string) {
	if from == "" || to == "" {
		b.dropped[dependence]++
		return
	}
	b.putRelation(dependence, from, kind, to, rng, ownerKey, detail)
}

// declsByStart is the file's declaration indices ordered by start offset,
// ties in declaration order, so owner finds the innermost declaration
// containing a span by a binary search instead of a scan of the file.
func (b *builder) declsByStart() []int {
	order := make([]int, len(b.decls))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(x, y int) int { return cmp.Compare(b.decls[x].Start, b.decls[y].Start) })
	return order
}

// owner is the declaration that owns a function's variables, reads and
// writes: the innermost structural declaration of the file whose range
// contains the function's span, else the file's module. It answers the
// owner's node, the native key its facts' evidence carries (its qualified
// name) and the qualified name its variables are named under, which is empty
// for the module, whose variables are named by their name alone.
//
// Of the declarations starting at or before the span, the one with the
// greatest start that contains it is the innermost; among equal starts the
// later declaration is nested in the earlier one.
func (b *builder) owner(byStart []int, s wire.Span) (model.NodeID, string, string) {
	n, _ := slices.BinarySearchFunc(byStart, s.Start+1, func(i int, start uint32) int { return cmp.Compare(b.decls[i].Start, start) })
	for k := n - 1; k >= 0; k-- {
		d := &b.decls[byStart[k]]
		if d.End >= s.End {
			return d.res.Node.ID, d.Qualified, d.Qualified
		}
	}
	return b.module.Node.ID, b.fv.Path, ""
}

// variable resolves and publishes the variable entity declared by the
// identifier at v (docs/providers-treesitter.md, Variable entity). Its native
// key and alias are the cross-provider declaration key over the identifier,
// so another producer of the same declaration resolves to the same identity.
// A key over its bound is omitted, never truncated: the variable is dropped
// and counted, and it answers the empty id, which drops its facts with it.
// A name or qualified name over its storage ceiling is cut and flagged, as a
// declaration's is.
func (b *builder) variable(v wire.Span, rng *model.SourceRange, ownerQualified string) (model.NodeID, error) {
	name := string(b.src[v.Start:v.End])
	key := b.declKey(name, v.Start, v.End, rng)
	if key == "" {
		b.dropped[dependence]++
		return "", nil
	}
	qualified := name
	if ownerQualified != "" {
		qualified = ownerQualified + b.lang.Separator + name
	}
	var truncated truncations
	name = truncated.cut("name", name, model.MaxNameBytes)
	qualified = truncated.cut("qualified_name", qualified, model.MaxQualifiedNameBytes)
	res, err := b.resolve(model.NodeCandidate{
		ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: key, Kind: model.NodeVariable, Language: b.lang.Name,
		Name: name, QualifiedName: qualified, FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: rng,
	})
	if err != nil {
		return "", err
	}
	var meta map[string]any
	if len(truncated) > 0 {
		meta = map[string]any{"truncated_fields": truncated}
	}
	b.putNode(dependence, res, rng, key, meta)
	b.ambiguous(res, rng)
	b.putAlias(b.scope, key, res.Node.ID)
	return res.Node.ID, nil
}

// capabilities is the file's five capability rows: the structure row as the
// caller built it, then one row per dependence family, which share one
// state. They are partial when the file has syntax errors, when a function's
// analysis failed, when a jump resolved to no target, or when a bound kept a
// dependence fact out, and failed when the file's dependence pass failed as
// a whole. None of this touches the structure row or fails the unit.
func (b *builder) capabilities(structureRow model.CapabilityState) []model.CapabilityState {
	dep := model.CapabilityState{ProviderID: structureRow.ProviderID, Scope: structureRow.Scope, State: model.CapabilityFresh}
	partial := b.ex.done.SyntaxErrors
	failed, first := 0, -1
	var unresolved uint64
	for i := range b.ex.functions {
		f := &b.ex.functions[i]
		if f.Failed {
			if first < 0 {
				first = i
			}
			failed++
			continue
		}
		unresolved += uint64(f.Unresolved)
	}
	if failed > 0 {
		f := &b.ex.functions[first]
		dep = dep.WithDetail(detailFailedFunctions, strconv.Itoa(failed)+" "+
			strconv.FormatUint(uint64(f.Span.Start), 10)+"-"+strconv.FormatUint(uint64(f.Span.End), 10)).
			WithDetail(detailFailureCause, f.Cause)
		partial = true
	}
	if unresolved > 0 {
		dep = dep.WithDetail(detailUnresolvedJumps, strconv.FormatUint(unresolved, 10))
		partial = true
	}
	dep, bounded := b.bounds(dependence, dep)
	switch {
	case b.ex.done.DependenceFailure != "":
		dep = dep.WithDetail(detailFailureCause, b.ex.done.DependenceFailure)
		dep.State, dep.DiagnosticCode = model.CapabilityFailed, model.CodeInternal
	case partial || bounded:
		dep.State, dep.DiagnosticCode = model.CapabilityPartial, model.CodeCoverageIncomplete
	}
	rows := make([]model.CapabilityState, 0, len(capabilities))
	rows = append(rows, structureRow)
	for _, c := range capabilities[1:] {
		row := dep
		row.Capability = c
		rows = append(rows, row)
	}
	return rows
}
