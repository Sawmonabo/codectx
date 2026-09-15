// Package manifest is the Section 11.2 build-metadata and documentation
// provider: package, module, dependency and configuration nodes parsed from
// go.mod, go.work, package.json, Cargo.toml, pyproject.toml and pom.xml, and
// the headings and source links of Markdown documents. Every parser is a
// static reader of the pinned bytes: nothing is fetched, evaluated, executed
// or resolved against another file, and a value the format leaves dynamic
// (a workspace-inherited version, a Maven property reference, a PEP 621
// dynamic field) stays unresolved and says so.
//
// One unit is one manifest file (R7-1) and depends on the filesystem unit of
// the same file, whose file node the manifest's `defines` relation starts
// from. A malformed file is reported as a failed capability at that file's
// scope; the unit still seals, so one broken manifest never fails the
// provider run.
package manifest

import (
	"context"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/source"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Provider identity and capabilities.
const (
	ID      = "manifest"
	Version = "2"

	// CapabilityManifests covers the parsed package and build formats.
	CapabilityManifests = "manifests"
	// CapabilityDocumentation covers Markdown headings and links.
	CapabilityDocumentation = "documentation"
)

// Dependency kinds retained on `depends_on` evidence. The relation itself is
// one canonical edge; each kind a manifest lists the dependency under is a
// distinct evidence occurrence, so deduplication never loses a kind.
const (
	KindRuntime  = "runtime"
	KindDev      = "dev"
	KindPeer     = "peer"
	KindOptional = "optional"
	KindBuild    = "build"
	KindTest     = "test"
)

// Bound names. A manifest over one of them is reported partial with
// CTX_RESOURCE_LIMIT and the capability carries the count that crossed it.
// All four are unlimited by default: how much a manifest declares, and how
// many lines or elements it takes to declare it, are properties of the
// repository, not something the product cuts on the user's behalf.
const (
	BoundDependencies = "max_dependencies"
	BoundEntries      = "max_entries" // modules, members, headings, links, properties
	// BoundXMLElements and BoundTOMLLines bound the parse itself rather than
	// one of the manifest's lists: the POM token stream and the TOML line
	// layout. Both are user-set keys, unlimited by default like the two
	// above, and both cut rather than refuse -- what parsed before the bound
	// is published, and the count that crossed it is reported.
	BoundXMLElements = "max_xml_elements"
	BoundTOMLLines   = "max_toml_lines"
)

// Options are the admission settings this provider honours.
// MaxParseFileBytes (workspace.max_parse_file_bytes) is part of the analysis
// configuration hash; the four providers.manifest.* bounds below are NOT yet
// folded into config.AnalysisConfigHash, so raising one does not by itself
// invalidate units indexed under a lower one and their cut facts stay cut.
// Closing that is four quoteLimit lines in config/fingerprint.go.
type Options struct {
	// MaxParseFileBytes is the largest manifest or document parsed. A
	// larger file stays retained and searchable; its capability here is
	// unavailable.
	MaxParseFileBytes config.Limit
	// MaxDependencies is providers.manifest.max_dependencies: how many
	// dependencies the user wants one manifest to declare. Unlimited by
	// default; a user-set value that is crossed cuts the list and is
	// reported on the unit's capability with the count.
	MaxDependencies config.Limit
	// MaxEntries is providers.manifest.max_entries, the same contract for a
	// manifest's modules, members, headings, links and properties.
	MaxEntries config.Limit
	// MaxTOMLLines is providers.manifest.max_toml_lines: how many lines of a
	// TOML manifest the evidence-range scan places. Unlimited by default; a
	// user-set value that is crossed leaves only the facts past that line
	// without a range, and is reported with the line count.
	MaxTOMLLines config.Limit
	// MaxXMLElements is providers.manifest.max_xml_elements: how many
	// elements of a POM the token walk reads. Unlimited by default; a
	// user-set value that is crossed publishes what parsed before it and is
	// reported with the element count.
	MaxXMLElements config.Limit
}

// Provider is the manifest provider.
type Provider struct {
	opts Options
}

// New validates the options and returns the provider.
func New(opts Options) (*Provider, error) {
	return &Provider{opts: opts}, nil
}

// Descriptor declares the dependency on filesystem: the file node a manifest
// defines its package from is resolved through that unit's aliases.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: ID, Version: Version, Capabilities: []string{CapabilityManifests, CapabilityDocumentation},
		DependsOn: []string{filesystem.ID}, InvalidationScope: model.InvalidationFile, Required: true}
}

