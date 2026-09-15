package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
			name:     "project disables the strict read gate",
			project:  "[context]\nstrict_read_gate = false\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			// Against the unlimited default this file would be NARROWING, which
			// a project may do; the escalation is only against a ceiling the
			// user actually set, so the case has to set one.
			name:     "project raises a ceiling it may only lower",
			user:     "[workspace]\nmax_files = 1000\n",
			project:  "[workspace]\nmax_files = 500000\n",
			wantCode: model.CodeTrustRequired,
		},
		{
			// Unlimited is the TOP of the lattice: proposing it over a finite
			// user ceiling removes the ceiling, which is an escalation even
			// though the proposed number is the smaller one.
			name:     "project proposes unlimited over a finite user ceiling",
			user:     "[workspace]\nmax_files = 1000\n",
			project:  "[workspace]\nmax_files = \"unlimited\"\n",
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
			// Retention is by ref, and 0 is "keep every ref" now, matching
			// index.max_retained_bytes. Only a negative value is refused: it is
			// not a third meaning.
			name:     "a negative bound is rejected",
			user:     "[index]\nretain_refs = -1\n",
			wantCode: model.CodeConfigInvalid,
		},
		{
			// A reservation is not a bound. 0 records per batch is a broken
			// reservation, not an unbounded one, so it stays refused -- this is
			// what keeps the `< 0` rule for bounds from leaking into sizing.
			name:     "a zero reservation is rejected",
			user:     "[index]\nbatch_records = 0\n",
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
		quoteInt(cfg.Workspace.MaxFiles.Value()),
	)
	if cfg.SourcePolicyHash() == bare {
		t.Error("SourcePolicyHash covers only the configured toggles; the built-in exclusion lists are not an input")
	}
}

// TestTraversalPolicyCarriesEveryTraversalBound protects the "every key is
// read" invariant for the four workspace bounds the traversal owns. Three of
// them were dead fields: configured, validated, documented and never carried
// into the policy, so the walk's own SkipDirEntries and SkipDepth reports were
// unreachable and an operator had no escape hatch over a pathological tree.
func TestTraversalPolicyCarriesEveryTraversalBound(t *testing.T) {
	c := Defaults()
	c.Workspace.MaxFiles = 11
	c.Workspace.MaxDirEntries = 22
	c.Workspace.MaxDepth = 33
	c.Workspace.MaxIgnoredRoots = 44
	p := c.TraversalPolicy()
	for _, tc := range []struct {
		key  string
		got  int64
		want int64
	}{
		{"workspace.max_files", p.MaxFiles, 11},
		{"workspace.max_dir_entries", p.MaxDirEntries, 22},
		{"workspace.max_depth", p.MaxDepth, 33},
		{"workspace.max_ignored_roots", p.MaxIgnoredRoots, 44},
	} {
		if tc.got != tc.want {
			t.Errorf("TraversalPolicy carried %s as %d, want %d: the key is configured and documented but unread",
				tc.key, tc.got, tc.want)
		}
	}
	// Unlimited is the default and must travel as 0, which the traversal reads
	// as no bound at all rather than as a bound of zero.
	if d := Defaults().TraversalPolicy(); d.MaxFiles != 0 || d.MaxDirEntries != 0 || d.MaxDepth != 0 || d.MaxIgnoredRoots != 0 {
		t.Errorf("the default traversal policy carries bounds %+v, want every one unlimited (0)", d)
	}
}

// storage.synchronous decides whether every store commit fsyncs the
// write-ahead log, so the default it resolves to is a durability promise and an
// unrecognized spelling must be refused at load time rather than silently
// mapped onto one of the two modes (docs/adr/ADR-0004-wal-synchronous-mode.md).
func TestStorageSynchronous(t *testing.T) {
	cfg, err := loadFixture(t, "", "")
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if cfg.Storage.Synchronous != SynchronousNormal {
		t.Fatalf("default storage.synchronous = %q, want %q", cfg.Storage.Synchronous, SynchronousNormal)
	}
	if cfg, err = loadFixture(t, "[storage]\nsynchronous = \"full\"\n", ""); err != nil {
		t.Fatalf("load full: %v", err)
	}
	if cfg.Storage.Synchronous != SynchronousFull {
		t.Fatalf("storage.synchronous = %q, want %q", cfg.Storage.Synchronous, SynchronousFull)
	}
	_, err = loadFixture(t, "[storage]\nsynchronous = \"off\"\n", "")
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeConfigInvalid {
		t.Fatalf("synchronous = off: got %v, want CTX_CONFIG_INVALID", err)
	}
	if want := `storage.synchronous is "off"; use "normal" or "full"`; typed.Message != want {
		t.Fatalf("message = %q, want %q", typed.Message, want)
	}
}

// TestEvidenceClipIsUserSetAndRekeysUnits protects index.max_evidence_per_fact,
// the only setting that removes evidence occurrences from a sealed fact. Three
// silent failures: a default that clips (a fact would lose occurrences nobody
// asked to lose), a value above the record ceiling accepted as if it did
// something, and a clip left out of the analysis key (units sealed under a clip
// would be reused for a run that asked for every occurrence).
func TestEvidenceClipIsUserSetAndRekeysUnits(t *testing.T) {
	absent, err := loadFixture(t, "", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !absent.Index.MaxEvidencePerFact.IsUnlimited() {
		t.Errorf("default index.max_evidence_per_fact is %s, want unlimited", absent.Index.MaxEvidencePerFact)
	}
	// A repository's own file may narrow what this tool retains for it.
	project, err := loadFixture(t, "", "version = 1\n[index]\nmax_evidence_per_fact = 3\n")
	if err != nil {
		t.Fatalf("Load with project clip: %v", err)
	}
	if project.Index.MaxEvidencePerFact != 3 {
		t.Errorf("project index.max_evidence_per_fact is %d, want 3", project.Index.MaxEvidencePerFact)
	}
	if project.AnalysisConfigHash() == absent.AnalysisConfigHash() {
		t.Error("a user-set evidence clip left AnalysisConfigHash unchanged; units sealed with fewer occurrences stay cached")
	}
	zero, err := loadFixture(t, "", "version = 1\n[index]\nmax_evidence_per_fact = 0\n")
	if err != nil {
		t.Fatalf("Load with explicit 0: %v", err)
	}
	if zero.AnalysisConfigHash() != absent.AnalysisConfigHash() {
		t.Error("an explicit 0 re-keyed every unit; 0 is the default and means unlimited")
	}
	_, err = loadFixture(t, "[index]\nmax_evidence_per_fact = 65537\n", "")
	var ctxErr *model.Error
	if !errors.As(err, &ctxErr) || ctxErr.Code != model.CodeConfigInvalid {
		t.Fatalf("a clip above the record ceiling was accepted: err = %v", err)
	}
	if !strings.Contains(ctxErr.Message, "index.max_evidence_per_fact") {
		t.Errorf("rejection does not name the key: %s", ctxErr.Message)
	}
}
