package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// Definition is everything this build knows about one supported language
// server: its name — which is also the name of its entry in the embedded tool
// lock — the manifest language tags it serves, the argument array that puts it
// in stdio mode, the parent environment variables it may see, the workspace
// markers that suggest a repository is its kind, and its bounds.
//
// A Definition is a description, not a capability. It becomes runnable only
// through Resolve, which hands back the payload the lock pinned and the store
// verified. Nothing here is configurable and nothing is looked up on PATH
// (Section 20.2).
type Definition struct {
	Name string
	// Languages are the snapshot manifest language tags this server serves.
	Languages []string
	// Args is the argument array appended to the resolved payload's own argv
	// prefix. ${input_dir} is the materialization root and ${work_dir} the
	// server's private working directory; there is no other substitution and
	// no shell.
	Args []string
	// RuntimeArgs are arguments of the runtime that hosts the *pinned* payload,
	// placed between the runtime executable and the payload itself. A JVM
	// option after `-jar` is an application argument, not a JVM option, so a
	// payload the managed JDK hosts has no other way to ask for one. They
	// belong to the managed launch only: an override replaces the binary and
	// never the invocation, so no runtime is composed around it and there is
	// nothing for them to configure (see Profile.runtimeArgs).
	RuntimeArgs []string
	// EnvAllowlist names the parent variables the server may see. The child's
	// environment is exactly these (those the parent actually has) plus the
	// variables the resolved payload carries, and nothing else.
	EnvAllowlist []string
	// RootMarkers are root-relative files whose presence suggests the
	// repository is this server's kind. Detection reads their metadata through
	// the confined root and nothing else.
	RootMarkers []string
	// MemoryBudgetBytes and DiskBudgetBytes are the runner reservations;
	// Timeout bounds the server's whole lifetime. The manager's idle TTL
	// usually stops it long before.
	MemoryBudgetBytes int64
	DiskBudgetBytes   int64
	Timeout           time.Duration
}

// Standard bounds for a language server. A server is an interactive process
// held open across many requests, so the timeout is a lifetime ceiling rather
// than a per-request bound (Options.RequestTimeout is that one), and the
// reservations are what the runner accounts before the child starts.
const (
	serverLifetime     = time.Hour
	serverMemoryBudget = 4 << 30
	serverDiskBudget   = 2 << 30
	serverMemoryLarge  = 8 << 30
	serverDiskLarge    = 4 << 30
	workDirName        = "lsp"
	// serverJDTLS is the one payload with a platform configuration directory to
	// seed and runtime arguments to pass; both are named by this constant
	// rather than by a repeated string literal.
	serverJDTLS         = "jdtls"
	substitutionInput   = "${input_dir}"
	substitutionWorkDir = "${work_dir}"
)

