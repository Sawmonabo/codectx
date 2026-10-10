package context

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// seedbudget_test.go proves the lexical read's stop (ADR-0016 decisions 1-5)
// end to end: a compile whose read stops at its floor against the same compile
// reading every match (Compiler.fullRead), each on a store of its own, because
// fullRead is not part of the manifest identity and a second compile on one
// store would reuse the first plan.
//
// No test names a search score. Every node's lexical document is its symbol,
// and the task has four words, so how many of them a symbol holds sets the
// level its score falls in, whatever the index computes:
//
//   - seedHead holds all four: the top level, every such match tied.
//   - seedHalf holds the two rarer ones, refund and ledger: about half the top.
//   - seedTail holds only audit, the word most matches share: about a tenth.
//
// lexicalTau is 1.27 times a contribution, so a level below about 0.79 of the
// floor's is never read once the floor is known; each fixture's comment says
// which levels its read takes and where it stops. A symbol with a further word
// (seedHead + "_memo") scores a little below its level.
const seedTask = "refund ledger rounding audit"

const (
	seedHead = "refund_ledger_rounding_audit"
	seedHalf = "refund_ledger"
	seedTail = "audit"
)

// seedFile is one function node of a fixture file. The builder takes a file's
// content from its first row, so a further node of the same file passes none.
func seedFile(path, symbol string) fixtureFileSpec {
	return fixtureFileSpec{path, symbol, model.NodeFunction,
		fmt.Sprintf("package pay\n\n// %s\nfunc f() {}\n", path), false}
}

func seedNode(path, symbol string) fixtureFileSpec {
	return fixtureFileSpec{path, symbol, model.NodeFunction, "", false}
}

// The closure fixture. Three files hold a top-level match, so with a file
// budget of 2 the third is the F+1-th witness, the floor is the top level, and
// the read stops at the first tail match: Y, U and Z are never read. The pack
// keeps a.go and b.go.
//
//   - Z is a second node of the kept a.go whose symbol matches the task
//     (audit) below the stop. Only the kept-file read admits it.
//   - X is a second node of the kept b.go that matches nothing; the one edge
//     that reaches it is `Y calls X`, from the unread match Y outside the kept
//     files. Only the closure walk admits it.
//   - U is an unread match outside the kept files that calls V; both are in the
//     kept files' package, so U's walk crosses an edge there.
const (
	closureA = "internal/pay/a.go"
	closureB = "internal/pay/b.go"
	closureC = "internal/pay/c.go"
	closureY = "internal/pay/y.go"
	closureU = "internal/pay/u.go"
	closureV = "internal/pay/v.go"

	closureZ = "audit_memo_note"
	closureX = "settle"
	closureW = "post"
)

func closureSpecs() []fixtureFileSpec {
	return []fixtureFileSpec{
		seedFile(closureA, seedHead), seedNode(closureA, closureZ),
		seedFile(closureB, seedHead), seedNode(closureB, closureX),
		seedFile(closureC, seedHead),
		seedFile(closureY, seedTail),
		seedFile(closureU, seedTail),
		seedFile(closureV, closureW),
	}
}

func closureScope(fx *contextFixture) []model.Relation {
	return []model.Relation{
		fx.nodeEdge(fx.NodeOf(closureY, seedTail), model.RelCalls, fx.NodeOf(closureB, closureX)),
		fx.nodeEdge(fx.NodeOf(closureU, seedTail), model.RelCalls, fx.NodeOf(closureV, closureW)),
	}
}

// seedRequest is the task under a file budget, nothing else bounded.
func seedRequest(files int) model.ContextRequest {
	return model.ContextRequest{Task: seedTask, Phase: model.PhaseVerify, Budget: model.Budget{MaxFiles: files}}
}

// seedPlan is one compiled manifest read back whole, with the stop each of its
// rounds ended at when the compile was driven round by round.
type seedPlan struct {
	fx       *contextFixture
	m        model.ContextManifest
	entries  []model.ContextEntry
	slices   []model.ContextSlice
	excluded []model.ExcludedContextEntry
	// stops is every round's stop, the last one after the round decision,
	// so it carries the reason a stop could not be confirmed. Empty for a
	// compile that read every match.
	stops []seedStop
}

