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
// node) with every φ resolved to the definitions it merges; a may-definition
// kills nothing, so a use it reaches pairs with it and with every definition
// reaching its node. Every label is its own node, spanning the label.
func TestGoLoweringGolden(t *testing.T) {
	runGolden(t, "go", []goldenCase{
		{
			// Go spec, For statements with single condition: an absent condition is
			// equivalent to true, so the loop has no exit edge.
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
			// Go spec, Goto statements and Labeled statements: goto L transfers
			// control to the statement labelled L in the same function.
			// Nodes: x@17, x > 0@29, return@37, L@47 (the label's own node),
			// x++@50, goto L@55. Succ: x > 0→{return, L}; return→EXIT;
			// L→x++→goto L→L. {L, x++, goto L} cannot reach EXIT; L is its
			// first RPO member, so augmentation adds L→EXIT. IPDom: return,
			// L, x > 0 → EXIT; x++ → goto L → L. Frontier walks: x > 0 over
			// return and L; L over x++, goto L and itself.
			name:     "region unreachable from exit and a resolved goto",
			protects: "a goto loop that never returns is wired back to its label and still gets post-dominators",
			mutation: "resolve goto only to labels declared before it, or skip augmenting a sink component",
			src:      "package p\nfunc f(x int) { if x > 0 { return }; L: x++; goto L }",
			cd: []string{"x > 0@29 -> return@37", "x > 0@29 -> L@47", "L@47 -> x++@50",
				"L@47 -> goto L@55", "L@47 -> L@47"},
			du: []string{"x@17 -> x > 0@29", "x@17 -> x++@50", "x++@50 -> x++@50"},
		},
		{
			// Go spec, Goto statements: a goto transfers control to the statement with
			// the corresponding label in the same function; with no label M the
			// program is invalid, and the goto is lowered unresolved.
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
			// Go spec, Break statements and Continue statements: break L terminates
			// the for labelled L, and continue L begins its next iteration at the
			// post statement (For statements with for clause).
			// Nodes: n@17, L@26 (the label's own node, one successor, so it
			// controls nothing), i := 0@33, i < n@41 (outer head), i++@48 (update),
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
			// Go spec, Expression switches and Fallthrough statements: the cases are
			// compared top-to-bottom, and fallthrough transfers control to the first
			// statement of the next case clause.
			// Nodes: x@17, x@33 (tag head, a Stmt), 1@42, x++@45,
			// fallthrough@50, 2@68, g(x)@71, x--@86, g(x)@93. Both case
			// conditions compare against the tag, so each uses x, which only
			// the parameter defines before them. Succ: x@33→1@42→{x++, 2@68};
			// x++→fallthrough→g(x)@71; 2@68→{g(x)@71, x--}; g(x)@71 and
			// x-- → g(x)@93 → EXIT. IPDom: 1@42, 2@68, g(x)@71, x-- →
			// g(x)@93; fallthrough → g(x)@71; x++ → fallthrough.
			name:     "switch with fallthrough",
			protects: "fallthrough carries a case body into the next case body, not out of the switch",
			mutation: "end every case with an implicit break, dropping the fallthrough edge",
			src:      "package p\nfunc f(x int) { switch x { case 1: x++; fallthrough; case 2: g(x); default: x-- }; g(x) }",
			cd: []string{"1@42 -> x++@45", "1@42 -> fallthrough@50", "1@42 -> g(x)@71", "1@42 -> 2@68",
				"2@68 -> g(x)@71", "2@68 -> x--@86"},
			du: []string{"x@17 -> x@33", "x@17 -> 1@42", "x@17 -> 2@68", "x@17 -> x++@45", "x++@45 -> g(x)@71", "x@17 -> g(x)@71",
				"x@17 -> x--@86", "x++@45 -> g(x)@93", "x@17 -> g(x)@93", "x--@86 -> g(x)@93"},
		},
		{
			// Go spec, If statements and For statements with single condition: each
			// branch assigns x, and the condition is evaluated before each iteration.
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
		{
			// Go spec, Assignment statements: a tuple assignment proceeds in two
			// phases, every operand evaluated first, then the targets assigned.
			// Nodes: a@17, b@20, c@23 (params), c@36, a@39, b@42, return
			// b@55. Targets read: c nothing, a old b, b old a. c is read by
			// no other target and goes first; a and b form a cycle, broken
			// at a, whose node also uses its own old value; b's read of a
			// stays and resolves to a@39, which carries old a.
			name:     "cyclic multi-assignment keeps the later target's read",
			protects: "every target of a multi-assignment depends on the right-hand values as they were before any target was written, a swap included",
			mutation: "erase the later target's read of the cycle-broken target (b@42 loses a@39 -> b@42), or give the old value to the statement's first node instead of the cycle-broken target's (a@17 -> c@36 replaces a@17 -> a@39)",
			src:      "package p\nfunc f(a, b, c int) int { c, a, b = 0, b, a; return b }",
			du:       []string{"a@17 -> a@39", "b@20 -> a@39", "a@39 -> b@42", "b@42 -> return b@55"},
		},
		{
			// Go spec, Logical operators: the left operand is evaluated, and then the
			// right if the condition requires it; the result is computed from both.
			// Nodes: a@17, b@20, a@40 and b@45 (the hoisted operands),
			// x := a && b@35, return x@48. Succ: a@40→{b@45, x := a && b};
			// b@45→x := a && b. IPDom: a@40, b@45 → x := a && b.
			name:     "a short-circuit value uses both operands",
			protects: "the node owning an && expression reads both operands, so its definition depends on them",
			mutation: "skip && and || subtrees when collecting a node's uses (loses a@17 -> x := a && b@35 and b@20 -> x := a && b@35)",
			src:      "package p\nfunc f(a, b bool) bool { x := a && b; return x }",
			cd:       []string{"a@40 -> b@45"},
			du: []string{"a@17 -> a@40", "b@20 -> b@45", "a@17 -> x := a && b@35", "b@20 -> x := a && b@35",
				"x := a && b@35 -> return x@48"},
		},
		{
			// Go spec, Logical operators and If statements: the condition's value is
			// computed from both operands, the right one evaluated only if a is true.
			// Nodes: a@17, b@20, a@33, b@38 (operands), a && b@33 (the
			// condition), g()@42. Succ: a@33→{b@38, a && b}; b@38→a && b;
			// a && b→{g(), EXIT}. IPDom: a@33, b@38 → a && b → EXIT.
			name:     "a short-circuit condition uses both operands",
			protects: "an if condition built from && reads both operands, so its branch depends on them",
			mutation: "skip && and || subtrees when collecting a node's uses (loses a@17 -> a && b@33 and b@20 -> a && b@33)",
			src:      "package p\nfunc f(a, b bool) { if a && b { g() } }",
			cd:       []string{"a@33 -> b@38", "a && b@33 -> g()@42"},
			du:       []string{"a@17 -> a@33", "b@20 -> b@38", "a@17 -> a && b@33", "b@20 -> a && b@33"},
		},
		{
			// Go spec, For statements with range clause: the range expression is
			// evaluated once before beginning the loop, and the blank key `_`
			// declares no variable.
			// Nodes: xs@17, sum := 0@33, xs@61 (the range expression, which
			// defines the iteration variable), range@55 (head), v@50,
			// sum += v@66, return sum@78. Succ: xs@61→range→{v, return sum};
			// v→sum += v→range. IPDom: v → sum += v → range → return sum.
			name:     "a range loop's head and value depend on the range expression",
			protects: "the loop head and every key/value definition read the range expression evaluated before the loop",
			mutation: "give the head and the key/value nodes no use of the range expression's result (loses xs@61 -> range@55 and xs@61 -> v@50)",
			src:      "package p\nfunc f(xs []int) int { sum := 0; for _, v := range xs { sum += v }; return sum }",
			cd:       []string{"range@55 -> v@50", "range@55 -> sum += v@66", "range@55 -> range@55"},
			du: []string{"xs@17 -> xs@61", "xs@61 -> range@55", "xs@61 -> v@50", "v@50 -> sum += v@66",
				"sum := 0@33 -> sum += v@66", "sum += v@66 -> sum += v@66", "sum := 0@33 -> return sum@78",
				"sum += v@66 -> return sum@78"},
		},
		{
			// Go spec, Expression switches: each case expression is compared against
			// the switch expression; with no match and no default the switch ends.
			// Nodes: x@17, y := 0@30, x@45 (tag head, a Stmt), 1@54 (case
			// condition), y = 1@57, return y@66. Succ: x@45→1@54→{y = 1,
			// return y}; y = 1→return y. IPDom: 1@54 → return y.
			name:     "an expression switch's case condition reads the tag",
			protects: "a case condition compares against the tag, so the case and its body depend on the tag's variables",
			mutation: "give case conditions only their own values' uses (loses x@17 -> 1@54)",
			src:      "package p\nfunc f(x int) int { y := 0; switch x { case 1: y = 1 }; return y }",
			cd:       []string{"1@54 -> y = 1@57"},
			du:       []string{"x@17 -> x@45", "x@17 -> 1@54", "y := 0@30 -> return y@66", "y = 1@57 -> return y@66"},
		},
		{
			// Go spec, Type switches: the guard reads v, and the alias v is declared
			// in the implicit block of each clause.
			// Nodes: v@17, v := v.(type)@37 (head: uses the parameter v,
			// then defines the alias v), int@58 (type case), return v@63,
			// return 0@75. Succ: head→int→{return v, return 0}. IPDom: int
			// → EXIT.
			name:     "a type switch's type case reads the switched value, not the alias",
			protects: "a type case tests the switched value, read before the alias shadowing it is declared",
			mutation: "give type-case nodes no uses (loses v@17 -> int@58), or collect the tag after declaring the alias (v := v.(type)@37 -> int@58 replaces v@17 -> int@58)",
			src:      "package p\nfunc f(v any) int { switch v := v.(type) { case int: return v }; return 0 }",
			cd:       []string{"int@58 -> return v@63", "int@58 -> return 0@75"},
			du:       []string{"v@17 -> v := v.(type)@37", "v@17 -> int@58", "v := v.(type)@37 -> return v@63"},
		},
		{
			// Go spec, Assignment statements and Selectors: the left operand s.f is
			// a field selector, written through s.
			// Nodes: s@17, v@22, s.f = v@33 (uses s and v, may-defines s),
			// return s@42. A may-definition kills nothing, so both the write
			// and the parameter reach the return.
			name:     "a field write updates its base variable without killing it",
			protects: "a write to s.f reaches a later use of s, and so does s's earlier definition",
			mutation: "record no definition of the base (loses s.f = v@33 -> return s@42), or a killing one (loses s@17 -> return s@42)",
			src:      "package p\nfunc f(s S, v int) S { s.f = v; return s }",
			du: []string{"s@17 -> s.f = v@33", "v@22 -> s.f = v@33", "s.f = v@33 -> return s@42",
				"s@17 -> return s@42"},
		},
		{
			// Go spec, Assignment statements and Index expressions: the left operand
			// m[k] is a map index expression, written through m.
			// Nodes: m@17, k@32, v@35, m[k] = v@56 (uses m, k and v,
			// may-defines m), return m@66.
			name:     "an index write updates its base variable without killing it",
			protects: "a write to m[k] reaches a later use of m, and so does m's earlier definition",
			mutation: "record no definition of the base (loses m[k] = v@56 -> return m@66), or a killing one (loses m@17 -> return m@66)",
			src:      "package p\nfunc f(m map[int]int, k, v int) map[int]int { m[k] = v; return m }",
			du: []string{"m@17 -> m[k] = v@56", "k@32 -> m[k] = v@56", "v@35 -> m[k] = v@56",
				"m[k] = v@56 -> return m@66", "m@17 -> return m@66"},
		},
		{
			// Go spec, Function literals: a function literal is a closure sharing the
			// variables of the surrounding function.
			// Nodes: x@17, n := 0@30, h := func() { n += x }@38 (uses n and
			// x, defines h, may-defines n), h()@62, return n@67. The literal
			// may run at any later point, or never, so both n := 0 and the
			// creating node reach return n.
			name:     "a closure's write to an enclosing variable reaches a later use",
			protects: "a function literal that writes an enclosing variable is a non-killing may-definition of it where it is created",
			mutation: "record a literal's writes as uses only (loses h := func() { n += x }@38 -> return n@67), or as a killing definition (loses n := 0@30 -> return n@67)",
			src:      "package p\nfunc f(x int) int { n := 0; h := func() { n += x }; h(); return n }",
			du: []string{"x@17 -> h := func() { n += x }@38", "n := 0@30 -> h := func() { n += x }@38",
				"h := func() { n += x }@38 -> h()@62", "h := func() { n += x }@38 -> return n@67",
				"n := 0@30 -> return n@67"},
		},
		{
			// Go spec, Select statements: every channel operand and sent value is
			// evaluated exactly once, in source order, on entering the select.
			// Nodes: a@17, b@20, y@32, x := 0@45, select@53 (head), <-a@67,
			// x = 1@72, b <- y@84, x = 2@92, return x@101. Succ: select→{<-a,
			// b <- y}; <-a→x = 1→return x; b <- y→x = 2→return x. IPDom:
			// select → return x. x := 0 is killed on both clauses.
			name:     "a select head reads every channel operand and sent value",
			protects: "which select clause runs depends on the channels and sent values Go evaluates on entering the select",
			mutation: "give the select head no uses (loses a@17, b@20 and y@32 -> select@53)",
			src:      "package p\nfunc f(a, b chan int, y int) int { x := 0; select { case <-a: x = 1; case b <- y: x = 2 }; return x }",
			cd: []string{"select@53 -> <-a@67", "select@53 -> x = 1@72", "select@53 -> b <- y@84",
				"select@53 -> x = 2@92"},
			du: []string{"a@17 -> select@53", "b@20 -> select@53", "y@32 -> select@53", "a@17 -> <-a@67",
				"b@20 -> b <- y@84", "y@32 -> b <- y@84", "x = 1@72 -> return x@101", "x = 2@92 -> return x@101"},
		},
		{
			// Go spec, Goto statements, Labeled statements and Blocks: goto L lands
			// on the label of an empty block.
			// Nodes: c@17, d@20, c@33, d@40, goto L@44, a()@54, L@59 (the
			// label's own node; `L: {}` lowers nothing else), b()@74. Succ:
			// c@33→{d@40, b()}; d@40→{goto L, a()}; goto L→L; a()→L;
			// L→EXIT; b()→EXIT. IPDom: goto L, a(), d@40 → L; L, b(), c@33
			// → EXIT.
			name:     "a goto to an empty labelled statement lands on the label",
			protects: "goto L reaches L's own node inside the then branch, never the else branch's first node",
			mutation: "bind a label to the next node created anywhere, so goto L lands on b()@74 (adds d@40 -> b()@74)",
			src:      "package p\nfunc f(c, d bool) { if c { if d { goto L }; a(); L: {} } else { b() } }",
			cd: []string{"c@33 -> d@40", "c@33 -> L@59", "c@33 -> b()@74", "d@40 -> goto L@44",
				"d@40 -> a()@54"},
			du: []string{"c@17 -> c@33", "d@20 -> d@40"},
		},
		{
			// Go spec, Goto statements and Return statements: the statements after
			// the return are reached by no path from the function's entry.
			// Nodes: x@17, return x@30, L@40, x++@43, goto L@48. Succ:
			// return x→EXIT; L→x++→goto L→L. The cycle has no predecessor
			// from ENTRY; L is its first member by id, so augmentation adds
			// L→EXIT. IPDom: x++ → goto L → L → EXIT. The only definition
			// reaching L is the cycle's own x++, so x@17 reaches only the
			// return.
			name:     "an unreachable goto cycle resolves its uses within the cycle",
			protects: "a use in a cycle entered by no path from ENTRY pairs only with the cycle's own definitions, never with one from before the return",
			mutation: "resolve a block's predecessor-less leader to the definitions at ENTRY, or memoise a variable's value per block across the cycle's leader (adds x@17 -> x++@43)",
			src:      "package p\nfunc f(x int) int { return x; L: x++; goto L }",
			cd:       []string{"L@40 -> x++@43", "L@40 -> goto L@48", "L@40 -> L@40"},
			du:       []string{"x@17 -> return x@30", "x++@43 -> x++@43"},
		},
		{
			// Go spec, Address operators: `&x` yields a pointer through which
			// the callee may write x. Nodes: x := 1@25 (defines x), g(&x)@33
			// (Uses x, may-defines x), return x@40. Straight line, so no
			// control dependence; the may-definition kills nothing, so the
			// return's x pairs with both x := 1 and g(&x).
			name:     "taking an address is a may-definition of the variable",
			protects: "a use after a call that received a variable's address sees the write the call may make through it",
			mutation: "drop the address-taking may-definition in collect (g(&x)@33 -> return x@40 vanishes), or make it a killing Def (x := 1@25 -> return x@40 vanishes)",
			src:      "package p\nfunc f() int { x := 1; g(&x); return x }",
			du:       []string{"x := 1@25 -> g(&x)@33", "x := 1@25 -> return x@40", "g(&x)@33 -> return x@40"},
		},
		{
			// Go spec, Short variable declarations and Assignment statements:
			// every operand is evaluated, then the targets are assigned, so
			// x's final value is g's result whatever g wrote through &x.
			// Nodes: x := 0@25, the target nodes x@33 (Uses x through &x,
			// defines x; the may-definition &x makes is on it, the node that
			// evaluates g(&x), and is overwritten by its own assignment) and
			// y@36 (defines y), return x@51, reached only by x@33.
			name:     "a multi-assignment's address-taking lands on the node that evaluates it",
			protects: "the may-definition of an address taken in a paired value is attached by position, never to the statement's last node",
			mutation: "attach the statement's may-definitions to its last node (y@36 -> return x@51 appears)",
			src:      "package p\nfunc f() int { x := 0; x, y := g(&x), 2; return x }",
			du:       []string{"x := 0@25 -> x@33", "x@33 -> return x@51"},
		},
		{
			// Go spec, Select statements: the channel operand and the sent
			// value `&x` are evaluated once, on entering the select, so the
			// head select@45 Uses ch and x and may-defines x; the clause node
			// ch <- &x@59 Uses them again and writes nothing. The head has one
			// successor, so there is no control dependence. return x@72 is
			// reached by x := 0@37 and the head's non-killing may-definition.
			name:     "a select clause's address-taking is may-defined once, at the head",
			protects: "an address taken in a select's channel operand or sent value is a may-definition of the head that evaluates it, not of the clause",
			mutation: "collect a send clause's operands with their may-definitions on its node (ch <- &x@59 -> return x@72 appears)",
			src:      "package p\nfunc f(ch chan *int) int { x := 0; select { case ch <- &x: }; return x }",
			du: []string{"ch@17 -> select@45", "x := 0@37 -> select@45", "ch@17 -> ch <- &x@59",
				"x := 0@37 -> ch <- &x@59", "select@45 -> ch <- &x@59",
				"x := 0@37 -> return x@72", "select@45 -> return x@72"},
		},
		{
			// Go spec, Logical operators and Address operators: g(&x) is
			// evaluated only when a is true, by its own hoisted Branch
			// g(&x)@47, which Uses x and may-defines it; the condition
			// a && g(&x)@42 reads a and x and writes nothing. Nodes: a@17,
			// x := 0@31, the operand Branch a@42, g(&x)@47, the condition,
			// return x@58. a@42 reaches the condition directly when false,
			// so it controls g(&x)@47 only.
			name:     "a short-circuit operand's address-taking is on the operand's node",
			protects: "an address taken inside a hoisted && operand is a may-definition of the operand's node, not also of the node owning the expression",
			mutation: "keep the may-definitions of short-circuit operands when collecting the owning node (a && g(&x)@42 -> return x@58 appears)",
			src:      "package p\nfunc f(a bool) int { x := 0; if a && g(&x) { }; return x }",
			cd:       []string{"a@42 -> g(&x)@47"},
			du: []string{"a@17 -> a@42", "a@17 -> a && g(&x)@42", "x := 0@31 -> g(&x)@47",
				"x := 0@31 -> a && g(&x)@42", "g(&x)@47 -> a && g(&x)@42",
				"x := 0@31 -> return x@58", "g(&x)@47 -> return x@58"},
		},
	})
}
