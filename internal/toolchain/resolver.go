package toolchain

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Source says where a runnable tool came from.
type Source string

const (
	// SourceManaged is a payload the lock pinned and this package installed.
	SourceManaged Source = "managed"
	// SourceOverride is a user-configured executable verified against the
	// checksum the user declared for it.
	SourceOverride Source = "override"
)

// Tool is a runnable analyzer. Everything a caller needs to start it is here
// and nothing else is: the argv prefix is complete, the environment is the
// variables the child needs rather than anything inherited, and the checksum is
// what the executable hashed to at this resolution, not at install time.
type Tool struct {
	Name, Version string
	// Root is the store or override directory, absolute.
	Root string
	// Executable is absolute; for a runtime-dependent tool it is the runtime
	// binary, because that is the process that actually starts.
	Executable string
	// ArgvPrefix is the full launcher argv; ArgvPrefix[0] == Executable. It is
	// never empty and its first element is never the empty string: a resolution
	// that could not name a launcher fails with CTX_INTERNAL rather than handing
	// back a prefix a caller would have to check. Every consumer may index it.
	ArgvPrefix []string
	// Env carries the variables the child needs, such as JAVA_HOME. Nothing is
	// inherited: Section 21 requires an allowlisted environment.
	Env []string
	// Checksum is the lowercase hex SHA-256 of Executable at resolution time.
	Checksum string
	// EntryChecksum is the lowercase hex SHA-256 of this tool's own pinned
	// entry -- the binary, script or jar the lock names -- also observed at
	// resolution time. For a tool that runs as itself it equals Checksum; for a
	// runtime-dependent tool Checksum is the runtime binary and this is the
	// tool, which is what keeps two Node-hosted indexers from looking alike.
	EntryChecksum string
	// PayloadDigest is the lock's SHA-256 of the platform payload this tool was
	// installed from. It is empty for an override, where the user's checksum is
	// the only pinned digest there is.
	PayloadDigest string
	Source        Source
}

// fingerprintDomain separates this digest from every other canonical hash.
const fingerprintDomain = "tool-fingerprint-v1"

// Fingerprint is the stable identity of exactly these bytes: the tool, its
// version, the payload it came from and the executables actually hashed at this
// resolution. Section 20.2 keeps `[tools]` out of the analysis configuration
// hash, so this is what a consumer folds into UnitSpec.ProviderVersion and the
// LSP input digest to make a tool change invalidate the units it produced.
// It excludes paths and every operational value, so two machines that resolved
// the same pinned tool produce the same fingerprint.
func (t Tool) Fingerprint() string {
	return t.Name + "@" + t.Version + "+" +
		model.H(fingerprintDomain, string(t.Source), t.Name, t.Version, t.PayloadDigest, t.EntryChecksum, t.Checksum)
}

// Override is a user-configured replacement for a lock entry. It mirrors
// config.ToolOverride; the composition root adapts one to the other so this
// package does not depend on configuration parsing.
type Override struct {
	// Executable is an absolute path.
	Executable string
	// Version is the exact version the user asserts.
	Version string
	// Checksum is the required lowercase hex SHA-256 of Executable.
	Checksum string
}

// Options configure a resolver. Every bound is explicit: there is no default
// byte cap or timeout here, because a fetch with an implicit bound is an
// unbounded operation waiting for the day the configuration stops being read.
type Options struct {
	// DataDir is the data directory; the store is <data_dir>/tools, created
	// 0o700 by the first install. Required unless StoreDir names the store.
	DataDir string
	// StoreDir, when set, is the tool store itself, used verbatim. Otherwise the
	// store is StoreDir(DataDir), i.e. <data_dir>/tools. tools.cache_dir names a
	// store, not a parent of one, so the configuration key and the option agree.
	StoreDir string
	// Offline turns every fetch into a typed refusal without opening a socket.
	Offline bool
	// Mirror optionally replaces the scheme and host of every lock asset URL,
	// keeping the original host as the first path segment so one mirror serves
	// every publisher the lock names. It must be an absolute https URL.
	Mirror string
	// MaxFetchBytes caps one payload.
	MaxFetchBytes int64
	// FetchTimeout bounds one payload fetch, including every retry, and is also
	// how long a resolution waits for another process's install of the same
	// tool.
	FetchTimeout time.Duration
	// Overrides are the user's [tools.override.<name>] entries.
	Overrides map[string]Override
	// Log receives one record per completed fetch and nothing else.
	Log *slog.Logger
}

