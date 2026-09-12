// Package joern is the optional Section 11.6 deep-analysis adapter: it runs
// an approved local Joern installation's built-in noninteractive tools
// (joern-parse, joern-export) through the shared process runner against a
// private materialization of the pinned snapshot, streams the exported
// Neo4j CSV and GraphML with every field, record and depth bounded before a
// standard decoder can allocate, stages the graph in an on-disk scratch
// database so forward edges are never dropped, and publishes only the
// relations the pinned profile owns: calls, control dependence and
// intraprocedural data dependence between methods.
//
// There is no product-owned Scala or shell helper and no interpreter server.
// A missing tool is honest absence; a timeout, a malformed export or an
// unsupported profile fails the unit, and provider.RunUnit deletes whatever
// it wrote. A dependence edge is published as exactly that: it never claims a
// proven source-to-sink flow.
package joern

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// ProfileName is the only built-in profile this build knows. Its argument
// arrays follow the current Joern documentation and are unverified against a
// real installation (Ruling R11-1); docs/providers-joern.md carries the smoke
// commands a maintainer runs before declaring it supported.
const ProfileName = "pinned-default"

// Analyzer table names the profile reads its two executables from
// (Section 20.2 `[analyzers.<name>]`). Both are user-configuration-only.
const (
	AnalyzerParse  = "joern-parse"
	AnalyzerExport = "joern-export"
)

// Typed argv placeholders. The arrays are owned by this package for the
// pinned profile, never by configuration, so a profile can never turn a
// repository-controlled value into a different argument shape; nothing
// outside the package substitutes them, so they stay unexported.
const (
	placeholderInputDir = "${input_dir}"
	placeholderCPG      = "${cpg}"
	placeholderOutDir   = "${out_dir}"
)

// Tool is one approved executable of the profile.
type Tool struct {
	// Path is the absolute executable path; PATH lookup is never an approval.
	Path string
	// Checksum is the optional expected lowercase SHA-256 of the executable.
	// Pin it: a Joern upgraded in place at the same path would otherwise be
	// admitted under the same unit identity.
	Checksum string
	// VersionConstraint is matched against the version probe: an exact
	// version, optionally prefixed with "v", or a "major.minor.*" wildcard.
	VersionConstraint string
	// Timeout bounds one invocation of this tool.
	Timeout time.Duration
	// MemoryBudgetBytes and DiskBudgetBytes are the reservations the runner
	// accounts before the child starts and the disk bound this provider
	// enforces on what the tool leaves in the work directory.
	MemoryBudgetBytes int64
	DiskBudgetBytes   int64
}

// Profile is the complete typed description of how Joern is run and read.
type Profile struct {
	Name          string
	Parse, Export Tool
	// VersionArgs is the version probe run at detection for both tools.
	VersionArgs []string
	// ParseArgs produces the CPG from ${input_dir} at ${cpg}.
	ParseArgs []string
	// ExportAllArgs exports every node and edge of ${cpg} as Neo4j CSV into
	// ${out_dir}; ExportPDGArgs exports the program dependence graph as
	// GraphML into ${out_dir}.
	ExportAllArgs []string
	ExportPDGArgs []string
	// WorkDir is the absolute private directory every run works under. Each
	// run gets its own subdirectory, removed on every termination path.
	WorkDir string
	// EnvAllowlist names the parent environment variables the tools may see
	// (JAVA_HOME, PATH for the java launcher); nothing else is inherited.
	EnvAllowlist []string
	// Network is the declared posture; the provider requires denied. It is a
	// policy statement, not an OS sandbox (Section 21).
	Network config.NetworkPolicy
	// Timeout bounds one whole unit: parse, both exports and the import.
	Timeout time.Duration
}

// PinnedDefault returns the built-in argument arrays of ProfileName with no
// executables, work directory or budgets; ProfileFromConfig fills those from
// the approved analyzer table.
func PinnedDefault() Profile {
	return Profile{
		Name:          ProfileName,
		VersionArgs:   []string{"--version"},
		ParseArgs:     []string{placeholderInputDir, "--output", placeholderCPG},
		ExportAllArgs: []string{placeholderCPG, "--repr=all", "--format=neo4jcsv", "--out", placeholderOutDir},
		ExportPDGArgs: []string{placeholderCPG, "--repr=pdg", "--format=graphml", "--out", placeholderOutDir},
		Network:       config.NetworkDenied,
	}
}

