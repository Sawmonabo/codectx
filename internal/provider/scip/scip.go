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
// carries matching embedded text or a row of a qualifying input-hash
// manifest, which must commit to the index's own digest).
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
//
// Version 3 is the Section 11.3/11.4 mapping: every reference occurrence
// publishes a call-site alias, an external entity's evidence carries no
// location, a read or write occurrence is a `references` edge because
// `reads`/`writes` are the dependence provider's facts (Section 11.6), and a
// document that does not declare its position encoding is converted in the
// measured encoding of the tool **build** that wrote the index and only after
// that encoding is proved against the pinned bytes.
//
// Each bump is what makes the units of the version before it unreachable. A
// unit sealed by an older version over unchanged inputs has the same scope key
// and the same input hash, so without it `UnitID` would be identical and the
// stale unit eligible for reuse. Version 1 sealed *zero* facts for
// scip-typescript, scip-java, scip-python, scip-go and scip-clang, because
// those five leave `position_encoding` unspecified and version 1 skipped every
// such document; version 2 sealed `reads`/`writes` edges this provider does
// not own, and facts converted under an encoding assumed from a tool name
// alone, which lands a fraction of them on source bytes that are not the
// symbol. Neither may be reused. `documentHashDomain` changes only when what
// the document hash covers changes, which forces a bump here in turn.
const (
	ID      = "scip"
	Version = "3"
)

// Capabilities this provider offers. Definitions covers symbol nodes and
// aliases; references covers reference and import occurrences;
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
	// supplied with the index. It verifies the index only in the v1 format
	// (see importer.loadManifest): the header, the SHA-256 of the index bytes
	// themselves, then one `<sha256>  <path>` row per file. A list of file
	// hashes that does not commit to the index is an assertion about some
	// index, not about this one, and proves nothing.
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
	profs, err := profiles(o.Analyzers)
	if err != nil {
		return nil, err
	}
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
		// A profile whose tool is not installed plans no unit: a planned unit
		// materializes the whole snapshot before the run and would then fail
		// on the missing binary, which is work and a failed unit for an
		// absence Detect already reports honestly.
		if !executableInstalled(prof.Executable) {
			continue
		}
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
	fv, err := p.importFile(ctx, view, scopeKey)
	if err != nil {
		return "", err
	}
	sc, err := openScratch(ctx, p.workDir, p.limits.MaxSpoolBytes)
	if err != nil {
		return "", err
	}
	defer sc.close()
	im := &importer{p: p, req: provider.UnitRequest{Content: view}, sc: sc, ctx: ctx, indexHash: fv.ContentHash}
	if err := im.openDelta(ctx); err != nil {
		return "", err
	}
	if p.manifestPath != "" {
		if err := im.loadManifest(ctx); err != nil {
			return "", err
		}
	}
	binding, _, err := im.scanBinding(ctx, p.fileOpener(view, fv))
	return binding, err
}

// ImportOptions carry what the Provider interface has no room for: the
// previous state of the unit being refreshed.
type ImportOptions struct {
	// Previous is the document manifest of the sealed unit this run refreshes,
	// or nil for a full import. When it is given the run publishes facts only
	// for the documents whose canonical hash changed, and Report.Delta names
	// the paths whose stored rows are kept, replaced and deleted.
	Previous *DocumentManifest
}

// Report is one import: the provider result the coordinator records, the
// per-document delta the storage writer applies, and the fresh document
// manifest it stores for the next refresh.
//
// Manifest is a private temporary file owned by the caller: Save copies it to
// durable storage and Close removes it. It is nil only when the import failed.
type Report struct {
	Result   model.ProviderResult
	Delta    Delta
	Manifest *DocumentManifest

	// Documents an index described and this import did not admit. They are
	// counted, never guessed about: OutsideRoot is a document whose
	// relative_path escapes the project root, DuplicatePaths a document a
	// later document with the same path superseded, Skipped a document the
	// snapshot does not hold or whose encoding or size the import cannot
	// stand behind.
	OutsideRoot, DuplicatePaths, Skipped int64
	// SkippedOccurrences are coordinates that did not land on the pinned
	// bytes under an unverified binding; SkippedCallsiteAliases are call-site
	// aliases whose key would exceed the alias bounds;
	// TruncatedEdgeOccurrences are occurrences past a relation's evidence
	// bound.
	SkippedOccurrences, SkippedCallsiteAliases, TruncatedEdgeOccurrences int64
	// AssumedPositionEncoding counts documents that left `position_encoding`
	// unspecified and were converted in the measured encoding of the tool that
	// wrote the index (see toolPositionEncoding).
	AssumedPositionEncoding int64
}