// final is the stop the plan was packed under.
func (p seedPlan) final(t *testing.T) seedStop {
	t.Helper()
	if len(p.stops) == 0 {
		t.Fatal("the plan was compiled without its rounds")
	}
	return p.stops[len(p.stops)-1]
}

// compileStopped compiles req on a store of its own with the production read,
// first round by round for the stops, then through Compile for the manifest.
// tune, when set, adjusts the configuration before the compiler is built.
func compileStopped(t *testing.T, specs []fixtureFileSpec, rels func(*contextFixture) []model.Relation,
	req model.ContextRequest, tune func(*config.Config)) seedPlan {
	t.Helper()
	fx := newFixtureFrom(t, specs, rels)
	if tune != nil {
		tune(&fx.Cfg)
	}
	c := intCompiler(t, fx, fx.Now)
	p := compileOn(t, fx, c, req)
	p.stops = roundStops(t, fx, c, req)
	return p
}

// compileFull is compileStopped reading every lexical match: the plan the stop
// is proved against.
func compileFull(t *testing.T, specs []fixtureFileSpec, rels func(*contextFixture) []model.Relation,
	req model.ContextRequest) seedPlan {
	t.Helper()
	fx := newFixtureFrom(t, specs, rels)
	c := intCompiler(t, fx, fx.Now)
	c.fullRead = true
	return compileOn(t, fx, c, req)
}

func compileOn(t *testing.T, fx *contextFixture, c *Compiler, req model.ContextRequest) seedPlan {
	t.Helper()
	m, err := c.Compile(fx.ctx, req)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	entries, slices, excluded := readBackManifest(t, fx, m.ID)
	return seedPlan{fx: fx, m: m, entries: entries, slices: slices, excluded: excluded}
}

// roundStops runs req's rounds exactly as Compile does -- every pass of a
// round, then nextRound, until it answers done -- and answers each round's
// stop. It persists nothing, so the Compile beside it still compiles.
//
// A round either raises the witness count or changes the kept files, and
// nextRound refuses a round that repeats an earlier one, so the rounds are
// bounded by the fixture's files; more than that is reported, not waited on.
func roundStops(t *testing.T, fx *contextFixture, c *Compiler, req model.ContextRequest) []seedStop {
	t.Helper()
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, 0, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	sorts := openSorts(t, c.cfg)
	resolved, err := resolveBudget(req.Budget, c.cfg.Context)
	if err != nil {
		t.Fatalf("resolveBudget: %v", err)
	}
	st := &compileState{pass: passIngest}
	var stops []seedStop
	for {
		if err := c.runPasses(fx.ctx, reader, reader.Binding().GenerationID, req, sorts, resolved, st,
			func(int) bool { return false }); err != nil {
			t.Fatalf("round %d: %v", st.round, err)
		}
		next, done, err := nextRound(fx.ctx, st)
		if err != nil {
			t.Fatalf("the decision after round %d: %v", st.round, err)
		}
		stops = append(stops, st.scope.Stop)
		if done {
			return stops
		}
		if len(stops) > len(fx.Files)+2 {
			t.Fatalf("the compile ran %d rounds over %d files without ending", len(stops), len(fx.Files))
		}
		st.beginRound(next)
	}
}

// stoppedNotice reports whether the manifest says its lexical read stopped.
func stoppedNotice(m model.ContextManifest) bool {
	for _, n := range m.Notices {
		if strings.HasPrefix(n, "Lexical matches scoring below") {
			return true
		}
	}
	return false
}

// aboveStop reports whether an entry's rank key is above (recommended, tau),
// the part of a plan ADR-0016 decision 6 states exactly.
func aboveStop(e model.ContextEntry, tau int64) bool {
	rank, rec := requirementRank(e.Requirement), requirementRank(model.RequirementRecommended)
	return rank < rec || (rank == rec && e.ScoreMicros > tau)
}

// headRow is one entry of the ranked head: its place, its identity and its
// rank key.
type headRow struct {
	Ordinal     int               `json:"ordinal"`
	NodeID      model.NodeID      `json:"node,omitempty"`
	FileID      model.FileID      `json:"file,omitempty"`
	Requirement model.Requirement `json:"requirement"`
	Score       int64             `json:"score"`
}

