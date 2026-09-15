package scip

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// Kind is one of the six precise indexers Section 11.4 names. The value is
// also the name of the entry in the embedded tool lock and the name the
// index's own `tool_info` must declare, which is what lets one constant carry
// the identity of the profile end to end. Confirmed against a real index from
// each of the six on this platform; see the lane report.
type Kind string

const (
	KindClang      Kind = "scip-clang"
	KindGo         Kind = "scip-go"
	KindJava       Kind = "scip-java"
	KindPython     Kind = "scip-python"
	KindRust       Kind = "rust-analyzer"
	KindTypeScript Kind = "scip-typescript"
)

// NetworkPosture is what a profile declares about reaching the network. It is
// a posture codectx records and does not enforce: no sandbox is claimed, and a
// diagnostic says which posture the run was declared under so an operator can
// tell a profile that resolves dependencies over the network from one that
// cannot. Section 21's single outbound path is the toolchain fetcher; nothing
// here opens a socket on its own.
type NetworkPosture string

const (
	// NetworkDenied is a profile that indexes only what the snapshot already
	// holds.
	NetworkDenied NetworkPosture = "denied"
	// NetworkAllowed is a profile that drives a build tool which may resolve
	// dependencies from a remote index.
	NetworkAllowed NetworkPosture = "allowed"
)

// argPaths are the private paths one profile run is given. They are the only
// values that vary between runs of the same kind: everything else about the
// invocation is a constant of this build.
type argPaths struct {
	// InputDir is the materialization root, which is also the child's working
	// directory.
	InputDir string
	// OutputFile is where the index must be written. It is outside InputDir so
	// a tool cannot mistake its own output for an input.
	OutputFile string
	// WorkDir is the run's private scratch root.
	WorkDir string
}

// kindSpec is everything about one indexer that used to live in a user's
// `[analyzers.<name>]` table and is now product code (Section 20.2): the lock
// entry to resolve, the manifests that make a workspace a candidate, the exact
// argument array, the parent environment variables the child may see, the
// declared network posture, the runner reservations and the run's own bound.
//
// Every argument array below was run against a real fixture through the
// resolved payload before it was pinned; the runs are in the lane report.
type kindSpec struct {
	triggers []string
	args     func(argPaths) []string
	// env names the parent variables the child may inherit. The child's
	// environment is exactly these (those the parent actually has) plus the
	// variables the resolved tool carries, and nothing else.
	env               []string
	network           NetworkPosture
	memoryBudgetBytes int64
	diskBudgetBytes   int64
	timeout           time.Duration
}

