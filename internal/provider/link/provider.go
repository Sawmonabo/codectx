// Package link is the package-scoped linking provider of ADR-0012 decision 9.
// It publishes `calls` for every language with a lowering, from the link
// tables the structural provider stores with each file's unit, and never
// parses a file.
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
// changed (trace.go), and a flow-family unit resolves in full. It
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
	// Version is the descriptor version.
	Version = "1"
	// TableKind is the delta-state kind a structural unit stores its file's
	// Table under.
	TableKind = "link.tables"
	// ResolutionKind is the delta-state kind a linking unit stores its
	// resolution state under, which a later build of the unit starts from.
	ResolutionKind = "link.resolution"
)

// Capabilities are the capabilities the provider declares: `calls` alone.
var Capabilities = []string{string(model.RelCalls)}

// The evidence detail of a published `calls` occurrence names the technique
// that bound the site.
const (
	// DetailDirect: the callee names the declaration directly, by its
	// package, module, import or scope.
	DetailDirect = "direct"
	// DetailHierarchy: class-hierarchy analysis over the receiver's static
	// type and its subtypes.
	DetailHierarchy = "hierarchy"
	// DetailAddressTaken: an indirect site joined to an address-taken
	// function of the same signature.
	DetailAddressTaken = "address_taken"
	// DetailFlow: the receiver's type was recovered by flow-based type
	// inference through assignments, parameters and returns.
	DetailFlow = "flow"
	// DetailField: field-based resolution, a function written to the field
	// the site calls through.
	DetailField = "field"
	// DetailImport: an `imports` from a file's module to the module of a
	// file its import resolves to.
	DetailImport = "import"
	// DetailArgument: a `data_flows_to` from a variable an argument reads to
	// the parameter the argument binds at a bound call site.
	DetailArgument = "argument"
	// DetailSupertype: an `extends`, `implements` or `overrides` the source
	// declares: a written supertype, and a method of the same name and
	// signature along the written supertypes.
	DetailSupertype = "supertype"
	// DetailMethodSet: a Go `implements` or `overrides` of an interface the
	// type satisfies by its method set, which the source never writes.
	DetailMethodSet = "method_set"
	// DetailAmbiguous is the `calls` edge to an ambiguous site's callee
	// node, and DetailAmbiguousTarget each of its `may_refer_to` edges,
	// spelled as the structural provider spells its own.
	DetailAmbiguous       = "ambiguous"
	DetailAmbiguousTarget = "ambiguous call target"
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
// failed link degrades `calls` and nothing else.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: ProviderID, Version: Version, Capabilities: Capabilities,
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

// Link links one unit: it streams the members' tables, finds every site's
// bindings (or keeps opts.Previous's), publishes `calls` (every fact, or with
// opts.Previous only the facts of the members whose facts changed, named to
// opts.Replaced with the members that are gone) and writes the resolution
// state to opts.State.
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
	} else {
		res = &rf.prev.resolution
	}
	bindings := res.bindings
	targets, err := targetsOf(dir, bindings)
	if err != nil {
		return failed, err
	}
	defer targets.Close()
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

	records, err := pub.publish(bindings, targets)
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
	return model.ProviderResult{RunID: req.Run, State: model.RunSucceeded, RecordsEmitted: pub.records,
		Capabilities: []model.CapabilityState{pub.capability(records.Len())}}, nil
}

// resolve is the unit's resolution by its family's: the static family's
// re-runs only the members whose reads changed (resolveStatic); the flow
// family's inference is a least fixed point over the whole unit, which a
// removal cannot re-seed soundly without the provenance of every value, so
// it resolves every site and names each binding by identity, and the
// refresh's binding diff keeps the republication to the files whose facts
// changed.
func resolve(ctx context.Context, u *unit, rf *refresh, fields []manifestField) (*resolution, error) {
	switch u.family {
	case FamilyJavaScript, FamilyPython:
	default:
		return resolveStatic(ctx, u, rf, fields)
	}
	sorted, err := newBindingSort(u.dir, compareSiteTarget)
	if err != nil {
		return nil, err
	}
	defer sorted.Close()
	sorted.WithFold(keepOne)
	// A resolver may emit from any goroutine; the sort has one producer.
	var mu sync.Mutex
	emit := func(b Binding) error {
		if _, err := encodeBinding(b); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		return sorted.Add(b)
	}
	if err := resolverFor(u.family)(ctx, u, emit); err != nil {
		return nil, err
	}
	run, err := sorted.Sorted()
	if err != nil {
		return nil, err
	}
	defer run.Close()
	res := &resolution{}
	ok := false
	defer func() {
		if !ok {
			res.close()
		}
	}()
	if res.bindings, err = identify(ctx, u, run); err != nil {
		return nil, err
	}
	if res.groups, err = emptyOf(newGroupSort(u.dir)); err != nil {
		return nil, err
	}
	if res.reads, err = emptyOf(newReadSort(u.dir, compareRead)); err != nil {
		return nil, err
	}
	if res.taken, err = emptyOf(newTakenSort(u.dir, compareTakenFrom)); err != nil {
		return nil, err
	}
	ok = true
	return res, nil
}

// emptyOf is the empty run of a sort just opened.
func emptyOf[T any](s *pagination.ExternalSort[T], err error) (*pagination.SortedRun[T], error) {
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Sorted()
}

// capability is the unit's `calls` row over its members: fresh when every
// member's table was read whole and every site with a binding was published,
// and partial, with what was left out counted, otherwise.
func (p *publisher) capability(members int64) model.CapabilityState {
	st := model.CapabilityState{ProviderID: ProviderID, Capability: string(model.RelCalls), Scope: p.req.Unit.ScopeKey,
		State: model.CapabilityFresh}
	if p.cov.untabled == 0 && p.cov.dropped == 0 && p.cov.unkeyed == 0 {
		return st
	}
	st.State, st.DiagnosticCode = model.CapabilityPartial, model.CodeCoverageIncomplete
	if p.cov.untabled > 0 {
		st = st.WithDetail("members_without_table", strconv.FormatInt(p.cov.untabled, 10))
	}
	if p.cov.dropped > 0 {
		st = st.WithDetail("dropped_table_entries", strconv.FormatInt(p.cov.dropped, 10))
	}
	if p.cov.unkeyed > 0 {
		st = st.WithDetail("unkeyed_ambiguous_sites", strconv.FormatInt(p.cov.unkeyed, 10))
	}
	return st.WithDetail("members", strconv.FormatInt(members, 10))
}
