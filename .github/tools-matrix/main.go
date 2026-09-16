// Command tools-matrix is the per-platform smoke of the managed analyzer
// toolchain (Section 11.7 "Verification", Task 22 Step 3). It runs after
// `codectx tools prefetch --all` on each operating system of
// .github/workflows/tools-matrix.yml and answers one question the unit tests
// cannot: does the payload the lock pinned for *this* platform actually run,
// and does it produce real output for the language it claims?
//
// It resolves every tool through the product's own toolchain.Resolver with
// tools.offline set, so a fetch here would mean the prefetch the workflow just
// ran did not install what it reported. It then indexes a nine-language
// fixture -- Go, TypeScript, TSX, JavaScript, Python, Java, C, C++ and Rust,
// every one of them carrying non-ASCII identifiers and string literals -- and
// requires each indexer to name the documents it was given. One of the
// projects carries no compiler configuration at all, because a matrix whose
// every project is configured proves the argv only for the configured half of
// a repository.
//
// Two deliberate limits, so nobody reads more into a green run than it proves:
//
//   - The index check is a byte-level presence check on the document paths, not
//     a protobuf parse. The wire format is the SCIP provider's own test surface.
//   - The language servers are exercised only as far as an identity
//     invocation reaches: gopls, clangd, ty and typescript-language-server
//     print a version, while jdtls speaks nothing but LSP over stdio and is
//     proved here only to the extent that its payload installs and verifies.
//     A real initialize/shutdown handshake belongs to the LSP provider.
//
// The argv of each indexer is the product's own: every run below is built by
// scip.Argv, the single source of every indexer invocation this product
// issues. A matrix that ran a different argument array would prove that some
// argv works on the platform rather than that the one the product issues does,
// which is the only question worth an hour of runner time. The fixture and the
// document assertions stay this program's own. When the CLI grows
// `codectx index`, the polyglot fixture below is what that end-to-end step
// should be pointed at.
//
// One trap worth naming: this program lives under a dot directory, so the go
// command excludes it from ./... -- `go build`, `go vet` and `go mod tidy` all
// walk past it. It therefore imports nothing but the standard library and this
// module's own packages; an external import added here would compile locally
// and then fail in CI with a missing go.sum entry, because tidy never saw it.
//
// Usage:
//
//	go run ./.github/tools-matrix --store <dir> [--work <dir>] [--keep]
//
// --store is the tool store itself -- the same directory the workflow put in
// tools.cache_dir, which is what `codectx tools` resolved its store from. It is
// the store and not its parent: toolchain.Options.StoreDir takes it verbatim.
package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

//go:embed fixture
var fixtureFS embed.FS

// runTimeout bounds one analyzer run. The slowest leg observed on Linux is the
// engine parse of the Java fixture at ~5 s; the bound is two orders of
// magnitude above that so a slow runner is never the failure, while a hung
// child still fails its own run rather than the whole job's 60-minute budget.
const runTimeout = 10 * time.Minute

// check is one analyzer run against one part of the fixture.
type check struct {
	// kind is the profile whose argv this run issues; its value is also the
	// lock entry name, which is what lets one constant carry the identity of
	// the tool and of the invocation at once.
	kind scip.Kind
	// languages are the languages this run covers, in the Section 11.7 spelling.
	languages []string
	// dir is the fixture subdirectory the tool runs in, which is the profile's
	// input directory.
	dir string
	// documents are the paths the produced index must name. For the engine,
	// which writes a binary graph rather than an index, this is empty and the
	// output file's existence and size are the whole check.
	documents []string
	// needs names a host program the tool shells out to. A runner that does not
	// provide it fails this run: the profile genuinely needs that language's own
	// toolchain to load the project model, so its absence is the matrix not
	// being able to prove the platform, not an honest absence like a lock entry
	// with no payload here. The detail says which program, so the failure points
	// at the runner image rather than at the payload.
	needs string
}

