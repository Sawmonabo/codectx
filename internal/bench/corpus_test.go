package bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
)

// This file is the Section 23.5 fingerprint-parity harness and the pinned
// corpora manifest. It has no TestMain of its own: the one in
// plateau_test.go makes this test binary the parser worker, which is what lets
// the workspace composed below spawn parsers through os.Executable. The two
// files are therefore a set, not independent units.
//
// Seam note: the corpus generator seam and the frozen Section 23.2 budget
// table are defined here because the parity rows are their first consumer;
// budgets_test.go widens corpusSpec with the small-real, >200-file and
// reference scales and reads the budget constants.

// --- Section 23.2 budget table (frozen; read by budgets_test.go) ------------
//
// Absolute release-gate ceilings. A measurement over one of these fails the
// gate: Section 23.5 forbids meeting a target by excluding the failing feature
// or by rewriting the benchmark after seeing the result.
const (
	budgetVersionP95              = 50 * time.Millisecond
	budgetExactQueryP95           = 50 * time.Millisecond
	budgetLexicalSearchP95        = 150 * time.Millisecond
	budgetOneHopP95               = 100 * time.Millisecond
	budgetContextPlanP95          = time.Second
	budgetContextPlanVisitedNodes = 50_000
	budgetTenFileRefreshP95       = 1500 * time.Millisecond
	budgetNoChangeRefreshP95      = 250 * time.Millisecond
	budgetColdIndexReference      = 3 * time.Minute
	budgetIdleMCPRSSBytes         = 128 << 20
	budgetInteractivePeakBytes    = 256 << 20
	budgetIndexingPeakBytes       = 768 << 20
	budgetStorageRatio            = 3.5
)

// --- the generated corpus ---------------------------------------------------

// corpusPrefix is carried by every generated symbol in every language, so one
// workspace-symbol query reaches the whole corpus. Without it the parity rows
// would query terms that only match one language and a run that dropped the
// other languages would fingerprint identically.
const corpusPrefix = "Ctxbench"

// corpusSpec is the generator seam. Packages is the per-language package count;
// Seed drives the deterministic filler sizes, so a spec is a workload, not a
// coin flip.
//
// Helpers and HelperLines are the per-file SHAPE, and they exist because
// Section 23.1 pins the reference fixture by lines and bytes as well as by
// files: 10,000 files of the original ~0.5 KiB filler is a quarter of the
// line count and a fourteenth of the byte count the reference workload names,
// so a measurement over it describes a much smaller corpus than the one the
// budgets were written for. Helpers is the number of generated function
// bodies per source file and HelperLines the statement lines in each; when
// Helpers is zero the generator keeps its original shape -- a handful of
// one-line helpers whose count is drawn from Seed -- which is what the tiny,
// small-real and >200-file specs are measured and fingerprinted at.
type corpusSpec struct {
	Packages    int
	Seed        int64
	Helpers     int
	HelperLines int
}

// corpusTiny is sized for the parity rows: large enough to carry every language
// and a high-fanout caller, small enough that one unpaged page holds the whole
// symbol set (model.MaxPageItems is 200).
var corpusTiny = corpusSpec{Packages: 4, Seed: 21}

