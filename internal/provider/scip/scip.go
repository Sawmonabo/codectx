// Package scip is the Section 11.4 provider: streaming import of SCIP
// precise indexes and execution of the six pinned SCIP indexers (scip-go,
// scip-typescript, scip-python, scip-java, rust-analyzer and scip-clang)
// against a private materialization of the pinned snapshot.
//
// Two inputs feed one provider. A supplied `.scip` file inside the snapshot
// is imported as it is; a managed profile is run through the shared process
// runner and its output imported the same way. A profile is product code, not
// configuration: its argument array, environment allowlist, budgets, timeout
// and declared network posture are constants of this build, and the binary it
// starts is the payload the embedded tool lock pinned and internal/toolchain
// verified (Section 20.2 — trust is the lock, not a user approval). Both go through the
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
	"slices"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/toolchain"
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
	Version = "4"
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

// Limits bound one import. Every field is a user-set bound
// (providers.scip.*) whose zero value is unlimited, which is the default:
// nothing here refuses a repository on the user's behalf (Section 6).
//
// Five of the seven cut nothing at all. An index is streamed record by record,
// documents and occurrences spool to an on-disk database, and a manifest is
// scanned line by line, so those figures bound neither heap nor the facts the
// unit emits: a user-set value is a REPORTING threshold, published on the
// unit's capability rows as `partial` + CTX_RESOURCE_LIMIT and never a refusal.
// MaxSourceFileBytes and MaxMaterializeBytes are the two that bound heap and
// disk respectively, so a user-set value there does leave a document or a file
// out -- and that skip is reported under the same detail.
type Limits struct {
	// MaxIndexBytes bounds the whole index file.
	MaxIndexBytes config.Limit
	// MaxRecordBytes bounds one metadata, occurrence or symbol record. It is
	// the wire reader's pre-allocation ceiling -- the bytes a single record
	// may cause to be allocated before it is decoded -- not a user-set bound,
	// so it stays positive and stays out of the configuration (plan class B).
	MaxRecordBytes int64
	// MaxDocuments and MaxOccurrencesPerDocument bound the walk.
	MaxDocuments              config.Limit
	MaxOccurrencesPerDocument config.Limit
	// MaxSpoolBytes bounds the bytes spooled to the scratch database.
	MaxSpoolBytes config.Limit
	// MaxSourceFileBytes bounds one document's source, which is held whole
	// while its positions are converted, as the parse limit does for
	// tree-sitter.
	MaxSourceFileBytes config.Limit
	// MaxMaterializeBytes bounds the private materialization a profile runs
	// against.
	MaxMaterializeBytes config.Limit
	// MaxManifestBytes bounds a supplied input-hash manifest and the
	// compilation database the C/C++ profile normalizes.
	MaxManifestBytes config.Limit
}

// defaultRecordBytes is the wire reader's pre-allocation ceiling when the
// caller names none. It is the only bound this provider still defaults to a
// finite figure, and it bounds an allocation rather than the work.
const defaultRecordBytes = 4 << 20

