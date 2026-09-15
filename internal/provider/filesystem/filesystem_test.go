package filesystem_test

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// TestFilesystemConform runs the shared conformance fixture over a polyglot
// repository: a nested source file, a README, a build file and a file made
// of one line longer than a chunk. Failure mode: a unit whose identities
// depended on discovery order, wall time or the sink's flush timing would
// publish different repository, directory or file IDs for the same bytes,
// so unchanged files could never reuse their units and a relation could
// reach storage before the node it references.
func TestFilesystemConform(t *testing.T) {
	files := map[string]string{
		"README.md":       "# App\n\nSee [main](cmd/app/main.go).\n",
		"cmd/app/main.go": "package main\n\nfunc main() {}\n",
		"Dockerfile":      "FROM scratch\n",
		"docs/long.txt":   strings.Repeat("é", 20000),
		"assets/logo.bin": pngBytes(2048),
		"empty.txt":       "",
	}
	p, err := filesystem.New(filesystem.Options{MaxSearchFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"README.md", "cmd/app/main.go", "Dockerfile", "docs/long.txt", "assets/logo.bin", "empty.txt"} {
		providertest.Conform(t, p, files, filesystem.ScopeKey(path), []string{path})
	}
}

// capturingSink records the search documents a unit publishes while still
// persisting them, so a test can assert what the lexical index of a file
// actually covers.
type capturingSink struct {
	provider.UnitOutput
	docs []model.SearchUnit
}

func (c *capturingSink) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	c.docs = append(c.docs, docs...)
	return c.UnitOutput.PutSearchUnits(ctx, docs)
}

