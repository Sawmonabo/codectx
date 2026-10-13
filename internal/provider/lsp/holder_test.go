package lsp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// TestAnIdleServersRoomGoesToAnyReservationThatWaits protects the reclaim of
// an idle server's room: when any reservation at the head of the ledger's
// queue -- here an indexer unit's, which brings no make-room step of its own
// -- does not fit, an idle server is stopped and the reservation is granted
// once its process has exited, instead of waiting for the server's idle TTL.
//
// Mutation: drop the manager's Holder registration in New -> the unit waits
// for the hour-long idle TTL and the guard fails the test.
func TestAnIdleServersRoomGoesToAnyReservationThatWaits(t *testing.T) {
	h := providertest.New(t, map[string]string{"main.go": mainGo})
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 1, MemoryBudgetBytes: 16 << 30, DiskBudgetBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODECTX_LSP_FAKE", "1")
	t.Setenv("CODECTX_LSP_FAKE_ENCODING", "utf-16")
	resolver := offlineResolver(t, map[string]toolchain.Override{
		"gopls": {Executable: exe, Version: "1.2.3", Checksum: fileDigest(t, exe)},
	})
	ctx := context.Background()
	profile, err := Resolve(ctx, resolver, config.Defaults(), "gopls")
	if err != nil {
		t.Fatal(err)
	}
	profile.EnvAllowlist = append(profile.EnvAllowlist, "CODECTX_LSP_FAKE", "CODECTX_LSP_FAKE_ENCODING")
	led := testAdmission(t, 8<<30)
	mgr, err := New(Options{Runner: runner, DataDir: h.Policy.DataDir, Admission: led, IdleTTL: time.Hour,
		StopTimeout: 500 * time.Millisecond, RequestStallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	ov, err := mgr.Open(ctx, h.View, profile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ov.Close()
	allocation, reserved := led.Snapshot()
	if reserved <= 0 || mgr.Servers() != 1 {
		t.Fatalf("the idle server holds %d bytes and %d servers run; the check below would prove nothing", reserved, mgr.Servers())
	}
	guard, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unit := func(ctx context.Context, l *admission.Ledger) (*admission.Holding, error) {
		return l.Hold(ctx, admission.Reservation{MemoryBytes: allocation - reserved + 1}, nil)
	}
	held, err := unit(guard, led)
	if err != nil {
		t.Fatalf("a unit that did not fit beside an idle server waited for its idle TTL: %v", err)
	}
	defer held.Release()
	if mgr.Servers() != 0 {
		t.Fatalf("the unit was granted while %d servers still run", mgr.Servers())
	}
}