// definitions are the six servers Section 11.5 names. Every one of them was
// resolved through the real lock and store on this platform before its
// argument array was pinned; the protocol handshake is exercised against gopls
// and against the fake server. docs/providers-lsp.md records exactly which.
var definitions = map[string]Definition{
	"gopls": {
		Name: "gopls", Languages: []string{"go"},
		Args:              []string{"serve"},
		EnvAllowlist:      []string{"PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOPROXY", "GOPRIVATE"},
		RootMarkers:       []string{"go.mod", "go.work"},
		MemoryBudgetBytes: serverMemoryBudget, DiskBudgetBytes: serverDiskBudget, Timeout: serverLifetime,
	},
	"rust-analyzer": {
		Name: "rust-analyzer", Languages: []string{"rust"},
		EnvAllowlist:      []string{"PATH", "HOME", "CARGO_HOME", "RUSTUP_HOME"},
		RootMarkers:       []string{"Cargo.toml"},
		MemoryBudgetBytes: serverMemoryLarge, DiskBudgetBytes: serverDiskLarge, Timeout: serverLifetime,
	},
	// The python server is a native static binary started with its server
	// subcommand, so it runs as itself like gopls, clangd and rust-analyzer
	// rather than under the managed Node runtime (ADR-0006).
	"ty": {
		Name: "ty", Languages: []string{"python"},
		Args:              []string{"server"},
		EnvAllowlist:      []string{"PATH", "HOME"},
		RootMarkers:       []string{"pyproject.toml", "ty.toml", "setup.py", "requirements.txt"},
		MemoryBudgetBytes: serverMemoryBudget, DiskBudgetBytes: serverDiskBudget, Timeout: serverLifetime,
	},
	"typescript-language-server": {
		Name: "typescript-language-server", Languages: []string{"typescript", "tsx", "javascript"},
		Args:              []string{"--stdio"},
		EnvAllowlist:      []string{"PATH", "HOME"},
		RootMarkers:       []string{"tsconfig.json", "jsconfig.json", "package.json"},
		MemoryBudgetBytes: serverMemoryBudget, DiskBudgetBytes: serverDiskBudget, Timeout: serverLifetime,
	},
	"clangd": {
		Name: "clangd", Languages: []string{"c", "cpp"},
		EnvAllowlist:      []string{"PATH", "HOME"},
		RootMarkers:       []string{"compile_commands.json", "compile_flags.txt", ".clangd", "CMakeLists.txt"},
		MemoryBudgetBytes: serverMemoryBudget, DiskBudgetBytes: serverDiskBudget, Timeout: serverLifetime,
	},
	serverJDTLS: {
		Name: serverJDTLS, Languages: []string{"java"},
		// The Equinox launcher needs its application identity and its module
		// openings as JVM options, before -jar. jdt.ls reflects into java.base
		// and the upstream launcher passes the same set; the configuration
		// directory's config.ini repeats the three eclipse.* properties, which
		// is why the launcher bootstraps even without them (measured), but a
		// payload that stopped carrying them would then fail silently.
		RuntimeArgs: []string{
			"-Declipse.application=org.eclipse.jdt.ls.core.id1",
			"-Dosgi.bundles.defaultStartLevel=4",
			"-Declipse.product=org.eclipse.jdt.ls.core.product",
			"--add-modules=ALL-SYSTEM",
			"--add-opens", "java.base/java.util=ALL-UNNAMED",
			"--add-opens", "java.base/java.lang=ALL-UNNAMED",
		},
		// Equinox *writes* into its configuration directory -- OSGi caches, a p2
		// data area, its own error log -- so it is a private copy under the work
		// directory, seeded from the payload once (see seedPlatformConfig).
		// Pointed at the payload's own config_<platform>, three failed starts
		// left four new paths inside a published, digest-identified store
		// version that `codectx tools verify` cannot see, because verify
		// rehashes the pinned entry and not the payload tree.
		Args: []string{
			"-configuration", substitutionWorkDir + "/config",
			"-data", substitutionWorkDir + "/data",
		},
		// JAVA_HOME is deliberately absent: the resolved payload carries the
		// managed JDK's own, and a host value in the allowlist would shadow
		// the runtime the lock pinned.
		EnvAllowlist:      []string{"PATH", "HOME"},
		RootMarkers:       []string{"pom.xml", "build.gradle", "build.gradle.kts"},
		MemoryBudgetBytes: serverMemoryLarge, DiskBudgetBytes: serverDiskLarge, Timeout: serverLifetime,
	},
}

