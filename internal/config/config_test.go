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
//     responses that cannot fit their own wire ceiling (Section 20.1).
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
			name:     "zero is not unlimited",
			user:     "[resources]\nmax_concurrent_queries = 0\n",
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
