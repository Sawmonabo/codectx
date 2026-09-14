// Package provider is the Section 11.1 provider runtime: the contract every
// fact producer implements, the registry that orders and selects providers,
// the byte- and record-bounded sink their output flows through, and the one
// place a unit is sealed or discarded.
//
// A provider emits small immutable units with explicit inputs, never a mutable
// global graph. The coordinator (internal/index) assigns scope, source view,
// reservations and dependencies; the sink stages bounded batches through
// storage's UnitWriter; storage validates and seals; only a sealed unit can be
// a generation member. Failed unsealed output is deleted and can never be
// queried. There is no plugin framework here and no second process
// abstraction: an external tool runs through internal/process.
package provider

import (
	"context"
	"maps"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// ScopeWorkspace is the capability scope the registry reports for a provider
// that produced no unit at all: disabled, unavailable or failed before it ran.
const ScopeWorkspace = "workspace"

// MaxDetectionInputs bounds Detection.InputPaths. Detection names the inputs a
// provider recognized (manifests, index files), not the repository's file
// list; a provider that recognizes more than this reports the bound, not the
// list.
const MaxDetectionInputs = model.MaxRecordsPerResult

// Detection is a provider's answer to "can you run against this workspace".
// Available false with a DiagnosticCode is the honest optional-absence
// outcome of Section 11.1: a missing or unapproved tool is unavailable, which
// is distinct from a failure. Detection must not walk the whole repository
// into memory; it inspects declared inputs through the confined Root.
type Detection struct {
	Available bool `json:"available"`
	// DiagnosticCode is the Section 22 code explaining an unavailable result,
	// for example CTX_TRUST_REQUIRED for an unapproved executable or
	// CTX_PROVIDER_UNAVAILABLE for a tool that is not installed.
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
	// Capabilities are the declared capabilities this provider can actually
	// offer here; a subset of its descriptor's list.
	Capabilities []string `json:"capabilities,omitempty"`
	// InputPaths are the root-relative inputs detection recognized, bounded by
	// MaxDetectionInputs.
	InputPaths []string `json:"input_paths,omitempty"`
	// ObservedVersion is the exact tool version detection observed for an
	// external analyzer (for example the output of a trusted `--version`
	// probe). It is empty for bundled providers. The coordinator folds it into
	// UnitSpec.ProviderVersion so units are keyed by the tool that actually
	// produced them; Descriptor() itself stays static.
	ObservedVersion string `json:"observed_version,omitempty"`
	// Details carry the machine-readable particulars of a detection that
	// DiagnosticCode cannot hold, as bounded key/value pairs. A provider that
	// covers several languages through several payloads is available as soon as
	// one of them resolves, and then has no other place to say that another
	// language's indexer is absent and why: it plans no unit for that language,
	// so no capability row is ever produced for it either. One pair per
	// affected input -- `details["rust-analyzer"] = "CTX_TOOL_OFFLINE"` --
	// keeps that typed rather than silent.
	Details map[string]string `json:"details,omitempty"`
}

// WithDetail returns the detection with one bounded diagnostic pair added, so
// a provider can build its answer in one expression. It is
// model.CapabilityState.WithDetail in shape and in bound: values are truncated
// to model.MaxDetailBytes, the map is capped at model.MaxCapabilityDetails
// entries, an empty key and an overflowing new key are both dropped, and the
// map is copied rather than written through, because a Detection is a value
// type a caller may already have copied.
func (d Detection) WithDetail(key, value string) Detection {
	if key == "" {
		return d
	}
	if _, replacing := d.Details[key]; !replacing && len(d.Details) >= model.MaxCapabilityDetails {
		return d
	}
	details := make(map[string]string, len(d.Details)+1)
	maps.Copy(details, d.Details)
	// The truncation is model's own. CapabilityState.WithDetail applies exactly
	// the bound Validate checks below, and the helper underneath it is not
	// exported, so borrowing the method keeps one implementation of the rule
	// rather than a second copy that can drift from the bound it must satisfy.
	details[key] = model.CapabilityState{}.WithDetail(key, value).Details[key]
	d.Details = details
	return d
}

// Validate enforces the detection shape against the provider's descriptor:
// bounded lists, a diagnostic code on every unavailable result and no
// capability the descriptor did not declare.
func (d Detection) Validate(desc model.ProviderDescriptor) error {
	if !d.Available && d.DiagnosticCode == "" {
		return outputInvalid(desc.ID, "an unavailable detection must carry a diagnostic code")
	}
	if len(d.DiagnosticCode) > model.MaxIdentifierBytes {
		return outputInvalid(desc.ID, "detection diagnostic code exceeds its bound")
	}
	if len(d.Capabilities) > model.MaxCapabilityStates {
		return outputInvalid(desc.ID, "detection lists more capabilities than the bound")
	}
	for _, c := range d.Capabilities {
		declared := false
		for _, want := range desc.Capabilities {
			if c == want {
				declared = true
				break
			}
		}
		if !declared {
			return outputInvalid(desc.ID, "detection claims a capability the descriptor does not declare").WithDetail("capability", c)
		}
	}
	if len(d.ObservedVersion) > model.MaxIdentifierBytes {
		return outputInvalid(desc.ID, "detection observed version exceeds its bound")
	}
	if len(d.Details) > model.MaxCapabilityDetails {
		return outputInvalid(desc.ID, "detection carries more details than the bound")
	}
	for k, v := range d.Details {
		if k == "" || len(k) > model.MaxIdentifierBytes || len(v) > model.MaxDetailBytes {
			return outputInvalid(desc.ID, "detection detail key or value is empty or exceeds its bound")
		}
	}
	if len(d.InputPaths) > MaxDetectionInputs {
		return outputInvalid(desc.ID, "detection lists more inputs than the bound")
	}
	for _, p := range d.InputPaths {
		if p == "" || len(p) > model.MaxPathBytes {
			return outputInvalid(desc.ID, "detection input path is empty or exceeds its bound")
		}
	}
	return nil
}

// UnitRequest is one assigned unit of work. Binding is the staging generation
// the unit is built for; Unit is the immutable identity storage has already
// opened; Content is the pinned snapshot view every byte must be read from;
// Resolver reconciles candidates against the unit's completed dependencies.
//
// Run is not in the Section 11.1 sketch. It is carried because every Evidence
// row must name the producing run (storage rejects evidence for another run)
// and ProviderResult.RunID must echo it; without it a provider could not build
// a single valid fact.
type UnitRequest struct {
	Binding  model.Binding
	Unit     model.UnitSpec
	Run      model.ProviderRunID
	Content  model.SnapshotView
	Resolver Resolver
}

// Resolver reconciles a provider candidate to canonical identity. The result
// depends only on the candidate and the set of sealed dependency units, never
// on scheduling (Section 9.4).
type Resolver interface {
	Resolve(context.Context, model.NodeCandidate) (model.Resolution, error)
}

// Sink receives a unit's facts. Ownership of a handed-off slice transfers to
// the sink: the provider must not retain or mutate it afterwards. A call
// blocks while the sink's retained-byte reservation is exhausted and returns
// promptly on cancellation; a single record over the configured limit is a
// CTX_RESOURCE_LIMIT failure, not a bypass.
//
// Hand a node fact to the sink before any relation, alias or search document
// that references its identity. Batch flush timing is not under the
// provider's control: a batch may be persisted at any call, including by
// another unit relieving pool pressure, and storage requires a relation's
// endpoints and an alias's target to be registered identities when the row
// is written.
type Sink interface {
	PutNodes(context.Context, []model.NodeFact) error
	PutRelations(context.Context, []model.RelationFact) error
	PutAliases(context.Context, []model.NativeAlias) error
	PutSearchUnits(context.Context, []model.SearchUnit) error
}

// Provider is one fact producer. Descriptor is static identity and
// scheduling contract; Detect inspects the workspace without indexing it;
// IndexUnit produces exactly the assigned unit through the sink.
//
// Detect takes the confined workspace root and traversal policy rather than
// the sketch's `workspace.View`, which does not exist: Root is the only safe
// way to open a repository file and Policy is what decides eligibility.
type Provider interface {
	Descriptor() model.ProviderDescriptor
	Detect(context.Context, workspace.Root, workspace.Policy) (Detection, error)
	IndexUnit(context.Context, UnitRequest, Sink) (model.ProviderResult, error)
}

func outputInvalid(providerID, msg string) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: msg, Details: map[string]string{"provider_id": providerID}}
}

func invalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}

func resourceLimit(msg string) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: msg}
}
