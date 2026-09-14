package cli

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/spf13/cobra"
)

// opener opens the workspace one command runs against. It is injected rather
// than chosen inside runService because the difference between the two openers
// is real and not drift: a read command pins a generation and takes no
// cross-process lock, while a building command must be the single owner of
// Section 13.2 and waits a bounded time for the workspace lock. A runner that
// decided this for itself would have to be told which kind of command it was
// running anyway, and the flag that told it would be the second call path.
type opener func(ctx context.Context, repo string) (*app.Workspace, error)

// openForReport opens a read-only report workspace: no workspace lock, no
// generation mutation, nothing installed. Every command that only answers
// questions uses it, so a report answers while another process indexes.
func openForReport() opener {
	return app.OpenWorkspaceForReport
}

// openForBuild opens the workspace as the single cross-process writer, with the
// bounded lock wait, rebuild cache and supplied-index inputs the caller's flags
// resolved to. Only the building commands use it.
func openForBuild(o app.OpenOptions) opener {
	return func(ctx context.Context, repo string) (*app.Workspace, error) {
		return app.OpenWorkspace(ctx, repo, o)
	}
}

// runService is the one runner behind every command that reaches the
// application facade: it reads --repo, opens the workspace through the supplied
// opener, closes it on every path, applies the operator's --timeout to the
// service call alone and types a bare context failure on the way out.
//
// The workspace is handed to the callback beside *Services only for the two
// needs the facade deliberately does not carry (digest 17 Section 4):
// Workspace.ResolveNodes, which resolves `<name-or-id>` through storage rather
// than search, and Workspace.Coordinator, which `watch` streams from. A command
// that reaches for anything else on *Workspace has found a second call path
// into an operation the facade already exposes.
//
// *Services is never cached: it is a view over the services this workspace
// composed, and it dies with the deferred Close below.
func runService(cmd *cobra.Command, open opener,
	fn func(context.Context, *app.Workspace, *app.Services) error) error {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return err
	}
	// Not every command that opens a workspace is a query: `index`, `refresh`
	// and `status` declare --repo without --timeout, and reading a flag a
	// command does not have would reject a correct command line as invalid.
	var timeout time.Duration
	if cmd.Flags().Lookup(queryTimeoutFlag) != nil {
		if timeout, err = durationFlag(cmd, queryTimeoutFlag); err != nil {
			return err
		}
	}
	ws, err := open(cmd.Context(), repo)
	if err != nil {
		return queryFailure(err)
	}
	defer ws.Close()
	// The deadline covers the service call alone, for the reason queryContext
	// gives: a slow workspace open is not an operation that ran out of time.
	ctx, cancel := queryContext(cmd.Context(), timeout)
	defer cancel()
	// queryFailure types a bare context failure: left untyped, a deadline the
	// operator set with --timeout would reach ExitCode as the invalid-argument
	// class and report itself as a command line they typed wrong.
	return queryFailure(fn(ctx, ws, ws.Services()))
}

// The Section 18.2 omission indicator is frozen here as a NAME ONLY:
//
//	func writeOmitted(b *strings.Builder, shown, total int)
//
// It is not written by this lane. The renderers that landed clip per cell
// (tableCell, clip) and drop no rows, so a function created up front would ship
// with no caller -- the placeholder the Section 30.1 completion gate forbids.
// The first lane that actually omits rows writes it, under this name; if no
// lane omits rows, it is never written at all.
