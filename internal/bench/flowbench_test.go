package bench

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
)

// This file measures the per-function dependence core in this process: the
// parse memory of all nine grammars, the per-function cost of every analysis
// pass for the languages that have a lowering, and the per-file cost of
// parsing, lowering and analysing a whole file. Each measurement is one JSON
// object per row, written to the file CODECTX_BENCH_OUT names (truncated once
// per run by TestMain) or, when it is unset, to the test log. The `row` field
// names the row's kind:
//
//	parse     one per input file of any language (TestParseMemory)
//	function  one per callable of a lowered language (TestFunctionAnalysis)
//	file      one per input file of a lowered language (TestFileAnalysis)
//
// Inputs are the provider's nine grammar fixtures, this package's control-flow
// fixtures, and, when CODECTX_BENCH_TREE names a directory, every regular file
// under it that the language table classifies — walked read-only through
// os.Root, skipping the .git directory and following no symlink, with no
// limit on files or bytes. A file that cannot be read fails the test and is
// never measured as empty.
//
// Native bytes are what the counting allocator (allocator.go) sees: the core
// parser library's allocations, not the grammar scanners' nor the binding's
// input copies, which are reported beside them. A resident-set figure the
// platform cannot report is JSON null, never 0.

// parseChunk is the size of each slice the read callback hands the parser,
// the same as the worker's parseChunkBytes (worker/extract.go): the binding
// copies every slice into a C string held until the parse ends, so the chunk
// decides that transient cost and must match the worker's for the rows to
// predict it.
const parseChunk = 64 << 10

// rows is the benchmark row sink; see openRows.
var rows struct {
	mu sync.Mutex
	f  *os.File
}

// openRows truncates and opens the file CODECTX_BENCH_OUT names, once per
// run, so every test's rows accumulate in one file the controller aggregates.
// Unset, rows go to the test log.
func openRows() error {
	path := os.Getenv("CODECTX_BENCH_OUT")
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	rows.f = f
	return nil
}

// closeRows closes the row file; every row was written with one unbuffered
// write, so there is nothing to flush.
func closeRows() error {
	if rows.f == nil {
		return nil
	}
	return rows.f.Close()
}

// emit writes one row as one JSON line.
func emit(t *testing.T, row any) {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode a benchmark row: %v", err)
	}
	rows.mu.Lock()
	defer rows.mu.Unlock()
	if rows.f == nil {
		t.Log(string(b))
		return
	}
	if _, err := rows.f.Write(append(b, '\n')); err != nil {
		t.Fatalf("write a benchmark row: %v", err)
	}
}

// resident is this process's resident set, nil when the platform cannot
// report it.
func resident() *int64 {
	v, ok := wire.ResidentBytes()
	if !ok {
		return nil
	}
	return &v
}

// input is one file to measure.
type input struct {
	// origin is "fixture" or "tree"; path is relative to this package for a
	// fixture and to CODECTX_BENCH_TREE for a tree file.
	origin, path string
	language     lang.Language
	src          []byte
}

// inputs calls visit with every input whose language accept admits: the
// fixtures, then the CODECTX_BENCH_TREE files in walk order.
func inputs(t *testing.T, accept func(lang.Language) bool, visit func(in input)) {
	t.Helper()
	for _, dir := range []string{filepath.Join("..", "provider", "treesitter", "testdata"), "testdata"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("list fixtures in %s: %v", dir, err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			l, ok := lang.ByExtension(e.Name())
			if !ok || !accept(l) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read fixture %s: %v", p, err)
			}
			visit(input{origin: "fixture", path: filepath.ToSlash(p), language: l, src: src})
		}
	}
	dir := os.Getenv("CODECTX_BENCH_TREE")
	if dir == "" {
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open CODECTX_BENCH_TREE: %v", err)
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				return err
			}
			// A directory that cannot be listed or an entry that cannot be
			// stat'ed is a loss the run must show, not skip.
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// A symlink is not followed; a device, socket or pipe is not source.
		if !d.Type().IsRegular() {
			return nil
		}
		l, ok := lang.ByExtension(p)
		if !ok || !accept(l) {
			return nil
		}
		src, err := root.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		visit(input{origin: "tree", path: p, language: l, src: src})
		return nil
	})
	if err != nil {
		t.Fatalf("walk CODECTX_BENCH_TREE: %v", err)
	}
}

// parsers holds one parser per language for a test's duration, reused
// across files as the worker reuses its own.
type parsers map[string]*ts.Parser

