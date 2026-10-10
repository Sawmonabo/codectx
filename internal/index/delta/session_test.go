package delta_test

import (
	"database/sql"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/delta"
	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
)

// session drives one repository through a sequence of trees the way the
// coordinator does: each tree is a snapshot, each source file whose bytes
// moved gets a fresh structural unit and every other file's sealed one is
// attached, the link project plan decides the linking units and their
// inputs, and each linking unit is refreshed from the unit its scope had in
// the previous tree. Beside every refresh it builds the same tree's linking
// units in full, in a generation of their own, so a refresh can be compared
// with what it must equal.
type session struct {
	t      *testing.T
	f      *fixture
	ts     *treesitter.Provider
	tables *countedTables
	lp     *link.Provider
	a      delta.Applier
	db     *sql.DB
	// structural is the sealed structural unit of each (path, content).
	structural map[string]model.UnitID
	// last is the linking unit each scope had in the previous tree.
	last map[string]linkBuild
	// fulls is the full build of each set of linking inputs.
	fulls map[string]model.UnitID
}

// linkBuild is one linking unit of one tree.
type linkBuild struct {
	scope string
	// inputs spells the unit's inputs, path and content, in order.
	inputs   string
	previous model.UnitID
	unit     model.UnitID
	full     model.UnitID
	res      delta.Result
	// reads is how often the refresh read each file's table.
	reads map[string]int
	// reused is a unit whose inputs did not move, attached as it was.
	reused bool
}

func newSession(t *testing.T) *session {
	t.Helper()
	f := newFixture(t)
	tables := &countedTables{StoredTables: link.StoreTables(f.store), reads: map[string]int{}}
	lp := link.New(tables)
	return &session{t: t, f: f, ts: f.structuralProvider(), tables: tables, lp: lp,
		a: delta.NewLink(f.store, lp, tables, limits, f.pool), db: f.raw(),
		structural: map[string]model.UnitID{}, last: map[string]linkBuild{}, fulls: map[string]model.UnitID{}}
}

// advance records files as the next tree, refreshes every linking unit the
// plan gives it and builds each in full beside it. It returns the units in
// scope order.
func (s *session) advance(files map[string]string) []linkBuild {
	s.t.Helper()
	tr := s.f.snapshot(files)
	gen := s.f.generation(tr)
	var structural []model.UnitID
	for _, p := range tr.paths {
		if link.FamilyOf(srclang.Of(p)) == "" {
			continue
		}
		key := p + "\x00" + tr.files[p].ContentHash
		id, ok := s.structural[key]
		if ok {
			s.f.attach(gen, id)
		} else {
			id = s.f.structural(s.ts, tr, gen, p)
			s.structural[key] = id
		}
		structural = append(structural, id)
	}
	plan, err := link.PlanUnits(s.f.ctx, tr.view)
	if err != nil {
		s.t.Fatalf("PlanUnits: %v", err)
	}
	var out []linkBuild
	paths := map[string][]string{}
	for _, pu := range plan.Units {
		var owned []string
		var spelled strings.Builder
		for _, p := range tr.paths {
			if pu.OwnsInput(p) {
				owned = append(owned, p)
				spelled.WriteString(p + "\x00" + tr.files[p].ContentHash + "\x00")
			}
		}
		paths[pu.ScopeKey] = owned
		b := linkBuild{scope: pu.ScopeKey, inputs: spelled.String()}
		prev, had := s.last[pu.ScopeKey]
		if had && prev.inputs == b.inputs {
			// The coordinator reuses a unit whose inputs did not move.
			s.f.attach(gen, prev.unit)
			prev.reused = true
			out = append(out, prev)
			continue
		}
		if had {
			b.previous = prev.unit
		}
		req := s.f.request(s.lp, build{tree: tr, gen: gen, cfgHash: "config", scopeKey: pu.ScopeKey,
			paths: owned, previous: b.previous})
		s.tables.reset()
		b.res = applyWithin(s.t, s.a, s.f.ctx, req, 2*time.Minute)
		b.reads = s.tables.snapshot()
		b.unit = req.Build.Spec.ID
		s.f.flush()
		out = append(out, b)
	}

	fullGen := s.f.generation(tr)
	for _, id := range structural {
		s.f.attach(fullGen, id)
	}
	for i := range out {
		b := &out[i]
		if id, ok := s.fulls[b.inputs]; ok {
			s.f.attach(fullGen, id)
			b.full = id
			continue
		}
		req := s.f.request(s.lp, build{tree: tr, gen: fullGen, cfgHash: "full", scopeKey: b.scope, paths: paths[b.scope]})
		if res := applyWithin(s.t, s.a, s.f.ctx, req, 2*time.Minute); !res.Full {
			s.t.Fatalf("the full build of %s is %+v, want a build with no predecessor", b.scope, res)
		}
		b.full = req.Build.Spec.ID
		s.fulls[b.inputs] = b.full
		s.f.flush()
	}
	s.last = map[string]linkBuild{}
	for _, b := range out {
		s.last[b.scope] = b
	}
	return out
}

