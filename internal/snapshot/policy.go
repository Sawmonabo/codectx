package snapshot

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// TraversalPolicy is the walk policy every component that traverses this
// workspace starts from: the configured policy plus the Git exclusion the
// repository itself defines, so provider detection, the notification watcher
// and the capture agree about which files exist.
//
// Without it a detection walk sees the ignored trees a capture never contains
// -- `node_modules/`, `target/`, `.venv/`, a build directory -- and answers
// with providers detected, versions observed and scopes proposed for source no
// generation can hold, which the planner then degrades as `no-inputs` after
// walking them.
//
// The exclusion is derived from `git ls-files --others --ignored --directory`,
// which lists only untracked paths, so a tracked file always wins over an
// ignore match (Section 10.2) without this having to ask what is tracked.
//
// It is an optimisation, not a correctness requirement: the capture applies
// its own, narrower rule regardless, so a caller that gets the base policy
// back walks the ignored trees, pays for them and proposes scopes the capture
// will not hold -- which costs work and coverage, and does not make the index
// wrong. Refusing to open the workspace does make it wrong, so a repository
// with more ignored roots than the bound degrades to the base policy with a
// warning instead of failing every command that traverses it.
//
// It is deliberately a *bounded* approximation of what a capture applies.
// Builder's own passes replace Ignore, ForceInclude and ForceIncludeDir with
// predicates backed by the capture's staging database, which admits exactly
// the paths Git's index and status name; that whitelist is strictly narrower
// than this blacklist and needs per-path state no standalone call has. So a
// caller of this function may still see an untracked, non-ignored path that a
// capture excludes for another reason. That direction is the safe one: an
// extra path costs a hash comparison, a missing one costs a capability.
func TraversalPolicy(ctx context.Context, base workspace.Policy, root workspace.Root, g *git.Git) (workspace.Policy, error) {
	if !root.HasGit || g == nil {
		return base, nil
	}
	// base.MaxIgnoredRoots (workspace.max_ignored_roots) bounds the set this
	// holds; zero -- the default -- is unlimited, and git.ListIgnoredRoots
	// reads zero the same way. The set is one entry per OUTERMOST ignored path
	// and every lookup is a random-access ancestor probe, so it is resident by
	// construction; a worktree whose ignored roots are genuinely numerous is
	// still admitted rather than refused, which is why the default is no bound
	// at all. `git ls-files --others --ignored --directory` collapses only
	// *wholly* untracked ignored directories, so a directory holding a tracked
	// file lists its ignored untracked files one by one -- the ordinary shape
	// of a checkout with `*.o`, `*.class` or `*.pyc` next to tracked sources --
	// which is why an operator may want a bound here at all.
	ignored := make(map[string]bool, 16)
	err := g.ListIgnoredRoots(ctx, root.Path, base.MaxIgnoredRoots, func(p string) error {
		ignored[p] = true
		return nil
	})
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeResourceLimit {
			// The logger is the package default because this function's
			// signature is the one every traversing component shares and
			// none of them has a logger to give it; the composition root
			// installs the process logger as that default.
			slog.Default().Warn("this worktree has more ignored roots than workspace.max_ignored_roots allows; the ignored trees are walked rather than excluded",
				"component", "snapshot", "max_ignored_roots", base.MaxIgnoredRoots)
			return base, nil
		}
		return workspace.Policy{}, err
	}
	if len(ignored) == 0 {
		return base, nil
	}
	base.Ignore = func(rel string, isDir bool) bool { return ignoredPath(ignored, rel, isDir) }
	return base, nil
}

// ignoredPath answers whether rel is one of the ignored roots or lies beneath
// one. Only ancestors are examined, so the cost is the path's depth, which the
// walk already bounds; nothing enumerates the set.
func ignoredPath(ignored map[string]bool, rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	if isDir && ignored[rel+"/"] {
		return true
	}
	if !isDir && ignored[rel] {
		return true
	}
	for i, ch := range []byte(rel) {
		if ch == '/' && ignored[rel[:i+1]] {
			return true
		}
	}
	return false
}
