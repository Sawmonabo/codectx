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
		{load: 3.9, cpus: 8, want: false},
		{load: 4.1, cpus: 8, want: true},
		{load: 6.4, cpus: 8, want: true}, // the load that flaked the seal ratio
	} {
		if got := overloaded(c.load, c.cpus); got != c.want {
			t.Errorf("overloaded(%v, %d) = %v, want %v", c.load, c.cpus, got, c.want)
		}
	}
}