// indexChecks is the nine-language matrix. Every advertised language of Section
// 11.7 appears at least once, C++ and TSX explicitly, because the research
// rounds exercised only C and TypeScript.
var indexChecks = []check{
	{
		kind: scip.KindGo, languages: []string{"go"}, dir: "go",
		documents: []string{"sample.go"}, needs: "go",
	},
	{
		kind: scip.KindTypeScript, languages: []string{"typescript", "tsx", "javascript"}, dir: "ts",
		documents: []string{"src/sample.ts", "src/sample.tsx", "src/sample.js"},
	},
	{
		// A project whose only manifest is package.json: no tsconfig.json, no
		// jsconfig.json, nothing that says how to compile it. Two of the three
		// triggers of the TypeScript profile name exactly this project, and the
		// leg above cannot prove it because its fixture carries a tsconfig.json
		// -- which is why an argv that failed every configuration-less project
		// passed this matrix and shipped. The languages it covers are already
		// covered by that leg; what this one proves is the profile's own
		// argument array over a project with no compiler configuration.
		kind: scip.KindTypeScript, languages: []string{"javascript"}, dir: "js",
		documents: []string{"src/index.js"},
	},
	{
		kind: scip.KindPython, languages: []string{"python"}, dir: "py",
		documents: []string{"sample.py"}, needs: "pip3",
	},
	{
		// The Java profile hands scip-java a --scip-config build description
		// and lets it compile with the managed JDK's own javac, so this leg
		// needs neither Maven nor Gradle on the runner. materialize writes the
		// same shape of description the profile generates.
		kind: scip.KindJava, languages: []string{"java"}, dir: "java",
		documents: []string{"src/main/java/com/example/Sample.java"},
	},
	{
		kind: scip.KindClang, languages: []string{"c", "cpp"}, dir: "c",
		documents: []string{"sample.c", "sample.cpp"},
	},
	{
		kind: scip.KindRust, languages: []string{"rust"}, dir: "rust",
		documents: []string{"src/lib.rs"}, needs: "cargo",
	},
}

// engineFrontends are the engine's own spellings for the parser of each
// language family, matching internal/provider/dependence/joern. The engine
// covers all nine languages through six frontends.
var engineFrontends = []struct {
	frontend  string
	dir       string
	languages []string
}{
	{"golang", "go", []string{"go"}},
	{"jssrc", "ts", []string{"typescript", "tsx", "javascript"}},
	{"pythonsrc", "py", []string{"python"}},
	{"javasrc", "java", []string{"java"}},
	{"c", "c", []string{"c", "cpp"}},
	{"rust", "rust", []string{"rust"}},
}

// serverIdentity is the subset of managed language servers that answer an
// identity invocation. jdtls speaks only LSP and is deliberately absent; see
// the package comment.
var serverIdentity = map[string][]string{
	"gopls":                      {"version"},
	"ty":                         {"--version"},
	"clangd":                     {"--version"},
	"typescript-language-server": {"--version"},
}

type result struct {
	name      string
	languages []string
	ok        bool
	// skipped is a lock entry with no payload for this platform. Section 11.7
	// makes that honest absence, so it is neither a pass nor a failure -- but it
	// contributes no language coverage, so another run has to cover the language
	// or the job still fails. scip-clang on Windows is the case that exists
	// today: the lock ships it for Linux and macOS only, and clangd and the
	// graph engine carry C and C++ there.
	skipped bool
	detail  string
	elapsed time.Duration
}

// unsupportedHere reports whether a resolution failed because the lock carries
// no payload for this platform, rather than because something is broken.
func unsupportedHere(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeToolUnsupportedPlatform
}

func main() {
	store := flag.String("store", "", "the tool store the workflow prefetched into")
	work := flag.String("work", "", "directory to materialize the fixture in (default: a temporary directory)")
	keep := flag.Bool("keep", false, "keep the materialized fixture and its indexes")
	flag.Parse()

	if err := run(*store, *work, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "tools-matrix:", err)
		os.Exit(1)
	}
}

func run(store, work string, keep bool) error {
	if store == "" {
		return fmt.Errorf("--store is required")
	}
	abs, err := filepath.Abs(store)
	if err != nil {
		return err
	}
	res, err := toolchain.New(toolchain.Options{
		StoreDir: abs,
		// Everything must already be installed: the workflow ran
		// `codectx tools prefetch --all` immediately before this program.
		Offline:       true,
		MaxFetchBytes: 1 << 31,
		FetchTimeout:  runTimeout,
	})
	if err != nil {
		return err
	}

	root := work
	if root == "" {
		if root, err = os.MkdirTemp("", "codectx-polyglot-"); err != nil {
			return err
		}
	}
	if root, err = filepath.Abs(root); err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(root)
	}
	if err := materialize(root); err != nil {
		return err
	}
	fmt.Printf("platform  %s\nstore     %s\nfixture   %s\n\n",
		toolchain.Current().Key(), res.StoreDir(), root)

	ctx := context.Background()
	var results []result
	for _, c := range indexChecks {
		results = append(results, runIndexCheck(ctx, res, root, c))
	}
	results = append(results, runEngineChecks(ctx, res, root)...)
	results = append(results, runServerChecks(ctx, res)...)

	return summarize(results)
}