// State is what `codectx tools status` reports for one lock entry.
type State string

const (
	StateInstalled           State = "installed"
	StateAvailable           State = "available"
	StateUnsupportedPlatform State = "unsupported_platform"
	StateOverride            State = "override"
	StateCorrupt             State = "corrupt"
)

// Status is one line of the toolchain report. Detail is a short safe phrase; it
// never carries a path or a URL.
type Status struct {
	Name, Version string
	State         State
	Languages     []string
	Detail        string
}

// Resolver hands out runnable tools. It is safe for concurrent use: it holds no
// mutable state, and concurrent installs of the same tool -- in this process or
// another -- are serialized by the store's per-tool lock.
type Resolver struct {
	lock      Lock
	store     *store
	fetch     *fetcher
	platform  Platform
	offline   bool
	wait      time.Duration
	overrides map[string]Override
}

// New builds a resolver over the embedded lock.
func New(opts Options) (*Resolver, error) { return NewFromLock(Embedded(), opts) }

// NewFromLock builds a resolver over a lock the caller parsed with ParseLock.
// It exists for the release gate in internal/tools/toollock, which has to
// install payloads through this package from the lock file it is checking
// rather than from whichever lock happened to be compiled into the running
// binary: those are the same document only by coincidence of build order, and a
// gate that verifies the wrong one is not a gate. Product code calls New.
func NewFromLock(lock Lock, opts Options) (*Resolver, error) { return newResolver(lock, opts, nil) }

// newResolver is New with the lock and the HTTP transport injected. The tests
// use it to serve a synthetic lock from httptest and to prove that an offline
// resolution never dials; production always passes the embedded lock and the
// package's own transport.
func newResolver(lock Lock, opts Options, transport http.RoundTripper) (*Resolver, error) {
	if err := validateLock(lock); err != nil {
		return nil, err
	}
	storeDir := opts.StoreDir
	if storeDir == "" {
		if opts.DataDir == "" || !filepath.IsAbs(opts.DataDir) {
			return nil, invalid("the tool store needs an absolute data directory")
		}
		storeDir = StoreDir(opts.DataDir)
	} else if !filepath.IsAbs(storeDir) {
		return nil, invalid("the tool store directory must be an absolute path")
	}
	if opts.MaxFetchBytes <= 0 {
		return nil, invalid("tools.max_fetch_bytes must be positive")
	}
	if opts.FetchTimeout <= 0 {
		return nil, invalid("tools.fetch_timeout must be positive")
	}
	var mirror *url.URL
	if opts.Mirror != "" {
		u, err := url.Parse(opts.Mirror)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, invalid("tools.mirror must be an absolute https URL")
		}
		mirror = u
	}
	overrides := make(map[string]Override, len(opts.Overrides))
	for name, ov := range opts.Overrides {
		if _, ok := lock.Tools[name]; !ok {
			return nil, overrideInvalid(name, "no lock entry carries that name")
		}
		if err := validOverride(name, ov); err != nil {
			return nil, err
		}
		overrides[name] = ov
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Resolver{
		lock:      lock,
		store:     &store{dir: storeDir},
		fetch:     newFetcher(transport, mirror, opts.MaxFetchBytes, opts.FetchTimeout, log),
		platform:  Current(),
		offline:   opts.Offline,
		wait:      opts.FetchTimeout,
		overrides: overrides,
	}, nil
}

// StoreDir is the directory this resolver installs into and reports on. It is
// the one place that answers "which store is this", so a caller that prints the
// path -- `codectx tools` does -- cannot print a different one from the one the
// resolver uses.
func (r *Resolver) StoreDir() string { return r.store.dir }

