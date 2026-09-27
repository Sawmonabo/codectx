package worker

import "testing"

// TestGoLoweringGolden pins the Go lowering's control-dependence and def-use
// pairs on hand-derived functions. A wrong pair here is a wrong dependence
// fact served to every consumer. Each source is one line after the package
// clause, so a node's offset is its column plus 10.
// The derivation and rendering rules are runGolden's. Every label is its own
// node, spanning the label.
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
			// fallthrough@50, 2@68, g(x)@71, x--@86, g(x)@93. The head reads
			// x once and defines the variable holding the tag; both case
			// conditions Use that variable, so each pairs with the head,
			// never with x's definitions. Succ: x@33→1@42→{x++, 2@68};
			// x++→fallthrough→g(x)@71; 2@68→{g(x)@71, x--}; g(x)@71 and
			// x-- → g(x)@93 → EXIT. IPDom: 1@42, 2@68, g(x)@71, x-- →
			// g(x)@93; fallthrough → g(x)@71; x++ → fallthrough.
			name:     "switch with fallthrough",
			protects: "fallthrough carries a case body into the next case body, not out of the switch",
			mutation: "end every case with an implicit break, dropping the fallthrough edge",
			src:      "package p\nfunc f(x int) { switch x { case 1: x++; fallthrough; case 2: g(x); default: x-- }; g(x) }",
			cd: []string{"1@42 -> x++@45", "1@42 -> fallthrough@50", "1@42 -> g(x)@71", "1@42 -> 2@68",
				"2@68 -> g(x)@71", "2@68 -> x--@86"},
			du: []string{"x@17 -> x@33", "x@33 -> 1@42", "x@33 -> 2@68", "x@17 -> x++@45", "x++@45 -> g(x)@71", "x@17 -> g(x)@71",
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
			// x := a && b@35, return x@48. Each operand node defines the
			// expression's result variable, and x := a && b Uses it: SSA
			// merges a@40's definition (a false) with b@45's. Succ:
			// a@40→{b@45, x := a && b}; b@45→x := a && b. IPDom: a@40, b@45
			// → x := a && b.
			name:     "a short-circuit value uses both operands",
			protects: "the node owning an && expression Uses the result both operand nodes define, so its definition depends on each operand's evaluation",
			mutation: "give the owning node no use of the short-circuit result (loses a@40 -> x := a && b@35 and b@45 -> x := a && b@35), or let it re-read the operands' names (adds a@17 -> x := a && b@35)",
			src:      "package p\nfunc f(a, b bool) bool { x := a && b; return x }",
			cd:       []string{"a@40 -> b@45"},
			du: []string{"a@17 -> a@40", "b@20 -> b@45", "a@40 -> x := a && b@35", "b@45 -> x := a && b@35",
				"x := a && b@35 -> return x@48"},
		},
		{
			// Go spec, Logical operators and If statements: the condition's value is
			// computed from both operands, the right one evaluated only if a is true.
			// Nodes: a@17, b@20, a@33, b@38 (operands), a && b@33 (the
			// condition), g()@42. Succ: a@33→{b@38, a && b}; b@38→a && b;
			// a && b→{g(), EXIT}. IPDom: a@33, b@38 → a && b → EXIT. The
			// operand nodes define the result variable the condition Uses.
			name:     "a short-circuit condition uses both operands",
			protects: "an if condition built from && Uses the result both operand nodes define, so its branch depends on each operand's evaluation",
			mutation: "give the condition no use of the short-circuit result (loses a@33 -> a && b@33 and b@38 -> a && b@33), or let it re-read the operands' names (adds a@17 -> a && b@33)",
			src:      "package p\nfunc f(a, b bool) { if a && b { g() } }",
			cd:       []string{"a@33 -> b@38", "a && b@33 -> g()@42"},
			du:       []string{"a@17 -> a@33", "b@20 -> b@38", "a@33 -> a && b@33", "b@38 -> a && b@33"},
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
			// return y}; y = 1→return y. IPDom: 1@54 → return y. The head
			// defines the variable holding the tag, which 1@54 Uses.
			name:     "an expression switch's case condition reads the tag",
			protects: "a case condition compares against the tag's value as the head evaluated it, so the case and its body depend on the head",
			mutation: "give case conditions only their own values' uses (loses x@45 -> 1@54), or let them re-read the tag's names (x@17 -> 1@54 appears)",
			src:      "package p\nfunc f(x int) int { y := 0; switch x { case 1: y = 1 }; return y }",
			cd:       []string{"1@54 -> y = 1@57"},
			du:       []string{"x@17 -> x@45", "x@45 -> 1@54", "y := 0@30 -> return y@66", "y = 1@57 -> return y@66"},
		},
		{
			// Go spec, Type switches: the guard reads v, and the alias v is declared
			// in the implicit block of each clause.
			// Nodes: v@17, v := v.(type)@37 (head: uses the parameter v,
			// then defines the alias v, which holds the switched value),
			// int@58 (type case, Using the alias as the variable holding the
			// tag), return v@63, return 0@75. Succ: head→int→{return v,
			// return 0}. IPDom: int → EXIT.
			name:     "a type switch's type case tests the value the head evaluated",
			protects: "the guard reads the outer v before the alias shadowing it is declared, and a type case tests the value the head holds",
			mutation: "give type-case nodes no uses (loses v := v.(type)@37 -> int@58), or collect the guard after declaring the alias (loses v@17 -> v := v.(type)@37)",
			src:      "package p\nfunc f(v any) int { switch v := v.(type) { case int: return v }; return 0 }",
			cd:       []string{"int@58 -> return v@63", "int@58 -> return 0@75"},
			du:       []string{"v@17 -> v := v.(type)@37", "v := v.(type)@37 -> int@58", "v := v.(type)@37 -> return v@63"},
		},
		{
			// Go spec, Assignment statements and Selectors: the left operand s.f is
			// a field selector, written through s.
			// Nodes: s@17, v@22, s.f = v@33 (uses s and v, may-defines s),
			// return s@42. The may-definition is a χ: the return pairs with
			// the write and, through it, with the parameter it passes on.
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
			// select → return x. x := 0 is killed on both clauses. The head
			// defines the variable holding the evaluated operands, and each
			// clause node Uses it, never a, b or y.
			name:     "a select head reads every channel operand and sent value",
			protects: "which select clause runs depends on the channels and sent values Go evaluates on entering the select, and each clause communicates the values the head evaluated",
			mutation: "give the select head no uses (loses a@17, b@20 and y@32 -> select@53), or let a clause re-read its operands' names (adds a@17 -> <-a@67)",
			src:      "package p\nfunc f(a, b chan int, y int) int { x := 0; select { case <-a: x = 1; case b <- y: x = 2 }; return x }",
			cd: []string{"select@53 -> <-a@67", "select@53 -> x = 1@72", "select@53 -> b <- y@84",
				"select@53 -> x = 2@92"},
			du: []string{"a@17 -> select@53", "b@20 -> select@53", "y@32 -> select@53", "select@53 -> <-a@67",
				"select@53 -> b <- y@84", "x = 1@72 -> return x@101", "x = 2@92 -> return x@101"},
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
			// control dependence; the may-definition is a χ, so the return's
			// x pairs with g(&x) and, through it, with x := 1.
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
			// head select@45 Uses ch and x, may-defines x and defines the
			// variable holding the operands; the clause node ch <- &x@59 Uses
			// that variable and writes nothing. The head has one
			// successor, so there is no control dependence. return x@72 is
			// reached by the head's may-definition, a χ, and through it by
			// x := 0@37.
			name:     "a select clause's address-taking is may-defined once, at the head",
			protects: "an address taken in a select's channel operand or sent value is a may-definition of the head that evaluates it, not of the clause",
			mutation: "collect a send clause's operands with their may-definitions on its node (ch <- &x@59 -> return x@72 appears)",
			src:      "package p\nfunc f(ch chan *int) int { x := 0; select { case ch <- &x: }; return x }",
			du: []string{"ch@17 -> select@45", "x := 0@37 -> select@45", "select@45 -> ch <- &x@59",
				"x := 0@37 -> return x@72", "select@45 -> return x@72"},
		},
		{
			// Go spec, Logical operators and Address operators: g(&x) is
			// evaluated only when a is true, by its own hoisted Branch
			// g(&x)@47, which Uses x and may-defines it; both operand nodes
			// define the result variable, which the condition a && g(&x)@42
			// Uses, and it writes nothing. Nodes: a@17,
			// x := 0@31, the operand Branch a@42, g(&x)@47, the condition,
			// return x@58. a@42 reaches the condition directly when false,
			// so it controls g(&x)@47 only.
			name:     "a short-circuit operand's address-taking is on the operand's node",
			protects: "an address taken inside a hoisted && operand is a may-definition of the operand's node, not also of the node owning the expression",
			mutation: "collect a short-circuit expression's operands, with their may-definitions, on the owning node (a && g(&x)@42 -> return x@58 appears)",
			src:      "package p\nfunc f(a bool) int { x := 0; if a && g(&x) { }; return x }",
			cd:       []string{"a@42 -> g(&x)@47"},
			du: []string{"a@17 -> a@42", "a@42 -> a && g(&x)@42", "x := 0@31 -> g(&x)@47",
				"g(&x)@47 -> a && g(&x)@42",
				"x := 0@31 -> return x@58", "g(&x)@47 -> return x@58"},
		},
		{
			// Go spec, Select statements: the sent value a && g(&x) is
			// evaluated on entering the select, so its operands are hoisted
			// before the head. Nodes: a@17, ch@25, x := 0@45, the operand
			// Branches a@73 (Uses a) and g(&x)@78 (Uses x, may-defines x),
			// both defining the result variable; select@53 (Uses ch and the
			// result, defines the variable holding the operands); the clause
			// ch <- a && g(&x)@67 (Uses that variable); return x@88. Succ:
			// a@73→{g(&x)@78, select@53}; g(&x)@78→select@53→clause→return
			// x→EXIT. IPDom: a@73, g(&x)@78 → select@53 → clause → return x.
			// Frontier walk: a@73 over g(&x)@78.
			name:     "a select clause's short-circuit operands are evaluated before the head",
			protects: "the && operands of a sent value run on entering the select, so the head sees their result and their writes",
			mutation: "hoist a select clause's short-circuit operands at the clause, after the head (loses a@73 -> select@53 and g(&x)@78 -> select@53)",
			src:      "package p\nfunc f(a bool, ch chan bool) int { x := 0; select { case ch <- a && g(&x): }; return x }",
			cd:       []string{"a@73 -> g(&x)@78"},
			du: []string{"a@17 -> a@73", "x := 0@45 -> g(&x)@78", "ch@25 -> select@53", "a@73 -> select@53",
				"g(&x)@78 -> select@53", "select@53 -> ch <- a && g(&x)@67", "x := 0@45 -> return x@88",
				"g(&x)@78 -> return x@88"},
		},
		{
			// Go spec, Select statements: the receive's channel operand is
			// evaluated on entering the select, and its left-hand side is
			// assigned when the clause is chosen. Nodes: a@17, y@29 (params),
			// select@42 (Uses a, defines the variable holding the operand),
			// y = <-a@56 (Uses that variable, defines y), return y@68. One
			// successor each, so no control dependence; y = <-a kills the
			// parameter y on the only path to the return.
			name:     "a select receive clause assigns the value the head evaluated",
			protects: "a receive clause's assignment depends on the head that evaluated its channel, never on the channel's names again",
			mutation: "let a receive clause collect its right-hand side itself (adds a@17 -> y = <-a@56)",
			src:      "package p\nfunc f(a chan int, y int) int { select { case y = <-a: }; return y }",
			du:       []string{"a@17 -> select@42", "select@42 -> y = <-a@56", "y = <-a@56 -> return y@68"},
		},
		{
			// Go spec, If statements and Expression switches: a condition, a
			// tag and a case value are expressions, and a parenthesized
			// expression is the expression inside it. Nodes: x@17, x > 0@34
			// (condition), x = 1@43, x@60 (tag head, defines the variable
			// holding the tag), 1@71 (case condition, Uses it), return 2@75,
			// return x@87. Succ: x > 0→{x = 1, x@60}; x = 1→x@60→1@71→
			// {return 2, return x}. IPDom: x > 0, x = 1 → x@60 → 1@71 →
			// EXIT. Frontier walks: x > 0 over x = 1; 1@71 over both returns.
			name:     "a parenthesized condition, tag and case value span the expression inside",
			protects: "a node spanning an expression renders it with its enclosing parentheses stripped, so its pairs name the expression",
			mutation: "span a condition, a tag or a one-value case list as parsed (x > 0@34, x@60 and 1@71 render as (x > 0)@33, (x)@59 and (1)@70)",
			src:      "package p\nfunc f(x int) int { if (x > 0) { x = 1 }; switch (x) { case (1): return 2 }; return x }",
			cd:       []string{"x > 0@34 -> x = 1@43", "1@71 -> return 2@75", "1@71 -> return x@87"},
			du: []string{"x@17 -> x > 0@34", "x@17 -> x@60", "x = 1@43 -> x@60", "x@60 -> 1@71",
				"x@17 -> return x@87", "x = 1@43 -> return x@87"},
		},
		{
			// Go spec, Declarations and scope: n and p are declared in no block
			// of f, so they are package-level names, not variables of f (the
			// lowering's "Names that resolve to no variable"). n stands as an
			// assignment target, an op= target, a ++ operand, one of two
			// targets, the base of &, a captured write and ++ in a function
			// literal, and a range target assigned by `=`; p as the base of a
			// field write. Go has no embedded assignment and no deletion of a
			// name. Nodes: x@17, xs@24, y := 0@40, n = x@48 (Uses x),
			// n += x@55 (Uses x), n++@63 (no use, no definition),
			// n, y = y, n@68 (one node, since n is no target: Uses y, defines
			// y), p.f = y@81 (Uses y, may-defines nothing), g(&n)@90 (nothing),
			// defer func() { n = y; n++ }()@97 (Uses the captured y only),
			// xs@142 (defines the iteration variable), range@136 (head), n@132
			// (Uses the iteration variable, defines nothing), y++@147,
			// return y + n@154 (Uses y). Succ: straight line to xs@142, then
			// range@136→{n@132, return}; n@132→y++@147→range@136. IPDom: n@132
			// → y++@147 → range@136 → return.
			name:     "a write to a name that resolves to no variable defines nothing",
			protects: "an unresolved name in every write position makes its node with its other operands' reads and never reaches flow.Builder as -1",
			mutation: "drop a variable >= 0 test: on assign's identifier targets (n = x@48 Defs -1 and panics), on an inc/dec identifier (n++@63), on a range target assigned by = (n@132), on captured in scanWrite (the literal's n), or on baseVar in assign or collect (p.f, &n)",
			src:      "package p\nfunc f(x int, xs []int) int { y := 0; n = x; n += x; n++; n, y = y, n; p.f = y; g(&n); defer func() { n = y; n++ }(); for n = range xs { y++ }; return y + n }",
			cd:       []string{"range@136 -> n@132", "range@136 -> y++@147", "range@136 -> range@136"},
			du: []string{"x@17 -> n = x@48", "x@17 -> n += x@55", "y := 0@40 -> n, y = y, n@68",
				"n, y = y, n@68 -> p.f = y@81", "n, y = y, n@68 -> defer func() { n = y; n++ }()@97",
				"xs@24 -> xs@142", "xs@142 -> range@136", "xs@142 -> n@132", "n, y = y, n@68 -> y++@147",
				"y++@147 -> y++@147", "n, y = y, n@68 -> return y + n@154", "y++@147 -> return y + n@154"},
		},
		{
			// Go spec, Method declarations: the receiver is declared in the
			// function block, as a parameter is. Nodes: s@16 (the receiver,
			// before the parameters), v@24, s.n = v@33 (Uses s and v,
			// may-defines s). Straight line.
			name:     "a method's receiver is a node defining it",
			protects: "a read of the receiver in a method body pairs with the receiver's own definition",
			mutation: "lower no node for the receiver (loses s@16 -> s.n = v@33)",
			src:      "package p\nfunc (s *S) f(v int) { s.n = v }",
			du:       []string{"s@16 -> s.n = v@33", "v@24 -> s.n = v@33"},
		},
		{
			// Go spec, Function declarations and Return statements: a named result
			// is declared in the function block, and a return with no operands
			// returns the results' current values. Nodes: c@17, r@26 (the named
			// result), c@38 (condition), r = 1@42, return@51. Succ: c@38 →
			// {r = 1, return}; r = 1 → return. IPDom: c@38, r = 1 → return.
			name:     "a bare return uses every named result",
			protects: "a bare return depends on each definition of a named result that reaches it, the result's own declaration included",
			mutation: "lower no node for a named result (loses r@26 -> return@51), or give a bare return no uses (loses both pairs into return@51)",
			src:      "package p\nfunc f(c bool) (r int) { if c { r = 1 }; return }",
			cd:       []string{"c@38 -> r = 1@42"},
			du:       []string{"c@17 -> c@38", "r@26 -> return@51", "r = 1@42 -> return@51"},
		},
		{
			// Go spec, Variable declarations: a var declaration of one spec, and a
			// parenthesized list of specs, each initialized in order. Nodes:
			// a@17, var x = a@30 (the declaration, one spec), y = x@47 and
			// z = y@54 (one node per spec of the list), return z@64. Straight
			// line.
			name:     "a var declaration of one spec spans the declaration, and a spec list makes a node per spec",
			protects: "each var spec defines its own names at its own node, so a later spec reads an earlier one",
			mutation: "span a one-spec declaration by its spec (renders x = a@34), or lower a spec list as one node (y = x@47 and z = y@54 merge)",
			src:      "package p\nfunc f(a int) int { var x = a; var ( y = x; z = y; ); return z }",
			du: []string{"a@17 -> var x = a@30", "var x = a@30 -> y = x@47", "y = x@47 -> z = y@54",
				"z = y@54 -> return z@64"},
		},
		{
			// Go spec, Send statements: both the channel and the value are
			// evaluated before communication begins. Nodes: c@17, x@29,
			// c <- x@38 (one simple-statement node).
			name:     "a send statement outside a select is one node reading channel and value",
			protects: "a send depends on the channel and the value it sends",
			mutation: "lower a send statement with no uses (loses both pairs)",
			src:      "package p\nfunc f(c chan int, x int) { c <- x }",
			du:       []string{"c@17 -> c <- x@38", "x@29 -> c <- x@38"},
		},
		{
			// Go spec, Assignment statements: every operand is evaluated before any
			// target is assigned. Targets: a (redeclared, reads nothing: 1) and b
			// (new, reads a). b is read by no other target and goes first, so
			// its node reads the old a. Nodes: a@17, b@33, a@30, return
			// a + b@44.
			name:     "an acyclic multi-assignment orders its target nodes so none reads an earlier one's write",
			protects: "a target whose value reads another target's variable sees that variable's value from before the statement",
			mutation: "emit the target nodes in source order (a@30 -> b@33 replaces a@17 -> b@33)",
			src:      "package p\nfunc f(a int) int { a, b := 1, a; return a + b }",
			du:       []string{"a@17 -> b@33", "a@30 -> return a + b@44", "b@33 -> return a + b@44"},
		},
		{
			// Go spec, Assignment statements: a multi-valued call's results are
			// assigned to the targets in order. Nodes: a@17, x@30 and y@33
			// (each Uses the whole right side, g(a): a), return x + y@44.
			name:     "a multi-value call is read by every target node",
			protects: "each target of a multi-value call depends on the call's operands",
			mutation: "give the call's reads to the first target node only (loses a@17 -> y@33)",
			src:      "package p\nfunc f(a int) int { x, y := g(a); return x + y }",
			du:       []string{"a@17 -> x@30", "a@17 -> y@33", "x@30 -> return x + y@44", "y@33 -> return x + y@44"},
		},
		{
			// Go spec, Assignment statements: `_` and s.f are not variable
			// targets. The values paired with them (c, and the old b) and the
			// field target's operand s are read by the statement's first node,
			// a@41; b@52 is the last node and may-defines s. Nodes: s@17,
			// a@22, b@25, c@28, a@41, b@52, return a + b@68. Straight line.
			name:     "a blank or field target's reads ride on the statement's first node",
			protects: "the operands and values of targets that name no variable are read once, before any target is written",
			mutation: "carry a non-variable target's operands and value on no node (loses c@28, s@17 and b@25 -> a@41), or its may-definition on the first node (loses s@17 -> b@52)",
			src:      "package p\nfunc f(s S, a, b, c int) int { a, _, s.f, b = 1, c, b, 2; return a + b }",
			du: []string{"c@28 -> a@41", "s@17 -> a@41", "b@25 -> a@41", "s@17 -> b@52",
				"a@41 -> return a + b@68", "b@52 -> return a + b@68"},
		},
		{
			// Go spec, Assignment statements: the field write s.f is done when the
			// statement's last target is written. Nodes: s@17, a@22, b@25, a@41
			// (the first node: Uses s, the field target's operand, and b, its
			// own value), b@44 (the last node: defines b, may-defines s),
			// return s@57.
			name:     "a statement's write-through may-definition rides on its last node",
			protects: "a use after a multi-assignment that writes a field sees the write at the node where every target is written",
			mutation: "put the statement's write-through may-definitions on its first node (a@41 -> return s@57 replaces b@44 -> return s@57)",
			src:      "package p\nfunc f(s S, a, b int) S { s.f, a, b = 1, b, 2; return s }",
			du: []string{"s@17 -> a@41", "b@25 -> a@41", "s@17 -> b@44", "b@44 -> return s@57",
				"s@17 -> return s@57"},
		},
		{
			// Go spec, Address operators and Assignment statements: *p = 2 writes
			// through p. Nodes: x@17, p := &x@30 (Uses x, defines p,
			// may-defines x), *p = 2@39 (Uses p, may-defines p, not x),
			// g(p)@47, return x@53. The write to x through p is given up.
			name:     "an indirect write may-defines the pointer, not what it points to",
			protects: "a write through a pointer reaches later reads of the pointer and no read of the pointee",
			mutation: "may-define the pointee's variable too (adds *p = 2@39 -> return x@53), or not the pointer (loses *p = 2@39 -> g(p)@47)",
			src:      "package p\nfunc f(x int) int { p := &x; *p = 2; g(p); return x }",
			du: []string{"x@17 -> p := &x@30", "p := &x@30 -> *p = 2@39", "p := &x@30 -> g(p)@47",
				"*p = 2@39 -> g(p)@47", "x@17 -> return x@53", "p := &x@30 -> return x@53"},
		},
		{
			// Go spec, IncDec statements: s.f++ is s.f += 1, a write through s.
			// Nodes: s@17, s.f++@26 (Uses s, may-defines s), return s@33.
			name:     "a field increment may-defines its base",
			protects: "a use after x.f++ sees the increment and the base's earlier definition",
			mutation: "make x.f++ define its base killing (loses s@17 -> return s@33), or define nothing (loses s.f++@26 -> return s@33)",
			src:      "package p\nfunc f(s S) S { s.f++; return s }",
			du:       []string{"s@17 -> s.f++@26", "s.f++@26 -> return s@33", "s@17 -> return s@33"},
		},
		{
			// lower.go, May-definitions: a use pairs with the killing definition
			// behind a chain of may-definitions and with the nearest one only.
			// Nodes: s@17, s.f = 1@26, s.g = 2@35 (each Uses s, may-defines
			// s), return s@44. s.g = 2 follows s.f = 1 on the only path, so
			// s.f = 1 does not pair with the return.
			name:     "a use pairs with the nearest may-definition and the killing one behind the chain",
			protects: "a chain of field writes yields only the nearest write and the base's definition at a later use",
			mutation: "pair a use with every may-definition reaching it (adds s.f = 1@26 -> return s@44)",
			src:      "package p\nfunc f(s S) S { s.f = 1; s.g = 2; return s }",
			du: []string{"s@17 -> s.f = 1@26", "s@17 -> s.g = 2@35", "s.f = 1@26 -> s.g = 2@35",
				"s.g = 2@35 -> return s@44", "s@17 -> return s@44"},
		},
		{
			// Go spec, Address operators: &s.f and &a[0] take the address of part
			// of s and a. Nodes: s@17, a@22, g(&s.f)@33 (Uses s, may-defines
			// s), h(&a[0])@42 (Uses a, may-defines a), k(s, a)@52.
			name:     "taking the address of a field or element may-defines its base",
			protects: "a use after &x.f or &a[i] is passed on sees the write the callee may make",
			mutation: "take the base of &x.f or &a[i] to be no variable (loses g(&s.f)@33 -> k(s, a)@52 or h(&a[0])@42 -> k(s, a)@52)",
			src:      "package p\nfunc f(s S, a []int) { g(&s.f); h(&a[0]); k(s, a) }",
			du: []string{"s@17 -> g(&s.f)@33", "a@22 -> h(&a[0])@42", "s@17 -> k(s, a)@52",
				"g(&s.f)@33 -> k(s, a)@52", "a@22 -> k(s, a)@52", "h(&a[0])@42 -> k(s, a)@52"},
		},
		{
			// Go spec, Short variable declarations: the unpaired right side g(&z)
			// is evaluated once, before x and y are assigned. Nodes: z := 0@25,
			// x@33 (the first node: Uses z, may-defines z, defines x), y@36
			// (Uses z, which it reads after x@33's may-definition by position:
			// the sound over-approximation of reading it once), return
			// x + y + z@48.
			name:     "an address taken in an unpaired right side lands on the first node",
			protects: "the may-definition of an address taken by a multi-value call is attached to the statement's first node, never its last",
			mutation: "put an unpaired right side's may-definitions on the last node (loses x@33 -> y@36)",
			src:      "package p\nfunc f() int { z := 0; x, y := g(&z); return x + y + z }",
			du: []string{"z := 0@25 -> x@33", "z := 0@25 -> y@36", "x@33 -> y@36", "x@33 -> return x + y + z@48",
				"y@36 -> return x + y + z@48", "z := 0@25 -> return x + y + z@48"},
		},
		{
			// Go spec, Constant declarations, Type declarations and Declarations and
			// scope: const x and type y shadow the parameters in the inner block
			// and make no node. Nodes: x@17, y@20, g(x, y)@60 (Uses nothing),
			// return x + y@71.
			name:     "a const or type declaration makes no node and shadows",
			protects: "a name a const or type declaration rebinds is no variable read in its block",
			mutation: "let a const or type declaration leave the enclosing binding visible (adds x@17 -> g(x, y)@60 and y@20 -> g(x, y)@60)",
			src:      "package p\nfunc f(x, y int) int { { const x = 2; type y int; g(x, y) }; return x + y }",
			du:       []string{"x@17 -> return x + y@71", "y@20 -> return x + y@71"},
		},
		{
			// Go spec, Go statements and Defer statements: the function value and
			// parameters are evaluated where the statement executes. Nodes:
			// x@17, go g(x)@26, defer h(x)@35.
			name:     "go and defer read their call's arguments",
			protects: "a go or defer statement depends on the arguments it evaluates",
			mutation: "give a go or defer statement no uses (loses x@17 -> go g(x)@26 or x@17 -> defer h(x)@35)",
			src:      "package p\nfunc f(x int) { go g(x); defer h(x) }",
			du:       []string{"x@17 -> go g(x)@26", "x@17 -> defer h(x)@35"},
		},
		{
			// Go spec, Go statements, Defer statements and Function literals: the
			// literal may run at any later point, so its writes are
			// may-definitions of its statement's node. Nodes: x := 0@25,
			// go func() { x = 1 }()@33 (may-defines x), defer func() { x++
			// }()@56 (Uses x, may-defines x), return x@80.
			name:     "a go or defer literal's writes are may-definitions of its node",
			protects: "a use after a go or defer whose literal writes a variable sees the write and the definition before it",
			mutation: "record a go or defer literal's writes as no definition (loses go func() { x = 1 }()@33 -> defer func() { x++ }()@56 and defer func() { x++ }()@56 -> return x@80), or as killing (loses x := 0@25 -> return x@80)",
			src:      "package p\nfunc f() int { x := 0; go func() { x = 1 }(); defer func() { x++ }(); return x }",
			du: []string{"x := 0@25 -> go func() { x = 1 }()@33", "go func() { x = 1 }()@33 -> defer func() { x++ }()@56",
				"x := 0@25 -> defer func() { x++ }()@56", "defer func() { x++ }()@56 -> return x@80",
				"x := 0@25 -> return x@80"},
		},
		{
			// Go spec, Declarations and scope and Short variable declarations: the
			// if body's := declares a new x after reading the parameter x.
			// Nodes: x@17, true@33 (condition), x := x + 1@40, g(x)@52, return
			// x@60. Succ: true → {x := x + 1, return x}; g(x) → return x.
			name:     "an inner-block := shadows and reads the outer name first",
			protects: "a short declaration in an inner block reads the enclosing variable and defines a new one no later outer use sees",
			mutation: "declare a := target before reading its right side (loses x@17 -> x := x + 1@40), or bind it in the enclosing block (adds x := x + 1@40 -> return x@60)",
			src:      "package p\nfunc f(x int) int { if true { x := x + 1; g(x) }; return x }",
			cd:       []string{"true@33 -> x := x + 1@40", "true@33 -> g(x)@52"},
			du:       []string{"x@17 -> x := x + 1@40", "x := x + 1@40 -> g(x)@52", "x@17 -> return x@60"},
		},
		{
			// Go spec, Composite literals: a map key is an expression. Nodes:
			// k@17, return map[int]int{k: 1}@38.
			name:     "a composite-literal key naming a variable is a use",
			protects: "a map literal depends on the variable its key names",
			mutation: "skip keyed elements' keys in collect (loses k@17 -> return map[int]int{k: 1}@38)",
			src:      "package p\nfunc f(k int) map[int]int { return map[int]int{k: 1} }",
			du:       []string{"k@17 -> return map[int]int{k: 1}@38"},
		},
		{
			// Go spec, Function literals and Declarations and scope: the literal's
			// parameter x shadows f's. Nodes: x@17, h := func(x int) int {
			// return x }@30 (Uses nothing, defines h), return h(1)@65.
			name:     "a function literal's own parameters shadow enclosing names",
			protects: "a literal reading its own parameter captures no enclosing variable of that name",
			mutation: "resolve a literal's reads without its parameters shadowing (adds x@17 -> h := func(x int) int { return x }@30)",
			src:      "package p\nfunc f(x int) int { h := func(x int) int { return x }; return h(1) }",
			du:       []string{"h := func(x int) int { return x }@30 -> return h(1)@65"},
		},
		{
			// Go spec, Function literals: a literal nested in a literal shares the
			// enclosing function's variables. Nodes: y@17, h := func() func()
			// int { return func() int { return y } }@30 (Uses y), return
			// h()()@89.
			name:     "a nested literal's captures reach the outer creating node",
			protects: "the node creating a literal depends on every enclosing variable a literal nested in it reads",
			mutation: "stop capture resolution at a nested literal (loses y@17 -> h := func() func() int { return func() int { return y } }@30)",
			src:      "package p\nfunc f(y int) int { h := func() func() int { return func() int { return y } }; return h()() }",
			du: []string{"y@17 -> h := func() func() int { return func() int { return y } }@30",
				"h := func() func() int { return func() int { return y } }@30 -> return h()()@89"},
		},
		{
			// Go spec, Function literals: the literal writes s through a field, i
			// by ++ and a through &a[0]. Nodes: s@17, a@22, i := 0@37, h :=
			// func() { s.f = 1; i++; g(&a[0]) }@45 (Uses s, i and a,
			// may-defines each, defines h), h()@85, g(s)@90, g(a)@96, return
			// i@102.
			name:     "a literal's field, increment and address writes are may-definitions at its creation",
			protects: "a use after a literal writing an enclosing variable by a field, ++ or & sees the creating node",
			mutation: "record a literal's field write, ++ or element address as a use only (loses h := ...@45 -> g(s)@90, -> return i@102 or -> g(a)@96)",
			src:      "package p\nfunc f(s S, a []int) int { i := 0; h := func() { s.f = 1; i++; g(&a[0]) }; h(); g(s); g(a); return i }",
			du: []string{"s@17 -> h := func() { s.f = 1; i++; g(&a[0]) }@45", "a@22 -> h := func() { s.f = 1; i++; g(&a[0]) }@45",
				"i := 0@37 -> h := func() { s.f = 1; i++; g(&a[0]) }@45", "h := func() { s.f = 1; i++; g(&a[0]) }@45 -> h()@85",
				"h := func() { s.f = 1; i++; g(&a[0]) }@45 -> g(s)@90", "s@17 -> g(s)@90",
				"h := func() { s.f = 1; i++; g(&a[0]) }@45 -> g(a)@96", "a@22 -> g(a)@96",
				"h := func() { s.f = 1; i++; g(&a[0]) }@45 -> return i@102", "i := 0@37 -> return i@102"},
		},
		{
			// Go spec, Function literals: the literal only writes n. Nodes:
			// n := 0@25, h := func() { n = 1 }@33 (may-defines n, a χ that
			// reads n's prior version), h()@56, return n@61.
			name:     "a literal that only writes a variable still pairs at its creation",
			protects: "the creating node of a write-only literal pairs with the definitions it may leave in place",
			mutation: "pair a creating node only with the variables its literal reads (loses n := 0@25 -> h := func() { n = 1 }@33)",
			src:      "package p\nfunc f() int { n := 0; h := func() { n = 1 }; h(); return n }",
			du: []string{"n := 0@25 -> h := func() { n = 1 }@33", "h := func() { n = 1 }@33 -> h()@56",
				"h := func() { n = 1 }@33 -> return n@61", "n := 0@25 -> return n@61"},
		},
		{
			// Go spec, Expression switches: a case with several values matches if
			// any equals the tag. Nodes: x@17, x@37 (tag head), 1, 2@46 (the case
			// condition, spanning the list), return 1@52, return 0@64. Succ:
			// x@37 → 1, 2 → {return 1, return 0}. IPDom: 1, 2 → EXIT.
			name:     "a case list of several values spans the list",
			protects: "a multi-value case is one condition naming every value it compares",
			mutation: "span only the first value of a case list (1, 2@46 renders as 1@46)",
			src:      "package p\nfunc f(x int) int { switch x { case 1, 2: return 1 }; return 0 }",
			cd:       []string{"1, 2@46 -> return 1@52", "1, 2@46 -> return 0@64"},
			du:       []string{"x@17 -> x@37", "x@37 -> 1, 2@46"},
		},
		{
			// Go spec, Logical operators and Operator precedence: in (a || b) && c,
			// a true skips b and reaches c; a false evaluates b; c is evaluated
			// only when a || b holds. Only the three non-short-circuit operands
			// are nodes: a@37, b@42, c@48, then the condition (a || b) &&
			// c@36 and g()@52. Succ: a → {c, b}; b → {c, condition}; c →
			// condition; condition → {g(), EXIT}. IPDom: a, b, c → condition →
			// EXIT. Frontier walks: a over b and c; b over c. Every path from a
			// passes b or c, which redefine the result, so a's definition
			// reaches no use.
			name:     "a nested || inside && is wired by short-circuit",
			protects: "the left operand of || reaches the operand after the || when true and the right operand when false",
			mutation: "wire || like && (loses a@37 -> c@48), or make the parenthesized || a node of its own",
			src:      "package p\nfunc f(a, b, c bool) { if (a || b) && c { g() } }",
			cd:       []string{"a@37 -> b@42", "a@37 -> c@48", "b@42 -> c@48", "(a || b) && c@36 -> g()@52"},
			du: []string{"a@17 -> a@37", "b@20 -> b@42", "c@23 -> c@48", "b@42 -> (a || b) && c@36",
				"c@48 -> (a || b) && c@36"},
		},
		{
			// Go spec, For statements with single condition: the condition is
			// evaluated before each iteration. The condition's first hoisted
			// operand a@34 is the loop head and the back-edge target. Nodes:
			// a@17, b@20, a@34, b@39, a && b@34 (condition), g()@43. Succ:
			// a@34 → {b@39, condition}; b@39 → condition; condition → {g(),
			// EXIT}; g() → a@34. IPDom: g() → a@34 → condition → EXIT; b@39 →
			// condition. Frontier walks: a@34 over b@39; the condition over
			// g(), a@34 and itself.
			name:     "a loop whose condition is hoisted && heads at its first operand",
			protects: "each iteration re-evaluates the short-circuit condition from its first operand",
			mutation: "make the condition node the back-edge target (loses a && b@34 -> a@34; g()@43 reaches the condition directly)",
			src:      "package p\nfunc f(a, b bool) { for a && b { g() } }",
			cd: []string{"a@34 -> b@39", "a && b@34 -> g()@43", "a && b@34 -> a@34",
				"a && b@34 -> a && b@34"},
			du: []string{"a@17 -> a@34", "b@20 -> b@39", "a@34 -> a && b@34", "b@39 -> a && b@34"},
		},
		{
			// Go spec, Type switches: a clause listing several types matches any
			// of them; with no alias the guard's value is held by a variable the
			// lowering owns. Nodes: v@17, v.(type)@37 (head, Uses v), int,
			// string@53 (type case, Uses the owned variable), return 1@66,
			// return 0@78. IPDom: int, string → EXIT.
			name:     "an aliasless type switch holds its guard in an owned variable, and a type list spans the list",
			protects: "a type case with no alias still tests the value the head evaluated, over every type it lists",
			mutation: "give an aliasless type switch no tag variable (loses v.(type)@37 -> int, string@53), or span a type list by its first type (renders int@53)",
			src:      "package p\nfunc f(v any) int { switch v.(type) { case int, string: return 1 }; return 0 }",
			cd:       []string{"int, string@53 -> return 1@66", "int, string@53 -> return 0@78"},
			du:       []string{"v@17 -> v.(type)@37", "v.(type)@37 -> int, string@53"},
		},
		{
			// Go spec, Expression switches: a missing switch expression is
			// equivalent to true, so there is no head and each case condition
			// reads its own operands. Nodes: x@17, x > 0@44, return 1@51, return
			// 0@63.
			name:     "a tagless switch has no head and its case reads its own operands",
			protects: "a tagless switch's case condition depends on the variables it names",
			mutation: "lower case conditions of a tagless switch without their own reads (loses x@17 -> x > 0@44)",
			src:      "package p\nfunc f(x int) int { switch { case x > 0: return 1 }; return 0 }",
			cd:       []string{"x > 0@44 -> return 1@51", "x > 0@44 -> return 0@63"},
			du:       []string{"x@17 -> x > 0@44"},
		},
		{
			// Go spec, Expression switches: the cases are tested top to bottom, and
			// default runs only when none matches, wherever it stands. Nodes:
			// x@17, y := 0@30, x@45 (head), 1@54, 2@85 (conditions, in source
			// order, default skipped), y = 1@57, y = 3@73, y = 2@88, return
			// y@97. Succ: 1 → {y = 1, 2}; 2 → {y = 2, y = 3}. IPDom: 1, 2 →
			// return y. Frontier walks: 1 over y = 1 and 2; 2 over y = 3 and
			// y = 2. Every path writes y, so y := 0 reaches nothing.
			name:     "a default in the middle is tested last",
			protects: "a default clause is entered from the last case condition's false edge, not from the case before it in the source",
			mutation: "enter default from the condition before it in the source (1@54 -> y = 3@73 replaces 2@85 -> y = 3@73)",
			src:      "package p\nfunc f(x int) int { y := 0; switch x { case 1: y = 1; default: y = 3; case 2: y = 2 }; return y }",
			cd:       []string{"1@54 -> y = 1@57", "1@54 -> 2@85", "2@85 -> y = 3@73", "2@85 -> y = 2@88"},
			du: []string{"x@17 -> x@45", "x@45 -> 1@54", "x@45 -> 2@85", "y = 1@57 -> return y@97",
				"y = 3@73 -> return y@97", "y = 2@88 -> return y@97"},
		},
		{
			// Go spec, Expression switches: with no case, default always runs.
			// Nodes: x@17, y := 0@30, x@45 (head), y = 1@58 (entered from the
			// head), return y@67. Straight line: y = 1 kills y := 0.
			name:     "a switch with only default enters it from the head",
			protects: "a default-only switch always runs its default body",
			mutation: "let a switch with no case also leave by its head's fringe (adds y := 0@30 -> return y@67)",
			src:      "package p\nfunc f(x int) int { y := 0; switch x { default: y = 1 }; return y }",
			du:       []string{"x@17 -> x@45", "y = 1@58 -> return y@67"},
		},
		{
			// Go spec, If statements: the init statement runs before the condition,
			// in the if's implicit block. Nodes: a@17, x := g(a)@33, x > 0@44,
			// return x@52, return a@64. IPDom: x > 0 → EXIT.
			name:     "an if init statement is its own node",
			protects: "the condition reads the variable the init statement declares, not the init's operands",
			mutation: "fold the init statement into the condition (x := g(a)@33 vanishes and the condition reads a)",
			src:      "package p\nfunc f(a int) int { if x := g(a); x > 0 { return x }; return a }",
			cd:       []string{"x > 0@44 -> return x@52", "x > 0@44 -> return a@64"},
			du: []string{"a@17 -> x := g(a)@33", "x := g(a)@33 -> x > 0@44", "x := g(a)@33 -> return x@52",
				"a@17 -> return a@64"},
		},
		{
			// Go spec, Switch statements: the init statement runs before the tag
			// is evaluated. Nodes: a@17, x := g(a)@37, x@48 (tag head), 1@57,
			// return x@60, return a@72. IPDom: 1 → EXIT.
			name:     "a switch init statement is its own node and the tag reads only what it names",
			protects: "the tag head depends on the init's variable, not on the init's operands",
			mutation: "let the tag head Use the init statement's reads (adds a@17 -> x@48)",
			src:      "package p\nfunc f(a int) int { switch x := g(a); x { case 1: return x }; return a }",
			cd:       []string{"1@57 -> return x@60", "1@57 -> return a@72"},
			du: []string{"a@17 -> x := g(a)@37", "x := g(a)@37 -> x@48", "x@48 -> 1@57",
				"x := g(a)@37 -> return x@60", "a@17 -> return a@72"},
		},
		{
			// Go spec, Handling panics: panic stops the function's normal
			// execution. Nodes: x@17, x < 0@33, panic(x)@41 (a Jump, then
			// Throw: no catch exists, so to EXIT), return x@53. IPDom: x < 0 →
			// EXIT.
			name:     "a panic statement leaves the function",
			protects: "the statements after a conditional panic depend on the condition that avoids it",
			mutation: "lower panic as an ordinary call statement (panic(x)@41 falls through to return x@53, and x < 0@33 no longer controls return x@53)",
			src:      "package p\nfunc f(x int) int { if x < 0 { panic(x) }; return x }",
			cd:       []string{"x < 0@33 -> panic(x)@41", "x < 0@33 -> return x@53"},
			du:       []string{"x@17 -> x < 0@33", "x@17 -> panic(x)@41", "x@17 -> return x@53"},
		},
		{
			// Go spec, Break statements and Continue statements: an unlabelled
			// continue begins the innermost loop's next iteration at its post
			// statement, and break ends it. Nodes: n@17, i := 0@30, i < n@38
			// (head), i == 2@54, continue@63, i == 5@78, break@87, g(i)@96,
			// i++@45. Succ: i < n → {i == 2, EXIT}; i == 2 → {continue,
			// i == 5}; continue → i++; i == 5 → {break, g(i)}; break → EXIT;
			// g(i) → i++ → i < n. IPDom: i == 2, i == 5, break, i < n → EXIT;
			// continue, g(i) → i++ → i < n. Frontier walks: i < n over i == 2;
			// i == 2 over continue, i++, i < n and i == 5; i == 5 over break,
			// g(i), i++ and i < n.
			name:     "unlabelled break and continue target the innermost loop",
			protects: "continue reaches the post statement and break the loop's exit",
			mutation: "resolve an unlabelled continue to the loop's exit (loses i == 2@54 -> i++@45 and i == 2@54 -> i < n@38)",
			src:      "package p\nfunc f(n int) { for i := 0; i < n; i++ { if i == 2 { continue }; if i == 5 { break }; g(i) } }",
			cd: []string{"i < n@38 -> i == 2@54", "i == 2@54 -> continue@63", "i == 2@54 -> i++@45",
				"i == 2@54 -> i < n@38", "i == 2@54 -> i == 5@78", "i == 5@78 -> break@87", "i == 5@78 -> g(i)@96",
				"i == 5@78 -> i++@45", "i == 5@78 -> i < n@38"},
			du: []string{"n@17 -> i < n@38", "i := 0@30 -> i < n@38", "i++@45 -> i < n@38",
				"i := 0@30 -> i == 2@54", "i++@45 -> i == 2@54", "i := 0@30 -> i == 5@78", "i++@45 -> i == 5@78",
				"i := 0@30 -> g(i)@96", "i++@45 -> g(i)@96", "i := 0@30 -> i++@45", "i++@45 -> i++@45"},
		},
		{
			// Go spec, Break statements: break M names no enclosing statement
			// labelled M, so the program is invalid; the jump is lowered
			// unresolved, with no successor. Nodes: for@21 (head), break M@27.
			// Augmentation adds break M → EXIT.
			name:       "a break naming no open frame is unresolved",
			protects:   "a labelled break with no matching frame is counted, never resolved to the innermost loop",
			mutation:   "resolve a labelled break naming no frame to the innermost loop (unresolved becomes 0)",
			src:        "package p\nfunc f() { for { break M } }",
			unresolved: 1,
		},
		{
			// Go spec, Select statements: a select with no cases blocks forever.
			// Nodes: select@21, whose one successor is itself; the sink
			// component cannot reach EXIT, so augmentation adds select → EXIT.
			// Frontier walk: select over itself.
			name:     "an empty select is a self-loop",
			protects: "select {} never completes, so nothing after it is reached from it",
			mutation: "lower an empty select as falling through (loses select@21 -> select@21)",
			src:      "package p\nfunc f() { select {} }",
			cd:       []string{"select@21 -> select@21"},
		},
		{
			// Go spec, Select statements and Declarations and scope: n is declared
			// in no block of f. Nodes: c@17, select@31 (head, Uses c), n =
			// <-c@45 (Uses the variable the head defines, defines nothing),
			// g()@57.
			name:     "a select receive into a name that resolves to no variable defines nothing",
			protects: "an unresolved receive target keeps the clause node and never reaches the builder as -1",
			mutation: "drop the variable >= 0 test on a receive target (the clause Defs -1 and panics), or drop the clause node (loses select@31 -> n = <-c@45)",
			src:      "package p\nfunc f(c chan int) { select { case n = <-c: }; g() }",
			du:       []string{"c@17 -> select@31", "select@31 -> n = <-c@45"},
		},
		{
			// Go spec, Select statements: v, ok := <-c assigns both on the chosen
			// clause. Nodes: c@17, select@35, v@49 and ok@52 (each Uses the
			// variable the head defines), ok@66 (condition), return v@71, return
			// 0@85. IPDom: ok@66 → EXIT.
			name:     "a two-target receive is a node per target, each reading the head's value",
			protects: "both targets of a receive depend on the channel the head evaluated",
			mutation: "give the head's variable to the receive's first node only (loses select@35 -> ok@52)",
			src:      "package p\nfunc f(c chan int) int { select { case v, ok := <-c: if ok { return v } }; return 0 }",
			cd:       []string{"ok@66 -> return v@71", "ok@66 -> return 0@85"},
			du: []string{"c@17 -> select@35", "select@35 -> v@49", "select@35 -> ok@52", "ok@52 -> ok@66",
				"v@49 -> return v@71"},
		},
		{
			// Go spec, Select statements: the default clause runs when no
			// communication can proceed; it evaluates nothing on entry. Nodes:
			// c@17, x := 0@35, select@43, x = <-c@57, x = 1@75, return x@84.
			// Succ: select → {x = <-c, x = 1} → return x. IPDom: select →
			// return x. Both clauses write x, so x := 0 reaches nothing.
			name:     "a select's default clause is entered from the head",
			protects: "a select default clause's body depends on the head choosing it",
			mutation: "enter a select's default clause from after the other clauses (loses select@43 -> x = 1@75)",
			src:      "package p\nfunc f(c chan int) int { x := 0; select { case x = <-c: default: x = 1 }; return x }",
			cd:       []string{"select@43 -> x = <-c@57", "select@43 -> x = 1@75"},
			du: []string{"c@17 -> select@43", "select@43 -> x = <-c@57", "x = <-c@57 -> return x@84",
				"x = 1@75 -> return x@84"},
		},
		{
			// Go spec, For statements with for clause: an absent condition is
			// equivalent to true. Nodes: n@17, i := 0@30, for@26 (head, one
			// successor), i > n@49, break@57, i++@40. Succ: for → i > n →
			// {break, i++}; i++ → for; break → EXIT. IPDom: i++ → for → i > n
			// → EXIT. Frontier walk: i > n over break, i++, for and itself.
			name:     "a for clause with no condition has a head with no exit edge",
			protects: "a for clause without a condition leaves only by its break",
			mutation: "give the head an exit edge (for@26 -> EXIT: i > n@49 no longer controls for@26)",
			src:      "package p\nfunc f(n int) { for i := 0; ; i++ { if i > n { break } } }",
			cd:       []string{"i > n@49 -> break@57", "i > n@49 -> i++@40", "i > n@49 -> for@26", "i > n@49 -> i > n@49"},
			du: []string{"n@17 -> i > n@49", "i := 0@30 -> i > n@49", "i++@40 -> i > n@49", "i := 0@30 -> i++@40",
				"i++@40 -> i++@40"},
		},
		{
			// Go spec, For statements with range clause: an identifier target
			// assigned by = is written each iteration. Nodes: xs@17, k := 0@33,
			// xs@55 (range expression), range@49 (head), k@45 (defines k, Uses
			// the iteration variable), return k@63. Succ: range → {k, return
			// k}; k → range.
			name:     "a range target assigned by = defines it",
			protects: "a use after a range loop with an = target sees the loop's writes and the value before it",
			mutation: "let a range target assigned by = define nothing (loses k@45 -> return k@63)",
			src:      "package p\nfunc f(xs []int) int { k := 0; for k = range xs { }; return k }",
			cd:       []string{"range@49 -> k@45", "range@49 -> range@49"},
			du: []string{"xs@17 -> xs@55", "xs@55 -> range@49", "xs@55 -> k@45", "k := 0@33 -> return k@63",
				"k@45 -> return k@63"},
		},
		{
			// Go spec, For statements with range clause: := declares both key and
			// value. Nodes: m@17, s := 0@38, m@64, range@58, k@50, v@53 (each
			// Uses the iteration variable), s += k + v@68, return s@82. Succ:
			// range → {k, return s}; k → v → s += k + v → range.
			name:     "a range with := key and value makes a node per target",
			protects: "both the key and the value depend on the range expression as evaluated before the loop",
			mutation: "give the iteration variable to the key node only (loses m@64 -> v@53)",
			src:      "package p\nfunc f(m map[int]int) int { s := 0; for k, v := range m { s += k + v }; return s }",
			cd: []string{"range@58 -> k@50", "range@58 -> v@53", "range@58 -> s += k + v@68",
				"range@58 -> range@58"},
			du: []string{"m@17 -> m@64", "m@64 -> range@58", "m@64 -> k@50", "m@64 -> v@53",
				"k@50 -> s += k + v@68", "v@53 -> s += k + v@68", "s := 0@38 -> s += k + v@68",
				"s += k + v@68 -> s += k + v@68", "s := 0@38 -> return s@82", "s += k + v@68 -> return s@82"},
		},
		{
			// Go spec, For statements with range clause: the range expression is
			// evaluated once, so the body's xs = nil changes no iteration.
			// Nodes: xs@17, s := 0@33, xs@59, range@53, v@48, s += v@64,
			// xs = nil@72, return s@84. Succ: range → {v, return s}; v →
			// s += v → xs = nil → range. xs = nil reaches no use.
			name:     "a body write to the iterated variable does not reach the head",
			protects: "the loop head depends on the range expression as evaluated, never on a body's write to its variables",
			mutation: "let the head read the range expression's names (adds xs = nil@72 -> range@53)",
			src:      "package p\nfunc f(xs []int) int { s := 0; for _, v := range xs { s += v; xs = nil }; return s }",
			cd: []string{"range@53 -> v@48", "range@53 -> s += v@64", "range@53 -> xs = nil@72",
				"range@53 -> range@53"},
			du: []string{"xs@17 -> xs@59", "xs@59 -> range@53", "xs@59 -> v@48", "v@48 -> s += v@64",
				"s := 0@33 -> s += v@64", "s += v@64 -> s += v@64", "s := 0@33 -> return s@84",
				"s += v@64 -> return s@84"},
		},
		{
			// Go spec, Function literals and Short variable declarations: the
			// literal paired with x is created when x's value is evaluated.
			// Nodes: z := 0@25, x@33 (defines x, may-defines z by the literal's
			// write), y@36, x()@62, return y + z@67. Straight line.
			name:     "a literal's write in a multi-target statement lands on its target's node",
			protects: "the may-definition a paired function literal makes is attached to the node of the target it is paired with",
			mutation: "attach the literal's may-definition to the statement's last node (loses x@33 -> return y + z@67, adds z := 0@25 -> y@36)",
			src:      "package p\nfunc f() int { z := 0; x, y := func() { z = 1 }, 2; x(); return y + z }",
			du: []string{"z := 0@25 -> x@33", "x@33 -> x()@62", "x@33 -> return y + z@67", "z := 0@25 -> return y + z@67",
				"y@36 -> return y + z@67"},
		},
		{
			// Go spec, Assignment statements: x is assigned after g(&x) runs, so
			// what g writes through &x is overwritten. Targets: x reads y, y's
			// value g(&x) reads x: a cycle, broken at x, whose node x@38 also
			// uses its own old value; y@41's read of x resolves through x@38,
			// and y@41's may-definition of x is dropped, since x@38 already
			// assigned x. Nodes: y@17, x := 0@30, x@38, y@41, return x@55.
			name:     "a may-definition on a node after the one assigning the variable is dropped",
			protects: "an address taken in a later target's value never overrides the statement's own assignment of the variable",
			mutation: "keep the may-definition of a variable an earlier node of the statement assigned (adds y@41 -> return x@55)",
			src:      "package p\nfunc f(y int) int { x := 0; x, y = y, g(&x); return x }",
			du:       []string{"y@17 -> x@38", "x := 0@30 -> x@38", "x@38 -> y@41", "x@38 -> return x@55"},
		},
		{
			// Go spec, For statements with range clause: an index target's
			// operands are evaluated on every iteration, as in an assignment.
			// Nodes: a@17, xs@20, i@30, xs@56 (range expression), range@50
			// (head), a[i]@43 (Uses a, i and the iteration variable,
			// may-defines a), i++@61. Succ: range → {a[i], EXIT}; a[i] → i++
			// → range.
			name:     "an index range target reads its operands each iteration and may-defines its base",
			protects: "an index target of a range sees the body's writes to its index and leaves its base's earlier value in place",
			mutation: "evaluate the target's operands once before the loop (loses i++@61 -> a[i]@43), or make it define nothing (loses a[i]@43 -> a[i]@43)",
			src:      "package p\nfunc f(a, xs []int, i int) { for a[i] = range xs { i++ } }",
			cd:       []string{"range@50 -> a[i]@43", "range@50 -> i++@61", "range@50 -> range@50"},
			du: []string{"xs@20 -> xs@56", "xs@56 -> range@50", "xs@56 -> a[i]@43", "a@17 -> a[i]@43",
				"a[i]@43 -> a[i]@43", "i@30 -> a[i]@43", "i++@61 -> a[i]@43", "i@30 -> i++@61", "i++@61 -> i++@61"},
		},
		{
			// Go spec, For statements with range clause: range over an integer n
			// evaluates n once; the values run from 0 to n-1. Nodes: n@17,
			// s := 0@30, n@53, range@47, i@42, s += i@57, return s@67.
			name:     "a range over an integer follows the iteration model",
			protects: "the head and the value of an integer range depend on the integer as evaluated before the loop",
			mutation: "let the head read the range expression's names (adds n@17 -> range@47)",
			src:      "package p\nfunc f(n int) int { s := 0; for i := range n { s += i }; return s }",
			cd:       []string{"range@47 -> i@42", "range@47 -> s += i@57", "range@47 -> range@47"},
			du: []string{"n@17 -> n@53", "n@53 -> range@47", "n@53 -> i@42", "i@42 -> s += i@57",
				"s := 0@30 -> s += i@57", "s += i@57 -> s += i@57", "s := 0@30 -> return s@67",
				"s += i@57 -> return s@67"},
		},
		{
			// Go spec, For statements with range clause: range over a channel
			// evaluates the channel once and receives until it is closed. Nodes:
			// c@17, s := 0@35, c@58, range@52, v@47, s += v@62, return s@72.
			name:     "a range over a channel follows the iteration model",
			protects: "the head and the value of a channel range depend on the channel as evaluated before the loop",
			mutation: "let the head read the range expression's names (adds c@17 -> range@52)",
			src:      "package p\nfunc f(c chan int) int { s := 0; for v := range c { s += v }; return s }",
			cd:       []string{"range@52 -> v@47", "range@52 -> s += v@62", "range@52 -> range@52"},
			du: []string{"c@17 -> c@58", "c@58 -> range@52", "c@58 -> v@47", "v@47 -> s += v@62",
				"s := 0@35 -> s += v@62", "s += v@62 -> s += v@62", "s := 0@35 -> return s@72",
				"s += v@62 -> return s@72"},
		},
		{
			// Go spec, For statements with range clause (range over function): the
			// function literal written as x is created once, at x's node, which
			// Uses n (the literal's n++ reads it) and may-defines n. Nodes: n@17,
			// func(y func(int) bool) { n++ }@41, range@35, k@30, g(k, n)@74.
			// Succ: x's node → range → {k, EXIT}; k → g(k, n) → range.
			name:     "a function literal ranged over is created at the range expression's node",
			protects: "a use in the body sees the range function's captured write, made where the function is created",
			mutation: "lower a function literal written as x with no captures (loses n@17 -> func(y func(int) bool) { n++ }@41 and func(y func(int) bool) { n++ }@41 -> g(k, n)@74)",
			src:      "package p\nfunc f(n int) { for k := range func(y func(int) bool) { n++ } { g(k, n) } }",
			cd:       []string{"range@35 -> k@30", "range@35 -> g(k, n)@74", "range@35 -> range@35"},
			du: []string{"n@17 -> func(y func(int) bool) { n++ }@41", "func(y func(int) bool) { n++ }@41 -> range@35",
				"func(y func(int) bool) { n++ }@41 -> k@30", "k@30 -> g(k, n)@74",
				"func(y func(int) bool) { n++ }@41 -> g(k, n)@74", "n@17 -> g(k, n)@74"},
		},
		{
			// Go spec, For statements with range clause and Length and capacity:
			// len(a) of an array a is constant, and with no iteration variable
			// the specification does not evaluate it; the lowering does not
			// apply that exception, so x's node Uses a. Nodes: var a
			// [3]int@25, n := 0@39, len(a)@57, range@51, n++@66, return n@73.
			// Succ: len(a) → range → {n++, return n}; n++ → range.
			name:     "the constant-range exception is not applied",
			protects: "a range expression is always read at its own node, so no read is dropped on a guess about its type",
			mutation: "apply the constant-range exception (loses var a [3]int@25 -> len(a)@57)",
			src:      "package p\nfunc f() int { var a [3]int; n := 0; for range len(a) { n++ }; return n }",
			cd:       []string{"range@51 -> n++@66", "range@51 -> range@51"},
			du: []string{"var a [3]int@25 -> len(a)@57", "len(a)@57 -> range@51", "n := 0@39 -> n++@66",
				"n++@66 -> n++@66", "n := 0@39 -> return n@73", "n++@66 -> return n@73"},
		},
		{
			// Go spec, Type switches: the alias is declared in each clause, and a
			// clause's own declaration shadows it. Nodes: v@17, x :=
			// v.(type)@37 (head, defines the alias), int@58 and string@78 (type
			// cases, each Using the alias), return x@63, x := 0@86, return
			// x@94, return 0@106. Succ: int → {return x@63, string}; string →
			// {x := 0, return 0}. IPDom: int, string → EXIT. Frontier walks:
			// int over return x@63 and string; string over x := 0, return x@94
			// and return 0.
			name:     "a type switch's alias serves every clause and a clause's declaration shadows it",
			protects: "every clause reads the head's alias, and a clause's own := shadows it for that clause's later reads",
			mutation: "resolve a clause's read to the alias past its own declaration (x := v.(type)@37 -> return x@94 replaces x := 0@86 -> return x@94), or give a type case after the first no use of the alias (loses x := v.(type)@37 -> string@78)",
			src:      "package p\nfunc f(v any) int { switch x := v.(type) { case int: return x; case string: x := 0; return x }; return 0 }",
			cd: []string{"int@58 -> return x@63", "int@58 -> string@78", "string@78 -> x := 0@86",
				"string@78 -> return x@94", "string@78 -> return 0@106"},
			du: []string{"v@17 -> x := v.(type)@37", "x := v.(type)@37 -> int@58", "x := v.(type)@37 -> string@78",
				"x := v.(type)@37 -> return x@63", "x := 0@86 -> return x@94"},
		},
		{
			// Go spec, Break statements and Continue statements: an unlabelled
			// break in a switch ends the switch, continue passes it to the
			// loop, and break L ends the select labelled L. Nodes: n@17, c@24,
			// i := 0@42, i < n@50, i@70 (tag head), 1@79, 2@94, break@82,
			// continue@97, L@109, select@112, <-c@126, break L@131, g(i)@142,
			// i++@57. Succ: i < n → {i@70, EXIT}; i@70 → 1 → {break, 2}; 2 →
			// {continue, L}; break → L; continue → i++; L → select → <-c →
			// break L → g(i) → i++ → i < n. IPDom: i@70 → 1; 1, 2, continue,
			// g(i) → i++ → i < n → EXIT; break → L → select → <-c → break L
			// → g(i). Frontier walks: i < n over i@70, 1, i++ and itself; 1
			// over break, L, select, <-c, break L, g(i) and 2; 2 over
			// continue, L, select, <-c, break L and g(i).
			name:     "a break in a switch ends the switch, and a labelled break ends its select",
			protects: "an unlabelled break inside a switch in a loop and a labelled select break both continue after their statement, inside the loop",
			mutation: "let an unlabelled break target the innermost loop, skipping the switch (break@82 reaches EXIT: 1@79 loses L@109 and the nodes after it)",
			src:      "package p\nfunc f(n int, c chan int) { for i := 0; i < n; i++ { switch i { case 1: break; case 2: continue }; L: select { case <-c: break L }; g(i) } }",
			cd: []string{"i < n@50 -> i@70", "i < n@50 -> 1@79", "i < n@50 -> i++@57", "i < n@50 -> i < n@50",
				"1@79 -> break@82", "1@79 -> 2@94", "1@79 -> L@109", "1@79 -> select@112", "1@79 -> <-c@126",
				"1@79 -> break L@131", "1@79 -> g(i)@142", "2@94 -> continue@97", "2@94 -> L@109",
				"2@94 -> select@112", "2@94 -> <-c@126", "2@94 -> break L@131", "2@94 -> g(i)@142"},
			du: []string{"n@17 -> i < n@50", "i := 0@42 -> i < n@50", "i++@57 -> i < n@50", "i := 0@42 -> i@70",
				"i++@57 -> i@70", "i@70 -> 1@79", "i@70 -> 2@94", "c@24 -> select@112", "select@112 -> <-c@126",
				"i := 0@42 -> g(i)@142", "i++@57 -> g(i)@142", "i := 0@42 -> i++@57", "i++@57 -> i++@57"},
		},
		{
			// Go spec, Short variable declarations: := may redeclare a variable of
			// the same block, which it then assigns. Nodes: x := 0@21, L@29,
			// g(x)@32, x@38 (assigns the existing x), y@41, g(y)@52, goto
			// L@58. Succ: straight line to goto L → L. {L, …, goto L} cannot
			// reach EXIT; L is its first RPO member, so augmentation adds L →
			// EXIT. Frontier walk: L over every member and itself.
			name:     "a := redeclaration assigns the existing variable",
			protects: "a redeclared name in := is the same variable, so its new value reaches reads of the old one around a loop",
			mutation: "declare every := target anew (loses x@38 -> g(x)@32)",
			src:      "package p\nfunc f() { x := 0; L: g(x); x, y := 1, 2; g(y); goto L }",
			cd: []string{"L@29 -> g(x)@32", "L@29 -> x@38", "L@29 -> y@41", "L@29 -> g(y)@52",
				"L@29 -> goto L@58", "L@29 -> L@29"},
			du: []string{"x := 0@21 -> g(x)@32", "x@38 -> g(x)@32", "y@41 -> g(y)@52"},
		},
		{
			// Go spec, Labeled statements and Continue statements: L and M both
			// label the for, so continue L targets it. Nodes: n@17, L@26,
			// M@29, i := 0@36, i < n@44, i > 1@60, continue L@68, break M@82,
			// i++@51. Succ: i < n → {i > 1, EXIT}; i > 1 → {continue L,
			// break M}; continue L → i++ → i < n; break M → EXIT. IPDom:
			// i > 1 → EXIT; continue L → i++ → i < n → EXIT.
			name:     "stacked labels all name the labelled loop",
			protects: "a jump naming the outer of two stacked labels reaches the loop they both label",
			mutation: "let only the innermost label reach the frame (continue L is unresolved: unresolved 1)",
			src:      "package p\nfunc f(n int) { L: M: for i := 0; i < n; i++ { if i > 1 { continue L }; break M } }",
			cd: []string{"i < n@44 -> i > 1@60", "i > 1@60 -> continue L@68", "i > 1@60 -> i++@51",
				"i > 1@60 -> i < n@44", "i > 1@60 -> break M@82"},
			du: []string{"n@17 -> i < n@44", "i := 0@36 -> i < n@44", "i++@51 -> i < n@44", "i := 0@36 -> i > 1@60",
				"i++@51 -> i > 1@60", "i := 0@36 -> i++@51", "i++@51 -> i++@51"},
		},
		{
			// Go spec, Logical operators and Operator precedence: a && b || c is
			// (a && b) || c in value position. Only the operands are nodes:
			// a@45, b@50, c@55, then return a && b || c@38. Succ: a → {b, c};
			// b → {return, c}; c → return. IPDom: a, b, c → return. Frontier
			// walks: a over b and c; b over c. a's definition of the result is
			// redefined by b or c on every path, so it reaches no use.
			name:     "a mixed short-circuit chain in value position makes a node per operand",
			protects: "a false left operand of && skips to the || operand, and the owning node Uses the result the deciding operands define",
			mutation: "make the && sub-expression a node of its own, or send a false a to the owning node (loses a@45 -> c@55)",
			src:      "package p\nfunc f(a, b, c bool) bool { return a && b || c }",
			cd:       []string{"a@45 -> b@50", "a@45 -> c@55", "b@50 -> c@55"},
			du: []string{"a@17 -> a@45", "b@20 -> b@50", "c@23 -> c@55", "b@50 -> return a && b || c@38",
				"c@55 -> return a && b || c@38"},
		},
		{
			// A statement the parser cannot recognise: `x )` is taken to recover
			// as one ERROR node in the statement list (an assumed recovery,
			// settled by the harness's parse). The lowering's default branch
			// makes it one Stmt node carrying its reads. Nodes: x@17, g(x)@26,
			// x )@32, h(x)@37. Straight line.
			name:      "an unrecognised statement keeps its reads",
			protects:  "a syntax error inside a body drops no read of the statement it garbles",
			mutation:  "let the default branch make no node or collect nothing (loses x@17 -> x )@32)",
			src:       "package p\nfunc f(x int) { g(x); x ); h(x) }",
			recovered: true,
			du:        []string{"x@17 -> g(x)@26", "x@17 -> x )@32", "x@17 -> h(x)@37"},
		},
	})
}
