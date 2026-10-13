package admission

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

const childGiB int64 = 1 << 30

func childLedger(t *testing.T, allocation int64) *Ledger {
	t.Helper()
	l, err := NewLedger(allocation, 64*childGiB)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// teach runs one external tool's admission and records what it measured, as a
// profile unit or a language server does when its run ends.
func teach(t *testing.T, l *Ledger, needs *ChildNeeds, key ChildKey, work, need int64) {
	t.Helper()
	h, err := l.HoldChild(context.Background(), needs, key, work, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Observe(need, true)
	h.Release()
}

// TestAToolReservesInProportionToItsWork protects the need model's dependence
// on the run's work: within one learned class, a smaller project reserves less
// than a larger one, and a first run of a class reserves the allocation.
//
// Mutation: plan the prediction for the observation's work instead of the
// run's (m.h.Predict(n.model's last work) or a constant) -> both projects
// reserve the same figure. Mutation: reserve a learned figure on a class with
// no observation -> the cold class reserves less than the allocation.
func TestAToolReservesInProportionToItsWork(t *testing.T) {
	l := childLedger(t, 64*childGiB)
	var needs ChildNeeds
	key := ChildKey{Tool: "indexer t", Fingerprint: "f"}
	// 1,500 and 2,000 bytes share size class 11; 100 bytes of need per byte.
	teach(t, l, &needs, key, 1500, 150_000)

	plan := func(work int64) ChildNeed {
		t.Helper()
		h, err := l.HoldChild(context.Background(), &needs, key, work, 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Release()
		n, _ := h.Need()
		return n
	}
	small, large := plan(1100), plan(2000)
	if !small.Learned || !large.Learned {
		t.Fatalf("a learned class planned as a first run: small %+v large %+v", small, large)
	}
	if small.Bytes >= large.Bytes {
		t.Fatalf("a 1,100-byte project reserves %d bytes and a 2,000-byte one %d; the smaller work must reserve less", small.Bytes, large.Bytes)
	}
	if cold := plan(1 << 20); cold.Learned || cold.Bytes != 64*childGiB {
		t.Fatalf("a class nothing was measured in reserved %d bytes (learned %v), want the whole allocation", cold.Bytes, cold.Learned)
	}
}

// TestNoToolReservesMoreThanTheAllocation protects the ceiling every plan is
// held within: a learned need above the allocation, and a first run, both
// reserve exactly the allocation the ledger re-derived for the admission.
//
// Mutation: drop the min against the allocation in plan -> the learned
// 40 GiB need reserves 40 GiB of a 16 GiB allocation.
func TestNoToolReservesMoreThanTheAllocation(t *testing.T) {
	l := childLedger(t, 64*childGiB)
	var needs ChildNeeds
	key := ChildKey{Tool: "server s", Fingerprint: "f"}
	teach(t, l, &needs, key, 1<<20, 40*childGiB)
	l.SetAllocation(16 * childGiB)
	h, err := l.HoldChild(context.Background(), &needs, key, 1<<20, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if n, _ := h.Need(); !n.Learned || n.Bytes != 16*childGiB {
		t.Fatalf("a learned 40 GiB need reserved %d bytes (learned %v) of a 16 GiB allocation", n.Bytes, n.Learned)
	}
	if _, reserved := l.Snapshot(); reserved != 16*childGiB {
		t.Fatalf("the ledger holds %d bytes for one tool within a 16 GiB allocation", reserved)
	}
}

// TestAResidentSetAboveTheReservationRaisesIt protects the correction while a
// tool runs: a reading above what its holding holds raises the holding at
// once, so a reservation that fit beside the planned figure waits beside the
// real one, and the run's need is the highest reading.
//
// Mutation: drop the Adjust in Residency.sample -> the ledger still holds the
// planned 1 GiB and the 6 GiB reservation is granted beside a tool using 5.
func TestAResidentSetAboveTheReservationRaisesIt(t *testing.T) {
	l := childLedger(t, 10*childGiB)
	h, err := l.Hold(context.Background(), Reservation{MemoryBytes: childGiB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reading atomic.Int64
	reading.Store(5 * childGiB)
	read := func() (int64, bool) { return reading.Load(), true }
	follow := FollowResidency(h, read, time.Millisecond)
	if need, ok := follow.Peak(); !ok || need != 5*childGiB {
		t.Fatalf("the follower read %d (ok %v), want the 5 GiB reading", need, ok)
	}
	if _, reserved := l.Snapshot(); reserved != 5*childGiB {
		t.Fatalf("the ledger holds %d bytes for a tool read at 5 GiB above its 1 GiB reservation", reserved)
	}
	waiting, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := l.Hold(waiting, Reservation{MemoryBytes: 6 * childGiB}, nil); err == nil {
		t.Fatal("a 6 GiB reservation was granted beside a tool using 5 GiB of a 10 GiB allocation")
	}
	reading.Store(2 * childGiB)
	if need, _ := follow.Stop(); need != 5*childGiB {
		t.Fatalf("the run's need is %d, want its highest reading, 5 GiB", need)
	}
	if _, reserved := l.Snapshot(); reserved != 5*childGiB {
		t.Fatalf("a lower reading lowered the holding to %d; a tool that shrank can grow again", reserved)
	}
	h.Release()
}

// TestAColdClassIsPlannedFromItsFirstRun protects the cold start: the runs of
// a class with no observation wait for its first run and are planned from what
// it measured, instead of each reserving the allocation one after another; and
// a first run that ends without a measurement hands the role on rather than
// stranding them.
//
// Mutation: drop the probing gate in plan -> the second run is planned at once
// as a first run, at the allocation. Mutation: drop the release of the probe in
// done -> the second run never returns and the guard fails the test.
func TestAColdClassIsPlannedFromItsFirstRun(t *testing.T) {
	l := childLedger(t, 64*childGiB)
	var needs ChildNeeds
	key := ChildKey{Tool: "indexer t", Fingerprint: "f"}
	guard, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := l.HoldChild(guard, &needs, key, 4096, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	type planned struct {
		h   *Holding
		err error
	}
	second := make(chan planned, 1)
	go func() {
		h, err := l.HoldChild(guard, &needs, key, 4096, 1, 0, nil)
		second <- planned{h, err}
	}()
	select {
	case <-second:
		t.Fatal("a second run of a cold class was planned before the first one measured anything")
	case <-time.After(50 * time.Millisecond):
	}
	first.Observe(4096*10, true)
	first.Release()
	got := <-second
	if got.err != nil {
		t.Fatal(got.err)
	}
	if n, _ := got.h.Need(); !n.Learned || n.Bytes >= 64*childGiB {
		t.Fatalf("the second run reserved %d bytes (learned %v); it must be planned from the first run's measurement", n.Bytes, n.Learned)
	}
	got.h.Release()

	// A first run that teaches nothing hands the role on.
	coldKey := ChildKey{Tool: "indexer u", Fingerprint: "f"}
	probe, err := l.HoldChild(guard, &needs, coldKey, 4096, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		h, err := l.HoldChild(guard, &needs, coldKey, 4096, 1, 0, nil)
		second <- planned{h, err}
	}()
	probe.Release()
	got = <-second
	if got.err != nil {
		t.Fatalf("the run waiting on a first run that measured nothing was not released: %v", got.err)
	}
	if n, _ := got.h.Need(); n.Learned || n.Bytes != 64*childGiB {
		t.Fatalf("after a first run that measured nothing the next run reserved %d bytes (learned %v), want the allocation as the next first run", n.Bytes, n.Learned)
	}
	got.h.Release()
}

// manifestView is a snapshot manifest and nothing else: ProjectWork reads no
// file's bytes.
type manifestView []model.FileVersion

func (v manifestView) Header() model.Snapshot { return model.Snapshot{} }
func (v manifestView) EachFile(_ context.Context, _ model.FileSelection, fn func(model.FileVersion) error) error {
	for _, f := range v {
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}
func (v manifestView) Open(context.Context, model.FileID) (io.ReadCloser, model.FileVersion, error) {
	return nil, model.FileVersion{}, errors.New("not read")
}
func (v manifestView) ReadRange(context.Context, model.FileID, model.ByteRange) ([]byte, model.FileVersion, error) {
	return nil, model.FileVersion{}, errors.New("not read")
}

// TestAProjectsWorkIsItsOwnDirectory protects what a run is planned on: the
// live bytes at or under its project directory, never a sibling's that shares
// its name as a prefix, a deleted file's or the whole repository's; and the
// half-life is the repository's directories holding the tool's marker.
//
// Mutation: count every file's bytes -> the 100-byte project reports the
// repository's 1,000,111 bytes and would reserve as much as the root project.
// Mutation: match the root as a bare prefix -> "app2/big.go" is counted in
// "app". Mutation: count marker files rather than directories -> the root's
// two markers count twice.
func TestAProjectsWorkIsItsOwnDirectory(t *testing.T) {
	view := manifestView{
		{Path: "go.mod", Size: 10},
		{Path: "go.work", Size: 1},
		{Path: "app/go.mod", Size: 20},
		{Path: "app/main.go", Size: 80},
		{Path: "app/gone.go", Size: 0, Status: model.FileDeleted},
		{Path: "app2/go.mod", Size: 0},
		{Path: "app2/big.go", Size: 1_000_000},
	}
	markers := []string{"go.mod", "go.work"}
	work, projects, err := ProjectWork(context.Background(), view, "app", markers)
	if err != nil {
		t.Fatal(err)
	}
	if work != 100 || projects != 3 {
		t.Fatalf("project app: work %d bytes, %d projects; want 100 bytes and 3 project directories", work, projects)
	}
	if work, _, _ = ProjectWork(context.Background(), view, "", markers); work != 1_000_111 {
		t.Fatalf("the workspace root's work is %d bytes, want every live file's 1,000,111", work)
	}
}

// TestAMeasuredFirstRunGivesTheAllocationBack protects a long-lived first run:
// once its need is measured, its holding is lowered from the whole allocation
// to what its class predicts for its work, so a language server that keeps
// running does not hold every other child out for its lifetime.
//
// Mutation: drop the Adjust in Holding.Observe -> the first run keeps the 16
// GiB allocation and the 1 GiB reservation beside it waits.
func TestAMeasuredFirstRunGivesTheAllocationBack(t *testing.T) {
	l := childLedger(t, 16*childGiB)
	var needs ChildNeeds
	h, err := l.HoldChild(context.Background(), &needs, ChildKey{Tool: "server s", Fingerprint: "f"}, 1<<20, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if _, reserved := l.Snapshot(); reserved != 16*childGiB {
		t.Fatalf("a first run reserved %d bytes, want the 16 GiB allocation", reserved)
	}
	h.Observe(2*childGiB, true)
	if b := h.Bytes(); b < 2*childGiB || b >= 3*childGiB {
		t.Fatalf("a first run measured at 2 GiB holds %d bytes; want its class's prediction for its work", b)
	}
	waiting, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	beside, err := l.Hold(waiting, Reservation{MemoryBytes: childGiB}, nil)
	if err != nil {
		t.Fatalf("a 1 GiB reservation waited beside a measured 2 GiB first run in a 16 GiB allocation: %v", err)
	}
	beside.Release()
}
