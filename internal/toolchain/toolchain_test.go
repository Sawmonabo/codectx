package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The four tests in this file are the critical invariants of Task 22 Step 1.
// Each protects a failure that would otherwise be silent: unverified bytes
// becoming a permanent tool, a hostile archive writing outside the store, an
// "offline" resolution that still dials, and a half-written or edited store
// being executed as if the lock had approved it. Every payload is served from
// httptest; nothing here downloads anything.

const (
	testTool    = "scip-go"
	testVersion = "0.2.7"
	testEntry   = "bin/tool"
)

// archiveEntry is one member of a test tarball.
type archiveEntry struct {
	name string
	body string
	typ  byte
	mode int64
	link string
}

func tarball(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: e.mode, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.name, err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// goodPayload is the two-file tarball the brief calls for: one executable entry
// and one data file.
func goodPayload(t *testing.T) []byte {
	t.Helper()
	return tarball(t,
		archiveEntry{name: testEntry, body: "#!/bin/sh\necho managed\n", mode: 0o755},
		archiveEntry{name: "README", body: "pinned payload\n", mode: 0o644},
	)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// entryDigest hashes one archive member, or reports an unreachable digest when
// the fixture has no such member: a hostile archive is refused long before its
// entry is looked for, so the pinned value only has to be well-formed.
func entryDigest(t *testing.T, archive []byte, name string) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return strings.Repeat("0", 64)
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Name != name {
			continue
		}
		sum := sha256.New()
		if _, err := io.Copy(sum, tr); err != nil {
			t.Fatalf("entry hash: %v", err)
		}
		return hex.EncodeToString(sum.Sum(nil))
	}
}

// fixture is one payload served from httptest, the lock that pins it, and the
// data directory a resolver installs it into.
type fixture struct {
	lock      Lock
	dataDir   string
	hits      *atomic.Int64
	transport http.RoundTripper
}

const payloadURL = "https://assets.invalid/" + testTool + "-" + testVersion + ".tar.gz"

// handlerTransport runs the fixture handler through httptest's recorder instead
// of over a socket. Everything fetch.go does to a response -- status handling,
// the redirect and byte bounds, the streaming hash -- runs unchanged, and the
// fixture cannot be decided by whether a loopback listener happened to become
// connectable. That the offline path opens no socket at all is proved
// separately, by a real http.Transport whose DialContext fails the test.
type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newFixture(t *testing.T, served []byte) *fixture {
	t.Helper()
	hits := &atomic.Int64{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(served)
	})
	return &fixture{
		lock: Lock{LockVersion: lockVersion, Tools: map[string]Entry{testTool: {
			Name: testTool, Version: testVersion, Kind: "indexer", License: "Apache-2.0",
			Upstream: "https://example.invalid/scip-go", Entry: testEntry,
			EntrySHA256: entryDigest(t, served, testEntry),
			Platforms: map[string]Payload{Current().Key(): {
				URL: payloadURL, SHA256: digestOf(served), Size: int64(len(served)),
			}},
		}}},
		dataDir:   t.TempDir(),
		hits:      hits,
		transport: handlerTransport{handler: handler},
	}
}

// pin rewrites the payload the lock claims to expect.
func (f *fixture) pin(mangle func(*Payload)) {
	e := f.lock.Tools[testTool]
	p := e.Platforms[Current().Key()]
	mangle(&p)
	e.Platforms[Current().Key()] = p
	f.lock.Tools[testTool] = e
}

func (f *fixture) resolver(t *testing.T, offline bool, transport http.RoundTripper) *Resolver {
	t.Helper()
	if transport == nil {
		transport = f.transport
	}
	r, err := newResolver(f.lock, Options{
		DataDir: f.dataDir, Offline: offline,
		MaxFetchBytes: 64 << 20, FetchTimeout: 10 * time.Second,
	}, transport)
	if err != nil {
		t.Fatalf("newResolver: %v", err)
	}
	return r
}

func (f *fixture) versionDir() string {
	return filepath.Join(StoreDir(f.dataDir), testTool, testVersion)
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Fatalf("error is not typed: %v", err)
	}
	return typed.Code
}

func requireAbsent(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s still exists after a refused install (%v)", what, err)
	}
}

