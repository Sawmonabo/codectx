// Package scip is the Section 11.4 provider: streaming import of SCIP
// precise indexes and execution of approved, already-installed SCIP indexers
// (scip-go, scip-typescript, scip-java) against a private materialization of
// the pinned snapshot.
//
// Two inputs feed one provider. A supplied `.scip` file inside the snapshot
// is imported as it is; an approved profile is run through the shared
// process runner and its output imported the same way. Both go through the
// same bounded wire decoder: top-level and nested fields are walked with a
// reader that never allocates past a record bound, a document of any size is
// streamed field by field, and forward and external references are resolved
// through an on-disk symbol map rather than an in-memory symbol table.
//
// Facts are exact-source compiler evidence only when the index provably
// describes the pinned bytes (codectx invoked the indexer, or every document
// carries matching embedded text or a matching input-hash manifest entry).
// Anything else is imported with source_binding=unverified: still available
// for discovery, never silently upgraded (Section 11.4). See
// docs/providers-scip.md.
package scip

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// ID and Version identify the provider. Version is part of every unit key:
// bump it when the mapping of SCIP records to facts changes.
const (
	ID      = "scip"
	Version = "1"
)

// Capabilities this provider offers. Definitions covers symbol nodes and
// aliases; references covers reference, import, read and write occurrences;
// implementations covers SCIP relationships.
const (
	CapabilityDefinitions     = "precise_definitions"
	CapabilityReferences      = "precise_references"
	CapabilityImplementations = "precise_implementations"
)

var capabilities = []string{CapabilityDefinitions, CapabilityReferences, CapabilityImplementations}

// Provider IDs this provider depends on (Section 11.1 table). They are
// declared by ID only; the registry verifies they are registered.
const (
	dependsFilesystem = "filesystem"
	dependsTreeSitter = "treesitter"
	dependsManifest   = "manifest"
)

// Scope key prefixes. One unit is built per source: the supplied index or one
// approved profile. The coordinator plans the unit for each key Scopes
// returns and declares every eligible snapshot file of the workspace as its
// inputs, so a fact may name any file the index describes.
const (
	scopeImport  = "import:"
	scopeProfile = "profile:"
)

// ImportScope is the unit scope key of a supplied index at a root-relative
// path.
func ImportScope(path string) string { return scopeImport + path }

// ProfileScope is the unit scope key of an approved indexer profile.
func ProfileScope(name string) string { return scopeProfile + name }

// Limits bound one import. Every field is required to be positive; nothing
// here means unlimited (Section 6).
type Limits struct {
	// MaxIndexBytes bounds the whole index file.
	MaxIndexBytes int64
	// MaxRecordBytes bounds one metadata, occurrence or symbol record. It is
	// resources.max_provider_record_bytes: the bound a single fact must fit.
	MaxRecordBytes int64
	// MaxDocuments and MaxOccurrencesPerDocument bound the walk.
	MaxDocuments              int64
	MaxOccurrencesPerDocument int64
	// MaxSpoolBytes bounds the bytes spooled to the scratch database.
	MaxSpoolBytes int64
	// MaxSourceFileBytes bounds one document's source, which is held whole
	// while its positions are converted, as the parse limit does for
	// tree-sitter.
	MaxSourceFileBytes int64
	// MaxMaterializeBytes bounds the private materialization a profile runs
	// against.
	MaxMaterializeBytes int64
	// MaxManifestBytes bounds a supplied input-hash manifest.
	MaxManifestBytes int64
}

// DefaultLimits are the Section 20.1 defaults this provider derives from.
func DefaultLimits() Limits {
	return Limits{
		MaxIndexBytes:             1 << 30,
		MaxRecordBytes:            4 << 20,
		MaxDocuments:              1_000_000,
		MaxOccurrencesPerDocument: 4_000_000,
		MaxSpoolBytes:             4 << 30,
		MaxSourceFileBytes:        5 << 20,
		MaxMaterializeBytes:       4 << 30,
		MaxManifestBytes:          64 << 20,
	}
}

// Validate rejects a non-positive bound.
func (l Limits) Validate() error {
	for _, b := range []struct {
		name string
		v    int64
	}{{"max_index_bytes", l.MaxIndexBytes}, {"max_record_bytes", l.MaxRecordBytes}, {"max_documents", l.MaxDocuments},
		{"max_occurrences_per_document", l.MaxOccurrencesPerDocument}, {"max_spool_bytes", l.MaxSpoolBytes},
		{"max_source_file_bytes", l.MaxSourceFileBytes}, {"max_materialize_bytes", l.MaxMaterializeBytes}, {"max_manifest_bytes", l.MaxManifestBytes}} {
		if b.v <= 0 {
			return invalid(fmt.Sprintf("scip limit %s is %d; every bound must be positive", b.name, b.v))
		}
	}
	return nil
}