// index runs one file through the provider and returns the search documents
// it published and the state of its search capability.
func index(t *testing.T, files map[string]string, path string) ([]model.SearchUnit, model.CapabilityState) {
	t.Helper()
	p, err := filesystem.New(filesystem.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := providertest.New(t, files)
	u := h.Plan(t, p, filesystem.ScopeKey(path), []string{path})
	sink := &capturingSink{UnitOutput: h.Begin(t, u, []string{path})}
	res, err := provider.RunUnit(context.Background(), p, u.Request, sink, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("RunUnit(%s): %v", path, err)
	}
	if res.State != model.RunSucceeded {
		t.Fatalf("run state for %s = %s, want succeeded", path, res.State)
	}
	for _, cs := range res.Capabilities {
		if cs.Capability == filesystem.CapabilitySearch {
			return sink.docs, cs
		}
	}
	t.Fatalf("no search capability state for %s", path)
	return nil, model.CapabilityState{}
}

// assertCovered fails unless the chunk documents together cover every byte of
// the file, each body is valid UTF-8 and each body is exactly as long as the
// byte range it claims -- the invariant that lets a body offset stay a file
// offset.
func assertCovered(t *testing.T, docs []model.SearchUnit, size uint64) {
	t.Helper()
	var ranges []model.ByteRange
	for _, d := range docs {
		if d.Bytes.End == d.Bytes.Start {
			continue // a name-only symbol document carries no range
		}
		if got, want := uint64(len(d.Body)), d.Bytes.End-d.Bytes.Start; got != want {
			t.Fatalf("chunk [%d,%d) body is %d bytes, want %d", d.Bytes.Start, d.Bytes.End, got, want)
		}
		if !utf8.ValidString(d.Body) {
			t.Fatalf("chunk [%d,%d) body is not valid UTF-8", d.Bytes.Start, d.Bytes.End)
		}
		ranges = append(ranges, d.Bytes)
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	covered := uint64(0)
	for _, r := range ranges {
		if r.Start > covered {
			t.Fatalf("bytes [%d,%d) of the file are in no search document", covered, r.Start)
		}
		if r.End > covered {
			covered = r.End
		}
	}
	if covered != size {
		t.Fatalf("search documents cover %d of %d bytes", covered, size)
	}
}

// TestSearchCoversEveryByte protects the no-skip rule on the lexical index: a
// file whose bytes are not UTF-8 throughout (here a Windows-1252 HTML document
// larger than one chunk, the shape found on a real repository) and a file that
// is one line far longer than a chunk are both indexed whole, with the search
// capability fresh and any substitution disclosed in its detail. Failure mode:
// the chunker used to drop every chunk that was not valid UTF-8 and mark the
// capability partial, so ~32 KiB of that document's text was searchable
// nowhere and the whole index read as degraded under default settings.
func TestSearchCoversEveryByte(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("<html><body>\n")
	for i := 0; doc.Len() < 40<<10; i++ {
		// \xa0 is a non-breaking space in Windows-1252 and is not valid
		// UTF-8; \x92 is that encoding's right single quote.
		doc.WriteString("<p>Risk\xa0Management section " + strconv.Itoa(i) + " doesn\x92t stop here.</p>\n")
	}
	doc.WriteString("</body></html>\n")
	// A newline-free file longer than one chunk whose bytes force the chunk
	// boundary off a rune boundary: an invalid byte early on (so a
	// validity-gated trim gives up), and at byte 32768 either a multi-byte
	// rune straddling the edge or a run of bare continuation bytes longer
	// than any well-formed sequence. Both used to end the chunk mid-sequence
	// and fail the next call with "offset 32768 is inside a UTF-8 sequence",
	// losing the whole unit.
	straddle := func(at []byte) string {
		raw := []byte(strings.Repeat("x", 70<<10))
		raw[100] = 0xa0 // invalid UTF-8, as in the document above
		copy(raw[filesystem.ChunkBytes-1:], at)
		return string(raw)
	}
	files := map[string]string{
		"policy.htm":       doc.String(),
		"minified.txt":     strings.Repeat("x", 5<<20),
		"rune.min.js":      straddle([]byte("\u20ac")),
		"contbytes.min.js": straddle([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}),
	}
	for _, tc := range []struct {
		path      string
		wantLossy bool
	}{
		{"policy.htm", true},
		{"minified.txt", false},
		{"rune.min.js", true},
		{"contbytes.min.js", true},
	} {
		docs, state := index(t, files, tc.path)
		if state.State != model.CapabilityFresh {
			t.Fatalf("%s: search capability = %s (%s), want fresh", tc.path, state.State, state.DiagnosticCode)
		}
		assertCovered(t, docs, uint64(len(files[tc.path])))
		if _, ok := state.Details["lossy_utf8_bytes"]; ok != tc.wantLossy {
			t.Fatalf("%s: lossy_utf8_bytes disclosed = %v, want %v (details %v)", tc.path, ok, tc.wantLossy, state.Details)
		}
	}
}

// pngBytes is a PNG header followed by deterministic pseudo-random bytes: a
// real binary file, not a text file with a NUL in it.
func pngBytes(n int) string {
	raw := []byte("\x89PNG\r\n\x1a\n")
	x := uint32(0x12345678)
	for len(raw) < n {
		x = x*1664525 + 1013904223
		raw = append(raw, byte(x>>24), byte(x>>16), byte(x>>8), byte(x))
	}
	return string(raw)
}

// TestBinaryDecisionIsAProportion protects the no-skip rule against the binary
// sniff: a text file is judged by how much of its head is not text at all, not
// by whether a single NUL appears in it. Failure mode: one stray NUL byte in a
// source file used to leave that file with no lexical index at all under
// default settings -- state unavailable, its whole content searchable nowhere
// -- while the file is plainly text a caller expects to find.
func TestBinaryDecisionIsAProportion(t *testing.T) {
	files := map[string]string{
		"src/config.go":  "package main\n\nconst banner = \"codectx\x00\"\n" + strings.Repeat("// filler line\n", 200),
		"assets/img.png": pngBytes(8192),
		"assets/utf16.txt": func() string {
			var b strings.Builder
			for i := 0; i < 2000; i++ {
				b.WriteString("a\x00")
			}
			return b.String()
		}(),
	}
	// One NUL in a text file: indexed whole, fresh, and the substitution
	// disclosed rather than silent.
	docs, state := index(t, files, "src/config.go")
	if state.State != model.CapabilityFresh {
		t.Fatalf("search capability for a text file with one NUL = %s (%s), want fresh", state.State, state.DiagnosticCode)
	}
	assertCovered(t, docs, uint64(len(files["src/config.go"])))
	if got := state.Details["nul_bytes"]; got != "1" {
		t.Fatalf("nul_bytes = %q, want \"1\" (details %v)", got, state.Details)
	}
	// Real binary content still has no lexical index, and says why.
	for _, path := range []string{"assets/img.png", "assets/utf16.txt"} {
		_, state := index(t, files, path)
		if state.State != model.CapabilityUnavailable || state.DiagnosticCode != model.CodeProviderUnavailable {
			t.Fatalf("%s: search capability = %s (%s), want unavailable/%s", path, state.State, state.DiagnosticCode, model.CodeProviderUnavailable)
		}
	}
}
