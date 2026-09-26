#!/usr/bin/env sh
# Nothing under version control may name a private repository the product was
# measured on: not its name, not a path, class or identifier taken from it.
# Corpora are named by class instead ("an unconfigured JavaScript-majority
# repository with its dependencies committed"), and public corpora keep their
# names.
#
# The list below holds only the SHA-256 of each forbidden token, never the
# token, so this file names nothing it guards. Every tracked file's path and
# content is split into tokens and each token is hashed:
#   * a token is a maximal run of letters, digits, `-` and `_`;
#   * each run is also split on `-`/`_`, and every contiguous sequence of its
#     parts is checked joined by `-` (so `a_b`, `a-b` and `x-a-b-y` all yield
#     `a-b`);
#   * each part is also split at camelCase boundaries (`FooBar` yields `foo`
#     and `bar`);
#   * everything is lowercased before hashing.
# A binary file's printable runs of four or more characters are checked the
# same way. A hit is reported as file and line (or commit and message line)
# with the hash prefix only, so the report names nothing either.
#
# To add a token: printf '%s' '<lowercase token>' | sha256sum, and append the
# hash here. Never commit the token itself.
#
# Usage:
#   sh scripts/private-names-check.sh                      tracked paths and content
#   sh scripts/private-names-check.sh --messages <range>   commit messages in <range>
# Exit 1 lists every hit. The check fails closed: when git cannot list the
# tracked files or the commits, when it lists no tracked file, when a tracked
# file cannot be read, or when the scanner cannot run, it exits 2 with the
# reason and never reports clean.
set -eu
cd "$(dirname "$0")/.."

hashes='
01f73c4f1f85baf1b6b61909aaf8adee7bf96911d170486facdeaab2bd5c7d1f
024b7550bd62e6a4e12ad7e506022bfbbc62f6dbddb9ea0eea7ec64c4983eb7f
05838e3655a4a94307f02c663e8ad9fb285867be3d3f7570902526683eabd88e
09b375ad5d712887a5ff068ddab85c2891d6ed4e537e24c0a2d0ee5c72b8c4f7
1fbd0e9cfa108d0edf1ad334527d76e142e1bd7c621376b15a2fe8b3af222ef0
264f6ad2195a0011191601330de89e7a6ddb7daa00d6a97d49745f03b4a907c0
2899b48cc5049d7a60745350f355ebdc1a093adbfa599dbc9bdb418496e1811e
2a87fb36539cf37b688fd2084a4277e3cf8e1c67aec97e1865226867081e64e3
2c2f2bfc0753f1ea7e4460c2b620aa509b8fb1f52c7a1b509c8ead46c41d3d40
319a4e495aa744abc8c8afcdc62fc0f8e618746f2abced30d482e6576ec5cb5a
39cc3013eaed0aeee3eb447c7fe84c07bc9310c96b1c3c17e702059efd3128fd
3aed69afb3da4ca1d138a21cd35992d95fa4a88eb73239990100b91dd97fa7f8
3d55fbffa6a1ce1381d7b8789edae994e2b1ab3f41c1b687a03715223ac68668
4b876d8835ef71d69b242cb4899a2bb7d0033c4bc79c68613ff7af0fd64230ad
67d55f34b8b804346faefe2c34197dd3a6cfed2be92476c733431588646fce1d
67e7a00defafec49d95c12fc3a17888263fe9470b3765dfe853b247844520028
7d84202ec06371b4911d9d8d0c2ded75b745af8139ce18d928f1ef2c677c70a1
7e398ef2e5c888c687f172900800950fd8165c93d1506f452849add33ef8c2a1
83886e8b2ab7434c9915b15f452cdbff09a58960d7d5f3879c1d97713bc49329
8ab599c6d8bab2336a14e787e5c76df7e34268d86f75a505392f965c86232132
8cb060d190100f7b61629681b746580c39933399375b52e940b64e8df2f6735a
8ff008a291492658eaeef90b822e17875e0e552442a001bf55427089db7bafc2
9166b4df11754155c004ff64d795e0a9c071f7ae99b535ec8951d141f89e0aba
adda55ba21a272200c84c9440f08a9f552dd780814aa00666342e565575896e5
b3817b7b219275cdd12d3371a1b88e3d410d8ca9ca485210da4da09d3f93e8a7
b696ec19b1e95356f45db8ec4736266d08cca80b6e9bc9db6dc4155b070f7376
c2bd4d80632dd731b32b8f45f0fe4c1c70bd50b8442983cb02bc4ce4db37e0e6
c8cfe6ae87210fc5eac9b3548a1d4081686dae0deb832b1d20985f0313d0375d
de6f7047330aad6a0a5d893289f929fdc7e24ef92a7fa22f31b2a0b7804410ab
e412ee94d2457b7841cc5f7dfd152bd259a465b7649e75a2d652d043ff9e92c6
e49d63b2a8a78f048bafc4b4590029603a5a4165ee8bf98af15d62f24cd83479
fcea588a704287278eb5c63a328ad401d647f09b4cf328192dab65fbe583c33c
'