// rootManifests are the well-known manifests detection reports when they sit
// at the workspace root. Detection is informational: the provider is
// available regardless, because documents are everywhere.
var rootManifests = []string{"go.mod", "go.work", "package.json", "Cargo.toml", "pyproject.toml", "pom.xml"}

// Detect reports the root manifests present without walking anything.
func (p *Provider) Detect(_ context.Context, root workspace.Root, _ workspace.Policy) (provider.Detection, error) {
	det := provider.Detection{Available: true, Capabilities: []string{CapabilityManifests, CapabilityDocumentation}}
	for _, name := range rootManifests {
		if info, err := root.Lstat(name); err == nil && info.Mode().IsRegular() {
			det.InputPaths = append(det.InputPaths, name)
		}
	}
	return det, nil
}

// Recognize reports whether the path is one this provider parses, which is
// how the coordinator decides that a manifest unit exists for a file.
func Recognize(rel string) bool {
	return capabilityFor(filesystem.Classify(rel).Format) != ""
}

// capabilityFor maps a parsed format to the capability it reports under.
func capabilityFor(format string) string {
	switch format {
	case filesystem.FormatGoMod, filesystem.FormatGoWork, filesystem.FormatPackageJSON, filesystem.FormatCargo, filesystem.FormatPyProject, filesystem.FormatPom:
		return CapabilityManifests
	case filesystem.FormatMarkdown, "adr":
		return CapabilityDocumentation
	}
	return ""
}

// IndexUnit parses the unit's file and emits its facts.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	rel, err := filesystem.PathFromScope(req.Unit.ScopeKey)
	if err != nil {
		return model.ProviderResult{}, err
	}
	cls := filesystem.Classify(rel)
	capability := capabilityFor(cls.Format)
	if capability == "" {
		return model.ProviderResult{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the manifest provider was assigned a file it does not parse", Details: map[string]string{"path": rel}}
	}
	rc, fv, err := req.Content.Open(ctx, model.NewFileID(req.Binding.RepositoryID, rel))
	if err != nil {
		return model.ProviderResult{}, err
	}
	defer rc.Close()
	e := filesystem.NewEmitter(req, sink, fv)
	if p.opts.MaxParseFileBytes.Exceeded(fv.Size) {
		e.Capability(capability, model.CapabilityUnavailable, model.CodeResourceLimit)
		return e.Result(), nil
	}
	data, err := io.ReadAll(io.LimitReader(rc, fv.Size+1))
	if err != nil {
		return model.ProviderResult{}, err
	}
	if int64(len(data)) != fv.Size {
		return model.ProviderResult{}, &model.Error{Code: model.CodeSourceIntegrity, Message: "retained bytes differ in length from the manifest"}
	}
	e.AddBytes(uint64(len(data)))
	u := &unit{e: e, data: data, cursor: source.NewCursor(data), capability: capability, state: model.CapabilityFresh,
		deps: p.opts.MaxDependencies, entries: p.opts.MaxEntries,
		tomlLines: p.opts.MaxTOMLLines, xmlElements: p.opts.MaxXMLElements}
	switch cls.Format {
	case filesystem.FormatGoMod:
		err = u.goMod(ctx)
	case filesystem.FormatGoWork:
		err = u.goWork(ctx)
	case filesystem.FormatPackageJSON:
		err = u.packageJSON(ctx)
	case filesystem.FormatCargo:
		err = u.cargo(ctx)
	case filesystem.FormatPyProject:
		err = u.pyproject(ctx)
	case filesystem.FormatPom:
		err = u.pom(ctx)
	default:
		err = u.markdown(ctx)
	}
	if err != nil {
		return model.ProviderResult{}, err
	}
	e.Capability(capability, u.state, u.code)
	for bound, detail := range u.over {
		e.CapabilityDetail(capability, bound, detail)
	}
	if err := e.Flush(ctx); err != nil {
		return model.ProviderResult{}, err
	}
	return e.Result(), nil
}

