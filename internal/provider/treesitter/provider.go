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
//
// Memory is taken from each file's observed need (ADR-0012 decision 5): a
// worker holds its reported base on the process's reservation ledger, and
// each file reserves its predicted increment before it is dispatched -- the
// learned p99 of need per source byte for its repository, language, grammar
// fingerprint and size class, or the structural prior for the first file of
// that key. The need a worker measures covers the parse and the extraction
// only, since the dependence lowering does not run in the worker, so the
// model learns that and nothing more. The overrun target of at most 2% of
// files per class after the first generation is not claimed: overruns are
// counted per class and disclosed in Stats, and every file that overruns
// still runs.
package treesitter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/config"
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
// the hidden wire.Subcommand argument; tests point it at their
// own test binary, whose TestMain dispatches to worker.Main.
type WorkerCommand struct {
	Path string
	Args []string
}

// Options configure the provider. Nothing here has a default of its own: the
// two config.Limit fields are unlimited at zero, as config's defaults are;
// MaxEvidencePerFact selects the model's ceiling at zero; and the worker
// count, the allocation's re-derivation, the ledger, the runner, the worker
// and its directory come from the composition root and are required. There is no
// parse timeout -- a worker is ended only by a progress-based hang detector
// (see pool.go).
type Options struct {
	// Languages restricts the supported set (config tree_sitter.languages);
	// empty means every pinned language.
	Languages []string
	// MaxWorkers is one per CPU (config.ParserWorkers): the most parser subprocesses
	// alive at once.
	MaxWorkers int
	// MaxParseFileBytes is workspace.max_parse_file_bytes, the only size
	// policy on a file: a larger one is reported unavailable, never streamed.
	// Zero is unlimited. A file of any admitted size is streamed to the worker
	// in transport-unit frames (wire.ChunkBytes).
	MaxParseFileBytes config.Limit
	// MaxCalleeReferences is tree_sitter.max_callee_references: how many
	// distinct cross-file callee names one file may mint nodes for. Unlimited
	// by default; past a user-set bound the call is counted into the file's
	// dropped count and the file reports partial.
	MaxCalleeReferences config.Limit
	// MaxEvidencePerFact is the effective per-fact evidence clip: the operator's
	// index.max_evidence_per_fact, or the model's record ceiling when they set
	// none. Zero selects the ceiling. Occurrences past it are counted and
	// disclosed, never dropped in silence.
	MaxEvidencePerFact int
	// Rederive re-derives the admission allocation from the kernel's figure
	// and the product's own residency, of which workerResidentBytes is the
	// parser workers' part; the pool calls it between files. It is required.
	Rederive func(workerResidentBytes int64)
	// Admission is the process's one reservation ledger. Each worker holds
	// its base on it from before it is started until the runner has reaped
	// it, and each file holds its predicted increment from before it is
	// dispatched until its Done, so parser workers are admitted against the
	// same allocation, in the same queue, as every other heavy child. It is required: a pool with a running total of its own beside
	// the ledger is the oversubscription the ledger exists to prevent.
	Admission *admission.Ledger
	// Worker is the worker executable; Runner starts it; WorkDir is the
	// absolute private directory it runs in.
	Worker  WorkerCommand
	Runner  *process.Runner
	WorkDir string
}

// Provider is the treesitter provider. It is safe for concurrent use; Close
// stops every worker.
type Provider struct {
	opts      Options
	languages map[string]lang.Language
	pool      *pool

	// stage counts the parse callers in flight and the stages opened by
	// OpenStage. The parser workers stay warm for exactly as long as that
	// count is above zero: the last caller to leave drains the pool, so a run
	// that has stopped parsing holds no worker process at all. See pool.drain
	// for why this is not a timer. stageOpened is when the count last left
	// zero, and stageWall sums the stages that have closed.
	stageMu     sync.Mutex
	stage       int
	stageOpened time.Time
	stageWall   time.Duration
}

// enterStage registers one unit's parse work and leaveStage gives it back,
// draining the pool when the last unit leaves. They bracket the UNIT rather
// than the parse, so a worker is reused across the files of a unit and across
// units that overlap in time, and exits when the last of them is done.
//
// A stage is an indexing stage and nothing else. ParseProbe is deliberately
// outside it: a probe is one diagnostic parse, and bracketing each one would
// launch and reap a process per probe, which is both slower than the work and
// blind to whether a worker leaks across parses. A probe joins whatever stage
// is running and its worker is released by that stage's drain, or by Close.
func (p *Provider) enterStage(ctx context.Context) {
	p.stageMu.Lock()
	if p.stage == 0 {
		p.stageOpened = time.Now()
	}
	p.stage++
	p.stageMu.Unlock()
	p.pool.enterStage(ctx)
}

func (p *Provider) leaveStage(ctx context.Context) {
	p.stageMu.Lock()
	last := p.stage == 1
	p.stage--
	if last {
		p.stageWall += time.Since(p.stageOpened)
	}
	p.stageMu.Unlock()
	if last {
		p.pool.drain()
	}
	// After the drain: a worker's span ends when the runner has reaped it, so
	// the total this may close is closed over children that have all recorded
	// what they cost.
	p.pool.leaveStage(ctx)
}

