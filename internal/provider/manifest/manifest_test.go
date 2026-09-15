package manifest_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// fixture is the polyglot repository both tests run over: one manifest per
// supported ecosystem exercising the dependency kinds, explicit workspace
// inheritance, an unresolved dynamic value and property reference, a Maven
// <exclusions> block whose coordinates must not be taken for the enclosing
// dependency's, a malformed manifest, a Markdown document with headings and
// source links, a file whose single line is longer than a search chunk, and
// a multi-line file longer than a chunk so the line-boundary overlap is
// exercised.
var fixture = map[string]string{
	"go.mod":  "module example.com/app\n\ngo 1.27\n\nrequire (\n\tgithub.com/a/b v1.2.3\n\tgithub.com/c/d v0.1.0 // indirect\n)\n\nreplace github.com/a/b => ../b\n",
	"go.work": "go 1.27\n\nuse (\n\t.\n\t./tools\n)\n",
	"web/package.json": `{
  "name": "@acme/web",
  "version": "1.0.0",
  "scripts": {"build": "tsc"},
  "dependencies": {"react": "^18.0.0"},
  "devDependencies": {"typescript": "^5.0.0", "react": "^18.0.0"},
  "peerDependencies": {"react-dom": "^18.0.0"},
  "optionalDependencies": {"fsevents": "^2.3.0"}
}
`,
	"crates/core/Cargo.toml": "[package]\nname = \"core\"\nversion.workspace = true\nedition = \"2021\"\n\n[dependencies]\nserde = { workspace = true }\nanyhow = \"1\"\n\n[dev-dependencies]\ntempfile = \"3\"\n\n[build-dependencies]\ncc = \"1\"\n",
	"Cargo.toml":             "[workspace]\nmembers = [\"crates/*\"]\n\n[workspace.dependencies]\nserde = \"1.0\"\n",
	"py/pyproject.toml":      "[project]\nname = \"My_Service\"\ndynamic = [\"version\"]\ndependencies = [\n  \"requests>=2\",\n  \"Django\",\n]\n\n[project.optional-dependencies]\ntest = [\"pytest\"]\n\n[dependency-groups]\ndev = [\"ruff\"]\n\n[build-system]\nrequires = [\"hatchling\"]\nbuild-backend = \"hatchling.build\"\n",
	"java/pom.xml": `<?xml version="1.0"?>
<project>
  <parent>
    <groupId>org.acme</groupId>
    <artifactId>parent</artifactId>
    <version>1.0</version>
  </parent>
  <artifactId>svc</artifactId>
  <properties>
    <guava.version>33.0</guava.version>
  </properties>
  <modules>
    <module>sub</module>
  </modules>
  <dependencies>
    <dependency>
      <groupId>com.google.guava</groupId>
      <artifactId>guava</artifactId>
      <version>${guava.version}</version>
      <exclusions>
        <exclusion>
          <groupId>org.excluded</groupId>
          <artifactId>badlib</artifactId>
        </exclusion>
      </exclusions>
    </dependency>
    <dependency>
      <groupId>junit</groupId>
      <artifactId>junit</artifactId>
      <version>4.13</version>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>org.projectlombok</groupId>
      <artifactId>lombok</artifactId>
      <version>1.18</version>
      <scope>provided</scope>
      <optional>true</optional>
    </dependency>
  </dependencies>
</project>
`,
	"broken/Cargo.toml": "[package\nname = \"oops\"\n",
	"docs/README.md":    "# Service\n\nSee [go.mod](../go.mod) and [the web app](../web/) or <https://example.com>.\n\n```\n# not a heading\n[nope](../nope.go)\n```\n\n## Usage\n\nMore.\n\n## Usage\n",
	// One 40001-byte line: the 32 KiB budget falls inside a two-byte rune,
	// so the split must back off to the rune boundary at 32767.
	"docs/long.txt": "x" + strings.Repeat("é", 20000),
	// 600 lines of 59 bytes = 35400 bytes: over one chunk, and cut on a
	// line boundary, so consecutive chunks must overlap by whole lines.
	"docs/wide.txt":   strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit sed\n", 600),
	"assets/logo.bin": "\x89PNG\r\n\x1a\n\x00\x00\x00\x00IHDR",
}

func newProviders(t *testing.T) (*filesystem.Provider, *manifest.Provider) {
	t.Helper()
	fs, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fs, mf
}