// unit is the parsing state of one manifest file.
type unit struct {
	e      *filesystem.Emitter
	data   []byte
	cursor *source.Cursor

	// file is the file node `defines` starts from, resolved on first use.
	// A format that defines nothing from the file (Markdown emits its
	// document node directly) never resolves it, so no unit pays for a
	// candidate it does not name.
	file     model.Node
	fileSeen bool

	capability string
	state      model.CapabilityStateValue
	code       string

	// deps and entries are the unit's configured list bounds, tomlLines and
	// xmlElements its configured parse bounds, and over holds one capability
	// detail per bound this unit cut, keyed by bound name.
	deps, entries          config.Limit
	tomlLines, xmlElements config.Limit
	over                   map[string]string
	// seen is the largest count recorded against each bound. Several
	// independent lists share one bound name, so without it the last list to
	// cross would overwrite a larger crossing and the unit would under-report
	// what it cut.
	seen map[string]int64
}

// fileNode resolves this manifest's own file node. The node is the
// filesystem unit's; resolving the same candidate hits the alias that unit
// published, so `defines` starts from the identity already in the index.
func (u *unit) fileNode(ctx context.Context) (model.Node, error) {
	if u.fileSeen {
		return u.file, nil
	}
	node, err := u.e.Node(ctx, filesystem.PathCandidate(ID, model.NodeFile, u.e.File().Path), filesystem.Attrs{Precision: model.PrecisionSyntax, Located: true})
	if err != nil {
		return model.Node{}, err
	}
	u.file, u.fileSeen = node, true
	return node, nil
}

// malformed records that the file did not parse as its format. It is a
// capability outcome, never a provider failure.
func (u *unit) malformed() { u.state, u.code = model.CapabilityFailed, model.CodeArgumentInvalid }

// overBound records that a user-set list bound was crossed: the unit is
// partial with CTX_RESOURCE_LIMIT and seen -- the count that crossed the
// bound -- is kept so the capability can report it. Nothing is cut when the
// bound is unlimited, so this is never reached for a default configuration.
func (u *unit) overBound(bound string, limit config.Limit, seen int64) {
	if prev, recorded := u.seen[bound]; recorded && seen <= prev {
		u.degraded(bound, strconv.FormatInt(prev, 10)+" over "+limit.String())
		return
	}
	if u.seen == nil {
		u.seen = map[string]int64{}
	}
	u.seen[bound] = seen
	u.degraded(bound, strconv.FormatInt(seen, 10)+" over "+limit.String())
}

// degraded records that a parser bound cut this manifest. Unlike the list
// bounds these are structural ceilings with no count to report -- a token
// stream stopped, a layout not built -- so the detail names what was cut.
func (u *unit) degraded(bound, detail string) {
	if u.state == model.CapabilityFresh {
		u.state, u.code = model.CapabilityPartial, model.CodeResourceLimit
	}
	if u.over == nil {
		u.over = map[string]string{}
	}
	u.over[bound] = detail
}

// cut reports whether n crosses the bound, recording the crossing when it
// does. It is the one place a manifest list bound is enforced.
func (u *unit) cut(bound string, limit config.Limit, n int64) bool {
	if !limit.Exceeded(n) {
		return false
	}
	u.overBound(bound, limit, n)
	return true
}

// rng converts a byte interval of the file to a source range, or nil when the
// interval is not a valid one (a parser position off a rune boundary would
// otherwise attribute a fact to bytes it does not describe).
func (u *unit) rng(start, end int) *model.SourceRange {
	if start < 0 || end < start || end > len(u.data) {
		return nil
	}
	s, err := u.cursor.PositionAt(uint64(start))
	if err != nil {
		return nil
	}
	e, err := u.cursor.PositionAt(uint64(end))
	if err != nil {
		return nil
	}
	return &model.SourceRange{Start: s, End: e}
}

