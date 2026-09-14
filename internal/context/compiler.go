// Package context compiles one immutable model.ContextManifest from a
// model.ContextRequest: seeds, required scope, integer ranking, budgeting,
// slices and the persisted manifest (Section 15). It deliberately shares its
// name with the standard library "context" package, which it imports unaliased
// below at file scope; consumers alias this package contextpkg.
//
// This file holds every symbol that crosses a lane boundary. The signatures
// here are frozen: a lane that needs a different one reports the need rather
// than changing it.
package context

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Options composes the compiler: Store pins the generation and persists the
// manifest, Search is the Section 15.2 seed resolver, Graph the Section 15.3
// expansion source, Config the Section 20.1 [context] bounds.
type Options struct {
	Store  *sqlite.Store
	Repo   model.RepositoryID
	Search *search.Service
	Graph  GraphFactory
	Config config.Config
	Now    func() time.Time
	Logger *slog.Logger
}

// GraphFactory opens a bounded engine over ONE explicit generation and returns
// the release that drops its lease. Workspace.Query (app/workspace.go:112)
// satisfies it exactly, so the sqlite Adjacency adapter (app/query.go:59) is
// reused, never duplicated here.
type GraphFactory func(ctx context.Context, gen model.GenerationID) (*graph.Engine, func() error, error)

// Compiler compiles deterministic context manifests. Safe for concurrent use.
type Compiler struct {
	store  *sqlite.Store
	repo   model.RepositoryID
	search *search.Service
	graph  GraphFactory
	cfg    config.Config
	now    func() time.Time
	log    *slog.Logger
}

// New validates the options and builds a Compiler. Every dependency is
// required: the composition root builds this eagerly when a workspace opens, so
// a missing one is a wiring defect that must fail there and not per compile,
// where it would look like a data problem.
func New(o Options) (*Compiler, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "context compiler New is not implemented"}
}

// Compile produces the immutable manifest for req. It pins one generation for
// the whole compile and passes that explicit generation into every downstream
// request, so an activation mid-compile can never split one manifest across two
// generations (Section 15.1). A timeout or cancellation returns an explicit
// incomplete answer and never persists a manifest.
func (c *Compiler) Compile(ctx context.Context, req model.ContextRequest) (model.ContextManifest, error) {
	return model.ContextManifest{}, &model.Error{Code: model.CodeInternal, Message: "context compiler Compile is not implemented"}
}

// Close releases the compiler's own resources. It does not close the injected
// Store or Search service, which the composition root owns.
func (c *Compiler) Close() error {
	return &model.Error{Code: model.CodeInternal, Message: "context compiler Close is not implemented"}
}

// candidate is the ONE intermediate record that crosses lane boundaries: seed
// extraction fills it, scope marks its requirement, rank scores it, budget
// sizes and packs it, manifest persists it. Nothing else is shared state.
type candidate struct {
	NodeID      model.NodeID
	FileID      model.FileID
	Path        string // normalized, root-relative
	Kind        model.NodeKind
	Requirement model.Requirement
	Origin      originKind // which Section 15.2 step produced it
	Depth       int        // 0 for a seed
	StartByte   int64
	ScoreMicros int64
	Boosts      int64
	SizeBytes   int64 // model.FileVersion.Size; 0 until sized
	Status      model.FileStatus
	Reasons     []string             // <= model.MaxReasonsPerEntry, each <= MaxReasonBytes
	Paths       []model.RelationPath // <= cfg.Context.MaxReasonPathsPerEntry
	MorePaths   int64                // routes not enumerated (Section 15.3)
	Excluded    string               // non-empty => excluded, with this reason
}

// originKind names the Section 15.2 seed step, in discovery order.
type originKind int

const (
	originExplicitSeed originKind = iota
	originBacktick
	originPathToken
	originQualified
	originExactResolve
	originLexical
	originChangedFile
	originExpansion
)

