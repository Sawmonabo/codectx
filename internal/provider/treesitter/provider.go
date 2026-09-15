// Package treesitter is the bundled structural provider of Section 11.3: a
// tree-sitter pass over each supported source file that publishes
// declarations, containing scopes, signatures, imports and exports, syntax
// references and call sites, test declarations and attached documentation at
// precision syntax. Parsing runs in isolated worker subprocesses (package
// worker) started through the shared process runner; this package is the
// parent side, which streams pinned bytes to a worker, validates every framed
// fact it answers with against those bytes, resolves identities through the
// unit's resolver and emits facts through the sink. It never links the
// grammars itself and never holds a repository-wide AST or source cache: the
// unit of work is one file, and its bytes live only for that unit.
package treesitter

import (
	"context"
	"errors"
	"fmt"
	"github.com/Sawmonabo/codectx/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/source"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// ScopePrefix is the unit scope this provider indexes: one file, as
// "file:"+path. The coordinator plans one unit per supported file.
const ScopePrefix = "file:"

// fingerprint is the pinned grammar and query identity every worker must
// report before it is trusted with a parse.
var fingerprint = lang.Fingerprint()

// WorkerCommand is how a parser worker is started: the absolute path of the
// executable and its literal arguments. In production it is this binary with
// the hidden wire.Subcommand argument (ruling R8-1); tests point it at their
// own test binary, whose TestMain dispatches to worker.Main.
type WorkerCommand struct {
	Path string
	Args []string
}

// Options configure the provider. Zero values take the defaults below, which
// match config's: two workers, 5 MiB per file, a 60-second idle TTL.
type Options struct {
	// Languages restricts the supported set (config tree_sitter.languages);
	// empty means every pinned language.
	Languages []string
	// MaxWorkers is index.max_parser_workers: the most parser subprocesses
	// alive at once.
	MaxWorkers int
	// MaxParseFileBytes is workspace.max_parse_file_bytes; a larger file is
	// reported unavailable, never streamed.
	MaxParseFileBytes config.Limit
	// WorkerIdleTTL is tree_sitter.worker_idle_ttl: how long an idle worker
	// is kept before it is stopped.
	WorkerIdleTTL time.Duration
	// MaxCalleeReferences is tree_sitter.max_callee_references: how many
	// distinct cross-file callee names one file may mint nodes for. Unlimited
	// by default; past a user-set bound the call is counted into the file's
	// dropped count and the file reports partial.
	MaxCalleeReferences config.Limit
	// MaxRecordsPerFile is tree_sitter.max_records_per_file: how many
	// declarations, imports or references (each counted separately) one file
	// may yield. Unlimited by default. It is carried on every parse request so
	// the worker that extracts and the parent that reads the frames back apply
	// the one number the operator set.
	MaxRecordsPerFile config.Limit
	// MaxEvidencePerFact is the effective per-fact evidence clip: the operator's
	// index.max_evidence_per_fact, or the model's record ceiling when they set
	// none. Zero selects the ceiling. Occurrences past it are counted and
	// disclosed, never dropped in silence.
	MaxEvidencePerFact int
	// ParseTimeout bounds one parse; a worker past it is killed and the unit
	// is CTX_PROVIDER_TIMEOUT.
	ParseTimeout time.Duration
	// WorkerMemoryBytes is the memory reservation each worker is admitted
	// against in the runner.
	WorkerMemoryBytes int64
	// Worker is the worker executable; Runner starts it; WorkDir is the
	// absolute private directory it runs in.
	Worker  WorkerCommand
	Runner  *process.Runner
	WorkDir string
}

const (
	defaultMaxWorkers   = 2
	defaultIdleTTL      = 60 * time.Second
	defaultParseTimeout = 60 * time.Second
	defaultWorkerMemory = 256 << 20
)

// Provider is the treesitter provider. It is safe for concurrent use; Close
// stops every worker.
type Provider struct {
	opts      Options
	languages map[string]lang.Language
	pool      *pool
}

// New validates options and builds the provider. Nothing is started until the
// first unit.
func New(o Options) (*Provider, error) {
	if o.MaxWorkers <= 0 {
		o.MaxWorkers = defaultMaxWorkers
	}
	// Unlimited is left unlimited: substituting a finite default here would
	// discard a user's explicit "no bound" with no report. The worker's
	// class-B source ceiling is enforced at the admission sites below, which
	// is where a file that exceeds it is reported unavailable.
	if o.MaxParseFileBytes.Exceeded(wire.MaxSourceBytes) {
		return nil, invalidOption(fmt.Sprintf("max parse file bytes %d exceed the worker's %d-byte source ceiling", o.MaxParseFileBytes, wire.MaxSourceBytes))
	}
	if o.WorkerIdleTTL <= 0 {
		o.WorkerIdleTTL = defaultIdleTTL
	}
	if o.ParseTimeout <= 0 {
		o.ParseTimeout = defaultParseTimeout
	}
	if o.WorkerMemoryBytes <= 0 {
		o.WorkerMemoryBytes = defaultWorkerMemory
	}
	if o.Runner == nil {
		return nil, invalidOption("the treesitter provider needs the shared process runner")
	}
	if !filepath.IsAbs(o.Worker.Path) {
		return nil, invalidOption("the worker executable must be an absolute path")
	}
	if !filepath.IsAbs(o.WorkDir) {
		return nil, invalidOption("the worker working directory must be an absolute private directory")
	}
	langs := map[string]lang.Language{}
	if len(o.Languages) == 0 {
		for _, l := range lang.All {
			langs[l.Name] = l
		}
	}
	for _, name := range o.Languages {
		l, ok := lang.Lookup(name)
		if !ok {
			return nil, invalidOption("language " + bound(name, 64) + " is not one the treesitter provider supports")
		}
		langs[l.Name] = l
	}
	return &Provider{opts: o, languages: langs,
		pool: newPool(o.Runner, o.Worker, o.WorkDir, o.MaxWorkers, o.WorkerIdleTTL, o.ParseTimeout, o.WorkerMemoryBytes)}, nil
}

func invalidOption(msg string) *model.Error {
	return &model.Error{Code: model.CodeConfigInvalid, Message: msg}
}

// Descriptor reports identity and the scheduling contract. Version folds the
// binding module, every pinned grammar version and ABI and the query packs
// (lang.Version), so a grammar or query change invalidates every unit.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{
		ID: lang.ProviderID, Version: lang.Version(), Capabilities: []string{capabilityName},
		DependsOn: []string{"filesystem"}, InvalidationScope: model.InvalidationFile, Required: true,
	}
}

