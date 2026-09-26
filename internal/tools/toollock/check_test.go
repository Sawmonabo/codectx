package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestCheckReportsEveryDifferingEntry protects the release gate's completeness.
//
// Requirement: -check is the only thing that says "the lock describes what is
// actually published", so it reports EVERY entry that differs.
//
// Mutation that fails it: return on the first differing entry. A re-pin that
// moved several payloads then reports one of them, and whoever reads the
// failure cannot tell a single moved asset from a whole release republished
// under different bytes -- each hidden entry costing another multi-gigabyte run
// of the job to discover.
func TestCheckReportsEveryDifferingEntry(t *testing.T) {
	lock := Lock{LockVersion: 1, Tools: map[string]Entry{
		"alpha": {Name: "alpha", Platforms: map[string]Payload{
			"linux_amd64":  {URL: "https://example.invalid/alpha-linux", SHA256: "aaa", Size: 11},
			"darwin_arm64": {URL: "https://example.invalid/alpha-darwin", SHA256: "bbb", Size: 22},
		}},
		"beta": {Name: "beta", Platforms: map[string]Payload{
			"linux_amd64": {URL: "https://example.invalid/beta-linux", SHA256: "ccc", Size: 33},
		}},
	}}
	// alpha/darwin_arm64 matches; the other two do not. A first-differing-entry
	// loop would stop inside alpha and never reach beta.
	check := func(_ context.Context, _ Entry, p Payload) error {
		if p.SHA256 == "bbb" {
			return nil
		}
		return fmt.Errorf("payload sha256 live-%s, lock says %s", p.SHA256, p.SHA256)
	}
	checked, bytesRead, differing := checkAll(context.Background(), lock, nil, false, check)
	if checked != 1 || bytesRead != 22 {
		t.Errorf("checked %d payloads holding %d bytes, want the one matching entry and its 22 bytes",
			checked, bytesRead)
	}
	if len(differing) != 2 {
		t.Fatalf("checkAll reported %d differing entries %v, want both", len(differing), differing)
	}
	for _, want := range []string{"alpha/linux_amd64", "beta/linux_amd64"} {
		found := false
		for _, got := range differing {
			if strings.HasPrefix(got, want+": ") && strings.Contains(got, "live-") {
				found = true
			}
		}
		if !found {
			t.Errorf("no line naming %s with its live and expected digest in %v", want, differing)
		}
	}
}
