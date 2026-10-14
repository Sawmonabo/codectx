package scip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/hosttool"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// fileDigestOf is the SHA-256 a toolchain override must declare for path.
func fileDigestOf(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// overrideResolver resolves each named kind to the given executable, offline.
func overrideResolver(t *testing.T, exe string, kinds ...Kind) *toolchain.Resolver {
	t.Helper()
	sum := fileDigestOf(t, exe)
	overrides := map[string]toolchain.Override{}
	for _, k := range kinds {
		overrides[string(k)] = toolchain.Override{Executable: exe, Version: "1", Checksum: sum}
	}
	r, err := toolchain.New(toolchain.Options{DataDir: t.TempDir(), Offline: true, MaxFetchBytes: 1 << 20, Overrides: overrides})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testRunner(t *testing.T) *process.Runner {
	t.Helper()
	r, err := process.NewRunner(process.Limits{MaxConcurrent: 1, MemoryBudgetBytes: 16 << 30, DiskBudgetBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// noGoAnywhere stands in for a machine with no Go toolchain on PATH, under
// GOROOT or in any install location.
func noGoAnywhere(t *testing.T) {
	t.Helper()
	saved := findHost
	findHost = func(name string, lookup hosttool.Lookup) (string, bool) {
		if name == "go" {
			return "", false
		}
		return saved(name, lookup)
	}
	t.Cleanup(func() { findHost = saved })
}

// TestAGoProjectWithNoGoAnywhereIsRefusedBeforeAnyUnit protects the refusal
// that replaced a wasted pass. scip-go runs the host go command and exits 1
// at once without it; when the go command was found nowhere, every Go project
// of a repository was still planned as a unit, and each unit first copied the
// whole snapshot into a private directory for a run that could not start
// (measured: 41 units, six minutes, on a 31,407-file repository whose indexer
// was started with a service manager's PATH).
//
// Mutation: drop the construction-time check and the Go project plans a unit
// again, while Detect reports the provider healthy.
func TestAGoProjectWithNoGoAnywhereIsRefusedBeforeAnyUnit(t *testing.T) {
	ctx := context.Background()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	newProvider := func(t *testing.T) *Provider {
		t.Helper()
		p, err := New(ctx, Options{WorkDir: t.TempDir(), Runner: testRunner(t), Resolver: overrideResolver(t, exe, KindGo, KindRust)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	found := newProvider(t).Descriptor().Version
	noGoAnywhere(t)

	t.Run("beside a language that can run, Go is named as refused with its reason", func(t *testing.T) {
		h := providertest.New(t, map[string]string{"go.mod": "module m\n", "a.go": "package m\n",
			"Cargo.toml": "[package]\nname = \"r\"\nversion = \"0.1.0\"\n", "src/lib.rs": "pub fn a() {}\n"})
		p := newProvider(t)
		det, err := p.Detect(ctx, h.Root, h.Policy)
		if err != nil {
			t.Fatal(err)
		}
		if !det.Available {
			t.Fatal("Detect reports unavailable although the Rust project can be indexed")
		}
		if got := det.Details[string(KindGo)]; got != model.CodeProviderUnavailable {
			t.Errorf("details[scip-go] = %q, want %s", got, model.CodeProviderUnavailable)
		}
		if !strings.Contains(det.Reason, "scip-go runs the go command") || !strings.Contains(det.Reason, "GOROOT") {
			t.Errorf("Reason = %q; it must say the go command was found nowhere and where it was looked for", det.Reason)
		}
		scopes := p.Scopes(det)
		if !slices.Equal(scopes, []string{ProfileScope(string(KindRust), "")}) {
			t.Errorf("Scopes = %v; want the Rust project alone, no Go unit", scopes)
		}
	})

	t.Run("alone, Go makes the provider unavailable with the reason", func(t *testing.T) {
		h := providertest.New(t, map[string]string{"go.mod": "module m\n", "a.go": "package m\n"})
		p := newProvider(t)
		det, err := p.Detect(ctx, h.Root, h.Policy)
		if err != nil {
			t.Fatal(err)
		}
		if det.Available || det.DiagnosticCode != model.CodeProviderUnavailable {
			t.Fatalf("Detect = available %v, code %q; want unavailable, %s", det.Available, det.DiagnosticCode, model.CodeProviderUnavailable)
		}
		if !strings.Contains(det.Reason, "go command") {
			t.Errorf("Reason = %q; it must name the missing go command", det.Reason)
		}
		if scopes := p.Scopes(det); len(scopes) != 0 {
			t.Errorf("Scopes = %v, want none", scopes)
		}
	})

	t.Run("the refusal does not rekey the provider", func(t *testing.T) {
		if got := newProvider(t).Descriptor().Version; got != found {
			t.Errorf("version without go = %q, with go = %q; the payloads did not change, so the version must not", got, found)
		}
	})
}

// TestAGoIndexerFindsGoOutsideAShortPATH protects the reason Go indexing
// failed under a service manager: its PATH lacked the directory the user's Go
// is in. The child's PATH is the parent's with the found toolchain's directory
// appended, and a kind with no host command keeps the parent's PATH as it is.
//
// Mutation: pass the parent's PATH through unchanged and the child gets a PATH
// with no go on it.
func TestAGoIndexerFindsGoOutsideAShortPATH(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	short := t.TempDir()
	vars := map[string]string{"PATH": short, "GOROOT": root, "GOENV": "off", "HOME": t.TempDir()}
	lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }

	pathOf := func(env []string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				return v
			}
		}
		return ""
	}
	if got, want := pathOf(Profile{Kind: KindGo}.env(lookup)), short+string(os.PathListSeparator)+bin; got != want {
		t.Errorf("scip-go PATH = %q, want %q", got, want)
	}
	if got := pathOf(Profile{Kind: KindPython}.env(lookup)); got != short {
		t.Errorf("scip-python PATH = %q, want the parent's %q unchanged", got, short)
	}
}

// TestAFailedIndexerKeepsItsOwnWords protects the one record of why an
// indexer failed. The runner's error says only that the tool exited; what the
// tool printed is the cause ("go command required, not found" was the cause of
// every Go unit failing for weeks), and it was dropped, so the run said only
// "exited with status 1". The tail of standard error now travels as the
// stderr_tail detail, which the run keeps and no capability row carries.
//
// Mutation: drop the detail and the failure is again a bare exit status.
func TestAFailedIndexerKeepsItsOwnWords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in indexer is a shell script")
	}
	ctx := context.Background()
	script := filepath.Join(t.TempDir(), "scip-go")
	body := "#!/bin/sh\nprintf 'Resolving module name\\nLoading Packages\\n' >&2\n" +
		"printf 'error: err: go command required, not found\\n' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"go.mod": "module m\n", "a.go": "package m\n"}
	h := providertest.New(t, files)
	p, err := New(ctx, Options{WorkDir: t.TempDir(), Runner: testRunner(t), Resolver: overrideResolver(t, script, KindGo)})
	if err != nil {
		t.Fatal(err)
	}
	det, err := p.Detect(ctx, h.Root, h.Policy)
	if err != nil {
		t.Fatal(err)
	}
	scopes := p.Scopes(det)
	if len(scopes) != 1 {
		t.Fatalf("Scopes = %v, want the one Go project", scopes)
	}
	inputs := []string{"a.go", "go.mod"}
	u := h.Plan(t, p, scopes[0], inputs)
	_, err = p.Import(ctx, u.Request, h.Begin(t, u, inputs), ImportOptions{})
	typed, ok := errors.AsType[*model.Error](err)
	if !ok {
		t.Fatalf("Import error = %v, want a typed failure", err)
	}
	tail := typed.Details[model.DetailStderrTail]
	if !strings.Contains(tail, "go command required, not found") {
		t.Fatalf("stderr_tail = %q; the indexer's own error must be kept (details %v)", tail, typed.Details)
	}
	if !strings.HasSuffix(tail, "not found") {
		t.Errorf("stderr_tail = %q; it must end where the indexer's output ends", tail)
	}
}

// TestStderrTailKeepsTheEnd protects the bound's direction: an indexer prints
// its progress first and its error last, so a bound that kept the front would
// keep only progress.
func TestStderrTailKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("Visiting Packages [1/9]\n", 200) + "é error: the cause\n"
	got := stderrTail([]byte(long))
	if len(got) > model.MaxDetailBytes {
		t.Fatalf("tail is %d bytes, over the %d bound", len(got), model.MaxDetailBytes)
	}
	if !strings.HasSuffix(got, "error: the cause") {
		t.Errorf("tail = %q; want it to end with the error", got[max(0, len(got)-40):])
	}
	// Two-byte runes then one ASCII byte: the bound's cut falls on the second
	// byte of a rune, which must not lead the tail.
	if got := stderrTail([]byte(strings.Repeat("é", model.MaxDetailBytes) + "z")); !strings.HasPrefix(got, "é") || !strings.HasSuffix(got, "z") {
		t.Errorf("a cut through a multi-byte rune left a partial rune at the front: %q", got[:4])
	}
}