// TestManifestConform runs the shared conformance fixture over every
// manifest and document of the polyglot repository. Failure mode: a manifest
// unit whose package, dependency or heading identities depended on map
// iteration order or flush timing would publish different IDs for the same
// bytes, so a dependency named by two manifests would not converge on one
// node and a `depends_on` edge could reach storage before its endpoints.
func TestManifestConform(t *testing.T) {
	_, mf := newProviders(t)
	paths := make([]string, 0, len(fixture))
	for path := range fixture {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if manifest.Recognize(path) {
			providertest.Conform(t, mf, fixture, filesystem.ScopeKey(path), []string{path})
		}
	}
}

// TestCanonicalFacts is the single canonical fact-set comparison of Task 7:
// both providers run over the polyglot repository through the production
// unit path (manifest units depending on their filesystem unit) and the
// rendered facts must equal the expected set exactly. Failure modes it
// protects: a dependency kind collapsed by deduplication (react is both
// runtime and dev), an inherited or dynamic value invented instead of left
// unresolved, a malformed manifest failing the provider run instead of its
// own capability, a link or module resolved to the wrong path, a nested
// <exclusions> coordinate overwriting the dependency that encloses it, and a
// chunk whose body is not the exact source bytes of its range or whose
// overlap with the previous chunk is unbounded or leaves a gap (wrong chunk
// bytes is wrong source served). It also pins that every heading of a
// document is its own section node contained by the document -- the README
// fixture repeats `## Usage` deliberately -- because headings sharing the
// document's node id collapse a whole file's headings into one search hit.
func TestCanonicalFacts(t *testing.T) {
	fs, mf := newProviders(t)
	h := providertest.New(t, fixture)
	paths := make([]string, 0, len(fixture))
	for p := range fixture {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	cap := &capture{}
	states := map[string]model.CapabilityState{}
	for _, p := range paths {
		fsUnit := runUnit(t, h, fs, p, cap, states)
		if manifest.Recognize(p) {
			runUnit(t, h, mf, p, cap, states, fsUnit)
		}
	}
	got := cap.render(t, states)
	want := strings.Split(strings.TrimSpace(expected), "\n")
	if !slices.Equal(got, want) {
		t.Fatalf("canonical facts differ:\n--- got\n%s\n--- want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Every chunk body must be exactly the bytes of its range, and the
	// chunks of one file must advance with a bounded overlap.
	chunks := map[string][]model.SearchUnit{}
	for _, doc := range cap.search {
		if doc.Kind != model.NodeFile {
			continue
		}
		src := fixture[doc.Path]
		if doc.Bytes.End > uint64(len(src)) || doc.Body != src[doc.Bytes.Start:doc.Bytes.End] {
			t.Fatalf("chunk %s [%d,%d) body is not the source bytes of its range", doc.Path, doc.Bytes.Start, doc.Bytes.End)
		}
		if len(doc.Body) > filesystem.ChunkBytes {
			t.Fatalf("chunk %s [%d,%d) is %d bytes, over the 32 KiB ceiling", doc.Path, doc.Bytes.Start, doc.Bytes.End, len(doc.Body))
		}
		chunks[doc.Path] = append(chunks[doc.Path], doc)
	}
	for path, docs := range chunks {
		sort.Slice(docs, func(i, j int) bool { return docs[i].Bytes.Start < docs[j].Bytes.Start })
		for i := 1; i < len(docs); i++ {
			prev, cur := docs[i-1], docs[i]
			if cur.Bytes.Start > prev.Bytes.End || cur.Bytes.End <= prev.Bytes.End {
				t.Fatalf("chunks %s [%d,%d) and [%d,%d) leave a gap or make no progress",
					path, prev.Bytes.Start, prev.Bytes.End, cur.Bytes.Start, cur.Bytes.End)
			}
			over := fixture[path][cur.Bytes.Start:prev.Bytes.End]
			if len(over) > filesystem.MaxOverlapBytes || strings.Count(over, "\n") > filesystem.MaxOverlapLines {
				t.Fatalf("chunks %s [%d,%d) and [%d,%d) overlap by %d bytes / %d lines, over the bound",
					path, prev.Bytes.Start, prev.Bytes.End, cur.Bytes.Start, cur.Bytes.End, len(over), strings.Count(over, "\n"))
			}
		}
	}
}

// runUnit plans, opens and runs one unit through RunUnit with the capturing
// output, recording the per-file capability states the result carried.
func runUnit(t *testing.T, h *providertest.Harness, p provider.Provider, path string, cap *capture, states map[string]model.CapabilityState, deps ...model.UnitID) model.UnitID {
	t.Helper()
	u := h.Plan(t, p, filesystem.ScopeKey(path), []string{path}, deps...)
	out := h.Begin(t, u, []string{path})
	result, err := provider.RunUnit(context.Background(), p, u.Request, &captureOutput{UnitOutput: out, c: cap}, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("RunUnit(%s, %s): %v", p.Descriptor().ID, path, err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state = %s for %s", result.State, path)
	}
	for _, cs := range result.Capabilities {
		states[cs.ProviderID+" "+cs.Capability+" "+cs.Scope] = cs
	}
	return u.Build.Spec.ID
}

// capture retains every fact that reached storage across units.
type capture struct {
	mu        sync.Mutex
	nodes     map[model.NodeID]model.Node
	relations []model.RelationFact
	search    []model.SearchUnit
}

type captureOutput struct {
	provider.UnitOutput
	c *capture
}

func (o *captureOutput) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	o.c.mu.Lock()
	if o.c.nodes == nil {
		o.c.nodes = map[model.NodeID]model.Node{}
	}
	for _, f := range facts {
		// The located fact (the file's own unit) wins over a referencing one.
		if have, ok := o.c.nodes[f.Node.ID]; !ok || (have.FileID == "" && f.Node.FileID != "") {
			o.c.nodes[f.Node.ID] = f.Node
		}
	}
	o.c.mu.Unlock()
	return o.UnitOutput.PutNodes(ctx, facts)
}

func (o *captureOutput) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	o.c.mu.Lock()
	o.c.relations = append(o.c.relations, facts...)
	o.c.mu.Unlock()
	return o.UnitOutput.PutRelations(ctx, facts)
}

func (o *captureOutput) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	o.c.mu.Lock()
	o.c.search = append(o.c.search, docs...)
	o.c.mu.Unlock()
	return o.UnitOutput.PutSearchUnits(ctx, docs)
}

// render produces the sorted canonical lines: every node with its kind,
// qualified name and metadata; every relation occurrence with its detail;
// every search document with its kind, name and range; every non-fresh
// capability state.
func (c *capture) render(t *testing.T, states map[string]model.CapabilityState) []string {
	t.Helper()
	name := func(id model.NodeID) string {
		n, ok := c.nodes[id]
		if !ok {
			t.Fatalf("relation references node %s with no fact", id)
		}
		return string(n.Kind) + " " + n.QualifiedName
	}
	var out []string
	for _, n := range c.nodes {
		line := fmt.Sprintf("node %s %s", n.Kind, n.QualifiedName)
		if n.Language != "" {
			line += " lang=" + n.Language
		}
		if n.FileID != "" {
			line += " located"
		}
		if len(n.Metadata) > 0 {
			line += " " + string(n.Metadata)
		}
		out = append(out, line)
	}
	seen := map[string]bool{}
	for _, r := range c.relations {
		for _, ev := range r.Evidence {
			line := fmt.Sprintf("rel %s %s -> %s", r.Relation.Kind, name(r.Relation.From), name(r.Relation.To))
			if ev.Range != nil {
				line += fmt.Sprintf(" [%d,%d)", ev.Range.Start.Byte, ev.Range.End.Byte)
			}
			line += " " + string(ev.Precision)
			if ev.Detail != "" {
				line += " " + ev.Detail
			}
			if !seen[line] {
				seen[line] = true
				out = append(out, line)
			}
		}
	}
	for _, d := range c.search {
		line := fmt.Sprintf("search %s %s [%d,%d)", d.Kind, d.Path, d.Bytes.Start, d.Bytes.End)
		if d.Name != "" {
			line += " name=" + d.Name
		}
		if d.QualifiedName != "" {
			line += " qn=" + d.QualifiedName
		}
		out = append(out, line)
	}
	for _, cs := range states {
		if cs.State != model.CapabilityFresh {
			out = append(out, fmt.Sprintf("capability %s %s %s %s %s", cs.ProviderID, cs.Capability, cs.Scope, cs.State, cs.DiagnosticCode))
		}
	}
	sort.Strings(out)
	return out
}

const expected = `
capability filesystem search file:assets/logo.bin unavailable CTX_PROVIDER_UNAVAILABLE
capability manifest manifests file:broken/Cargo.toml failed CTX_ARGUMENT_INVALID
node configuration cargo:workspace:Cargo.toml lang=rust located {"workspace_members":["crates/*"]}
node configuration go:work:go.work lang=go located {"go":"1.27"}
node dependency cargo:anyhow lang=rust
node dependency cargo:cc lang=rust
node dependency cargo:serde lang=rust
node dependency cargo:tempfile lang=rust
node dependency go:github.com/a/b lang=go
node dependency go:github.com/c/d lang=go
node dependency maven:com.google.guava:guava lang=java
node dependency maven:junit:junit lang=java
node dependency maven:org.acme:parent lang=java
node dependency maven:org.projectlombok:lombok lang=java
node dependency npm:fsevents lang=javascript
node dependency npm:react lang=javascript
node dependency npm:react-dom lang=javascript
node dependency npm:typescript lang=javascript
node dependency pypi:django lang=python
node dependency pypi:hatchling lang=python
node dependency pypi:pytest lang=python
node dependency pypi:requests lang=python
node dependency pypi:ruff lang=python
node directory assets
node directory broken
node directory crates
node directory crates/core
node directory docs
node directory java
node directory java/sub
node directory py
node directory tools
node directory web
node document docs/README.md lang=markdown located {"format":"markdown"}
node document docs/long.txt lang=text located {"format":"text"}
node document docs/wide.txt lang=text located {"format":"text"}
node file Cargo.toml lang=toml located {"binary":false,"executable":false,"format":"cargo","size":75}
node file assets/logo.bin located {"binary":true,"executable":false,"size":16}
node file broken/Cargo.toml lang=toml located {"binary":false,"executable":false,"format":"cargo","size":23}
node file crates/core/Cargo.toml lang=toml located {"binary":false,"executable":false,"format":"cargo","size":190}
node file docs/README.md lang=markdown located {"binary":false,"executable":false,"format":"markdown","size":159}
node file docs/long.txt lang=text located {"binary":false,"executable":false,"format":"text","size":40001}
node file docs/wide.txt lang=text located {"binary":false,"executable":false,"format":"text","size":35400}
node file go.mod located {"binary":false,"executable":false,"format":"gomod","size":135}
node file go.work located {"binary":false,"executable":false,"format":"gowork","size":29}
node file java/pom.xml lang=xml located {"binary":false,"executable":false,"format":"pom","size":1035}
node file py/pyproject.toml lang=toml located {"binary":false,"executable":false,"format":"pyproject","size":262}
node file web/package.json lang=json located {"binary":false,"executable":false,"format":"packagejson","size":284}
node module go:example.com/app lang=go located {"go":"1.27"}
node package cargo:core lang=rust located {"edition":"2021","inherited":["version"]}
node package maven:org.acme:svc lang=java located {"inherited":["groupId","version"],"properties":{"guava.version":"33.0"},"version":"1.0"}
node package npm:@acme/web lang=javascript located {"scripts":{"build":"tsc"},"version":"1.0.0"}
node package pypi:my-service lang=python located {"build_backend":"hatchling.build","dynamic":["version"]}
node repository .
node section docs/README.md#Service > Usage lang=markdown located
node section docs/README.md#Service > Usage~2 lang=markdown located
node section docs/README.md#Service lang=markdown located
rel builds package maven:org.acme:svc -> directory java/sub [268,288) syntax {"module":"sub"}
rel configures configuration go:work:go.work -> directory tools [19,26) syntax {"use":"./tools"}
rel configures configuration go:work:go.work -> repository . [16,17) syntax {"use":"."}
rel configures module go:example.com/app -> dependency go:github.com/a/b [104,134) syntax {"kind":"runtime","replace":"../b"}
rel contains directory assets -> file assets/logo.bin syntax
rel contains directory broken -> file broken/Cargo.toml syntax
rel contains directory crates -> directory crates/core syntax
rel contains directory crates/core -> file crates/core/Cargo.toml syntax
rel contains directory docs -> file docs/README.md syntax
rel contains directory docs -> file docs/long.txt syntax
rel contains directory docs -> file docs/wide.txt syntax
rel contains directory java -> file java/pom.xml syntax
rel contains directory py -> file py/pyproject.toml syntax
rel contains directory web -> file web/package.json syntax
rel contains document docs/README.md -> section docs/README.md#Service > Usage [133,141) syntax
rel contains document docs/README.md -> section docs/README.md#Service > Usage~2 [150,158) syntax
rel contains document docs/README.md -> section docs/README.md#Service [0,9) syntax
rel contains repository . -> directory assets syntax
rel contains repository . -> directory broken syntax
rel contains repository . -> directory crates syntax
rel contains repository . -> directory docs syntax
rel contains repository . -> directory java syntax
rel contains repository . -> directory py syntax
rel contains repository . -> directory web syntax
rel contains repository . -> file Cargo.toml syntax
rel contains repository . -> file go.mod syntax
rel contains repository . -> file go.work syntax
rel defines file Cargo.toml -> configuration cargo:workspace:Cargo.toml [12,35) syntax
rel defines file crates/core/Cargo.toml -> package cargo:core [10,23) syntax
rel defines file docs/README.md -> document docs/README.md heuristic
rel defines file docs/long.txt -> document docs/long.txt heuristic
rel defines file docs/wide.txt -> document docs/wide.txt heuristic
rel defines file go.mod -> module go:example.com/app [0,22) syntax
rel defines file go.work -> configuration go:work:go.work syntax
rel defines file java/pom.xml -> package maven:org.acme:svc syntax
rel defines file py/pyproject.toml -> package pypi:my-service [10,29) syntax
rel defines file web/package.json -> package npm:@acme/web [4,23) syntax
rel depends_on configuration cargo:workspace:Cargo.toml -> dependency cargo:serde [61,74) syntax {"declared":"workspace","kind":"runtime","requirement":"1.0"}
rel depends_on module go:example.com/app -> dependency go:github.com/a/b [44,65) syntax {"indirect":"false","kind":"runtime","requirement":"v1.2.3"}
rel depends_on module go:example.com/app -> dependency go:github.com/c/d [67,88) syntax {"indirect":"true","kind":"runtime","requirement":"v0.1.0"}
rel depends_on package cargo:core -> dependency cargo:anyhow [111,123) syntax {"kind":"runtime","requirement":"1"}
rel depends_on package cargo:core -> dependency cargo:cc [181,189) syntax {"kind":"build","requirement":"1"}
rel depends_on package cargo:core -> dependency cargo:serde [82,110) syntax {"inherited":"workspace","kind":"runtime"}
rel depends_on package cargo:core -> dependency cargo:tempfile [144,158) syntax {"kind":"dev","requirement":"3"}
rel depends_on package maven:org.acme:svc -> dependency maven:com.google.guava:guava [323,638) syntax {"kind":"runtime","requirement":"${guava.version}","unresolved":"property"}
rel depends_on package maven:org.acme:svc -> dependency maven:junit:junit [643,797) syntax {"kind":"test","requirement":"4.13","scope":"test"}
rel depends_on package maven:org.acme:svc -> dependency maven:org.acme:parent [34,149) syntax {"kind":"build","requirement":"1.0","role":"parent"}
rel depends_on package maven:org.acme:svc -> dependency maven:org.projectlombok:lombok [802,1005) syntax {"kind":"optional","optional":"true","requirement":"1.18","scope":"provided"}
rel depends_on package npm:@acme/web -> dependency npm:fsevents [260,280) syntax {"kind":"optional","requirement":"^2.3.0"}
rel depends_on package npm:@acme/web -> dependency npm:react [164,182) syntax {"kind":"dev","requirement":"^18.0.0"}
rel depends_on package npm:@acme/web -> dependency npm:react [97,115) syntax {"kind":"runtime","requirement":"^18.0.0"}
rel depends_on package npm:@acme/web -> dependency npm:react-dom [208,230) syntax {"kind":"peer","requirement":"^18.0.0"}
rel depends_on package npm:@acme/web -> dependency npm:typescript [140,162) syntax {"kind":"dev","requirement":"^5.0.0"}
rel depends_on package pypi:my-service -> dependency pypi:django [88,96) syntax {"kind":"runtime"}
rel depends_on package pypi:my-service -> dependency pypi:hatchling [215,226) syntax {"kind":"build"}
rel depends_on package pypi:my-service -> dependency pypi:pytest [141,149) syntax {"extra":"test","kind":"optional"}
rel depends_on package pypi:my-service -> dependency pypi:requests [71,84) syntax {"kind":"runtime","requirement":">=2"}
rel depends_on package pypi:my-service -> dependency pypi:ruff [179,185) syntax {"group":"dev","kind":"dev"}
rel documents document docs/README.md -> directory docs heuristic
rel documents document docs/README.md -> directory web [53,60) heuristic {"link":"../web/"}
rel documents document docs/README.md -> file go.mod [24,33) heuristic {"link":"../go.mod"}
search configuration Cargo.toml [12,35) name=workspace qn=cargo:workspace:Cargo.toml
search configuration go.work [0,0) name=go.work qn=go:work:go.work
search dependency Cargo.toml [61,74) name=serde qn=cargo:serde
search dependency crates/core/Cargo.toml [111,123) name=anyhow qn=cargo:anyhow
search dependency crates/core/Cargo.toml [144,158) name=tempfile qn=cargo:tempfile
search dependency crates/core/Cargo.toml [181,189) name=cc qn=cargo:cc
search dependency crates/core/Cargo.toml [82,110) name=serde qn=cargo:serde
search dependency go.mod [44,65) name=github.com/a/b qn=go:github.com/a/b
search dependency go.mod [67,88) name=github.com/c/d qn=go:github.com/c/d
search dependency java/pom.xml [323,638) name=com.google.guava:guava qn=maven:com.google.guava:guava
search dependency java/pom.xml [34,149) name=org.acme:parent qn=maven:org.acme:parent
search dependency java/pom.xml [643,797) name=junit:junit qn=maven:junit:junit
search dependency java/pom.xml [802,1005) name=org.projectlombok:lombok qn=maven:org.projectlombok:lombok
search dependency py/pyproject.toml [141,149) name=pytest qn=pypi:pytest
search dependency py/pyproject.toml [179,185) name=ruff qn=pypi:ruff
search dependency py/pyproject.toml [215,226) name=hatchling qn=pypi:hatchling
search dependency py/pyproject.toml [71,84) name=requests qn=pypi:requests
search dependency py/pyproject.toml [88,96) name=django qn=pypi:django
search dependency web/package.json [140,162) name=typescript qn=npm:typescript
search dependency web/package.json [208,230) name=react-dom qn=npm:react-dom
search dependency web/package.json [260,280) name=fsevents qn=npm:fsevents
search dependency web/package.json [97,115) name=react qn=npm:react
search document docs/README.md [0,0) name=README.md qn=docs/README.md
search document docs/long.txt [0,0) name=long.txt qn=docs/long.txt
search document docs/wide.txt [0,0) name=wide.txt qn=docs/wide.txt
search file Cargo.toml [0,75)
search file broken/Cargo.toml [0,23)
search file crates/core/Cargo.toml [0,190)
search file docs/README.md [0,159)
search file docs/long.txt [0,32767)
search file docs/long.txt [32767,40001)
search file docs/wide.txt [0,32745)
search file docs/wide.txt [32627,35400)
search file go.mod [0,135)
search file go.work [0,29)
search file java/pom.xml [0,1035)
search file py/pyproject.toml [0,262)
search file web/package.json [0,284)
search module go.mod [0,22) name=example.com/app qn=go:example.com/app
search package crates/core/Cargo.toml [10,23) name=core qn=cargo:core
search package java/pom.xml [0,0) name=svc qn=maven:org.acme:svc
search package py/pyproject.toml [10,29) name=My_Service qn=pypi:my-service
search package web/package.json [4,23) name=@acme/web qn=npm:@acme/web
search section docs/README.md [0,9) name=Service qn=docs/README.md#Service
search section docs/README.md [133,141) name=Usage qn=docs/README.md#Service > Usage
search section docs/README.md [150,158) name=Usage qn=docs/README.md#Service > Usage~2
`

// TestDependencyBoundUnlimitedByDefaultAndReportedWhenSet is the one test of
// the manifest list bounds. Failure modes it protects: a default build that
// cuts a large manifest's dependency list (the hard 4096/1024 caps this
// replaced did exactly that, so a monorepo's go.mod lost modules), and a
// user-set bound that cuts in silence — the capability must carry the count
// that crossed it, or the answer claims coverage it does not have.
func TestDependencyBoundUnlimitedByDefaultAndReportedWhenSet(t *testing.T) {
	const requires = 5000
	var b strings.Builder
	b.WriteString("module example.com/big\n\ngo 1.22\n\nrequire (\n")
	for i := range requires {
		fmt.Fprintf(&b, "\texample.com/dep%d v1.0.0\n", i)
	}
	b.WriteString(")\n")
	files := map[string]string{"go.mod": b.String()}

	count := func(t *testing.T, opts manifest.Options) (int, model.CapabilityState) {
		t.Helper()
		mf, err := manifest.New(opts)
		if err != nil {
			t.Fatal(err)
		}
		h := providertest.New(t, files)
		cap := &capture{}
		states := map[string]model.CapabilityState{}
		runUnit(t, h, mf, "go.mod", cap, states)
		n := 0
		for _, r := range cap.relations {
			if r.Relation.Kind == model.RelDependsOn {
				n++
			}
		}
		return n, states[manifest.ID+" "+manifest.CapabilityManifests+" "+filesystem.ScopeKey("go.mod")]
	}

	n, cs := count(t, manifest.Options{MaxParseFileBytes: 1 << 20})
	if n != requires {
		t.Fatalf("the default configuration published %d of %d requires; every count bound defaults to unlimited", n, requires)
	}
	if cs.State != model.CapabilityFresh {
		t.Fatalf("the default configuration reported %s/%s; nothing was cut", cs.State, cs.DiagnosticCode)
	}

	n, cs = count(t, manifest.Options{MaxParseFileBytes: 1 << 20, MaxDependencies: 100})
	if n != 100 {
		t.Fatalf("a bound of 100 published %d requires, want 100", n)
	}
	if cs.State != model.CapabilityPartial || cs.DiagnosticCode != model.CodeResourceLimit {
		t.Fatalf("a cut list reported %s/%s, want partial/%s", cs.State, cs.DiagnosticCode, model.CodeResourceLimit)
	}
	if got := cs.Details[manifest.BoundDependencies]; got != "5000 over 100" {
		t.Fatalf("capability detail %q = %q, want the count that crossed the bound", manifest.BoundDependencies, got)
	}
}

// TestNoDefaultBoundDegradesARealManifest is the VF5 regression: two shapes a
// real repository holds that the provider degraded on a default
// configuration. A long CHANGELOG crossed the 1024-entry cap and published
// `partial`/CTX_RESOURCE_LIMIT with no user-set bound anywhere; a
// tool-configuration-only pyproject.toml — no [project], no [tool.poetry],
// which is a valid and common file — published `failed`/CTX_ARGUMENT_INVALID,
// saying a file that parsed cleanly did not parse as its format.
func TestNoDefaultBoundDegradesARealManifest(t *testing.T) {
	var md strings.Builder
	for i := range 1500 {
		fmt.Fprintf(&md, "## Release %d\n\nSee [entry %d](./notes/%d.md).\n\n", i, i, i)
	}
	files := map[string]string{
		"CHANGELOG.md": md.String(),
		"QA/pyproject.toml": "[tool.ruff]\nline-length = 100\n\n[tool.ruff.lint]\nselect = [\"E\", \"W\"]\n\n" +
			"[tool.robocop]\nreports = [\"all\"]\n",
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	h := providertest.New(t, files)
	states := map[string]model.CapabilityState{}
	for _, path := range []string{"CHANGELOG.md", "QA/pyproject.toml"} {
		runUnit(t, h, mf, path, &capture{}, states)
	}
	for _, want := range []struct{ capability, path string }{
		{manifest.CapabilityDocumentation, "CHANGELOG.md"},
		{manifest.CapabilityManifests, "QA/pyproject.toml"},
	} {
		cs := states[manifest.ID+" "+want.capability+" "+filesystem.ScopeKey(want.path)]
		if cs.State != model.CapabilityFresh || cs.DiagnosticCode != "" {
			t.Fatalf("%s published %s/%s for %s on a default configuration, want fresh: no default bound may degrade a capability",
				want.capability, cs.State, cs.DiagnosticCode, want.path)
		}
	}
}

// TestParseBoundsAreUnlimitedByDefaultAndDegradeOnlyPastTheBound covers the
// two parse bounds — the TOML line layout and the POM token walk. Both were
// hard-coded 200000 constants that cut a repository's own data with nothing
// the operator could set, and the TOML one discarded the layout WHOLE past
// its cap, so one long manifest cost every fact of that file its byte range,
// not only the facts past the bound. Those are the two failures asserted
// here: a default build cuts neither, and a set bound leaves the facts before
// it fully ranged while reporting the count that crossed it.
func TestParseBoundsAreUnlimitedByDefaultAndDegradeOnlyPastTheBound(t *testing.T) {
	var cargo strings.Builder
	cargo.WriteString("[package]\nname = \"wide\"\nversion = \"0.1.0\"\n\n[dependencies]\n")
	for i := range 4000 {
		fmt.Fprintf(&cargo, "dep%d = \"1.0.0\"\n", i)
	}
	var pom strings.Builder
	pom.WriteString("<project><groupId>org.acme</groupId><artifactId>wide</artifactId><version>1</version><modules>\n")
	for i := range 4000 {
		fmt.Fprintf(&pom, "<module>m%d</module>\n", i)
	}
	pom.WriteString("</modules></project>\n")
	files := map[string]string{"Cargo.toml": cargo.String(), "pom.xml": pom.String()}

	run := func(t *testing.T, opts manifest.Options, path string) (*capture, model.CapabilityState) {
		t.Helper()
		mf, err := manifest.New(opts)
		if err != nil {
			t.Fatal(err)
		}
		h := providertest.New(t, files)
		cap := &capture{}
		states := map[string]model.CapabilityState{}
		runUnit(t, h, mf, path, cap, states)
		return cap, states[manifest.ID+" "+manifest.CapabilityManifests+" "+filesystem.ScopeKey(path)]
	}
	// ranged counts the dependency evidence occurrences that carry a byte
	// range. The layout is the only thing that gives them one, so it is the
	// discriminator between "the layout was built" and "it was discarded".
	ranged := func(c *capture) (withRange, total int) {
		for _, r := range c.relations {
			if r.Relation.Kind != model.RelDependsOn {
				continue
			}
			for _, ev := range r.Evidence {
				total++
				if ev.Range != nil {
					withRange++
				}
			}
		}
		return withRange, total
	}

	unlimited := manifest.Options{MaxParseFileBytes: 1 << 24}
	c, cs := run(t, unlimited, "Cargo.toml")
	if cs.State != model.CapabilityFresh {
		t.Fatalf("the default configuration reported %s/%s for a 4000-line Cargo.toml; no parse bound has a default", cs.State, cs.DiagnosticCode)
	}
	withRange, total := ranged(c)
	if total == 0 || withRange != total {
		t.Fatalf("the default configuration ranged %d of %d dependency occurrences, want all of them", withRange, total)
	}

	// A bound that stops the scan a few lines into the dependency table: the
	// dependencies before it keep their ranges, the ones past it do not, and
	// the capability says how many lines the file really had.
	c, cs = run(t, manifest.Options{MaxParseFileBytes: 1 << 24, MaxTOMLLines: 10}, "Cargo.toml")
	if cs.State != model.CapabilityPartial || cs.DiagnosticCode != model.CodeResourceLimit {
		t.Fatalf("a cut layout reported %s/%s, want partial/%s", cs.State, cs.DiagnosticCode, model.CodeResourceLimit)
	}
	if got := cs.Details[manifest.BoundTOMLLines]; got != "4006 over 10" {
		t.Fatalf("capability detail %q = %q, want the file's line count over the bound", manifest.BoundTOMLLines, got)
	}
	withRange, total = ranged(c)
	if withRange == 0 {
		t.Fatalf("a cut layout ranged 0 of %d dependency occurrences: the bound discarded the whole layout instead of degrading only the facts past it", total)
	}
	if withRange == total {
		t.Fatalf("a bound of 10 lines ranged all %d occurrences; this leg is not exercising the cut", total)
	}

	c, cs = run(t, unlimited, "pom.xml")
	if cs.State != model.CapabilityFresh {
		t.Fatalf("the default configuration reported %s/%s for a 4000-module pom.xml; no parse bound has a default", cs.State, cs.DiagnosticCode)
	}
	full := len(c.relations)
	if full == 0 {
		t.Fatal("the default configuration published no relations for pom.xml")
	}
	c, cs = run(t, manifest.Options{MaxParseFileBytes: 1 << 24, MaxXMLElements: 100}, "pom.xml")
	if cs.State != model.CapabilityPartial || cs.DiagnosticCode != model.CodeResourceLimit {
		t.Fatalf("a cut token stream reported %s/%s, want partial/%s: a large POM is large, not malformed", cs.State, cs.DiagnosticCode, model.CodeResourceLimit)
	}
	if got := cs.Details[manifest.BoundXMLElements]; got != "101 over 100" {
		t.Fatalf("capability detail %q = %q, want the element count that crossed the bound", manifest.BoundXMLElements, got)
	}
	if got := len(c.relations); got >= full {
		t.Fatalf("a 100-element bound published %d relations against %d unbounded; the walk did not stop", got, full)
	}
}
