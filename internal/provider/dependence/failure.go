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

// CapabilityUnsupportedLabels is the pseudo-capability under which a unit
// reports export rows the importer recognised as real but did not map. It is
// the one row that is not a product relation: it says which shapes of fact this
// build cannot read at all, which is a statement about the build rather than
// about the repository. It is only ever put in ProviderResult.Capabilities,
// which storage records as provenance; Detection.Capabilities is validated
// against the descriptor and must never carry it.
const CapabilityUnsupportedLabels = "unsupported_labels"

// detailUntrackedLabels is the reserved detail key under which the
// unsupported-label row reports how many distinct labels did not fit in its
// bounded detail map. It is a count of labels, not one of them.
const detailUntrackedLabels = "untracked_labels"

// MaxReportedSkips bounds the skipped-method names a result names one by one.
// The count is always exact; the names are a sample when a unit skips more
// than this, because a result list is bounded (Section 6) and a thousand
// generated constant tables are not a useful report.
const MaxReportedSkips = 32

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
	// The observed peak is the tree's, sampled while it ran: what the failed
	// unit actually used, against what it was admitted for. It is reported for
	// every class, because a timeout or a crash at the memory ceiling reads
	// very differently from one with headroom left. Where the platform cannot
	// sample it the figure is simply absent; a zero would claim the tree used
	// no memory.
	if !o.PeakUnsampled && o.PeakBytes > 0 {
		err = err.WithDetail("observed_peak_bytes", strconv.FormatInt(o.PeakBytes, 10))
	}
	if class == FailureMemory {
		err = err.WithDetail("heap_cap_bytes", strconv.FormatInt(r.HeapCapBytes, 10)).
			WithDetail("estimated_bytes", strconv.FormatInt(r.ParseBytes(), 10))
		if o.PeakUnsampled {
			// A memory failure whose observed figure is missing must say why,
			// or the absent detail reads as "the tree used nothing".
			err = err.WithDetail("observed_peak_bytes_unavailable", "this platform does not sample process-tree memory")
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
	// UnplannedProjects is how many projects of this unit's family the planner
	// refused a unit of their own for an oversized scope key. Their files fall
	// back to the unit enclosing them, so nothing goes unanalysed, but the
	// analysis ran at a coarser project boundary than the source owns — which
	// degrades every capability of the family alike, not one pass.
	UnplannedProjects int
	// Family is the unit's language family, carried so the two user-set
	// threshold reports below can name it: a capability row carries a scope
	// key, and the project count they report is the family's, not the scope's.
	Family Family
	// OverUnitsPerFamily is the project count that crossed a user-set
	// providers.dependence.max_units_per_family (0 when it did not, or when
	// the user set no threshold), and UnitsPerFamilyBound that threshold.
	// Nothing was refused or dropped: every project has its unit and every
	// part of a subdivided unit was analysed. The row says so because the user
	// asked to be told, which is the only reason these two keys exist.
	OverUnitsPerFamily  int
	UnitsPerFamilyBound int64
	// StagedRows is the import's staged row count and StagedRowsBound the
	// user-set providers.dependence.max_staged_rows it crossed; both are zero
	// unless it crossed. The import staged and published every row regardless.
	StagedRows      int64
	StagedRowsBound int64
	// DerivedRows is the count of relation occurrences the import projected
	// and DerivedRowsBound the user-set providers.dependence.max_derived_rows
	// it crossed; both are zero unless it crossed. The projection was neither
	// truncated nor refused.
	DerivedRows      int64
	DerivedRowsBound int64
	// TruncatedFields counts, by field name, the descriptive storage values
	// the import cut to their model ceiling before writing them. The facts
	// were published whole apart from those values; the map is what keeps the
	// clipping from being silent, and it is bounded by the fixed set of
	// descriptive field names, never by the repository.
	TruncatedFields map[string]int
	// ClippedEvidence counts the evidence occurrences the import removed from
	// facts it published, under the user's own index.max_evidence_per_fact.
	// The key's contract (internal/config/config.go) is that "the cut is
	// reported on the unit's capability detail, never silent", so this is what
	// carries it to the generation.
	ClippedEvidence int
}

// capabilities renders the publication as the result's capability list: the
// five published capabilities at the unit's scope, each carrying the bounded
// details of why it is not fresh, plus one unavailable row when the importer
// met labels it does not map.
//
// The details are a fixed, small set of keys — a count of skipped methods and
// their names, the subdivided unit and its backend failure, the count of
// projects the planner could not admit — so a row's detail map is bounded by
// this code rather than by the repository.
//
// The particulars travel in CapabilityState.Details. They are not encoded into
// extra capability rows whose name is the detail text: a row per skipped method
// grows the list with the repository, it spends the MaxCapabilityStates budget
// on prose, and it claims a capability identity for something that is not a
// capability.
func (p publication) capabilities(scopeKey string) []model.CapabilityState {
	out := make([]model.CapabilityState, 0, len(Capabilities)+1)
	for _, c := range Capabilities {
		row := model.CapabilityState{ProviderID: ProviderID, Capability: c, Scope: scopeKey, State: model.CapabilityFresh}
		// A subdivided unit degrades every capability; skipped methods degrade
		// only the pass they come from. Both can hold at once, and the row then
		// carries both reasons.
		if c == CapabilityDataFlowsTo && p.SkippedCount > 0 {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeResourceLimit
			row = row.WithDetail("skipped_methods", strconv.Itoa(p.SkippedCount))
			if names := p.skippedNames(); names != "" {
				row = row.WithDetail("skipped_method_names", names)
			}
		}
		// Source analysed under a project boundary that is not its own
		// degrades every capability of the family alike: a resolution that
		// depends on the project's extent is affected in every pass.
		if p.UnplannedProjects > 0 {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeProviderOutputInvalid
			row = row.WithDetail("unplanned_projects", strconv.Itoa(p.UnplannedProjects))
		}
		// Neither threshold below lost anything: they are the user's own
		// reporting bounds on a count that belongs to the repository. The row
		// is marked partial with CodeResourceLimit all the same, because a
		// fresh row's Details are nil'd on the way into the generation
		// (internal/index/status.go) and a report nobody can read is the
		// silence the posture forbids as squarely as a refusal.
		if p.OverUnitsPerFamily > 0 {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeResourceLimit
			row = row.WithDetail("family", string(p.Family)).
				WithDetail("units_per_family", strconv.Itoa(p.OverUnitsPerFamily)).
				WithDetail("max_units_per_family", strconv.FormatInt(p.UnitsPerFamilyBound, 10))
		}
		if p.StagedRows > 0 {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeResourceLimit
			row = row.WithDetail("staged_rows", strconv.FormatInt(p.StagedRows, 10)).
				WithDetail("max_staged_rows", strconv.FormatInt(p.StagedRowsBound, 10))
		}
		if p.DerivedRows > 0 {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeResourceLimit
			row = row.WithDetail("derived_rows", strconv.FormatInt(p.DerivedRows, 10)).
				WithDetail("max_derived_rows", strconv.FormatInt(p.DerivedRowsBound, 10))
		}
		// A clipped stored value degrades every capability alike: any pass can
		// be the one that published the fact whose name or signature was cut.
		if names := p.truncatedFields(); names != "" {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeResourceLimit
			row = row.WithDetail("truncated_fields", names)
		}
		// Evidence removed from a published fact degrades every capability
		// alike: any pass can be the one whose fact lost occurrences, and a
		// consumer counting occurrences reads a number the clip decided.
		if p.ClippedEvidence > 0 {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeResourceLimit
			row = row.WithDetail(model.DetailEvidenceClipped, strconv.Itoa(p.ClippedEvidence))
		}
		if p.Subdivided != "" {
			row.State, row.DiagnosticCode = model.CapabilityPartial, model.CodeProviderOutputInvalid
			row = row.WithDetail("subdivided", truncate(p.Subdivided, model.MaxDetailBytes))
			if p.BackendFailed != "" {
				row = row.WithDetail("backend_failure", truncate(p.BackendFailed, model.MaxDetailBytes))
			}
		}
		out = append(out, row)
	}
	if len(p.UnknownLabels) > 0 {
		out = append(out, p.unsupportedLabels(scopeKey))
	}
	return out
}

// truncatedFields renders the cut storage fields as one detail value,
// "field=count" in field order, empty when nothing was cut. The field names
// are a fixed set, so the value is bounded by this code; the ceiling is
// honoured all the same, on a separator boundary, because a value clipped
// mid-pair would report a count nobody measured.
func (p publication) truncatedFields() string {
	fields := make([]string, 0, len(p.TruncatedFields))
	for f, n := range p.TruncatedFields {
		if n > 0 {
			fields = append(fields, f)
		}
	}
	slices.Sort(fields)
	var b strings.Builder
	for _, f := range fields {
		pair := f + "=" + strconv.Itoa(p.TruncatedFields[f])
		sep := 0
		if b.Len() > 0 {
			sep = 1
		}
		if b.Len()+sep+len(pair) > model.MaxDetailBytes {
			break
		}
		if sep > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pair)
	}
	return b.String()
}