// Resolve returns the runnable tool named by one lock entry, installing the
// pinned payload if it is not already in the store. Resolution order is
// Section 11.7's: a user override, then a store entry whose publication marker
// and entry digest both match the lock, then a fetch.
func (r *Resolver) Resolve(ctx context.Context, name string) (Tool, error) {
	e, ok := r.lock.Tools[name]
	if !ok {
		// Profiles name lock entries; a name the lock does not carry is a
		// product defect, not a user-correctable condition.
		return Tool{}, internalError("no lock entry is named %q", name)
	}
	if ov, ok := r.overrides[name]; ok {
		return resolveOverride(name, ov)
	}
	p, ok := e.Platforms[r.platform.Key()]
	if !ok {
		return Tool{}, unsupported(name, r.platform)
	}
	var runtime Tool
	if e.Runtime != "" {
		// One level deep and no further: validateLock rejects a runtime that
		// itself names a runtime, so this cannot recurse.
		var err error
		if runtime, err = r.Resolve(ctx, e.Runtime); err != nil {
			return Tool{}, err
		}
	}
	dir, entryHash, err := r.ensure(ctx, name, e, p)
	if err != nil {
		return Tool{}, err
	}
	return compose(name, e, p, dir, entryHash, runtime)
}

// ensure returns the installed payload directory and the entry digest observed
// at this resolution, installing the payload when the store cannot supply one.
func (r *Resolver) ensure(ctx context.Context, name string, e Entry, p Payload) (string, string, error) {
	dir := r.store.versionDir(name, e.Version)
	hash, state, _, err := r.inspect(name, e, p, true)
	if err != nil {
		return "", "", err
	}
	if state == StateInstalled {
		return dir, hash, nil
	}
	if r.offline {
		return "", "", offline(name)
	}
	held, err := r.store.acquire(ctx, name, r.wait)
	if err != nil {
		return "", "", err
	}
	defer held.release()

	// Another process may have installed it while this one waited for the lock.
	if hash, state, _, err = r.inspect(name, e, p, true); err != nil {
		return "", "", err
	}
	if state == StateInstalled {
		return dir, hash, nil
	}
	if err := r.store.install(ctx, r.fetch, name, e, p); err != nil {
		return "", "", err
	}
	hash, state, detail, err := r.inspect(name, e, p, true)
	if err != nil {
		return "", "", err
	}
	if state != StateInstalled {
		return "", "", corrupt(name, "a freshly installed payload is still not usable: %s", detail)
	}
	return dir, hash, nil
}

// inspect reports what the store holds for one lock entry. A directory without
// a marker, a marker that disagrees with the lock's payload digest, and an
// entry executable that is absent or hashes wrong are all "corrupt": the
// payload is invisible and a fresh install is the repair. rehash trades the
// cheap presence check the status report needs for the full entry digest every
// resolution performs.
func (r *Resolver) inspect(name string, e Entry, p Payload, rehash bool) (string, State, string, error) {
	dir := r.store.versionDir(name, e.Version)
	digest, err := r.store.marker(name, e.Version)
	if err != nil {
		return "", "", "", err
	}
	if digest == "" {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return "", StateAvailable, "not installed", nil
		} else if err != nil {
			return "", "", "", ioError("tool store stat", err)
		}
		return "", StateCorrupt, "an install left no publication marker", nil
	}
	if digest != p.SHA256 {
		return "", StateCorrupt, "the installed payload digest disagrees with the lock", nil
	}
	if !rehash {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(e.entryPath(p))))
		if err != nil || !info.Mode().IsRegular() {
			return "", StateCorrupt, "the installed payload has no entry executable", nil
		}
		return "", StateInstalled, "", nil
	}
	hash, err := hashEntry(name, dir, e.entryPath(p), e.entryDigest(p))
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeToolCorrupt {
			return "", StateCorrupt, typed.Message, nil
		}
		return "", "", "", err
	}
	return hash, StateInstalled, "", nil
}

