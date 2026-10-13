package ledger_test

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/ledger"
)

// TestNeedModelsAreKeyedByTheirKind protects what a learned need is a need of:
// a parsed file's memory and an external tool's memory and disk are separate
// models even when their subject, fingerprint, build and size class agree, each
// read back under its own kind, and an observation of a kind the store does
// not hold is dropped rather than written.
//
// Mutation: leave kind out of the need_models key, or out of NeedModel's
// WHERE -> the tool's disk observation lands on, or reads back as, the parse
// model. Mutation: drop the kind check in ObserveNeed -> the unknown kind
// fails the batch's CHECK and loses it.
func TestNeedModelsAreKeyedByTheirKind(t *testing.T) {
	ctx := context.Background()
	l, dir := openLedger(t)
	defer func() {
		if err := l.Stop(); err != nil {
			t.Errorf("stop the ledger: %v", err)
		}
	}()
	run, runCtx := newRun(t, l)
	_, span := ledger.Start(runCtx, "parse", "")
	span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	parse := ledger.NeedKey{Kind: ledger.NeedParse, Subject: "go", Fingerprint: "f", Build: "b", SizeClass: 12}
	disk := parse
	disk.Kind = ledger.NeedIndexerDisk
	run.ObserveNeed(ledger.NeedObservation{Key: parse, ReservedBytes: 10, NeedBytes: 9, State: []byte("parse")})
	run.ObserveNeed(ledger.NeedObservation{Key: disk, ReservedBytes: 10, NeedBytes: 20, State: []byte("disk")})
	unknown := parse
	unknown.Kind = "network"
	run.ObserveNeed(ledger.NeedObservation{Key: unknown, ReservedBytes: 1, NeedBytes: 1, State: []byte("x")})
	if err := l.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for key, want := range map[ledger.NeedKey]string{parse: "parse", disk: "disk"} {
		got, found, err := run.NeedModel(ctx, key)
		if err != nil || !found || string(got) != want {
			t.Fatalf("the %s model reads %q found=%v (%v), want %q", key.Kind, got, found, err, want)
		}
	}
	if c := needClass(t, dir, disk); c.Observations != 1 || c.Overruns != 1 {
		t.Fatalf("the disk class reads %+v, want its own one observation and one overrun", c)
	}
	if c := needClass(t, dir, parse); c.Observations != 1 || c.Overruns != 0 {
		t.Fatalf("the parse class reads %+v, want its own one observation and no overrun", c)
	}
	if _, found, err := run.NeedModel(ctx, unknown); err != nil || found {
		t.Fatalf("an observation of an unknown kind was stored: found=%v (%v)", found, err)
	}
}
