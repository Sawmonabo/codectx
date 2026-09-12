// Package bench holds the resource benchmarks of Section 30.1 that are too
// slow or too environment-dependent for the ordinary unit suite. Every test
// here skips under -short; CI runs them on the resource stage. There is no
// production code in this package.
package bench
