# The one reader of tracked content for tracked-refs-check.sh, loaded with
# `require`.
#
# tracked_blobs($visit, $failure) reads every index entry's blob, never the
# working tree: the index is what a commit records, and a working-tree read
# follows a tracked symlink to whatever it points at, so the link text itself
# was never checked. A symlink's blob IS its link text, so it is checked as
# text like any file. A file holding NUL bytes is handed over whole, never
# summarized as "binary".
#
# $visit is called as $visit->($path, $mode, $data) for every entry, in index
# order; $data is undef for a submodule (mode 160000), whose content is another
# repository's and not in this one's object store, and for a path the request
# stream cannot carry. Both are also reported to $failure, so a guard that
# could not check them never reports clean. $failure is called with a message
# for everything else that could not be read: git failing, a missing object, a
# short read. The return value is the number of index entries, so a caller can
# refuse an empty listing.
#
# The object requests go to one `git cat-file --batch` through a scratch file
# rather than a pipe, so the reader never holds a request back behind an
# unread answer; each request carries its entry's mode and path back in the
# `%(rest)` field.
use strict;
use warnings;
use File::Temp qw(tempfile);
use POSIX qw(_exit);

sub tracked_blobs {
  my ($visit, $failure) = @_;
  my ($req, $reqname) = tempfile(UNLINK => 1);
  binmode $req;
  open(my $ls, q(-|), qw(git ls-files -s -z)) or do { $failure->("cannot run git ls-files: $!"); return 0; };
  binmode $ls;
  my ($entries, $requested, $answered) = (0, 0, 0);
  {
    local $/ = "\0";
    while (my $rec = <$ls>) {
      chomp $rec;
      $entries++;
      my ($mode, $oid, $path) = $rec =~ /\A([0-7]{6}) ([0-9a-f]+) [0-3]\t(.+)\z/s
        or do { $failure->("git ls-files printed an entry this reader cannot parse"); next; };
      if ($mode eq q(160000)) {
        $failure->("$path: a submodule, whose content is not in this repository");
        $visit->($path, $mode, undef);
        next;
      }
      if ($path =~ /\n/) {
        $failure->("a tracked path holds a newline, which the object request cannot carry");
        $visit->($path, $mode, undef);
        next;
      }
      print {$req} "$oid $mode $path\n" or do { $failure->("cannot write the object requests: $!"); return $entries; };
      $requested++;
    }
  }
  close($ls) or $failure->("git ls-files failed" . ($! ? ": $!" : " with exit status " . ($? >> 8)));
  close($req) or do { $failure->("cannot write the object requests: $!"); return $entries; };

  my $pid = open(my $out, q(-|));
  defined $pid or do { $failure->("cannot start git cat-file: $!"); return $entries; };
  if (!$pid) {
    # _exit, not exit: the child must not run the parent's cleanup, which
    # would remove the request file.
    open(STDIN, q(<), $reqname) or _exit(2);
    exec(qw(git cat-file), q(--batch=%(objectname) %(objecttype) %(objectsize) %(rest))) or _exit(2);
  }
  binmode $out;
  local $/ = "\n";
  while (my $head = <$out>) {
    chomp $head;
    $answered++;
    my ($type, $size, $mode, $path) = $head =~ /\A[0-9a-f]+ (\S+) ([0-9]+) ([0-7]{6}) (.+)\z/s
      or do { $failure->("git cat-file could not read an object: $head"); next; };
    my $data = q();
    while (length($data) < $size) {
      my $n = read($out, $data, $size - length($data), length($data));
      defined $n && $n > 0 or do { $failure->("$path: git cat-file ended inside the object"); return $entries; };
    }
    my $nl;
    read($out, $nl, 1) == 1 && $nl eq "\n" or do { $failure->("$path: git cat-file framed the object wrongly"); return $entries; };
    if ($type ne q(blob)) { $failure->("$path: the index names a $type, not a blob"); next; }
    $visit->($path, $mode, $data);
  }
  close($out) or $failure->("git cat-file failed" . ($! ? ": $!" : " with exit status " . ($? >> 8)));
  $answered == $requested or $failure->("git cat-file answered $answered of $requested object requests");
  return $entries;
}

1;