// ProfileFromConfig builds the profile the configuration selects:
// providers.joern.profile names the built-in argument set, and the
// `joern-parse` and `joern-export` analyzer entries supply the approved
// executables, checksums, version constraints, budgets, timeouts, the shared
// work directory and the environment allowlist. An analyzer entry with its
// own args is refused: the argv is product-owned for the pinned profile, and
// a profile this build does not know is unsupported rather than guessed.
func ProfileFromConfig(cfg config.Config) (Profile, error) {
	if cfg.Providers.Joern.Profile != ProfileName {
		return Profile{}, configInvalid("providers.joern.profile %q is not a profile this build supports; the only pinned profile is %q",
			cfg.Providers.Joern.Profile, ProfileName)
	}
	p := PinnedDefault()
	p.Timeout = cfg.Providers.Joern.Timeout.Std()
	tools := []struct {
		name string
		dst  *Tool
	}{{AnalyzerParse, &p.Parse}, {AnalyzerExport, &p.Export}}
	for i, t := range tools {
		a, ok := cfg.Analyzers[t.name]
		if !ok {
			return Profile{}, configInvalid("analyzers.%s is not configured; the joern provider needs both %s and %s approved in the user configuration",
				t.name, AnalyzerParse, AnalyzerExport)
		}
		if len(a.Args) > 0 {
			return Profile{}, configInvalid("analyzers.%s.args must be empty; the %s profile owns the exact argument arrays", t.name, ProfileName)
		}
		if a.Network != config.NetworkDenied {
			return Profile{}, configInvalid("analyzers.%s.network must be %q for the joern provider", t.name, config.NetworkDenied)
		}
		if i == 0 {
			p.WorkDir = a.WorkDir
		} else if a.WorkDir != p.WorkDir {
			return Profile{}, configInvalid("analyzers.%s.work_dir differs from analyzers.%s.work_dir; both tools share one private work directory", AnalyzerExport, AnalyzerParse)
		}
		*t.dst = Tool{Path: a.Executable, Checksum: a.Checksum, VersionConstraint: a.VersionConstraint, Timeout: a.Timeout.Std(),
			MemoryBudgetBytes: a.MemoryBudgetBytes, DiskBudgetBytes: a.DiskBudgetBytes}
		p.EnvAllowlist = append(p.EnvAllowlist, a.EnvAllowlist...)
	}
	slices.Sort(p.EnvAllowlist)
	p.EnvAllowlist = slices.Compact(p.EnvAllowlist)
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Validate enforces the profile shape before anything is stat'ed or run.
func (p Profile) Validate() error {
	if p.Name == "" || len(p.Name) > model.MaxIdentifierBytes {
		return configInvalid("joern profile name is required and bounded to %d bytes", model.MaxIdentifierBytes)
	}
	for _, t := range []struct {
		name string
		tool Tool
	}{{"parse", p.Parse}, {"export", p.Export}} {
		if !filepath.IsAbs(t.tool.Path) || strings.ContainsRune(t.tool.Path, 0) {
			return configInvalid("joern %s executable %q is not an absolute path", t.name, t.tool.Path)
		}
		if t.tool.Checksum != "" && !model.ValidHexID(t.tool.Checksum) {
			return configInvalid("joern %s checksum is not %d lowercase hex characters", t.name, model.IDHexLen)
		}
		if t.tool.VersionConstraint == "" {
			return configInvalid("joern %s version constraint is required", t.name)
		}
		if t.tool.Timeout <= 0 || t.tool.MemoryBudgetBytes <= 0 || t.tool.DiskBudgetBytes <= 0 {
			return configInvalid("joern %s needs a positive timeout, memory budget and disk budget", t.name)
		}
	}
	if !filepath.IsAbs(p.WorkDir) || strings.ContainsRune(p.WorkDir, 0) {
		return configInvalid("joern work_dir %q is not an absolute private directory", p.WorkDir)
	}
	if p.Network != config.NetworkDenied {
		return configInvalid("joern network policy must be %q", config.NetworkDenied)
	}
	if p.Timeout <= 0 {
		return configInvalid("joern unit timeout must be positive")
	}
	for _, args := range [][]string{p.VersionArgs, p.ParseArgs, p.ExportAllArgs, p.ExportPDGArgs} {
		if len(args) == 0 {
			return configInvalid("joern profile %q has an empty argument array", p.Name)
		}
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return configInvalid("joern profile %q has an argument containing a NUL byte", p.Name)
			}
		}
	}
	for _, name := range p.EnvAllowlist {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return configInvalid("joern env allowlist entry %q is not an environment variable name", name)
		}
	}
	return nil
}

// substitute fills the typed placeholders of one argument array. A
// placeholder with no value is a defect in this package, not a literal.
func substitute(args []string, values map[string]string) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, a := range args {
		s := a
		for k, v := range values {
			s = strings.ReplaceAll(s, k, v)
		}
		if strings.Contains(s, "${") {
			return nil, &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf("joern argument %q has an unfilled placeholder", a)}
		}
		out = append(out, s)
	}
	return out, nil
}

// versionMatches reports whether an observed version satisfies a constraint:
// equal after trimming a leading "v", or a "major.minor.*" style wildcard.
func versionMatches(constraint, observed string) bool {
	c := strings.TrimPrefix(strings.TrimSpace(constraint), "v")
	o := strings.TrimPrefix(strings.TrimSpace(observed), "v")
	if c == "" || o == "" {
		return false
	}
	if prefix, ok := strings.CutSuffix(c, ".*"); ok {
		return o == prefix || strings.HasPrefix(o, prefix+".")
	}
	return c == o
}

// versionOf extracts the version token from a probe's output: the last
// whitespace-separated field of the first non-empty line that begins with a
// digit or "v" followed by a digit ("joern-parse 2.0.448", "Joern version
// v4.0.100", "2.0.448").
func versionOf(output []byte) string {
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		for i := len(fields) - 1; i >= 0; i-- {
			f := strings.TrimPrefix(fields[i], "v")
			if f != "" && f[0] >= '0' && f[0] <= '9' {
				return f
			}
		}
		if len(fields) > 0 {
			return ""
		}
	}
	return ""
}

func configInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeConfigInvalid, Message: fmt.Sprintf(format, args...)}
}
