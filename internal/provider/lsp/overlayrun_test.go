//go:build unix

package lsp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// TestAServerStartIsRecordedAgainstTheProcessOverlayRun protects both halves of
// how work that belongs to no generation is accounted for.
//
// A language server start is lazy, pooled and shared between generations, so it
// can be attributed to none of them: recorded under a generation's run it would
// be charged to whichever run happened to trigger it, and recorded nowhere it
// would be the one multi-second cost of a query that the product cannot
// account for at all. It therefore hangs off a run of its own, one per process.
//
// The second half is the price of that run: it belongs to no generation, so the
// retention that deletes a generation's runs never reaches it, and a process
// that did not take its own row with it would leave one run and its spans in
// the file for ever -- unbounded growth in a file whose whole purpose is to be
// read live.
func TestAServerStartIsRecordedAgainstTheProcessOverlayRun(t *testing.T) {
	h := providertest.New(t, map[string]string{"main.go": mainGo, "util.go": utilGo})
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

	// The one ledger the composition root opens beside the store. The manager
	// is handed it and never opens one of its own.
	led, err := ledger.Open(ctx, h.Policy.DataDir)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	recorded := make(chan ledger.SpanRow, 8)
	led.Subscribe(func(row ledger.SpanRow) {
		if row.Stage == stageServerStart {
			select {
			case recorded <- row:
			default:
			}
		}
	})
	mgr, err := New(Options{Runner: runner, DataDir: h.Policy.DataDir, AllocationBytes: 8 << 30,
		IdleTTL: 200 * time.Millisecond, StopTimeout: 500 * time.Millisecond,
		RequestStallTimeout: 10 * time.Second, StartTimeout: 30 * time.Second, Ledger: led})
	if err != nil {
		t.Fatal(err)
	}
	ov, err := mgr.Open(ctx, h.View, profile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var row ledger.SpanRow
	select {
	case row = <-recorded:
	case <-time.After(60 * time.Second):
		t.Fatal("the start of a language server was recorded nowhere: the one multi-second cost of an " +
			"overlay query is unaccounted for")
	}
	if row.Outcome != ledger.OutcomeOK || row.Provider != profile.Name {
		t.Fatalf("the server start recorded outcome %q for provider %q, want ok for %q",
			row.Outcome, row.Provider, profile.Name)
	}
	repo := string(h.View.Header().RepositoryID)
	reader, ok, err := ledger.OpenReader(ctx, h.Policy.DataDir)
	if err != nil || !ok {
		t.Fatalf("OpenReader: %v, present=%v", err, ok)
	}
	view, ok, err := reader.LatestRun(ctx, repo, 0)
	if err != nil || !ok {
		t.Fatalf("LatestRun: %v, present=%v", err, ok)
	}
	if view.Run.RunID != row.RunID {
		t.Fatalf("the server start was recorded against run %s, want the process's own run %s",
			row.RunID, view.Run.RunID)
	}
	if view.Run.Kind != ledger.KindOverlay || view.Run.GenerationID != nil {
		t.Fatalf("the server start's run is %q with generation %v, want an overlay run belonging to no "+
			"generation: a pooled server shared between generations cannot be charged to one of them",
			view.Run.Kind, view.Run.GenerationID)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	// Clean exit: the manager stops every server and takes its own run with it.
	if err := ov.Close(); err != nil {
		t.Fatalf("Overlay.Close: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Manager.Close: %v", err)
	}
	// Stopping the ledger is what would write a forgotten run back in, so the
	// assertion is made after it and not before.
	if err := led.Stop(); err != nil {
		t.Fatalf("ledger.Stop: %v", err)
	}
	reader, ok, err = ledger.OpenReader(ctx, h.Policy.DataDir)
	if err != nil || !ok {
		t.Fatalf("OpenReader: %v, present=%v", err, ok)
	}
	defer reader.Close()
	if view, ok, err := reader.LatestRun(ctx, repo, 0); err != nil || ok {
		t.Fatalf("a clean exit left the overlay run %s (%v): no generation will ever collect it, so the "+
			"ledger keeps one run and its spans per process for ever", view.Run.RunID, err)
	}
}
