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
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
	"github.com/Sawmonabo/codectx/internal/residency"
)

// This file measures the per-function dependence core in this process: the
// parse memory of all nine grammars, the per-function cost of every analysis
// pass for the languages that have a lowering, and the per-file cost of
// parsing, lowering and analysing a whole file. Each measurement is one JSON
// object per row, written to the file CODECTX_BENCH_OUT names (truncated once
// per run by TestMain). The three tests skip unless it is set: their rows
// are the product, and a run that kept them nowhere would only spend the
// time. The `row` field names the row's kind:
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
// input copies, which are reported beside them. A figure that was not or
// cannot be measured is JSON null, never 0: a resident set the platform
// cannot report, the scanner allocations of the grammars whose scanner
// bypasses the counter (scanner_bytes), a timing run that produced no tree.

// rows is the benchmark row sink; see openRows.
var rows struct {
	mu sync.Mutex
	f  *os.File
}

// openRows truncates and opens the file CODECTX_BENCH_OUT names, once per
// run, so every test's rows accumulate in one file for aggregation. Unset,
// there is no row file and the tests that write rows skip (requireRows).
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

// requireRows skips a row-writing test under -short or when no row file is
// open.
func requireRows(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("dependence-core measurement; run without -short")
	}
	if rows.f == nil {
		t.Skip("dependence-core measurement; set CODECTX_BENCH_OUT to the file its rows are written to")
	}
}

// emit writes one row as one JSON line to the row file.
func emit(t *testing.T, row any) {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode a benchmark row: %v", err)
	}
	rows.mu.Lock()
	defer rows.mu.Unlock()
	if rows.f == nil {
		t.Fatal("a row was emitted with no row file open; the test must call requireRows first")
	}
	if _, err := rows.f.Write(append(b, '\n')); err != nil {
		t.Fatalf("write a benchmark row: %v", err)
	}
}

// resident is this process's resident set, nil when the platform cannot
// report it.
func resident() *int64 {
	r := residency.Read().Resident
	if r == nil {
		return nil
	}
	v := int64(*r)
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
	fixtures(t, accept, visit)
	treeFiles(t, accept, visit)
}

// fixtures calls visit with every fixture whose language accept admits: the
// provider's grammar fixtures, then this package's.
func fixtures(t *testing.T, accept func(lang.Language) bool, visit func(in input)) {
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
}

// treeFiles calls visit with every CODECTX_BENCH_TREE file whose language accept
// admits, in walk order; nothing when it is unset.
func treeFiles(t *testing.T, accept func(lang.Language) bool, visit func(in input)) {
	t.Helper()
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
	p, err := worker.NewParser(l.Name)
	if err != nil {
		t.Fatal(err)
	}
	ps[l.Name] = p
	return p
}

func (ps parsers) close() {
	for _, p := range ps {
		p.Close()
	}
}

// parse parses src through worker.Parse and returns the tree (nil when the
// parser produced none) and the bytes of the C strings the binding copied
// the slices into: each slice plus its terminator, the empty end-of-input
// slice included.
func parse(p *ts.Parser, src []byte) (*ts.Tree, int64) {
	var copies int64
	tree := worker.Parse(p, src, func(chunk []byte) { copies += int64(len(chunk)) + 1 })
	return tree, copies
}

// passes runs the analysis passes the dependence core runs over g, in the
// order TestFunctionAnalysis times them; the results live in a until its next
// Begin. The forward dominator tree is not one of them: nothing downstream
// consumes it, so TestFunctionAnalysis measures it in its own window.
func passes(g *flow.Graph, a *flow.Arena) {
	pd := flow.PostDominators(g, a)
	_ = flow.ControlDependence(g, pd, a)
	_ = flow.DefUse(g, a)
}

// lowered admits the languages that have a lowering.
func lowered(l lang.Language) bool {
	_, ok := worker.LoweringFor(l.Name)
	return ok
}

