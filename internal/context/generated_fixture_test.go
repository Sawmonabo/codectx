package context

import (
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// generated_fixture_test.go holds the ONE generated fixture of this package and
// the structural preconditions every proof built on it depends on.
//
// Why it exists: C-P3 measured that the shared seven-file fixture yields four
// wanted relations over seven candidates, so the min-sequence fold (L1), the
// node dedupe (L2) and the unlimited edge scan (C8) are not merely unproven
// against it -- they are structurally unreachable, and a mutation of any of
// them passes for a reason that says nothing about the mutation. A fixture that
// cannot discriminate is worse than no fixture, because its green run reads as
// evidence. So the generated fixture asserts its own discriminating power in
// TestTheGeneratedFixtureCanDiscriminateTheStreamedPasses below, and every
// proof that uses it fails loudly the day the topology stops producing the
// shape it claims.
//
// One fixture, reused: the parity rows and the heap proof all build from
// generatedSpecs / generatedScope rather than shaping a fixture each.

const (
	// generatedLeaves is the fan-out of the core. It sets the file count (one
	// per leaf plus the four structural files) and, with generatedKinds, the
	// relation count.
	generatedLeaves = 300
	// generatedChanged is how many leaves are published as captured
	// working-tree changes. It is deliberately larger than one so a step that
	// reads changed files can be asked for MORE THAN ONE PAGE of them, which
	// the shared fixture (exactly one change) cannot do.
	generatedChanged = 5
)

// generatedKinds are the relation kinds the fan-out repeats, drawn from model's
// own vocabulary. Each kind between the same pair is a DISTINCT relation id
// (model.NewRelationID hashes the kind), so the edge count is leaves*kinds and
// not leaves.
var generatedKinds = []model.RelationKind{
	model.RelCalls, model.RelReferences, model.RelReads, model.RelWrites,
	model.RelDependsOn, model.RelDataFlowsTo, model.RelControlDependsOn,
	model.RelOwns, model.RelImports, model.RelExports, model.RelBuilds,
	model.RelImplements, model.RelTests, model.RelDocuments, model.RelConfigures,
	model.RelDefines, model.RelContains,
}

// The four structural files. `Shared` is the entity two seeds reach by two
// routes of DIFFERENT length -- `RootA` calls it directly, `RootB` reaches it
// through `Mid` -- which is what makes the min-sequence fold and the node
// dedupe observable at all.
const (
	genRootA  = "internal/gen/root_a.go"
	genRootB  = "internal/gen/root_b.go"
	genMid    = "internal/gen/mid.go"
	genShared = "internal/gen/shared.go"
)

func genLeaf(i int) string { return fmt.Sprintf("docs/gen/leaf%03d.md", i) }

// generatedSpecs is the generated file table: the four structural files, then
// generatedLeaves leaves, of which the first generatedChanged are captured
// changes. Content is per-file distinct so every blob, content hash and search
// document is its own, and small so publishing three hundred of them costs
// milliseconds rather than the seconds a realistic body would.
func generatedSpecs() []fixtureFileSpec {
	out := []fixtureFileSpec{
		{genRootA, "RootA", model.NodeFunction, "package gen\n\nfunc RootA() {}\n", false},
		{genRootB, "RootB", model.NodeFunction, "package gen\n\nfunc RootB() {}\n", false},
		{genMid, "Mid", model.NodeFunction, "package gen\n\nfunc Mid() {}\n", false},
		{genShared, "Shared", model.NodeInterface, "package gen\n\ntype Shared interface{ S() }\n", false},
	}
	for i := range generatedLeaves {
		sym := fmt.Sprintf("Leaf%03d", i)
		// A document, not a function. boundaryRequirement (scope.go:77) makes a
		// function reached in one hop RequirementSymbol -- required, and so
		// undroppable -- and C-P3 measured that an all-required scope either
		// fits a budget or refuses it, which is why the two drop-named parity
		// rows drop nothing. A document is Recommended at one hop, so the
		// packer has something to drop and the drop ORDER becomes observable.
		out = append(out, fixtureFileSpec{genLeaf(i), sym, model.NodeDocument,
			fmt.Sprintf("# %s\n\nleaf %d of the generated fan-out.\n", sym, i), i < generatedChanged})
	}
	return out
}

// generatedScope is the edge table. Every leaf hangs off `RootA` once per kind,
// so the walk admits three hundred entities on routes and names
// generatedLeaves*len(generatedKinds) distinct relation ids -- far past
// model.MaxPageItems, which is what ruling C8's unlimited edge scan is about.
// The two routes to `Shared` are the fold's input.
func generatedScope(fx *contextFixture) []model.Relation {
	out := make([]model.Relation, 0, generatedLeaves*len(generatedKinds)+4)
	for i := range generatedLeaves {
		for _, k := range generatedKinds {
			out = append(out, fx.edge(genRootA, k, genLeaf(i)))
		}
	}
	out = append(out,
		fx.edge(genRootA, model.RelCalls, genShared),
		fx.edge(genRootB, model.RelCalls, genMid),
		fx.edge(genMid, model.RelCalls, genShared),
		fx.edge(genRootB, model.RelCalls, genRootA),
	)
	return out
}

// newGeneratedFixture publishes the generated snapshot through the SAME
// builder, provider run, unit writer and activation as the shared fixture.
func newGeneratedFixture(t *testing.T) *contextFixture {
	t.Helper()
	return newFixtureFrom(t, generatedSpecs(), generatedScope)
}

// generatedRequest is the request the parity rows and the structural
// precondition compile. It names both roots, so `Shared` is reached twice.
func generatedRequest(b model.Budget) model.ContextRequest {
	return model.ContextRequest{Task: "make `RootA` idempotent", Seeds: []string{"RootA", "RootB"},
		Phase: model.PhaseVerify, Budget: b}
}

// generatedDropBudget is the budget the drop rows compile under: bytes and
// slices far above the required scope so the only bound that bites is the file
// count, and a file count well under the admitted candidate set so the packer
// emits a drop stream over more than a hundred files.
var generatedDropBudget = model.Budget{MaxBytes: 1 << 24, MaxSlices: 4000, MaxFiles: 60}

// generatedFullBudget is the same budget with the file count lifted clear, so
// every admitted candidate survives to an entry.
var generatedFullBudget = model.Budget{MaxBytes: 1 << 24, MaxSlices: 4000, MaxFiles: 4000}

// TestTheGeneratedFixtureCanDiscriminateTheStreamedPasses asserts the fixture's
// OWN discriminating power, and is the reason this file exists.
//
// C-P3 ran six plan-table mutations against the shared seven-file fixture; four
// of them passed, and only temporary instrumentation revealed why: that scope
// yields four wanted relations over seven candidates admitted once each, so the
// min-sequence fold, the node dedupe, the drop order and the unlimited edge
// scan have no input to get wrong. A green mutation run over a fixture that
// cannot discriminate reads as evidence and is not any.
//
// So the structural properties every proof on this fixture depends on are
// asserted here, at measured floors, and a topology change that quietly takes
// one of them away fails THIS test by name instead of silently turning some
// other test's mutation run green.
func TestTheGeneratedFixtureCanDiscriminateTheStreamedPasses(t *testing.T) {
	fx := newGeneratedFixture(t)
	if got := len(fx.Files); got < 300 {
		t.Errorf("the generated snapshot publishes %d files, want at least 300", got)
	}
	if got := len(fx.Rels); got < 5000 {
		t.Errorf("the generated scope declares %d relations, want at least 5000", got)
	}

	// (a) Every admitted candidate survives to an entry, and the routes those
	//     entries carry name far more than one page of distinct relations --
	//     model.MaxPageItems is 200. A relation scan that stopped at one page
	//     would leave routes untyped here; over the shared fixture's four
	//     wanted relations it could not.
	full := compileGenerated(t, fx, generatedFullBudget)
	if len(full.entries) < 300 {
		t.Errorf("the unbounded compile produced %d entries, want at least 300; the walk is not being read to "+
			"exhaustion, or the fan-out is not reaching the packer", len(full.entries))
	}
	if len(full.wanted) < 250 {
		t.Errorf("the entries name %d distinct relations on retained routes, want at least 250; "+
			"below this the unlimited edge scan of ruling C8 has no input that one page would truncate",
			len(full.wanted))
	}
	if len(full.reasons) < 2 {
		t.Errorf("the entries carry %d distinct reasons, want at least 2 so a reason ORDER is observable",
			len(full.reasons))
	}

	// (b) Two seeds reach `Shared` by routes of different length, so the scope
	//     dedupe sees one entity twice with different payloads. That is the
	//     min-sequence fold's only input: two arrivals at equal depth fold to
	//     the same record whichever side survives.
	if !full.byNode[fx.Node(genShared)] {
		t.Errorf("the entity both seeds reach is not in the plan, so the arrival fold has no input")
	}

	// (c) A file budget the required scope clears but the recommended fan-out
	//     does not, so the packer emits a drop stream over more than a hundred
	//     files and the ORDER those drops are persisted in is decided by rank
	//     rather than by file.
	dropped := compileGenerated(t, fx, generatedDropBudget)
	if len(dropped.excluded) < 100 {
		t.Errorf("the drop budget produced %d exclusions, want at least 100; "+
			"C-P3 measured that the two drop-named rows of the shared fixture drop nothing at all",
			len(dropped.excluded))
	}
	if !outOfFileOrder(dropped.excluded) {
		t.Errorf("every exclusion is in file order, so a drop stream emitted in file order instead of " +
			"in rank order would produce this same table and the C1 ordinal order is unproven")
	}
}

// compiled is what a generated compile exposes to a structural assertion.
type compiled struct {
	entries  []model.ContextEntry
	excluded []model.ExcludedContextEntry
	wanted   map[model.RelationID]bool
	reasons  map[string]bool
	byNode   map[model.NodeID]bool
}

// compileGenerated compiles the generated fixture through the production
// Compile and reads the persisted manifest back, which is the only place the
// structural properties above are observable without a production accessor.
func compileGenerated(t *testing.T, fx *contextFixture, b model.Budget) compiled {
	t.Helper()
	m, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, generatedRequest(b))
	if err != nil {
		t.Fatalf("Compile(budget %+v): %v", b, err)
	}
	out := compiled{wanted: map[model.RelationID]bool{}, reasons: map[string]bool{},
		byNode: map[model.NodeID]bool{}}
	out.entries, _, out.excluded = readBackManifest(t, fx, m.ID)
	for _, e := range out.entries {
		out.byNode[e.NodeID] = true
		for _, r := range e.Reasons {
			out.reasons[r] = true
		}
		for _, p := range e.EvidencePaths {
			for _, id := range p {
				out.wanted[id] = true
			}
		}
	}
	return out
}

// outOfFileOrder reports whether the persisted exclusions are in some order
// other than their files'. The exclusions are read back in the order the packer
// wrote them, so a single inversion is proof the order is the drop rank's and
// not the file table's.
func outOfFileOrder(rows []model.ExcludedContextEntry) bool {
	prev := ""
	for _, r := range rows {
		p := r.Reference.Path
		if p == "" {
			continue
		}
		if p < prev {
			return true
		}
		prev = p
	}
	return false
}