// Definitions returns the supported servers in name order.
func Definitions() []Definition {
	out := make([]Definition, 0, len(definitions))
	for _, d := range definitions {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProjectRoot is the directory this server is rooted at when it answers about
// the snapshot file at rel: the DEEPEST directory at or above that file which
// holds one of the definition's root markers, spelled root-relative, with the
// empty string meaning the workspace root itself.
//
// A repository does not keep its projects at its root. Rooted at the workspace
// root of a monorepo, a server is handed a directory whose manifest describes
// none of the projects under it: it resolves no dependency, builds no project
// model and answers about a file with whatever it can infer from that file
// alone. Each project therefore gets its own server, rooted where the
// language's own toolchain expects to be started.
//
// The deepest marker wins because that is the project that owns the file: a
// module inside a workspace is its own project, and the workspace manifest
// above it describes the aggregate, not the module. A file with no marker
// above it belongs to the workspace root, which is the honest answer for a
// repository that declares nothing.
//
// The answer comes from the pinned snapshot's own manifest, never from the
// live checkout: the server is materialized from that snapshot, so a marker
// the working tree has and the snapshot does not names a directory the server
// would find empty. Nothing repository-sized is retained -- the scan keeps one
// string.
func (d Definition) ProjectRoot(ctx context.Context, view model.SnapshotView, rel string) (string, error) {
	markers := make(map[string]bool, len(d.RootMarkers))
	for _, m := range d.RootMarkers {
		markers[m] = true
	}
	best := ""
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if !markers[path.Base(fv.Path)] {
			return nil
		}
		dir := ""
		if i := strings.LastIndexByte(fv.Path, '/'); i >= 0 {
			dir = fv.Path[:i]
		}
		if len(dir) > len(best) && underDir(rel, dir) {
			best = dir
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return best, nil
}

// underDir reports whether the root-relative path rel lies at or under the
// root-relative directory dir; the empty dir is the workspace root and
// contains everything.
func underDir(rel, dir string) bool {
	if dir == "" {
		return true
	}
	return strings.HasPrefix(rel, dir+"/")
}

// Profile is a runnable server: a Definition joined with the payload the
// toolchain resolved for it. Only Resolve constructs one, so holding a Profile
// is holding a verified payload — the lock pinned its bytes, the store
// verified them and the resolver hashed its entry at this resolution.
type Profile struct {
	Definition
	// Tool is the resolved payload. Its ArgvPrefix is the complete launcher
	// (the binary itself, or the managed Node or JDK and the pinned entry) and
	// its Env carries what that launcher needs.
	Tool toolchain.Tool
	// Root is the root-relative project directory this server is started at,
	// as ProjectRoot answers it; the empty string is the workspace root. It is
	// part of the server's identity: two projects of one repository answered by
	// one server name is two servers, two private working directories and two
	// overlay input digests.
	Root string
}

// Resolve returns the named server's runnable profile. The overlay must be
// enabled and the name must be one of the six this build supports; everything
// else about the server — which binary starts, under which runtime, with which
// arguments and budgets — comes from this build and the embedded tool lock.
//
// A payload that cannot be resolved surfaces the toolchain's own CTX_TOOL_*
// error verbatim: offline, an unsupported platform, a corrupt store or an
// invalid override are different conditions and a caller must be able to tell
// them apart.
func Resolve(ctx context.Context, resolver *toolchain.Resolver, cfg config.Config, name string) (Profile, error) {
	if cfg.Providers.LSP.Enabled == config.Disabled {
		return Profile{}, unavailable("the lsp overlay is disabled by configuration").WithDetail("profile", name)
	}
	def, ok := definitions[name]
	if !ok {
		return Profile{}, invalid("%q is not a supported language server; see docs/providers-lsp.md", truncate(name, 64))
	}
	if resolver == nil {
		return Profile{}, unavailable("the lsp overlay needs the managed toolchain resolver").WithDetail("profile", name)
	}
	t, err := resolver.Resolve(ctx, name)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) {
			return Profile{}, typed.WithDetail("profile", name)
		}
		return Profile{}, err
	}
	// RuntimeArgs only mean anything when a runtime hosts the payload, which for
	// a managed payload is exactly when the resolved prefix carries more than
	// the executable. A pinned definition that sets them for a directly
	// executable payload could not start as written; that is a product defect,
	// not a condition a user can correct. An override is exempt: it is always a
	// bare executable by construction, and runtimeArgs drops them for it.
	if len(def.RuntimeArgs) > 0 && t.Source == toolchain.SourceManaged && len(t.ArgvPrefix) < 2 {
		return Profile{}, internalError("language server %q declares runtime arguments but its payload is not runtime-hosted", name)
	}
	return Profile{Definition: def, Tool: t}, nil
}

// workDir is the server's private working directory under the data directory.
// It belongs to codectx, not to a user's shell, and the server start creates
// it before the runner ever inspects it.
//
// The last component is the resolved payload's identity, so a payload change
// gets a new directory. What lives here is derived from the payload and is not
// rewritten once written: jdtls's Equinox configuration is seeded from the
// payload's own config tree and names bundle jars by exact version, so a new
// payload booted against the previous one's configuration fails at startup with
// no way back except deleting the directory by hand. The workspace index under
// -data is derived from it too, and re-creating that after an upgrade is the
// safe direction. The previous payload's directory is left in place: nothing
// under <data_dir>/lsp is reclaimed today (the tool store under
// <data_dir>/tools is swept by `codectx tools gc`, and dependence sweeps its
// own private tree; neither touches this one), so a machine keeps one tree per
// jdtls version it has been pinned to.
// That bound is the cost of not rewriting a configuration underneath a running
// server, and reclaiming it belongs to whoever owns data-directory retention.
//
// The identity is the payload fingerprint rather than the version, because the
// version is not by itself an identity: an override carries the version its
// user typed, a re-pinned payload may keep its upstream version string, and the
// fingerprint folds the bytes as well. It is the fingerprint's digest half
// (Tool.FingerprintDigest) because this is a path component: the rendered
// fingerprint embeds that same user-supplied version verbatim, and a version of
// ".." would name the data directory's parent.
// The last component is the project the server is rooted at, because what
// lives under -data is that project's own workspace index: two projects
// sharing one directory is two servers writing one index of two different
// programs. It is a digest of the root-relative directory rather than the
// directory itself, for the same reason the payload identity is a digest: a
// snapshot path is not a legal path component and ".." would name the data
// directory's parent.
func (p Profile) workDir(dataDir string) string {
	return filepath.Join(dataDir, workDirName, p.Name, p.Tool.FingerprintDigest(), model.H(domainServerProject, p.Root)[:16])
}

// domainServerProject separates the project-directory digest above from every
// other hash this build computes.
const domainServerProject = "lsp-server-project"

// argv is the complete argument array after the launcher: the resolved
// payload's own prefix supplies argv[0] and, for a runtime-hosted payload, the
// pinned entry, and the definition supplies the rest with its two typed
// substitutions applied.
func (p Profile) argv(inputDir, workDir string) (path string, args []string) {
	prefix := p.Tool.ArgvPrefix
	// RuntimeArgs precede the payload the runtime hosts, never follow it.
	args = append(append([]string(nil), p.runtimeArgs()...), prefix[1:]...)
	for _, a := range p.Args {
		a = strings.ReplaceAll(a, substitutionInput, inputDir)
		a = strings.ReplaceAll(a, substitutionWorkDir, workDir)
		args = append(args, a)
	}
	return prefix[0], args
}

// runtimeArgs are the runtime arguments this launch actually uses: the
// definition's for the managed payload, none for a user override. An override
// is run directly, with no managed runtime composed around it, so a JVM option
// would reach it as one of its own arguments. It is a method rather than a
// field read so argv and inputDigest cannot disagree about what was passed.
func (p Profile) runtimeArgs() []string {
	if p.Tool.Source != toolchain.SourceManaged {
		return nil
	}
	return p.RuntimeArgs
}

// env builds the child's complete environment: the allowlisted parent
// variables the parent actually has, then the variables the resolved payload
// carries. The payload's come last so a host JAVA_HOME can never shadow the
// managed JDK the lock pinned. A variable the parent does not have is absent,
// not empty.
func (p Profile) env() []string {
	var out []string
	for _, name := range p.EnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	return append(out, p.Tool.Env...)
}

// serverVersion reduces what a server reports in serverInfo.version to the
// label the overlay carries. gopls reports a compact JSON build description
// whose top-level Version field holds the tag; other servers report a short
// string. The label is bounded to model.MaxIdentifierBytes so the overlay
// binding validates; the full report still feeds the input digest.
//
// The report is provenance, never a gate. Nothing compares it against a
// constraint: the lock's entry digest is what identifies these bytes, and the
// version a server chooses to print has no fixed relationship to the release
// it came from — the rust-analyzer release tagged 2026-08-17.4 reports
// "1.98.0 (88d9e12 2026-08-18)" and the scip-java 0.13.1 payload reports
// "0.0.0-SNAPSHOT" (both measured here). A constraint written to accept those
// is a constraint that accepts anything, which is a gate in name only.
// A server that reports nothing falls back to pinned, the version of the
// payload the lock pinned, which is never empty for a resolved tool.
func serverVersion(reported, pinned string) string {
	v := strings.TrimSpace(reported)
	if strings.HasPrefix(v, "{") {
		var build struct {
			Version string `json:"Version"`
		}
		if json.Unmarshal([]byte(v), &build) == nil && build.Version != "" {
			v = build.Version
		}
	}
	if v == "" {
		v = pinned
	}
	if len(v) > model.MaxIdentifierBytes {
		v = v[:model.MaxIdentifierBytes]
	}
	return v
}

// inputDigest is the overlay label's input hash: the snapshot's identity and
// manifest, the payload the lock pinned and how it was started, and the server
// the handshake revealed. Two overlays with equal digests answered from the
// same bytes with the same tool; a different digest is a different question.
//
// The payload fingerprint is what makes a replaced tool a different question:
// Section 20.2 keeps `[tools]` out of the analysis configuration hash, so a
// tool change reaches an answer's identity only here and through the
// providers' own versions.
func inputDigest(snap model.Snapshot, p Profile, serverVersion, encoding string) string {
	h := model.NewHasher("lsp-overlay-v1")
	h.AddString(string(snap.ID))
	h.AddString(snap.ManifestHash)
	h.AddString(p.Name)
	// The project the server was rooted at: two projects of one repository
	// answered by one server name are two different questions, and without
	// this their labels would be byte-identical while the answers came from
	// two different programs.
	h.AddString(p.Root)
	h.AddString(p.Tool.Fingerprint())
	h.AddString(serverVersion)
	h.AddString(encoding)
	h.AddString(strings.Join(p.Args, "\x00"))
	h.AddString(strings.Join(p.runtimeArgs(), "\x00"))
	return h.Sum()
}