// runIndexCheck resolves one indexer and runs it over its part of the fixture.
// The result is a named return so the deferred timing lands in the value this
// function actually returns.
func runIndexCheck(ctx context.Context, res *toolchain.Resolver, root string, c check) (r result) {
	r = result{name: string(c.kind), languages: c.languages}
	started := time.Now()
	defer func() { r.elapsed = time.Since(started) }()

	if c.needs != "" {
		if _, err := exec.LookPath(c.needs); err != nil {
			// Not r.skipped: an uncovered language fails summarize anyway, so
			// recording this as a skip would reach the same verdict by a longer
			// road while calling a broken runner "unsupported here".
			r.detail = c.needs + " is not on PATH; this runner cannot prove this leg"
			return r
		}
	}
	// scip.Kind is also the lock entry name, so one constant names the profile
	// and the payload.
	tool, err := res.Resolve(ctx, string(c.kind))
	if err != nil {
		r.skipped = unsupportedHere(err)
		r.detail = "resolve: " + err.Error()
		return r
	}
	// The three paths the profile varies per run. The index is written outside
	// the input directory and the scratch root is private to this run, exactly
	// as the provider arranges them, so a tool cannot read its own output back
	// as an input.
	dir := filepath.Join(root, c.dir)
	output := filepath.Join(root, "out", string(c.kind)+".scip")
	workDir := filepath.Join(root, "work", string(c.kind))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		r.detail = "work directory: " + err.Error()
		return r
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		r.detail = "output directory: " + err.Error()
		return r
	}
	_ = os.Remove(output)

	if out, err := runTool(ctx, tool, scip.Argv(c.kind, dir, output, workDir), dir); err != nil {
		r.detail = err.Error() + tail(out)
		return r
	}
	index, err := os.ReadFile(output)
	if err != nil {
		r.detail = "the run produced no index: " + err.Error()
		return r
	}
	if len(index) == 0 {
		r.detail = "the run produced an empty index"
		return r
	}
	var missing []string
	for _, doc := range c.documents {
		// The indexers write slash-separated document paths regardless of the
		// host separator, so the expectation is compared as written.
		if !bytes.Contains(index, []byte(doc)) {
			missing = append(missing, doc)
		}
	}
	if len(missing) > 0 {
		r.detail = fmt.Sprintf("%d bytes of index, but it names none of: %s",
			len(index), strings.Join(missing, ", "))
		return r
	}
	r.ok = true
	r.detail = fmt.Sprintf("all %d documents named, %d bytes", len(c.documents), len(index))
	return r
}

// runEngineChecks parses each language family of the fixture with the managed
// graph engine, which is the dependence provider's backend.
func runEngineChecks(ctx context.Context, res *toolchain.Resolver, root string) []result {
	var out []result
	tool, err := res.Resolve(ctx, "joern")
	if err != nil {
		return []result{{name: "joern", skipped: unsupportedHere(err), detail: "resolve: " + err.Error()}}
	}
	for _, fe := range engineFrontends {
		r := result{name: "joern/" + fe.frontend, languages: fe.languages}
		started := time.Now()
		dir := filepath.Join(root, fe.dir)
		graph := filepath.Join(root, fe.frontend+".cpg.bin")
		// --max-num-def is the pinned per-method definition cap of the product's
		// engine profile; the smoke uses the same one so it exercises the same
		// code path in the frontend.
		args := []string{"--language", fe.frontend, "--max-num-def", "40000", "--output", graph, dir}
		if runOut, err := runTool(ctx, tool, args, root); err != nil {
			r.detail = err.Error() + tail(runOut)
		} else if info, err := os.Stat(graph); err != nil || info.Size() == 0 {
			r.detail = "the parse produced no graph"
		} else {
			r.ok = true
			r.detail = fmt.Sprintf("%d bytes of graph", info.Size())
		}
		r.elapsed = time.Since(started)
		out = append(out, r)
	}
	return out
}

// runServerChecks starts each language server that answers an identity
// invocation, which proves the payload executes on this platform.
func runServerChecks(ctx context.Context, res *toolchain.Resolver) []result {
	names := make([]string, 0, len(serverIdentity))
	for name := range serverIdentity {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]result, 0, len(names))
	for _, name := range names {
		r := result{name: name}
		started := time.Now()
		tool, err := res.Resolve(ctx, name)
		if err != nil {
			r.skipped = unsupportedHere(err)
			r.detail = "resolve: " + err.Error()
		} else if identity, err := runTool(ctx, tool, serverIdentity[name], ""); err != nil {
			r.detail = err.Error() + tail(identity)
		} else {
			r.ok = true
			r.detail = firstLine(identity)
		}
		r.elapsed = time.Since(started)
		out = append(out, r)
	}
	return out
}