// generateCorpus writes the deterministic multi-language corpus of Section 23.1
// at spec's scale into dir: Go, Python and TypeScript sources that share
// corpusPrefix, a manifest per ecosystem, a documentation file, and one
// high-fanout Go file that calls every generated package.
func generateCorpus(t testing.TB, dir string, spec corpusSpec) {
	t.Helper()
	rng := rand.New(rand.NewSource(spec.Seed))
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for p := range spec.Packages {
		id := fmt.Sprintf("%02d", p)
		// The draw happens whichever shape is generated: a spec that asks for
		// the wide shape must not shift the filler sizes of the specs that do
		// not, and those are fingerprinted.
		fill := 2 + rng.Intn(4)
		// Each language gets its own symbol infix. Sharing a name across
		// languages makes workspace-symbol resolution answer with one node per
		// name, which would hide two thirds of the corpus from the capture.
		goP, pyP, tsP := corpusPrefix+"Go", corpusPrefix+"Py", corpusPrefix+"Ts"

		// The Go package name carries the prefix too: a Go symbol's qualified
		// name is package-qualified, and the workspace-symbol prefix tier
		// matches on the qualified name (search/exact.go:50).
		write(filepath.Join("src", "go", goP+id, "store.go"), fmt.Sprintf(`package %[2]s%[1]s

// %[2]sStore%[1]s holds one generated record.
type %[2]sStore%[1]s struct {
	%[2]sName string
}

// %[2]sLoad%[1]s returns the record name.
func (s *%[2]sStore%[1]s) %[2]sLoad%[1]s() string {
	return s.%[2]sName
}

// %[2]sRun%[1]s builds a store and reads it back.
func %[2]sRun%[1]s() string {
	s := &%[2]sStore%[1]s{%[2]sName: "%[1]s"}
	return s.%[2]sLoad%[1]s()
}
%[3]s`, id, goP, goFiller(spec, fill, goP, id)))

		write(filepath.Join("src", "py", "ctxbench_"+id+".py"), fmt.Sprintf(`"""Generated Python package %[1]s."""


class %[2]sStore%[1]s:
    def %[2]sLoad%[1]s(self):
        return "%[1]s"


def %[2]sRun%[1]s():
    return %[2]sStore%[1]s().%[2]sLoad%[1]s()
%[3]s`, id, pyP, pyFiller(spec, fill, pyP, id)))

		write(filepath.Join("src", "ts", "ctxbench_"+id+".ts"), fmt.Sprintf(`// Generated TypeScript package %[1]s.

export class %[2]sStore%[1]s {
  %[2]sLoad%[1]s(): string {
    return "%[1]s";
  }
}

export function %[2]sRun%[1]s(): string {
  return new %[2]sStore%[1]s().%[2]sLoad%[1]s();
}
%[3]s`, id, tsP, tsFiller(spec, fill, tsP, id)))
	}

	// The high-fanout case Section 23.1 asks for: one symbol that calls every
	// generated package, so a dropped fact family is visible as missing edges
	// and not only as missing nodes.
	var fanout strings.Builder
	goPkg := corpusPrefix + "Go"
	fanout.WriteString("package " + goPkg + "fanout\n\nimport (\n")
	for p := range spec.Packages {
		fanout.WriteString(fmt.Sprintf("\t\"example.com/ctxbench/src/go/%s%02d\"\n", goPkg, p))
	}
	fanout.WriteString(")\n\n// " + corpusPrefix + "GoFanOut calls every generated package.\nfunc " +
		corpusPrefix + "GoFanOut() []string {\n\treturn []string{\n")
	for p := range spec.Packages {
		fanout.WriteString(fmt.Sprintf("\t\t%[1]s%[2]02d.%[1]sRun%[2]02d(),\n", goPkg, p))
	}
	fanout.WriteString("\t}\n}\n")
	write(filepath.Join("src", "go", goPkg+"fanout", "fanout.go"), fanout.String())

	write("go.mod", "module example.com/ctxbench\n\ngo 1.27\n")
	write("package.json", "{\n  \"name\": \"ctxbench\",\n  \"version\": \"0.0.0\",\n  \"private\": true\n}\n")
	write("README.md", "# ctxbench\n\nGenerated benchmark corpus. Fixed content, fixed layout, no network.\n")
}

// --- the two filler shapes --------------------------------------------------
//
// Every generated source file ends in filler, and the filler is what makes a
// spec's line and byte counts what they are. There are two shapes:
//
//   - the NARROW shape (spec.Helpers == 0): n one-line helpers, n drawn from
//     the seed. It is what corpusTiny, corpusSmallReal and corpusOver200Files
//     are generated at, so it is frozen -- the parity fingerprint and the
//     >200-file session shape are both measured over it.
//   - the WIDE shape (spec.Helpers > 0): spec.Helpers function bodies per
//     file, each spec.HelperLines statements long, with the line widths real
//     source has. It exists so corpusReference can reach Section 23.1's
//     reference workload -- ~1M lines over ~80 MiB -- at the file count the
//     section pins.
//
// Both shapes are deterministic: given a spec, the bytes are fixed.

