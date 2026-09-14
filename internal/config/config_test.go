package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// loadFixture is the one shared configuration safety fixture: it lays out a
// user configuration directory and a repository root, writes the two optional
// files and resolves the layered configuration from them.
func loadFixture(t *testing.T, userTOML, projectTOML string) (Config, error) {
	t.Helper()
	base := t.TempDir()
	userDir := filepath.Join(base, "config")
	root := filepath.Join(base, "repo")
	mustMkdir(t, filepath.Join(userDir, appDirName))
	mustMkdir(t, root)
	if userTOML != "" {
		mustWrite(t, filepath.Join(userDir, appDirName, userConfigName), userTOML)
	}
	if projectTOML != "" {
		mustWrite(t, filepath.Join(root, ProjectConfigName), projectTOML)
	}
	userConfigDir = func() (string, error) { return userDir, nil }
	userCacheDir = func() (string, error) { return filepath.Join(base, "cache"), nil }
	t.Cleanup(func() {
		userConfigDir = os.UserConfigDir
		userCacheDir = os.UserCacheDir
	})
	return Load(root)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLoadTrustAndBudgets protects the three configuration failures that are
// silent and unrecoverable if they are not rejected at load time:
//
//   - an unknown key is a typo or a speculative setting that the user believes
//     is in effect while the tool ignores it;
//   - a project file that grants execution, weakens a safety toggle or raises a
//     ceiling is a repository escalating its own trust (Section 20.2); and
//   - a resolved configuration whose related budgets disagree produces
//     responses that cannot fit their own wire ceiling (Section 20.1); and
//   - a tool override without its checksum admits whatever binary happens to
//     sit at that path on the next run (Section 20.2).
//
// The first case also pins the shipped defaults: if they ever stop satisfying
// their own cross-field validation, every command fails at startup.
func TestLoadTrustAndBudgets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     string
		project  string
		wantCode string
	}{
		{
			name: "shipped defaults resolve",
		},
		{
			name:     "unknown project key",
			project:  "version = 1\n[workspace]\nindex_vendr = true\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			name:     "unknown user key",
			user:     "[resources]\nmax_memmory = 1\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			name:     "project weakens a safety toggle",
			project:  "[workspace]\nfollow_symlinks = true\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			name:     "project authorizes an executable",
			project:  "[analyzers.scip]\nexecutable = \"/usr/bin/scip\"\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			name:     "project disables the strict read gate",
			project:  "[context]\nstrict_read_gate = false\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			name:     "project raises a ceiling it may only lower",
			project:  "[workspace]\nmax_files = 500000\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			name:     "source chunk cannot fit the source response budget",
			user:     "[coverage]\nmax_chunk_bytes = 1048576\n[resources]\nmax_source_response_bytes = 262144\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			// The file budget is a positive finite bound and nothing more.
			// Comparing it to a coverage-capsule ceiling was a category error
			// that capped a large repository at an unrelated number.
			name: "a large file budget is a budget, not a capsule ceiling",
			user: "[workspace]\nmax_files = 2000000\n",
		},
		{
			// There is no shell, so "}" in an argv element is a literal.
			name: "a literal brace in an analyzer argument is not a substitution",
			user: "[analyzers.demo]\nexecutable = \"/usr/local/bin/demo\"\nversion_constraint = \">=1.0\"\nwork_dir = \"/tmp/demo\"\nmemory_budget_bytes = 1048576\ndisk_budget_bytes = 1048576\ntimeout = \"30s\"\nnetwork = \"denied\"\nargs = [\"--opt={a}\", \"${input_dir}\"]\n",
		},
		{
			name:     "an unknown substitution is rejected",
			user:     "[analyzers.demo]\nexecutable = \"/usr/local/bin/demo\"\nversion_constraint = \">=1.0\"\nwork_dir = \"/tmp/demo\"\nmemory_budget_bytes = 1048576\ndisk_budget_bytes = 1048576\ntimeout = \"30s\"\nnetwork = \"denied\"\nargs = [\"${output_dr}\"]\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			name:     "zero is not unlimited",
			user:     "[resources]\nmax_concurrent_queries = 0\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			// The toolchain decides which binaries this build executes, so a
			// repository that could set any of it would choose them.
			name:     "project sets a tools key",
			project:  "[tools]\noffline = true\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			// An override is the one binary the lock does not describe. Without
			// its checksum nothing about it is verifiable at run start.
			name:     "a tool override without a checksum is rejected",
			user:     "[tools.override.scip-go]\nexecutable = \"/opt/codectx/scip-go\"\nversion = \"0.5.0\"\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			// Retention is by ref: 0 retained refs would prune the results the
			// active ref is being served from.
			name:     "retain_refs 0 is rejected",
			user:     "[index]\nretain_refs = 0\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			// 0 means the machine-derived allocation; a negative value is not a
			// third meaning, and admitting one would size every unit from it.
			name:     "a negative dependence memory ceiling is rejected",
			user:     "[providers.dependence]\nunit_memory_ceiling_bytes = -1\n",
			wantCode: model.CodeConfigInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadFixture(t, tc.user, tc.project)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Load returned %v, want the shipped defaults to resolve", err)
				}
				if cfg.Storage.DataDir == "" {
					t.Fatal("Load left storage.data_dir empty; every consumer would re-derive it")
				}
				if !filepath.IsAbs(cfg.Storage.DataDir) {
					t.Fatalf("storage.data_dir %q is not absolute", cfg.Storage.DataDir)
				}
				return
			}
			var typed *model.Error
			if !errors.As(err, &typed) {
				t.Fatalf("Load returned %v, want a typed %s", err, tc.wantCode)
			}
			if typed.Code != tc.wantCode {
				t.Fatalf("Load returned %s (%s), want %s", typed.Code, typed.Message, tc.wantCode)
			}
		})
	}
}

