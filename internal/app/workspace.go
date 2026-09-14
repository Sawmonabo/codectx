package app

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Workspace is one opened workspace: the composed stack plus the index
// coordinator over it. It is the single cross-process owner of Section 13.2 --
// the workspace lock is held for its whole lifetime -- so exactly one of
// `index`, `refresh`, `watch` and the MCP server builds at a time and a second
// caller is told the workspace is busy rather than becoming a second writer.
type Workspace struct {
	s     *stack
	coord *index.Coordinator
}

// OpenWorkspace composes the workspace for a run that will index. wait is how
// long it waits for the workspace lock before reporting CTX_WORKSPACE_BUSY;
// wait <= 0 tries once.
//
// A payload an admitted unit needs is installed on demand here (Section 11.7):
// this is the indexing path, and an analyzer that is pinned but not yet
// downloaded is a fetch, not a missing capability.
func OpenWorkspace(ctx context.Context, repo string, wait time.Duration) (*Workspace, error) {
	return open(ctx, repo, wait, modeIndex)
}

// OpenWorkspaceForReport composes the workspace for a read-only report and
// installs nothing while doing it (ledger 159). `status` must be able to say
// that an analyzer is not installed; a status path that installs it first can
// only ever report the answer it created.
func OpenWorkspaceForReport(ctx context.Context, repo string, wait time.Duration) (*Workspace, error) {
	return open(ctx, repo, wait, modeReport)
}

func open(ctx context.Context, repo string, wait time.Duration, mode openMode) (*Workspace, error) {
	s, err := openStack(ctx, repo, wait, mode)
	if err != nil {
		return nil, err
	}
	coord, err := index.New(index.Options{
		Root:     s.root,
		Config:   s.cfg,
		Store:    s.store,
		Registry: s.registry,
		CAS:      s.cas,
		Git:      s.git,
		Lock:     s.lock,
		Pool:     s.pool,
		Resolver: s.resolver,
		Logger:   s.logger,
		Now:      time.Now,
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return &Workspace{s: s, coord: coord}, nil
}

// Coordinator is the index coordinator over this workspace.
func (w *Workspace) Coordinator() *index.Coordinator { return w.coord }

// Resolver is the managed-toolchain resolver this workspace resolves analyzers
// through, and ToolStore is the store it reads. A report renders them; nothing
// else reaches for a tool outside a provider.
func (w *Workspace) Resolver() *toolchain.Resolver { return w.s.resolver }

// ToolStore is the absolute path of the tool store the resolver reads.
func (w *Workspace) ToolStore() string { return w.s.toolDir }

// LSP is the working-tree overlay manager (Section 11.5). It is not a registry
// provider: no overlay fact is ever written to a generation, so it is answered
// at query time and lives beside the coordinator rather than inside it.
func (w *Workspace) LSP() *lsp.Manager { return w.s.lsp }

// Root is the confined workspace root and Config the resolved configuration.
func (w *Workspace) Root() workspace.Root { return w.s.root }

// Config is the resolved configuration this workspace was composed from.
func (w *Workspace) Config() config.Config { return w.s.cfg }

// States are the capability rows for providers that could not be constructed
// at all, which detection therefore never sees. A reader that publishes
// completeness must fold these in, or an optional provider whose payload is
// absent disappears from the report instead of being reported unavailable.
func (w *Workspace) States() []model.CapabilityState { return w.s.states }

// Close releases the coordinator and then everything below it, in reverse.
// Both halves run even when the first fails: the workspace lock must be
// released whatever else went wrong.
func (w *Workspace) Close() error {
	if w == nil {
		return nil
	}
	var first error
	if w.coord != nil {
		first = w.coord.Close()
	}
	if err := w.s.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
