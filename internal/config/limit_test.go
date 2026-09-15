package config

import "testing"

// TestLimitUnlimitedSpellingsAreOneIdentity protects the fingerprint invariant
// the whole unlimited posture rests on: `0` and "unlimited" are the same absent
// bound, so a workspace configured either way must reuse the other's stored
// results, and neither may collide with a workspace that really does have a
// bound. If the two spellings hashed differently, switching a comment in a
// config file would invalidate every snapshot, unit and manifest on disk.
func TestLimitUnlimitedSpellingsAreOneIdentity(t *testing.T) {
	zero := loadLimitFixture(t, "[workspace]\nmax_files = 0\nmax_parse_file_bytes = 0\n[context]\nmax_manifest_bytes = 0\n")
	word := loadLimitFixture(t, "[workspace]\nmax_files = \"unlimited\"\nmax_parse_file_bytes = \"unlimited\"\n"+
		"[context]\nmax_manifest_bytes = \"unlimited\"\n")
	finite := loadLimitFixture(t, "[workspace]\nmax_files = 1000\nmax_parse_file_bytes = 1000\n[context]\nmax_manifest_bytes = 8388608\n")
	for _, h := range []struct {
		name          string
		of            func(Config) string
		zero, word, f string
	}{
		{"SourcePolicyHash", Config.SourcePolicyHash, "", "", ""},
		{"AnalysisConfigHash", Config.AnalysisConfigHash, "", "", ""},
		{"ContextPolicyHash", Config.ContextPolicyHash, "", "", ""},
	} {
		if h.of(zero) != h.of(word) {
			t.Errorf("%s differs between max_files = 0 and \"unlimited\"; the same policy has two identities", h.name)
		}
		if h.of(zero) == h.of(finite) {
			t.Errorf("%s is the same unlimited and at a finite bound; a bound is not a fingerprint input", h.name)
		}
	}
	if !zero.Workspace.MaxFiles.IsUnlimited() || !word.Workspace.MaxFiles.IsUnlimited() {
		t.Fatal("neither spelling decoded to an unlimited bound")
	}
}

// TestLimitExceededAndMin protects the two predicates every enforcement site in
// L1-L5 calls. The failure mode is an unlimited bound read as a zero-sized one,
// which turns "index everything" into "index nothing" at every call site at
// once, and a Min that treats unlimited as the bottom of the lattice, which
// would let an absent bound silently narrow a configured one.
func TestLimitExceededAndMin(t *testing.T) {
	if Unlimited.Exceeded(1 << 40) {
		t.Error("an unlimited bound reported a value as exceeding it")
	}
	if got := Limit(10); got.Exceeded(10) || !got.Exceeded(11) {
		t.Error("Exceeded is not strictly greater-than at the boundary")
	}
	if got := Unlimited.Min(Limit(5)); got != 5 {
		t.Errorf("Unlimited.Min(5) = %s, want 5; unlimited must be the top of the lattice", got)
	}
	if got := Limit(5).Min(Unlimited); got != 5 {
		t.Errorf("Limit(5).Min(Unlimited) = %s, want 5", got)
	}
	if got := Unlimited.Min(Unlimited); !got.IsUnlimited() {
		t.Errorf("Unlimited.Min(Unlimited) = %s, want unlimited", got)
	}
	if got := Unlimited.ValueOr(64); got != 64 {
		t.Errorf("Unlimited.ValueOr(64) = %d, want the fallback 64, not a zero-sized budget", got)
	}
	if got := Limit(7).ValueOr(64); got != 7 {
		t.Errorf("Limit(7).ValueOr(64) = %d, want 7", got)
	}
}

func loadLimitFixture(t *testing.T, user string) Config {
	t.Helper()
	cfg, err := loadFixture(t, user, "")
	if err != nil {
		t.Fatalf("Load(%q): %v", user, err)
	}
	return cfg
}