// less is the total Section 15.3 order: requirement rank, descending
// ScoreMicros, normalized path, start byte, entity id.
func (a candidate) less(b candidate) bool {
	if ra, rb := requirementRank(a.Requirement), requirementRank(b.Requirement); ra != rb {
		return ra < rb
	}
	if a.ScoreMicros != b.ScoreMicros {
		return a.ScoreMicros > b.ScoreMicros
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartByte != b.StartByte {
		return a.StartByte < b.StartByte
	}
	// The last key is unique, so the order is total. A node identity wins over
	// the file identity: a file-level candidate carries no NodeID.
	return a.entityID() < b.entityID()
}

// entityID is the final, unique tie-break key: the node identity when the
// candidate names one, else the file identity.
func (a candidate) entityID() string {
	if a.NodeID != "" {
		return string(a.NodeID)
	}
	return string(a.FileID)
}

// requirementRank orders the first tie-break key (full 0 ... optional 3). An
// unknown requirement sorts last rather than silently ranking as required.
func requirementRank(r model.Requirement) int {
	switch r {
	case model.RequirementFull:
		return 0
	case model.RequirementSymbol:
		return 1
	case model.RequirementRecommended:
		return 2
	case model.RequirementOptional:
		return 3
	}
	return 4
}

// scale is the Section 15.3 fixed point. No float exists in this package.
const scale int64 = 1_000_000

// mulScaled multiplies two scaled integers, rounding half away from zero, and
// reports overflow (Section 15.3 requires the check even though inputs bound
// the product at 4e12).
func mulScaled(a, b int64) (int64, error) {
	p := a * b
	if a != 0 && (p/a != b || (a == -1 && b == math.MinInt64)) {
		return 0, overflowErr(a, b)
	}
	half := scale / 2
	switch {
	case p >= 0 && p > math.MaxInt64-half:
		return 0, overflowErr(a, b)
	case p < 0 && p < math.MinInt64+half:
		return 0, overflowErr(a, b)
	case p >= 0:
		p += half
	default:
		p -= half
	}
	return p / scale, nil
}

// overflowErr reports a ranking arithmetic overflow as a defect, not a user
// input problem: the Section 15.3 weights bound every product well inside
// int64, so reaching this is a wiring error in a new weight.
func overflowErr(a, b int64) *model.Error {
	return (&model.Error{Code: model.CodeInternal, Message: "ranking arithmetic overflowed"}).
		WithDetail("a", fmt.Sprint(a)).WithDetail("b", fmt.Sprint(b))
}

// contributionWeight is the Section 15.3 relation contribution in scaled
// micros. Weights are compile-time constants, not config
// (internal/graph/cost.go:5-14 states why). A relation kind absent from this
// map contributes nothing and is not admitted to a path.
var contributionWeight = map[model.RelationKind]int64{
	model.RelImplements:       940_000,
	model.RelOverrides:        940_000,
	model.RelExtends:          940_000,
	model.RelTests:            920_000,
	model.RelCalls:            900_000,
	model.RelReads:            880_000,
	model.RelWrites:           880_000,
	model.RelDataFlowsTo:      880_000,
	model.RelControlDependsOn: 880_000,
	model.RelReferences:       780_000,
	model.RelExports:          780_000,
	model.RelDependsOn:        720_000,
	model.RelConfigures:       720_000,
	model.RelImports:          600_000,
	model.RelDocuments:        550_000,
	// Same package or module: containment, ownership and definition.
	model.RelContains: 350_000,
	model.RelOwns:     350_000,
	model.RelDefines:  350_000,
}

// precisionMultiplier is the Section 15.3 per-edge precision multiplier in
// scaled micros. Precision lives on Evidence, not Relation; an edge with no
// evidence row takes the heuristic multiplier.
var precisionMultiplier = map[model.Precision]int64{
	model.PrecisionCompiler:       1_000_000,
	model.PrecisionLanguageServer: 950_000,
	model.PrecisionStaticAnalysis: 900_000,
	model.PrecisionSyntax:         720_000,
	model.PrecisionHeuristic:      450_000,
}

const (
	seedContribution    int64 = 1_000_000
	exactContribution   int64 = 980_000
	depthDecay          int64 = 650_000
	boostTaskIdentifier int64 = 200_000
	boostActiveChange   int64 = 120_000
	boostAssociatedTest int64 = 100_000
	boostCentralityMax  int64 = 50_000
	maxBoostMicros      int64 = boostTaskIdentifier + boostActiveChange +
		boostAssociatedTest + boostCentralityMax
)

// compilerPolicyVersion is the frozen code-side ranking/budget policy label
// stored in every manifest header.
const compilerPolicyVersion = "codectx.context.v1"

// The model.H domains. Two lanes computing a different preimage would make the
// Section 15.1 immutable-manifest reuse lookup silently never hit, so the
// preimages are frozen here and nowhere else.
const (
	manifestIDDomain    = "codectx.manifest.id.v1"
	requestHashDomain   = "codectx.manifest.request.v1"
	canonicalHashDomain = "codectx.manifest.canonical.v1"
	hashFieldSep        = "\x00"
)

// serializedBytes is the ACTUAL canonical-JSON length of v. Section 15.4 puts
// headers, metadata, delimiters and escaping in the budget, so overhead is
// measured, never assumed.
func serializedBytes(v any) (int64, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, &model.Error{Code: model.CodeInternal, Message: "context entry metadata is not serializable: " + err.Error()}
	}
	return int64(len(raw)), nil
}

