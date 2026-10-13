package link

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// everyKind is one fact of each kind with every field its kind carries set.
func everyKind() []idBinding {
	return []idBinding{
		{Kind: FactCall, SiteFile: "a", Site: 0, TargetFile: "b", Target: "b.f", Detail: DetailHierarchy, Dispatch: model.DispatchVirtual},
		{Kind: FactCall, SiteFile: "a", Site: 0, External: externalMethod("java", "java.util.List", "add"), Detail: DetailHierarchy,
			Dispatch: model.DispatchExact},
		{Kind: FactExternalSite, SiteFile: "a", Site: 1},
		{Kind: FactOpenSite, SiteFile: "a", Site: 2, Reason: model.ReasonValueUntracked},
		{Kind: FactFlow, SiteFile: "a", Site: 0, Arg: 3, TargetFile: "b", Target: "b.f.x", Detail: DetailArgument},
		{Kind: FactImport, SiteFile: "a", Site: 4, TargetFile: "b", Target: "mod:b", Detail: DetailImport},
		{Kind: FactExtends, SiteFile: "a", Site: 5, External: externalType("java", "java.lang.Thread"),
			Detail: DetailSupertype},
		{Kind: FactImplements, SiteFile: "a", Site: 5, TargetFile: "b", Target: "b.I", Detail: DetailMethodSet},
		{Kind: FactOverrides, SiteFile: "a", Site: 6, TargetFile: "b", Target: "b.I.m", Detail: DetailSupertype},
	}
}

