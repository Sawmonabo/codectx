package cli

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/spf13/cobra"
)

// opener opens the workspace one command runs against. It is injected rather
// than chosen inside runService because the differences between the openers are
// real and not drift: a read command pins a generation and takes no
// cross-process lock; a one-shot building command must be the single owner of
// Section 13.2 for its whole run and waits for the workspace lock for as long
// as whoever holds it keeps making progress; a watching command builds too but
// owns the workspace only for the beat that builds. A runner that decided this
// for itself would have to be told which kind of command it was running
// anyway, and the flag that told it would be the second call path.
type opener func(ctx context.Context, repo string) (*app.Workspace, error)

// openForReport opens a read-only report workspace: no workspace lock, no
// generation mutation, nothing installed. Every command that only answers
// questions uses it, so a report answers while another process indexes.
func openForReport() opener {
	return app.OpenWorkspaceForReport
}

// openForQuery opens a workspace that writes nothing to the database: no
// writer connection at all, the schema fingerprint verified by reading and the
// pinned generation held without a retention lease. Every command that only
// answers questions uses it, so it answers throughout another process's index
// instead of waiting out that run's write transaction.
func openForQuery() opener {
	return app.OpenWorkspaceForQuery
}

// openForBuild opens the workspace as the single cross-process writer, with the
// operation name, waiting line, rebuild cache and supplied-index inputs the
// caller's flags resolved to. The ONE-SHOT building commands use it, and each
// names itself: the name is what the lock file carries for whichever process is
// refused while this one holds the workspace, and what the process waiting
// behind it prints while it waits.
func openForBuild(o app.OpenOptions) opener {
	return func(ctx context.Context, repo string) (*app.Workspace, error) {
		return app.OpenWorkspace(ctx, repo, o)
	}
}

// openForWatch opens the workspace for a session that watches: it builds, so it
// is one of the cross-process owners, but it takes the workspace for the beat
// that builds and gives it back when that beat ends rather than owning it for
// the session. A watch that is idle between beats is idle in every sense, and
// the person's own `codectx index` beside it runs.
//
// It is separate from openForBuild rather than a flag on it because the two
// differ in what the composition IS, which is also what decides that a
// one-shot command waits for a progressing holder and a beat does not: there is
// no waiting line to pass here, because nothing in this open waits.
func openForWatch(o app.OpenOptions) opener {
	return func(ctx context.Context, repo string) (*app.Workspace, error) {
		return app.OpenWorkspaceForWatch(ctx, repo, o)
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
		// durationFlag refuses a negative duration for every duration flag in
		// the tree, so the value reaching queryContext below is never the
		// negative that it would otherwise read as "no deadline" -- the
		// opposite of what the operator asked for. This is the only screen;
		// a second one here would be the same command line checked twice.
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
