#!/usr/bin/env sh
# Tracked files must stand alone: nothing under version control may point at a
# gitignored path (the planning ledger, its briefs and reports, the worktrees),
# embed an absolute path rooted in a developer's home directory, or name the
# process that produced it. Such a reference is unreadable in a clone, so the
# content it points at is inlined, and the rule it records is stated as the
# current rule.
#
# The patterns spell their own literals with one-character bracket expressions
# (`[s]` matches `s`) so that this file is checked by the same rule as every
# other tracked file instead of exempting itself. Exit 1 lists every offending
# line.
#
# Every form is caught ANYWHERE in a line, not only at its start: a path in the
# first column of a CSV row, a comment, a test fixture or a doc example is just
# as unreadable in a clone.
#
# The forms, none of which has a legitimate use in tracked content:
#   * a path into the ignored planning ledger or a worktree.
#   * an agent scratch directory: an ABSOLUTE path through a per-agent cache
#     directory, and an agent temporary root, wherever they appear and whoever
#     they belong to. A script's own `$HOME`-rooted default for one is not a
#     host path: it resolves wherever it is run.
#   * a process name: a branch or worktree token (lane/<name>, a wave letter,
#     FX-<letter>-<token>), a lane named by its id or by the id of a stage that
#     froze an interface, a performance-investigation id, a numbered finding or
#     round of a review, a round named after its branch, the report a lane
#     files, a note that a test was committed unrun, and a failure named after
#     the corpus run that showed it. These date the moment the work merges and
#     mean nothing to a reader of the repository.
#   * a home directory, /home/<user>/ or /Users/<user>/, on any machine. The
#     synthetic homes the fixtures use (tester, private, example-user) are
#     test data, not a host, and are allowed.
# A bare `~/` or `$HOME/` is NOT one of them: `~/.local/bin`, `~/.config` and
# `$XDG_DATA_HOME/...` are product locations this documentation has to name.
set -eu
cd "$(dirname "$0")/.."
LC_ALL=C
export LC_ALL
w='(^|[^A-Za-z0-9_])'
pattern='\.superpower[s]|\.worktree[s]/|-report\.m[d]|-brief\.m[d]|progres[s]\.md|implementation-pla[n]/|GPERF-diges[t]'
pattern="$pattern"'|/[^ "'"'"']*/[.]cache/codect[x]-|/tmp/claude[-]'
pattern="$pattern|${w}lan[e]/[A-Za-z]|${w}[Ww]av[e]-[A-Za-z]([^A-Za-z]|\$)|F[X]-[A-Z]-[A-Za-z0-9]"
pattern="$pattern|${w}[Ll]an[e] \\(?[A-Z]+(-[A-Za-z0-9]+|[0-9])|${w}[Ll]an[e]('s)? report"
pattern="$pattern|${w}L[0-9]+ froz[e]|${w}L[0-9]+ FACAD[E]|${w}[A-Z]+PER[F]-[0-9]"
pattern="$pattern|${w}[Nn]ative-cor[e]([^A-Za-z]|\$)|${w}[Rr]evie[w] (finding|round)|NOT RU[N]|${w}r[0-9]+ failur[e]"
home="${w}/(hom[e]|User[s])/[A-Za-z0-9._-]+"
allowed='/(hom[e]|User[s])/(tester|private|example-user)([^A-Za-z0-9._-]|$)'
hits="$(git ls-files -z | grep -zv -E '^\.gitignore$' | xargs -0 grep -n -E "$pattern|$home" 2>/dev/null |
  PATTERN="$pattern" HOMEPATH="$home" ALLOWED="$allowed" awk '{
    line = $0
    sub(/^[^:]*:[0-9]+:/, "", line)
    if (line ~ ENVIRON["PATTERN"]) { print; next }
    gsub(ENVIRON["ALLOWED"], " ", line)
    if (line ~ ENVIRON["HOMEPATH"]) print
  }' || true)"
if [ -n "$hits" ]; then
  echo "tracked files reference ignored or host-local paths or name the process:"
  echo "$hits"
  exit 1
fi
echo "tracked-refs-check: clean"
