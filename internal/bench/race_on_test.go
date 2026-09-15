//go:build race

package bench

// raceDetector is true in a race-instrumented build. It is the one bit the
// Section 23.2 gate needs from the build configuration: the detector's
// instrumentation slows every measured path by roughly an order of magnitude,
// so a latency or memory ceiling measured under it describes the detector and
// not the product (Section 23.5 measures on release builds).
//
// It is a build-tagged constant rather than a runtime probe because there is
// no supported runtime probe for the detector, and a constant lets the two
// arms be compiled out.
const raceDetector = true
