package worker

import "testing"

// TestGoLoweringGolden pins the Go lowering's control-dependence and def-use
// pairs on hand-derived functions. A wrong pair here is a wrong dependence
// fact served to every consumer. Each source is one line after the package
// clause, so a node's offset is its column plus 10.
//
// Derivation rules: augmentation adds an edge to EXIT from every node with
// no successor and from the smallest-reverse-post-order member of each sink
// strongly connected component that cannot reach EXIT; control dependence
// is the post-dominance frontier over the augmented graph with no
// entry-to-exit edge (nothing depends on ENTRY; a loop head whose back edge
// it controls depends on itself); a def-use pair is (defining node, using
// node) with every φ resolved to the definitions it merges.
func TestGoLoweringGolden(t *testing.T) {
	runGolden(t, "go", []goldenCase{
		{
			// Nodes: c@17 (param), for@27 (head), c@36 (if), g()@40.
			// Succ: ENTRY→c@17→for@27→c@36→{g()@40, for@27}; g()@40→for@27.
			// {for, c@36, g()} cannot reach EXIT; for@27 is its first RPO
			// member, so augmentation adds for@27→EXIT. IPDom: c@36, g()@40
			// → for@27; for@27 → EXIT. Frontier walks: for@27 over c@36 and
			// itself; c@36 over g()@40.
			name:     "infinite loop with a branch",
			protects: "an infinite loop's nodes keep their control dependences through exit augmentation",
			mutation: "drop augmentation step (ii), the edge from a sink component that cannot reach exit",
			src:      "package p\nfunc f(c bool) { for { if c { g() } } }",
			cd:       []string{"for@27 -> c@36", "for@27 -> for@27", "c@36 -> g()@40"},
			du:       []string{"c@17 -> c@36"},
		},
		{
			// Nodes: x@17, x > 0@29, return@37, x++@50 (labelled L), goto L@55.
			// Succ: x > 0→{return, x++}; return→EXIT; x++→goto L→x++.
			// {x++, goto L} cannot reach EXIT: augmentation adds x++→EXIT.
			// IPDom: return, x++, x > 0 → EXIT; goto L → x++.
			name:     "region unreachable from exit and a resolved goto",
			protects: "a goto loop that never returns is wired back to its label and still gets post-dominators",
			mutation: "resolve goto only to labels declared before it, or skip augmenting a sink component",
			src:      "package p\nfunc f(x int) { if x > 0 { return }; L: x++; goto L }",
			cd: []string{"x > 0@29 -> return@37", "x > 0@29 -> x++@50", "x++@50 -> goto L@55",
				"x++@50 -> x++@50"},
			du: []string{"x@17 -> x > 0@29", "x@17 -> x++@50", "x++@50 -> x++@50"},
		},
		{
			// Nodes: x@17, x > 0@29, goto M@37 (no label M: no successor),
			// x++@47. Augmentation adds goto M→EXIT.
			name:       "dangling goto",
			protects:   "a goto to no label is counted as unresolved and keeps no successor",
			mutation:   "drop the unresolved count, or let the dangling goto fall through to the next statement",
			src:        "package p\nfunc f(x int) { if x > 0 { goto M }; x++ }",
			cd:         []string{"x > 0@29 -> goto M@37", "x > 0@29 -> x++@47"},
			du:         []string{"x@17 -> x > 0@29", "x@17 -> x++@47"},
			unresolved: 1,
		},
		{
			// Nodes: n@17, i := 0@33, i < n@41 (outer head), i++@48 (update),
			// for@54 (inner head), i > 1@63, continue L@71, break L@85.
			// Succ: i < n→{for@54, EXIT}; for@54→i > 1→{continue L, break L};
			// continue L→i++→i < n; break L→EXIT. The inner loop has no back
			// edge: its body always leaves by a labelled jump.
			// IPDom: for@54 → i > 1; i > 1 → EXIT; continue L → i++ → i < n.
			name:     "labelled break and continue out of a nested loop",
			protects: "break L and continue L target the labelled outer loop, not the innermost one",
			mutation: "resolve a labelled break or continue to the innermost loop",
			src:      "package p\nfunc f(n int) { L: for i := 0; i < n; i++ { for { if i > 1 { continue L }; break L } } }",
			cd: []string{"i < n@41 -> for@54", "i < n@41 -> i > 1@63", "i > 1@63 -> continue L@71",
				"i > 1@63 -> i++@48", "i > 1@63 -> i < n@41", "i > 1@63 -> break L@85"},
			du: []string{"n@17 -> i < n@41", "i := 0@33 -> i < n@41", "i++@48 -> i < n@41",
				"i := 0@33 -> i > 1@63", "i++@48 -> i > 1@63", "i := 0@33 -> i++@48", "i++@48 -> i++@48"},
		},
		{
			// Nodes: x@17, x@33 (tag), 1@42, x++@45, fallthrough@50, 2@68,
			// g(x)@71, x--@86, g(x)@93. Succ: x@33→1@42→{x++, 2@68};
			// x++→fallthrough→g(x)@71; 2@68→{g(x)@71, x--}; g(x)@71 and
			// x-- → g(x)@93 → EXIT. IPDom: 1@42, 2@68, g(x)@71, x-- →
			// g(x)@93; fallthrough → g(x)@71; x++ → fallthrough.
			name:     "switch with fallthrough",
			protects: "fallthrough carries a case body into the next case body, not out of the switch",
			mutation: "end every case with an implicit break, dropping the fallthrough edge",
			src:      "package p\nfunc f(x int) { switch x { case 1: x++; fallthrough; case 2: g(x); default: x-- }; g(x) }",
			cd: []string{"1@42 -> x++@45", "1@42 -> fallthrough@50", "1@42 -> g(x)@71", "1@42 -> 2@68",
				"2@68 -> g(x)@71", "2@68 -> x--@86"},
			du: []string{"x@17 -> x@33", "x@17 -> x++@45", "x++@45 -> g(x)@71", "x@17 -> g(x)@71",
				"x@17 -> x--@86", "x++@45 -> g(x)@93", "x@17 -> g(x)@93", "x--@86 -> g(x)@93"},
		},
		{
			// Nodes: c@17, x := 0@31, c@42, x = 1@46, x = 2@61, x < 9@74 (head),
			// x++@82, return x@89. x < 9 merges φ(φ(x = 1, x = 2), x++); both
			// branches kill x := 0, which therefore reaches nothing.
			name:     "def through an if/else φ and a loop φ",
			protects: "every φ is resolved to the definitions it merges, and a killed definition reaches no use",
			mutation: "emit the φ instead of resolving it, or let x := 0 reach past both branches",
			src:      "package p\nfunc f(c bool) int { x := 0; if c { x = 1 } else { x = 2 }; for x < 9 { x++ }; return x }",
			cd:       []string{"c@42 -> x = 1@46", "c@42 -> x = 2@61", "x < 9@74 -> x++@82", "x < 9@74 -> x < 9@74"},
			du: []string{"c@17 -> c@42", "x = 1@46 -> x < 9@74", "x = 2@61 -> x < 9@74", "x++@82 -> x < 9@74",
				"x = 1@46 -> x++@82", "x = 2@61 -> x++@82", "x++@82 -> x++@82",
				"x = 1@46 -> return x@89", "x = 2@61 -> return x@89", "x++@82 -> return x@89"},
		},
	})
}