// New validates options and builds the provider. Nothing is started until the
// first unit.
func New(o Options) (*Provider, error) {
	// A worker count is the caller's one-per-core figure (config.ParserWorkers)
	// and is never defaulted here: a count restated in this package would be
	// one nothing measured, and a provider composed with no workers would
	// start and parse nothing.
	if o.MaxWorkers <= 0 {
		return nil, invalidOption(fmt.Sprintf("the parser provider was given %d workers; it needs at least one", o.MaxWorkers))
	}
	if o.Rederive == nil {
		return nil, invalidOption("the treesitter provider needs the step that re-derives the allocation between files")
	}
	if o.Admission == nil {
		return nil, invalidOption("the treesitter provider needs the process reservation ledger its workers are admitted against")
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
		pool: newPool(o.Runner, o.Admission, o.Worker, o.WorkDir, o.MaxWorkers, o.Rederive)}, nil
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
func (p *Provider) Stats() Stats {
	s := p.pool.stats()
	p.stageMu.Lock()
	wall := p.stageWall
	if p.stage > 0 {
		wall += time.Since(p.stageOpened)
	}
	p.stageMu.Unlock()
	s.StageWallMS = wall.Milliseconds()
	return s
}

// Close stops every worker and waits for the runner to reap each one.
func (p *Provider) Close() { p.pool.close() }

// OpenStage opens one parse stage that holds the pool across every unit
// indexed before closeStage is called. IndexUnit keeps its own bracket, so a
// caller that never opens a stage is served exactly as before. closeStage
// leaves the stage once, however often it is called, under the run the stage
// was opened in: the context it is given is not consulted, so a caller cannot
// close another run's total. It answers what the pool did while the stage was
// open, measured after the leave so a drained worker's processor time is in
// it, and every later call answers the same figures.
func (p *Provider) OpenStage(ctx context.Context) (closeStage func(ctx context.Context) model.StageFigures) {
	window := p.pool.openWindow()
	p.enterStage(ctx)
	var once sync.Once
	var figures model.StageFigures
	return func(context.Context) model.StageFigures {
		once.Do(func() {
			p.leaveStage(ctx)
			figures = p.pool.closeWindow(window)
		})
		return figures
	}
}

// IndexUnit indexes the one file the unit's scope key names. The run always
// reports succeeded when facts were produced or the file was honestly
// skipped, with the capability state for the file's scope saying fresh,
// partial (syntax errors, a query that exceeded its match limit, or a bound
// the builder disclosed) or unavailable (over max_parse_file_bytes, wider than
// the parser can address, not UTF-8, or not a supported language); an
// unhealthy worker is replaced and the parse retried once; anything else fails
// the unit.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	p.enterStage(ctx)
	defer p.leaveStage(ctx)
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
	if p.opts.MaxParseFileBytes.Exceeded(fv.Size) || fv.Size > wire.MaxSourceOffset {
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
	ex, err := p.parse(ctx, req.Content, wire.Request{Language: l.Name, Path: fv.Path, SourceBytes: uint64(len(src))}, src)
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
	state, bounded := b.bounds(state)
	if ex.done.SyntaxErrors || ex.done.Truncated || bounded {
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
// or a startup failure is never retried. The file is admitted at its
// predicted increment with the worker in hand and learned from on its Done
// (see pool); view is the snapshot the file belongs to, and nil for a probe,
// which learns nothing. A caller that gives its worker back while it waits for
// the increment asks again; that is not an attempt.
func (p *Provider) parse(ctx context.Context, view model.SnapshotView, req wire.Request, src []byte) (*extraction, error) {
	f, err := p.pool.plan(ctx, view, req.Language, int64(len(src)), p.languageName)
	if err != nil {
		return nil, err
	}
	r := p.pool.request(int64(len(src)))
	for attempt := 0; ; {
		w, err := p.pool.acquire(ctx, r)
		if err != nil {
			return nil, err
		}
		increment, yielded, err := p.pool.reserveParse(ctx, f.reserved)
		if yielded {
			p.pool.release(w, true)
			continue
		}
		if err != nil {
			p.pool.release(w, true)
			return nil, err
		}
		ex, err := p.pool.parse(ctx, w, req, src)
		p.pool.endParse(increment)
		if err == nil {
			p.pool.count(ctx, w, ex)
			p.pool.settle(f, ex.done.Memory)
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
		attempt++
		p.pool.mu.Lock()
		p.pool.retries++
		p.pool.mu.Unlock()
	}
}

// languageName is LanguageOf as the pool counts a snapshot's files by it.
func (p *Provider) languageName(fv model.FileVersion) (string, bool) {
	l, ok := p.LanguageOf(fv)
	return l.Name, ok
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
// framing, validation and retry as IndexUnit, with no unit or sink -- and, as
// enterStage records, no stage of its own.
func (p *Provider) ParseProbe(ctx context.Context, relPath string, src []byte) (Probe, error) {
	l, ok := p.LanguageOf(model.FileVersion{Path: relPath})
	if !ok {
		return Probe{}, &model.Error{Code: model.CodeProviderUnavailable, Message: "no pinned grammar for " + bound(relPath, 256)}
	}
	if p.opts.MaxParseFileBytes.Exceeded(int64(len(src))) {
		return Probe{}, &model.Error{Code: model.CodeResourceLimit, Message: "the file exceeds max parse file bytes"}
	}
	if int64(len(src)) > wire.MaxSourceOffset {
		return Probe{}, &model.Error{Code: model.CodeResourceLimit, Message: "the file is longer than the parser's byte offsets can address"}
	}
	ex, err := p.parse(ctx, nil, wire.Request{Language: l.Name, Path: relPath, SourceBytes: uint64(len(src))}, src)
	if err != nil {
		return Probe{}, err
	}
	return Probe{Language: l.Name, Declarations: len(ex.decls), Imports: len(ex.imports), References: len(ex.refs),
		SyntaxErrors: ex.done.SyntaxErrors, Truncated: ex.done.Truncated}, nil
}
