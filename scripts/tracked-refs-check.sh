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
#     files, a note that a test was left without being compiled or run (in
#     any casing: "not been", "was not" or "were not" before "run" or
#     "compiled", the one-word forms with an "un" prefix, "test(s) not run"
#     and a parenthesized "not run"; and a "not run" whose first letter
#     is a capital), a failure named after the corpus run that showed it, and
#     a ruling cited by its id. These date the moment the work merges and
#     mean nothing to a reader of the repository. A bare lowercase "not run"
#     is not one of them: "does not run" and "cannot run" are how this
#     repository describes a provider or a check.
#   * a home directory, /home/<user>/ or /Users/<user>/, on any machine. The
#     synthetic homes the fixtures use (tester, private, example-user) are
#     test data, not a host, and are allowed.
# A bare `~/` or `$HOME/` is NOT one of them: `~/.local/bin`, `~/.config` and
# `$XDG_DATA_HOME/...` are product locations this documentation has to name.
#
# Every form is checked in each tracked file's NAME as well as its content.
# The content is what the index records, read from its blobs
# (scripts/tracked-blobs.pl): a tracked symlink is checked by its link text,
# never by the file it points at, a file holding NUL bytes is searched line by
# line like any other rather than reported as a binary match, and a file is
# checked as it is staged, so stage an edit before checking it.
#
# The private-name guard (scripts/private-names-check.sh) runs here too, over
# every tracked path and its content, so one check fails on either kind of
# reference and both lists are printed.
#
# The check takes no argument. It fails closed: on any argument, when git
# cannot list the tracked files, when it lists none, or when a tracked entry
# cannot be read (a submodule included), it exits 2 with the reason and never
# reports clean.
set -eu
cd "$(dirname "$0")/.."
fail() {
  echo "tracked-refs-check: $* (NOT checked)" >&2
  exit 2
}
[ "$#" -eq 0 ] || fail "usage: tracked-refs-check.sh (it takes no argument)"
LC_ALL=C
export LC_ALL
w='(^|[^A-Za-z0-9_])'
pattern='\.superpower[s]|\.worktree[s]/|-report\.m[d]|-brief\.m[d]|progres[s]\.md|implementation-pla[n]/|GPERF-diges[t]'
pattern="$pattern"'|/[^ "'"'"']*/[.]cache/codect[x]-|/tmp/claude[-]'
pattern="$pattern|${w}lan[e]/[A-Za-z]|${w}[Ww]av[e]-[A-Za-z]([^A-Za-z]|\$)|F[X]-[A-Z]-[A-Za-z0-9]"
pattern="$pattern|${w}[Ll]an[e] \\(?[A-Z]+(-[A-Za-z0-9]+|[0-9])|${w}[Ll]an[e]('s)? report"
pattern="$pattern|${w}L[0-9]+ froz[e]|${w}L[0-9]+ FACAD[E]|${w}[A-Z]+PER[F]-[0-9]"
pattern="$pattern|${w}[Nn]ative-cor[e]([^A-Za-z]|\$)|${w}[Rr]evie[w] (finding|round)|${w}r[0-9]+ failur[e]"
# The notes that a test was left without being compiled or run, in any casing.
ran='([Rr][Uu][Nn]|[Cc][Oo][Mm][Pp][Ii][Ll][Ee][Dd])([^A-Za-z-]|$)'
pattern="$pattern|N[Oo][Tt] [Rr][Uu][Nn]|${w}[Uu][Nn]$ran"
pattern="$pattern|${w}([Bb][Ee][Ee][Nn]|[Ww][Aa][Ss]|[Ww][Ee][Rr][Ee]) [Nn][Oo][Tt] ([Yy][Ee][Tt] )?$ran"
pattern="$pattern|${w}[Nn][Oo][Tt] [Bb][Ee][Ee][Nn] $ran|${w}[Tt][Ee][Ss][Tt][Ss]? [Nn][Oo][Tt] ([Yy][Ee][Tt] )?$ran"
pattern="$pattern|\\([Nn][Oo][Tt] ([Yy][Ee][Tt] )?[Rr][Uu][Nn]\\)"
pattern="$pattern|${w}[Rr]ulin[g] [A-Z]+-?[0-9]"
home="${w}/(hom[e]|User[s])/[A-Za-z0-9._-]+"
allowed='/(hom[e]|User[s])/(tester|private|example-user)([^A-Za-z0-9._-]|$)'
# The scanner exits 0 when clean, 1 when it printed an offending line, and 2
# when it could not read every tracked entry.
scan='
use strict; use warnings;
my ($pattern, $home, $allowed) = map { qr/$_/ } @ARGV;
my $failed = 0; my $hits = 0;
sub failure { print STDERR "tracked-refs-check: $_[0]\n"; $failed++; }
sub offends {
  my ($line) = @_;
  return 1 if $line =~ $pattern;
  (my $rest = $line) =~ s/$allowed/ /g;
  return $rest =~ $home;
}
eval { require "./scripts/tracked-blobs.pl"; 1 } or do { failure("cannot load scripts/tracked-blobs.pl: $@"); exit 2; };
my $entries = tracked_blobs(sub {
  my ($path, $kind, $data) = @_;
  if (offends($path)) { $hits++; print "$path (file name)\n"; }
  # The ignore file names the ignored paths by design.
  return if !defined $data || $path eq q(.gitignore);
  my $what = $kind eq q(120000) ? q( (symlink target)) : q();
  my $n = 0;
  for my $line (split /\n/, $data) {
    $n++;
    if (offends($line)) { $hits++; print "$path$what:$n:$line\n"; }
  }
}, \&failure);
failure("git ls-files listed no tracked file") unless $entries;
exit($failed ? 2 : $hits ? 1 : 0);
'
found=0
hits="$(perl -e "$scan" "$pattern" "$home" "$allowed")" || found=$?
[ "$found" -le 1 ] || fail "the tracked content could not all be read"
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