// runTool starts a resolved tool with its own argv prefix and environment. The
// child also receives PATH and HOME because three of the indexers shell out to
// a host toolchain (go, cargo, pip3) that resolves both, and the platform
// variables Windows processes cannot start without. Nothing else of this
// process's environment is inherited.
func runTool(ctx context.Context, tool toolchain.Tool, args []string, dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	argv := append(append([]string{}, tool.ArgvPrefix...), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// The passthrough goes first and the payload's own variables last, exactly
	// as the product's profiles compose it, so a host JAVA_HOME can never shadow
	// the managed JDK the lock pinned.
	cmd.Env = append(passthroughEnv(), tool.Env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w", filepath.Base(argv[0]), err)
	}
	return out, nil
}

// passthroughEnv is the allowlist a child analyzer receives from this process.
func passthroughEnv() []string {
	keys := []string{"PATH", "HOME"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "Path", "SystemRoot", "windir", "TEMP", "TMP",
			"USERPROFILE", "LOCALAPPDATA", "APPDATA", "PATHEXT", "COMSPEC")
	}
	var env []string
	for _, key := range keys {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// materialize writes the embedded fixture under root and generates the two
// manifests that must name absolute paths.
func materialize(root string) error {
	err := fs.WalkDir(fixtureFS, "fixture", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "fixture")
		rel = strings.TrimPrefix(rel, "/")
		// The Go fixture's module file is stored as go.mod.txt: a real go.mod
		// anywhere under this repository would make its directory a separate
		// module, and the go command then excludes the subtree from this
		// module's files, so //go:embed would never see it. Only that one file
		// is renamed -- a blanket .txt strip would silently mangle the next
		// fixture file that legitimately ends in .txt, such as the
		// compile_flags.txt clangd and scip-clang look for.
		if rel == "go/go.mod.txt" {
			rel = "go/go.mod"
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fixtureFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return err
	}

	slash := func(elem ...string) string { return filepath.ToSlash(filepath.Join(elem...)) }
	cDir := slash(root, "c")
	compdb := fmt.Sprintf(`[
  {"directory": %q, "file": "sample.c", "arguments": ["clang", "-c", "sample.c", "-o", "sample.c.o"]},
  {"directory": %q, "file": "sample.cpp", "arguments": ["clang++", "-std=c++17", "-c", "sample.cpp", "-o", "sample.cpp.o"]}
]
`, cDir, cDir)
	if err := os.WriteFile(filepath.Join(root, "c", "compile_commands.json"), []byte(compdb), 0o644); err != nil {
		return err
	}

	// The same shape the Java profile's writeScipJavaConfig emits: the input
	// directory is both the source root and the only source directory, and
	// nothing is resolved from a remote index. The scratch root is not a field
	// here -- the profile passes it as --targetroot, which scip.Argv builds.
	// The file lands in the directory this leg passes as inputDir, which is
	// where --scip-config looks for it.
	javaDir := slash(root, "java")
	scipJava := fmt.Sprintf(`{
  "projects": [
    {
      "sourceroot": %q,
      "sourceDirectories": [%q],
      "dependencies": [],
      "classpath": [],
      "javacOptions": []
    }
  ]
}
`, javaDir, javaDir)
	return os.WriteFile(filepath.Join(root, "java", "scip-java.json"), []byte(scipJava), 0o644)
}

// summarize prints the per-run table and the per-language coverage, and fails
// the program when any run failed or any advertised language went uncovered.
func summarize(results []result) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tLANGUAGES\tRESULT\tELAPSED\tDETAIL")
	failed, skipped := 0, 0
	covered := map[string]bool{}
	for _, r := range results {
		verdict := "ok"
		switch {
		case r.ok:
			for _, lang := range r.languages {
				covered[lang] = true
			}
		case r.skipped:
			verdict = "skipped"
			skipped++
		default:
			verdict = "FAILED"
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.name, strings.Join(r.languages, ", "),
			verdict, r.elapsed.Round(time.Millisecond), r.detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// The nine languages of Section 11.7. A green run that silently stopped
	// covering one of them is the failure this list exists to catch.
	all := []string{"go", "typescript", "tsx", "javascript", "python", "java", "c", "cpp", "rust"}
	var uncovered []string
	for _, lang := range all {
		if !covered[lang] {
			uncovered = append(uncovered, lang)
		}
	}
	fmt.Printf("\n%d of %d runs ok, %d skipped as unsupported here; %d of %d languages covered\n",
		len(results)-failed-skipped, len(results), skipped, len(all)-len(uncovered), len(all))
	switch {
	case failed > 0:
		return fmt.Errorf("%d of %d runs failed", failed, len(results))
	case len(uncovered) > 0:
		return fmt.Errorf("no run covered %s", strings.Join(uncovered, ", "))
	}
	return nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

// tail bounds what a failing child's output contributes to the report, so one
// broken run cannot bury the table it belongs to.
func tail(b []byte) string {
	const max = 800
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return "; " + strings.ReplaceAll(s, "\n", " | ")
}
