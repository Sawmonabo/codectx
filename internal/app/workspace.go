package app

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// Workspace is one opened workspace: the composed stack plus the index
// coordinator over it. Opened for indexing it is the single cross-process owner
// of Section 13.2 -- the workspace lock is held for its whole lifetime -- so
// exactly one of `index`, `refresh`, `watch` and the MCP server builds at a
// time and a second caller is told the workspace is busy rather than becoming a
// second writer. Opened for a report it holds no lock and can build nothing.
type Workspace struct {
	s     *stack
	coord *index.Coordinator
}

// OpenWorkspace composes the workspace for a run that will index. wait is how
// long it waits for the workspace lock before reporting CTX_WORKSPACE_BUSY;
// wait <= 0 tries once. rebuild opens an explicitly requested new cache beside
// the configured one and leaves the existing database untouched (Section 12.2).
//
// A payload an admitted unit needs is installed on demand, by the unit
// (Section 11.7): an analyzer that is pinned but not yet downloaded is a fetch
// at unit time, not a missing capability and not a cost this call pays.
func OpenWorkspace(ctx context.Context, repo string, wait time.Duration, rebuild bool) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeIndex, wait: wait, rebuild: rebuild})
}

// OpenWorkspaceForReport composes the workspace for a read-only report. It
// takes no workspace lock, writes nothing and installs nothing, so a report
// answers while another process indexes or watches.
func OpenWorkspaceForReport(ctx context.Context, repo string) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeReport})
}

func open(ctx context.Context, repo string, o openOptions) (*Workspace, error) {
	s, err := openStack(ctx, repo, o)
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
		Watcher:  s.watcher,
		States:   s.states,
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

// DataDir is the cache this workspace actually opened. It is the configured
// data directory, or the sibling `index --rebuild` created, which is the one
// thing a caller cannot derive from the configuration alone.
func (w *Workspace) DataDir() string { return w.s.dataDir }

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
