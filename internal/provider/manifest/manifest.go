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
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/source"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Provider identity and capabilities.
const (
	ID      = "manifest"
	Version = "1"

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

// Bounds on what one manifest may declare. A manifest over a bound is
// reported partial with CTX_RESOURCE_LIMIT rather than indexed without limit.
const (
	MaxDependencies = 4096
	MaxEntries      = 1024 // modules, members, headings, links, properties
)

// Options are the admission settings this provider honours
// (workspace.max_parse_file_bytes); they are part of the analysis
// configuration hash.
type Options struct {
	// MaxParseFileBytes is the largest manifest or document parsed. A
	// larger file stays retained and searchable; its capability here is
	// unavailable.
	MaxParseFileBytes int64
}

// Provider is the manifest provider.
type Provider struct {
	opts Options
}

// New validates the options and returns the provider.
func New(opts Options) (*Provider, error) {
	if opts.MaxParseFileBytes <= 0 {
		return nil, &model.Error{Code: model.CodeArgumentInvalid, Message: "manifest provider needs a positive max_parse_file_bytes; zero would mean unlimited"}
	}
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
	if fv.Size > p.opts.MaxParseFileBytes {
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
	u := &unit{e: e, data: data, cursor: source.NewCursor(data), capability: capability, state: model.CapabilityFresh}
	// The file node is the filesystem unit's; resolving the same candidate
	// hits its alias, so `defines` starts from the identity it published.
	if u.file, err = e.Node(ctx, filesystem.PathCandidate(ID, model.NodeFile, rel), filesystem.Attrs{Precision: model.PrecisionSyntax, Located: true}); err != nil {
		return model.ProviderResult{}, err
	}
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
	file   model.Node

	capability string
	state      model.CapabilityStateValue
	code       string
}

// malformed records that the file did not parse as its format. It is a
// capability outcome, never a provider failure.
func (u *unit) malformed() { u.state, u.code = model.CapabilityFailed, model.CodeArgumentInvalid }

// overBound records that a bounded list was cut.
func (u *unit) overBound() {
	if u.state == model.CapabilityFresh {
		u.state, u.code = model.CapabilityPartial, model.CodeResourceLimit
	}
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
	node, err := u.e.Node(ctx, cand, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng, Located: true, Metadata: filesystem.Metadata(meta)})
	if err != nil {
		return model.Node{}, err
	}
	if err := u.e.Relation(u.file.ID, model.RelDefines, node.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng}); err != nil {
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
