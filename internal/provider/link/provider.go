// Package link is the package-scoped linking provider of ADR-0012 decision 9.
// It publishes `calls`, `imports`, `data_flows_to`, `extends`, `implements`
// and `overrides` for every language with a lowering, with an open-site node
// for every call site and dynamic import it cannot bind and a node for every
// external type and method its facts target (ADR-0015), from the link tables
// the structural provider stores with each file's unit, and never parses a
// file.
//
// A unit is one project of the repository, the scopes PlanUnits derives. Its
// inputs are the project's member files, each recording the MemberDigest of
// the table it linked (model.UnitInput.Digest). Its resolution state names
// every site by its file and table index and every target by its file and
// declaration node, whose identity a body edit or an inserted declaration
// does not move (ADR-0014). A build against its predecessor's state
// (LinkOptions.Previous) keeps the predecessor's bindings unless a member's
// table digest, the membership or a manifest field the linker reads moved;
// then a static-family unit re-runs only the members whose recorded reads
// changed (trace.go), and a flow-family unit re-derives only the members its
// moved boundary values reach (refresh.go). It
// republishes only the members whose facts changed (refresh.go), and the
// build carries the rest over (docs/providers-link.md).
//
// Resolution is decision 2's, per language family: class-hierarchy analysis
// for Java; direct binding, the signature-keyed address-taken × indirect-site
// cross-product and class-hierarchy analysis for Go, Rust, C and C++;
// flow-based type inference through assignments, parameters and returns,
// field-based resolution and class-hierarchy analysis for the ECMAScript
// family and Python. No whole-program points-to and no join on the callee
// name alone. Bindings sort through runs that spill (pagination.ExternalSort)
// and type questions are point lookups into sorted index files on disk, so
// only one key per 32 KiB block of each index is resident.
package link