// warmLowerings lowers and analyses every callable of the first fixture of
// each lowered language into a and s, untimed and on a parser of its own, so
// that what a lowering resolves once per process (its kind and field tables),
// the arena's first backing and the scratch's first lists are paid before any
// timed window opens.
func warmLowerings(t *testing.T, a *flow.Arena, s *worker.Scratch) {
	t.Helper()
	ps := parsers{}
	defer ps.close()
	seen := map[string]bool{}
	fixtures(t, lowered, func(in input) {
		if seen[in.language.Name] {
			return
		}
		seen[in.language.Name] = true
		l, _ := worker.LoweringFor(in.language.Name)
		tree, _ := parse(ps.get(t, in.language), in.src)
		if tree == nil {
			t.Fatalf("%s: the parser produced no tree", in.path)
		}
		defer tree.Close()
		if err := l.Functions(tree.RootNode(), func(fn *ts.Node) error {
			passes(l.Lower(fn, in.src, a, s), a)
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", in.path, err)
		}
	})
}

// scannerAllocations says, for every grammar lang.All pins, whether its
// external scanner allocates past the counting allocator (see nativeCounter,
// "What routes through the hook"). It names every grammar so that one added
// later fails scannerBytes instead of being reported as allocating nothing.
var scannerAllocations = map[string]bool{
	"c": false, "cpp": true, "go": false, "java": false, "javascript": false,
	"python": true, "rust": true, "tsx": false, "typescript": false,
}

// scannerBytes is a parse row's scanner_bytes: 0 for a grammar whose scanner
// allocates nothing (or that has none), nil for one whose scanner allocates
// through libc, which no counter here sees.
func scannerBytes(t *testing.T, language string) *uint64 {
	t.Helper()
	bypasses, ok := scannerAllocations[language]
	if !ok {
		t.Fatalf("scannerAllocations does not say whether the %s scanner allocates", language)
	}
	if bypasses {
		return nil
	}
	var zero uint64
	return &zero
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
	// It excludes the grammar scanner's allocations; ScannerBytes says
	// whether those are known to be none (0) or unmeasured (null).
	NativePeakBytes uint64  `json:"native_peak_bytes"`
	ScannerBytes    *uint64 `json:"scanner_bytes"`
	// NativeAfterBytes is counted live after the parse, above base, with the
	// tree still open; negative if the parse released retained state.
	NativeAfterBytes int64 `json:"native_after_bytes"`
	// TreeBytes is what closing the tree released: the tree itself.
	TreeBytes int64 `json:"tree_bytes"`
	// FlatBytes is the flattened node array the lowering reads in place of
	// the tree (worker.Flatten), in the Go heap. It is taken from the open
	// tree with native counting paused, since the tree cursor the flattening
	// walks with allocates natively and frees before it returns.
	FlatBytes       int64  `json:"flat_bytes"`
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
	// which skips the table for new allocations but still takes its lock on
	// every free and realloc (see nativeCounter, Pausing); null when that
	// parse produced no tree. CountedParseNs is the measured parse, which
	// pays the whole table.
	ParseNs        *int64 `json:"parse_ns"`
	CountedParseNs int64  `json:"counted_parse_ns"`
}

// TestParseMemory records, for every input of all nine grammars, the native
// bytes a parse peaks at and leaves live, the resident set around it, and its
// time. It asserts nothing about the figures: they are the evidence a
// per-file parse reservation is derived from, so it measures both the
// allocator's view and the resident set to learn which one a reservation
// should use.
func TestParseMemory(t *testing.T) {
	requireRows(t)
	// The timing parses use their own parsers: a parser keeps and regrows
	// its buffers across parses, and one regrown while paused would leave
	// untracked blocks in the counted parser (see nativeCounter).
	ps, timing := parsers{}, parsers{}
	defer ps.close()
	defer timing.close()
	native.SetCounting(true)
	inputs(t, func(lang.Language) bool { return true }, func(in input) {
		p := ps.get(t, in.language)
		row := parseRow{Row: "parse", Origin: in.origin, Path: in.path, Language: in.language.Name, SourceBytes: len(in.src),
			ScannerBytes: scannerBytes(t, in.language.Name)}
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
		native.SetCounting(false)
		flat, err := worker.Flatten(tree)
		native.SetCounting(true)
		if err != nil {
			tree.Close()
			t.Errorf("%s: %v", in.path, err)
			return
		}
		row.FlatBytes = int64(flat.Bytes())
		tree.Close()
		row.NativeBaseBytes = base
		row.NativeAfterBytes = int64(after) - int64(base)
		row.TreeBytes = int64(after) - int64(native.Live())
		row.InputCopyBytes = copies

		native.SetCounting(false)
		tp := timing.get(t, in.language)
		start = time.Now()
		timed, _ := parse(tp, in.src)
		ns := time.Since(start).Nanoseconds()
		native.SetCounting(true)
		if timed == nil {
			// The counted figures stand; only the timing is missing.
			t.Errorf("%s: the timing parse produced no tree", in.path)
		} else {
			timed.Close()
			row.ParseNs = &ns
		}
		emit(t, row)
	})
}

// functionRow is one callable's analysis cost. The arena figures are read
// after the three passes the dependence core runs (post-dominators, control
// dependence, def-use) and before the forward dominator tree, which is
// measured in its own window, and before the next function's Begin reclaims
// them.
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
	PostDominatorsNs    int64 `json:"postdominators_ns"`
	ControlDependenceNs int64 `json:"control_dependence_ns"`
	DefUseNs            int64 `json:"def_use_ns"`
	// DominatorsNs and DominatorsBytes are the forward dominator tree, which
	// the dependence core does not run: its time, and the arena bytes it adds
	// on top of ArenaBytes.
	DominatorsNs    int64 `json:"dominators_ns"`
	DominatorsBytes int   `json:"dominators_bytes"`
	// ArenaBytes is Arena.Bytes after the three dependence-core passes.
	// BoundBytes is 96·N + 64, the design's per-function figure the
	// benchmark compares against, not a bound; BoundRatio is their quotient.
	// ScratchBytes is the builder's scratch high-water since Begin.
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
// separately, and the arena's bytes against the design's 96·N + 64 figure.
// One Arena serves every function, as in a worker. Each function is lowered
// and analysed once untimed before the timed run, so the timed run finds the
// arena's backing already sized to it, as a worker's steady state does; a
// function whose previous run used more than the arena's release threshold
// has its backing dropped at Begin, and its timed run pays the fresh
// allocation every such function pays in a worker. Native counting is
// paused: no figure here is native, and the table would only add to every
// pass's time.
func TestFunctionAnalysis(t *testing.T) {
	requireRows(t)
	ps := parsers{}
	defer ps.close()
	native.SetCounting(false)
	defer native.SetCounting(true)
	var arena flow.Arena
	var scratch worker.Scratch
	defer scratch.Close()
	warmLowerings(t, &arena, &scratch)
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
			passes(l.Lower(fn, in.src, &arena, &scratch), &arena)
			t0 := time.Now()
			g := l.Lower(fn, in.src, &arena, &scratch)
			t1 := time.Now()
			pd := flow.PostDominators(g, &arena)
			t2 := time.Now()
			cd := flow.ControlDependence(g, pd, &arena)
			t3 := time.Now()
			du := flow.DefUse(g, &arena)
			t4 := time.Now()
			row.LowerNs = t1.Sub(t0).Nanoseconds()
			row.PostDominatorsNs = t2.Sub(t1).Nanoseconds()
			row.ControlDependenceNs = t3.Sub(t2).Nanoseconds()
			row.DefUseNs = t4.Sub(t3).Nanoseconds()
			row.Nodes, row.Defs, row.Vars = g.Len(), g.DefCount(), g.Vars()
			row.ArenaBytes = arena.Bytes()
			row.BoundBytes = 96*row.Nodes + 64
			row.BoundRatio = float64(row.ArenaBytes) / float64(row.BoundBytes)
			row.ScratchBytes = arena.ScratchBytes()
			row.RetainedBytes = arena.Retained()
			row.Unresolved = g.Unresolved()
			row.Augmented = pd.Augmented()
			row.ControlEdges, row.DefUseEdges = cd.Len(), du.Len()
			// The dominator tree's own window, after every figure above is
			// read: its arrays stay in the arena until the next Begin.
			t5 := time.Now()
			_ = flow.Dominators(g, &arena)
			row.DominatorsNs = time.Since(t5).Nanoseconds()
			row.DominatorsBytes = arena.Bytes() - row.ArenaBytes
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
	// WallNs is a second run with native counting paused (new allocations
	// skip the table; frees and reallocs still take its lock), on the arena
	// the first run left sized to this file; null when that run produced no
	// tree. CountedWallNs is the measured run, which pays the whole table
	// and finds the arena as the previous file left it, as a worker does.
	WallNs        *int64 `json:"wall_ns"`
	CountedWallNs int64  `json:"counted_wall_ns"`
	// NativePeakBytes is the counted native high-water over the whole file,
	// above the bytes live before it; ScannerBytes is as in parseRow.
	NativePeakBytes uint64  `json:"native_peak_bytes"`
	ScannerBytes    *uint64 `json:"scanner_bytes"`
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
	requireRows(t)
	// Separate timing parsers, as in TestParseMemory.
	ps, timing := parsers{}, parsers{}
	defer ps.close()
	defer timing.close()
	defer native.SetCounting(true)
	var arena flow.Arena
	var scratch worker.Scratch
	defer scratch.Close()
	warmLowerings(t, &arena, &scratch)
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
			g := l.Lower(fn, in.src, &arena, &scratch)
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
		row := fileRow{Row: "file", Origin: in.origin, Path: in.path, Language: in.language.Name, SourceBytes: len(in.src),
			ScannerBytes: scannerBytes(t, in.language.Name)}
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
		ns := time.Since(start).Nanoseconds()
		if !ok {
			// The counted figures stand; only the timing is missing.
			t.Errorf("%s: the timing run produced no tree", in.path)
		} else {
			row.WallNs = &ns
		}
		emit(t, row)
	})
}