// narrowFiller renders n one-line helpers through line.
func narrowFiller(n int, line func(i int) string) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(line(i))
	}
	return b.String()
}

// goFiller renders one Go file's filler in the shape spec asks for.
func goFiller(spec corpusSpec, fill int, goP, id string) string {
	if spec.Helpers == 0 {
		return narrowFiller(fill, func(i int) string {
			return fmt.Sprintf("\n// %sNote%s%d documents a generated helper.\nfunc %sHelper%s%d() int { return %d }\n",
				goP, id, i, goP, id, i, i)
		})
	}
	var b strings.Builder
	for i := range spec.Helpers {
		fmt.Fprintf(&b, "\n// %[1]sNote%[2]s%[3]d documents one generated helper: the seed it folds, "+
			"the label it carries and the total it returns.\n"+
			"func %[1]sHelper%[2]s%[3]d(seed int, label string) (int, string) {\n\ttotal := seed\n",
			goP, id, i)
		for step := range spec.HelperLines {
			fmt.Fprintf(&b, "\ttotal = total*%[1]d + (seed+%[2]d)%%97 // step %[2]d of the generated body, "+
				"repeated verbatim by every file in this corpus\n", 3+step%7, step)
		}
		b.WriteString("\treturn total, label\n}\n")
	}
	return b.String()
}

// pyFiller renders one Python file's filler in the shape spec asks for.
func pyFiller(spec corpusSpec, fill int, pyP, id string) string {
	if spec.Helpers == 0 {
		return narrowFiller(fill, func(i int) string {
			return fmt.Sprintf("\n\ndef %sHelper%s%d():\n    return %d\n", pyP, id, i, i)
		})
	}
	var b strings.Builder
	for i := range spec.Helpers {
		fmt.Fprintf(&b, "\n\ndef %[1]sHelper%[2]s%[3]d(seed, label):\n"+
			"    \"\"\"%[1]sHelper%[2]s%[3]d folds its seed and returns it beside the label it was called with.\"\"\"\n"+
			"    total = seed\n", pyP, id, i)
		for step := range spec.HelperLines {
			fmt.Fprintf(&b, "    total = total * %[1]d + (seed + %[2]d) %%%% 97  # step %[2]d of the generated body, "+
				"repeated verbatim by every file\n", 3+step%7, step)
		}
		b.WriteString("    return total, label\n")
	}
	return b.String()
}

// tsFiller renders one TypeScript file's filler in the shape spec asks for.
func tsFiller(spec corpusSpec, fill int, tsP, id string) string {
	if spec.Helpers == 0 {
		return narrowFiller(fill, func(i int) string {
			return fmt.Sprintf("\nexport function %sHelper%s%d(): number {\n  return %d;\n}\n", tsP, id, i, i)
		})
	}
	var b strings.Builder
	for i := range spec.Helpers {
		fmt.Fprintf(&b, "\n// %[1]sNote%[2]s%[3]d documents one generated helper: the seed it folds, "+
			"the label it carries and the total it returns.\n"+
			"export function %[1]sHelper%[2]s%[3]d(seed: number, label: string): [number, string] {\n"+
			"  let total = seed;\n", tsP, id, i)
		for step := range spec.HelperLines {
			fmt.Fprintf(&b, "  total = total * %[1]d + ((seed + %[2]d) %%%% 97); // step %[2]d of the generated body, "+
				"repeated verbatim by every file\n", 3+step%7, step)
		}
		b.WriteString("  return [total, label];\n}\n")
	}
	return b.String()
}