// Options compose the provider.
type Options struct {
	// Import is the root-relative path of a supplied index inside the
	// snapshot (the explicit import request field), or empty.
	Import string
	// Manifest is the root-relative path of an optional input-hash manifest
	// (`<sha256>  <path>` lines) supplied with the index.
	Manifest string
	// Analyzers are the approved profiles of the user configuration; only
	// those named after a known SCIP indexer kind are used.
	Analyzers []config.Analyzer
	// Runner is the shared process runner; required when a profile is
	// configured.
	Runner *process.Runner
	// Timeout caps every profile run (providers.scip.timeout).
	Timeout time.Duration
	// WorkDir is the absolute private directory for import scratch state.
	WorkDir string
	// Limits bound the import; zero selects DefaultLimits.
	Limits Limits
	// LookupEnv supplies allowlisted environment values; nil means
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Provider is the SCIP provider.
type Provider struct {
	importPath, manifestPath string
	profiles                 []Profile
	runner                   *process.Runner
	timeout                  time.Duration
	workDir                  string
	limits                   Limits
	lookupEnv                func(string) (string, bool)
}

var _ provider.Provider = (*Provider)(nil)

// New validates the options and returns the provider.
func New(o Options) (*Provider, error) {
	if o.Limits == (Limits{}) {
		o.Limits = DefaultLimits()
	}
	if err := o.Limits.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(o.WorkDir) {
		return nil, invalid("scip work directory must be an absolute private path")
	}
	for _, p := range []string{o.Import, o.Manifest} {
		if p != "" && (strings.HasPrefix(p, "/") || filepath.IsAbs(p) || strings.Contains(p, "..") || len(p) > model.MaxPathBytes) {
			return nil, invalid("scip import paths must be normalized root-relative paths")
		}
	}
	if o.Manifest != "" && o.Import == "" {
		return nil, invalid("a scip input-hash manifest needs an index to describe")
	}
	profs := profiles(o.Analyzers)
	if len(profs) > 0 && o.Runner == nil {
		return nil, invalid("scip profiles require the shared process runner")
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	return &Provider{importPath: o.Import, manifestPath: o.Manifest, profiles: profs, runner: o.Runner, timeout: o.Timeout,
		workDir: o.WorkDir, limits: o.Limits, lookupEnv: o.LookupEnv}, nil
}

// Descriptor declares the provider: optional, workspace-invalidated, on top
// of the filesystem and tree-sitter providers, and on the manifest provider
// when a profile is configured (a profile reads the package manifests).
func (p *Provider) Descriptor() model.ProviderDescriptor {
	deps := []string{dependsFilesystem, dependsTreeSitter}
	if len(p.profiles) > 0 {
		deps = append(deps, dependsManifest)
	}
	return model.ProviderDescriptor{ID: ID, Version: Version, Capabilities: capabilities, DependsOn: deps,
		InvalidationScope: model.InvalidationWorkspace, Required: false}
}

// Scopes lists the unit scope keys this provider builds: the supplied index
// and each approved profile whose trigger detection recognized.
func (p *Provider) Scopes(det provider.Detection) []string {
	var out []string
	recognized := map[string]bool{}
	for _, in := range det.InputPaths {
		recognized[in] = true
	}
	if p.importPath != "" && recognized[p.importPath] {
		out = append(out, ImportScope(p.importPath))
	}
	for _, prof := range p.profiles {
		for _, trig := range prof.kind.triggers {
			if recognized[trig] {
				out = append(out, ProfileScope(prof.Name))
				break
			}
		}
	}
	return out
}

// Detect inspects declared inputs only: the supplied index file and the
// manifests that trigger an approved profile, through the confined root. It
// never runs a tool. An absent index and no installed approved indexer is
// honest absence (CTX_PROVIDER_UNAVAILABLE).
func (p *Provider) Detect(_ context.Context, root workspace.Root, _ workspace.Policy) (provider.Detection, error) {
	det := provider.Detection{Capabilities: capabilities}
	if p.importPath != "" {
		if info, err := root.Lstat(p.importPath); err == nil && info.Mode().IsRegular() {
			det.Available = true
			det.InputPaths = append(det.InputPaths, p.importPath)
		}
	}
	for _, prof := range p.profiles {
		triggered := false
		for _, trig := range prof.kind.triggers {
			if info, err := root.Lstat(trig); err == nil && info.Mode().IsRegular() {
				triggered = true
				det.InputPaths = append(det.InputPaths, trig)
			}
		}
		if triggered && executableInstalled(prof.Executable) {
			det.Available = true
		}
	}
	if !det.Available {
		det.DiagnosticCode = model.CodeProviderUnavailable
	}
	if len(det.InputPaths) > provider.MaxDetectionInputs {
		det.InputPaths = det.InputPaths[:provider.MaxDetectionInputs]
	}
	return det, nil
}

// Verify decides the source binding the coordinator opens the unit with
// (model.UnitBuild.SourceBinding) before any fact exists. IndexUnit repeats
// the same check and reports the outcome in its capability states, so a unit
// opened as verified whose index turns out unverified is visibly partial
// with CTX_SOURCE_BINDING_UNVERIFIED rather than silently exact.
func (p *Provider) Verify(ctx context.Context, view model.SnapshotView, scopeKey string) (model.SourceBinding, error) {
	if strings.HasPrefix(scopeKey, scopeProfile) {
		if _, err := p.profileFor(scopeKey); err != nil {
			return "", err
		}
		return model.SourceBindingVerified, nil
	}
	sc, err := openScratch(ctx, p.workDir, p.limits.MaxSpoolBytes)
	if err != nil {
		return "", err
	}
	defer sc.close()
	im := &importer{p: p, req: provider.UnitRequest{Content: view}, sc: sc, ctx: ctx}
	open, err := p.importOpener(ctx, view, scopeKey)
	if err != nil {
		return "", err
	}
	if p.manifestPath != "" {
		if err := im.loadManifest(ctx); err != nil {
			return "", err
		}
	}
	binding, _, err := im.scanBinding(ctx, open)
	return binding, err
}

// IndexUnit builds one unit: the supplied index or one profile run.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	if req.Content == nil || req.Resolver == nil || sink == nil {
		return model.ProviderResult{}, invalid("scip unit request needs a snapshot view, a resolver and a sink")
	}
	im := &importer{p: p, req: req, sink: sink, ctx: ctx}
	defer im.close()
	var open opener
	var err error
	workDir := p.workDir
	if strings.HasPrefix(req.Unit.ScopeKey, scopeProfile) {
		prof, err := p.profileFor(req.Unit.ScopeKey)
		if err != nil {
			return model.ProviderResult{}, err
		}
		im.profile = &prof
		runDir, err := os.MkdirTemp(prof.WorkDir, "scip-run-")
		if err != nil {
			return model.ProviderResult{}, internal("scip run directory: " + err.Error())
		}
		defer os.RemoveAll(runDir)
		workDir = runDir
		output, err := p.runProfile(ctx, prof, req.Content, runDir)
		if err != nil {
			return model.ProviderResult{}, err
		}
		open = func(context.Context) (io.ReadCloser, int64, error) {
			f, err := os.Open(output)
			if err != nil {
				return nil, 0, internal("scip output: " + err.Error())
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, 0, internal("scip output: " + err.Error())
			}
			return f, info.Size(), nil
		}
	} else if open, err = p.importOpener(ctx, req.Content, req.Unit.ScopeKey); err != nil {
		return model.ProviderResult{}, err
	}
	if im.sc, err = openScratch(ctx, workDir, p.limits.MaxSpoolBytes); err != nil {
		return model.ProviderResult{}, err
	}
	defer im.sc.close()
	if err := im.run(ctx, open); err != nil {
		return model.ProviderResult{}, err
	}
	return im.result(), nil
}

