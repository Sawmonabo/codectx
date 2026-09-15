package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/Sawmonabo/codectx/internal/model"
)

// ProjectConfigName is the optional per-repository file. It is written by
// whoever can write the repository, so it carries the least trust.
const ProjectConfigName = ".codectx.toml"

const (
	appDirName     = "codectx"
	userConfigName = "config.toml"
	// toolStoreDirName is the store's directory name under the user data
	// directory. It matches the name the toolchain package uses under a data
	// directory and the one the installer's bundle path creates.
	toolStoreDirName = "tools"
)

// userConfigDir, userCacheDir and userDataDir are indirected so the layered
// resolution can be exercised without depending on the developer's real home
// directory.
var (
	userConfigDir = os.UserConfigDir
	userCacheDir  = os.UserCacheDir
	userDataDir   = osUserDataDir
)

// osUserDataDir is the XDG user data directory. The standard library has no
// os.UserDataDir, and the installer already writes the shared tool store to
// ${XDG_DATA_HOME:-$HOME/.local/share}/codectx/tools, so this resolves the same
// base by the same rule. A relative XDG_DATA_HOME is ignored rather than
// honoured: the specification requires an absolute path, and a relative one
// would produce a tool store that changes with the process working directory
// and that validate rejects for not being absolute.
func osUserDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// projectPermitted is the closed set of keys a repository's own file may set:
// source inclusion, language selection and safe query preferences (Section
// 20.2). Everything else -- executables, argv, environment, shells, network,
// path roots, strict-gate weakening, security ceilings and exploratory waiver
// consolidation -- requires user-level configuration.
//
// The set is deliberately a literal list rather than a rule over field names:
// a rule would silently admit every future key that happened to match it.
var projectPermitted = map[string]bool{
	"version":                          true,
	"workspace.include_untracked":      true,
	"workspace.index_generated":        true,
	"workspace.index_vendor":           true,
	"workspace.max_files":              true,
	"workspace.max_parse_file_bytes":   true,
	"workspace.max_search_file_bytes":  true,
	"index.max_evidence_per_fact":      true,
	"providers.tree_sitter.languages":  true,
	"context.default_phase":            true,
	"context.default_estimated_tokens": true,
	"context.default_max_bytes":        true,
	"context.default_max_files":        true,
}

// Load resolves the configuration for the workspace rooted at root: built-in
// defaults, then the user configuration file, then the permitted fields of the
// project file. Only explicitly present fields are merged at each layer, and
// the complete resolved result is validated once at the end.
func Load(root string) (Config, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Config{}, configInvalid("workspace root %q cannot be resolved: %v", root, err)
	}
	cfg := Defaults()

	userPath, err := UserConfigPath()
	if err != nil {
		return Config{}, err
	}
	if err := mergeUserConfig(&cfg, userPath); err != nil {
		return Config{}, err
	}
	if err := mergeProjectConfig(&cfg, filepath.Join(absRoot, ProjectConfigName)); err != nil {
		return Config{}, err
	}

	if cfg.Storage.DataDir == "" {
		dir, err := DefaultDataDir(absRoot)
		if err != nil {
			return Config{}, err
		}
		cfg.Storage.DataDir = dir
	} else if !filepath.IsAbs(cfg.Storage.DataDir) {
		return Config{}, configInvalid("storage.data_dir %q is not an absolute path", cfg.Storage.DataDir)
	} else {
		cfg.Storage.DataDir = filepath.Clean(cfg.Storage.DataDir)
	}

	// The tool store is resolved after the layered merge, and it is resolved to
	// a machine-wide path rather than to a subdirectory of the data directory
	// above. The payloads are many gigabytes and are identical for every
	// checkout, so a per-workspace store would install them again for each one
	// and leave a fresh repository with nothing installed. `[tools]` is
	// user-configuration-only, so nothing a project file wrote can reach here.
	if cfg.Tools.CacheDir == "" {
		dir, err := DefaultToolStoreDir()
		if err != nil {
			return Config{}, err
		}
		cfg.Tools.CacheDir = dir
	} else {
		if !filepath.IsAbs(cfg.Tools.CacheDir) {
			return Config{}, configInvalid("tools.cache_dir %q is not an absolute path", cfg.Tools.CacheDir)
		}
		cfg.Tools.CacheDir = filepath.Clean(cfg.Tools.CacheDir)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DefaultToolStoreDir is the machine-wide managed tool store: one store shared
// by every workspace on the host, outside every repository and outside the
// per-workspace data directory. It is the same path the installer populates
// from the offline bundle, so a bundle install and an on-demand install fill
// one store rather than two.
//
// Load does not create it. The toolchain owner creates it user-private on the
// first install, which is what keeps a read-only report from materializing a
// directory on a host that has never installed anything.
func DefaultToolStoreDir() (string, error) {
	base, err := userDataDir()
	if err != nil {
		return "", configInvalid("the user data directory is unavailable: %v", err)
	}
	return filepath.Join(base, appDirName, toolStoreDirName), nil
}

// UserConfigPath is the user-level configuration file location. It lives in the
// OS configuration directory, outside any repository.
func UserConfigPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", configInvalid("the user configuration directory is unavailable: %v", err)
	}
	return filepath.Join(dir, appDirName, userConfigName), nil
}