// kindSpecs are the six profiles. Budgets and timeouts are the measured
// figures of docs/research/08-scip-empirical-six-indexers.md widened for a
// real repository: the one-file fixtures run in seconds, and the two profiles
// that drive a compiler over the whole project (scip-java through the managed
// JDK's javac, rust-analyzer through cargo) need both the time and the room.
//
// Four of the six load the project model through the host's own language
// toolchain and cannot do otherwise: scip-go needs `go`, rust-analyzer needs
// `cargo`, scip-python needs `python3`/`pip3`, and scip-clang needs the
// compilation database the project's build produced. scip-java does not:
// see KindJava. docs/providers-scip.md tables this per profile.
var kindSpecs = map[Kind]kindSpec{
	// scip-go drives the host `go` toolchain through go/packages, so `go` must
	// be reachable and its caches must be addressable; without PATH it exits 1
	// with "go command required" and writes no index (measured).
	KindGo: {
		triggers: []string{"go.mod", "go.work"},
		args: func(a argPaths) []string {
			return []string{"index", "--output", a.OutputFile}
		},
		env:               []string{"PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOPROXY", "GOPRIVATE"},
		network:           NetworkAllowed,
		memoryBudgetBytes: 2 << 30,
		diskBudgetBytes:   1 << 30,
		timeout:           20 * time.Minute,
	},
	// scip-typescript reads the project's own node_modules out of the
	// materialization; it resolves nothing itself, so its posture is denied
	// and it needs no PATH. The progress bar is disabled because the child's
	// streams are bounded diagnostics, not a terminal.
	KindTypeScript: {
		triggers: []string{"tsconfig.json", "jsconfig.json", "package.json"},
		args: func(a argPaths) []string {
			return []string{"index", "--cwd", a.InputDir, "--output", a.OutputFile, "--no-progress-bar"}
		},
		env:               []string{"HOME"},
		network:           NetworkDenied,
		memoryBudgetBytes: 4 << 30,
		diskBudgetBytes:   2 << 30,
		timeout:           30 * time.Minute,
	},
	// scip-python resolves the project's installed distributions, which it
	// finds through the interpreter and pip on PATH (research note 4).
	//
	// --project-version is pinned and not optional. Left out, scip-python asks
	// git for the current revision, and the private materialization is never a
	// repository: the lookup fails, the version stays undefined and the indexer
	// dies inside its symbol constructor with a TypeError after writing nothing
	// (measured). A constant is also the only correct value here -- the version
	// is part of every symbol string it emits, so deriving it from the snapshot
	// would rename every symbol in the repository on every commit and no fact
	// would ever be reusable. The project name stays empty, which is
	// scip-python's own "repository-local navigation only" mode and exactly
	// what this provider publishes.
	KindPython: {
		triggers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"},
		args: func(a argPaths) []string {
			return []string{"index", "--cwd", a.InputDir, "--output", a.OutputFile,
				"--project-version", pythonProjectVersion, "--quiet"}
		},
		env:               []string{"PATH", "HOME"},
		network:           NetworkDenied,
		memoryBudgetBytes: 4 << 30,
		diskBudgetBytes:   2 << 30,
		timeout:           30 * time.Minute,
	},
	// scip-java compiles the snapshot itself, with the managed JDK's own
	// javac, against the build description writeScipJavaConfig generates in the
	// materialization. Its other mode -- driving the project's own Maven or
	// Gradle build, found on PATH -- is what the profile deliberately does not
	// use: it makes precise Java indexing depend on a host build tool codectx
	// does not own and on a remote dependency index. With --scip-config the
	// profile resolves nothing remotely, so its posture is denied and
	// MAVEN_OPTS and GRADLE_USER_HOME are not in the allowlist.
	//
	// The managed JDK reaches the child as JAVA_HOME from the resolved tool, so
	// JAVA_HOME is deliberately not in this allowlist either: a host value
	// would shadow the pinned runtime.
	KindJava: {
		triggers: []string{"pom.xml", "build.gradle", "build.gradle.kts"},
		args: func(a argPaths) []string {
			return []string{"index",
				"--scip-config", filepath.Join(a.InputDir, scipJavaConfigName),
				"--targetroot", filepath.Join(a.WorkDir, scipJavaTargetRootName),
				"--output", a.OutputFile}
		},
		env:               []string{"PATH", "HOME"},
		network:           NetworkDenied,
		memoryBudgetBytes: 4 << 30,
		diskBudgetBytes:   4 << 30,
		timeout:           45 * time.Minute,
	},
	// rust-analyzer's scip subcommand loads cargo metadata, so cargo must be
	// reachable and its home addressable.
	KindRust: {
		triggers: []string{"Cargo.toml"},
		args: func(a argPaths) []string {
			return []string{"scip", a.InputDir, "--output", a.OutputFile}
		},
		env:               []string{"PATH", "HOME", "CARGO_HOME", "RUSTUP_HOME"},
		network:           NetworkAllowed,
		memoryBudgetBytes: 8 << 30,
		diskBudgetBytes:   4 << 30,
		timeout:           45 * time.Minute,
	},
	// scip-clang carries its own Clang and reads the project's compilation
	// database. The database is normalized in the private materialization
	// first: an entry whose `directory` names a path outside the copy crashes
	// the indexing worker (measured), so the run would otherwise burn its
	// whole timeout on a stale absolute path.
	KindClang: {
		triggers: []string{"compile_commands.json", "compile_flags.txt"},
		args: func(a argPaths) []string {
			return []string{
				"--compdb-path=" + filepath.Join(a.InputDir, compileCommandsName),
				"--index-output-path=" + a.OutputFile,
			}
		},
		env:               []string{"PATH", "HOME"},
		network:           NetworkDenied,
		memoryBudgetBytes: 4 << 30,
		diskBudgetBytes:   2 << 30,
		timeout:           30 * time.Minute,
	},
}

// pythonProjectVersion is the constant scip-python stamps into every symbol it
// emits. See the KindPython comment for why it is a constant.
const pythonProjectVersion = "0.0.0"