// stagingEmpty asserts that no staged tree survived a refusal, so a rejected
// payload can never be picked up again out of a poisoned cache.
func stagingEmpty(t *testing.T, dataDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(StoreDir(dataDir), stagingDirName, testTool))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging listing: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging kept %d trees after a refused install", len(entries))
	}
}

// TestPayloadDisagreeingWithTheLockIsNeverInstalled protects the trust root:
// bytes whose size or SHA-256 differ from the lock must never be extracted,
// executed, or left in the store. Silent breakage would let one bad response
// from an asset host become a tool the product runs forever.
func TestPayloadDisagreeingWithTheLockIsNeverInstalled(t *testing.T) {
	payload := goodPayload(t)
	for _, tc := range []struct {
		name   string
		mangle func(*Payload)
	}{
		{"digest disagrees", func(p *Payload) { p.SHA256 = strings.Repeat("b", 64) }},
		{"declared size is short", func(p *Payload) { p.Size-- }},
		{"declared size is long", func(p *Payload) { p.Size++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, payload)
			f.pin(tc.mangle)
			r := f.resolver(t, false, nil)

			_, err := r.Resolve(t.Context(), testTool)
			if got := codeOf(t, err); got != CodeToolDigestMismatch {
				t.Fatalf("code = %q, want %q (err %v)", got, CodeToolDigestMismatch, err)
			}
			requireAbsent(t, f.versionDir(), "the store version directory")
			stagingEmpty(t, f.dataDir)
			if f.hits.Load() != 1 {
				t.Fatalf("a digest mismatch was retried: %d fetches, want 1", f.hits.Load())
			}
		})
	}
}

// TestHostileArchiveIsRejectedBeforeAnyByteEscapes protects the extraction
// boundary: an archive whose bytes match the lock is still untrusted content.
// An absolute path, a "..", a symlink leaving the payload, a hard link, a
// device node, a forged publication marker or a decompression bomb must be
// refused, and nothing may be written outside the staging tree. Silent breakage
// here is an arbitrary file write running as the user.
func TestHostileArchiveIsRejectedBeforeAnyByteEscapes(t *testing.T) {
	canaryDir := t.TempDir()
	canary := filepath.Join(canaryDir, "canary")
	bomb := strings.Repeat("\x00", 24<<20)
	for _, tc := range []struct {
		name    string
		entries []archiveEntry
		code    string
	}{
		{"absolute path", []archiveEntry{{name: canary, body: "x"}}, CodeToolCorrupt},
		{"parent traversal", []archiveEntry{{name: "../canary", body: "x"}}, CodeToolCorrupt},
		{"unnormalized traversal", []archiveEntry{{name: "bin/../../canary", body: "x"}}, CodeToolCorrupt},
		{"symlink leaving the payload", []archiveEntry{
			{name: "escape", typ: tar.TypeSymlink, link: "../../canary"},
		}, CodeToolCorrupt},
		{"absolute symlink", []archiveEntry{
			{name: "escape", typ: tar.TypeSymlink, link: canary},
		}, CodeToolCorrupt},
		{"hard link", []archiveEntry{
			{name: testEntry, body: "ok", mode: 0o755},
			{name: "alias", typ: tar.TypeLink, link: testEntry},
		}, CodeToolCorrupt},
		{"device node", []archiveEntry{{name: "dev", typ: tar.TypeChar}}, CodeToolCorrupt},
		{"forged publication marker", []archiveEntry{{name: completeName, body: "x"}}, CodeToolCorrupt},
		{"expands past its bound", []archiveEntry{{name: "big", body: bomb}}, model.CodeResourceLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The lock pins the hostile bytes honestly, so the digest gate
			// passes and the extractor is what has to refuse them.
			f := newFixture(t, tarball(t, tc.entries...))
			r := f.resolver(t, false, nil)

			_, err := r.Resolve(t.Context(), testTool)
			if got := codeOf(t, err); got != tc.code {
				t.Fatalf("code = %q, want %q (err %v)", got, tc.code, err)
			}
			requireAbsent(t, canary, "a file outside the store")
			requireAbsent(t, f.versionDir(), "the store version directory")
			stagingEmpty(t, f.dataDir)
		})
	}
}

