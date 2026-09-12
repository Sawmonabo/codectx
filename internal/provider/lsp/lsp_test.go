//go:build unix

package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// TestMain turns the test binary into the fake language server when the
// runner starts it with CODECTX_LSP_FAKE=1. No binary is committed; the
// fixture is this package's own test build.
func TestMain(m *testing.M) {
	if os.Getenv("CODECTX_LSP_FAKE") == "1" {
		os.Exit(runFakeServer())
	}
	os.Exit(m.Run())
}

// mainGo is the one fixture document. Line 2 (one-based) carries a two-byte
// and a four-byte code point before the queried identifier, so UTF-8, UTF-16
// and UTF-32 columns for the same byte all differ.
const mainGo = "package main\n" +
	"var s = \"héllo😀\"; var y = 1\n" +
	"func héllo(y int) int { return y }\n" +
	"func other() int { return héllo(1) }\n"

// TestFakeServerLifecycle is the one authorized fake-server scenario. Each
// case names the failure mode it protects:
//
//   - out-of-order replies matched by id: a swapped answer attributes one
//     symbol's definition to another (wrong source);
//   - negotiated UTF-16 / UTF-8 / UTF-32 positions resolved against the exact
//     bytes: a column counted in the wrong units names different bytes (wrong
//     source);
//   - cancellation releases the call and sends $/cancelRequest: a call that
//     never returns leaks its slot (resource leak);
//   - hostile / external URIs are never followed and never appear as snapshot
//     results (repository safety);
//   - an unadvertised method is unavailable, not a substituted answer (false
//     readiness);
//   - a server-initiated workspace/applyEdit is refused (repository safety);
//   - shutdown/exit leaves no process and no materialization (resource leak);
//   - a crashed server releases the pending call and the overlay reports
//     failure (resource leak, false readiness).
func TestFakeServerLifecycle(t *testing.T) {
	for _, enc := range []string{"utf-16", "utf-8", "utf-32"} {
		t.Run(enc, func(t *testing.T) { runScenario(t, enc) })
	}
}

