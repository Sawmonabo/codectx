package lsp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// TestPythonServerRunsAsItself pins the Definition ADR-0006 chose for python
// and, more importantly, the shape of its launch: the payload is a native
// binary, so the resolved argv is the executable itself with the definition's
// arguments after it and nothing composed in front. The previous python server
// was hosted by the managed Node runtime, and a definition that kept a
// runtime's argument shape would start the wrong process with no test failing.
func TestPythonServerRunsAsItself(t *testing.T) {
	def, ok := definitions["ty"]
	if !ok {
		t.Fatal("no python server definition named ty")
	}
	if want := []string{"python"}; !reflect.DeepEqual(def.Languages, want) {
		t.Fatalf("languages = %v, want %v", def.Languages, want)
	}
	if want := []string{"server"}; !reflect.DeepEqual(def.Args, want) {
		t.Fatalf("args = %v, want %v", def.Args, want)
	}
	if want := []string{"PATH", "HOME"}; !reflect.DeepEqual(def.EnvAllowlist, want) {
		t.Fatalf("env allowlist = %v, want %v", def.EnvAllowlist, want)
	}
	want := []string{"pyproject.toml", "ty.toml", "setup.py", "requirements.txt"}
	if !reflect.DeepEqual(def.RootMarkers, want) {
		t.Fatalf("root markers = %v, want %v", def.RootMarkers, want)
	}
	if len(def.RuntimeArgs) != 0 {
		t.Fatalf("runtime args = %v, want none: the payload is not runtime-hosted", def.RuntimeArgs)
	}
	// The lock must agree that it runs as itself, or Resolve would compose a
	// managed runtime in front of it.
	entry, ok := toolchain.Embedded().Tools["ty"]
	if !ok {
		t.Fatal("the lock carries no ty entry")
	}
	if entry.Runtime != "" {
		t.Fatalf("lock runtime = %q, want empty", entry.Runtime)
	}
	if entry.Kind != "server" {
		t.Fatalf("lock kind = %q, want server", entry.Kind)
	}
	if _, ok := toolchain.Embedded().Tools["pyright"]; ok {
		t.Fatal("the replaced python server is still in the lock")
	}

	// argv over a directly executable payload: exactly the binary and the
	// definition's own arguments.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resolver := offlineResolver(t, map[string]toolchain.Override{
		"ty": {Executable: exe, Version: "0.0.81", Checksum: fileDigest(t, exe)},
	})
	p, err := Resolve(context.Background(), resolver, config.Defaults(), "ty")
	if err != nil {
		t.Fatal(err)
	}
	path, args := p.argv("/in", "/work")
	if path != exe {
		t.Fatalf("argv[0] = %q, want the payload itself (%q)", path, exe)
	}
	if !reflect.DeepEqual(args, []string{"server"}) {
		t.Fatalf("args = %v, want [server] with nothing composed in front", args)
	}
}

// TestBindingCarriesTheServerReportAndEncoding is the digest invariant the
// python swap depends on. The new server reports serverInfo and negotiates
// utf-8 where the old one reported nothing and was used at the utf-16 default,
// so a cached python answer from before the swap must not be reused. Both the
// reported version and the negotiated encoding therefore have to reach
// InputDigest -- and the binding must carry the *reported* version rather than
// the lock's pinned one.
func TestBindingCarriesTheServerReportAndEncoding(t *testing.T) {
	open := func(t *testing.T, enc string, silent bool) (string, string) {
		t.Helper()
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
		t.Setenv("CODECTX_LSP_FAKE_ENCODING", enc)
		if silent {
			t.Setenv("CODECTX_LSP_FAKE_NO_SERVERINFO", "1")
		} else {
			os.Unsetenv("CODECTX_LSP_FAKE_NO_SERVERINFO")
		}
		resolver := offlineResolver(t, map[string]toolchain.Override{
			"gopls": {Executable: exe, Version: "pinned-9.9.9", Checksum: fileDigest(t, exe)},
		})
		ctx := context.Background()
		profile, err := Resolve(ctx, resolver, config.Defaults(), "gopls")
		if err != nil {
			t.Fatal(err)
		}
		profile.EnvAllowlist = append(profile.EnvAllowlist,
			"CODECTX_LSP_FAKE", "CODECTX_LSP_FAKE_ENCODING", "CODECTX_LSP_FAKE_NO_SERVERINFO")
		mgr, err := New(Options{Runner: runner, DataDir: h.Policy.DataDir, AllocationBytes: 8 << 30, IdleTTL: 200 * time.Millisecond,
			StopTimeout: 500 * time.Millisecond, RequestStallTimeout: 10 * time.Second, StartTimeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		ov, err := mgr.Open(ctx, h.View, profile)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ov.Close()
		events := eventLog{t: t, path: filepath.Join(profile.workDir(h.Policy.DataDir), "events.log")}
		if got := events.wait("encoding="); !strings.Contains(got, "encoding="+enc) {
			t.Fatalf("the fake did not negotiate %s: %v", enc, events.lines())
		}
		b := ov.Binding()
		if err := b.Validate(); err != nil {
			t.Fatalf("binding %+v: %v", b, err)
		}
		return b.ProviderVersion, b.InputDigest
	}

	// Each case changes exactly one of the two, so a digest that stopped
	// folding in either one is caught by its own comparison rather than
	// masked by the other.
	reportingVersion, utf8Digest := open(t, "utf-8", false)
	if reportingVersion != "1.2.3" {
		t.Fatalf("provider_version = %q, want the reported 1.2.3", reportingVersion)
	}
	// Same server report, different negotiated encoding.
	if _, utf16Digest := open(t, "utf-16", false); utf16Digest == utf8Digest {
		t.Fatalf("input digest %q is unchanged across a different position encoding", utf8Digest)
	}
	// Same encoding, no server report -- the shape of the server this one
	// replaced, which fell back to the version the lock pinned.
	silentVersion, silentDigest := open(t, "utf-8", true)
	if silentVersion != "pinned-9.9.9" {
		t.Fatalf("provider_version = %q, want the pinned fallback", silentVersion)
	}
	if silentDigest == utf8Digest {
		t.Fatalf("input digest %q is unchanged across a different server report", utf8Digest)
	}
}
