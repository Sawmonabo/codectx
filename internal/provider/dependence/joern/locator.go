package joern

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// lockName is the entry of the embedded tool lock this backend runs. The
// engine's name appears on no product surface; it appears here because this is
// the one package that knows which engine produces the facts.
const lockName = "joern"

// runtimeLockName is the managed JDK the engine executes under. The lock says
// so too (the engine entry's `runtime`), and resolving the engine resolves it;
// naming it here is what lets the locator report the runtime's identity as
// part of the unit's semantic closure.
const runtimeLockName = "jdk"

// parseEntry and exportEntry are the two noninteractive tools of the engine's
// own distribution. The lock pins the first as the entry it verifies; the
// second lives beside it in the same payload.
const (
	parseEntry  = "joern-parse"
	exportEntry = "joern-export"
)

// Locator resolves the engine payload through the managed toolchain. It is the
// one type that knows which entry of the lock the engine is; the neutral
// dependence package never learns it.
type Locator struct {
	resolver *toolchain.Resolver
}

// NewLocator binds a resolver.
func NewLocator(resolver *toolchain.Resolver) (*Locator, error) {
	if resolver == nil {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the analysis engine locator needs the managed toolchain resolver"}
	}
	return &Locator{resolver: resolver}, nil
}

// LocateInstalled describes the engine without installing anything. It is the
// construction-time half of the locator: the payload is reported only when the
// store already holds it in a verified state, and a payload the lock pins but
// the store does not hold is reported as absent rather than fetched.
//
// This is what keeps Section 11.6's guarantee that the provider never delays
// base readiness. Resolving the payload here fetched roughly two gigabytes
// inside every OpenWorkspace, in every repository, before the snapshot was even
// captured and whether or not the planner would emit a dependence unit at all.
// The first unit that actually needs the engine pays that cost instead, at unit
// time, under the scheduler's reservation gate -- the same shape
// internal/provider/scip uses for a deferred indexer kind.
//
// The second result is "the store holds it". false with a nil error is honest
// absence, and the Engine returned then carries the payload's *pinned*
// identity: the version the lock names and the fingerprint the payload will
// have once it is installed. Section 11.6 folds Digest and RuntimeDigest into
// the graph cache key and the provider descriptor's version, so a unit sealed
// after the payload landed during this process must key exactly as one sealed
// by a process that started with it already installed. PinnedFingerprint is
// computed from the lock alone and is that same string, which is what makes the
// two indistinguishable.
//
// Every other condition -- an unsupported platform, a corrupt store entry, an
// invalid override, a resolver the user configured offline -- stays a typed
// error, because none of them is repaired by running a unit later.
func (l *Locator) LocateInstalled(ctx context.Context) (dependence.Engine, bool, error) {
	t, installed, err := l.resolver.ResolveInstalled(ctx, lockName)
	if err != nil {
		return dependence.Engine{}, false, err
	}
	if installed {
		e, err := l.describe(ctx, t)
		if err != nil {
			return dependence.Engine{}, false, err
		}
		return e, true, nil
	}
	digest, err := l.resolver.PinnedFingerprint(lockName)
	if err != nil {
		return dependence.Engine{}, false, err
	}
	// The version comes from the same embedded lock the resolver was built
	// over: it is the one field of the identity that PinnedFingerprint folds
	// but does not return, and reading it here costs no store access. It
	// reaches `status` and the ledger through Detection.ObservedVersion, so an
	// uninstalled payload reports the release it will run rather than nothing.
	e := dependence.Engine{Version: toolchain.Embedded().Tools[lockName].Version, Digest: digest}
	// A runtime whose identity cannot be pinned is not fatal for the same
	// reason Locate's runtime resolution is not: the engine runs either way and
	// the closure then records no runtime, which is honest rather than wrong.
	if rt, err := l.resolver.PinnedFingerprint(runtimeLockName); err == nil {
		e.RuntimeDigest = rt
	}
	return e, false, nil
}