// check asserts that every unit step rebuilt is the full build of its tree,
// fact for fact, and that a unit with a predecessor got there by inheriting
// from it: a refresh that fell back to resolving and publishing everything
// would equal the full build whatever the incremental path does.
func (s *session) check(step string, units []linkBuild) {
	s.t.Helper()
	for _, b := range units {
		if b.reused {
			continue
		}
		if b.previous == "" {
			if !b.res.Full || b.res.FullReason != delta.FullNoPredecessor {
				s.t.Fatalf("%s: the new unit %s is %+v, want a full build with no predecessor", step, b.scope, b.res)
			}
		} else if b.res.Full || !b.res.Filtered {
			s.t.Fatalf("%s: the refresh of %s is %+v, want the predecessor's resolution inherited", step, b.scope, b.res)
		}
		failed := s.t.Failed()
		sameFacts(s.t, s.db, b.full, b.unit)
		if !failed && s.t.Failed() {
			s.t.Fatalf("%s: the refreshed unit of %s differs from the full build of the same tree", step, b.scope)
		}
	}
}

// only is the one unit a single-project tree plans.
func (s *session) only(units []linkBuild) linkBuild {
	s.t.Helper()
	if len(units) != 1 {
		s.t.Fatalf("the tree plans %d linking units, want one", len(units))
	}
	return units[0]
}

// wantRepublished asserts that the refresh b republished exactly the files
// named and carried every other file's facts over. Carried rows and
// republished rows are alike in storage (a carried evidence row is minted
// again under the new unit), so it is read from what the carry-over
// inherited: every evidence row the predecessor located in a file that was
// not republished, none of a republished file's, and none of the index-level
// bucket's. A file the refresh republished without naming it is short of its
// carried rows, which the insert of the fresh copy pre-empted; a file it
// named replaced is not carried at all. Each witness must hold evidence in the predecessor, or
// its republication could not be told from its carrying.
func (s *session) wantRepublished(step string, b linkBuild, republished []string, witnesses ...string) {
	s.t.Helper()
	ids := map[string]string{}
	for _, p := range republished {
		ids[string(model.NewFileID(s.f.repo, p))] = p
	}
	rows, err := s.db.Query(`SELECT coalesce(lower(hex(file_id)), ''), count(*) FROM evidence WHERE unit_id = ? GROUP BY file_id`,
		unitRowID(s.t, s.db, b.previous))
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	per := map[string]int64{}
	var want int64
	for rows.Next() {
		var file string
		var n int64
		if err := rows.Scan(&file, &n); err != nil {
			s.t.Fatal(err)
		}
		per[file] = n
		// The index-level bucket (evidence of no file: the external nodes)
		// is replaced on every refresh and published again, never carried.
		if _, ok := ids[file]; !ok && file != "" {
			want += n
		}
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	for _, w := range witnesses {
		if per[string(model.NewFileID(s.f.repo, w))] == 0 {
			s.t.Fatalf("%s: %s holds no evidence in the predecessor, so its republication cannot be observed", step, w)
		}
	}
	if b.res.Carried.Evidence != want {
		s.t.Errorf("%s: the refresh carried %d evidence rows, want %d: every row of the predecessor but those of %v",
			step, b.res.Carried.Evidence, want, republished)
	}
}

// snapshot is a copy of the reads counted since the last reset.
func (c *countedTables) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.reads)
}

// script is one language's tree and the edits applied to it in order.
type script struct {
	name  string
	base  map[string]string
	edits []edit
}

// edit is one change to a tree: substitutions in files, whole files written,
// files moved to a new path with their bytes, and files removed.
type edit struct {
	name    string
	replace []sub
	write   map[string]string
	move    [][2]string
	remove  []string
}

// sub replaces the one occurrence of old in path with new.
type sub struct{ path, old, new string }

// apply is files after e. A substitution whose text is not in its file
// exactly once is a broken script, never a silent no-op.
func (e edit) apply(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	out := maps.Clone(files)
	for _, r := range e.replace {
		src, ok := out[r.path]
		if !ok || strings.Count(src, r.old) != 1 {
			t.Fatalf("%s: %q is not in %s exactly once", e.name, r.old, r.path)
		}
		out[r.path] = strings.Replace(src, r.old, r.new, 1)
	}
	for p, src := range e.write {
		out[p] = src
	}
	for _, m := range e.move {
		src, ok := out[m[0]]
		if !ok {
			t.Fatalf("%s: no file %s to move", e.name, m[0])
		}
		delete(out, m[0])
		out[m[1]] = src
	}
	for _, p := range e.remove {
		if _, ok := out[p]; !ok {
			t.Fatalf("%s: no file %s to remove", e.name, p)
		}
		delete(out, p)
	}
	if maps.Equal(out, files) {
		t.Fatalf("%s: the edit changes nothing", e.name)
	}
	return out
}