// Detect reports whether the worker executable can be started. The grammars
// are linked into that executable, so nothing in the repository decides
// availability; an absent or non-executable worker is unavailable, not a
// failure.
func (p *Provider) Detect(_ context.Context, _ workspace.Root, _ workspace.Policy) (provider.Detection, error) {
	info, err := os.Stat(p.opts.Worker.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return provider.Detection{Available: false, DiagnosticCode: model.CodeProviderUnavailable}, nil
	}
	return provider.Detection{Available: true, Capabilities: []string{capabilityName}}, nil
}

// LanguageOf reports the pinned language for a manifest row: the snapshot's
// language tag when it names one this provider supports, else the extension.
func (p *Provider) LanguageOf(fv model.FileVersion) (lang.Language, bool) {
	if l, ok := p.languages[fv.Language]; ok {
		return l, true
	}
	l, ok := lang.ByExtension(fv.Path)
	if !ok {
		return lang.Language{}, false
	}
	_, ok = p.languages[l.Name]
	return l, ok
}

// Stats is the aggregate parent-plus-worker resource view.
func (p *Provider) Stats() Stats { return p.pool.stats() }

// Close stops every worker and waits for the runner to reap each one.
func (p *Provider) Close() { p.pool.close() }

// IndexUnit indexes the one file the unit's scope key names. The run always
// reports succeeded when facts were produced or the file was honestly
// skipped, with the capability state for the file's scope saying fresh,
// partial (syntax errors or a record bound reached) or unavailable (over the
// size limit, not UTF-8, or not a supported language); an unhealthy worker is
// replaced and the parse retried once; anything else fails the unit.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	relPath, ok := strings.CutPrefix(req.Unit.ScopeKey, ScopePrefix)
	if !ok || relPath == "" {
		return model.ProviderResult{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "treesitter units are scoped to one file as \"file:<path>\"",
			Details: map[string]string{"scope_key": bound(req.Unit.ScopeKey, 256)}}
	}
	fv, err := p.lookup(ctx, req.Content, relPath)
	if err != nil {
		return model.ProviderResult{}, err
	}
	result := model.ProviderResult{RunID: req.Run, State: model.RunSucceeded}
	state := model.CapabilityState{ProviderID: lang.ProviderID, Capability: capabilityName, Scope: req.Unit.ScopeKey, State: model.CapabilityFresh}
	finish := func(st model.CapabilityStateValue, code string) (model.ProviderResult, error) {
		state.State, state.DiagnosticCode = st, code
		result.Capabilities = []model.CapabilityState{state}
		return result, nil
	}
	l, ok := p.LanguageOf(fv)
	if !ok {
		return finish(model.CapabilityUnavailable, model.CodeProviderUnavailable)
	}
	if p.opts.MaxParseFileBytes.Exceeded(fv.Size) || fv.Size > wire.MaxSourceBytes {
		return finish(model.CapabilityUnavailable, model.CodeResourceLimit)
	}
	src, err := p.read(ctx, req.Content, fv)
	if err != nil {
		return model.ProviderResult{}, err
	}
	result.BytesProcessed = uint64(len(src))
	if !utf8.Valid(src) {
		return finish(model.CapabilityUnavailable, model.CodeProviderUnavailable)
	}
	ex, err := p.parse(ctx, wire.Request{Language: l.Name, Path: fv.Path, SourceBytes: uint32(len(src)),
		MaxRecordsPerFile: uint64(p.opts.MaxRecordsPerFile.Value())}, src)
	if err != nil {
		return model.ProviderResult{}, err
	}
	b := &builder{ctx: ctx, req: req, fv: fv, lang: l, src: src, cur: source.NewCursor(src), ex: ex,
		maxCallees: p.opts.MaxCalleeReferences, evidenceClip: p.opts.MaxEvidencePerFact}
	if err := b.build(); err != nil {
		return model.ProviderResult{}, err
	}
	records, err := b.emit(sink)
	if err != nil {
		return model.ProviderResult{}, err
	}
	result.RecordsEmitted = records
	if ex.done.SyntaxErrors || ex.done.Truncated || b.dropped > 0 {
		return finish(model.CapabilityPartial, model.CodeCoverageIncomplete)
	}
	return finish(model.CapabilityFresh, "")
}

