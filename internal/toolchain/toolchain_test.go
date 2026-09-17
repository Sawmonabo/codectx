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
	"net/url"
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

// gzipped wraps body in a bare gzip member: the single-file payload shape three
// pinned upstreams publish, which is a gzip stream that is not a tar.
func gzipped(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(body)); err != nil {
		t.Fatalf("gzip write: %v", err)
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
		if err == io.EOF {
			// The fixture carries no such member; a payload that never reaches
			// its entry check only needs a well-formed digest in the lock.
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
		MaxFetchBytes: 64 << 20,
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
// executed, or left in the store, and the request that fetched them must be the
// one the lock identifies. Silent breakage would let one bad response from an
// asset host become a tool the product runs forever.
//
// The mirror and redirect rows carry the second half of that. A mirror serves
// three publishers at once, so a substitution that dropped the original host
// would flatten three origin trees into one namespace and let one publisher's
// path answer for another's -- the digest would still be checked, but against
// bytes fetched from somewhere the lock never named. The redirect rows pin the
// delegate set to the host the lock names rather than the host actually dialed,
// which is what keeps a mirror from silently widening the host set.
func TestPayloadDisagreeingWithTheLockIsNeverInstalled(t *testing.T) {
	payload := goodPayload(t)
	t.Run("a mirrored URL still identifies its upstream host", func(t *testing.T) {
		for _, tc := range []struct{ mirror, raw, want, wantHost string }{
			{"", "https://github.com/o/r/releases/download/t/a.tar.gz",
				"https://github.com/o/r/releases/download/t/a.tar.gz", "github.com"},
			{"https://mirror.invalid/ctx", "https://nodejs.org/dist/v1/node.tar.gz",
				"https://mirror.invalid/ctx/nodejs.org/dist/v1/node.tar.gz", "nodejs.org"},
			{"https://mirror.invalid/ctx", "https://download.eclipse.org/jdtls/m/j.tar.gz",
				"https://mirror.invalid/ctx/download.eclipse.org/jdtls/m/j.tar.gz", "download.eclipse.org"},
			{"https://mirror.invalid", "https://github.com/o/r/releases/download/t/a.tar.gz",
				"https://mirror.invalid/github.com/o/r/releases/download/t/a.tar.gz", "github.com"},
		} {
			var mirror *url.URL
			if tc.mirror != "" {
				var err error
				if mirror, err = url.Parse(tc.mirror); err != nil {
					t.Fatalf("mirror %q: %v", tc.mirror, err)
				}
			}
			got, host, err := newFetcher(nil, mirror, 1<<30, nil).target(tc.raw)
			if err != nil {
				t.Fatalf("target(%q): %v", tc.raw, err)
			}
			if got.String() != tc.want || host != tc.wantHost {
				t.Fatalf("mirror %q: target(%q) = %q/%q, want %q/%q",
					tc.mirror, tc.raw, got, host, tc.want, tc.wantHost)
			}
		}
	})
	t.Run("a redirect may only reach the lock host's own delegates", func(t *testing.T) {
		for _, tc := range []struct {
			name, from, to, lockHost string
			refused                  bool
		}{
			{"same host", "https://github.com/a", "https://github.com/b", "github.com", false},
			{"github delegate", "https://github.com/a", "https://release-assets.githubusercontent.com/b", "github.com", false},
			{"github delegate behind a mirror", "https://mirror.invalid/ctx/github.com/a",
				"https://release-assets.githubusercontent.com/b", "github.com", false},
			{"the mirror's own host is not a delegate", "https://mirror.invalid/ctx/nodejs.org/a",
				"https://cdn.mirror.invalid/b", "nodejs.org", true},
			{"a delegate of a host the lock did not name", "https://mirror.invalid/ctx/nodejs.org/a",
				"https://release-assets.githubusercontent.com/b", "nodejs.org", true},
			{"leaving https", "https://github.com/a", "http://github.com/b", "github.com", true},
		} {
			via, err := http.NewRequestWithContext(withLockHost(t.Context(), tc.lockHost), http.MethodGet, tc.from, nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			next, err := http.NewRequestWithContext(via.Context(), http.MethodGet, tc.to, nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := checkRedirect(next, []*http.Request{via}) != nil; got != tc.refused {
				t.Fatalf("%s: refused = %v, want %v", tc.name, got, tc.refused)
			}
		}
	})
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
			if got := codeOf(t, err); got != model.CodeToolDigestMismatch {
				t.Fatalf("code = %q, want %q (err %v)", got, model.CodeToolDigestMismatch, err)
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
		// served overrides the tarball built from entries, for a payload shape
		// that is not an archive at all.
		served []byte
		code   string
	}{
		{"absolute path", []archiveEntry{{name: canary, body: "x"}}, nil, model.CodeToolCorrupt},
		{"parent traversal", []archiveEntry{{name: "../canary", body: "x"}}, nil, model.CodeToolCorrupt},
		{"unnormalized traversal", []archiveEntry{{name: "bin/../../canary", body: "x"}}, nil, model.CodeToolCorrupt},
		{"symlink leaving the payload", []archiveEntry{
			{name: "escape", typ: tar.TypeSymlink, link: "../../canary"},
		}, nil, model.CodeToolCorrupt},
		{"absolute symlink", []archiveEntry{
			{name: "escape", typ: tar.TypeSymlink, link: canary},
		}, nil, model.CodeToolCorrupt},
		{"hard link", []archiveEntry{
			{name: testEntry, body: "ok", mode: 0o755},
			{name: "alias", typ: tar.TypeLink, link: testEntry},
		}, nil, model.CodeToolCorrupt},
		{"device node", []archiveEntry{{name: "dev", typ: tar.TypeChar}}, nil, model.CodeToolCorrupt},
		{"forged publication marker", []archiveEntry{{name: completeName, body: "x"}}, nil, model.CodeToolCorrupt},
		{"expands past its bound", []archiveEntry{{name: "big", body: bomb}}, nil, model.CodeResourceLimit},
		// A lone gzip member carries no trustworthy decompressed length, so its
		// byte budget is charged while the bytes move rather than up front. That
		// is a second implementation of the bound the row above covers, and the
		// shape three pinned upstreams actually publish.
		{"a single-gzip payload expands past its bound", nil, gzipped(t, bomb), model.CodeResourceLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The lock pins the hostile bytes honestly, so the digest gate
			// passes and the extractor is what has to refuse them.
			served := tc.served
			if served == nil {
				served = tarball(t, tc.entries...)
			}
			f := newFixture(t, served)
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
			if got := codeOf(t, err); got != model.CodeToolOffline {
				t.Fatalf("code = %q, want %q (err %v)", got, model.CodeToolOffline, err)
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
	// The committed lock is this binary's trust root and Embedded panics on one
	// that will not parse or validate, by design. Every case below injects a
	// synthetic lock, so without this line the real file is read by no test at
	// all and a field the generator adds would surface as an init-time panic in
	// production rather than as a failure here.
	if embedded := Embedded(); embedded.LockVersion != lockVersion || len(embedded.Tools) == 0 {
		t.Fatalf("the embedded lock is version %d with %d tools", embedded.LockVersion, len(embedded.Tools))
	}
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
			// A consumer that plans work for a pinned-but-absent payload keys
			// its facts by PinnedFingerprint and the same facts by
			// Fingerprint once the payload lands. If the two ever disagree,
			// every unit sealed by the fetching run is silently re-keyed and
			// re-indexed by the next process.
			pinned, err := r.PinnedFingerprint(testTool)
			if err != nil || pinned != first.Fingerprint() {
				t.Fatalf("PinnedFingerprint = %q (%v), want the installed %q", pinned, err, first.Fingerprint())
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
			// Failure mode: the two digests `tools verify` prints are both
			// read from the lock, so the "installed" column repeats what this
			// binary pins and a damaged or absent store renders exactly like a
			// healthy one. The operator is then told the store was checked
			// when nothing on disk was ever hashed. The pinned digest is known
			// for every supported platform; the installed one exists only when
			// the bytes were read.
			if damaged[0].EntrySHA256 == "" {
				t.Fatalf("a corrupt entry must still report the digest the lock pins: %+v", damaged[0])
			}
			if damaged[0].InstalledSHA256 != "" {
				t.Fatalf("a corrupt entry reported an installed digest %q; nothing on disk hashed to it",
					damaged[0].InstalledSHA256)
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
			if repairedReport[0].InstalledSHA256 != repairedReport[0].EntrySHA256 {
				t.Fatalf("installed digest %q does not match the pinned %q after repair",
					repairedReport[0].InstalledSHA256, repairedReport[0].EntrySHA256)
			}
			got := r.Status(t.Context())
			if got[0].State != StateInstalled || got[0].Languages[0] != "go" {
				t.Fatalf("status after repair = %+v", got)
			}
			// Status does not rehash, so it must not claim a disk digest.
			if got[0].InstalledSHA256 != "" {
				t.Fatalf("status reported an installed digest %q without rehashing", got[0].InstalledSHA256)
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

// TestUnlistedStorePayloadIsReportedAndRemoved protects the store's boundary:
// every directory in it is an executable tree some run may pick up, and the
// only thing that vouches for one is a lock entry naming it. A superseded entry
// -- the replaced python server is the live case -- leaves its payload behind
// on upgrade. If `verify` walks only the lock, it calls that store clean while
// it holds an unvouched-for analyzer, and nothing short of `gc` ever reclaims
// the bytes.
func TestUnlistedStorePayloadIsReportedAndRemoved(t *testing.T) {
	payload := goodPayload(t)
	f := newFixture(t, payload)
	r := f.resolver(t, false, nil)
	if _, err := r.Resolve(t.Context(), testTool); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	stale := filepath.Join(StoreDir(f.dataDir), "pyright")
	mkdirAll(t, filepath.Join(stale, "1.1.414"))

	rows, err := r.Verify(t.Context())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var unlisted []Status
	for _, row := range rows {
		if row.State == StateUnlisted {
			unlisted = append(unlisted, row)
		}
	}
	if len(unlisted) != 1 || unlisted[0].Name != "pyright" {
		t.Fatalf("verify = %+v, want one unlisted row for pyright", rows)
	}
	// An unlisted directory is not a lock entry, so it must carry neither a
	// version nor a digest: a row that reported either would claim this binary
	// pins something it does not.
	if unlisted[0].Version != "" || unlisted[0].EntrySHA256 != "" || unlisted[0].InstalledSHA256 != "" {
		t.Fatalf("an unlisted row claimed lock data: %+v", unlisted[0])
	}

	if err := r.Prefetch(t.Context(), []string{testTool}); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	requireAbsent(t, stale, "the unlisted store payload")
	after, err := r.Verify(t.Context())
	if err != nil {
		t.Fatalf("verify after prefetch: %v", err)
	}
	if len(after) != 1 || after[0].State != StateInstalled {
		t.Fatalf("verify after prefetch = %+v, want only the installed lock entry", after)
	}
}

// pacedBody delivers one byte every beat until the payload is exhausted, then
// blocks until the request context ends. A total of stalls beats deliver
// nothing at all, which is what a wedged host looks like.
type pacedBody struct {
	ctx     context.Context
	payload []byte
	sent    int
	beat    time.Duration
	dead    bool
}

func (b *pacedBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(b.beat):
	}
	if b.dead || b.sent >= len(b.payload) {
		// Nothing more will ever arrive; only the caller's context ends this.
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	p[0] = b.payload[b.sent]
	b.sent++
	return 1, nil
}

func (b *pacedBody) Close() error { return nil }

// pacedTransport answers one GET with a pacedBody.
type pacedTransport struct {
	payload []byte
	beat    time.Duration
	dead    bool
}

func (t pacedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       &pacedBody{ctx: req.Context(), payload: t.payload, beat: t.beat, dead: t.dead},
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestAFetchIsEndedByStoppedBytesAndNotByElapsedTime protects the one thing
// that separates a hang detector from a deadline on the fetch path.
//
// Failure mode: a payload is as large as a language runtime and the link it
// arrives over is the operator's, so any clock over the whole download is a
// sustained-rate requirement. The 2 GiB ceiling under a ten-minute deadline
// demanded 3.4 MB/s, and a download progressing below that was terminated --
// repeatedly, since each retry restarts it -- while a host that accepted the
// connection and then delivered nothing held the attempt open for the whole
// ten minutes. Both outcomes are backwards.
//
// The slow transfer below moves one byte per stall quarter and takes several
// stall windows in total, so any deadline shorter than the whole of it fails
// this test; the dead one delivers nothing and must be ended at the window.
//
// Mutation: restore the deadline -- wrap the fetch context in
// context.WithTimeout(ctx, f.stall) in download and drop the watch in attempt
// -> the slow payload below is cancelled part way through and reported as a
// stream that ended early.
func TestAFetchIsEndedByStoppedBytesAndNotByElapsedTime(t *testing.T) {
	const stall = 200 * time.Millisecond
	payload := []byte("a tool payload delivered one byte at a time")
	sum := sha256.Sum256(payload)
	p := Payload{URL: "https://example.invalid/tool.bin", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	target, err := url.Parse(p.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	t.Run("a slow but moving transfer is never ended", func(t *testing.T) {
		// A byte every stall/16, well inside the detector's stall/4 poll: the
		// transfer must never look quiet for a whole window, and the whole of
		// it still spans several windows.
		f := newFetcher(pacedTransport{payload: payload, beat: stall / 16}, nil, 1<<30, nil)
		f.stall = stall
		dst := stagingFile(t)
		// The whole transfer takes about len(payload)/4 stall windows, which is
		// far longer than the window itself: only a detector that watches bytes
		// lets it finish.
		if err := f.attempt(context.Background(), "tool", target, p, dst); err != nil {
			t.Fatalf("a transfer delivering a byte every %s was ended: %v", stall/16, err)
		}
	})

	t.Run("a transfer that stops delivering is ended at the window", func(t *testing.T) {
		f := newFetcher(pacedTransport{payload: payload, beat: stall / 16, dead: true}, nil, 1<<30, nil)
		f.stall = stall
		dst := stagingFile(t)
		started := time.Now()
		err := f.attempt(context.Background(), "tool", target, p, dst)
		if err == nil {
			t.Fatal("a host that accepted the request and delivered no bytes was never detected")
		}
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != model.CodeToolFetchFailed || !typed.Retryable {
			t.Fatalf("a stalled transfer reported %v; want a retryable CTX_TOOL_FETCH_FAILED", err)
		}
		if !strings.Contains(typed.Message, "delivered no bytes") {
			t.Fatalf("the failure reads %q; a stalled transfer must not be reported as a stream that ended early", typed.Message)
		}
		// Ended by the window, not by a deadline over the whole download: well
		// under the ten minutes the deleted key allowed.
		if elapsed := time.Since(started); elapsed > 10*stall {
			t.Fatalf("a dead transfer was held open for %s, over the %s window", elapsed, stall)
		}
	})
}

// stagingFile is the staging file a download streams into.
func stagingFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatalf("staging file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