// headOf is the plan's entries above (recommended, tau), in ordinal order. The
// order is the rank order, so they are the plan's prefix.
func headOf(entries []model.ContextEntry, tau int64) []headRow {
	var out []headRow
	for _, e := range entries {
		if aboveStop(e, tau) {
			out = append(out, headRow{Ordinal: e.Ordinal, NodeID: e.NodeID, FileID: e.FileID,
				Requirement: e.Requirement, Score: e.ScoreMicros})
		}
	}
	return out
}

// packedEntities is every packed file with the entities packed in it, sorted.
func packedEntities(entries []model.ContextEntry) map[model.FileID][]string {
	out := map[model.FileID][]string{}
	for _, e := range entries {
		key := string(e.NodeID)
		if key == "" {
			key = "file " + string(e.FileID)
		}
		out[e.FileID] = append(out[e.FileID], key)
	}
	for _, keys := range out {
		sort.Strings(keys)
	}
	return out
}

// assertTheFullReadAboveTheStop is the differential of ADR-0016 decision 7:
// the stopped plan's ranked head above (recommended, tau), its packed file set
// and each packed file's entries equal the full read's.
func assertTheFullReadAboveTheStop(t *testing.T, stopped seedPlan, full seedPlan) {
	t.Helper()
	stop := stopped.final(t)
	if !stop.Stopped || !stoppedNotice(stopped.m) {
		t.Fatalf("the compile read every match (notices %q); the fixture no longer stops, so nothing is compared",
			stopped.m.Notices)
	}
	if stop.Unconfirmed != "" {
		t.Fatalf("the stop was not confirmed: %s", stop.Unconfirmed)
	}
	if stoppedNotice(full.m) {
		t.Fatalf("the full read stopped (notices %q)", full.m.Notices)
	}
	if len(headOf(full.entries, stop.Tau)) == 0 {
		t.Fatalf("the full read packs nothing above the stop (tau %d); the head comparison is vacuous", stop.Tau)
	}
	sameJSON(t, "the ranked head above the stop", headOf(full.entries, stop.Tau), headOf(stopped.entries, stop.Tau))
	sameJSON(t, "the packed files and the entries inside each", packedEntities(full.entries),
		packedEntities(stopped.entries))
	if stopped.m.ScopeComplete != full.m.ScopeComplete {
		t.Errorf("the stopped plan's scope_complete is %v, the full read's %v", stopped.m.ScopeComplete,
			full.m.ScopeComplete)
	}
}

// entryOf finds the plan's entry for node.
func entryOf(entries []model.ContextEntry, node model.NodeID) (model.ContextEntry, bool) {
	for _, e := range entries {
		if e.NodeID == node {
			return e, true
		}
	}
	return model.ContextEntry{}, false
}

// excludes reports whether the plan's exclusions name node.
func excludes(rows []model.ExcludedContextEntry, node model.NodeID) bool {
	for _, r := range rows {
		if r.Reference.NodeID == node {
			return true
		}
	}
	return false
}

// TestEveryMatchAndWalkEntryInsideAKeptFileIsPacked protects ADR-0016
// decision 5: a stopped read still packs every lexical match and every walk
// entry inside the files it packs.
//
// On the closure fixture the read stops at the first tail match, so neither Z
// (a match inside the kept a.go, below the stop) nor Y (the only node with an
// edge to X, inside the kept b.go) is read. Z reaches the plan only through
// the kept-file read, and X only through the closure walk from Y.
//
// Mutations it catches: skip the kept-file read (Z is missing), and skip the
// closure walk (X is missing). Each is its own assertion.
func TestEveryMatchAndWalkEntryInsideAKeptFileIsPacked(t *testing.T) {
	p := compileStopped(t, closureSpecs(), closureScope, seedRequest(2), nil)
	stop := p.final(t)
	if !stop.Stopped || !stoppedNotice(p.m) {
		t.Fatalf("the read did not stop (notices %q); Z and Y would be read by the main read", p.m.Notices)
	}
	y := p.fx.NodeOf(closureY, seedTail)
	if _, ok := entryOf(p.entries, y); ok || excludes(p.excluded, y) {
		t.Fatalf("Y is in the plan or its exclusions, so the main read reached it; X is not reachable only through the closure")
	}
	packed := packedEntities(p.entries)
	for _, f := range []string{closureA, closureB} {
		if _, ok := packed[p.fx.File(f).ID]; !ok {
			t.Fatalf("%s is not packed; the fixture's kept files are a.go and b.go, packed %v", f, packed)
		}
	}

	z, ok := entryOf(p.entries, p.fx.NodeOf(closureA, closureZ))
	if !ok {
		t.Errorf("Z, a match inside the kept %s scoring below the stop, is not in the plan: the kept-file read did not run",
			closureA)
	} else {
		inside := false
		for _, r := range z.Reasons {
			if strings.Contains(r, "inside a file the plan keeps") {
				inside = true
			}
		}
		if !inside {
			t.Errorf("Z is in the plan without the kept-file read's reason (%q); the main read reached it and the row proves nothing",
				z.Reasons)
		}
	}
	x, ok := entryOf(p.entries, p.fx.NodeOf(closureB, closureX))
	if !ok {
		t.Errorf("X, inside the kept %s and reached only from the unread match Y, is not in the plan: the closure walk did not run",
			closureB)
	} else if isRequired(x.Requirement) {
		t.Errorf("X is %s; an entry the closure walk reaches from a lexical match is at most %s",
			x.Requirement, model.RequirementRecommended)
	}
}

