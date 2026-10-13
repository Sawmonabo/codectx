package textindex

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"testing"
)

// TestTheArchiveIDNamesTheLinkedArchive checks that archive_id.go names the
// SHA-256 of the archive this test binary linked. The go command keys its
// caches on Go sources, never on the archive's bytes, so a library rebuilt
// without the generate step that rewrites archive_id.go would leave cached
// results and binaries in place; this test fails instead, and the test cache
// reruns it whenever the archive file changes, since the test opens it.
//
// Mutation caught: a `cargo build` without the archive_id.go step after it (or
// a generate line that drops the step).
func TestTheArchiveIDNamesTheLinkedArchive(t *testing.T) {
	src, err := os.ReadFile("archive_id.go")
	if err != nil {
		t.Fatalf("archive_id.go: %v (run go generate ./internal/search/textindex)", err)
	}
	m := regexp.MustCompile(`var archiveSHA256 = "([0-9a-f]{64})"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("archive_id.go names no SHA-256:\n%s", src)
	}
	b, err := os.ReadFile("_build/release/libcodectx_textindex.a")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != string(m[1]) {
		t.Fatalf("archive_id.go names %s, the archive is %s: run go generate ./internal/search/textindex", m[1], got)
	}
}