// Locate installs and verifies the pinned payload and describes how to start
// its two tools.
//
// Both argv arrays are complete launcher prefixes; internal/toolchain
// guarantees that a resolved tool's ArgvPrefix is never empty, so this does not
// re-check it. The pinned payload's entry
// is a launcher script that finds the managed JDK through JAVA_HOME, which the
// resolved tool already carries in its environment, so the parse argv is the
// resolver's own prefix unchanged. The export tool is not the lock's entry and
// therefore is not covered by the entry digest the resolver re-hashes at every
// resolution: it is only inside the payload digest, which was checked once at
// install. It is therefore checked here for presence and shape, so a payload
// damaged after installation is a typed refusal rather than an exec failure in
// the middle of a unit.
//
// Digest is the resolved tool's fingerprint rather than the bare payload
// digest. The fingerprint commits to the payload digest and additionally to
// the executables hashed at this resolution, and — decisively — it is never
// empty: an override's payload digest is empty by construction, and an empty
// Digest makes the backend refuse the engine as incomplete. Section 11.6 folds
// this value into the graph cache key and the descriptor version, so it has to
// identify the bytes that ran.
func (l *Locator) Locate(ctx context.Context) (dependence.Engine, error) {
	t, err := l.resolver.Resolve(ctx, lockName)
	if err != nil {
		return dependence.Engine{}, err
	}
	return l.describe(ctx, t)
}

// describe turns one resolved payload into the neutral engine description.
// Locate and LocateInstalled share it so an installed payload is described
// identically whichever of the two observed it.
func (l *Locator) describe(ctx context.Context, t toolchain.Tool) (dependence.Engine, error) {
	export, err := exportArgv(t)
	if err != nil {
		return dependence.Engine{}, err
	}
	e := dependence.Engine{
		ParseArgv:  append([]string(nil), t.ArgvPrefix...),
		ExportArgv: export,
		Env:        append([]string(nil), t.Env...),
		Version:    t.Version,
		Digest:     t.Fingerprint(),
	}
	// The runtime is resolved separately for its identity only: the engine's
	// own resolution already installed it, so this costs a store lookup and no
	// fetch. A failure here is not fatal -- the engine runs either way -- but
	// the closure then records no runtime, which is honest rather than wrong.
	if rt, err := l.resolver.Resolve(ctx, runtimeLockName); err == nil {
		e.RuntimeDigest = rt.Fingerprint()
	}
	return e, nil
}

// exportArgv derives the export tool's launcher from the parse tool's. The two
// ship side by side in one payload and share the platform's extension, so the
// derivation is a name substitution on the entry's own base name rather than a
// second lock field.
func exportArgv(t toolchain.Tool) ([]string, error) {
	argv := append([]string(nil), t.ArgvPrefix...)
	last := len(argv) - 1
	dir, base := filepath.Split(argv[last])
	if !strings.HasPrefix(base, parseEntry) {
		// For the pinned payload this cannot happen. For a user override it is
		// a real constraint on the file the user named, so the refusal has to
		// point at their configuration rather than at the store.
		if t.Source == toolchain.SourceOverride {
			return nil, &model.Error{Code: model.CodeToolOverrideInvalid,
				Message:     "the analysis engine override must be the payload's own parse launcher, whose name the export tool beside it is derived from",
				Remediation: "point the override at the engine payload's parse launcher; see docs/configuration.md"}
		}
		return nil, corrupt("the analysis engine payload does not carry the expected launcher name")
	}
	path := filepath.Join(dir, exportEntry+strings.TrimPrefix(base, parseEntry))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, corrupt("the analysis engine payload is missing its export tool")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return nil, corrupt("the analysis engine payload's export tool is not executable")
	}
	argv[last] = path
	return argv, nil
}

// corrupt is a payload failure raised on the dependence provider's own run
// path. It carries no `tool` detail: model.Error.Details is rendered verbatim
// in the CLI's failure envelope, and the lock entry's name on an indexing
// diagnostic is exactly the surface Section 11.6 keeps the engine's name off.
// The remediation already sends the operator to `codectx tools verify`, which
// is the inventory surface where the name belongs.
func corrupt(msg string) *model.Error {
	return &model.Error{Code: model.CodeToolCorrupt, Message: msg,
		Remediation: "run `codectx tools verify` and re-install the payload"}
}
