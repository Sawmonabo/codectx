package treesitter_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
)

// TestMain makes this test binary its own parser worker, the way the codectx
// binary is in production (ruling R8-1): invoked with wire.Subcommand as its
// first argument it runs worker.Main instead of the tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fixtures is one small file per pinned grammar. Each carries a nested
// declaration and a non-ASCII string before a declaration, so a byte range
// that was computed in characters, or a line/column that was trusted from
// the child instead of derived from the bytes, misses its source.
var fixtures = []struct {
	file   string
	nested string // a declaration nested in another
	outer  string // the declaration it is nested in
}{
	{"sample.go", "inner", "Start"},
	{"sample.py", "inner", "start"},
	{"sample.js", "inner", "start"},
	{"sample.ts", "inner", "serve"},
	{"sample.tsx", "label", "View"},
	{"Sample.java", "run", "start"},
	{"sample.rs", "inner", "start"},
	{"sample.c", "local", "start"},
	{"sample.cpp", "Config", "Server"},
}

func newProvider(t *testing.T) *treesitter.Provider {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 4, MemoryBudgetBytes: 4 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	p, err := treesitter.New(treesitter.Options{
		MaxWorkers: 2, WorkerIdleTTL: 2 * time.Second, ParseTimeout: 30 * time.Second, WorkerMemoryBytes: 64 << 20,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}},
		Runner: runner, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestLanguageFixtures runs the shared conformance check over every pinned
// grammar and then asserts the one invariant the conformance check cannot
// see: every declaration's published byte range, read back from the sealed
// unit's facts, selects exactly the source bytes of that declaration, and
// its nested declaration is contained by the outer one.
//
// Failure mode: a worker that reported character offsets, a parent that
// trusted a child's line/column, or a chunked read callback that dropped a
// boundary would publish a range that does not match the bytes; the second
// conformance run with a flush after every Put would catch a relation or
// alias handed over before the node it references.
func TestLanguageFixtures(t *testing.T) {
	p := newProvider(t)
	for _, fx := range fixtures {
		t.Run(fx.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", fx.file))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(src), "日本") {
				t.Fatalf("fixture %s carries no non-ASCII range", fx.file)
			}
			files := map[string]string{fx.file: string(src)}
			providertest.Conform(t, p, files, treesitter.ScopePrefix+fx.file, []string{fx.file})

			h := providertest.New(t, files)
			u := h.Plan(t, p, treesitter.ScopePrefix+fx.file, []string{fx.file})
			cap := &capture{UnitOutput: h.Begin(t, u, []string{fx.file})}
			result, err := provider.RunUnit(context.Background(), p, u.Request, cap, providertest.Limits, h.Pool)
			if err != nil {
				t.Fatalf("RunUnit: %v", err)
			}
			if len(result.Capabilities) != 1 || result.Capabilities[0].State != model.CapabilityFresh {
				t.Fatalf("capability state = %+v, want one fresh structure state", result.Capabilities)
			}
			fv := h.File(t, fx.file)
			var nested *model.Node
			var outers []*model.Node
			for i := range cap.nodes {
				n := &cap.nodes[i].Node
				if n.FileID == "" && n.Range == nil {
					continue // an import target or an unresolved call placeholder has no location
				}
				if n.FileID != fv.ID || n.Range == nil {
					t.Fatalf("node %s %q has no located range in the unit's file", n.Kind, n.Name)
				}
				r := n.Range
				if r.End.Byte > uint64(len(src)) || r.Start.Byte > r.End.Byte {
					t.Fatalf("node %q range [%d,%d) does not fit %d bytes", n.Name, r.Start.Byte, r.End.Byte, len(src))
				}
				text := string(src[r.Start.Byte:r.End.Byte])
				if n.Kind != model.NodeModule && !strings.Contains(text, n.Name) {
					t.Fatalf("node %s %q range [%d,%d) selects %q, which does not contain its name", n.Kind, n.Name, r.Start.Byte, r.End.Byte, text)
				}
				if got := lineOf(src, r.Start.Byte); got != r.Start.Line {
					t.Fatalf("node %q start line %d, bytes say %d", n.Name, r.Start.Line, got)
				}
				if got := lineOf(src, r.End.Byte); got != r.End.Line {
					t.Fatalf("node %q end line %d, bytes say %d", n.Name, r.End.Line, got)
				}
				switch n.Name {
				case fx.nested:
					nested = n
				case fx.outer:
					outers = append(outers, n)
				}
			}
			if nested == nil {
				t.Fatalf("nested declaration %q was not extracted; nodes: %s", fx.nested, names(cap.nodes))
			}
			// The outer name may be shared (a C++ class and its constructor); one
			// of its declarations must contain the nested one.
			contained := false
			for _, o := range outers {
				if o.Range.Start.Byte <= nested.Range.Start.Byte && nested.Range.End.Byte <= o.Range.End.Byte {
					contained = true
				}
			}
			if !contained {
				t.Fatalf("nested %q [%d,%d) is not contained by any declaration named %q; nodes: %s", fx.nested,
					nested.Range.Start.Byte, nested.Range.End.Byte, fx.outer, names(cap.nodes))
			}
			for _, e := range cap.evidence {
				if e.Range != nil && (e.Range.End.Byte > uint64(len(src)) || e.FileID != fv.ID) {
					t.Fatalf("evidence range [%d,%d) is not in the unit's file", e.Range.Start.Byte, e.Range.End.Byte)
				}
			}
			if p.Stats().Processes > 2 {
				t.Fatalf("worker processes = %d, over the pool bound", p.Stats().Processes)
			}
		})
	}
	p.Close()
	if s := p.Stats(); s.Processes != 0 || s.WorkersExited != s.WorkersStarted {
		t.Fatalf("after Close: %+v, want every started worker exited", s)
	}
}

// capture records the facts a unit persisted so the test can read the
// ranges back rather than trust the provider's own view of them.
type capture struct {
	provider.UnitOutput
	nodes    []model.NodeFact
	evidence []model.Evidence
}

func (c *capture) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	c.nodes = append(c.nodes, facts...)
	for _, f := range facts {
		c.evidence = append(c.evidence, f.Evidence...)
	}
	return c.UnitOutput.PutNodes(ctx, facts)
}

func (c *capture) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	for _, f := range facts {
		c.evidence = append(c.evidence, f.Evidence...)
	}
	return c.UnitOutput.PutRelations(ctx, facts)
}

func lineOf(src []byte, offset uint64) uint32 {
	line := uint32(1)
	for _, b := range src[:offset] {
		if b == '\n' {
			line++
		}
	}
	return line
}

func names(facts []model.NodeFact) string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, string(f.Node.Kind)+":"+f.Node.Name)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}