// TestAnUnreadMatchsWalkLeavesTheKeptEntriesScoresAlone protects ADR-0016
// decision 2: centrality counts only the edges named roots' walks cross, so a
// match the read never reached cannot move the boost of an entry above the
// stop.
//
// On the closure fixture the unread match U calls V, an edge in the package of
// the kept heads. The full read walks it; the stopped read does not. With
// centrality from named roots only (and the task names none) the head is the
// same in both plans.
//
// Mutation it catches: centrality from every root. The full read then counts
// U's edge in internal/pay, every head entry there gains a boost the stopped
// plan does not give it, and the heads differ.
func TestAnUnreadMatchsWalkLeavesTheKeptEntriesScoresAlone(t *testing.T) {
	stopped := compileStopped(t, closureSpecs(), closureScope, seedRequest(2), nil)
	full := compileFull(t, closureSpecs(), closureScope, seedRequest(2))
	u, v := stopped.fx.NodeOf(closureU, seedTail), stopped.fx.NodeOf(closureV, closureW)
	if _, ok := entryOf(full.entries, v); !ok && !excludes(full.excluded, v) {
		t.Fatalf("the full read never reached V, so U's walk did not run and its edge is no input")
	}
	for _, n := range []model.NodeID{u, v} {
		if _, ok := entryOf(stopped.entries, n); ok || excludes(stopped.excluded, n) {
			t.Fatalf("the stopped read reached %s; U must be unread for its edge to be the difference", n)
		}
	}
	stop := stopped.final(t)
	sh, fh := headOf(stopped.entries, stop.Tau), headOf(full.entries, stop.Tau)
	if len(sh) == 0 {
		t.Fatalf("the stopped plan packs nothing above the stop")
	}
	sameJSON(t, "the head's rank keys", fh, sh)
}

