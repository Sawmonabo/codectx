package admission

import (
	"context"
	"testing"
)

// Disk gates admission, and it gates it on the same queue memory does. Every
// wait below is observed through the waiter's own makeRoom step, which the
// ledger calls exactly when the waiter is at the head and does not fit, so no
// case depends on a timer. Mutations this fails on:
//   - pump checking only the memory sum: the disk waiter is admitted beside the
//     first child although their staged bytes together exceed the free space
//     the host has, which is a run filling the device;
//   - pump checking the disk sum for a head that requests no disk: the
//     memory-only child is held behind a disk dimension another child filled,
//     for a resource it does not want;
//   - release not returning the disk bytes: the disk waiter is never admitted
//     after the first child leaves, although a memory-only child is still
//     running, so the runs-alone rule cannot mask the leak.
func TestDiskIsAdmittedOnTheSameQueueAsMemory(t *testing.T) {
	l, err := NewLedger(8<<30, 10<<30)
	if err != nil {
		t.Fatalf("the ledger was refused: %v", err)
	}
	ctx := context.Background()

	// Admitted alone on an idle ledger, above the whole disk allocation: the
	// disk dimension is now past full.
	first, err := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 12 << 30}, nil)
	if err != nil {
		t.Fatalf("the first child was not admitted alone: %v", err)
	}

	// A child that stages nothing fits the memory allocation and must be
	// admitted at once. Were it held, its makeRoom step would run and cancel
	// the wait.
	memCtx, memCancel := context.WithCancel(ctx)
	defer memCancel()
	memoryOnly, err := l.ReserveWith(memCtx, Reservation{MemoryBytes: 1 << 30}, memCancel)
	if err != nil {
		t.Fatalf("a child that stages nothing was held behind a full disk dimension: %v", err)
	}
	defer memoryOnly()

	// Fits the memory allocation with room to spare and does not fit the disk
	// one, so it must wait.
	waited := make(chan struct{}, 1)
	signalWait := func() {
		select {
		case waited <- struct{}{}:
		default:
		}
	}
	admitted := make(chan error, 1)
	go func() {
		release, rerr := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 6 << 30}, signalWait)
		admitted <- rerr
		if rerr == nil {
			release()
		}
	}()
	select {
	case rerr := <-admitted:
		t.Fatalf("a child whose staged bytes do not fit the free space did not wait for them (error: %v)", rerr)
	case <-waited:
	}

	// The memory-only child still runs, so the waiter is re-pumped against
	// the sums and not against an idle ledger: it fits only if the release
	// gave the disk back.
	first()
	select {
	case rerr := <-admitted:
		if rerr != nil {
			t.Fatalf("the waiting child was refused rather than admitted: %v", rerr)
		}
	case <-waited:
		t.Fatal("the waiting child was not admitted after the disk it waited for was released")
	}
}

// waitSignal is a makeRoom step that records, without blocking, that its
// reservation reached the head of the queue and did not fit.
func waitSignal() (chan struct{}, func()) {
	waited := make(chan struct{}, 1)
	return waited, func() {
		select {
		case waited <- struct{}{}:
		default:
		}
	}
}

// holdAsync queues r on its own goroutine and delivers the admitted holding.
func holdAsync(l *Ledger, r Reservation, makeRoom func()) chan *Holding {
	got := make(chan *Holding, 1)
	go func() {
		h, err := l.Hold(context.Background(), r, makeRoom)
		if err != nil {
			panic(err)
		}
		got <- h
	}()
	return got
}