// Validate rejects a negative bound. Zero is unlimited at every user-set
// bound, which is what the defaults are; only MaxRecordBytes, the wire
// reader's pre-allocation ceiling, must be positive.
func (l Limits) Validate() error {
	for _, b := range []struct {
		name string
		v    config.Limit
	}{{"max_index_bytes", l.MaxIndexBytes}, {"max_documents", l.MaxDocuments},
		{"max_occurrences_per_document", l.MaxOccurrencesPerDocument}, {"max_spool_bytes", l.MaxSpoolBytes},
		{"max_source_file_bytes", l.MaxSourceFileBytes}, {"max_materialize_bytes", l.MaxMaterializeBytes},
		{"max_manifest_bytes", l.MaxManifestBytes}} {
		if b.v < 0 {
			return invalid(fmt.Sprintf("scip limit %s is %d; a bound is a positive value, or 0 for no bound at all", b.name, int64(b.v)))
		}
	}
	if l.MaxRecordBytes <= 0 {
		return invalid(fmt.Sprintf("scip limit max_record_bytes is %d; the wire reader's pre-allocation ceiling must be positive", l.MaxRecordBytes))
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
	// MaxEvidencePerFact is the effective per-fact evidence clip: the operator's
	// index.max_evidence_per_fact, or the model's record ceiling when they set
	// none. Zero selects the ceiling. Occurrences past it are counted and
	// disclosed, never dropped in silence.
	MaxEvidencePerFact int
	// Resolver hands out the pinned indexer payloads. It is the whole of the
	// provider's trust in a tool: nothing is looked up on PATH and nothing is
	// approved in configuration. A nil resolver means this build imports
	// supplied indexes only and runs no profile.
	Resolver *toolchain.Resolver
	// Runner is the shared process runner; required when a resolver is given.
	Runner *process.Runner
	// Timeout caps every profile run (providers.scip.timeout). Zero is no
	// wall-clock cap: an indexer on a monorepo is slow, not wedged.
	Timeout time.Duration
	// StallTimeout is the progress-based hang detector that stands in for the
	// wall clock (providers.scip.stall_timeout). Zero disables it.
	StallTimeout time.Duration
	// WorkDir is the absolute private directory for import scratch state.
	WorkDir string
	// Limits bound the import. Every field is unlimited by default; a zero
	// MaxRecordBytes selects the pre-allocation ceiling above.
	Limits Limits
	// LookupEnv supplies allowlisted environment values; nil means
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Provider is the SCIP provider.
type Provider struct {
	importPath, manifestPath string
	profiles                 []Profile
	// deferred are kinds this platform pins whose payload the store did not
	// hold at construction. They plan units like a resolved kind; the payload
	// is fetched by the first unit that runs one (Import).
	deferred []Kind
	missing  []unresolved
	version  string
	// resolver is kept for exactly that deferred fetch. Nothing else in the
	// provider reaches for a tool after construction.
	resolver *toolchain.Resolver
	runner   *process.Runner
	// timeout is the configured wall clock, zero meaning none; stallTimeout is
	// the progress-based hang detector that stands in its place.
	timeout      time.Duration
	stallTimeout time.Duration
	workDir      string
	limits       Limits
	// evidenceClip is the effective per-fact evidence bound this provider
	// emits under (Options.MaxEvidencePerFact), already resolved to a finite
	// number by New.
	evidenceClip int
	lookupEnv    func(string) (string, bool)
}

var _ provider.Provider = (*Provider)(nil)

// New validates the options, inspects the store for every managed profile
// payload and returns the provider.
//
// Construction installs nothing. It reads what the store already holds, and a
// pinned payload that is absent becomes a deferred kind that the first unit
// needing it fetches. Resolving with a fetch here installed the indexers of
// every language the lock knows on a machine that had not run
// `codectx tools prefetch`, before any detection had happened and before any
// unit existed (measured: 24.8 s of fetching at construction).
//
// What does not become lazy is the descriptor's version. It is fixed here,
// because Descriptor() takes no context and must be a deterministic function of
// the process: a version that changed as payloads appeared would key two units
// of the same run differently. It folds the identity of every kind that can
// produce facts -- the resolved fingerprint of a payload the store holds and
// the lock's pinned fingerprint of a deferred one, which are the same string
// for the same payload -- so a unit built before the payload was installed
// keys identically to one built after. Only a kind this machine cannot supply
// at all contributes an empty slot, and it produces no facts to key.
//
// A payload that cannot be resolved at all is not an error. It is recorded
// with the toolchain's own CTX_TOOL_* code and reported as honest absence, so
// a machine whose platform has no C++ payload still indexes Go.
func New(ctx context.Context, o Options) (*Provider, error) {
	if o.MaxEvidencePerFact <= 0 {
		o.MaxEvidencePerFact = model.MaxEvidencePerFact
	}
	if o.Limits.MaxRecordBytes == 0 {
		o.Limits.MaxRecordBytes = defaultRecordBytes
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
	if o.Resolver != nil && o.Runner == nil {
		return nil, invalid("scip profiles require the shared process runner")
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	var profs []Profile
	var deferred []Kind
	var identities map[Kind]string
	var missing []unresolved
	if o.Resolver != nil {
		profs, deferred, identities, missing = resolveProfiles(ctx, o.Resolver)
	}
	// A build with no payload identity at all carries no tools digest: an
	// import-only provider runs no indexer, so a digest over six empty slots
	// would key its units by tools it never had.
	version := Version
	if len(identities) > 0 {
		version = Version + "/" + toolsFingerprint(identities)
	}
	return &Provider{importPath: o.Import, manifestPath: o.Manifest, profiles: profs, deferred: deferred, missing: missing,
		version: version, resolver: o.Resolver, runner: o.Runner, timeout: o.Timeout, stallTimeout: o.StallTimeout,
		workDir: o.WorkDir, limits: o.Limits, evidenceClip: o.MaxEvidencePerFact, lookupEnv: o.LookupEnv}, nil
}

// Descriptor declares the provider: optional, workspace-invalidated, on top
// of the filesystem and tree-sitter providers, and on the manifest provider
// when a profile is configured (a profile reads the package manifests).
func (p *Provider) Descriptor() model.ProviderDescriptor {
	deps := []string{dependsFilesystem, dependsTreeSitter}
	if len(p.profiles) > 0 || len(p.deferred) > 0 {
		deps = append(deps, dependsManifest)
	}
	version := p.version
	if version == "" {
		version = Version
	}
	return model.ProviderDescriptor{ID: ID, Version: version, Capabilities: capabilities, DependsOn: deps,
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
	for _, k := range p.runnableKinds() {
		// A kind this machine cannot index precisely at all plans no unit: a
		// planned unit materializes the whole snapshot before the run and would
		// then fail on a payload Detect already reported as absent, with its
		// typed reason. A deferred kind does plan one -- its payload is pinned
		// for this platform and the unit fetches it.
		for _, trig := range Triggers(k) {
			if recognized[trig] {
				out = append(out, ProfileScope(string(k)))
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
	for _, k := range p.runnableKinds() {
		triggered := false
		for _, trig := range Triggers(k) {
			if info, err := root.Lstat(trig); err == nil && info.Mode().IsRegular() {
				triggered = true
				det.InputPaths = append(det.InputPaths, trig)
			}
		}
		if triggered {
			det.Available = true
			if slices.Contains(p.deferred, k) {
				// The kind is recorded as pending rather than as a refusal:
				// Select publishes a degraded capability row for a CTX_ value and
				// nothing for this one, which is why the spelling differs.
				det = det.WithDetail(string(k), markerDeferred)
			}
		}
	}
	// A language this workspace triggers whose indexer this machine cannot
	// supply is named here with the toolchain's own reason, whether or not some
	// other language's indexer resolved. Without it a repository with go.mod and
	// Cargo.toml on a machine with no usable rust-analyzer is reported available,
	// plans scip-go only, and says nothing anywhere about Rust: Scopes plans no
	// unit for the missing kind, so no capability row ever carries the reason
	// either.
	for _, u := range p.missing {
		if triggeredKind(root, u.kind) {
			det = det.WithDetail(string(u.kind), u.code)
		}
	}
	// The provenance line is the same bounded digest the descriptor commits to,
	// not a concatenation of six 82-byte fingerprints: ObservedVersion is capped
	// at model.MaxIdentifierBytes, and a concatenation would be truncated there
	// -- silently dropping whichever payloads sort last, so replacing one of them
	// would change nothing the coordinator folds into UnitSpec.ProviderVersion.
	if p.version != Version {
		det.ObservedVersion = p.version
	}
	if !det.Available {
		// The single diagnostic code of an unavailable detection is the first
		// triggered kind's reason; Details above carries every one of them.
		det.DiagnosticCode = model.CodeProviderUnavailable
		for _, u := range p.missing {
			if triggeredKind(root, u.kind) {
				det.DiagnosticCode = u.code
				break
			}
		}
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
		// The binding of a profile unit is decided by who produced the index,
		// not by which bytes the payload has: codectx invokes the indexer
		// itself. A deferred payload is therefore not fetched here -- Verify
		// runs before the unit is opened, and a fetch belongs to the run.
		if _, err := p.profileKind(scopeKey); err != nil {
			return "", err
		}
		return model.SourceBindingVerified, nil
	}
	fv, err := p.importFile(ctx, view, scopeKey)
	if err != nil {
		return "", err
	}
	sc, err := openScratch(ctx, p.workDir)
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
		prof, err := p.profileFor(ctx, req.Unit.ScopeKey)
		if err != nil {
			return Report{}, err
		}
		im.profile = &prof
		// The profile's work directory is the private root of every run. It is
		// the provider's own work directory, never a location a user names:
		// Section 20.2 leaves nothing about a profile to configuration.
		profWork := filepath.Join(p.workDir, "profiles", prof.Name())
		if err := os.MkdirAll(profWork, 0o700); err != nil {
			return Report{}, internal("scip profile work directory: " + err.Error())
		}
		runDir, err := os.MkdirTemp(profWork, "scip-run-")
		if err != nil {
			return Report{}, internal("scip run directory: " + err.Error())
		}
		defer os.RemoveAll(runDir)
		workDir = runDir
		output, manifestSHA, err := p.runProfile(ctx, prof, req.Content, runDir, &im.seen)
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
		im.seen.note(limitIndexBytes, fv.Size)
		im.indexHash, open = fv.ContentHash, p.fileOpener(req.Content, fv)
	}
	if im.sc, err = openScratch(ctx, workDir); err != nil {
		return Report{}, im.decorate(err)
	}
	defer im.sc.close()
	if err := im.run(ctx, open); err != nil {
		return Report{}, im.decorate(err)
	}
	rep := im.report()
	// False readiness. Every one of the six indexers needs the project's own
	// dependency context (research note 4), and four of them exit 0 having
	// produced a well-formed index that describes nothing when it is missing.
	// A unit sealed from such an index reports precise_definitions as fresh
	// over zero facts, which a consumer reads as an analysed absence. A full
	// profile import that admitted nothing is therefore a typed failure, not a
	// seal. A refresh is exempt: an unchanged snapshot legitimately emits no
	// record. This mirrors the dependence provider's export liveness gate.
	if im.profile != nil && opts.Previous == nil && rep.Result.RecordsEmitted == 0 {
		// This is the one refusal raised after a completed run, so it is the one
		// that holds a built manifest. The caller never receives this Report and
		// so can never close it; leaving it would orphan a temporary file under
		// the work directory on every false-readiness failure.
		_ = rep.Manifest.Close()
		return Report{}, im.decorate(&model.Error{Code: model.CodeProviderOutputInvalid,
			Message:     "the indexer exited successfully but its index describes no admitted document",
			Remediation: "restore the project's dependency context (installed packages, a build, a compilation database) and re-run"})
	}
	return rep, nil
}

// runnableKinds are the kinds this provider can plan a unit for, in Kinds
// order: those whose payload the store already holds and those whose pinned
// payload the first unit will fetch.
func (p *Provider) runnableKinds() []Kind {
	out := make([]Kind, 0, len(p.profiles)+len(p.deferred))
	for _, k := range Kinds {
		if slices.Contains(p.deferred, k) {
			out = append(out, k)
			continue
		}
		for _, prof := range p.profiles {
			if prof.Kind == k {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// triggeredKind reports whether the workspace holds a trigger of one kind. It
// reads metadata through the confined root and nothing else.
func triggeredKind(root workspace.Root, k Kind) bool {
	for _, trig := range kindSpecs[k].triggers {
		if info, err := root.Lstat(trig); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// profileKind resolves a profile scope key to the kind it names, without
// reaching for a payload. A kind this build knows but whose payload did not
// resolve reports the toolchain's own code, so a unit planned before a payload
// was lost fails with the reason rather than with "unknown profile".
func (p *Provider) profileKind(scopeKey string) (Kind, error) {
	name := strings.TrimPrefix(scopeKey, scopeProfile)
	for _, k := range p.runnableKinds() {
		if string(k) == name {
			return k, nil
		}
	}
	for _, u := range p.missing {
		if string(u.kind) == name {
			return "", u.err
		}
	}
	return "", invalid("scip unit scope names profile " + name + ", which is not a SCIP indexer this build knows")
}

// profileFor is profileKind followed by the payload. A kind the store already
// held is returned as it was resolved at construction; a deferred kind is
// fetched here, at the one moment the repository is known to contain the
// language. A fetch that fails fails the unit with the toolchain's typed code
// -- the same failure it would have had at construction, now only for a
// language this repository actually contains.
func (p *Provider) profileFor(ctx context.Context, scopeKey string) (Profile, error) {
	k, err := p.profileKind(scopeKey)
	if err != nil {
		return Profile{}, err
	}
	for _, prof := range p.profiles {
		if prof.Kind == k {
			return prof, nil
		}
	}
	if p.resolver == nil {
		return Profile{}, invalid("scip unit scope names profile " + string(k) + ", whose payload this build cannot resolve")
	}
	t, err := p.resolver.Resolve(ctx, string(k))
	if err != nil {
		return Profile{}, err
	}
	return Profile{Kind: k, Tool: t}, nil
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
	return fv, nil
}

// fileOpener streams one pinned snapshot file, once per pass.
func (p *Provider) fileOpener(view model.SnapshotView, fv model.FileVersion) opener {
	return func(ctx context.Context) (io.ReadCloser, int64, error) {
		rc, _, err := view.Open(ctx, fv.ID)
		return rc, fv.Size, err
	}
}