func (ps parsers) get(t *testing.T, l lang.Language) *ts.Parser {
	t.Helper()
	if p, ok := ps[l.Name]; ok {
		return p
	}
	g, ok := worker.Grammar(l.Name)
	if !ok {
		t.Fatalf("language %s has no linked grammar", l.Name)
	}
	p := ts.NewParser()
	if err := p.SetLanguage(g); err != nil {
		p.Close()
		t.Fatalf("set language %s: %v", l.Name, err)
	}
	ps[l.Name] = p
	return p
}

func (ps parsers) close() {
	for _, p := range ps {
		p.Close()
	}
}

// parse parses src the way the worker does, in parseChunk slices, and
// returns the tree (nil when the parser produced none) and the bytes of the
// C strings the binding copied the slices into: each slice plus its
// terminator, the empty end-of-input slice included.
func parse(p *ts.Parser, src []byte) (*ts.Tree, int64) {
	var copies int64
	tree := p.ParseWithOptions(func(offset int, _ ts.Point) []byte {
		var chunk []byte
		if offset < len(src) {
			chunk = src[offset:min(offset+parseChunk, len(src))]
		}
		copies += int64(len(chunk)) + 1
		return chunk
	}, nil, nil)
	return tree, copies
}

// passes runs every analysis pass over g, in the order TestFunctionAnalysis
// times them; the results live in a until its next Begin.
func passes(g *flow.Graph, a *flow.Arena) {
	_ = flow.Dominators(g, a)
	pd := flow.PostDominators(g, a)
	_ = flow.ControlDependence(g, pd, a)
	_ = flow.DefUse(g, a)
}

// lowered admits the languages that have a lowering.
func lowered(l lang.Language) bool {
	_, ok := worker.LoweringFor(l.Name)
	return ok
}

// parseRow is one file's parse memory. Native figures are relative to the
// counted bytes live just before the parse (NativeBaseBytes: the parser's own
// retained state and whatever else the process holds natively).
type parseRow struct {
	Row         string `json:"row"`
	Origin      string `json:"origin"`
	Path        string `json:"path"`
	Language    string `json:"language"`
	SourceBytes int    `json:"source_bytes"`
	HasError    bool   `json:"has_error"`
	// NativePeakBytes is the counted high-water during the parse, above base.
	NativePeakBytes uint64 `json:"native_peak_bytes"`
	// NativeAfterBytes is counted live after the parse, above base, with the
	// tree still open; negative if the parse released retained state.
	NativeAfterBytes int64 `json:"native_after_bytes"`
	// TreeBytes is what closing the tree released: the tree itself.
	TreeBytes       int64  `json:"tree_bytes"`
	NativeBaseBytes uint64 `json:"native_base_bytes"`
	// InputCopyBytes is the binding's uncounted C-string copies of the
	// source, live until the parse ends.
	InputCopyBytes int64 `json:"input_copy_bytes"`
	// UntrackedFrees is how many frees during the parse were of pointers the
	// counter never saw (the input copies).
	UntrackedFrees uint64 `json:"untracked_frees"`
	RSSBeforeBytes *int64 `json:"rss_before_bytes"`
	RSSAfterBytes  *int64 `json:"rss_after_bytes"`
	// ParseNs is a second parse of the same source with counting paused,
	// which pays what the default allocator pays; CountedParseNs is the
	// measured parse, which also pays the counter's table.
	ParseNs        int64 `json:"parse_ns"`
	CountedParseNs int64 `json:"counted_parse_ns"`
}

// TestParseMemory records, for every input of all nine grammars, the native
// bytes a parse peaks at and leaves live, the resident set around it, and its
// time. It asserts nothing about the figures: they are what the controller
// derives a per-file parse reservation from, so it measures both the
// allocator's view and the resident set to learn which one a reservation
// should use.
func TestParseMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("parse memory benchmark; run without -short")
	}
	// The timing parses use their own parsers: a parser keeps and regrows
	// its buffers across parses, and one regrown while paused would leave
	// untracked blocks in the counted parser (see nativeCounter).
	ps, timing := parsers{}, parsers{}
	defer ps.close()
	defer timing.close()
	native.SetCounting(true)
	inputs(t, func(lang.Language) bool { return true }, func(in input) {
		p := ps.get(t, in.language)
		row := parseRow{Row: "parse", Origin: in.origin, Path: in.path, Language: in.language.Name, SourceBytes: len(in.src)}
		row.RSSBeforeBytes = resident()
		untracked := native.Untracked()
		base := native.ResetPeak()
		start := time.Now()
		tree, copies := parse(p, in.src)
		row.CountedParseNs = time.Since(start).Nanoseconds()
		after := native.Live()
		row.NativePeakBytes = native.Peak() - base
		row.UntrackedFrees = native.Untracked() - untracked
		row.RSSAfterBytes = resident()
		if tree == nil {
			t.Errorf("%s: the parser produced no tree", in.path)
			return
		}
		row.HasError = tree.RootNode().HasError()
		tree.Close()
		row.NativeBaseBytes = base
		row.NativeAfterBytes = int64(after) - int64(base)
		row.TreeBytes = int64(after) - int64(native.Live())
		row.InputCopyBytes = copies

		native.SetCounting(false)
		tp := timing.get(t, in.language)
		start = time.Now()
		timed, _ := parse(tp, in.src)
		row.ParseNs = time.Since(start).Nanoseconds()
		native.SetCounting(true)
		if timed == nil {
			t.Errorf("%s: the timing parse produced no tree", in.path)
			return
		}
		timed.Close()
		emit(t, row)
	})
}

