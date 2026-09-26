// Package bench holds the resource benchmarks of Section 30.1 that are too
// slow or too environment-dependent for the ordinary unit suite. Every test
// here skips under -short. CI's test step runs the package without -short,
// and each measurement skips itself on its own condition: a loaded host, a
// race-instrumented build, or, for the dependence-core measurements, an
// unset CODECTX_BENCH_OUT.
//
// Its one non-test file, allocator.go, is the counting native allocator the
// benchmark process installs to measure parse memory; it lives outside the
// test files only because test files cannot use cgo. Nothing outside this
// package's tests calls it, and the production parser worker never installs
// it.
package bench
