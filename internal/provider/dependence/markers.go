package dependence

import "slices"

// ProjectMarkers are the manifest files whose presence makes a directory a
// project root of one family. It is the single source of that mapping: the
// planner reads it here, and `codectx tools prefetch --for-repo` reads the
// same table to decide which payloads a repository would ever need, rather
// than carrying a second copy that drifts from the one that plans the units.
//
// The result is a copy: the table is package state and a caller must not be
// able to reach into it. A family this provider does not analyse has no
// markers and returns nil.
func ProjectMarkers(f Family) []string { return slices.Clone(projectMarkers[f]) }

// ClosureMarkers are the build and lock files that belong to a family's
// semantic closure without defining a project root. The C/C++ family has only
// these: its unit is the repository itself, so a root declaring a C or C++
// build (CMakeLists.txt, Makefile, compile_commands.json) declares the family
// without declaring a project, and `--for-repo` reads them here so the
// standard out-of-source layout selects the engine. Same copy rule as
// ProjectMarkers.
func ClosureMarkers(f Family) []string { return slices.Clone(closureMarkers[f]) }