import (
	"context"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/Sawmonabo/codectx"
	"github.com/Sawmonabo/codectx/internal/graph"
	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

const (
	// ProviderID is the linking provider's descriptor identity. It is not
	// the `dependence` id, which is never reused (ADR-0012 decision 9).
	ProviderID = "link"
	// TableKind is the delta-state kind a structural unit stores its file's
	// Table under.
	TableKind = "link.tables"
	// ResolutionKind is the delta-state kind a linking unit stores its
	// resolution state under, which a later build of the unit starts from.
	ResolutionKind = "link.resolution"
)

// Version is the descriptor version: the digest of the linker's code
// (codectx.Digest of this package: every package of the module it is built
// from, every outside module they import and the toolchain), so a unit, and
// a predecessor's resolution state, that another build of different code
// wrote is never reused or read (ErrStateVersion), and one of an unchanged
// build always is. It is digested once, when first asked.
var Version = sync.OnceValue(func() string {
	code, err := codectx.Digest("internal/provider/link")
	if err != nil {
		panic("link version: " + err.Error())
	}
	return code[:24]
})

// Capabilities are the capabilities the provider declares, one row of each
// per unit.
var Capabilities = []string{string(model.RelCalls), string(model.RelImports), string(model.RelDataFlowsTo),
	string(model.RelExtends), string(model.RelImplements), string(model.RelOverrides), string(model.RelGenerates)}

// The evidence detail of a published occurrence names the technique that
// found its fact.
const (
	// DetailDirect: the callee names the declaration directly, by its
	// package, module, import or scope.
	DetailDirect = "direct"
	// DetailHierarchy: the method the receiver's static type declares or
	// inherits, found by walking its supertypes; the occurrence's dispatch
	// says whether an override in a subtype may run instead.
	DetailHierarchy = "hierarchy"
	// DetailAddressTaken: an indirect site joined to an address-taken
	// function of the same signature.
	DetailAddressTaken = graph.CallDetailAddressTaken
	// DetailReceiverValue: the method the class of an instance declares or
	// inherits, where the instance reached the receiver through a parameter
	// or a field the project's code fills, rather than one the call's own
	// function constructs, annotates or receives from a call: the receiver's
	// values are the project's, and code outside the project may pass or
	// store another (graph value_set).
	DetailReceiverValue = graph.CallDetailReceiverValue
	// DetailFlow: the receiver's type was recovered by flow-based type
	// inference through assignments, parameters and returns.
	DetailFlow = "flow"
	// DetailFunctionValue: the function values that reach the value the
	// site calls, through assignments, parameters, returns and fields
	// (flow-based value inference).
	DetailFunctionValue = graph.CallDetailFunctionValue
	// DetailField: field-based resolution, a function written to the field
	// the site calls through.
	DetailField = graph.CallDetailField
	// DetailImport: an `imports` from a file's module to the module of a
	// file its import resolves to.
	DetailImport = "import"
	// DetailArgument: a `data_flows_to` from a variable an argument reads to
	// the parameter the argument binds at a bound call site. It is the role
	// an impact walk reads (graph.FlowDetailArgument), so the two cannot drift.
	DetailArgument = graph.FlowDetailArgument
	// DetailSummary: a `data_flows_to` from a variable an argument reads to a
	// variable the bound call site's result is written into, through the
	// callee's parameter whose value reaches its return (the callee's return
	// summary). The published evidence detail is "summary:" and the
	// parameter's position (Parameter.Index).
	DetailSummary = "summary"
	// DetailReturn: a `data_flows_to` from the bound callee's return node to
	// a variable the call site's result is written into, or to the return
	// node of the callable that returns the call's value. An impact walk that
	// has taken a DetailArgument edge never takes one (ADR-0017 decision 7);
	// it is the role that walk reads (graph.FlowDetailReturn).
	DetailReturn = graph.FlowDetailReturn
	// DetailSupertype: an `extends`, `implements` or `overrides` the source
	// declares: a written supertype, and a method of the same name and
	// signature along the written supertypes.
	DetailSupertype = "supertype"
	// DetailMethodSet: a Go `implements` or `overrides` of an interface the
	// type satisfies by its method set, which the source never writes.
	DetailMethodSet = "method_set"
	// DetailSupertypeInterface: an `extends` or `implements` the source
	// declares into a type outside the project (a node of kind type) that
	// the source proves an interface or trait: a class's `implements`
	// clause, an interface's `extends` clause, a Rust trait, a Go
	// interface's embedded interface.
	DetailSupertypeInterface = "supertype_interface"
	// DetailSupertypeClass: an `extends` the source declares into a type
	// outside the project that the source proves a class: a class's
	// `extends` clause or base list.
	DetailSupertypeClass = "supertype_class"
	// DetailSupertypeCandidate: an `overrides` of a method of a type outside
	// the project that a Java `@Override` or C++ `override` method may
	// override: one of the external supertypes on a path no type of the
	// project declares the method along, read with its set size as "at least
	// one of n" (several may hold at once), or one published beside an
	// override of the project's own, which the mark already proves.
	DetailSupertypeCandidate = "supertype_candidate"
	// DetailMacroInput: a `generates` from a Rust macro_rules to a
	// declaration written in its input, which it may transcribe renamed,
	// repeated or not at all.
	DetailMacroInput = "macro_input"
	// DetailMacroTranscribes: a `generates` from a Rust macro_rules every
	// rule of which transcribes its item input once, unchanged: the
	// declaration is exactly an item of its expansion.
	DetailMacroTranscribes = "macro_transcribes"
)

// LinkOptions is what the delta build hands one unit's link.
type LinkOptions struct {
	// Previous streams the predecessor's resolution state, which the build
	// validates first (ValidateState): Link refuses a corrupt one. With it,
	// Link classifies the members against the predecessor's records, finds
	// bindings again only when a member's table digest, the membership or a
	// manifest field the linker reads moved, and then, for the static
	// family, only those of the members whose reads moved; it publishes only the facts
	// of the members whose facts changed, and names those and the members
	// that are gone to Replaced: the build carries the predecessor's other
	// facts over. Nil resolves and publishes in full.
	Previous io.Reader
	// Digests, required with Previous, reads each member's stored
	// MemberDigest, which classifies it without reading its table.
	Digests Digests
	// Replaced, with Previous, receives once the facts are published every
	// file whose predecessor's facts the build must not carry over, in
	// ascending order: the members Link republished and the predecessor's
	// members that are gone. Nil reports none.
	Replaced func(model.FileID) error
	// Members, when set, streams the unit's inputs as file versions in
	// ascending FileID order, the members among them and possibly other
	// inputs (a manifest); Link keeps the unit's members of them and does
	// not sort the manifest for them. It must yield the same files on every
	// walk. Nil finds the members in the pinned snapshot.
	Members func(yield func(model.FileVersion) error) error
	// State receives this unit's resolution state, which the build stores
	// under ResolutionKind; nil writes none.
	State io.Writer
	// Dir is where the unit's private scratch directory is made, the
	// directory its sorted runs spill into; empty takes the system's
	// temporary directory.
	Dir string
}

// Provider is the linking provider.
type Provider struct {
	tables Tables
}

// New builds the provider over the store's stored tables.
func New(tables Tables) *Provider { return &Provider{tables: tables} }

// Descriptor is the provider's static identity: package-scoped, after the
// structural provider whose tables it reads, and not required, since a
// failed link degrades the linker's relations and nothing else.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: ProviderID, Version: Version(), Capabilities: Capabilities,
		DependsOn: []string{lang.ProviderID}, InvalidationScope: model.InvalidationPackage}
}