// DefaultDataDir is the user-private cache/state location for one workspace:
// a per-workspace subdirectory of the OS cache directory, always outside the
// repository. The subdirectory name is derived from the canonical hash of the
// absolute root so two checkouts of the same project never share state and a
// path that is not a legal directory name cannot appear in it.
//
// Load does not create the directory. The storage owner creates it with
// user-private permissions when it first opens the database.
func DefaultDataDir(absRoot string) (string, error) {
	base, err := userCacheDir()
	if err != nil {
		return "", configInvalid("the user cache directory is unavailable: %v", err)
	}
	return filepath.Join(base, appDirName, WorkspaceKey(absRoot)), nil
}

// WorkspaceKey is the stable directory-safe name of one workspace root. It is
// a prefix of the canonical hash: long enough that a collision between the
// roots on one machine is not a practical concern, short enough to keep the
// data path well under the platform path limit.
func WorkspaceKey(absRoot string) string {
	return model.H(domainWorkspaceKey, filepath.ToSlash(absRoot))[:16]
}

// mergeUserConfig merges the user file over the defaults. The file is fully
// trusted: it is outside the repository and only the operator writes it.
func mergeUserConfig(cfg *Config, path string) error {
	md, err := decodeFile(path, cfg)
	if err != nil || md == nil {
		return err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return unknownKeyError(path, undecoded)
	}
	return nil
}

// mergeProjectConfig merges the permitted fields of the repository's own file.
// The file is decoded into a scratch Config so that its metadata can be checked
// against the complete schema: a key that exists in the schema but is not
// permitted is an escalation attempt, and a key that exists nowhere is a typo
// or a speculative setting the user believes is in effect.
func mergeProjectConfig(cfg *Config, path string) error {
	var scratch Config
	md, err := decodeFile(path, &scratch)
	if err != nil || md == nil {
		return err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return unknownKeyError(path, undecoded)
	}
	keys := definedLeafKeys(md)
	for _, key := range keys {
		if !projectPermitted[key] {
			return trustRequired("%s sets %q, which only the user configuration may set", ProjectConfigName, key)
		}
	}
	for _, key := range keys {
		if err := applyProjectKey(cfg, &scratch, key); err != nil {
			return err
		}
	}
	return nil
}