// functionRow is one callable's analysis cost. The arena figures are read
// after every pass and before the next function's Begin reclaims them.
type functionRow struct {
	Row       string `json:"row"`
	Origin    string `json:"origin"`
	Path      string `json:"path"`
	Language  string `json:"language"`
	Kind      string `json:"kind"`
	StartByte uint   `json:"start_byte"`
	EndByte   uint   `json:"end_byte"`
	StartLine uint   `json:"start_line"`
	// Nodes is N (Entry and Exit included), Defs is D, Vars the variables.
	Nodes int `json:"nodes"`
	Defs  int `json:"defs"`
	Vars  int `json:"vars"`
	// LowerNs covers Begin, the lowering and Finish.
	LowerNs             int64 `json:"lower_ns"`
	DominatorsNs        int64 `json:"dominators_ns"`
	PostDominatorsNs    int64 `json:"postdominators_ns"`
	ControlDependenceNs int64 `json:"control_dependence_ns"`
	DefUseNs            int64 `json:"def_use_ns"`
	// ArenaBytes is Arena.Bytes after all passes: the high-water the ADR
	// bounds by 96·N + 64 (BoundBytes); BoundRatio is their quotient.
	ArenaBytes    int     `json:"arena_bytes"`
	BoundBytes    int     `json:"bound_bytes"`
	BoundRatio    float64 `json:"bound_ratio"`
	ScratchBytes  int     `json:"scratch_bytes"`
	RetainedBytes int     `json:"retained_bytes"`
	Unresolved    int     `json:"unresolved"`
	Augmented     int     `json:"augmented"`
	// ControlEdges and DefUseEdges are the result sizes.
	ControlEdges int `json:"control_edges"`
	DefUseEdges  int `json:"def_use_edges"`
}

// TestFunctionAnalysis records, for every callable of every Go and
// JavaScript input, N, D and the time of lowering and of each analysis pass
// separately, and the arena's bytes against the ADR's 96·N + 64 bound, which
// it measures rather than assumes. One Arena serves every function, as in a
// worker. Native counting is paused: no figure here is native, and the table
// would only add to every pass's time.
func TestFunctionAnalysis(t *testing.T) {
	if testing.Short() {
		t.Skip("per-function analysis benchmark; run without -short")
	}
	ps := parsers{}
	defer ps.close()
	native.SetCounting(false)
	defer native.SetCounting(true)
	var arena flow.Arena
	inputs(t, lowered, func(in input) {
		l, _ := worker.LoweringFor(in.language.Name)
		tree, _ := parse(ps.get(t, in.language), in.src)
		if tree == nil {
			t.Errorf("%s: the parser produced no tree", in.path)
			return
		}
		defer tree.Close()
		err := l.Functions(tree.RootNode(), func(fn *ts.Node) error {
			row := functionRow{Row: "function", Origin: in.origin, Path: in.path, Language: in.language.Name,
				Kind: fn.Kind(), StartByte: fn.StartByte(), EndByte: fn.EndByte(), StartLine: fn.StartPosition().Row + 1}
			t0 := time.Now()
			g := l.Lower(fn, in.src, &arena)
			t1 := time.Now()
			_ = flow.Dominators(g, &arena)
			t2 := time.Now()
			pd := flow.PostDominators(g, &arena)
			t3 := time.Now()
			cd := flow.ControlDependence(g, pd, &arena)
			t4 := time.Now()
			du := flow.DefUse(g, &arena)
			t5 := time.Now()
			row.LowerNs = t1.Sub(t0).Nanoseconds()
			row.DominatorsNs = t2.Sub(t1).Nanoseconds()
			row.PostDominatorsNs = t3.Sub(t2).Nanoseconds()
			row.ControlDependenceNs = t4.Sub(t3).Nanoseconds()
			row.DefUseNs = t5.Sub(t4).Nanoseconds()
			row.Nodes, row.Defs, row.Vars = g.Len(), g.Defs(), g.Vars()
			row.ArenaBytes = arena.Bytes()
			row.BoundBytes = 96*row.Nodes + 64
			row.BoundRatio = float64(row.ArenaBytes) / float64(row.BoundBytes)
			row.ScratchBytes = arena.ScratchBytes()
			row.RetainedBytes = arena.Retained()
			row.Unresolved = g.Unresolved()
			row.Augmented = pd.Augmented()
			row.ControlEdges, row.DefUseEdges = cd.Len(), du.Len()
			emit(t, row)
			return nil
		})
		if err != nil {
			t.Errorf("%s: %v", in.path, err)
		}
	})
}