// TestALowerFloorFollowsAWitnessTheSliceCannotHold protects ADR-0016 decision
// 4: a witness counted at seed time can be skipped when packed, and the stop
// must then be confirmed by reading further.
//
// The witness fixture's b.go holds a top-level match, and its source encodes
// to exactly the slice byte budget: a witness, but its group (source plus the
// entry's metadata) does not fit a slice, so the packer drops it as oversized.
// With a file budget of 2, round one's floor is the top level and its read
// stops at the half-level match in d.go. The pack keeps a.go and c.go and drops
// nothing for the file limit, so the certificate fails with one oversized
// witness; the next round needs F+2 witnesses, reads d.go, and stops at the
// tail. d.go is then dropped for the file limit with a key above the new stop,
// and the certificate holds.
//
// Mutation it catches: count witnesses at seed time only (never raise the
// witness count from the pack). The read then ends at round one's floor with
// F+1 witnesses and the stop is unconfirmed.
func TestALowerFloorFollowsAWitnessTheSliceCannotHold(t *testing.T) {
	const (
		fileA = "internal/pay/a.go"
		fileW = "internal/pay/b.go"
		fileC = "internal/pay/c.go"
		fileD = "internal/pay/d.go"
	)
	wide := seedFile(fileW, seedHead)
	wide.content = "package pay\n\n" + strings.Repeat("// a witness whose group the slice cannot hold\n", 64) +
		"func f() {}\n"
	specs := []fixtureFileSpec{
		seedFile(fileA, seedHead), wide, seedFile(fileC, seedHead),
		seedFile(fileD, seedHalf),
		seedFile("internal/pay/t1.go", seedTail), seedFile("internal/pay/t2.go", seedTail),
	}
	slice, err := wireEncodedBytes(int64(len(wide.content)))
	if err != nil {
		t.Fatalf("wireEncodedBytes: %v", err)
	}
	const files = 2
	req := model.ContextRequest{Task: seedTask, Phase: model.PhaseVerify,
		Budget: model.Budget{MaxFiles: files, MaxBytes: slice, MaxSlices: 8, MaxEstimatedTokens: 1 << 20}}

	p := compileStopped(t, specs, nil, req, nil)
	w := p.fx.Node(fileW)
	oversized := false
	for _, r := range p.excluded {
		if r.Reference.NodeID == w && r.Reason == dropOversized {
			oversized = true
		}
	}
	if !oversized {
		t.Fatalf("the wide witness is not dropped as oversized (exclusions %+v); the row has no witness the pack skips",
			p.excluded)
	}
	first, final := p.stops[0], p.final(t)
	if !first.Stopped || len(first.Witnesses) != files+1 {
		t.Fatalf("round one stopped=%v over %d witnesses, want a stop over F+1 = %d",
			first.Stopped, len(first.Witnesses), files+1)
	}
	if final.Unconfirmed != "" || !p.m.ScopeComplete {
		t.Errorf("the stop was left unconfirmed (%q, scope_complete %v); the oversized witness should have lowered the floor",
			final.Unconfirmed, p.m.ScopeComplete)
	}
	if got := len(final.Witnesses); got != files+2 {
		t.Errorf("the final read counted %d witnesses, want F+2 = %d: one more for the witness the pack skipped",
			got, files+2)
	}
	if !final.Stopped || final.NextScore >= first.NextScore || final.Read <= first.Read {
		t.Errorf("the final stop is below score %d after %d matches, round one's below %d after %d: the read did not continue past the first floor",
			final.NextScore, final.Read, first.NextScore, first.Read)
	}
	for _, n := range p.m.Notices {
		if strings.Contains(n, "could not be confirmed") {
			t.Errorf("the manifest says the stop is unconfirmed: %q", n)
		}
	}
	if _, ok := entryOf(p.entries, p.fx.Node(fileD)); ok {
		t.Errorf("d.go was packed; the file budget of %d is held by a.go and c.go", files)
	}
	full := compileFull(t, specs, nil, req)
	assertTheFullReadAboveTheStop(t, p, full)
}

// The margin fixture: three top-level matches, so with a file budget of 2 the
// floor is the top level, and m.go, a match a little below it in a file that
// is a captured change. The active-change boost lifts m.go above every top
// match, so it is packed first. lexicalTau of its contribution reaches the
// floor, so the read takes it and stops at the tails.
func marginSpecs() []fixtureFileSpec {
	changed := seedFile("internal/pay/m.go", seedHead+"_memo")
	changed.modified = true
	return []fixtureFileSpec{
		seedFile("internal/pay/a.go", seedHead), seedFile("internal/pay/b.go", seedHead),
		seedFile("internal/pay/c.go", seedHead), changed,
		seedFile("internal/pay/t1.go", seedTail), seedFile("internal/pay/t2.go", seedTail),
	}
}

// TestTheStoppedPlanIsTheFullReadAboveTheStopAndInsideEveryPackedFile is the
// differential of ADR-0016 decision 7: on the fixtures, the stopped plan's
// ranked head above (recommended, tau), its packed file set and each packed
// file's entry set equal the plan built from every match.
//
// Mutation it catches: stop without the boost margin (stop at the first match
// whose contribution is below the floor). On the margin fixture that stops
// before m.go, whose match is one boost away from the floor and outranks the
// packed files once read: the stopped plan then packs a.go and b.go where the
// full read packs m.go and a.go.
func TestTheStoppedPlanIsTheFullReadAboveTheStopAndInsideEveryPackedFile(t *testing.T) {
	t.Run("the closure fixture", func(t *testing.T) {
		assertTheFullReadAboveTheStop(t,
			compileStopped(t, closureSpecs(), closureScope, seedRequest(2), nil),
			compileFull(t, closureSpecs(), closureScope, seedRequest(2)))
	})
	t.Run("a match one boost below the floor", func(t *testing.T) {
		stopped := compileStopped(t, marginSpecs(), nil, seedRequest(2), nil)
		full := compileFull(t, marginSpecs(), nil, seedRequest(2))
		m := full.fx.Node("internal/pay/m.go")
		if len(full.entries) == 0 || full.entries[0].NodeID != m {
			t.Fatalf("the changed match does not rank first in the full read; the fixture no longer puts a match "+
				"one boost below the floor above the packed files (entries %+v)", full.entries)
		}
		assertTheFullReadAboveTheStop(t, stopped, full)
	})
}