// Kinds are the six profiles in a fixed order. Map iteration order is random
// and this order reaches the provider version and the scope-key list, both of
// which must be a deterministic function of the build.
var Kinds = []Kind{KindClang, KindGo, KindJava, KindPython, KindRust, KindTypeScript}

// Profile is one runnable indexer: the kind this build knows and the payload
// the toolchain resolved for it. Holding a Profile is holding a verified
// payload — the lock pinned its bytes, the store verified them and the
// resolver hashed its entry at this resolution. There is no user approval
// anywhere in it (Section 20.2: trust is the lock).
type Profile struct {
	Kind Kind
	Tool toolchain.Tool
}

// Name is the profile's name on every surface: the scope key, the diagnostic
// detail and the tool the index must declare.
func (p Profile) Name() string { return string(p.Kind) }

// spec is the build's fixed description of this kind.
func (p Profile) spec() kindSpec { return kindSpecs[p.Kind] }

// Triggers are the manifests whose presence makes a workspace a candidate for
// one kind. It is the single source of the trigger mapping: `tools prefetch
// --for-repo` and the planner read it without resolving a payload, so the
// command that exists to install a payload does not have to install one first,
// and no second copy of the mapping can drift from this one.
//
// The result is a copy of package state; a kind this build does not know has
// no triggers and returns nil.
func Triggers(k Kind) []string { return slices.Clone(kindSpecs[k].triggers) }

// Argv is the argument array one kind is run with, after the resolved
// payload's own launcher prefix. It is the single source of every indexer
// invocation this product issues: the provider's own run path builds its
// arguments here, and so does the per-platform tools matrix, whose whole
// purpose is to prove that the invocation the product issues works on that
// platform. A matrix that ran a different argument array would answer a
// different question (see docs/toolchain.md).
//
// inputDir is the directory the indexer reads, which is also the child's
// working directory; outputFile is where the index must be written; workDir is
// the run's private scratch root. A kind this build does not know returns nil.
func Argv(k Kind, inputDir, outputFile, workDir string) []string {
	spec, ok := kindSpecs[k]
	if !ok {
		return nil
	}
	return spec.args(argPaths{InputDir: inputDir, OutputFile: outputFile, WorkDir: workDir})
}

// Triggers are the manifests whose presence makes a workspace a candidate.
func (p Profile) Triggers() []string { return Triggers(p.Kind) }

// Network is the declared posture of this profile.
func (p Profile) Network() NetworkPosture { return kindSpecs[p.Kind].network }

// argv is the complete argument array after the launcher: the resolved tool's
// own argv prefix supplies argv[0] and, for a runtime-hosted payload, the
// entry script, and the kind supplies the rest.
func (p Profile) argv(a argPaths) (path string, args []string) {
	prefix := p.Tool.ArgvPrefix
	args = append(append([]string(nil), prefix[1:]...), Argv(p.Kind, a.InputDir, a.OutputFile, a.WorkDir)...)
	return prefix[0], args
}

