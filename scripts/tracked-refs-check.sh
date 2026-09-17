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
#
# A host path is caught ANYWHERE in a line, not only at its start: the one that
# got through was the first column of a CSV row, and one in a comment, a test
# fixture or a doc example is just as unreadable in a clone.
#
# Four forms, none of which has a legitimate use in tracked content:
#   * this machine's own home directory, spelled out. A synthetic one
#     (/home/tester, /home/private) is a fixture and stays; the giveaway is
#     that the path is THIS developer's, so the check is against $HOME rather
#     than against a name.
#   * an agent scratch directory: an ABSOLUTE path through a per-agent cache
#     directory, and an agent temporary root, wherever they appear and whoever
#     they
#     belong to. A script's own `$HOME`-rooted default for one is not a host
#     path: it resolves wherever it is run.
#   * a lane or wave token -- lane/<name>, wave-<letter>, FX-<letter>-<token>.
#     These name a branch and a worktree in one round of this project's own
#     work; they mean nothing to a reader of the repository and date the moment
#     the lane merges.
# A bare `~/` or `$HOME/` is NOT one of them: `~/.local/bin`, `~/.config` and
# `$XDG_DATA_HOME/...` are product locations this documentation has to name.
# The forms above catch the host-local ones without flagging those.
set -eu
cd "$(dirname "$0")/.."
pattern='\.superpower[s]|\.worktree[s]/|-report\.m[d]|-brief\.m[d]|progres[s]\.md|implementation-pla[n]/|GPERF-diges[t]'
pattern="$pattern"'|/[^ "'"'"']*/[.]cache/codect[x]-|/tmp/claude[-]'
pattern="$pattern"'|(^|[^[:alnum:]_])lan[e]/[A-Za-z]|(^|[^[:alnum:]_])wav[e]-[a-z]([^a-z]|$)|F[X]-[A-Z]-[A-Za-z0-9]'
# This machine's own home, when it is a real one: a path rooted in it names a
# place no clone has.
home="${HOME:-}"
case "$home" in
  /home/*|/Users/*) pattern="$pattern|$(printf '%s' "$home" | sed 's/[.[\*^$]/\\&/g')/" ;;
esac
hits="$(git ls-files -z | grep -zv -E '^\.gitignore$' | xargs -0 grep -n -E "$pattern" 2>/dev/null || true)"
if [ -n "$hits" ]; then
  echo "tracked files reference ignored or host-local paths:"
  echo "$hits"
  exit 1
fi
echo "tracked-refs-check: clean"
