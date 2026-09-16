package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Registry is the validated provider DAG of Section 13.1. It is built once by
// the composition owner from the complete provider list; construction rejects
// a duplicate ID, a dependency on an unknown provider and a dependency cycle,
// because a coordinator that scheduled such a graph could report units as
// ready whose dependencies can never complete.
type Registry struct {
	byID  map[string]Provider
	order []string
}

// NewRegistry validates every descriptor and the graph they form and returns
// the registry with its stable dependency order.
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{byID: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p == nil {
			return nil, invalid("registry received a nil provider")
		}
		d := p.Descriptor()
		if err := d.Validate(); err != nil {
			return nil, err
		}
		if _, dup := r.byID[d.ID]; dup {
			return nil, invalid(fmt.Sprintf("provider %q is registered twice", d.ID))
		}
		r.byID[d.ID] = p
	}
	for id, p := range r.byID {
		for _, dep := range p.Descriptor().DependsOn {
			if _, ok := r.byID[dep]; !ok {
				return nil, invalid(fmt.Sprintf("provider %q depends on %q, which is not registered", id, dep))
			}
		}
	}
	order, err := r.topological()
	if err != nil {
		return nil, err
	}
	r.order = order
	return r, nil
}

// topological is Kahn's algorithm with the ready set kept sorted, so the order
// is a pure function of the descriptors: a dependency always precedes its
// dependents and ties break by ID, never by registration or map order.
func (r *Registry) topological() ([]string, error) {
	indegree := make(map[string]int, len(r.byID))
	dependents := make(map[string][]string, len(r.byID))
	for id, p := range r.byID {
		deps := p.Descriptor().DependsOn
		indegree[id] = len(deps)
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	var ready []string
	for id, n := range indegree {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	slices.Sort(ready)
	order := make([]string, 0, len(r.byID))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := dependents[id]
		slices.Sort(next)
		for _, d := range next {
			indegree[d]--
			if indegree[d] == 0 {
				i, _ := slices.BinarySearch(ready, d)
				ready = slices.Insert(ready, i, d)
			}
		}
	}
	if len(order) != len(r.byID) {
		var stuck []string
		for id, n := range indegree {
			if n > 0 {
				stuck = append(stuck, id)
			}
		}
		slices.Sort(stuck)
		return nil, invalid(fmt.Sprintf("provider dependencies form a cycle through %v", stuck))
	}
	return order, nil
}

// Providers returns every registered provider in dependency order.
func (r *Registry) Providers() []Provider {
	out := make([]Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Lookup returns the provider registered under id.
func (r *Registry) Lookup(id string) (Provider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// Selection is the outcome of detection over one workspace. Active holds the
// providers that will run, in dependency order. States holds every capability
// row detection publishes, at ScopeWorkspace and one per declared capability:
// `unavailable` for a disabled or absent optional provider, `failed` for an
// enabled provider that could not be detected, and `partial` for a provider
// that *will* run but whose detection named something inside it that cannot
// (Detection.Details). The categories are kept apart because a disabled
// optional tool must not make a healthy base generation falsely fail, while an
// enabled requested capability that fails makes the generation degraded
// (Sections 11.1, 13.3); a partial row is the middle case -- the provider runs,
// and the coordinator publishes the reasons the rest of it will not.
type Selection struct {
	Active []Provider
	States []model.CapabilityState
	// Detections holds the validated Detection of every Active provider, by
	// provider ID. The planner needs two things only a Detection carries --
	// ObservedVersion, folded into UnitSpec.ProviderVersion, and InputPaths,
	// which scope the SCIP profile units -- and must not re-run detection
	// against the live checkout when the plan is about the pinned snapshot.
	Detections map[string]Detection
}

// Select runs trusted detection over the registry in dependency order.
// enablement reports the configured state of each provider ID; a provider the
// configuration does not know is treated as config.Enabled, which is what a
// required base provider is. Under config.Auto an unavailable detection is
// honest absence; under config.Enabled it is a failure of a requested
// capability. A detection error is a failure under either. A provider whose
// dependency is not active cannot run and takes the dependency's category. A
// required provider that ends inactive is returned as an error: no generation
// can be built without it.
func (r *Registry) Select(ctx context.Context, root workspace.Root, policy workspace.Policy,
	enablement func(providerID string) config.Enablement) (Selection, error) {
	var sel Selection
	active := make(map[string]bool, len(r.order))
	inactive := make(map[string]inactiveProvider, len(r.order))
	for _, id := range r.order {
		p := r.byID[id]
		d := p.Descriptor()
		mode := config.Enabled
		if enablement != nil {
			mode = enablement(id)
		}
		state, code, details, det := detect(ctx, p, d, mode, root, policy, active, inactive)
		if err := ctx.Err(); err != nil {
			// A stopped selection is not a list of failed providers.
			return Selection{}, model.Canceled(err)
		}
		if state == "" {
			active[id] = true
			sel.Active = append(sel.Active, p)
			if sel.Detections == nil {
				sel.Detections = make(map[string]Detection, len(r.order))
			}
			sel.Detections[id] = det
			// A provider can be available as a whole and degraded in a part --
			// one of six SCIP indexers missing, say. Those reasons live only in
			// Detection.Details, which nothing else publishes, so without this
			// row a partly working provider is indistinguishable from a healthy
			// one at the only surface that carries capability state.
			//
			// Only a refusal degrades. A provider also uses Details to say what
			// a part of it is about to do -- a pinned payload the first unit
			// will fetch and then index at full precision -- and publishing
			// that as `partial` reports every first run of the product as
			// degraded. The discriminator is the value: a CTX_ code is a
			// refusal, anything else is a marker, and a row carries the
			// refusals alone so a reader cannot mistake one for the other.
			refusals := refusalDetails(details)
			if len(refusals) == 0 {
				continue
			}
			for _, c := range d.Capabilities {
				sel.States = append(sel.States, model.CapabilityState{ProviderID: id, Capability: c,
					Scope: ScopeWorkspace, State: model.CapabilityPartial, DiagnosticCode: code,
					// One map per row: every other detail map in the tree is
					// copy-on-write, and sharing one would let a later edit of
					// any row rewrite its siblings.
					Details: maps.Clone(refusals)})
			}
			continue
		}
		if d.Required {
			return Selection{}, (&model.Error{Code: model.CodeProviderUnavailable,
				Message: fmt.Sprintf("required provider %q is %s and no generation can be built without it", id, state)}).
				WithDetail("provider_id", id).WithDetail("diagnostic_code", code)
		}
		inactive[id] = inactiveProvider{state: state, code: code}
		for _, c := range d.Capabilities {
			sel.States = append(sel.States, model.CapabilityState{ProviderID: id, Capability: c, Scope: ScopeWorkspace, State: state, DiagnosticCode: code})
		}
	}
	return sel, nil
}

// refusalDetails keeps the detection details that name something the provider
// cannot do: those whose value is a Section 22 CTX_ code. It returns nil when
// none is left, so a detection that only carries markers publishes nothing at
// all rather than an empty degraded row.
func refusalDetails(details map[string]string) map[string]string {
	var out map[string]string
	for k, v := range details {
		if !strings.HasPrefix(v, "CTX_") {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(details))
		}
		out[k] = v
	}
	return out
}

// inactiveProvider records why a provider will not run, so a dependent can
// carry the same category and diagnostic code.
type inactiveProvider struct {
	state model.CapabilityStateValue
	code  string
}

// detect decides one provider. An empty state means active; otherwise the
// state the inactive capability rows carry. The diagnostic code and details are
// the detection's own either way: an active provider can still name the parts
// of itself that are unavailable.
func detect(ctx context.Context, p Provider, d model.ProviderDescriptor, mode config.Enablement, root workspace.Root, policy workspace.Policy,
	active map[string]bool, inactive map[string]inactiveProvider) (model.CapabilityStateValue, string, map[string]string, Detection) {
	if mode == config.Disabled {
		return model.CapabilityUnavailable, model.CodeProviderUnavailable, nil, Detection{}
	}
	for _, dep := range d.DependsOn {
		if active[dep] {
			continue
		}
		// The dependency's category and diagnostic propagate: an optional
		// provider waiting on a disabled tool is absent for that reason, one
		// waiting on a failed enabled tool has failed for the same reason.
		why := inactive[dep]
		return why.state, why.code, nil, Detection{}
	}
	det, err := p.Detect(ctx, root, policy)
	if err == nil {
		err = det.Validate(d)
	}
	if err != nil {
		return model.CapabilityFailed, CodeOf(err), nil, Detection{}
	}
	if det.Available {
		return "", det.DiagnosticCode, det.Details, det
	}
	// The per-input details belong to the available case alone: they are one
	// pair per language whose payload is missing, and an inactive provider
	// plans nothing for any of them. What does survive is the one phrase that
	// says why, because a bare diagnostic code names a category and not a
	// finding.
	why := unavailableReason(det)
	if mode == config.Auto {
		return model.CapabilityUnavailable, det.DiagnosticCode, why, Detection{}
	}
	return model.CapabilityFailed, det.DiagnosticCode, why, Detection{}
}

// unavailableReason is the detail map an inactive detection publishes: its own
// reason, or none at all when the provider gave none. It is one pair, so it
// cannot crowd out anything, and it is absent rather than empty so a reader
// never sees a reason key with nothing behind it.
func unavailableReason(det Detection) map[string]string {
	if det.Reason == "" {
		return nil
	}
	return map[string]string{"reason": model.TruncateDetail(det.Reason)}
}

// CodeOf extracts the Section 22 code an error carries for a diagnostic
// column: the typed code when there is one, CTX_CANCELED for a bare context
// end (a cancellation is never recorded as a provider crash) and CTX_INTERNAL
// otherwise. A nil error has no code.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.CodeCanceled
	}
	return model.CodeInternal
}