// env is the child's complete environment: the allowlisted parent variables
// the parent actually has, then the variables the resolved tool carries.
// The tool's own come last so a host JAVA_HOME can never shadow the managed
// JDK the lock pinned.
func (p Profile) env(lookup func(string) (string, bool)) []string {
	var out []string
	for _, name := range kindSpecs[p.Kind].env {
		if value, ok := lookup(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return append(out, p.Tool.Env...)
}

// unresolved records why one kind has no payload on this machine. The code is
// the toolchain's own CTX_TOOL_* code, carried verbatim so a diagnostic names
// the real condition (offline, unsupported platform, a corrupt store, an
// invalid override) rather than a flat "not installed".
type unresolved struct {
	kind Kind
	code string
	err  error
}

// markerDeferred labels a kind whose payload this platform pins but the store
// does not hold yet: the first unit that needs it fetches it and indexes the
// language at full precision. It is deliberately not spelled like a Section 22
// code. It shares a detail map with the toolchain's real CTX_TOOL_* refusals,
// and the one consumer of that map -- provider.Registry.Select -- publishes a
// degraded capability row for a refusal. A pending payload is not degradation,
// so it must be distinguishable by value: anything CTX_-prefixed in these
// details means "this part of the provider cannot run", and this does not.
const markerDeferred = "deferred"

// resolveProfiles sorts every kind into the three states construction can
// observe, without installing anything.
//
// ready is a payload the store already holds, verified at this resolution.
// deferred is a payload this platform pins that the store does not hold: the
// language is still indexable, and the first unit that needs it fetches it
// (see Provider.profileFor). Constructing a provider must not install half a
// gigabyte of indexers for languages the repository does not contain, which is
// what resolving with a fetch here did.
// bad is a kind this machine cannot index precisely at all -- an unsupported
// platform, a corrupt store entry, an invalid override -- which Section 11.7
// makes honest absence with the toolchain's own typed reason.
//
// identities is the payload identity of every kind that will produce facts,
// ready and deferred alike: the resolved fingerprint for one the store holds
// and the lock's pinned fingerprint for one it does not. They are the same
// string for the same payload, which is what makes Descriptor().Version
// independent of whether the payload happened to be installed when the process
// started (see toolsFingerprint).
//
// A deferred kind whose pinned identity cannot even be named joins bad. That is
// unreachable as the lock stands -- every Kind is a lock entry, a platform with
// no payload is already a typed refusal from ResolveInstalled, an overridden
// kind resolves without a store and so is never deferred, and an overridden
// runtime folds the override's own identity into the pinned fingerprint -- and
// planning a unit whose facts could not be keyed is the one outcome that must
// not be possible.
//
// All four results are deterministic in Kinds order.
func resolveProfiles(ctx context.Context, r *toolchain.Resolver) (ready []Profile, deferred []Kind, identities map[Kind]string, bad []unresolved) {
	identities = make(map[Kind]string, len(Kinds))
	for _, k := range Kinds {
		t, installed, err := r.ResolveInstalled(ctx, string(k))
		switch {
		case err != nil:
			bad = append(bad, unresolved{kind: k, code: codeOf(err), err: err})
		case installed:
			ready = append(ready, Profile{Kind: k, Tool: t})
			identities[k] = t.Fingerprint()
		default:
			pinned, err := r.PinnedFingerprint(string(k))
			if err != nil {
				bad = append(bad, unresolved{kind: k, code: codeOf(err), err: err})
				continue
			}
			deferred = append(deferred, k)
			identities[k] = pinned
		}
	}
	return ready, deferred, identities, bad
}

// codeOf is the typed code of a resolution failure, or CTX_PROVIDER_UNAVAILABLE
// for an error that carries none.
func codeOf(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code != "" {
		return typed.Code
	}
	return model.CodeProviderUnavailable
}

// runProfile executes one profile against a private materialization of the
// snapshot and returns the path of the validated index it produced and the
// SHA-256 of the input manifest that binds the run. The caller owns runDir and
// removes it on every path; the index and the manifest are inside it.
//
// The tool sees exactly: the materialized snapshot files as its working
// directory, the argument array this build pins for its kind, the allowlisted
// environment and the variables its own payload needs. There is no shell.
//
// The input manifest is written after the run, because its first lines commit
// to the SHA-256 of the index the run produced and that digest does not exist
// until the tool has written it. The manifest file dies with runDir; its own
// digest is the durable record and is carried on every diagnostic this path
// returns. The unit's durable binding is UnitSpec.InputHash, which the
// coordinator computes over the declared inputs.
func (p *Provider) runProfile(ctx context.Context, prof Profile, view model.SnapshotView, runDir string, seen *limitSeen) (indexPath, manifestSHA string, err error) {
	if p.runner == nil {
		return "", "", p.profileError(prof, "", &model.Error{Code: model.CodeProviderUnavailable, Message: "scip profiles need the shared process runner"})
	}
	// The whole snapshot is the deliberate selection, not an omitted Include
	// (snapshot.MaterializeOptions.Include exists and is not used here on
	// purpose). A precise indexer resolves a symbol through the project's own
	// dependency context -- the module graph, the package manifests, the
	// installed distributions, the headers -- and the unit's scope is the
	// workspace, so a copy narrowed to some subset of files would produce an
	// index that describes less than the unit claims. See docs/providers-scip.md.
	// max_materialize_bytes is unlimited by default, so the copy is the whole
	// snapshot unless an operator asks otherwise. A user-set budget leaves
	// files out, and the materializer is what reports those exclusions --
	// count and exemplars, to the operator, from the one place that knows
	// which files they were. It is deliberately not tallied here: this
	// provider would only be able to repeat the figure the operator set.
	mat, err := snapshot.Materialize(ctx, view, model.FileSelection{}, snapshot.MaterializeOptions{Dir: filepath.Join(runDir, "src"), MaxBytes: p.limits.MaxMaterializeBytes.Value()})
	if err != nil {
		return "", "", p.profileError(prof, "", err)
	}
	defer mat.Close()
	switch prof.Kind {
	case KindClang:
		if err := normalizeCompileCommands(mat.Root(), p.limits.MaxManifestBytes, seen); err != nil {
			return "", "", p.profileError(prof, "", err)
		}
	case KindJava:
		// Refused before the JVM starts, not diagnosed from its exit status.
		if err := requireJavaSources(mat.Root()); err != nil {
			return "", "", p.profileError(prof, "", err)
		}
		if err := writeScipJavaConfig(mat.Root()); err != nil {
			return "", "", p.profileError(prof, "", err)
		}
	}
	output := filepath.Join(runDir, "index.scip")
	manifestPath := filepath.Join(runDir, "inputs.manifest")
	path, args := prof.argv(argPaths{InputDir: mat.Root(), OutputFile: output, WorkDir: runDir})
	// The configured value is authoritative: zero, the default, is no wall
	// clock at all, and the profile's own figure is a built-in ceiling that
	// would otherwise reinstate the refusal the configuration removed. A
	// wedged indexer is caught by the stall detector instead, at any repository
	// size.
	timeout := p.timeout
	if spec := prof.spec().timeout; timeout > 0 && spec > 0 && spec < timeout {
		timeout = spec
	}
	_, err = p.runner.Run(ctx, process.Spec{
		Path: path, Args: args, Dir: mat.Root(), Env: prof.env(p.lookupEnv),
		MaxStdoutBytes: maxToolOutputBytes, MaxStderrBytes: maxToolOutputBytes,
		Timeout: timeout, StallTimeout: p.stallTimeout, Grace: toolGrace,
		MemoryReservationBytes: prof.spec().memoryBudgetBytes, DiskReservationBytes: prof.spec().diskBudgetBytes,
	})
	if err != nil {
		return "", "", p.profileError(prof, "", err)
	}
	// The output must be a regular file the tool wrote inside the run
	// directory, within the index bound. A symlink is refused: the decoder
	// would otherwise read whatever it points at as the tool's output.
	info, err := os.Lstat(output)
	if err != nil {
		return "", "", p.profileError(prof, "", &model.Error{Code: model.CodeProviderOutputInvalid, Message: prof.Name() + " produced no index at its output path"})
	}
	if !info.Mode().IsRegular() {
		return "", "", p.profileError(prof, "", &model.Error{Code: model.CodeProviderOutputInvalid, Message: prof.Name() + " output is not a regular file"})
	}
	seen.note(limitIndexBytes, info.Size())
	indexSHA, err := fileSHA256(output)
	if err != nil {
		return "", "", p.profileError(prof, "", err)
	}
	manifestSHA, err = writeManifest(ctx, view, manifestPath, indexSHA)
	if err != nil {
		return "", "", p.profileError(prof, "", err)
	}
	return output, manifestSHA, nil
}

// profileError carries the run-level facts a profile diagnostic must name: the
// profile, the payload identity the lock pinned, the declared network posture
// and, once it exists, the digest of the input manifest that binds the run to
// its inputs.
//
// model.Error.Details is the only run-level diagnostic channel the frozen
// provider contract offers: model.CapabilityState carries a diagnostic code
// and nothing else, and model.ProviderResult carries no details at all. See
// the report's "Shared-helper changes needed".
func (p *Provider) profileError(prof Profile, manifestSHA string, err error) error {
	var typed *model.Error
	if !errors.As(err, &typed) {
		return err
	}
	typed = typed.WithDetail("profile", prof.Name()).
		WithDetail("tool", prof.Tool.Fingerprint()).
		WithDetail("network", string(prof.Network()))
	if manifestSHA != "" {
		typed = typed.WithDetail("input_manifest_sha256", manifestSHA)
	}
	return typed
}

// fileSHA256 is the lowercase hex SHA-256 of a file this provider produced.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", internal("scip index digest: " + err.Error())
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", internal("scip index digest: " + err.Error())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const (
	// maxToolOutputBytes bounds each captured stream of an indexer. The
	// streams are diagnostics only and never logged raw (Section 22).
	maxToolOutputBytes = 1 << 20
	toolGrace          = 10 * time.Second
)

// Input-manifest format (v1). A manifest is a claim about which bytes an
// index describes, so it has to commit to the index as well as to the files:
// without the index digest the same list of file hashes would "verify" any
// index at all, which is an assertion and not a verification.
//
//	codectx-scip-manifest v1
//	index-sha256 <hex>
//	<sha256>  <root-relative path>
//	...
const (
	manifestHeader    = "codectx-scip-manifest v1"
	manifestIndexLine = "index-sha256 "
)

// writeManifest records the run's inputs in the v1 format: the digest of the
// index the run produced, then the content hash of every materialized file,
// one line per file in manifest order, streaming from the snapshot view so no
// file list is held. It returns the SHA-256 of the manifest bytes, which
// therefore commits to both the index and every input.
func writeManifest(ctx context.Context, view model.SnapshotView, path, indexSHA string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", internal("scip manifest: " + err.Error())
	}
	h := sha256.New()
	w := bufio.NewWriter(io.MultiWriter(f, h))
	if _, err = fmt.Fprintf(w, "%s\n%s%s\n", manifestHeader, manifestIndexLine, indexSHA); err == nil {
		err = view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
			if fv.Status == model.FileDeleted {
				return nil
			}
			_, err := fmt.Fprintf(w, "%s  %s\n", fv.ContentHash, fv.Path)
			return err
		})
	}
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", internal("scip manifest: " + err.Error())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkTool verifies that the produced index names the tool this profile ran.
// Output from another tool is not output from the payload the lock pinned, and
// an index the provider did not produce cannot be admitted as exact-source
// evidence on the strength of a run that produced something else.
//
// The tool's self-reported *version* is deliberately not compared against
// anything. The lock's entry digest is what identifies these bytes, and two of
// the six pinned payloads report a version that no constraint derived from the
// lock could match: scip-java 0.13.1 reports "0.0.0-SNAPSHOT" and the
// rust-analyzer release tagged 2026-08-17.4 reports "1.98.0 (88d9e12
// 2026-08-18)" (both measured here). A constraint that must be written to
// accept those is a constraint that accepts anything. The reported version is
// kept as provenance instead: it reaches Detection.ObservedVersion and the
// unit's identity through the payload fingerprint.
func checkTool(prof Profile, m metadata) error {
	if m.toolName != prof.Name() {
		return outputInvalidf("index was produced by %q, profile %s runs %q", m.toolName, prof.Name(), prof.Name())
	}
	return nil
}