// skippedNames is the sorted, deduplicated sample of skipped method names as
// one detail value. Names are added whole: a value truncated mid-name would
// name a method that does not exist.
func (p publication) skippedNames() string {
	names := slices.Clone(p.Skipped)
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > MaxReportedSkips {
		names = names[:MaxReportedSkips]
	}
	var b strings.Builder
	for _, n := range names {
		sep := 0
		if b.Len() > 0 {
			sep = 1
		}
		if b.Len()+sep+len(n) > model.MaxDetailBytes {
			break
		}
		if sep > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
	}
	return b.String()
}

// unsupportedLabels is the one row for every label this build did not map,
// counted by label. The map is bounded, so the labels are reported in
// descending count order — the ones costing the most facts — and the number
// that did not fit is reported under detailUntrackedLabels rather than
// silently dropped.
func (p publication) unsupportedLabels(scopeKey string) model.CapabilityState {
	labels := make([]string, 0, len(p.UnknownLabels))
	for l := range p.UnknownLabels {
		labels = append(labels, l)
	}
	slices.SortFunc(labels, func(a, b string) int {
		if d := p.UnknownLabels[b] - p.UnknownLabels[a]; d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	row := model.CapabilityState{ProviderID: ProviderID, Capability: CapabilityUnsupportedLabels,
		Scope: scopeKey, State: model.CapabilityUnavailable, DiagnosticCode: model.CodeProviderOutputInvalid}
	// One slot is kept for the overflow count, so a long tail of labels is
	// reported as a number instead of pushing the count out of the map.
	named := min(len(labels), model.MaxCapabilityDetails-1)
	for _, l := range labels[:named] {
		row = row.WithDetail(truncate(l, model.MaxIdentifierBytes), strconv.Itoa(p.UnknownLabels[l]))
	}
	if rest := len(labels) - named; rest > 0 {
		row = row.WithDetail(detailUntrackedLabels, strconv.Itoa(rest))
	}
	return row
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

// observedPeak is the analyzer tree's sampled peak, or zero when the platform
// does not sample it. Zero means unobserved, never "the tree used nothing", so
// every consumer of the figure must treat it as absent rather than as small.
func observedPeak(o Outcome) int64 {
	if o.PeakUnsampled {
		return 0
	}
	return o.PeakBytes
}

// peakForLog renders the sampled tree peak for a log line: the figure, or the
// word that says it was never observed. A log that printed 0 for an unsampled
// platform would be read as a measurement.
func peakForLog(o Outcome) any {
	if o.PeakUnsampled {
		return "unsampled"
	}
	return o.PeakBytes
}