// fileRow is one file end to end: parse, lower and analyse every callable,
// close the tree.
type fileRow struct {
	Row         string `json:"row"`
	Origin      string `json:"origin"`
	Path        string `json:"path"`
	Language    string `json:"language"`
	SourceBytes int    `json:"source_bytes"`
	Functions   int    `json:"functions"`
	// Nodes is N summed over the file's functions.
	Nodes int `json:"nodes"`
	// WallNs is a second run with native counting paused; CountedWallNs is
	// the measured run, which also pays the counter's table.
	WallNs        int64 `json:"wall_ns"`
	CountedWallNs int64 `json:"counted_wall_ns"`
	// NativePeakBytes is the counted native high-water over the whole file,
	// above the bytes live before it.
	NativePeakBytes uint64 `json:"native_peak_bytes"`
	// ArenaPeakBytes is the largest Bytes + ScratchBytes of any one function:
	// the Go-side analysis high-water, which the native figure does not see.
	ArenaPeakBytes     int    `json:"arena_peak_bytes"`
	ArenaRetainedBytes int    `json:"arena_retained_bytes"`
	RSSBeforeBytes     *int64 `json:"rss_before_bytes"`
	RSSAfterBytes      *int64 `json:"rss_after_bytes"`
}

// TestFileAnalysis records, for every Go and JavaScript input, the wall time
// of parsing it, lowering and analysing every callable in it and closing the
// tree, with the native high-water over all of it. Only languages with a
// lowering have an end to end; the other grammars' parse cost is
// TestParseMemory's.
func TestFileAnalysis(t *testing.T) {
	if testing.Short() {
		t.Skip("per-file analysis benchmark; run without -short")
	}
	// Separate timing parsers, as in TestParseMemory.
	ps, timing := parsers{}, parsers{}
	defer ps.close()
	defer timing.close()
	defer native.SetCounting(true)
	var arena flow.Arena
	// run is one end to end over in with p; it reports false when there was
	// no tree.
	run := func(in input, p *ts.Parser, row *fileRow) bool {
		l, _ := worker.LoweringFor(in.language.Name)
		tree, _ := parse(p, in.src)
		if tree == nil {
			return false
		}
		defer tree.Close()
		row.Functions, row.Nodes, row.ArenaPeakBytes = 0, 0, 0
		err := l.Functions(tree.RootNode(), func(fn *ts.Node) error {
			g := l.Lower(fn, in.src, &arena)
			passes(g, &arena)
			row.Functions++
			row.Nodes += g.Len()
			row.ArenaPeakBytes = max(row.ArenaPeakBytes, arena.Bytes()+arena.ScratchBytes())
			return nil
		})
		if err != nil {
			t.Errorf("%s: %v", in.path, err)
		}
		return true
	}
	inputs(t, lowered, func(in input) {
		row := fileRow{Row: "file", Origin: in.origin, Path: in.path, Language: in.language.Name, SourceBytes: len(in.src)}
		// A language's parser is created counted, so all its blocks are in
		// the table, but outside the measured window.
		native.SetCounting(true)
		p := ps.get(t, in.language)
		row.RSSBeforeBytes = resident()
		base := native.ResetPeak()
		start := time.Now()
		ok := run(in, p, &row)
		row.CountedWallNs = time.Since(start).Nanoseconds()
		row.NativePeakBytes = native.Peak() - base
		row.RSSAfterBytes = resident()
		row.ArenaRetainedBytes = arena.Retained()
		if !ok {
			t.Errorf("%s: the parser produced no tree", in.path)
			return
		}
		native.SetCounting(false)
		tp := timing.get(t, in.language)
		start = time.Now()
		ok = run(in, tp, &row)
		row.WallNs = time.Since(start).Nanoseconds()
		if !ok {
			t.Errorf("%s: the timing run produced no tree", in.path)
			return
		}
		emit(t, row)
	})
}