// fingerprintDomain separates the provider-version digest from every other
// canonical hash in the tree.
const fingerprintDomain = "scip-provider-tools-v1"

// toolsFingerprint is the digest of the payload identity of every kind that can
// produce facts, in Kinds order. It is what Descriptor().Version folds in so
// that replacing an indexer invalidates the units it produced: Section 20.2
// keeps `[tools]` out of AnalysisConfigHash, so the tool identity has to reach
// the unit key through the provider's own version.
//
// A deferred kind contributes the identity the lock pins for it, which is the
// identity the payload has once the unit that needs it has fetched it. The
// facts really are produced by the pinned indexer, so they are keyed by it,
// and a first run on a cold machine keys its units exactly as every later run
// does -- without which the fetching run's whole output is re-indexed by the
// next process.
//
// A kind whose payload this machine cannot supply at all contributes one fixed
// empty slot, never the reason it is missing. It plans no unit and produces no
// facts, and two machines that hold the same pinned payloads must key their
// units identically -- why some other language's indexer is absent (offline
// here, an unsupported platform there) is not part of what produced these
// facts. The typed reason belongs to Detection.Details, which carries it.
func toolsFingerprint(identities map[Kind]string) string {
	h := model.NewHasher(fingerprintDomain)
	for _, k := range Kinds {
		h.AddString(string(k))
		h.AddString(identities[k])
	}
	return h.Sum()
}

func outputInvalidf(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: fmt.Sprintf(format, args...)}
}
