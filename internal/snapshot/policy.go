package snapshot

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// maxIgnoredRoots bounds the ignored-root set one traversal policy holds. It is
// a product-owned structural bound, not a configuration knob: the set is the
// outermost ignored paths of a worktree, which is small on any real repository
// (this one has six), and the bound exists so a pathological tree is refused
// rather than held. A repository past it is a typed resource limit, never a
// truncated exclusion set -- a truncated one would silently admit the ignored
// trees it dropped.
const maxIgnoredRoots = 65536

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
	ignored := make(map[string]bool, 16)
	err := g.ListIgnoredRoots(ctx, root.Path, maxIgnoredRoots, func(p string) error {
		ignored[p] = true
		return nil
	})
	if err != nil {
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