// compose builds the launcher argv for one resolved payload. Section 11.7 gives
// three shapes and the store's own layout gives a fourth: a JDK-runtime tool
// whose entry is a launcher script rather than a jar, which is how the Joern
// distribution ships. That script reads JAVA_HOME (verified against the real
// Joern 4.0.627 launcher), so the managed JDK reaches it as an environment
// variable rather than as an argv element.
func compose(name string, e Entry, p Payload, dir, entryHash string, runtime Tool) (Tool, error) {
	entry := e.entryPath(p)
	entryPath := filepath.Join(dir, filepath.FromSlash(entry))
	t := Tool{Name: name, Version: e.Version, Root: dir, Source: SourceManaged,
		EntryChecksum: entryHash, PayloadDigest: p.SHA256}
	switch {
	case e.Runtime == "":
		t.Executable, t.ArgvPrefix, t.Checksum = entryPath, []string{entryPath}, entryHash
	case e.Runtime == "node":
		t.Executable, t.ArgvPrefix = runtime.Executable, []string{runtime.Executable, entryPath}
		t.Env, t.Checksum = runtime.Env, runtime.Checksum
	case strings.HasSuffix(entry, ".jar"):
		t.Executable = runtime.Executable
		t.ArgvPrefix = []string{runtime.Executable, "-jar", entryPath}
		t.Env, t.Checksum = javaEnv(runtime), runtime.Checksum
	default:
		t.Executable, t.ArgvPrefix, t.Checksum = entryPath, []string{entryPath}, entryHash
		t.Env = javaEnv(runtime)
	}
	// Tool.ArgvPrefix promises a runnable launcher, so the promise is kept here
	// rather than re-checked at each of the three call sites that index it. A
	// runtime-hosted shape whose runtime resolved without an executable is the
	// only way to reach this, and it is a product defect, not user input.
	if len(t.ArgvPrefix) == 0 || t.ArgvPrefix[0] == "" {
		return Tool{}, internalError("resolved tool %q has no launcher", name)
	}
	return t, nil
}

// javaEnv names the managed JDK for a child that looks it up itself. JAVA_HOME
// is derived from the runtime's own executable rather than from its payload
// root, so it is correct for both a payload whose entry is bin/java and the
// macOS layout whose entry is Contents/Home/bin/java.
func javaEnv(runtime Tool) []string {
	if runtime.Executable == "" {
		return nil
	}
	return []string{"JAVA_HOME=" + filepath.Dir(filepath.Dir(runtime.Executable))}
}

// resolveOverride verifies a user-configured executable on every run start, as
// Section 20.2 requires. An override replaces the binary and never the
// invocation: it is run directly, with no managed runtime composed around it,
// including for an entry whose pinned payload is a jar or a Node script. That
// makes an override of such a tool a directly executable launcher rather than
// the jar or the script itself, which is the constraint docs/configuration.md
// states on the surface a user actually reads. Composing the managed runtime
// around an override instead would put a binary the lock never described behind
// a runtime the lock pinned, and the override contract is that the user owns
// what starts.
func resolveOverride(name string, ov Override) (Tool, error) {
	if err := validOverride(name, ov); err != nil {
		return Tool{}, err
	}
	info, err := os.Lstat(ov.Executable)
	if errors.Is(err, fs.ErrNotExist) {
		return Tool{}, overrideInvalid(name, "the configured executable does not exist")
	}
	if err != nil {
		return Tool{}, overrideInvalid(name, "the configured executable cannot be inspected")
	}
	if !info.Mode().IsRegular() {
		return Tool{}, overrideInvalid(name, "the configured executable is not a regular file")
	}
	got, err := hashEntry(name, filepath.Dir(ov.Executable), filepath.Base(ov.Executable), "")
	if err != nil {
		return Tool{}, overrideInvalid(name, "the configured executable cannot be read")
	}
	if got != ov.Checksum {
		return Tool{}, overrideInvalid(name, "the configured executable does not hash to the configured checksum")
	}
	// ArgvPrefix's "never empty" guarantee holds here by construction: the
	// validOverride call above refuses an executable that is not an absolute
	// path, so the single element below is always a real one.
	return Tool{
		Name: name, Version: ov.Version, Root: filepath.Dir(ov.Executable),
		Executable: ov.Executable, ArgvPrefix: []string{ov.Executable},
		Checksum: got, EntryChecksum: got, Source: SourceOverride,
	}, nil
}