func runScenario(t *testing.T, enc string) {
	h := providertest.New(t, map[string]string{"main.go": mainGo})
	file := h.File(t, "main.go")
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 2, MemoryBudgetBytes: 1 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(t.TempDir(), "work")
	t.Setenv("CODECTX_LSP_FAKE", "1")
	t.Setenv("CODECTX_LSP_FAKE_ENCODING", enc)
	cfg := config.Defaults()
	cfg.Analyzers = map[string]config.Analyzer{"gopls": {
		Executable: exe, VersionConstraint: "1.2", WorkDir: workDir,
		EnvAllowlist:      []string{"CODECTX_LSP_FAKE", "CODECTX_LSP_FAKE_ENCODING"},
		MemoryBudgetBytes: 1 << 20, DiskBudgetBytes: 1 << 20, Timeout: config.Duration(time.Minute), Network: config.NetworkDenied,
	}}
	// An unapproved name is a trust refusal, never a start.
	if _, err := Trusted(cfg, "clangd"); !isCode(err, model.CodeTrustRequired) {
		t.Fatalf("Trusted(clangd) = %v, want %s", err, model.CodeTrustRequired)
	}
	profile, err := Trusted(cfg, "gopls")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := New(Options{Runner: runner, DataDir: h.Policy.DataDir, IdleTTL: 200 * time.Millisecond,
		StopTimeout: 500 * time.Millisecond, RequestTimeout: 10 * time.Second, StartTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	ctx := context.Background()

	ov, err := mgr.Open(ctx, h.View, profile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := ov.Binding(); got.ProviderVersion != "1.2.3" || got.ProviderID != "lsp:gopls" || got.Validate() != nil {
		t.Fatalf("binding = %+v (%v)", got, got.Validate())
	}
	if ov.Capabilities().WorkspaceSymbols || !ov.Capabilities().CallHierarchy {
		t.Fatalf("capabilities = %+v", ov.Capabilities())
	}
	events := eventLog{t: t, path: filepath.Join(workDir, "events.log")}
	if !strings.Contains(events.wait("encoding="), "encoding="+enc) {
		t.Fatalf("the fake did not negotiate %s: %v", enc, events.lines())
	}
	at := func(needle string, nth int) At {
		off := 0
		for i := 0; i < nth; i++ {
			j := strings.Index(mainGo[off:], needle)
			if j < 0 {
				t.Fatalf("%q occurrence %d not in fixture", needle, nth)
			}
			off += j + len(needle)
		}
		return At{File: file.ID, Byte: uint64(off - len(needle))}
	}
	bytesOf := func(loc Location) string {
		return mainGo[loc.Range.Start.Byte:loc.Range.End.Byte]
	}

	// Out-of-order replies and position encoding. The fake holds the line-0
	// definition until the second request arrives and answers that one first;
	// both answers must still land on their own identifiers. The second query
	// sits after "héllo😀" on its line, so its column differs per encoding.
	var held Result[Location]
	var heldErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		held, heldErr = ov.Definition(ctx, at("main", 1), 10)
	}()
	events.wait("holding-definition")
	second, err := ov.Definition(ctx, at("y = 1", 1), 10)
	wg.Wait()
	if err != nil || heldErr != nil {
		t.Fatalf("Definition: %v / %v", err, heldErr)
	}
	if len(held.Items) != 1 || bytesOf(held.Items[0]) != "main" {
		t.Fatalf("held definition = %+v, want the bytes of \"main\"", held.Items)
	}
	if len(second.Items) != 1 || bytesOf(second.Items[0]) != "y" || second.Items[0].Range.Start.Byte != at("y = 1", 1).Byte {
		t.Fatalf("definition after multibyte text = %+v, want exactly the bytes of \"y\" at %d", second.Items, at("y = 1", 1).Byte)
	}
	if second.Items[0].File != file.ID || second.Items[0].ContentHash != file.ContentHash || second.Overlay != ov.Binding() {
		t.Fatalf("result is not labelled with the pinned file and overlay: %+v", second.Items[0])
	}

	// Server-initiated requests: configuration answered with no settings,
	// applyEdit refused with a protocol error.
	if !strings.Contains(events.wait("response=\"srv-2\""), "error=-32601") {
		t.Fatalf("workspace/applyEdit was not refused: %v", events.lines())
	}
	if !strings.Contains(strings.Join(events.lines(), "\n"), "response=\"srv-1\" result=[null,null]") {
		t.Fatalf("workspace/configuration was not answered with bounded nulls: %v", events.lines())
	}

	// Unadvertised method: unavailable without a request being sent.
	if _, err := ov.WorkspaceSymbols(ctx, "main", 10); !isCode(err, model.CodeProviderUnavailable) {
		t.Fatalf("WorkspaceSymbols = %v, want %s", err, model.CodeProviderUnavailable)
	}

	// Hostile and external URIs: locations outside the materialization are
	// excluded, never opened; a non-file URI is a protocol violation.
	typeDef, err := ov.TypeDefinition(ctx, at("y = 1", 1), 10)
	if err != nil {
		t.Fatalf("TypeDefinition: %v", err)
	}
	if typeDef.Excluded != 2 || len(typeDef.Items) != 1 || typeDef.Items[0].Path != "main.go" {
		t.Fatalf("TypeDefinition = %+v, want 2 excluded and only main.go", typeDef)
	}
	if _, err := ov.Implementations(ctx, at("y = 1", 1), 10); !isCode(err, model.CodeProviderOutputInvalid) {
		t.Fatalf("Implementations over an https URI = %v, want %s", err, model.CodeProviderOutputInvalid)
	}

	// Document symbols and call hierarchy over the same validated bytes.
	syms, err := ov.DocumentSymbols(ctx, file.ID, 10)
	if err != nil || len(syms.Items) != 3 || syms.Items[1].Name != "héllo" || syms.Items[1].Container != "main" ||
		syms.Items[1].Kind != model.NodeFunction || bytesOf(Location{Range: *syms.Items[1].Location.Selection}) != "héllo" {
		t.Fatalf("DocumentSymbols = %+v, %v", syms.Items, err)
	}
	items, err := ov.PrepareCallHierarchy(ctx, at("héllo(y", 1), 10)
	if err != nil || len(items.Items) != 1 || items.Items[0].Name != "héllo" {
		t.Fatalf("PrepareCallHierarchy = %+v, %v", items.Items, err)
	}
	calls, err := ov.IncomingCalls(ctx, items.Items[0], 10)
	if err != nil || len(calls.Items) != 1 || calls.Items[0].Item.Name != "other" || len(calls.Items[0].Sites) != 1 ||
		bytesOf(calls.Items[0].Sites[0]) != "héllo" || calls.Items[0].Sites[0].Range.Start.Byte != at("héllo(1)", 1).Byte {
		t.Fatalf("IncomingCalls = %+v, %v", calls.Items, err)
	}
	// Outgoing calls take the other site-attribution branch: the sites belong
	// to the queried caller's document, not the peer's, and must resolve to
	// the exact bytes there.
	out, err := ov.OutgoingCalls(ctx, items.Items[0], 10)
	if err != nil || len(out.Items) != 1 || out.Items[0].Item.Name != "other" || len(out.Items[0].Sites) != 1 ||
		out.Items[0].Sites[0].File != items.Items[0].Location.File ||
		bytesOf(out.Items[0].Sites[0]) != "y" || out.Items[0].Sites[0].Range.Start.Byte != at("y }", 1).Byte {
		t.Fatalf("OutgoingCalls = %+v, %v", out.Items, err)
	}

	// Cancellation: the fake holds the references request until it sees
	// $/cancelRequest. The call must return as canceled, the cancel must reach
	// the server and the connection must remain usable.
	cctx, cancel := context.WithCancel(ctx)
	var refErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, refErr = ov.References(cctx, at("other", 1), true, 10)
	}()
	events.wait("holding-references")
	cancel()
	wg.Wait()
	if !isCode(refErr, model.CodeCanceled) {
		t.Fatalf("canceled References = %v, want %s", refErr, model.CodeCanceled)
	}
	events.wait("cancelled=")
	if after, err := ov.Definition(ctx, at("y = 1", 1), 10); err != nil || len(after.Items) != 1 {
		t.Fatalf("Definition after cancellation = %+v, %v", after.Items, err)
	}

	// Shutdown: closing the last overlay starts the idle TTL; afterwards the
	// server received shutdown and exit, its process is gone and the
	// materialization is removed.
	pid := events.pid()
	ov.Close()
	events.wait("exit")
	waitFor(t, "server process to exit", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	waitFor(t, "materialization to be removed", func() bool { return materializations(t, h.Policy.DataDir) == 0 })
	if !strings.Contains(strings.Join(events.lines(), "\n"), "shutdown") {
		t.Fatalf("shutdown was never requested: %v", events.lines())
	}
	if mgr.Servers() != 0 {
		t.Fatalf("manager still tracks %d servers after idle stop", mgr.Servers())
	}

	// Crash: a fresh server exits without answering. The pending call is
	// released with a typed failure, the overlay is failed afterwards, and
	// nothing is left behind.
	ov2, err := mgr.Open(ctx, h.View, profile)
	if err != nil {
		t.Fatalf("Open after idle stop: %v", err)
	}
	defer ov2.Close()
	pid = events.pid()
	start := time.Now()
	if _, err := ov2.References(ctx, at("héllo😀", 1), true, 10); !isCode(err, model.CodeProviderUnavailable) {
		t.Fatalf("References into a crashing server = %v, want %s", err, model.CodeProviderUnavailable)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the pending call was held %s after the server died", elapsed)
	}
	if _, err := ov2.Definition(ctx, at("main", 1), 10); !isCode(err, model.CodeProviderUnavailable) {
		t.Fatalf("Definition on a failed overlay = %v, want %s", err, model.CodeProviderUnavailable)
	}
	waitFor(t, "crashed process to be reaped", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	waitFor(t, "crashed materialization to be removed", func() bool { return materializations(t, h.Policy.DataDir) == 0 })
	if mgr.Servers() != 0 {
		t.Fatalf("manager still tracks %d servers after a crash", mgr.Servers())
	}
	if strings.Contains(strings.Join(events.lines(), "\n"), "seen-workspace-symbol") {
		t.Fatal("workspace/symbol reached a server that never advertised it")
	}
}

func isCode(err error, code string) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == code
}

// materializations counts live mat-* trees under the data directory.
func materializations(t *testing.T, dataDir string) int {
	t.Helper()
	entries, err := os.ReadDir(snapshot.MaterializeDir(dataDir))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// eventLog reads the fake server's event file.
type eventLog struct {
	t    *testing.T
	path string
}

func (e eventLog) lines() []string {
	raw, err := os.ReadFile(e.path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// wait blocks until a line with the prefix appears and returns the last such
// line.
func (e eventLog) wait(prefix string) string {
	e.t.Helper()
	var found string
	waitFor(e.t, "event "+prefix, func() bool {
		for _, l := range e.lines() {
			if strings.HasPrefix(l, prefix) {
				found = l
			}
		}
		return found != ""
	})
	return found
}

// pid returns the most recently started fake server's pid.
func (e eventLog) pid() int {
	e.t.Helper()
	line := e.wait("pid=")
	pid, err := strconv.Atoi(strings.TrimPrefix(line, "pid="))
	if err != nil {
		e.t.Fatal(err)
	}
	return pid
}