// lookup finds the manifest row for one path without walking the manifest.
func (p *Provider) lookup(ctx context.Context, view model.SnapshotView, relPath string) (model.FileVersion, error) {
	var found *model.FileVersion
	err := view.EachFile(ctx, model.FileSelection{Paths: []string{relPath}}, func(fv model.FileVersion) error {
		if fv.Path == relPath && found == nil {
			f := fv
			found = &f
		}
		return nil
	})
	if err != nil {
		return model.FileVersion{}, err
	}
	if found == nil || found.Status == model.FileDeleted {
		return model.FileVersion{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "the unit's file is not in the pinned snapshot",
			Details: map[string]string{"path": bound(relPath, 256)}}
	}
	return *found, nil
}

// read loads the file's pinned bytes through the snapshot view, bounded by
// the manifest size, and refuses bytes that do not match it.
func (p *Provider) read(ctx context.Context, view model.SnapshotView, fv model.FileVersion) ([]byte, error) {
	rc, _, err := view.Open(ctx, fv.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	src := make([]byte, 0, fv.Size+1)
	buf := make([]byte, 64<<10)
	for {
		n, err := rc.Read(buf)
		src = append(src, buf[:n]...)
		if int64(len(src)) > fv.Size {
			return nil, &model.Error{Code: model.CodeSourceIntegrity, Message: "the pinned blob is longer than its manifest size"}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if int64(len(src)) != fv.Size {
		return nil, &model.Error{Code: model.CodeSourceIntegrity, Message: "the pinned blob is shorter than its manifest size"}
	}
	return src, nil
}

// parse runs one request on a pooled worker, replacing an unhealthy worker
// and retrying exactly once (Section 11.3). A cancellation, a per-file error
// or a startup failure is never retried.
func (p *Provider) parse(ctx context.Context, req wire.Request, src []byte) (*extraction, error) {
	for attempt := 0; ; attempt++ {
		w, err := p.pool.acquire(ctx)
		if err != nil {
			return nil, err
		}
		ex, err := p.pool.parse(ctx, w, req, src)
		if err == nil {
			p.pool.release(w, true)
			return ex, nil
		}
		var perFile perFileError
		if errors.As(err, &perFile) {
			p.pool.release(w, true)
			return nil, perFile.err
		}
		p.pool.release(w, false)
		var typed *model.Error
		if ctx.Err() != nil || (errors.As(err, &typed) && (typed.Code == model.CodeCanceled || typed.Code == model.CodeProviderTimeout)) {
			return nil, err
		}
		if attempt == 1 {
			return nil, (&model.Error{Code: model.CodeProviderUnavailable, Message: "the parser worker failed twice on " + bound(req.Path, 256), Retryable: true}).
				WithDetail("cause", bound(err.Error(), 256))
		}
		p.pool.mu.Lock()
		p.pool.retries++
		p.pool.mu.Unlock()
	}
}

// Probe is the outcome of a diagnostic parse: what the worker extracted,
// without storage, identities or the resolver.
type Probe struct {
	Language     string
	Declarations int
	Imports      int
	References   int
	SyntaxErrors bool
	Truncated    bool
}

// ParseProbe parses src as the file at relPath through the real worker path
// and reports the extraction counts. It is the diagnostic surface behind
// doctor-style checks and the resource plateau benchmark: the same pool,
// framing, validation and retry as IndexUnit, with no unit or sink.
func (p *Provider) ParseProbe(ctx context.Context, relPath string, src []byte) (Probe, error) {
	l, ok := p.LanguageOf(model.FileVersion{Path: relPath})
	if !ok {
		return Probe{}, &model.Error{Code: model.CodeProviderUnavailable, Message: "no pinned grammar for " + bound(relPath, 256)}
	}
	if p.opts.MaxParseFileBytes.Exceeded(int64(len(src))) || int64(len(src)) > wire.MaxSourceBytes {
		return Probe{}, &model.Error{Code: model.CodeResourceLimit, Message: "the file exceeds max parse file bytes"}
	}
	ex, err := p.parse(ctx, wire.Request{Language: l.Name, Path: relPath, SourceBytes: uint32(len(src)),
		MaxRecordsPerFile: uint64(p.opts.MaxRecordsPerFile.Value())}, src)
	if err != nil {
		return Probe{}, err
	}
	return Probe{Language: l.Name, Declarations: len(ex.decls), Imports: len(ex.imports), References: len(ex.refs),
		SyntaxErrors: ex.done.SyntaxErrors, Truncated: ex.done.Truncated}, nil
}
