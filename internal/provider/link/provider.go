// Package link is the package-scoped linking provider of ADR-0012 decision 9.
// It publishes `calls` for every language with a lowering, from the link
// tables the structural provider stores with each file's unit, and never
// parses a file.
//
// A unit is one project of the repository, the scopes dependence.PlanUnits
// derives. Its inputs are the project's member files, each recording the
// digest of the table it linked (model.UnitInput.Digest). A build whose every
// member digest equals its predecessor's resolves nothing: it re-emits the
// predecessor's resolution with the ranges and identities of the fresh
// tables (docs/providers-link.md).
//
// Resolution is decision 2's, per language family: class-hierarchy analysis
// for Java; direct binding, the signature-keyed address-taken × indirect-site
// cross-product and class-hierarchy analysis for Go, Rust, C and C++;
// flow-based type inference through assignments, parameters and returns,
// field-based resolution and class-hierarchy analysis for the ECMAScript
// family and Python. No whole-program points-to and no join on the callee
// name alone. Every join is an ordered merge over sorted runs that spill
// through pagination.ExternalSort, so nothing the size of the project is
// resident.
package link

import (
	"context"
	"io"

	"github.com/Sawmonabo/codectx/internal/model"
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
	// table keeps its predecessor's digest; nil resolves in full.
	Previous io.Reader
	// State receives this unit's resolution state, which the build stores
	// under ResolutionKind.
	State io.Writer
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
// (or re-emits opts.Previous), publishes `calls` and writes the resolution
// state to opts.State.
func (p *Provider) Link(ctx context.Context, req provider.UnitRequest, sink provider.Sink, opts LinkOptions) (model.ProviderResult, error) {
	return model.ProviderResult{RunID: req.Run, State: model.RunFailed},
		&model.Error{Code: model.CodeInternal, Message: "link: the linker core is not built"}
}