// TestOfflineRefusesWithoutDialing protects the offline contract: tools.offline
// promises that no socket is opened, not merely that a download fails. The
// transport below fails the test if anything dials, so a refusal that still
// reached the network cannot pass quietly.
func TestOfflineRefusesWithoutDialing(t *testing.T) {
	payload := goodPayload(t)
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, f *fixture)
	}{
		{"nothing installed", func(*testing.T, *fixture) {}},
		{"install left no marker", func(t *testing.T, f *fixture) {
			mkdirAll(t, f.versionDir())
		}},
		{"marker names another payload", func(t *testing.T, f *fixture) {
			mkdirAll(t, f.versionDir())
			writeFile(t, filepath.Join(f.versionDir(), completeName), strings.Repeat("c", 64)+"\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, payload)
			tc.prepare(t, f)
			trap := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				t.Error("an offline resolution opened a connection")
				return nil, errors.New("dialing is forbidden in this test")
			}}
			r := f.resolver(t, true, trap)

			_, err := r.Resolve(t.Context(), testTool)
			if got := codeOf(t, err); got != CodeToolOffline {
				t.Fatalf("code = %q, want %q (err %v)", got, CodeToolOffline, err)
			}
			if f.hits.Load() != 0 {
				t.Fatalf("an offline resolution reached the asset host %d times", f.hits.Load())
			}
		})
	}
}

// TestUnpublishedOrAlteredStoreIsInvisibleAndRepaired protects reuse safety: a
// store directory is bytes on a disk the product does not own. Without the
// publication marker and the entry re-hash at every resolution, a half-finished
// install or an edited binary would be executed as if the lock had approved it.
func TestUnpublishedOrAlteredStoreIsInvisibleAndRepaired(t *testing.T) {
	payload := goodPayload(t)
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, f *fixture)
	}{
		{"marker removed", func(t *testing.T, f *fixture) {
			removeFile(t, filepath.Join(f.versionDir(), completeName))
		}},
		{"marker names another payload", func(t *testing.T, f *fixture) {
			replaceFile(t, filepath.Join(f.versionDir(), completeName), strings.Repeat("d", 64)+"\n")
		}},
		{"entry executable altered", func(t *testing.T, f *fixture) {
			replaceFile(t, filepath.Join(f.versionDir(), filepath.FromSlash(testEntry)), "#!/bin/sh\nrm -rf /\n")
		}},
		{"entry executable removed", func(t *testing.T, f *fixture) {
			removeFile(t, filepath.Join(f.versionDir(), filepath.FromSlash(testEntry)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, payload)
			r := f.resolver(t, false, nil)
			first, err := r.Resolve(t.Context(), testTool)
			if err != nil {
				t.Fatalf("first resolve: %v", err)
			}
			if f.hits.Load() != 1 {
				t.Fatalf("first resolve fetched %d times, want 1", f.hits.Load())
			}

			tc.damage(t, f)
			// Verify is the report that rehashes, so it is the one that must
			// see an edited entry executable rather than only a missing marker.
			damaged, err := r.Verify(t.Context())
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if len(damaged) != 1 || damaged[0].State != StateCorrupt {
				t.Fatalf("verify = %+v, want one corrupt entry", damaged)
			}

			repaired, err := r.Resolve(t.Context(), testTool)
			if err != nil {
				t.Fatalf("repairing resolve: %v", err)
			}
			if f.hits.Load() != 2 {
				t.Fatalf("a damaged store was reused: %d fetches, want 2", f.hits.Load())
			}
			if repaired.Checksum != first.Checksum || repaired.Fingerprint() != first.Fingerprint() {
				t.Fatalf("repaired tool differs from the original: %+v vs %+v", repaired, first)
			}
			repairedReport, err := r.Verify(t.Context())
			if err != nil {
				t.Fatalf("verify after repair: %v", err)
			}
			if repairedReport[0].State != StateInstalled {
				t.Fatalf("state after repair = %q, want %q", repairedReport[0].State, StateInstalled)
			}
			if got := r.Status(t.Context()); got[0].State != StateInstalled || got[0].Languages[0] != "go" {
				t.Fatalf("status after repair = %+v", got)
			}
		})
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// replaceFile overwrites a file that may be read-only, which the publication
// marker is.
func replaceFile(t *testing.T, path, body string) {
	t.Helper()
	removeFile(t, path)
	writeFile(t, path, body)
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
}
