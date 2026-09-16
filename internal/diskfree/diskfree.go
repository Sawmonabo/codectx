// Package diskfree measures the space a process can still write under a
// directory. The measurement is what an unprivileged writer can use, not
// what the filesystem holds in reserve, and a directory the platform cannot
// describe is reported as unmeasured rather than as full or as empty.
package diskfree