// The pairs fixture: the named seed `Origin` in core/n.go calls `forward`,
// which calls `relay`, which calls `finish`, a second node of n.go: `finish` is
// three hops from the named seed, so the named walk lends it optional at the
// score of a three-hop route. The tail match L in pay/l.go calls `finish`
// directly, one hop: L's walk would lend it recommended, at L's weight times
// one hop. Three top-level matches set the floor, so with a file budget of 2
// the read stops at the tails and L is never read. n.go holds the named seed,
// so it is packed and `finish` with it.
//
// Twelve more tail matches make audit the word nearly every document holds,
// which keeps a tail's contribution -- and with it the stop's tau and L's
// one-hop score -- well below the named seed's three-hop route score, so the
// two pairs differ in both halves and a mixed key is visible.
const (
	pairsNamed = "internal/core/n.go"
	pairsHop1  = "internal/core/a.go"
	pairsHop2  = "internal/core/b.go"
	pairsLex   = "internal/pay/l.go"
)

func pairsSpecs() []fixtureFileSpec {
	specs := []fixtureFileSpec{
		seedFile(pairsNamed, "Origin"), seedNode(pairsNamed, "finish"),
		seedFile(pairsHop1, "forward"), seedFile(pairsHop2, "relay"),
		seedFile(pairsLex, seedTail),
		seedFile("internal/pay/h1.go", seedHead), seedFile("internal/pay/h2.go", seedHead),
		seedFile("internal/pay/h3.go", seedHead),
	}
	for i := range 12 {
		specs = append(specs, seedFile(fmt.Sprintf("internal/pay/t%02d.go", i), seedTail))
	}
	return specs
}

func pairsScope(fx *contextFixture) []model.Relation {
	finish := fx.NodeOf(pairsNamed, "finish")
	return []model.Relation{
		fx.nodeEdge(fx.NodeOf(pairsNamed, "Origin"), model.RelCalls, fx.Node(pairsHop1)),
		fx.edge(pairsHop1, model.RelCalls, pairsHop2),
		fx.nodeEdge(fx.Node(pairsHop2), model.RelCalls, finish),
		fx.nodeEdge(fx.Node(pairsLex), model.RelCalls, finish),
	}
}

// TestAnEntryTakesOneRootsPairNeverAMixOfTwo protects ADR-0016 decision 1: an
// entry's rank key is the largest (requirement, score) pair any one root lends
// it, never the best requirement of one root with the best score of another.
//
// `finish` is three hops from the named seed and one hop from the unread tail
// match L. The full read reads L, and its key is L's pair: recommended, at a
// score no higher than L's own bound, so below the stop. The stopped read never
// reads L, and `finish` keeps the named seed's optional pair, also below the
// stop. So neither plan ranks `finish` above the stop, and the heads are equal.
//
// Mutation it catches: requirement and score maximized separately. The full
// read then gives `finish` L's recommended requirement with the named route's
// score, a key above the stop that only reading L produces, and the full read's
// head holds an entry the stopped plan's does not.
func TestAnEntryTakesOneRootsPairNeverAMixOfTwo(t *testing.T) {
	req := model.ContextRequest{Task: seedTask, Seeds: []string{"Origin"}, Phase: model.PhaseVerify,
		Budget: model.Budget{MaxFiles: 2}}
	stopped := compileStopped(t, pairsSpecs(), pairsScope, req, nil)
	full := compileFull(t, pairsSpecs(), pairsScope, req)
	stop := stopped.final(t)
	finish := stopped.fx.NodeOf(pairsNamed, "finish")
	lex := stopped.fx.Node(pairsLex)
	if _, ok := entryOf(stopped.entries, lex); ok || excludes(stopped.excluded, lex) {
		t.Fatalf("the stopped read reached L; the row needs L unread")
	}
	near, ok := entryOf(stopped.entries, finish)
	if !ok {
		t.Fatalf("`finish` is not in the stopped plan, though its file holds the named seed")
	}
	if near.Requirement != model.RequirementOptional {
		t.Fatalf("`finish` is %s in the stopped plan, want %s from the named seed three hops away",
			near.Requirement, model.RequirementOptional)
	}
	far, ok := entryOf(full.entries, finish)
	if !ok {
		t.Fatalf("`finish` is not in the full read's plan")
	}
	if far.Requirement != model.RequirementRecommended {
		t.Errorf("`finish` is %s in the full read, want %s: L's pair ranks first by requirement",
			far.Requirement, model.RequirementRecommended)
	}
	if aboveStop(far, stop.Tau) {
		t.Errorf("`finish` ranks above the stop in the full read (%s, %d > tau %d): its key mixes L's requirement with the named route's score",
			far.Requirement, far.ScoreMicros, stop.Tau)
	}
	if far.ScoreMicros >= near.ScoreMicros {
		t.Errorf("`finish` scores %d in the full read and %d from the named seed alone; L's pair must carry L's own score, not the named route's",
			far.ScoreMicros, near.ScoreMicros)
	}
	assertTheFullReadAboveTheStop(t, stopped, full)
}