// Detect reports the provider available: it reads only what the structural
// provider stores.
func (p *Provider) Detect(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
	return provider.Detection{Available: true, Capabilities: Capabilities}, nil
}

// IndexUnit links one unit in full.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	return p.Link(ctx, req, sink, LinkOptions{})
}

// Link links one unit: it streams the members' tables, finds every fact (or
// keeps opts.Previous's), publishes them (every member's, or with
// opts.Previous only those of the members whose facts changed, named to
// opts.Replaced with the members that are gone, and every external node the
// unit's facts target) and writes the resolution state to opts.State.
func (p *Provider) Link(ctx context.Context, req provider.UnitRequest, sink provider.Sink, opts LinkOptions) (model.ProviderResult, error) {
	failed := model.ProviderResult{RunID: req.Run, State: model.RunFailed}
	if opts.Previous != nil && opts.Digests == nil {
		return failed, internalErr("a link against its predecessor's state needs the members' stored digests")
	}
	// The unit's sorted runs spill into a directory of its own, removed
	// through the pace when the link ends.
	dir, err := os.MkdirTemp(opts.Dir, "codectx-link-")
	if err != nil {
		return failed, internalErr("link scratch directory: " + err.Error())
	}
	defer paced.RemoveAll(dir)

	// The delta build hands the unit its planned inputs (opts.Members), whose
	// membership the planner already decided, so they are kept by family and
	// liveness alone and the snapshot is not walked. Without them the unit
	// plans its own members in one read of the manifest.
	var pu Unit
	var each func(yield func(model.FileVersion) error) error
	if opts.Members != nil {
		family, root, ok := ParseScopeKey(req.Unit.ScopeKey)
		if !ok {
			return failed, &model.Error{Code: model.CodeArgumentInvalid, Message: "the linking provider has no unit for this scope",
				Details: map[string]string{"scope_key": req.Unit.ScopeKey}}
		}
		pu = Unit{ScopeKey: req.Unit.ScopeKey, Family: family, Root: root}
		each = func(yield func(model.FileVersion) error) error {
			return opts.Members(func(fv model.FileVersion) error {
				if fv.Status == model.FileDeleted || FamilyOf(srclang.Of(fv.Path)) != family {
					return nil
				}
				return yield(fv)
			})
		}
	} else {
		members, err := planMembers(ctx, req, dir)
		if err != nil {
			return failed, err
		}
		defer members.Close()
		pu, each = members.unit, members.Each
	}
	u := &unit{gen: req.Binding.GenerationID, tables: p.tables, dir: dir, root: pu.Root, content: req.Content,
		family: pu.Family, members: each}
	tables, spill, err := spillTargets(dir, u.tables)
	if err != nil {
		return failed, err
	}
	defer spill.close()
	u.tables = tables
	fields, err := manifestFields(ctx, u)
	if err != nil {
		return failed, err
	}

	rf := &refresh{u: u, resolve: true}
	pub := &publisher{ctx: ctx, req: req, sink: sink, u: u}
	if opts.Previous != nil {
		if rf.prev, err = loadState(opts.Previous, dir); err != nil {
			return failed, err
		}
		defer rf.prev.close()
		if err := rf.open(); err != nil {
			rf.close()
			return failed, err
		}
		defer rf.close()
		if err := rf.classify(ctx, opts.Digests, fields); err != nil {
			return failed, err
		}
		pub.prev = rf.prev.records
	}

	res := &resolution{}
	if rf.resolve {
		if res, err = resolve(ctx, u, rf, fields); err != nil {
			return failed, err
		}
		defer res.close()
		resolved := res.bindings
		defer resolved.Close()
		var prevFields []manifestField
		if rf.prev != nil {
			prevFields = rf.prev.fields
		}
		if res.bindings, err = withEntries(ctx, u, resolved, fields, prevFields); err != nil {
			return failed, err
		}
	} else {
		res = &rf.prev.resolution
	}
	u.es = nil // a resolution that reused its predecessor's hierarchy read no configuration past the fields
	if pub.spilled, err = spill.sorted(); err != nil {
		return failed, err
	}
	defer pub.spilled.Close()
	bindings := res.bindings
	targets, err := targetsOf(dir, bindings)
	if err != nil {
		return failed, err
	}
	defer targets.Close()
	externals, unnamed, err := externalsOf(dir, bindings)
	if err != nil {
		return failed, err
	}
	defer externals.Close()
	if rf.prev != nil {
		if rf.resolve {
			if err := rf.diff(bindings, targets); err != nil {
				return failed, err
			}
		}
		if pub.republished, err = rf.changed.Sorted(); err != nil {
			return failed, err
		}
		defer pub.republished.Close()
	}

	records, err := pub.publish(bindings, targets, externals)
	if err != nil {
		failed.RecordsEmitted = pub.records
		return failed, err
	}
	defer records.Close()
	if opts.State != nil {
		if err := writeState(opts.State, fields, records, res); err != nil {
			return failed, err
		}
	}
	if pub.republished != nil && opts.Replaced != nil {
		if err := pub.republished.Each(opts.Replaced); err != nil {
			return failed, err
		}
	}
	rows, err := goExternalCoverage(pub.capabilities(records.Len(), unnamed), res.h)
	if err != nil {
		return failed, err
	}
	return model.ProviderResult{RunID: req.Run, State: model.RunSucceeded, RecordsEmitted: pub.records,
		Capabilities: rows}, nil
}