// A file larger than the whole allocation still runs while parser workers hold
// their bases: a Parse reservation at the head is granted whenever no other
// Parse is held, and a second one waits for the first. Mutations this fails on:
//   - granting the head on "nothing admitted" alone (checking admitted rather
//     than the held Parse count): the first file waits forever behind the
//     worker bases that never leave while the pool is up;
//   - granting every Parse head whatever the sum: the second file runs beside
//     the first although together they exceed the allocation;
//   - release not returning the Parse count: the second file never runs.
func TestParseForwardProgressWhileBasesAreHeld(t *testing.T) {
	l, err := NewLedger(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	baseA, err := l.Hold(ctx, Reservation{MemoryBytes: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer baseA.Release()
	baseB, err := l.Hold(ctx, Reservation{MemoryBytes: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer baseB.Release()

	// Were the first file held, its makeRoom step would cancel the wait.
	firstCtx, firstCancel := context.WithCancel(ctx)
	defer firstCancel()
	first, err := l.Hold(firstCtx, Reservation{MemoryBytes: 500, Parse: true}, firstCancel)
	if err != nil {
		t.Fatalf("a file larger than the allocation was held while no other parse was in flight: %v", err)
	}

	waited, signal := waitSignal()
	second := holdAsync(l, Reservation{MemoryBytes: 1, Parse: true}, signal)
	select {
	case <-second:
		t.Fatal("a second file ran beside a parse in flight although the sum exceeds the allocation")
	case <-waited:
	}

	// The sum still does not fit, so only the forward-progress rule admits it.
	first.Release()
	(<-second).Release()
}

// A file that runs alone waits for every parse in flight however much room
// is left, and no parse runs beside it. It is the reservation of a file whose
// prediction has already fallen short, so a parse beside it could take room
// the file then outgrows. Mutations this fails on:
//   - admitting a Parse that runs alone on the sum alone: it runs beside the
//     parse in flight;
//   - admitting a Parse head on the sum while one that runs alone is held: it
//     runs beside it;
//   - release not returning the count of those that run alone: the last file
//     never runs.
func TestAParseThatRunsAloneRunsBesideNoOtherParse(t *testing.T) {
	l, err := NewLedger(1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inFlight, err := l.Hold(ctx, Reservation{MemoryBytes: 1, Parse: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waited, signal := waitSignal()
	alone := holdAsync(l, Reservation{MemoryBytes: 1, Parse: true, Alone: true}, signal)
	select {
	case <-alone:
		t.Fatal("a file that runs alone ran beside a parse in flight")
	case <-waited:
	}
	inFlight.Release()
	held := <-alone

	waited, signal = waitSignal()
	beside := holdAsync(l, Reservation{MemoryBytes: 1, Parse: true}, signal)
	select {
	case <-beside:
		t.Fatal("a parse ran beside a file that runs alone although it fitted the sum")
	case <-waited:
	}
	held.Release()
	(<-beside).Release()
}

// Adjust takes an upward figure at once and counts it, and a downward one
// admits a waiter. Mutations this fails on:
//   - Adjust upward not adding to the reserved sum: the ledger admits a child
//     into memory a worker already uses, which is how a host is overcommitted;
//   - Adjust downward not pumping: the waiter is admitted only by an unrelated
//     release, and here none comes;
//   - Release returning the reserved figure rather than the adjusted one, or
//     returning it twice: the reserved sum ends other than the waiter's own.
func TestAdjustTakesUpwardAtOnceAndDownwardAdmits(t *testing.T) {
	l, err := NewLedger(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	h, err := l.Hold(context.Background(), Reservation{MemoryBytes: 50}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Adjust(300)
	if _, reserved := l.Snapshot(); reserved != 300 {
		t.Fatalf("an upward adjustment beyond the allocation was not counted: reserved %d, want 300", reserved)
	}

	waited, signal := waitSignal()
	w := holdAsync(l, Reservation{MemoryBytes: 10}, signal)
	select {
	case <-w:
		t.Fatal("a child was admitted into memory an adjusted holding already uses")
	case <-waited:
	}
	h.Adjust(40)
	waiter := <-w
	defer waiter.Release()

	h.Release()
	h.Release()
	h.Adjust(1 << 40)
	if _, reserved := l.Snapshot(); reserved != 10 {
		t.Fatalf("after the adjusted holding was released, reserved %d, want the waiter's 10", reserved)
	}
}

// A re-derived allocation takes nothing back and gates the next reservation.
// Mutations this fails on:
//   - SetAllocation releasing or shrinking what is held: the holding's bytes
//     leave the sum while its child still uses them;
//   - SetAllocation not pumping: the waiter is not admitted when the
//     allocation rises, although it now fits;
//   - accepting a negative figure: every later reservation waits on an
//     allocation no release can satisfy.
func TestSetAllocationKeepsHoldingsAndGatesTheNext(t *testing.T) {
	l, err := NewLedger(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	h, err := l.Hold(context.Background(), Reservation{MemoryBytes: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	l.SetAllocation(50)
	l.SetAllocation(-1)
	if allocation, reserved := l.Snapshot(); allocation != 50 || reserved != 80 {
		t.Fatalf("after lowering the allocation: allocation %d reserved %d, want 50 and 80", allocation, reserved)
	}

	waited, signal := waitSignal()
	w := holdAsync(l, Reservation{MemoryBytes: 10}, signal)
	select {
	case <-w:
		t.Fatal("a child was admitted above a lowered allocation")
	case <-waited:
	}
	l.SetAllocation(200)
	(<-w).Release()
}

// warmHistogram holds one outlier observed first at 100 bytes per source byte,
// decayed by a one-file half-life under 98 files at 10, then one more at 10
// when warm is set: 99 observations, or exactly the 100 of the cutover.
func warmHistogram(warm bool) *NeedHistogram {
	h := &NeedHistogram{}
	h.Observe(100*1000, 1000, 1)
	for range 98 {
		h.Observe(10*1000, 1000, 1)
	}
	if warm {
		h.Observe(10*1000, 1000, 1)
	}
	return h
}

// inBucket reports whether a prediction for 1,000 source bytes is the upper
// edge of the 5% bucket holding ratio: at least ratio x 1,000, under 5% above.
func inBucket(got int64, ratio float64) bool {
	return float64(got) >= ratio*1000 && float64(got) < ratio*1000*1.05
}

// The model reserves the sample maximum below 100 observations and the
// weighted p99 from 100 on. Mutations this fails on:
//   - the percentile below the cutover: the 99-file model reserves the common
//     ratio, and the outlier that occurred overruns on its next occurrence
//     while the model still rests on few samples;
//   - the maximum from the cutover on: one decayed outlier holds every later
//     file's reservation at ten times its need, and concurrency with it;
//   - the cutover taken above 100 observations rather than at it.
func TestNeedHistogramMaximumThenPercentile(t *testing.T) {
	if got, ok := warmHistogram(false).Predict(1000); !ok || !inBucket(got, 100) {
		t.Fatalf("below 100 observations the model reserved %d (ok %v), want the sample maximum near 100000", got, ok)
	}
	if got, ok := warmHistogram(true).Predict(1000); !ok || !inBucket(got, 10) {
		t.Fatalf("at 100 observations the model reserved %d (ok %v), want the p99 near 10000", got, ok)
	}
	if _, ok := (&NeedHistogram{}).Predict(1000); ok {
		t.Fatal("an empty model claimed a prediction")
	}
}

// The persisted state predicts what the live model predicts, and a malformed
// state is an error. Mutations this fails on:
//   - the encoding dropping the observation count: the decoded model claims
//     no observation, and its key's files fall back to the prior;
//   - UnmarshalBinary yielding an empty model for bytes it cannot read: every
//     file of the key is then reserved at the prior as if never seen.
func TestNeedHistogramEncodingRoundTrip(t *testing.T) {
	live := warmHistogram(false)
	live.Observe(0, 1000, 1)
	state, err := live.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded := &NeedHistogram{}
	if err := decoded.UnmarshalBinary(state); err != nil {
		t.Fatalf("the encoded state was refused: %v", err)
	}
	for _, size := range []int64{1, 1000, 1 << 20} {
		want, _ := live.Predict(size)
		if got, ok := decoded.Predict(size); !ok || got != want {
			t.Fatalf("for %d source bytes the decoded model reserved %d (ok %v), the live one %d", size, got, ok, want)
		}
	}

	for _, bad := range []string{
		"", "not json", `{"observations":-1,"zero":0,"buckets":[]}`,
		`{"observations":1,"zero":0,"buckets":[{"index":3,"weight":-1}]}`,
		`{"observations":2,"zero":0,"buckets":[{"index":3,"weight":1},{"index":2,"weight":1}]}`,
		`{"observations":1,"zero":0,"buckets":[]}`,
		`{"observations":1,"zero":0,"buckets":[{"index":3,"weight":1}]} trailing`,
	} {
		h := warmHistogram(true)
		if err := h.UnmarshalBinary([]byte(bad)); err == nil {
			t.Fatalf("the malformed state %q was accepted", bad)
		}
		if got, ok := h.Predict(1000); !ok || !inBucket(got, 10) {
			t.Fatalf("a refused state %q changed the model: reserved %d (ok %v)", bad, got, ok)
		}
	}
}
