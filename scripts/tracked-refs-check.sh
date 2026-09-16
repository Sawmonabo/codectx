#!/usr/bin/env sh
# Tracked files must stand alone: nothing under version control may point at a
# gitignored path (the planning ledger, its lane briefs and reports, the lane
# worktrees) or embed an absolute path rooted in a developer's home directory.
# Such a reference is unreadable in a clone, so the content it points at is
# inlined instead.
#
# The pattern spells its own literals with one-character bracket expressions
# (`[s]` matches `s`) so that this file is checked by the same rule as every
# other tracked file instead of exempting itself. Exit 1 lists every offending
# line.
set -eu
cd "$(dirname "$0")/.."
pattern='\.superpower[s]|\.worktree[s]/|-report\.m[d]|-brief\.m[d]|progres[s]\.md|implementation-pla[n]/|GPERF-diges[t]|^/(home|Users)/[^/]+/'
# .gitignore is the one exemption: naming the ignored paths is what it is for.
hits="$(git ls-files -z | grep -zv -E '^\.gitignore$' | xargs -0 grep -n -E "$pattern" 2>/dev/null || true)"
if [ -n "$hits" ]; then
  echo "tracked files reference ignored or host-local paths:"
  echo "$hits"
  exit 1
fi
echo "tracked-refs-check: clean"