// resolve is the unit's resolution by its family's: the static family's
// re-runs only the members whose reads changed (resolveStatic); the flow
// family's re-derives only the region of members its boundary values reach
// (resolveFlow).
func resolve(ctx context.Context, u *unit, rf *refresh, fields []manifestField) (*resolution, error) {
	switch u.family {
	case FamilyJavaScript, FamilyPython:
		return resolveFlow(ctx, u, rf)
	}
	return resolveStatic(ctx, u, rf, fields)
}

// emptyOf is the empty run of a sort just opened.
func emptyOf[T any](s *pagination.ExternalSort[T], err error) (*pagination.SortedRun[T], error) {
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Sorted()
}

// capabilities are the unit's rows, one per declared capability: fresh
// when every member's table was read whole, and partial, with what was left
// out counted, otherwise. An open site is a fact the rows count, never
// coverage they lack. The `calls` row always carries the site counts, and
// the `imports` row the import counts, summed over the members' records.
func (p *publisher) capabilities(members int64, unnamed map[model.RelationKind]int64) []model.CapabilityState {
	c := p.cov
	out := make([]model.CapabilityState, 0, len(Capabilities))
	for _, capability := range Capabilities {
		st := model.CapabilityState{ProviderID: ProviderID, Capability: capability, Scope: p.req.Unit.ScopeKey,
			State: model.CapabilityFresh}
		switch model.RelationKind(capability) {
		case model.RelCalls:
			for _, d := range [...]struct {
				key string
				n   int64
			}{{"sites", c.Sites}, {"bound_sites", c.Bound}, {"open_sites", c.Open}, {"computed_sites", c.Computed},
				{"external_sites", c.External}, {"reflective_sites", c.Reflective}} {
				st = st.WithDetail(d.key, strconv.FormatInt(d.n, 10))
			}
		case model.RelImports:
			st = st.WithDetail("resolved_imports", strconv.FormatInt(c.Imports, 10)).
				WithDetail("open_import_sites", strconv.FormatInt(c.OpenImports, 10))
		}
		lost := unnamed[model.RelationKind(capability)]
		if c.untabled > 0 || c.dropped > 0 || lost > 0 {
			st.State, st.DiagnosticCode = model.CapabilityPartial, model.CodeCoverageIncomplete
			if lost > 0 {
				st = st.WithDetail("facts_without_node", strconv.FormatInt(lost, 10))
			}
			if c.untabled > 0 {
				st = st.WithDetail("members_without_table", strconv.FormatInt(c.untabled, 10))
			}
			if c.dropped > 0 {
				st = st.WithDetail("dropped_table_entries", strconv.FormatInt(c.dropped, 10))
			}
			st = st.WithDetail("members", strconv.FormatInt(members, 10))
		}
		out = append(out, st)
	}
	return out
}
