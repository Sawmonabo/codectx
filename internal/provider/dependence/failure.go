package dependence

import (
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The capabilities this provider publishes. They are the product's relation
// vocabulary (Section 9.2), never the engine's edge names.
const (
	CapabilityControlDependsOn = "control_depends_on"
	CapabilityDataFlowsTo      = "data_flows_to"
	CapabilityReads            = "reads"
	CapabilityWrites           = "writes"
	CapabilityCalls            = "calls"
)

// Capabilities is the descriptor's capability list in its published order.
var Capabilities = []string{CapabilityControlDependsOn, CapabilityDataFlowsTo, CapabilityReads, CapabilityWrites, CapabilityCalls}

// Detail rows carry what CapabilityState has no field for. The in-repo
// precedent is the unsupported-label row: a bounded, sorted set of extra
// capability rows whose name encodes the detail. They are only ever put in
// ProviderResult.Capabilities, which storage records as provenance;
// Detection.Capabilities is validated against the descriptor and must never
// carry one.
const (
	detailSkippedMethods = "skipped_methods:"
	detailSkippedMethod  = "skipped_method:"
	detailSubdivided     = "subdivided:"
	detailBackendFailure = "backend_failure:"
	detailUnknownLabel   = "unsupported_label:"
	detailUnknownUntrack = "unsupported_labels:untracked"
)

// MaxReportedSkips bounds the skipped-method names a result names one by one.
// The count is always exact; the names are a sample when a unit skips more
// than this, because a result list is bounded (Section 6) and a thousand
// generated constant tables are not a useful report.
const MaxReportedSkips = 32

// maxDetailRows bounds every detail row a result may carry together, leaving
// room for the five published capabilities and one overflow row under
// model.MaxCapabilityStates.
const maxDetailRows = model.MaxCapabilityStates - 5 - 1

// FailureClass is the typed reason a unit did not produce an admissible
// result. Section 11.6 requires each to be classified, never guessed: every
// one of them was reproduced against the real engine and is recorded in
// docs/research/10-round3-empirical.md Section 6.
type FailureClass string

const (
	// FailureNone is a step that produced a usable result.
	FailureNone FailureClass = ""
	// FailureMemory is heap exhaustion. The engine exits non-zero with an
	// out-of-memory error on stderr and writes no graph. It is the one class
	// that may be retried, and only once, at the machine-derived allocation.
	FailureMemory FailureClass = "memory"
	// FailureEngine is a deterministic analysis-pass crash, or a helper crash
	// the engine hides behind a zero exit and an empty graph. Neither is
	// retried: both reproduce. Sibling units are unaffected.
	FailureEngine FailureClass = "engine"
	// FailureTimeout is a step that exceeded the unit's deadline.
	FailureTimeout FailureClass = "timeout"
)

// code maps a class to its Section 22 error code.
func (c FailureClass) code() string {
	switch c {
	case FailureMemory:
		return model.CodeResourceLimit
	case FailureTimeout:
		return model.CodeProviderTimeout
	default:
		return model.CodeProviderOutputInvalid
	}
}

// failure builds the typed error for a classified unit failure. The bounded
// details are the figures Section 11.6 requires a failure to publish: for
// memory the cap that failed, the allocation the retry could have used and
// the unit's estimated requirement; for an engine crash the failing pass and
// exception class. They travel on model.Error, which is the one bounded
// structured channel a provider already has.
func failure(class FailureClass, scopeKey string, o Outcome, r Reservation) *model.Error {
	msg := "the dependence unit failed: "
	switch class {
	case FailureMemory:
		msg += "the analysis ran out of memory"
	case FailureTimeout:
		msg += "the analysis exceeded the unit deadline"
	default:
		msg += "the analysis backend crashed"
	}
	err := &model.Error{Code: class.code(), Message: msg, Details: map[string]string{
		"failure_class": string(class),
		"scope_key":     truncate(scopeKey, model.MaxIdentifierBytes),
		"exit_code":     strconv.Itoa(o.ExitCode),
	}}
	if o.Pass != "" {
		err = err.WithDetail("pass", truncate(o.Pass, model.MaxIdentifierBytes))
	}
	if o.Exception != "" {
		err = err.WithDetail("exception", truncate(o.Exception, model.MaxIdentifierBytes))
	}
	if class == FailureMemory {
		err = err.WithDetail("heap_cap_bytes", strconv.FormatInt(r.HeapCapBytes, 10)).
			WithDetail("estimated_bytes", strconv.FormatInt(r.ParseBytes(), 10))
		if o.PeakBytes > 0 {
			err = err.WithDetail("observed_peak_bytes", strconv.FormatInt(o.PeakBytes, 10))
		}
		if r.AllocationBytes > 0 {
			err = err.WithDetail("allocation_bytes", strconv.FormatInt(r.AllocationBytes, 10))
		}
	}
	return err
}

// publication is everything a succeeded unit publishes about how complete it
// is. A unit that ran whole and skipped nothing publishes five fresh rows and
// no detail.
type publication struct {
	// Skipped are the methods the engine declined to analyse for data flow,
	// and SkippedCount their exact number. They degrade data_flows_to only:
	// control dependence, reads, writes and calls do not come from that pass.
	Skipped      []string
	SkippedCount int
	// Subdivided names the unit that had to be split after a reproducible
	// crash, with the backend failure that forced it. Every capability of a
	// subdivided unit is partial: Section 11.6's honesty test is per
	// capability, and engine calls lose more than half their resolved targets
	// when a project is split.
	Subdivided    string
	BackendFailed string
	// UnknownLabels counts the export rows the importer recognised as real but
	// did not map. They are published as unavailable rows so a consumer sees
	// what this build cannot read.
	UnknownLabels map[string]int
}

// capabilities renders the publication as the result's capability list:
// the five published capabilities at the unit's scope, then the bounded,
// sorted detail rows.
func (p publication) capabilities(scopeKey string) []model.CapabilityState {
	out := make([]model.CapabilityState, 0, len(Capabilities)+8)
	for _, c := range Capabilities {
		state, code := model.CapabilityFresh, ""
		switch {
		case p.Subdivided != "":
			state, code = model.CapabilityPartial, model.CodeProviderOutputInvalid
		case c == CapabilityDataFlowsTo && p.SkippedCount > 0:
			state, code = model.CapabilityPartial, model.CodeResourceLimit
		}
		out = append(out, model.CapabilityState{ProviderID: ProviderID, Capability: c, Scope: scopeKey, State: state, DiagnosticCode: code})
	}
	var details []model.CapabilityState
	row := func(name string, state model.CapabilityStateValue) {
		details = append(details, model.CapabilityState{ProviderID: ProviderID,
			Capability: truncate(name, model.MaxIdentifierBytes), Scope: scopeKey, State: state})
	}
	if p.SkippedCount > 0 {
		row(detailSkippedMethods+strconv.Itoa(p.SkippedCount), model.CapabilityPartial)
		names := slices.Clone(p.Skipped)
		slices.Sort(names)
		names = slices.Compact(names)
		if len(names) > MaxReportedSkips {
			names = names[:MaxReportedSkips]
		}
		for _, n := range names {
			row(detailSkippedMethod+n, model.CapabilityPartial)
		}
	}
	if p.Subdivided != "" {
		row(detailSubdivided+p.Subdivided, model.CapabilityPartial)
		if p.BackendFailed != "" {
			row(detailBackendFailure+p.BackendFailed, model.CapabilityPartial)
		}
	}
	labels := make([]string, 0, len(p.UnknownLabels))
	var tracked int
	for l, n := range p.UnknownLabels {
		labels = append(labels, l)
		tracked += n
	}
	slices.Sort(labels)
	for _, l := range labels {
		row(detailUnknownLabel+l, model.CapabilityUnavailable)
	}
	if len(details) > maxDetailRows {
		details = details[:maxDetailRows]
		details = append(details, model.CapabilityState{ProviderID: ProviderID,
			Capability: detailUnknownUntrack, Scope: scopeKey, State: model.CapabilityUnavailable})
	}
	return append(out, details...)
}

// truncate bounds a diagnostic string on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