// TestProjectPermittedFieldsApply proves the permitted half of the trust split
// actually takes effect: rejecting everything would pass the table above.
func TestProjectPermittedFieldsApply(t *testing.T) {
	cfg, err := loadFixture(t, "", "version = 1\n[workspace]\nindex_vendor = true\nmax_files = 1000\n"+
		"[providers.tree_sitter]\nlanguages = [\"go\"]\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Workspace.IndexVendor {
		t.Error("project workspace.index_vendor was not applied")
	}
	if cfg.Workspace.MaxFiles != 1000 {
		t.Errorf("project workspace.max_files is %d, want 1000", cfg.Workspace.MaxFiles)
	}
	// A configured list replaces the default list. If decoding overwrote a
	// prefix and kept the tail, every fingerprint and every grammar selection
	// would silently include languages nobody asked for.
	if got := cfg.Providers.TreeSitter.Languages; len(got) != 1 || got[0] != "go" {
		t.Errorf("providers.tree_sitter.languages = %v, want exactly [go]", got)
	}
}

// TestFingerprintsCoverEligibilityInputs protects the two fingerprint inputs
// that are invisible in the configuration file itself. If either is left out,
// a stored result is reused under a policy it was never produced under, which
// is exactly the silent reuse Section 20.2 forbids.
func TestFingerprintsCoverEligibilityInputs(t *testing.T) {
	cfg := Defaults()
	// The built-in vendor and generated lists decide which files exist at all,
	// so a build that ships different ones must not match this fingerprint.
	bare := model.H(domainSourcePolicy,
		quoteBool(cfg.Workspace.FollowSymlinks),
		quoteBool(cfg.Workspace.IncludeUntracked),
		quoteBool(cfg.Workspace.IndexGenerated),
		quoteBool(cfg.Workspace.IndexVendor),
		quoteInt(cfg.Workspace.MaxFiles),
	)
	if cfg.SourcePolicyHash() == bare {
		t.Error("SourcePolicyHash covers only the configured toggles; the built-in exclusion lists are not an input")
	}

	// An analyzer's environment allowlist changes what the analyzer can
	// resolve, so two profiles differing only in it are not interchangeable.
	withHome := cfg
	withHome.Analyzers = map[string]Analyzer{"demo": {Name: "demo", EnvAllowlist: []string{"HOME"}}}
	withPath := cfg
	withPath.Analyzers = map[string]Analyzer{"demo": {Name: "demo", EnvAllowlist: []string{"HOME", "PATH"}}}
	if withHome.AnalysisConfigHash() == withPath.AnalysisConfigHash() {
		t.Error("AnalysisConfigHash ignores analyzers.*.env_allowlist")
	}
}