// openCorpus indexes dir as a workspace and returns the opened services. The
// configuration is written to the user file rather than the project file
// because provider enablement is user-level trust (config.projectPermitted);
// only the language selection below is a project-level key.
func openCorpus(t *testing.T, ctx context.Context, repo string, languages []string) *app.Services {
	t.Helper()
	home := t.TempDir()
	userCfg := filepath.Join(home, "config", "codectx")
	if err := os.MkdirAll(userCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	// The three optional providers are switched off explicitly: an "auto"
	// provider that happens to be unavailable on this host would make the
	// fingerprint a statement about the host rather than about the corpus.
	user := "[storage]\ndata_dir = \"" + filepath.Join(home, "data") + "\"\n" +
		"[tools]\noffline = true\n" +
		"[providers.scip]\nenabled = false\n[providers.lsp]\nenabled = false\n[providers.dependence]\nenabled = false\n"
	if err := os.WriteFile(filepath.Join(userCfg, "config.toml"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(languages) > 0 {
		project := "[providers.tree_sitter]\nlanguages = [\"" + strings.Join(languages, "\", \"") + "\"]\n"
		if err := os.WriteFile(filepath.Join(repo, ".codectx.toml"), []byte(project), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))

	w, err := app.OpenWorkspace(ctx, repo, app.OpenOptions{})
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	svc := w.Services()
	if _, err := svc.Index(ctx, model.IndexRequest{}); err != nil {
		t.Fatalf("index: %v", err)
	}
	return svc
}

// --- the fingerprint --------------------------------------------------------

// fingerprint is the Section 23.5 comparison record: fact family -> the sorted
// canonical lines that family produced. It deliberately carries no generation,
// snapshot, repository or node identifier and no analysis key: those are
// operational or root-derived, and a comparison that tripped over them would
// report a difference on every run and prove nothing about the facts.
type fingerprint map[string][]string

func (f fingerprint) add(family, line string) { f[family] = append(f[family], line) }

func (f fingerprint) digest(family string) string {
	h := sha256.New()
	for _, line := range f[family] {
		h.Write([]byte(line))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (f fingerprint) families() []string {
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// compareFingerprints reports every way other differs from base, naming the
// family. A dropped family is reported as dropped rather than as a changed
// hash, because that is the difference the release gate has to be able to
// state out loud: an optimization that stopped producing a fact family.
func compareFingerprints(base, other fingerprint) []string {
	var diffs []string
	for _, family := range base.families() {
		switch {
		case other[family] == nil:
			diffs = append(diffs, fmt.Sprintf("fact family %q disappeared (%d lines in the baseline)",
				family, len(base[family])))
		case base.digest(family) != other.digest(family):
			diffs = append(diffs, fmt.Sprintf("fact family %q changed: %d lines/%s -> %d lines/%s",
				family, len(base[family]), base.digest(family), len(other[family]), other.digest(family)))
		}
	}
	for _, family := range other.families() {
		if base[family] == nil {
			diffs = append(diffs, fmt.Sprintf("fact family %q appeared (%d lines)", family, len(other[family])))
		}
	}
	return diffs
}

// capture records the capabilities, the facts and the query answers the corpus
// produces. Every page is asserted complete: a truncated capture would compare
// two prefixes and call them equal.
func capture(t *testing.T, ctx context.Context, svc *app.Services) fingerprint {
	t.Helper()
	f := fingerprint{}

	status, err := svc.IndexStatus(ctx, model.StatusRequest{})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, c := range status.Completeness {
		f.add("capability", fmt.Sprintf("%s|%s|%s|%s|%s", c.ProviderID, c.Capability, c.Scope, c.State, c.DiagnosticCode))
	}

	overview, err := svc.Overview(ctx, model.OverviewRequest{})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	requireComplete(t, "overview", overview.Meta)
	for _, item := range overview.Items {
		f.add("overview", fmt.Sprintf("%s|%s|%s|%s|%d|%d|%d|%d",
			item.Kind, item.Path, item.Name, item.Language, item.Depth, item.FileCount, item.SymbolCount, item.SourceBytes))
	}

	symbols, err := svc.Symbol(ctx, model.SymbolRequest{
		Query: corpusPrefix, Operation: model.SymbolWorkspaceSymbols, SemanticSource: model.SemanticCanonical})
	if err != nil {
		t.Fatalf("workspace symbols: %v", err)
	}
	requireComplete(t, "workspace symbols", symbols.Meta)
	for _, n := range symbols.Items {
		// Node, relation and file identifiers are hashes over the repository
		// root, so two runs in two temporary directories never share them;
		// the kind, language, name and signature are the fact.
		f.add("node/"+languageOf(n.Language), fmt.Sprintf("%s|%s|%s|%s", n.Kind, n.Name, n.QualifiedName, n.Signature))

		refs, err := svc.References(ctx, model.ReferenceRequest{
			NodeID: n.ID, Operation: model.ReferenceReferences, SemanticSource: model.SemanticCanonical})
		if err != nil {
			t.Fatalf("references for %s: %v", n.QualifiedName, err)
		}
		requireComplete(t, "references", refs.Meta)
		for _, r := range refs.Items {
			f.add("relation/"+string(r.Kind), fmt.Sprintf("%s|%s|%s|%s", r.Kind, r.FromName, n.QualifiedName, r.Precision))
		}
	}

	// Query answers, not only stored facts: a lexical index that stopped being
	// written is a capability reduction the node families would not show.
	for _, term := range []string{corpusPrefix, corpusPrefix + "GoRun00", "generated"} {
		hits, err := svc.Search(ctx, model.SearchRequest{Query: term})
		if err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		requireComplete(t, "search "+term, hits.Meta)
		for _, h := range hits.Items {
			// Scores and reasons are ranking detail, not facts: they carry no
			// family information and are the fields most likely to wobble.
			f.add("search/"+term, fmt.Sprintf("%s|%s|%s|%s|%d", h.Path, h.Kind, h.Name, h.Tier, h.OccurrenceCount))
		}
	}

	for _, family := range f.families() {
		sort.Strings(f[family])
	}
	return f
}

func requireComplete(t *testing.T, what string, meta model.QueryMeta) {
	t.Helper()
	if meta.NextCursor != "" || meta.Truncated {
		t.Fatalf("%s did not fit one page (truncated=%v): the corpus must stay under model.MaxPageItems "+
			"or the fingerprint compares two prefixes", what, meta.Truncated)
	}
}

func languageOf(language string) string {
	if language == "" {
		return "none"
	}
	return language
}

// TestFingerprintParity is the Section 23.5 gate on optimizations: the same
// corpus indexed twice produces the same capability, fact and query-answer
// fingerprint, and a run that stops producing a fact family is reported as a
// dropped family rather than as a faster run.
//
// Failure mode it protects: a capability reduction ships as a performance win.
// Section 23.1 forbids meeting a target by excluding the failing feature, and
// nothing else in the tree compares two complete runs -- so an "optimization"
// that narrowed the parsed language set, or stopped writing a relation family,
// would show up only as a better number.
func TestFingerprintParity(t *testing.T) {
	if testing.Short() {
		t.Skip("fingerprint parity indexes three corpora; run without -short")
	}
	ctx := context.Background()

	// Two independent runs, in two roots, of the same generated corpus: the
	// fingerprint must be a statement about the corpus, not about where it sat.
	baseRepo, otherRepo := t.TempDir(), t.TempDir()
	generateCorpus(t, baseRepo, corpusTiny)
	generateCorpus(t, otherRepo, corpusTiny)
	base := capture(t, ctx, openCorpus(t, ctx, baseRepo, nil))
	other := capture(t, ctx, openCorpus(t, ctx, otherRepo, nil))

	// The mutation below removes Python and TypeScript, so the baseline must
	// actually carry them: a corpus whose symbols are all Go would make the
	// comparison below pass for the wrong reason.
	for _, want := range []string{"node/go", "node/python", "node/typescript", "relation/calls", "capability"} {
		if len(base[want]) == 0 {
			t.Fatalf("the baseline fingerprint carries no %q lines; families: %v", want, base.families())
		}
	}
	if diffs := compareFingerprints(base, other); len(diffs) != 0 {
		t.Fatalf("two runs of the same corpus disagree:\n%s", strings.Join(diffs, "\n"))
	}

	// The reduction: the same corpus parsed as Go only. This is exactly the
	// shape of an "optimization" -- less work, faster -- and it must fail.
	narrowedRepo := t.TempDir()
	generateCorpus(t, narrowedRepo, corpusTiny)
	narrowed := capture(t, ctx, openCorpus(t, ctx, narrowedRepo, []string{"go"}))

	diffs := compareFingerprints(base, narrowed)
	if len(diffs) == 0 {
		t.Fatal("a run that parsed only Go fingerprinted identically to the full run")
	}
	dropped := strings.Join(diffs, "\n")
	for _, want := range []string{`"node/python"`, `"node/typescript"`} {
		if !strings.Contains(dropped, want) {
			t.Fatalf("the comparison must name the dropped family %s; it reported:\n%s", want, dropped)
		}
	}
	t.Logf("reduction reported:\n%s", dropped)

	// The capability rows are what an operator reads, and they are why the
	// fact comparison has to exist: the structural capability still says
	// "fresh" over a corpus whose Python and TypeScript facts are gone.
	if base.digest("capability") != narrowed.digest("capability") {
		t.Logf("capability rows also moved: %v -> %v", base["capability"], narrowed["capability"])
	} else {
		t.Logf("capability rows are identical across the reduction: %v", base["capability"])
	}
}

// --- the pinned corpora manifest -------------------------------------------

// corpusEntry is one pinned real repository recorded beside the results as the
// Step 2 differential oracle. The generated corpus above is the reference
// workload (Section 23.1: real repositories are "an additional signal, not a
// moving substitute for the fixed fixture"), so an entry here is a record, not
// an input this package fetches: nothing in this lane clones anything.
type corpusEntry struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Commit      string `json:"commit"`
	Description string `json:"description"`
	// Measured marks an entry whose counts were produced by a run, not
	// estimated. Counts are required on a measured entry and must be absent on
	// an unmeasured one, so a placeholder can never be read as evidence.
	Measured    bool   `json:"measured"`
	Files       int64  `json:"files,omitempty"`
	Lines       int64  `json:"lines,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
	FixtureHash string `json:"fixture_hash,omitempty"`
}

type corporaManifest struct {
	SchemaVersion int           `json:"schema_version"`
	Corpora       []corpusEntry `json:"corpora"`
}

var (
	fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	fixtureSHA256 = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// validateEntry enforces the oracle rules. The commit rule is unconditional:
// an entry pinned to a branch or a tag names a moving tree, and a differential
// oracle whose other side moves is not an oracle.
func validateEntry(e corpusEntry) error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("entry has no name")
	}
	if !strings.HasPrefix(e.URL, "https://") {
		return fmt.Errorf("%s: url %q is not an https clone url", e.Name, e.URL)
	}
	if !fullCommitSHA.MatchString(e.Commit) {
		return fmt.Errorf("%s: commit %q is not a full 40-character lowercase commit sha", e.Name, e.Commit)
	}
	if strings.TrimSpace(e.Description) == "" {
		return fmt.Errorf("%s: entry has no description of what the corpus contributes", e.Name)
	}
	if e.Measured {
		if e.Files <= 0 || e.Lines <= 0 || e.Bytes <= 0 {
			return fmt.Errorf("%s: measured entry carries files=%d lines=%d bytes=%d",
				e.Name, e.Files, e.Lines, e.Bytes)
		}
		if !fixtureSHA256.MatchString(e.FixtureHash) {
			return fmt.Errorf("%s: measured entry fixture_hash %q is not sha256:<64 hex>", e.Name, e.FixtureHash)
		}
		return nil
	}
	if e.Files != 0 || e.Lines != 0 || e.Bytes != 0 || e.FixtureHash != "" {
		return fmt.Errorf("%s: unmeasured entry carries counts; a count nobody measured is not evidence", e.Name)
	}
	return nil
}

// TestCorporaManifest checks corpora.json against the oracle rules and then
// checks the rules themselves against a repository that is actually present --
// this one, at its current tip, measured here.
//
// Failure mode it protects: a "pinned" corpus that tracks a branch is not an
// oracle. A manifest entry that names a moving ref, or that carries counts
// nobody measured, turns the differential comparison into a comparison against
// whatever the upstream default branch happens to be on the day it runs.
func TestCorporaManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("bench package tests skip under -short (doc.go); run without -short")
	}
	raw, err := os.ReadFile("corpora.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest corporaManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("corpora.json: %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("corpora.json schema_version = %d, want 1", manifest.SchemaVersion)
	}
	if len(manifest.Corpora) == 0 {
		t.Fatal("corpora.json records no corpus")
	}
	seen := map[string]bool{}
	for _, e := range manifest.Corpora {
		if err := validateEntry(e); err != nil {
			t.Errorf("corpora.json: %v", err)
		}
		if seen[e.Name] {
			t.Errorf("corpora.json records %q twice", e.Name)
		}
		seen[e.Name] = true
	}

	// The rules must reject every way an entry stops being an oracle. This leg
	// needs no repository, so it runs before the measurement below, which is
	// skipped on a host without Git.
	pinned := "e8dc8e8f2a6e83e0a4a5f0c14b3f9a0a1c2d3e4f"
	// And they must reject every way an entry stops being an oracle.
	for _, bad := range []struct {
		why   string
		entry corpusEntry
	}{
		{"branch ref", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: "main", Description: "d"}},
		{"short sha", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: pinned[:12], Description: "d"}},
		{"uppercase sha", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: strings.ToUpper(pinned), Description: "d"}},
		{"no url", corpusEntry{Name: "x", Commit: pinned, Description: "d"}},
		{"no description", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: pinned}},
		{"measured without counts", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: pinned,
			Description: "d", Measured: true}},
		{"measured without fixture hash", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: pinned,
			Description: "d", Measured: true, Files: 1, Lines: 1, Bytes: 1}},
		{"unmeasured with counts", corpusEntry{Name: "x", URL: "https://example.invalid/x", Commit: pinned,
			Description: "d", Files: 1}},
	} {
		if err := validateEntry(bad.entry); err == nil {
			t.Errorf("an entry with a %s was accepted", bad.why)
		}
	}

	// The rules must accept a real, genuinely pinned, genuinely measured
	// repository. This one is the repository that is present, so it is the one
	// the shape is proved against; the tip moves with every commit, so the
	// entry is built here rather than asserted against the recorded one.
	live := measureRepository(t, filepath.Join("..", ".."), "codectx")
	if err := validateEntry(live); err != nil {
		t.Fatalf("the rules reject a real pinned repository: %v", err)
	}
	t.Logf("live shape proof: commit=%s files=%d lines=%d bytes=%d hash=%s",
		live.Commit, live.Files, live.Lines, live.Bytes, live.FixtureHash)

}

// measureRepository builds a measured entry for the Git checkout at dir: its
// tip commit, and the file/line/byte counts and fixture hash over the source
// files Git tracks there. It reads tracked files only, so it measures the
// repository rather than whatever a build left behind. dir must be the
// repository root: `git ls-files` lists the current directory and below, so a
// subdirectory would produce counts that describe a subtree under a name that
// claims the whole repository.
func measureRepository(t *testing.T, dir, name string) corpusEntry {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not on PATH, so the shape cannot be proved against a present repository: %v", err)
	}
	run := func(args ...string) string {
		out, err := exec.Command(git, append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := run("rev-parse", "HEAD")
	origin, err := exec.Command(git, "-C", dir, "remote", "get-url", "origin").Output()
	url := strings.TrimSpace(string(origin))
	if err != nil || !strings.HasPrefix(url, "https://") {
		// A checkout with no https origin is still a pinned tree; the manifest
		// rule is about the commit, and the canonical url is recorded rather
		// than guessed from a local remote.
		url = "https://github.com/Sawmonabo/codectx"
	}

	entry := corpusEntry{Name: name, URL: url, Commit: commit, Measured: true,
		Description: "measured at the tip this run observed, over the files Git tracks there whose " +
			"extension lang.Of recognizes as source"}
	h := sha256.New()
	for _, rel := range strings.Split(run("ls-files", "-z"), "\x00") {
		if rel == "" || lang.Of(rel) == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			continue // a tracked path that is not a readable regular file is not source
		}
		entry.Files++
		entry.Bytes += int64(len(body))
		entry.Lines += int64(strings.Count(string(body), "\n"))
		sum := sha256.Sum256(body)
		h.Write([]byte(rel + "\x00" + hex.EncodeToString(sum[:]) + "\n"))
	}
	if entry.Files == 0 {
		t.Fatalf("%s tracks no source files", dir)
	}
	entry.FixtureHash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return entry
}