// wireEncodedBytes is the worst-case transport length of n source bytes:
// base64 is the Section 16.2 fallback encoding, so 4*ceil(n/3).
func wireEncodedBytes(n int64) (int64, error) {
	if n < 0 {
		return 0, argumentInvalid("source byte count must not be negative, got %d", n)
	}
	groups := n / 3
	if n%3 != 0 {
		groups++
	}
	if groups > math.MaxInt64/4 {
		return 0, &model.Error{Code: model.CodeInternal, Message: "wire-encoded size overflowed"}
	}
	return groups * 4, nil
}

// estimateTokens is the ONLY token estimate in this package; the stored label
// is model.EstimateMethodUTF8Bytes and nothing else.
func estimateTokens(bytes int64) (int64, error) {
	tokens, err := model.EstimateTokensUTF8Bytes(bytes)
	if err != nil {
		return 0, err
	}
	return tokens, nil
}

// minimumBudget builds the Section 15.4 typed floor error. Detail keys are
// frozen: min_bytes, min_estimated_tokens, min_slices, min_files, missing.
// Required scope is never demoted, truncated or relabelled to fit a budget, so
// an impossible budget is reported with its floor instead.
func minimumBudget(floorBytes, floorTokens int64, floorSlices, floorFiles int, missing []string) *model.Error {
	return (&model.Error{
		Code:    model.CodeMinimumBudget,
		Message: "the budget cannot hold the required scope",
	}).
		WithDetail("min_bytes", fmt.Sprint(floorBytes)).
		WithDetail("min_estimated_tokens", fmt.Sprint(floorTokens)).
		WithDetail("min_slices", fmt.Sprint(floorSlices)).
		WithDetail("min_files", fmt.Sprint(floorFiles)).
		WithDetail("missing", strings.Join(missing, ",")).
		WithRemediation("raise the budget to at least the reported floor, or narrow the task")
}

// scopeIncomplete reports that a required boundary could not be resolved at
// all. A merely truncated or partially stale expansion sets ScopeComplete=false
// on the manifest instead and is not an error.
func scopeIncomplete(detail string) *model.Error {
	return (&model.Error{
		Code:    model.CodeScopeIncomplete,
		Message: "a required scope boundary could not be resolved",
	}).WithDetail("boundary", detail)
}

// argumentInvalid reports a structurally invalid request or option.
func argumentInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

// contextErr classifies a failed call against ctx: a deadline and a
// cancellation are explicit, complete-in-themselves incomplete answers
// (Section 14.4, Section 22), never an internal defect. Any other error is
// returned unchanged so a typed *model.Error from a dependency survives.
func contextErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &model.Error{Code: model.CodeQueryDeadline, Message: "the context compile exceeded its query deadline", Retryable: true}
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return &model.Error{Code: model.CodeCanceled, Message: "the context compile was canceled"}
	}
	return err
}
