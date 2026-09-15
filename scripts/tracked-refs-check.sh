#!/usr/bin/env sh
# Tracked files must stand alone: nothing under version control may point at a
# gitignored path (the SDD ledger, lane briefs and reports under .superpowers/,
# lane worktrees under .worktrees/) or embed an absolute path from a developer's
# machine. Such a reference is unreadable in a clone, so the content it points
# at is inlined instead. Exit 1 lists every offending line.
set -eu
cd "$(dirname "$0")/.."
pattern='\.superpowers|\.worktrees/|-report\.md|-brief\.md|progress\.md|implementation-plan/|GPERF-digest|/home/sabossedgh'
hits="$(git ls-files -z | grep -zv -E '^(\.gitignore|scripts/tracked-refs-check\.sh)$' | xargs -0 grep -n -E "$pattern" 2>/dev/null || true)"
if [ -n "$hits" ]; then
  echo "tracked files reference ignored or host-local paths:"
  echo "$hits"
  exit 1
fi
echo "tracked-refs-check: clean"