// profileFor resolves a profile scope key to the configured profile.
func (p *Provider) profileFor(scopeKey string) (Profile, error) {
	name := strings.TrimPrefix(scopeKey, scopeProfile)
	for _, prof := range p.profiles {
		if prof.Name == name {
			return prof, nil
		}
	}
	return Profile{}, invalid("scip unit scope names profile " + name + ", which is not an approved SCIP indexer profile")
}

// importOpener resolves an import scope key to the snapshot file it names.
func (p *Provider) importOpener(ctx context.Context, view model.SnapshotView, scopeKey string) (opener, error) {
	path, ok := strings.CutPrefix(scopeKey, scopeImport)
	if !ok || path == "" || path != p.importPath {
		return nil, invalid("scip unit scope " + scopeKey + " is neither the configured import nor an approved profile")
	}
	var fv model.FileVersion
	var found bool
	err := view.EachFile(ctx, model.FileSelection{Paths: []string{path}}, func(f model.FileVersion) error {
		if f.Status != model.FileDeleted {
			fv, found = f, true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &model.Error{Code: model.CodeProviderUnavailable, Message: "the snapshot holds no scip index at " + path}
	}
	if fv.Size > p.limits.MaxIndexBytes {
		return nil, overLimit("index bytes", fv.Size, p.limits.MaxIndexBytes)
	}
	return func(ctx context.Context) (io.ReadCloser, int64, error) {
		rc, _, err := view.Open(ctx, fv.ID)
		return rc, fv.Size, err
	}, nil
}