# The scanner runs the git command it is given (every argument after the hash
# list) and reads its NUL-separated records: in files mode each record is a
# tracked path whose name and content are checked; in messages mode each record
# is a commit id, a newline, and that commit's message. It exits 0 when clean,
# 1 when it found a hit, and 2 when it could not check everything: git failed
# or listed no tracked file, or a tracked file could not be read.
scan='
use strict; use warnings; use Digest::SHA qw(sha256_hex);
my ($mode, $list, @git) = @ARGV;
my $failed = 0;
sub failure { print STDERR "private-names-check: $_[0]\n"; $failed++; }
my %bad = map { $_ => 1 } grep { length } split /\s+/, $list;
my %seen; my $hits = 0;
sub hit {
  my ($t) = @_; my $l = lc $t;
  # A name with digits glued on (a numbered copy of a checkout) is checked with them removed as well.
  (my $bare = $l) =~ s/[0-9]+$//;
  return $seen{$l} //= (($bad{sha256_hex($l)} || ($bare ne $l && length($bare) >= 4 && $bad{sha256_hex($bare)}))
    ? substr(sha256_hex($l), 0, 12) : q());
}
sub check {
  my ($where, $text) = @_;
  for my $run (split /[^A-Za-z0-9_-]+/, $text) {
    my @parts = grep { length } split /[-_]+/, $run;
    my @found;
    for my $i (0 .. $#parts) {
      for my $j ($i .. $#parts) { my $h = hit(join q(-), @parts[$i .. $j]); push @found, $h if $h; }
      for my $c (split /(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])/, $parts[$i]) { my $h = hit($c); push @found, $h if $h; }
    }
    if (@found) { $hits++; print "$where: private token (sha256 $found[0])\n"; return; }
  }
}
open(my $in, q(-|), @git) or do { failure("cannot run @git: $!"); exit 2; };
local $/ = "\0";
my $records = 0;
while (my $rec = <$in>) {
  chomp $rec;
  $records++;
  if ($mode eq q(messages)) {
    my ($id, $body) = split /\n/, $rec, 2;
    my $n = 0;
    for my $line (split /\n/, $body // q()) { $n++; check("commit $id message line $n", $line); }
    next;
  }
  check("$rec (file name)", $rec);
  my $fh;
  unless (open($fh, q(<:raw), $rec)) { failure("$rec: cannot open: $!"); next; }
  my $data = do { local $/; <$fh> };
  unless (defined $data) { failure("$rec: cannot read: $!"); close $fh; next; }
  close $fh;
  if (index(substr($data, 0, 8000), "\0") >= 0) {
    while ($data =~ /([\x20-\x7e]{4,})/g) { check("$rec (binary, byte " . ($-[1]) . ")", $1); }
    next;
  }
  my $n = 0;
  for my $line (split /\n/, $data) { $n++; check("$rec:$n", $line); }
}
close($in) or failure("@git failed" . ($! ? ": $!" : " with exit status " . ($? >> 8)));
failure("@git listed no tracked file") if $mode eq q(files) && !$records;
exit($failed ? 2 : $hits ? 1 : 0);
'

# verdict turns the scanner's exit status into the guard's: clean, the hits
# already listed, or a check that did not complete, which is never clean.
verdict() {
  case "$1" in
  0) echo "private-names-check: ${2:+$2 }clean" ;;
  1) exit 1 ;;
  *)
    echo "private-names-check: ${2:-tracked files} NOT checked (exit $1)" >&2
    exit 2
    ;;
  esac
}

status=0
if [ "${1:-}" = "--messages" ]; then
  range="${2:?usage: private-names-check.sh --messages <rev-range>}"
  perl -e "$scan" messages "$hashes" git log -z --format='%H%n%B' "$range" || status=$?
  verdict "$status" "commit messages in $range"
  exit 0
fi

perl -e "$scan" files "$hashes" git ls-files -z || status=$?
verdict "$status" ""