// applyProjectKey copies one permitted key from the project file. Numeric
// limits may only be lowered: a repository may narrow what this tool does to it
// but may never raise a ceiling the user configured.
func applyProjectKey(cfg, scratch *Config, key string) error {
	switch key {
	case "version":
		cfg.Version = scratch.Version
	case "workspace.include_untracked":
		cfg.Workspace.IncludeUntracked = scratch.Workspace.IncludeUntracked
	case "workspace.index_generated":
		cfg.Workspace.IndexGenerated = scratch.Workspace.IndexGenerated
	case "workspace.index_vendor":
		cfg.Workspace.IndexVendor = scratch.Workspace.IndexVendor
	case "workspace.max_files":
		return lowerOnlyLimit(key, &cfg.Workspace.MaxFiles, scratch.Workspace.MaxFiles)
	case "workspace.max_parse_file_bytes":
		return lowerOnlyLimit(key, &cfg.Workspace.MaxParseFileBytes, scratch.Workspace.MaxParseFileBytes)
	case "workspace.max_search_file_bytes":
		return lowerOnlyLimit(key, &cfg.Workspace.MaxSearchFileBytes, scratch.Workspace.MaxSearchFileBytes)
	case "index.max_evidence_per_fact":
		return lowerOnlyLimit(key, &cfg.Index.MaxEvidencePerFact, scratch.Index.MaxEvidencePerFact)
	case "providers.tree_sitter.languages":
		cfg.Providers.TreeSitter.Languages = scratch.Providers.TreeSitter.Languages
	case "context.default_phase":
		cfg.Context.DefaultPhase = scratch.Context.DefaultPhase
	case "context.default_estimated_tokens":
		return lowerOnly(key, &cfg.Context.DefaultEstimatedTokens, scratch.Context.DefaultEstimatedTokens)
	case "context.default_max_bytes":
		return lowerOnly(key, &cfg.Context.DefaultMaxBytes, scratch.Context.DefaultMaxBytes)
	case "context.default_max_files":
		n := int64(cfg.Context.DefaultMaxFiles)
		if err := lowerOnly(key, &n, int64(scratch.Context.DefaultMaxFiles)); err != nil {
			return err
		}
		cfg.Context.DefaultMaxFiles = int(n)
	default:
		// Unreachable: projectPermitted and this switch are the same list, and
		// the caller checks membership first. A mismatch is a defect, not user
		// input, so it is reported as one rather than silently ignored.
		return &model.Error{Code: model.CodeInternal, Message: "permitted project key " + key + " has no merge rule"}
	}
	return nil
}

// lowerOnly applies a project value only when it narrows the effective caller
// budget. It is for the plain positive settings; a bound uses lowerOnlyLimit,
// where 0 does not mean "the smallest possible value".
func lowerOnly(key string, current *int64, proposed int64) error {
	if proposed > *current {
		return trustRequired("%s raises %q from %d to %d; a project file may only lower a limit",
			ProjectConfigName, key, *current, proposed)
	}
	*current = proposed
	return nil
}

// lowerOnlyLimit is lowerOnly over the unlimited-aware ordering. Unlimited is
// the TOP of the lattice, not the bottom: a project file naming any finite
// bound against an unlimited one is narrowing what this tool does to the
// repository, which is exactly what a project file is allowed to do. Only the
// reverse -- proposing unlimited, or a larger finite bound, over a finite one
// -- raises a ceiling the user set, and that is the escalation.
func lowerOnlyLimit(key string, current *Limit, proposed Limit) error {
	if current.IsUnlimited() || (!proposed.IsUnlimited() && proposed <= *current) {
		*current = proposed
		return nil
	}
	return trustRequired("%s raises %q from %s to %s; a project file may only lower a limit",
		ProjectConfigName, key, *current, proposed)
}

// decodeFile decodes path into v, merging only the fields the file actually
// contains. A missing file is not an error: both layers are optional. It
// returns a nil MetaData when there is no file.
func decodeFile(path string, v *Config) (*toml.MetaData, error) {
	md, err := toml.DecodeFile(path, v)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		var parseErr toml.ParseError
		if errors.As(err, &parseErr) {
			return nil, configInvalid("%s is not valid TOML: %s", path, parseErr.ErrorWithPosition())
		}
		return nil, configInvalid("%s cannot be read: %v", path, err)
	}
	return &md, nil
}

// definedLeafKeys returns the sorted dotted value keys the file set, excluding
// the table headers that merely contain them.
func definedLeafKeys(md *toml.MetaData) []string {
	var keys []string
	for _, k := range md.Keys() {
		if md.Type(k...) == "Hash" {
			continue
		}
		keys = append(keys, strings.Join(k, "."))
	}
	sort.Strings(keys)
	return keys
}

// unknownKeyError reports the first unknown key in a stable order. Section 20.1
// requires unknown keys to be rejected: there is no extension namespace, so an
// unrecognized key is always either a typo or a setting from another version
// that this build would silently ignore.
func unknownKeyError(path string, undecoded []toml.Key) error {
	names := make([]string, 0, len(undecoded))
	for _, k := range undecoded {
		names = append(names, k.String())
	}
	sort.Strings(names)
	return configInvalid("%s sets unknown key %q; this build has no extension namespace", path, names[0])
}

// isAbsolutePath reports whether p is a usable absolute path with no embedded
// NUL. filepath.IsAbs alone accepts a NUL-bearing string that the OS rejects
// only at open time.
func isAbsolutePath(p string) bool {
	return p != "" && filepath.IsAbs(p) && !strings.ContainsRune(p, 0)
}
