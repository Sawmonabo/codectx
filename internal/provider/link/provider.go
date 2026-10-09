// Package link is the package-scoped linking provider of ADR-0012 decision 9.
// It publishes `calls` for every language with a lowering, from the link
// tables the structural provider stores with each file's unit, and never
// parses a file.
//
// A unit is one project of the repository, the scopes dependence.PlanUnits
// derives. Its inputs are the project's member files, each recording the
// MemberDigest of the table it linked (model.UnitInput.Digest). A build whose
// every member digest equals its predecessor's resolves nothing: it inherits
// the predecessor's resolution, republishes only the facts of the members
// whose content changed, and the build carries the rest over
// (docs/providers-link.md).
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
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
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
	// resolution under, which a later build whose member tables all keep
	// their digests re-emits instead of resolving.
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
	// DetailAmbiguous is the `calls` edge to an ambiguous site's callee
	// node, and DetailAmbiguousTarget each of its `may_refer_to` edges,
	// spelled as the structural provider spells its own.
	DetailAmbiguous       = "ambiguous"
	DetailAmbiguousTarget = "ambiguous call target"
)

// LinkOptions is what the delta build hands one unit's link.
type LinkOptions struct {
	// Previous streams the predecessor's resolution state when every member
	// keeps its predecessor's MemberDigest; nil resolves in full. The build
	// validates it first (ValidateState): Link refuses a corrupt one.
	Previous io.Reader
	// Edited, with Previous, streams the FileIDs of the members whose content
	// changed since the predecessor, in ascending order; it may name files
	// that are not members and must yield the same IDs on every walk. Link
	// then publishes only the facts whose evidence lies in an edited member
	// and takes the coverage from Previous: the build carries the
	// predecessor's other facts over. Nil publishes every fact.
	Edited func(yield func(model.FileID) error) error
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

// Link links one unit: it streams the members' tables, resolves every site
// (or inherits opts.Previous), publishes `calls` (every fact, or with
// opts.Edited only the edited members') and writes the resolution state to
// opts.State.
func (p *Provider) Link(ctx context.Context, req provider.UnitRequest, sink provider.Sink, opts LinkOptions) (model.ProviderResult, error) {
	failed := model.ProviderResult{RunID: req.Run, State: model.RunFailed}
	if opts.Edited != nil && opts.Previous == nil {
		return failed, internalErr("a link that publishes only the edited members needs the predecessor's resolution")
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
	var pu dependence.Unit
	var each func(yield func(model.FileVersion) error) error
	var count int64
	if opts.Members != nil {
		family, root, ok := dependence.ParseScopeKey(req.Unit.ScopeKey)
		if !ok {
			return failed, &model.Error{Code: model.CodeArgumentInvalid, Message: "the linking provider has no unit for this scope",
				Details: map[string]string{"scope_key": req.Unit.ScopeKey}}
		}
		pu = dependence.Unit{ScopeKey: req.Unit.ScopeKey, Family: family, Root: root}
		each = func(yield func(model.FileVersion) error) error {
			return opts.Members(func(fv model.FileVersion) error {
				if fv.Status == model.FileDeleted || dependence.FamilyOf(srclang.Of(fv.Path)) != family {
					return nil
				}
				return yield(fv)
			})
		}
		if err := each(func(model.FileVersion) error { count++; return nil }); err != nil {
			return failed, err
		}
	} else {
		members, err := planMembers(ctx, req, dir)
		if err != nil {
			return failed, err
		}
		defer members.Close()
		pu, each, count = members.unit, members.Each, members.Len()
	}
	u := &unit{gen: req.Binding.GenerationID, tables: p.tables, dir: dir, root: pu.Root, content: req.Content,
		family: pu.Family, members: each}

	sorted, err := newBindingSort(dir, compareSiteTarget)
	if err != nil {
		return failed, err
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
	var inherited coverage
	if opts.Previous != nil {
		inherited, err = readState(opts.Previous, emit)
	} else {
		err = resolverFor(pu.Family)(ctx, u, emit)
	}
	if err != nil {
		return failed, err
	}
	bindings, err := sorted.Sorted()
	if err != nil {
		return failed, err
	}
	defer bindings.Close()

	pub := &publisher{ctx: ctx, req: req, sink: sink, u: u, members: count, edited: opts.Edited}
	if opts.Edited != nil {
		pub.cov = inherited
	}
	if err := pub.publish(bindings); err != nil {
		failed.RecordsEmitted = pub.records
		return failed, err
	}
	if opts.State != nil {
		if err := writeState(opts.State, bindings, pub.cov); err != nil {
			return failed, err
		}
	}
	return model.ProviderResult{RunID: req.Run, State: model.RunSucceeded, RecordsEmitted: pub.records,
		Capabilities: []model.CapabilityState{pub.capability()}}, nil
}

// capability is the unit's `calls` row: fresh when every member's table was
// read whole and every site with a binding was published, and partial, with
// what was left out counted, otherwise.
func (p *publisher) capability() model.CapabilityState {
	st := model.CapabilityState{ProviderID: ProviderID, Capability: string(model.RelCalls), Scope: p.req.Unit.ScopeKey,
		State: model.CapabilityFresh}
	if p.cov.untabled == 0 && p.cov.dropped == 0 && p.unkeyed == 0 {
		return st
	}
	st.State, st.DiagnosticCode = model.CapabilityPartial, model.CodeCoverageIncomplete
	if p.cov.untabled > 0 {
		st = st.WithDetail("members_without_table", strconv.FormatInt(p.cov.untabled, 10))
	}
	if p.cov.dropped > 0 {
		st = st.WithDetail("dropped_table_entries", strconv.FormatInt(p.cov.dropped, 10))
	}
	if p.unkeyed > 0 {
		st = st.WithDetail("unkeyed_ambiguous_sites", strconv.FormatInt(p.unkeyed, 10))
	}
	return st.WithDetail("members", strconv.FormatInt(p.members, 10))
}
