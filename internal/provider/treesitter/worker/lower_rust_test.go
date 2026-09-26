package worker

import "testing"

// TestRustLoweringGolden pins the Rust lowering's control-dependence and
// def-use pairs on hand-derived functions. A wrong pair here is a wrong
// dependence fact served to every consumer. Each source is one line, so a
// node's offset is its column; each case is callable 0 in Functions
// preorder, and each cites the section of The Rust Reference it follows.
//
// Derivation rules: augmentation adds an edge to EXIT from every node with
// no successor and from the smallest-reverse-post-order member of each sink
// strongly connected component that cannot reach EXIT; control dependence is
// the post-dominance frontier over the augmented graph with no entry-to-exit
// edge (nothing depends on ENTRY; a loop head whose back edge it controls
// depends on itself); a def-use pair is (defining node, using node) with
// every φ resolved to the definitions it merges; a may-definition kills
// nothing, so a use it reaches pairs with it and with every definition
// reaching its node.
func TestRustLoweringGolden(t *testing.T) {
	runGolden(t, "rust", []goldenCase{
		{
			// Expressions › Operator expressions › The question mark
			// operator: `x?` returns from the function when x is None.
			// Nodes: x@5 (param), x?@46 (Branch), let y = x?;@38, Some(y)@50
			// (the tail). Succ: x@5→x?→{EXIT, let}; let→Some(y)→EXIT.
			// IPDom: let → Some(y) → EXIT; x? → EXIT. Frontier walk from the
			// edge x?→let: let, Some(y).
			name:     "the question mark operator is a branch to the exit",
			protects: "everything after `e?` is control dependent on it, since `?` may return from the function",
			mutation: "lower `?` as a plain operand without its edge to Exit (x?@46 controls nothing and the two control dependences vanish)",
			src:      "fn f(x: Option<i32>) -> Option<i32> { let y = x?; Some(y) }",
			cd:       []string{"x?@46 -> let y = x?;@38", "x?@46 -> Some(y)@50"},
			du:       []string{"x@5 -> x?@46", "x@5 -> let y = x?;@38", "let y = x?;@38 -> Some(y)@50"},
		},
		{
			// Expressions › Loops and other breakable expressions › Infinite
			// loops, and › Break and loop values. Nodes: n@5, let mut i =
			// 0;@22, loop@45 (Stmt head), i += 1@52, i > n@63, break i *
			// 2@71, let r = …;@37 (Uses every read inside it: i and n), r@89.
			// Succ: loop→i += 1→i > n→{break, loop}; break→let r→r. The loop
			// has no exit edge. IPDom: i > n → break → let r; loop → i += 1
			// → i > n. Frontier walk from i > n→loop: loop, i += 1, i > n.
			name:     "a loop leaves only by its valued break, which the let consumes",
			protects: "`loop` has no exit edge of its own, and the let receiving a break's value reads the variables the break read",
			mutation: "give `loop` a false edge out of its head (loop@45 becomes a controller of i += 1 and i > n), or give the consuming let only its direct reads (loses i += 1@52 -> let r and n@5 -> let r)",
			src:      "fn f(n: i32) -> i32 { let mut i = 0; let r = loop { i += 1; if i > n { break i * 2; } }; r }",
			cd:       []string{"i > n@63 -> loop@45", "i > n@63 -> i += 1@52", "i > n@63 -> i > n@63"},
			du: []string{"n@5 -> i > n@63", "n@5 -> let r = loop { i += 1; if i > n { break i * 2; } };@37",
				"let mut i = 0;@22 -> i += 1@52", "i += 1@52 -> i += 1@52", "i += 1@52 -> i > n@63",
				"i += 1@52 -> break i * 2@71", "i += 1@52 -> let r = loop { i += 1; if i > n { break i * 2; } };@37",
				"let r = loop { i += 1; if i > n { break i * 2; } };@37 -> r@89"},
		},
		{
			// Expressions › Block expressions › Labelled block expressions.
			// Nodes: c@5, x@14, c@48, break 'a x@52, 0@66, let v = …;@31, v@71.
			// Succ: c@48→{break, 0}; break→let v (the block frame); 0→let v.
			// IPDom: c@48 → let v.
			name:     "a labelled block's valued break leaves the block, not the function",
			protects: "`break 'a v` lands after the labelled block, where both the break and the block's tail flow into the consuming let",
			mutation: "open no frame for a labelled block (the break is unresolved: unresolved becomes 1 and break 'a x@52 loses its pair to let v)",
			src:      "fn f(c: bool, x: i32) -> i32 { let v = 'a: { if c { break 'a x; } 0 }; v }",
			cd:       []string{"c@48 -> break 'a x@52", "c@48 -> 0@66"},
			du: []string{"c@5 -> c@48", "x@14 -> break 'a x@52", "c@5 -> let v = 'a: { if c { break 'a x; } 0 };@31",
				"x@14 -> let v = 'a: { if c { break 'a x; } 0 };@31", "let v = 'a: { if c { break 'a x; } 0 };@31 -> v@71"},
		},
		{
			// Expressions › Loops and other breakable expressions ›
			// Predicate pattern loops. Nodes: s@9, let mut t = 0;@31, let
			// Some(x) = s.pop()@52 (the head, a Branch), x@61 (defines x on
			// the taken path, Using the value's reads), t += x@76, t@86.
			// Succ: head→{x, t@86}; x→t += x→head. IPDom: x → t += x → head
			// → t@86.
			name:     "while let binds its pattern only on the path into the body",
			protects: "a while-let head is the loop's decision and its binding is defined after it, reading the tested value",
			mutation: "define the bound name on the head (x@61 vanishes and t += x@76 pairs with the head), or back-edge to the binding instead of the head",
			src:      "fn f(mut s: Vec<i32>) -> i32 { let mut t = 0; while let Some(x) = s.pop() { t += x; } t }",
			cd: []string{"let Some(x) = s.pop()@52 -> x@61", "let Some(x) = s.pop()@52 -> t += x@76",
				"let Some(x) = s.pop()@52 -> let Some(x) = s.pop()@52"},
			du: []string{"s@9 -> let Some(x) = s.pop()@52", "s@9 -> x@61", "x@61 -> t += x@76",
				"let mut t = 0;@31 -> t += x@76", "t += x@76 -> t += x@76", "let mut t = 0;@31 -> t@86", "t += x@76 -> t@86"},
		},
		{
			// Expressions › Loops and other breakable expressions › Iterator
			// loops, › Loop labels, › continue expressions. Nodes: v@5, let
			// mut n = 0;@25, v@53 (outer iterated value, defines the
			// iteration variable), for@44 (outer head), a@48, v@66, for@57
			// (inner head), b@61, a == b@73, continue 'o@82, n += b@97, n@109.
			// Succ: for@44→{a, n@109}; a→v@66→for@57→{b, for@44};
			// b→a == b→{continue, n += b}; continue→for@44; n += b→for@57.
			// IPDom: for@44 → n@109; a → v@66 → for@57 → for@44; b → a == b
			// → for@44; continue → for@44; n += b → for@57. Frontier walks:
			// for@44 over a, v@66, for@57 and itself; for@57 over b and
			// a == b; a == b over continue, n += b and for@57.
			name:     "a labelled continue re-enters the outer iterator loop",
			protects: "`continue 'o` targets the labelled outer loop's head, and each head and binding reads the value iterated before its loop",
			mutation: "resolve a labelled continue to the innermost loop (a == b@73 loses for@57 as a dependent and continue 'o@82 reaches for@57), or give the heads no use of the iteration variable (loses v@53 -> for@44 and v@66 -> for@57)",
			src:      "fn f(v: &[i32]) -> i32 { let mut n = 0; 'o: for a in v { for b in v { if a == b { continue 'o; } n += b; } } n }",
			cd: []string{"for@44 -> a@48", "for@44 -> v@66", "for@44 -> for@57", "for@44 -> for@44",
				"for@57 -> b@61", "for@57 -> a == b@73", "a == b@73 -> continue 'o@82", "a == b@73 -> n += b@97",
				"a == b@73 -> for@57"},
			du: []string{"v@5 -> v@53", "v@5 -> v@66", "v@53 -> for@44", "v@53 -> a@48", "v@66 -> for@57", "v@66 -> b@61",
				"a@48 -> a == b@73", "b@61 -> a == b@73", "b@61 -> n += b@97", "let mut n = 0;@25 -> n += b@97",
				"n += b@97 -> n += b@97", "let mut n = 0;@25 -> n@109", "n += b@97 -> n@109"},
		},
		{
			// Expressions › Match expressions, and › Match guards. Nodes:
			// o@5, k@21, o@44 (scrutinee), None@48 (a Branch: an earlier
			// arm's bare identifier may name a unit variant; it also defines
			// the name it would bind), 0@56, Some(x)@59, x@64, x > k@70,
			// x@79, Some(y)@82 (the last arm is refutable, so it keeps its
			// false edge out of the match), y@87, y@93. Every pattern node
			// and binding Uses the scrutinee's reads. Succ: None→{0,
			// Some(x)}; Some(x)→{x@64, Some(y)}; x@64→x > k→{x@79, Some(y)};
			// Some(y)→{y@87, EXIT}; y@87→y@93. IPDom: every Branch → EXIT;
			// x@64 → x > k.
			name:     "a failed match guard falls to the next arm and the last refutable arm keeps its false edge",
			protects: "each arm's pattern tests the scrutinee in source order, a guard's false edge reaches the next pattern, and no arm falls into another",
			mutation: "send a failed guard out of the match (x > k@70 loses Some(y)@82 as a dependent), or drop the last arm's false edge (Some(y)@82 controls nothing)",
			src:      "fn f(o: Option<i32>, k: i32) -> i32 { match o { None => 0, Some(x) if x > k => x, Some(y) => y } }",
			cd: []string{"None@48 -> 0@56", "None@48 -> Some(x)@59", "Some(x)@59 -> x@64", "Some(x)@59 -> x > k@70",
				"Some(x)@59 -> Some(y)@82", "x > k@70 -> x@79", "x > k@70 -> Some(y)@82", "Some(y)@82 -> y@87",
				"Some(y)@82 -> y@93"},
			du: []string{"o@5 -> o@44", "o@5 -> None@48", "o@5 -> Some(x)@59", "o@5 -> x@64", "o@5 -> Some(y)@82", "o@5 -> y@87",
				"k@21 -> x > k@70", "x@64 -> x > k@70", "x@64 -> x@79", "y@87 -> y@93"},
		},
		{
			// Statements › Let statements (a let introduces a new binding,
			// shadowing), and Expressions › If and if let expressions. Nodes:
			// o@5, d@21, let d = d + 1;@38 (reads the parameter d, defines
			// a new variable), let Some(v) = o@56 (Branch), v@65, v@74, d@85.
			// Succ: head→{v@65, d@85}; v@65→v@74. IPDom: head → EXIT.
			name:     "a shadowing let is a new variable and if let binds only in its consequence",
			protects: "the else branch reads the shadowing d, never the parameter, and the if-let binding is defined on the taken path only",
			mutation: "resolve a shadowing let to the variable it shadows (d@21 reaches d@85), or define v on the if-let head (v@65 vanishes)",
			src:      "fn f(o: Option<i32>, d: i32) -> i32 { let d = d + 1; if let Some(v) = o { v } else { d } }",
			cd:       []string{"let Some(v) = o@56 -> v@65", "let Some(v) = o@56 -> v@74", "let Some(v) = o@56 -> d@85"},
			du: []string{"o@5 -> let Some(v) = o@56", "o@5 -> v@65", "d@21 -> let d = d + 1;@38",
				"let d = d + 1;@38 -> d@85", "v@65 -> v@74"},
		},
		{
			// Statements › Let statements (let-else: the else block must
			// diverge). Nodes: o@5, the declaration@30 (Branch), panic!("none")@53
			// (a plain node, which then returns), v@39, v@71. Succ: head→{panic,
			// v@39}; panic→EXIT; v@39→v@71. IPDom: head → EXIT.
			name:     "a let-else block ending in a panicking macro never falls into the binding",
			protects: "a let-else's else block reaches Exit even when it ends in a plain panicking-macro node, so the binding and the rest are control dependent on the pattern test",
			mutation: "let the else block's fringe fall through (panic!@53 → v@39: the head then controls panic only)",
			src:      "fn f(o: Option<i32>) -> i32 { let Some(v) = o else { panic!(\"none\") }; v }",
			cd: []string{"let Some(v) = o else { panic!(\"none\") };@30 -> panic!(\"none\")@53",
				"let Some(v) = o else { panic!(\"none\") };@30 -> v@39", "let Some(v) = o else { panic!(\"none\") };@30 -> v@71"},
			du: []string{"o@5 -> let Some(v) = o else { panic!(\"none\") };@30", "o@5 -> v@39", "v@39 -> v@71"},
		},
		{
			// Expressions › Closure expressions (capture modes). Nodes: x@5,
			// let mut n = 0;@22, |d: i32| n += d + x@51 (Uses n and x,
			// may-defines n; its own parameter d shadows), let mut add =
			// …;@37 (Uses every read in its span: n and x), add(1)@72, n@80.
			// The closure may run at any later point, or never, so both let
			// mut n = 0; and the creating node reach every later use of n.
			name:     "a closure's write to a captured variable is a may-definition where it is created",
			protects: "a use after a closure's creation sees both the closure's write and the definition reaching the creation",
			mutation: "record a closure's writes as uses only (loses |d: i32| n += d + x@51 -> n@80), or as a killing definition (loses let mut n = 0;@22 -> n@80)",
			src:      "fn f(x: i32) -> i32 { let mut n = 0; let mut add = |d: i32| n += d + x; add(1); n }",
			du: []string{"x@5 -> |d: i32| n += d + x@51", "x@5 -> let mut add = |d: i32| n += d + x;@37",
				"let mut n = 0;@22 -> |d: i32| n += d + x@51", "let mut n = 0;@22 -> let mut add = |d: i32| n += d + x;@37",
				"|d: i32| n += d + x@51 -> let mut add = |d: i32| n += d + x;@37",
				"let mut add = |d: i32| n += d + x;@37 -> add(1)@72",
				"|d: i32| n += d + x@51 -> n@80", "let mut n = 0;@22 -> n@80"},
		},
		{
			// Items › Associated items › Methods (self is the receiver), and
			// Expressions › Assignment expressions (a place expression). Nodes:
			// self@19, v@25, self.a = v@42 (Uses self and v, may-defines
			// self), self.a@54.
			name:     "a field write through self updates the receiver without killing it",
			protects: "`self` is a variable of the method, and a write to self.a reaches a later read of self along with self's earlier definition",
			mutation: "record no definition of the base (loses self.a = v@42 -> self.a@54), a killing one (loses self@19 -> self.a@54), or leave self unresolved (every pair of self vanishes)",
			src:      "impl S { fn g(&mut self, v: i32) -> i32 { self.a = v; self.a } }",
			du: []string{"self@19 -> self.a = v@42", "v@25 -> self.a = v@42", "self.a = v@42 -> self.a@54",
				"self@19 -> self.a@54"},
		},
		{
			// Statements › Declaration statements › Item declarations: a
			// nested function item cannot access the enclosing function's
			// locals (the compiler rejects the capture below), so it creates
			// no node and reads nothing here. Nodes: x@5, g() + x@42.
			name:     "a nested function item captures nothing and makes no node",
			protects: "a function item in a block is its own function only, and is never a closure of the enclosing one",
			mutation: "lower a nested function item as a closure (a node spanning fn g() -> i32 { x } appears, paired with x@5)",
			src:      "fn f(x: i32) -> i32 { fn g() -> i32 { x } g() + x }",
			du:       []string{"x@5 -> g() + x@42"},
		},
	})
}
