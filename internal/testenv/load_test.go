package testenv

import "testing"

// TestTheLoadGuardRunsOnAQuietHost protects the guard from becoming a silent
// off switch.
//
// The failure mode: SkipIfLoaded stands in front of every wall-clock ratio
// proof in this repository. A threshold that trips on an idle machine would
// skip all of them, and a skipped test reports no failure -- the regressions
// those ratios forbid would land with a green run. The parse half matters for
// the same reason: a misread figure is a wrong decision in one direction or the
// other.
func TestTheLoadGuardRunsOnAQuietHost(t *testing.T) {
	got, err := parseLoadavg("0.52 1.31 2.04 1/764 12345\n")
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if got != 0.52 {
		t.Errorf("parseLoadavg read %v, want the one-minute figure 0.52", got)
	}
	for _, c := range []struct {
		load float64
		cpus int
		want bool
	}{
		{load: 0, cpus: 8, want: false},
		{load: 1.9, cpus: 8, want: false},
		{load: 2.1, cpus: 8, want: true},
		// Both endpoints are recorded observations of this repository's own
		// 16-CPU host: the quiet run the ratios were measured on, and the load
		// the flake this guard exists for was seen at.
		{load: 3.73, cpus: 16, want: false},
		{load: 6.4, cpus: 16, want: true},
	} {
		if got := overloaded(c.load, c.cpus); got != c.want {
			t.Errorf("overloaded(%v, %d) = %v, want %v", c.load, c.cpus, got, c.want)
		}
	}
}
