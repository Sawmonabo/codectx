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
#     files, a note in any casing that a test was committed uncompiled or unrun,
#     a failure named after the corpus run that showed it, and a ruling cited
#     by its id. These date
#     the moment the work merges and mean nothing to a reader of the
#     repository.
#   * a home directory, /home/<user>/ or /Users/<user>/, on any machine. The
#     synthetic homes the fixtures use (tester, private, example-user) are
#     test data, not a host, and are allowed.
# A bare `~/` or `$HOME/` is NOT one of them: `~/.local/bin`, `~/.config` and
# `$XDG_DATA_HOME/...` are product locations this documentation has to name.
#
# The private-name guard (scripts/private-names-check.sh) runs here too, over
# every tracked path and its content, so one check fails on either kind of
# reference and both lists are printed.
#
# The check fails closed: when git cannot list the tracked files, when it lists
# none, or when a tracked file cannot be searched, it exits 2 with the reason
# and never reports clean. Each stage writes to a file whose status is checked,
# because a pipeline's status is only its last command's.
set -eu
cd "$(dirname "$0")/.."
fail() {
  echo "tracked-refs-check: $* (NOT checked)" >&2
  exit 2
}
tmp="$(mktemp -d)" || fail "cannot create a scratch directory"
trap 'rm -rf "$tmp"' EXIT
LC_ALL=C
export LC_ALL
w='(^|[^A-Za-z0-9_])'
pattern='\.superpower[s]|\.worktree[s]/|-report\.m[d]|-brief\.m[d]|progres[s]\.md|implementation-pla[n]/|GPERF-diges[t]'
pattern="$pattern"'|/[^ "'"'"']*/[.]cache/codect[x]-|/tmp/claude[-]'
pattern="$pattern|${w}lan[e]/[A-Za-z]|${w}[Ww]av[e]-[A-Za-z]([^A-Za-z]|\$)|F[X]-[A-Z]-[A-Za-z0-9]"
pattern="$pattern|${w}[Ll]an[e] \\(?[A-Z]+(-[A-Za-z0-9]+|[0-9])|${w}[Ll]an[e]('s)? report"
pattern="$pattern|${w}L[0-9]+ froz[e]|${w}L[0-9]+ FACAD[E]|${w}[A-Z]+PER[F]-[0-9]"
pattern="$pattern|${w}[Nn]ative-cor[e]([^A-Za-z]|\$)|${w}[Rr]evie[w] (finding|round)|N[Oo][Tt] [Rr][Uu][Nn]|[Nn][Oo][Tt] [Bb][Ee][Ee][Nn] ([Cc][Oo][Mm][Pp][Ii][Ll][Ee][Dd]|[Rr][Uu][Nn])|${w}r[0-9]+ failur[e]"
pattern="$pattern|${w}[Rr]ulin[g] [A-Z]+-?[0-9]"
home="${w}/(hom[e]|User[s])/[A-Za-z0-9._-]+"
allowed='/(hom[e]|User[s])/(tester|private|example-user)([^A-Za-z0-9._-]|$)'
git ls-files -z >"$tmp/files" || fail "git ls-files failed"
[ -s "$tmp/files" ] || fail "git ls-files listed no tracked file"
grep -zv -E '^\.gitignore$' <"$tmp/files" >"$tmp/scan" || [ $? -eq 1 ] || fail "cannot filter the file list"
# grep exits 1 when a batch has no match and 2 when it cannot read a file; the
# wrapper turns 2 into 255, which stops xargs with a status of its own.
xargs -0 -r sh -c 'grep -H -n -E -e "$0" -- "$@"; [ $? -le 1 ] || exit 255' "$pattern|$home" \
  <"$tmp/scan" >"$tmp/matches" || fail "grep could not search every tracked file"
hits="$(PATTERN="$pattern" HOMEPATH="$home" ALLOWED="$allowed" awk '{
    line = $0
    sub(/^[^:]*:[0-9]+:/, "", line)
    if (line ~ ENVIRON["PATTERN"]) { print; next }
    gsub(ENVIRON["ALLOWED"], " ", line)
    if (line ~ ENVIRON["HOMEPATH"]) print
  }' "$tmp/matches")" || fail "awk could not filter the matches"
status=0
if [ -n "$hits" ]; then
  echo "tracked files reference ignored or host-local paths or name the process:"
  echo "$hits"
  status=1
fi
names=0
sh scripts/private-names-check.sh || names=$?
[ "$names" -le 1 ] || fail "the private-name guard did not complete"
[ "$names" -eq 0 ] || status=1
if [ "$status" -ne 0 ]; then
  exit 1
fi
echo "tracked-refs-check: clean"