// TestRaisingTheFileBudgetNeverDropsAFunctionAndThePageSizeNeverMovesThePlan
// protects ADR-0016 decision 7's budget monotonicity for plans bound by their
// file budget, and decision 3's rule that the stop depends on scores only.
//
// On the closure fixture, a file budget of 1 to 4 gives plans each bound by
// its file budget, and each plan's functions are all in the next one's. At a
// file budget of 2 the read stops, and the whole plan -- entries, slices,
// exclusions, notices, scope verdict and canonical hash -- is the same at a
// page size of 1, of 2 and of the default.
//
// Mutation it catches: stop on a page boundary instead of a score (end the read
// with the page on which the floor was found). At the default page every match
// is on page one and is read, at a page of 1 the read ends right after the
// floor, and the two plans differ in what they read and exclude.
func TestRaisingTheFileBudgetNeverDropsAFunctionAndThePageSizeNeverMovesThePlan(t *testing.T) {
	functions := map[string]bool{}
	var prev map[string]bool
	for files := 1; files <= 4; files++ {
		p := compileStopped(t, closureSpecs(), closureScope, seedRequest(files), nil)
		if files == 1 {
			for _, spec := range p.fx.Specs {
				if spec.kind == model.NodeFunction {
					functions[string(p.fx.NodeOf(spec.path, spec.symbol))] = true
				}
			}
		}
		if files == 2 && !stoppedNotice(p.m) {
			t.Fatalf("at a file budget of 2 the read did not stop (notices %q); the page-size rows below compare full reads",
				p.m.Notices)
		}
		got := map[string]bool{}
		for _, e := range p.entries {
			if functions[string(e.NodeID)] {
				got[string(e.NodeID)] = true
			}
		}
		for id := range prev {
			if !got[id] {
				t.Errorf("raising the file budget to %d removed the function %s from the plan", files, id)
			}
		}
		prev = got
	}

	base := compileStopped(t, closureSpecs(), closureScope, seedRequest(2), nil)
	for _, items := range []int{1, 2} {
		t.Run(fmt.Sprintf("a page of %d", items), func(t *testing.T) {
			p := compileStopped(t, closureSpecs(), closureScope, seedRequest(2),
				func(cfg *config.Config) { cfg.Resources.MaxPageItems = items })
			sameJSON(t, "entries", base.entries, p.entries)
			sameJSON(t, "slices", base.slices, p.slices)
			sameJSON(t, "exclusions", base.excluded, p.excluded)
			sameJSON(t, "notices", base.m.Notices, p.m.Notices)
			if p.m.ScopeComplete != base.m.ScopeComplete || p.m.CanonicalHash != base.m.CanonicalHash {
				t.Errorf("scope_complete %v hash %s at a page of %d, %v %s at the default page",
					p.m.ScopeComplete, p.m.CanonicalHash, items, base.m.ScopeComplete, base.m.CanonicalHash)
			}
		})
	}
}