// IndexUnit builds one unit: the supplied index or one profile run. It is the
// full-import form of Import, which is what the Provider interface can
// express; a coordinator that holds the unit's previous document manifest
// calls Import instead.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	rep, err := p.Import(ctx, req, sink, ImportOptions{})
	if err != nil {
		return model.ProviderResult{}, err
	}
	// A full import's manifest has no reader: IndexUnit's caller cannot
	// receive it, so it is not left behind in the work directory.
	if cerr := rep.Manifest.Close(); cerr != nil {
		return model.ProviderResult{}, cerr
	}
	return rep.Result, nil
}

// Import builds one unit and reports the per-document delta. See ImportOptions
// and Report.
func (p *Provider) Import(ctx context.Context, req provider.UnitRequest, sink provider.Sink, opts ImportOptions) (Report, error) {
	if req.Content == nil || req.Resolver == nil || sink == nil {
		return Report{}, invalid("scip unit request needs a snapshot view, a resolver and a sink")
	}
	im := &importer{p: p, req: req, sink: sink, ctx: ctx, previous: opts.Previous}
	defer im.close()
	var open opener
	var err error
	workDir := p.workDir
	if strings.HasPrefix(req.Unit.ScopeKey, scopeProfile) {
		prof, err := p.profileFor(req.Unit.ScopeKey)
		if err != nil {
			return Report{}, err
		}
		im.profile = &prof
		// The profile's work directory is the private root of every run; it
		// belongs to codectx, not to the user's shell, so it is created with
		// private permissions rather than assumed to exist.
		if err := os.MkdirAll(prof.WorkDir, 0o700); err != nil {
			return Report{}, internal("scip profile work directory: " + err.Error())
		}
		runDir, err := os.MkdirTemp(prof.WorkDir, "scip-run-")
		if err != nil {
			return Report{}, internal("scip run directory: " + err.Error())
		}
		defer os.RemoveAll(runDir)
		workDir = runDir
		output, manifestSHA, err := p.runProfile(ctx, prof, req.Content, runDir)
		if err != nil {
			return Report{}, err
		}
		im.manifestSHA = manifestSHA
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
	} else {
		fv, ferr := p.importFile(ctx, req.Content, req.Unit.ScopeKey)
		if ferr != nil {
			return Report{}, ferr
		}
		im.indexHash, open = fv.ContentHash, p.fileOpener(req.Content, fv)
	}
	if im.sc, err = openScratch(ctx, workDir, p.limits.MaxSpoolBytes); err != nil {
		return Report{}, im.decorate(err)
	}
	defer im.sc.close()
	if err := im.run(ctx, open); err != nil {
		return Report{}, im.decorate(err)
	}
	return im.report(), nil
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

// importFile resolves an import scope key to the pinned snapshot file it
// names. Its ContentHash is the SHA-256 of the index bytes themselves, which
// is what a supplied input-hash manifest must commit to before it can verify
// anything (see importer.loadManifest).
func (p *Provider) importFile(ctx context.Context, view model.SnapshotView, scopeKey string) (model.FileVersion, error) {
	path, ok := strings.CutPrefix(scopeKey, scopeImport)
	if !ok || path == "" || path != p.importPath {
		return model.FileVersion{}, invalid("scip unit scope " + scopeKey + " is neither the configured import nor an approved profile")
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
		return model.FileVersion{}, err
	}
	if !found {
		return model.FileVersion{}, &model.Error{Code: model.CodeProviderUnavailable, Message: "the snapshot holds no scip index at " + path}
	}
	if fv.Size > p.limits.MaxIndexBytes {
		return model.FileVersion{}, overLimit("index bytes", fv.Size, p.limits.MaxIndexBytes)
	}
	return fv, nil
}

// fileOpener streams one pinned snapshot file, once per pass.
func (p *Provider) fileOpener(view model.SnapshotView, fv model.FileVersion) opener {
	return func(ctx context.Context) (io.ReadCloser, int64, error) {
		rc, _, err := view.Open(ctx, fv.ID)
		return rc, fv.Size, err
	}
}