// stored writes a state of fields, recs and facts.
func stored(t *testing.T, dir string, fields []manifestField, recs []record, facts []idBinding) []byte {
	t.Helper()
	s, err := newIDBindingSort(dir)
	bindings := sortedRun(t, s, err, facts)
	r, err := newRecordSort(dir)
	records := sortedRun(t, r, err, recs)
	res := &resolution{bindings: bindings}
	t.Cleanup(res.close)
	if res.groups, err = emptyOf(newGroupSort(dir)); err != nil {
		t.Fatal(err)
	}
	if res.reads, err = emptyOf(newReadSort(dir, compareRead)); err != nil {
		t.Fatal(err)
	}
	if res.taken, err = emptyOf(newTakenSort(dir, compareTakenFrom)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeState(&buf, fields, records, res); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	return buf.Bytes()
}

// TestStateRoundTripsEveryKind stores one fact of every kind and member
// records with every count, and reads back exactly what it stored.
//
// Mutation: leave a fact's dispatch, reason, argument or external key out of
// its stored form -> the read-back fact differs. Mutation: leave a record's
// counts out -> the read-back record differs.
func TestStateRoundTripsEveryKind(t *testing.T) {
	dir := t.TempDir()
	fields := []manifestField{{Key: fieldGoModule, Value: "example.com/app"}}
	recs := []record{
		{File: "a", Content: "c1", Digest: "d1", Interface: "i1", Sites: "s1", Tabled: true, Dropped: 2,
			Counts: counts{Sites: 7, Bound: 1, Open: 2, Computed: 1, External: 3, Imports: 4, OpenImports: 5}},
		{File: "b", Content: "c2"},
	}
	facts := everyKind()
	raw := stored(t, dir, fields, recs, facts)
	if err := ValidateState(bytes.NewReader(raw)); err != nil {
		t.Fatalf("ValidateState refuses a state writeState wrote: %v", err)
	}
	prev, err := loadState(bytes.NewReader(raw), dir)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	defer prev.close()
	if !slices.Equal(prev.fields, fields) {
		t.Errorf("the fields read back are %v, want %v", prev.fields, fields)
	}
	var gotRecs []record
	if err := prev.records.Each(func(r record) error { gotRecs = append(gotRecs, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotRecs, recs) {
		t.Errorf("the records read back are\n%+v\nwant\n%+v", gotRecs, recs)
	}
	var got []idBinding
	if err := prev.bindings.Each(func(b idBinding) error { got = append(got, b); return nil }); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(facts)
	slices.SortFunc(want, compareIDSite)
	if !slices.Equal(got, want) {
		t.Errorf("the facts read back are\n%+v\nwant\n%+v", got, want)
	}
}

// TestStateRefusesCorruptForms refuses a state of a tag no resolution state
// opens with, one cut inside its tag, a truncated state and one with a byte
// past its sections, each as corrupt storage, so the build resolves in full
// instead.
//
// Mutation: read a tag that is a short piece of this one as another
// version's -> the state cut inside its tag is not corrupt. Mutation: stop
// reading at the last section -> the trailing byte is accepted.
func TestStateRefusesCorruptForms(t *testing.T) {
	dir := t.TempDir()
	raw := stored(t, dir, nil, []record{{File: "a", Tabled: true}}, everyKind())
	foreign := append([]byte("scip.manifest.v1\n"), raw[len(stateTag()):]...)
	for name, state := range map[string][]byte{
		"foreign tag": foreign,
		"cut tag":     raw[:len(stateTag())-1],
		"truncated":   raw[:len(raw)-1],
		"trailing":    append(slices.Clone(raw), 0),
	} {
		err := ValidateState(bytes.NewReader(state))
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != model.CodeStorageCorrupt {
			t.Errorf("%s: ValidateState = %v, want corrupt storage", name, err)
		}
	}
}

// TestFactDiffCoversEveryKind diffs a predecessor's facts against a
// refresh's in which a keyword argument's flow moved from one parameter to
// another (the callee's parameter was renamed, so the keyword binds its
// variadic keyword parameter) and nothing else of the caller changed: the
// caller's file is republished for its moved flow, the callee's for its
// changed set of targeted parameters, and a third file whose facts held is
// not.
//
// Mutation: diff call facts alone -> the caller's file is not republished.
// Mutation: collect targets from declarations alone -> the callee's file is
// not republished for its parameters.
func TestFactDiffCoversEveryKind(t *testing.T) {
	u := testUnit(t)
	callF := func(site string) idBinding {
		return idBinding{Kind: FactCall, SiteFile: model.FileID(site), Site: 0, TargetFile: "b.py", Target: "f",
			Detail: DetailDirect, Dispatch: model.DispatchExact}
	}
	flow := func(param string) idBinding {
		return idBinding{Kind: FactFlow, SiteFile: "a.py", Site: 0, Arg: 0, TargetFile: "b.py", Target: model.NodeID(param),
			Detail: DetailArgument}
	}
	s, err := newIDBindingSort(u.dir)
	before := sortedRun(t, s, err, []idBinding{callF("a.py"), flow("f.x"), callF("c.py")})
	s, err = newIDBindingSort(u.dir)
	after := sortedRun(t, s, err, []idBinding{callF("a.py"), flow("f.kw"), callF("c.py")})
	targets, err := targetsOf(u.dir, after)
	if err != nil {
		t.Fatal(err)
	}
	defer targets.Close()
	rf := &refresh{u: u, prev: &previous{resolution: resolution{bindings: before}}}
	if rf.changed, err = newFileIDSort(u.dir); err != nil {
		t.Fatal(err)
	}
	defer rf.changed.Close()
	if err := rf.diff(after, targets); err != nil {
		t.Fatalf("diff: %v", err)
	}
	changed, err := rf.changed.Sorted()
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	var got []model.FileID
	if err := changed.Each(func(id model.FileID) error { got = append(got, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := []model.FileID{"a.py", "b.py"}; !slices.Equal(got, want) {
		t.Errorf("the diff republishes %v, want %v", got, want)
	}
}

// TestStateOfAnEarlierBuildIsNotRead hands ValidateState and loadState the
// states an earlier build wrote, under the tag it opened them with: one whose
// fact carries technique byte 4, which that build's order gave `field` and
// this build's gives `function_value`, and one whose member record has the
// eight counts that build stored (no reflective count). Both are of another
// version, never this build's form and never corrupt storage: the build
// resolves in full, with nothing to warn about.
//
// Mutation: keep the tag free of Version (the linker's code digest) -> the first state is read, its
// field fact silently becoming a function-value one, and the second is
// refused as corrupt. Mutation: refuse every unknown tag as corrupt -> both
// read as corrupt storage.
func TestStateOfAnEarlierBuildIsNotRead(t *testing.T) {
	const earlierTag = "link.resolution.nodes\n"
	dir := t.TempDir()
	fact := idBinding{Kind: FactCall, SiteFile: "a", Site: 0, TargetFile: "b", Target: "b.f",
		Detail: details[4], Dispatch: model.DispatchExact}
	raw := stored(t, dir, nil, []record{{File: "a", Tabled: true}}, []idBinding{fact})
	if !bytes.HasPrefix(raw, []byte(stateTag())) {
		t.Fatalf("the stored state does not open with stateTag %q", stateTag())
	}
	earlierFact := append([]byte(earlierTag), raw[len(stateTag()):]...)

	// The earlier record: its file, four strings, the tabled byte and eight
	// counts.
	rec := appendString(nil, "a")
	for range 4 {
		rec = appendString(rec, "")
	}
	rec = append(rec, 1)
	for range 8 {
		rec = binary.AppendUvarint(rec, 0)
	}
	earlierRecord := binary.AppendUvarint([]byte(earlierTag), 0)
	earlierRecord = appendString(binary.AppendUvarint(earlierRecord, 1), string(rec))
	for range 7 {
		earlierRecord = binary.AppendUvarint(earlierRecord, 0)
	}
	earlierRecord = append(earlierRecord, 0, 0)

	for name, state := range map[string][]byte{"technique order": earlierFact, "record form": earlierRecord} {
		if err := ValidateState(bytes.NewReader(state)); !errors.Is(err, ErrStateVersion) {
			t.Errorf("%s: ValidateState = %v, want ErrStateVersion", name, err)
		}
		if p, err := loadState(bytes.NewReader(state), dir); !errors.Is(err, ErrStateVersion) {
			p.close()
			t.Errorf("%s: loadState = %v, want ErrStateVersion", name, err)
		}
	}
}