// defines emits the node the manifest file defines (a package, module or
// configuration) and the `defines` edge from the file. Identity is the
// qualified name within this file: two manifests declaring the same name are
// two packages.
func (u *unit) defines(ctx context.Context, kind model.NodeKind, qualified, name, language string, rng *model.SourceRange, meta map[string]any) (model.Node, error) {
	fv := u.e.File()
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: provider.ScopeWorkspace, NativeKey: string(kind) + ":" + qualified,
		Kind: kind, Language: language, Name: name, QualifiedName: qualified, FileID: fv.ID, ContentHash: fv.ContentHash}
	file, err := u.fileNode(ctx)
	if err != nil {
		return model.Node{}, err
	}
	node, err := u.e.Node(ctx, cand, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng, Located: true, Metadata: filesystem.Metadata(meta)})
	if err != nil {
		return model.Node{}, err
	}
	if err := u.e.Relation(file.ID, model.RelDefines, node.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng}); err != nil {
		return model.Node{}, err
	}
	u.e.Symbol(node, rng)
	return node, nil
}

// dependency is a workspace-scoped structural identity: every manifest that
// names the same ecosystem package converges on one node.
func dependencyCandidate(ecosystem, name, language string) model.NodeCandidate {
	qualified := ecosystem + ":" + name
	return model.NodeCandidate{ProviderID: ID, ScopeKey: provider.ScopeWorkspace, NativeKey: string(model.NodeDependency) + ":" + qualified,
		Kind: model.NodeDependency, Language: language, Name: name, QualifiedName: qualified}
}

// edge emits one occurrence of `from --kind--> dependency(ecosystem:name)`
// with the dependency kind and requirement on the evidence. extra are
// further detail pairs (a replacement target, an unresolved marker).
func (u *unit) edge(ctx context.Context, from model.Node, rel model.RelationKind, ecosystem, name, language, kind, requirement string, rng *model.SourceRange, extra ...string) error {
	if name == "" || len(name) > model.MaxNameBytes {
		u.malformedEntry()
		return nil
	}
	dep, err := u.e.Node(ctx, dependencyCandidate(ecosystem, name, language), filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng})
	if err != nil {
		return err
	}
	u.e.Symbol(dep, rng)
	detail := append([]string{"kind", kind, "requirement", requirement}, extra...)
	return u.e.Relation(from.ID, rel, dep.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng, Detail: filesystem.Detail(detail...)})
}

// malformedEntry downgrades to partial: one entry was unusable while the
// rest of the file parsed.
func (u *unit) malformedEntry() {
	if u.state == model.CapabilityFresh {
		u.state, u.code = model.CapabilityPartial, model.CodeArgumentInvalid
	}
}

// pathTarget resolves a path a manifest names relative to its own directory
// to the repository, directory or file node it denotes. ok is false for a
// path that leaves the workspace.
func (u *unit) pathTarget(ctx context.Context, target string, dir bool, rng *model.SourceRange, precision model.Precision) (model.Node, bool, error) {
	rel, ok := resolveRelative(path.Dir(u.e.File().Path), target)
	if !ok {
		return model.Node{}, false, nil
	}
	kind := model.NodeFile
	switch {
	case rel == ".":
		kind = model.NodeRepository
	case dir:
		kind = model.NodeDirectory
	}
	attrs := filesystem.Attrs{Precision: precision, Range: rng}
	if kind == model.NodeFile {
		// This unit did not observe the file; it only names it.
		attrs.Metadata = filesystem.Metadata(map[string]any{"resolution": "referenced"})
	}
	node, err := u.e.Node(ctx, filesystem.PathCandidate(ID, kind, rel), attrs)
	return node, err == nil, err
}

// resolveRelative joins a manifest-relative path onto the manifest's
// directory and normalizes it. An absolute path is taken as root-relative;
// a result above the root is rejected.
func resolveRelative(dir, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	var rel string
	if strings.HasPrefix(target, "/") {
		rel = path.Clean(strings.TrimLeft(target, "/"))
	} else {
		rel = path.Clean(path.Join(dir, target))
	}
	if rel == ".." || strings.HasPrefix(rel, "../") || rel == "" {
		return "", false
	}
	return rel, true
}

// asString is the string value of a decoded scalar, or "" for anything else.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
