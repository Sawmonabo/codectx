package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Definition is what this build knows about one supported language server:
// its name, the manifest language tags it serves, the argument array that
// puts it in stdio mode and the workspace markers that suggest a repository
// is its kind. A Definition authorizes nothing. It becomes runnable only when
// the user configuration approves an executable under the same name
// (Section 20.2: enabled="auto" uses an already approved profile and never
// executes what is found on PATH).
type Definition struct {
	Name string
	// Languages are the snapshot manifest language tags this server serves.
	Languages []string
	// Args is the default argument array. An approved profile's own Args
	// replace it entirely when present.
	Args []string
	// RootMarkers are root-relative files whose presence suggests the
	// repository is this server's kind. Detection reads their metadata through
	// the confined root and nothing else.
	RootMarkers []string
}

// definitions are the six servers Section 11.5 names. Only gopls has been
// exercised against a real installation, in an ad-hoc run recorded in the
// Task 10 report; the others are documented as unverified in
// docs/providers-lsp.md. The protocol itself is exercised by the fake server.
var definitions = map[string]Definition{
	"gopls": {
		Name: "gopls", Languages: []string{"go"},
		Args:        []string{"serve"},
		RootMarkers: []string{"go.mod", "go.work"},
	},
	"rust-analyzer": {
		Name: "rust-analyzer", Languages: []string{"rust"},
		RootMarkers: []string{"Cargo.toml"},
	},
	"pyright": {
		Name: "pyright", Languages: []string{"python"},
		Args:        []string{"--stdio"},
		RootMarkers: []string{"pyproject.toml", "pyrightconfig.json", "setup.py", "requirements.txt"},
	},
	"typescript-language-server": {
		Name: "typescript-language-server", Languages: []string{"typescript", "tsx", "javascript"},
		Args:        []string{"--stdio"},
		RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json"},
	},
	"clangd": {
		Name: "clangd", Languages: []string{"c", "cpp"},
		RootMarkers: []string{"compile_commands.json", "compile_flags.txt", ".clangd", "CMakeLists.txt"},
	},
	"jdtls": {
		Name: "jdtls", Languages: []string{"java"},
		Args:        []string{"-data", "${work_dir}"},
		RootMarkers: []string{"pom.xml", "build.gradle", "build.gradle.kts"},
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

// Detect reports which of the definition's root markers exist in the
// workspace. It inspects metadata through the confined root only, runs
// nothing, and its answer is a hint for choosing among already trusted
// profiles: a present marker never authorizes execution.
func (d Definition) Detect(root workspace.Root) []string {
	var found []string
	for _, marker := range d.RootMarkers {
		if info, err := root.Lstat(marker); err == nil && info.Mode().IsRegular() {
			found = append(found, marker)
		}
	}
	return found
}

// Profile is a trusted, runnable server: a Definition joined with the
// operator's approval from the user configuration. Only Trusted constructs
// one, so holding a Profile is holding the approval.
type Profile struct {
	Definition
	// Executable is the approved absolute path.
	Executable string
	// VersionConstraint is matched against the version the server reports in
	// its initialize result; a server that reports none or another version is
	// shut down as untrusted.
	VersionConstraint string
	// Checksum is the expected lowercase SHA-256 of the executable, or empty
	// to record the observed checksum without checking it.
	Checksum string
	// Args is the resolved argument array with substitutions still in place;
	// the server start applies them from validated private paths.
	Args []string
	// EnvAllowlist names the parent variables the server may see.
	EnvAllowlist []string
	// WorkDir is the private working directory the server runs in.
	WorkDir string
	// MemoryBudgetBytes and DiskBudgetBytes are the runner reservations.
	MemoryBudgetBytes int64
	DiskBudgetBytes   int64
	// Timeout bounds the server's whole lifetime.
	Timeout time.Duration
}

// Trusted resolves the named server from the configuration. It is the only
// constructor of a Profile: the LSP overlay must be enabled, the name must be
// a supported definition and the user configuration must approve it under
// [analyzers.<name>]. Each refusal has its own code so a caller can tell
// "disabled" from "not approved" from "not a language server".
func Trusted(cfg config.Config, name string) (Profile, error) {
	if cfg.Providers.LSP.Enabled == config.Disabled {
		return Profile{}, unavailable("the lsp overlay is disabled by configuration").WithDetail("profile", name)
	}
	def, ok := definitions[name]
	if !ok {
		return Profile{}, invalid("%q is not a supported language server; see docs/providers-lsp.md", truncate(name, 64))
	}
	approval, ok := cfg.Analyzers[name]
	if !ok {
		return Profile{}, trustRequired("language server %q is not approved in the user configuration", name).WithDetail("profile", name)
	}
	if !filepath.IsAbs(approval.Executable) {
		return Profile{}, trustRequired("language server %q is approved with a non-absolute executable", name).WithDetail("profile", name)
	}
	if approval.VersionConstraint == "" {
		return Profile{}, trustRequired("language server %q is approved without a version constraint", name).WithDetail("profile", name)
	}
	if approval.WorkDir == "" || approval.Timeout <= 0 || approval.MemoryBudgetBytes <= 0 || approval.DiskBudgetBytes <= 0 {
		return Profile{}, trustRequired("language server %q is approved without a work directory, timeout or budgets", name).WithDetail("profile", name)
	}
	args := def.Args
	if len(approval.Args) > 0 {
		args = approval.Args
	}
	for _, a := range args {
		if strings.Contains(a, "${output_file}") || strings.Contains(a, "${manifest}") {
			return Profile{}, invalid("language server %q argument %q uses a substitution that has no value for a language server", name, a)
		}
	}
	return Profile{
		Definition:        def,
		Executable:        approval.Executable,
		VersionConstraint: approval.VersionConstraint,
		Checksum:          approval.Checksum,
		Args:              slices.Clone(args),
		EnvAllowlist:      slices.Clone(approval.EnvAllowlist),
		WorkDir:           approval.WorkDir,
		MemoryBudgetBytes: approval.MemoryBudgetBytes,
		DiskBudgetBytes:   approval.DiskBudgetBytes,
		Timeout:           approval.Timeout.Std(),
	}, nil
}

// argv applies the typed substitutions to the profile's arguments. Only the
// two the closed set defines for a server are meaningful: ${input_dir} is the
// materialization root and ${work_dir} the private working directory.
func (p Profile) argv(inputDir string) []string {
	out := make([]string, len(p.Args))
	for i, a := range p.Args {
		a = strings.ReplaceAll(a, "${input_dir}", inputDir)
		a = strings.ReplaceAll(a, "${work_dir}", p.WorkDir)
		out[i] = a
	}
	return out
}

// env builds the child's complete environment from the allowlist. A variable
// the parent does not have is absent, not empty.
func (p Profile) env() []string {
	var out []string
	for _, name := range p.EnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// checksum reads the executable once and returns its SHA-256. When the
// profile pins one, a mismatch is a trust failure: the file at the approved
// path is not the file the operator approved.
func (p Profile) checksum() (string, error) {
	f, err := os.Open(p.Executable)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", unavailable("language server %q is not installed at %s", p.Name, p.Executable).WithDetail("profile", p.Name)
		}
		return "", trustRequired("language server %q cannot be inspected: %v", p.Name, err).WithDetail("profile", p.Name)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", trustRequired("language server %q cannot be read: %v", p.Name, err).WithDetail("profile", p.Name)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if p.Checksum != "" && p.Checksum != sum {
		return "", trustRequired("language server %q at %s does not match its approved checksum", p.Name, p.Executable).
			WithDetail("profile", p.Name).WithDetail("observed_checksum", sum)
	}
	return sum, nil
}

// versionMatches reports whether a server-reported version satisfies the
// constraint. Servers report free-form strings: "v0.23.0", "1.80.0 (abc
// 2024-01-01)", "clangd version 18.1.3", and gopls a compact JSON blob whose
// Main.Version holds the tag. The constraint therefore matches when it
// occurs in the report as a whole version token: preceded by nothing, a
// non-version character or a "v" prefix, and followed by nothing, a further
// ".x" component, a "-pre" suffix or a non-version character. "0.23" accepts
// "v0.23.1" and rejects "10.23" and "0.230".
func versionMatches(constraint, reported string) bool {
	c := strings.TrimPrefix(strings.TrimSpace(constraint), "v")
	if c == "" || reported == "" {
		return false
	}
	isVersionByte := func(b byte) bool {
		return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	for i := 0; ; {
		j := strings.Index(reported[i:], c)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(c)
		before := start == 0 || !isVersionByte(reported[start-1]) && reported[start-1] != '.' ||
			reported[start-1] == 'v' && (start == 1 || !isVersionByte(reported[start-2]))
		after := end == len(reported) || reported[end] == '.' || reported[end] == '-' || !isVersionByte(reported[end])
		if before && after {
			return true
		}
		i = start + 1
	}
}

// serverVersion reduces what a server reports in serverInfo.version to the
// label the overlay carries. gopls reports a compact JSON build description
// whose top-level Version field holds the tag; other servers report a short
// string. The label is bounded to model.MaxIdentifierBytes so the overlay
// binding validates; the full report still feeds the input digest.
func serverVersion(reported string) string {
	v := strings.TrimSpace(reported)
	if strings.HasPrefix(v, "{") {
		var build struct {
			Version string `json:"Version"`
		}
		if json.Unmarshal([]byte(v), &build) == nil && build.Version != "" {
			v = build.Version
		}
	}
	if len(v) > model.MaxIdentifierBytes {
		v = v[:model.MaxIdentifierBytes]
	}
	return v
}

// inputDigest is the overlay label's input hash: the snapshot's identity and
// manifest, the profile and how it was started, and the server the handshake
// revealed. Two overlays with equal digests answered from the same bytes with
// the same tool; a different digest is a different question.
func inputDigest(snap model.Snapshot, p Profile, checksum, serverVersion, encoding string) string {
	h := model.NewHasher("lsp-overlay-v1")
	h.AddString(string(snap.ID))
	h.AddString(snap.ManifestHash)
	h.AddString(p.Name)
	h.AddString(p.Executable)
	h.AddString(checksum)
	h.AddString(serverVersion)
	h.AddString(encoding)
	for _, a := range p.Args {
		h.AddString(a)
	}
	return h.Sum()
}