func validOverride(name string, ov Override) error {
	switch {
	case ov.Executable == "" || !filepath.IsAbs(ov.Executable):
		return overrideInvalid(name, "executable must be an absolute path")
	case strings.ContainsRune(ov.Executable, 0):
		return overrideInvalid(name, "executable path contains a NUL byte")
	case ov.Version == "":
		return overrideInvalid(name, "version must name the exact version installed")
	case !model.ValidHexID(ov.Checksum):
		return overrideInvalid(name, "checksum must be a lowercase hex SHA-256 digest")
	}
	return nil
}

// Status reports every lock entry in name order. It is the cheap report:
// presence and publication, no rehashing. Verify is the expensive one.
func (r *Resolver) Status(ctx context.Context) []Status {
	out, _ := r.report(ctx, false)
	return out
}

// Verify rehashes each installed entry executable against the lock, so a store
// a user has edited or a disk has damaged is reported as corrupt rather than
// discovered at the next run.
func (r *Resolver) Verify(ctx context.Context) ([]Status, error) {
	out, err := r.report(ctx, true)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, model.Canceled(ctxErr)
	}
	return out, err
}

// report builds the status list. A store that cannot be inspected at all is
// both reported as corrupt and returned as an error, so `tools verify` can
// distinguish "the payload is wrong" from "the data directory is unreadable".
func (r *Resolver) report(ctx context.Context, rehash bool) ([]Status, error) {
	names := r.lock.Names()
	out := make([]Status, 0, len(names))
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return out, model.Canceled(err)
		}
		e := r.lock.Tools[name]
		s := Status{Name: name, Version: e.Version, Languages: languagesOf(name, e)}
		switch ov, overridden := r.overrides[name]; {
		case overridden:
			s.State, s.Version, s.Detail = StateOverride, ov.Version, "a user override replaces the pinned payload"
		default:
			p, supported := e.Platforms[r.platform.Key()]
			if !supported {
				s.State = StateUnsupportedPlatform
				s.Detail = "the lock carries no payload for " + r.platform.Key()
				break
			}
			_, state, detail, err := r.inspect(name, e, p, rehash)
			s.State, s.Detail = state, detail
			if err != nil {
				s.State, s.Detail = StateCorrupt, "the store could not be inspected"
				errs = append(errs, err)
			}
		}
		out = append(out, s)
	}
	return out, errors.Join(errs...)
}

// Prefetch installs ahead of time. An empty list means every lock entry this
// platform supports. A tool the lock does not carry for this platform is
// skipped: Section 11.7 makes that honest absence, never a failure.
func (r *Resolver) Prefetch(ctx context.Context, names []string) error {
	if len(names) == 0 {
		names = r.lock.Names()
	} else {
		names = append([]string(nil), names...)
		sort.Strings(names)
	}
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		_, err := r.Resolve(ctx, name)
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeToolUnsupportedPlatform {
			continue
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// GC removes every store directory the current lock does not name, plus
// unpublished and abandoned ones. It is what makes a binary upgrade reclaim the
// previous release's payloads.
func (r *Resolver) GC(ctx context.Context) (int, error) { return r.store.gc(ctx, r.lock) }

func offline(name string) *model.Error {
	return (&model.Error{Code: model.CodeToolOffline,
		Message:     "the managed tool is not installed in a verified state and tools.offline forbids fetching it",
		Remediation: "run `codectx tools prefetch` with network access, use the offline bundle, or clear tools.offline"}).
		WithDetail("tool", name)
}

func unsupported(name string, p Platform) *model.Error {
	return (&model.Error{Code: model.CodeToolUnsupportedPlatform,
		Message:     "the managed tool has no pinned payload for this platform",
		Remediation: "this language runs at the precision its available providers give on " + p.Key()}).
		WithDetail("tool", name).WithDetail("platform", p.Key())
}

func overrideInvalid(name, detail string) *model.Error {
	return (&model.Error{Code: model.CodeToolOverrideInvalid,
		Message:     "the configured tool override is not usable: " + detail,
		Remediation: "correct [tools.override." + name + "] in the user configuration, or remove it to use the pinned payload"}).
		WithDetail("tool", name)
}
