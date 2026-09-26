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
			mutation: "open no frame for a labelled block (the break names no frame: unresolved becomes 1, and the break keeps no successor, so augmentation sends it to Exit and c@48 gains let v = 'a: { if c { break 'a x; } 0 };@31 and v@71 as dependents; the def-use pairs are unchanged)",
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
			// Expressions › Borrow operators: `&mut v` lends v mutably, so
			// the call receiving it may write v. Nodes: let mut v =
			// Vec::new();@18 (defines v; a path is no read), fill(&mut v)@42
			// (Uses v, may-defines v), v.len()@56. The may-definition kills
			// nothing, so both the let and the call reach v.len().
			name:     "a mutable borrow passed to a call is a may-definition of the borrowed variable",
			protects: "a mutable borrow passed to a call reaches later uses as a may-definition",
			mutation: "drop the MayDef on &mut (loses fill(&mut v)@42 -> v.len()@56)",
			src:      "fn f() -> usize { let mut v = Vec::new(); fill(&mut v); v.len() }",
			du: []string{"let mut v = Vec::new();@18 -> fill(&mut v)@42", "let mut v = Vec::new();@18 -> v.len()@56",
				"fill(&mut v)@42 -> v.len()@56"},
		},
		{
			// Statements › Declaration statements › Item declarations: an
			// item declared in a block is an item like any other and cannot
			// access the enclosing function's locals, so it runs nothing where
			// it is declared; g's x is its own parameter. Nodes: c@5, x@14,
			// c@34 (Branch), return g(x)@64 (Uses x; g names an item), x@79.
			// Succ: c@34→{return, x@79}; return→EXIT. IPDom: c@34 → EXIT.
			name:     "a nested function item makes no node where it is declared",
			protects: "a function item in a block is its own function only, and is never a statement or closure of the enclosing one",
			mutation: "lower a nested function item as a statement or closure (a node spanning fn g(x: i32) -> i32 { x }@38 appears in the consequence, and c@34 gains it as a dependent)",
			src:      "fn f(c: bool, x: i32) -> i32 { if c { fn g(x: i32) -> i32 { x } return g(x); } x }",
			cd:       []string{"c@34 -> return g(x)@64", "c@34 -> x@79"},
			du:       []string{"c@5 -> c@34", "x@14 -> return g(x)@64", "x@14 -> x@79"},
		},
		{
			// Expressions › Operator expressions › The question mark
			// operator: on a Result, `e?` returns Err(From::from(err)) from
			// the function when e is Err, and is the Ok value otherwise.
			// Nodes: s@5, s.parse()?@69 (Branch), let n: i32 = …;@56, Ok(n +
			// 1)@81. Succ: s@5→s.parse()?→{EXIT, let}; let→Ok→EXIT. IPDom:
			// s.parse()? → EXIT; let → Ok → EXIT.
			name:     "the question mark operator on a Result is a branch to the exit",
			protects: "an Err propagated by `?` leaves the function, so everything after `e?` on a Result is control dependent on it",
			mutation: "lower `?` as a plain operand without its edge to Exit (s.parse()?@69 controls nothing and both control dependences vanish)",
			src:      "fn f(s: &str) -> Result<i32, std::num::ParseIntError> { let n: i32 = s.parse()?; Ok(n + 1) }",
			cd:       []string{"s.parse()?@69 -> let n: i32 = s.parse()?;@56", "s.parse()?@69 -> Ok(n + 1)@81"},
			du: []string{"s@5 -> s.parse()?@69", "s@5 -> let n: i32 = s.parse()?;@56",
				"let n: i32 = s.parse()?;@56 -> Ok(n + 1)@81"},
		},
		{
			// Expressions › Operator expressions › The question mark
			// operator, inside a try block (a block the unstable try_blocks
			// feature adds, whose `?` completes the block rather than the
			// function). Nodes: a@5, a?@57 (Branch), a? + 1@57 (the block's
			// tail), let r: … = try { … };@30 (Uses every read in its span),
			// r.unwrap_or(0)@67. Succ: a?→{a? + 1, let} (its break lands
			// after the block); a? + 1→let→r.unwrap_or(0). IPDom: a? → let.
			name:     "a question mark inside a try block leaves the block, not the function",
			protects: "`?` in a try block breaks to the block's end, so what follows the block does not depend on it",
			mutation: "send `?` to Exit even inside a try block (a?@57 then also controls the let and r.unwrap_or(0)@67)",
			src:      "fn f(a: Option<i32>) -> i32 { let r: Option<i32> = try { a? + 1 }; r.unwrap_or(0) }",
			cd:       []string{"a?@57 -> a? + 1@57"},
			du: []string{"a@5 -> a?@57", "a@5 -> a? + 1@57", "a@5 -> let r: Option<i32> = try { a? + 1 };@30",
				"let r: Option<i32> = try { a? + 1 };@30 -> r.unwrap_or(0)@67"},
		},
		{
			// Expressions › If and if let expressions › Chains of
			// conditions: the operands of a let chain are evaluated in
			// order, each only when the ones before it hold, and a let
			// operand's bindings are in scope for the later operands and the
			// consequence. Nodes: o@5, k@21, let Some(x) = o@41 (Branch),
			// x@50 (the binding, Using o), x > k@60 (Branch), x@68, k@79.
			// Succ: let→{x@50, k@79}; x@50→x > k→{x@68, k@79}. IPDom: let →
			// EXIT; x@50 → x > k → EXIT.
			name:     "each member of a let chain is its own branch and every false edge reaches the else",
			protects: "a later chain member runs only when the let member matched, and reads the binding the let made",
			mutation: "lower the chain as one Branch spanning it (x@50 and x > k@60 vanish as nodes, and x@68 pairs with the chain node)",
			src:      "fn f(o: Option<i32>, k: i32) -> i32 { if let Some(x) = o && x > k { x } else { k } }",
			cd: []string{"let Some(x) = o@41 -> x@50", "let Some(x) = o@41 -> x > k@60", "let Some(x) = o@41 -> k@79",
				"x > k@60 -> x@68", "x > k@60 -> k@79"},
			du: []string{"o@5 -> let Some(x) = o@41", "o@5 -> x@50", "k@21 -> x > k@60", "x@50 -> x > k@60",
				"x@50 -> x@68", "k@21 -> k@79"},
		},
		{
			// Expressions › Operator expressions › Lazy boolean operators:
			// `a && b` evaluates b only when a is true, `a || b` only when a
			// is false; `&&` binds tighter, so the condition is (a && b) ||
			// c. Nodes: a@5, b@14, c@23, a@44 (Branch), b@49, a && b@44
			// (Branch), c@54, a && b || c@44 (Branch), 1@58, 0@69. Succ:
			// a@44→{b@49, a && b}; b@49→a && b→{c@54, whole}; c@54→whole→{1,
			// 0}. IPDom: a@44 → a && b → whole → EXIT.
			name:     "the right operand of a lazy boolean operator depends on the left",
			protects: "the short-circuit edges of `&&` and `||` make each right operand control dependent on its left operand",
			mutation: "lower `&&` and `||` as plain binary operators (a@44, b@49, a && b@44 and c@54 vanish, and only the whole condition branches)",
			src:      "fn f(a: bool, b: bool, c: bool) -> i32 { if a && b || c { 1 } else { 0 } }",
			cd: []string{"a@44 -> b@49", "a && b@44 -> c@54", "a && b || c@44 -> 1@58",
				"a && b || c@44 -> 0@69"},
			du: []string{"a@5 -> a@44", "a@5 -> a && b@44", "a@5 -> a && b || c@44", "b@14 -> b@49",
				"b@14 -> a && b@44", "b@14 -> a && b || c@44", "c@23 -> c@54", "c@23 -> a && b || c@44"},
		},
		{
			// Expressions › Loops and other breakable expressions ›
			// Predicate loops: the condition is evaluated before every
			// iteration and the loop ends when it is false. Nodes: n@5, let
			// mut i = 0;@22, i < n@43 (Branch, the head), i += 2@51, i@61.
			// Succ: head→{i += 2, i@61}; i += 2→head. IPDom: i += 2 → head
			// → i@61.
			name:     "a predicate loop's back edge re-enters its condition",
			protects: "a while condition is the loop's head: it controls the body and itself, and reads the body's writes",
			mutation: "close the back edge to the body's first node instead of the condition (loses i += 2@51 -> i < n@43 and i < n@43 -> i < n@43)",
			src:      "fn f(n: i32) -> i32 { let mut i = 0; while i < n { i += 2; } i }",
			cd:       []string{"i < n@43 -> i += 2@51", "i < n@43 -> i < n@43"},
			du: []string{"n@5 -> i < n@43", "let mut i = 0;@22 -> i < n@43", "let mut i = 0;@22 -> i += 2@51",
				"i += 2@51 -> i < n@43", "i += 2@51 -> i += 2@51", "let mut i = 0;@22 -> i@61", "i += 2@51 -> i@61"},
		},
		{
			// Patterns › Wildcard pattern, and Expressions › Match
			// expressions: `_` matches any value, so the match cannot fall
			// out and a later arm is unreachable. Nodes: o@5, o@36 (the
			// scrutinee), Some(x)@40 (Branch), x@45, x@51, 0@59; the None
			// arm lowers to nothing. Succ: Some(x)→{x@45, 0}; x@45→x@51.
			// IPDom: Some(x) → EXIT.
			name:     "a wildcard arm leaves no false edge and ends the arms",
			protects: "after `_` no arm is reachable and the match has no edge past its arms",
			mutation: "treat `_` as refutable, giving it a Branch and a false edge to the next arm (a `_` Branch then controls 0@59, and the None arm is lowered)",
			src:      "fn f(o: Option<i32>) -> i32 { match o { Some(x) => x, _ => 0, None => 1 } }",
			cd:       []string{"Some(x)@40 -> x@45", "Some(x)@40 -> x@51", "Some(x)@40 -> 0@59"},
			du:       []string{"o@5 -> o@36", "o@5 -> Some(x)@40", "o@5 -> x@45", "x@45 -> x@51"},
		},
		{
			// Expressions › Assignment expressions › Destructuring
			// assignments: a tuple struct `P(a)` and a struct `S { x, y }`
			// on the left are assignee expressions, and each assignee
			// inside is assigned. Nodes: p@48, s@54, let mut a = 0;@69 (its
			// definition is killed before any read), P(a) = p@98, a@100,
			// S { x, y } = s@108, x@112, y@115, a + x + y@124 (the let x; and
			// let y; declarations make no node). Straight line.
			name:     "tuple-struct and struct assignees define every field they name",
			protects: "`P(a) = p` and `S { x, y } = s` kill the earlier definitions of a, x and y, and read nothing on the left",
			mutation: "lower a call or struct expression on the left as a place (P(a) = p@98 then reads a and defines nothing, so let mut a = 0;@69 reaches a + x + y@124)",
			src:      "struct P(i32); struct S { x: i32, y: i32 } fn f(p: P, s: S) -> i32 { let mut a = 0; let x; let y; P(a) = p; S { x, y } = s; a + x + y }",
			du: []string{"p@48 -> P(a) = p@98", "p@48 -> a@100", "s@54 -> S { x, y } = s@108", "s@54 -> x@112",
				"s@54 -> y@115", "a@100 -> a + x + y@124", "x@112 -> a + x + y@124", "y@115 -> a + x + y@124"},
		},
		{
			// Expressions › Block expressions › Async blocks: an async block
			// captures what it uses and runs only when its future is polled,
			// so it is its own function; a gen block is lowered the same way.
			// Nodes: n@9, async { n += 1; }@36 (Uses n, may-defines n), let
			// fut = …;@26, drop(fut)@55, n@66.
			name:     "an async block is its own function and its write is a may-definition where it is created",
			protects: "the async block's body is not inline code of the enclosing function: its write kills nothing there",
			mutation: "lower an async block's body inline as a plain block (n += 1@44 becomes a killing definition, so n@9 -> n@66 is lost)",
			src:      "fn f(mut n: i32) -> i32 { let fut = async { n += 1; }; drop(fut); n }",
			du: []string{"n@9 -> async { n += 1; }@36", "n@9 -> let fut = async { n += 1; };@26",
				"async { n += 1; }@36 -> let fut = async { n += 1; };@26",
				"let fut = async { n += 1; };@26 -> drop(fut)@55", "n@9 -> n@66", "async { n += 1; }@36 -> n@66"},
		},
		{
			// Expressions › Closure expressions: a closure's parameters are
			// patterns binding in its body, so a parameter x shadows the
			// enclosing x there. Nodes: x@5, y@13, |x: i32| x + y@38 (Uses
			// y only), let g = …;@30, g(1) + x@54.
			name:     "a closure parameter shadows the enclosing variable of its name",
			protects: "a closure's use of its own parameter is not a capture of the enclosing variable",
			mutation: "resolve a closure's parameter names through the enclosing scope (x@5 -> |x: i32| x + y@38 and x@5 -> let g = |x: i32| x + y;@30 appear)",
			src:      "fn f(x: i32, y: i32) -> i32 { let g = |x: i32| x + y; g(1) + x }",
			du: []string{"y@13 -> |x: i32| x + y@38", "y@13 -> let g = |x: i32| x + y;@30",
				"let g = |x: i32| x + y;@30 -> g(1) + x@54", "x@5 -> g(1) + x@54"},
		},
		{
			// Expressions › Closure expressions › Capture modes: a `move`
			// closure captures by value, so `n += 1` writes the closure's
			// copy of n, never the enclosing n. Nodes: n@9, move || n +=
			// 1@38 (Uses n, may-defines nothing), let mut g = …;@26, g()@54,
			// n@59.
			name:     "a move closure's write is not a definition of the enclosing variable",
			protects: "only the definitions before a move closure reach a later read of a variable it writes",
			mutation: "keep a move closure's writes as may-definitions (gains move || n += 1@38 -> n@59 and move || n += 1@38 -> let mut g = move || n += 1;@26)",
			src:      "fn f(mut n: i32) -> i32 { let mut g = move || n += 1; g(); n }",
			du: []string{"n@9 -> move || n += 1@38", "n@9 -> let mut g = move || n += 1;@26",
				"let mut g = move || n += 1;@26 -> g()@54", "n@9 -> n@59"},
		},
		{
			// Statements › Expression statements, and Expressions › If and if
			// let expressions: `s = 1` inside the if's block is a statement
			// of its own whose value is 1, so it reads nothing, although the
			// let consuming the if read s in the condition. Nodes: s@9, s >
			// 0@37 (Branch), s = 1@45, 2@52, 3@63, let t = …;@26 (Uses the
			// condition's read), t + s@68. Succ: s > 0→{s = 1, 3}; s = 1→2→
			// let; 3→let. IPDom: s > 0 → let.
			name:     "a statement inside a valued expression reads only its own operands",
			protects: "an enclosing statement's earlier reads are not a nested statement's reads, so an assignment inside a valued if does not use the variable it assigns",
			mutation: "start a nested statement's reads at the enclosing statement's (s = 1@45 then Uses s: gains s@9 -> s = 1@45)",
			src:      "fn f(mut s: i32) -> i32 { let t = if s > 0 { s = 1; 2 } else { 3 }; t + s }",
			cd:       []string{"s > 0@37 -> s = 1@45", "s > 0@37 -> 2@52", "s > 0@37 -> 3@63"},
			du: []string{"s@9 -> s > 0@37", "s@9 -> let t = if s > 0 { s = 1; 2 } else { 3 };@26",
				"s = 1@45 -> let t = if s > 0 { s = 1; 2 } else { 3 };@26", "s@9 -> t + s@68", "s = 1@45 -> t + s@68",
				"let t = if s > 0 { s = 1; 2 } else { 3 };@26 -> t + s@68"},
		},
		{
			// Patterns › Identifier patterns: `ref mut v` binds v to a mutable
			// reference into the matched place o, through which `*v += 1`
			// writes o. Nodes: o@9, let Some(ref mut v) = o@45 (Branch), v@62
			// (defines v, may-defines o), *v += 1@71 (may-defines v, the
			// variable it writes through), o@82. Succ: let→{v, o@82}; v→*v
			// += 1→o@82. IPDom: let → o@82.
			name:     "a ref mut binding is a may-definition of the matched variable",
			protects: "a write through a `ref mut` binding reaches later reads of the matched variable",
			mutation: "record no may-definition for a ref mut binding (loses v@62 -> o@82)",
			src:      "fn f(mut o: Option<i32>) -> Option<i32> { if let Some(ref mut v) = o { *v += 1; } o }",
			cd:       []string{"let Some(ref mut v) = o@45 -> v@62", "let Some(ref mut v) = o@45 -> *v += 1@71"},
			du: []string{"o@9 -> let Some(ref mut v) = o@45", "o@9 -> v@62", "o@9 -> o@82", "v@62 -> *v += 1@71",
				"v@62 -> o@82"},
		},
		{
			// Expressions › Borrow operators, and Place expressions and value
			// expressions: `*p` is a place, so `&mut *p` may write through p,
			// but `!b` is a value, so `&mut !b` borrows a temporary and never
			// writes b. Nodes: p@5, b@18, g(&mut !b, &mut *p)@37 (Uses b and
			// p, may-defines p only), h(*p)@58, b@65.
			name:     "a mutable borrow reaches its base through a dereference but not through a value operator",
			protects: "only `*` of the unary operators is a place, so a borrow of `!b` or `-b` is no may-definition of b",
			mutation: "walk any unary operator to a base (gains g(&mut !b, &mut *p)@37 -> b@65), or none (loses g(&mut !b, &mut *p)@37 -> h(*p)@58)",
			src:      "fn f(p: &mut i32, b: bool) -> bool { g(&mut !b, &mut *p); h(*p); b }",
			du: []string{"p@5 -> g(&mut !b, &mut *p)@37", "b@18 -> g(&mut !b, &mut *p)@37", "p@5 -> h(*p)@58",
				"g(&mut !b, &mut *p)@37 -> h(*p)@58", "b@18 -> b@65"},
		},
		{
			// Macros › Macro invocation (the arguments are a token tree the
			// expansion evaluates), and Expressions › Operator expressions ›
			// The question mark operator: `g(s)?` returns from the function
			// on an error. Nodes: s@5, println!("{}", g(s)?)@34 (a Branch
			// Using s; g and the literal read nothing), Ok(1)@57. Succ:
			// println!→{EXIT, Ok(1)}; Ok(1)→EXIT. IPDom: println! → EXIT.
			name:     "a question mark inside a macro's arguments is a branch to the exit",
			protects: "everything after a macro invocation whose token tree holds `e?` is control dependent on it",
			mutation: "walk only a token tree's named children, or lower the invocation as a plain node (println!(\"{}\", g(s)?)@34 controls nothing)",
			src:      "fn f(s: &str) -> Result<i32, E> { println!(\"{}\", g(s)?); Ok(1) }",
			cd:       []string{"println!(\"{}\", g(s)?)@34 -> Ok(1)@57"},
			du:       []string{"s@5 -> println!(\"{}\", g(s)?)@34"},
		},
		{
			// Macros › Macro invocation, Expressions › Return expressions,
			// and › Lazy boolean operators (`c || return 0` returns when c is
			// false). The `||` follows an operand, so it is no closure. Nodes:
			// c@5, assert!(c || return 0)@23 (a Branch Using c), 1@47. Succ:
			// assert!→{EXIT, 1}; 1→EXIT. IPDom: assert! → EXIT.
			name:     "a return inside a macro's arguments is a branch to the exit",
			protects: "a return in a token tree leaves the function, so what follows the invocation depends on it",
			mutation: "leave return out of the token-tree jumps, or read `c ||` as a closure head (assert!(c || return 0)@23 controls nothing)",
			src:      "fn f(c: bool) -> i32 { assert!(c || return 0); 1 }",
			cd:       []string{"assert!(c || return 0)@23 -> 1@47"},
			du:       []string{"c@5 -> assert!(c || return 0)@23"},
		},
		{
			// Expressions › Operator expressions › The question mark
			// operator (it returns from the enclosing function or closure),
			// and › Closure expressions. The `o?` is in the closure `|| …`,
			// so the invocation does not jump. Nodes: o@5, println!(…)@38
			// (a Stmt Using o), Some(1)@77.
			name:     "a question mark inside a closure in a macro's arguments stays in the closure",
			protects: "a jump a closure written in a token tree makes is the closure's, never a jump of the enclosing function",
			mutation: "ignore closure heads in token trees (println!@38 becomes a Branch controlling Some(1)@77)",
			src:      "fn f(o: Option<i32>) -> Option<i32> { println!(\"{:?}\", (|| Some(o? + 1))()); Some(1) }",
			du:       []string{"o@5 -> println!(\"{:?}\", (|| Some(o? + 1))())@38"},
		},
		{
			// Expressions › Loops and other breakable expressions › break
			// expressions: an unlabelled break leaves the innermost loop,
			// here the one inside the token tree. Nodes: x@5,
			// println!(…)@22 (a Stmt Using x), x@57.
			name:     "a break inside a loop in a macro's arguments stays in that loop",
			protects: "an unlabelled break whose loop is written in the token tree is no jump of the enclosing function",
			mutation: "ignore loop heads in token trees (the break is issued with no loop open: unresolved becomes 1)",
			src:      "fn f(x: i32) -> i32 { println!(\"{}\", loop { break x; }); x }",
			du:       []string{"x@5 -> println!(\"{}\", loop { break x; })@22", "x@5 -> x@57"},
		},
		{
			// Expressions › Operator expressions › Borrow operators: `&mut
			// v` in the token tree lends v mutably to take, which may write
			// it. Nodes: let mut v = vec![1];@18, println!(…)@39 (Uses v,
			// may-defines v; std, mem and take are path segments), v.len()@81.
			name:     "a mutable borrow inside a macro's arguments is a may-definition of the borrowed variable",
			protects: "`&mut x` in a token tree reaches later uses of x as a may-definition on the invocation",
			mutation: "record no borrow for `& mut` tokens (loses println!(…)@39 -> v.len()@81)",
			src:      "fn f() -> usize { let mut v = vec![1]; println!(\"{:?}\", std::mem::take(&mut v)); v.len() }",
			du: []string{"let mut v = vec![1];@18 -> println!(\"{:?}\", std::mem::take(&mut v))@39",
				"let mut v = vec![1];@18 -> v.len()@81", "println!(\"{:?}\", std::mem::take(&mut v))@39 -> v.len()@81"},
		},
		{
			// Format strings follow the std::fmt library documentation (not
			// the Reference): Named parameters (an implicit `{x}` captures
			// the variable x unless a named argument x is passed), Width
			// (`w$` names the width argument, captured the same way), and
			// Escaping (`{{` is a literal brace). Nodes: x@5, w@13, y@23,
			// z@31, v@39, format!(…)@64 (Uses x and w by capture, v by the
			// named argument's value; y is the named argument, z is text).
			name:     "format-string captures are reads unless a named argument or an escaped brace",
			protects: "`{x}` and `{:w$}` read the variables they capture, and neither a named argument nor `{{z}}` reads a local",
			mutation: "read no format captures (loses x@5 and w@13 -> format!), ignore named arguments or `=` (adds y@23 -> format!), or ignore `{{` (adds z@31 -> format!)",
			src:      "fn f(x: i32, w: usize, y: i32, z: i32, v: Vec<i32>) -> String { format!(\"{x:>w$}{y}{{z}}\", y = v.len()) }",
			du: []string{"x@5 -> format!(\"{x:>w$}{y}{{z}}\", y = v.len())@64", "w@13 -> format!(\"{x:>w$}{y}{{z}}\", y = v.len())@64",
				"v@39 -> format!(\"{x:>w$}{y}{{z}}\", y = v.len())@64"},
		},
		{
			// Paths (a segment names a module, type or item, not a local),
			// Expressions › Field access and Method-call expressions (the
			// name after `.` is a field or method), and the std::fmt library
			// documentation's Named parameters (`y = e` names an argument).
			// Nodes: self@15, x@21, max@29, len@39, v@51, format!(…)@76
			// (Uses x and v only).
			name:     "path segments, field and method names, and named arguments in a token tree are not reads",
			protects: "a local whose name is also a path segment, a method name or a named argument is not read by the invocation",
			mutation: "read names before or after `::` (adds self@15 and max@29 -> format!), or after `.` (adds len@39 -> format!)",
			src:      "impl S { fn g(&self, x: i32, max: i32, len: usize, v: Vec<i32>) -> String { format!(\"{} {} {y}\", self::h(x), i32::max(x, 1), y = v.len()) } }",
			du: []string{"x@21 -> format!(\"{} {} {y}\", self::h(x), i32::max(x, 1), y = v.len())@76",
				"v@51 -> format!(\"{} {} {y}\", self::h(x), i32::max(x, 1), y = v.len())@76"},
		},
	})
}
