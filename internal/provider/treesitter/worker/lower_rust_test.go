package worker

import "testing"

// TestRustLoweringGolden pins the Rust lowering's control-dependence and
// def-use pairs on hand-derived functions. A wrong pair here is a wrong
// dependence fact served to every consumer. Each source is one line, so a
// node's offset is its column; each case is callable 0 in Functions
// preorder, and each cites the section of The Rust Reference it follows.
//
// The derivation and rendering rules are runGolden's.
func TestRustLoweringGolden(t *testing.T) {
	runGolden(t, "rust", []goldenCase{
		{
			// Expressions › Operator expressions › The try propagation
			// expression: `x?` returns from the function when x is None.
			// Nodes: x@5 (param), x?@46 (Branch, Using x and defining its
			// result, the Ok value), let y = x?;@38 (Uses that result),
			// Some(y)@50 (the tail). Succ: x@5→x?→{EXIT, let}; let→Some(y)→EXIT.
			// IPDom: let → Some(y) → EXIT; x? → EXIT. Frontier walk from the
			// edge x?→let: let, Some(y).
			name:     "the question mark operator is a branch to the exit",
			protects: "everything after `e?` is control dependent on it, since `?` may return from the function",
			mutation: "lower `?` as a plain operand without its edge to Exit (x?@46 controls nothing and the two control dependences vanish), or let the let re-read x instead of the `?`'s result (x?@46 -> let y = x?;@38 becomes x@5 -> let y = x?;@38)",
			src:      "fn f(x: Option<i32>) -> Option<i32> { let y = x?; Some(y) }",
			cd:       []string{"x?@46 -> let y = x?;@38", "x?@46 -> Some(y)@50"},
			du:       []string{"x@5 -> x?@46", "x?@46 -> let y = x?;@38", "let y = x?;@38 -> Some(y)@50"},
		},
		{
			// Expressions › Loops and other breakable expressions › Infinite
			// loops, and › `break` and loop values. Nodes: n@5, let mut i =
			// 0;@22, loop@45 (Stmt head), i += 1@52, i > n@63, break i *
			// 2@71 (Uses i, defines the loop's result), let r = …;@37 (Uses
			// the loop's result), r@89.
			// Succ: loop→i += 1→i > n→{break, loop}; break→let r→r. The loop
			// has no exit edge. IPDom: i > n → break → let r; loop → i += 1
			// → i > n. Frontier walk from i > n→loop: loop, i += 1, i > n.
			// The one definition of the result reaching let r is the break's.
			name:     "a loop leaves only by its valued break, which the let consumes",
			protects: "`loop` has no exit edge of its own, and the let receiving a break's value takes it from the break through the loop's result variable",
			mutation: "give `loop` a false edge out of its head (loop@45 becomes a controller of i += 1 and i > n), give a valued break no definition of its loop's result (loses break i * 2@71 -> let r), or let the let re-read the break's operand (gains i += 1@52 -> let r)",
			src:      "fn f(n: i32) -> i32 { let mut i = 0; let r = loop { i += 1; if i > n { break i * 2; } }; r }",
			cd:       []string{"i > n@63 -> loop@45", "i > n@63 -> i += 1@52", "i > n@63 -> i > n@63"},
			du: []string{"n@5 -> i > n@63",
				"let mut i = 0;@22 -> i += 1@52", "i += 1@52 -> i += 1@52", "i += 1@52 -> i > n@63",
				"i += 1@52 -> break i * 2@71", "break i * 2@71 -> let r = loop { i += 1; if i > n { break i * 2; } };@37",
				"let r = loop { i += 1; if i > n { break i * 2; } };@37 -> r@89"},
		},
		{
			// Expressions › Loops and other breakable expressions › Labeled
			// block expressions.
			// Nodes: c@5, x@14, c@48, break 'a x@52 (Uses x, defines the
			// block's result), 0@66 (the tail, defining it too), let v =
			// …;@31 (Uses the result), v@71.
			// Succ: c@48→{break, 0}; break→let v (the block frame); 0→let v.
			// IPDom: c@48 → let v.
			name:     "a labelled block's valued break leaves the block, not the function",
			protects: "`break 'a v` lands after the labelled block, where both the break and the block's tail hand their values to the consuming let through the block's result",
			mutation: "open no builder frame for a labelled block (the break names no frame: unresolved becomes 1, and the break keeps no successor, so augmentation sends it to Exit and c@48 gains let v = 'a: { if c { break 'a x; } 0 };@31 and v@71 as dependents), give a labelled break no definition of its block's result (loses break 'a x@52 -> let v), or let the let re-read the break's operand (gains x@14 -> let v)",
			src:      "fn f(c: bool, x: i32) -> i32 { let v = 'a: { if c { break 'a x; } 0 }; v }",
			cd:       []string{"c@48 -> break 'a x@52", "c@48 -> 0@66"},
			du: []string{"c@5 -> c@48", "x@14 -> break 'a x@52",
				"break 'a x@52 -> let v = 'a: { if c { break 'a x; } 0 };@31",
				"0@66 -> let v = 'a: { if c { break 'a x; } 0 };@31", "let v = 'a: { if c { break 'a x; } 0 };@31 -> v@71"},
		},
		{
			// Expressions › Loops and other breakable expressions ›
			// Predicate loops › `while let` patterns. Nodes: s@9, let mut t = 0;@31, let
			// Some(x) = s.pop()@52 (the head, a Branch Using s and defining
			// an owned variable holding the popped value), x@61 (defines x on
			// the taken path, Using that variable), t += x@76, t@86.
			// Succ: head→{x, t@86}; x→t += x→head. IPDom: x → t += x → head
			// → t@86.
			name:     "while let binds its pattern only on the path into the body",
			protects: "a while-let head is the loop's decision and its binding is defined after it, reading the tested value",
			mutation: "define the bound name on the head (x@61 vanishes and t += x@76 pairs with the head), back-edge to the binding instead of the head, or let the binding re-read the value's names (let Some(x) = s.pop()@52 -> x@61 becomes s@9 -> x@61)",
			src:      "fn f(mut s: Vec<i32>) -> i32 { let mut t = 0; while let Some(x) = s.pop() { t += x; } t }",
			cd: []string{"let Some(x) = s.pop()@52 -> x@61", "let Some(x) = s.pop()@52 -> t += x@76",
				"let Some(x) = s.pop()@52 -> let Some(x) = s.pop()@52"},
			du: []string{"s@9 -> let Some(x) = s.pop()@52", "let Some(x) = s.pop()@52 -> x@61", "x@61 -> t += x@76",
				"let mut t = 0;@31 -> t += x@76", "t += x@76 -> t += x@76", "let mut t = 0;@31 -> t@86", "t += x@76 -> t@86"},
		},
		{
			// Expressions › Loops and other breakable expressions › Iterator
			// loops, › Loop labels, › `continue` expressions. Nodes: v@5, let
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
			// Expressions › `match` expressions, and › Match guards. Nodes:
			// o@5, k@21, o@44 (scrutinee), None@48 (a Branch: an earlier
			// arm's bare identifier may name a unit variant; it also defines
			// the name it would bind), 0@56, Some(x)@59, x@64, x > k@70,
			// x@79, Some(y)@82 (the last arm is refutable, so it keeps its
			// false edge out of the match), y@87, y@93. The scrutinee node
			// defines an owned variable, which every pattern node and binding
			// Uses. Succ: None→{0,
			// Some(x)}; Some(x)→{x@64, Some(y)}; x@64→x > k→{x@79, Some(y)};
			// Some(y)→{y@87, EXIT}; y@87→y@93. IPDom: every Branch → EXIT;
			// x@64 → x > k.
			name:     "a failed match guard falls to the next arm and the last refutable arm keeps its false edge",
			protects: "each arm's pattern tests the scrutinee in source order, a guard's false edge reaches the next pattern, and no arm falls into another",
			mutation: "send a failed guard out of the match (x > k@70 loses Some(y)@82 as a dependent), drop the last arm's false edge (Some(y)@82 controls nothing), or let the patterns re-read the scrutinee's names (each o@44 -> pattern or binding pair becomes o@5 -> it)",
			src:      "fn f(o: Option<i32>, k: i32) -> i32 { match o { None => 0, Some(x) if x > k => x, Some(y) => y } }",
			cd: []string{"None@48 -> 0@56", "None@48 -> Some(x)@59", "Some(x)@59 -> x@64", "Some(x)@59 -> x > k@70",
				"Some(x)@59 -> Some(y)@82", "x > k@70 -> x@79", "x > k@70 -> Some(y)@82", "Some(y)@82 -> y@87",
				"Some(y)@82 -> y@93"},
			du: []string{"o@5 -> o@44", "o@44 -> None@48", "o@44 -> Some(x)@59", "o@44 -> x@64", "o@44 -> Some(y)@82",
				"o@44 -> y@87",
				"k@21 -> x > k@70", "x@64 -> x > k@70", "x@64 -> x@79", "y@87 -> y@93"},
		},
		{
			// Statements › Let statements (a let introduces a new binding,
			// shadowing), and Expressions › `if` expressions › `if let`
			// patterns. Nodes:
			// o@5, d@21, let d = d + 1;@38 (reads the parameter d, defines
			// a new variable), let Some(v) = o@56 (Branch, defining an owned
			// variable holding o's value), v@65 (Uses it), v@74, d@85.
			// Succ: head→{v@65, d@85}; v@65→v@74. IPDom: head → EXIT.
			name:     "a shadowing let is a new variable and if let binds only in its consequence",
			protects: "the else branch reads the shadowing d, never the parameter, and the if-let binding is defined on the taken path only",
			mutation: "resolve a shadowing let to the variable it shadows (d@21 reaches d@85), or define v on the if-let head (v@65 vanishes)",
			src:      "fn f(o: Option<i32>, d: i32) -> i32 { let d = d + 1; if let Some(v) = o { v } else { d } }",
			cd:       []string{"let Some(v) = o@56 -> v@65", "let Some(v) = o@56 -> v@74", "let Some(v) = o@56 -> d@85"},
			du: []string{"o@5 -> let Some(v) = o@56", "let Some(v) = o@56 -> v@65", "d@21 -> let d = d + 1;@38",
				"let d = d + 1;@38 -> d@85", "v@65 -> v@74"},
		},
		{
			// Statements › Let statements (let-else: the else block must
			// diverge). Nodes: o@5, the declaration@30 (Branch, defining an
			// owned variable holding o's value, which v@39 Uses), panic!("none")@53
			// (a plain node, which then returns), v@39, v@71. Succ: head→{panic,
			// v@39}; panic→EXIT; v@39→v@71. IPDom: head → EXIT.
			name:     "a let-else block ending in a panicking macro never falls into the binding",
			protects: "a let-else's else block reaches Exit even when it ends in a plain panicking-macro node, so the binding and the rest are control dependent on the pattern test",
			mutation: "let the else block's fringe fall through (panic!@53 → v@39: the head then controls panic only)",
			src:      "fn f(o: Option<i32>) -> i32 { let Some(v) = o else { panic!(\"none\") }; v }",
			cd: []string{"let Some(v) = o else { panic!(\"none\") };@30 -> panic!(\"none\")@53",
				"let Some(v) = o else { panic!(\"none\") };@30 -> v@39", "let Some(v) = o else { panic!(\"none\") };@30 -> v@71"},
			du: []string{"o@5 -> let Some(v) = o else { panic!(\"none\") };@30",
				"let Some(v) = o else { panic!(\"none\") };@30 -> v@39", "v@39 -> v@71"},
		},
		{
			// Expressions › Closure expressions, and Types › Closure types ›
			// Capture modes (a non-move closure captures n by unique
			// borrow). Nodes: x@5,
			// let mut n = 0;@22, |d: i32| n += d + x@51 (Uses n and x,
			// may-defines n; its own parameter d shadows), let mut add =
			// …;@37 (Uses the creating node's result, the closure; the
			// captures are the creating node's own reads),
			// add(1)@72, n@80.
			// The closure may run at any later point, or never, so both let
			// mut n = 0; and the creating node reach every later use of n.
			name:     "a closure's write to a captured variable is a may-definition where it is created",
			protects: "a use after a closure's creation sees both the closure's write and the definition reaching the creation",
			mutation: "record a closure's writes as uses only (loses |d: i32| n += d + x@51 -> n@80), as a killing definition (loses let mut n = 0;@22 -> n@80), record a compound-assigned capture as a write only, without its read (loses let mut n = 0;@22 -> |d: i32| n += d + x@51, the creating node's Use of the n it only compound-assigns), give the creating node no result (loses |d: i32| n += d + x@51 -> let mut add = |d: i32| n += d + x;@37), or let the let re-read the captures (gains x@5 and let mut n = 0;@22 -> let mut add = |d: i32| n += d + x;@37)",
			src:      "fn f(x: i32) -> i32 { let mut n = 0; let mut add = |d: i32| n += d + x; add(1); n }",
			du: []string{"x@5 -> |d: i32| n += d + x@51",
				"let mut n = 0;@22 -> |d: i32| n += d + x@51",
				"|d: i32| n += d + x@51 -> let mut add = |d: i32| n += d + x;@37",
				"let mut add = |d: i32| n += d + x;@37 -> add(1)@72",
				"|d: i32| n += d + x@51 -> n@80", "let mut n = 0;@22 -> n@80"},
		},
		{
			// Items › Associated items › Associated functions and methods ›
			// Methods (self is the receiver), and
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
			// Expressions › Operator expressions › Borrow operators: `&mut v`
			// lends v mutably, so
			// the call receiving it may write v. Nodes: let mut v =
			// Vec::new();@18 (defines v; a path is no read), fill(&mut v)@42
			// (Uses v, may-defines v), v.len()@56. v.len() pairs with the let,
			// the killing definition reaching it through the may-definition,
			// and with the call, the nearest may-definition.
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
			// Expressions › Operator expressions › The try propagation
			// expression: on a Result, `e?` returns Err(From::from(err)) from
			// the function when e is Err, and is the Ok value otherwise.
			// Nodes: s@5, s.parse()?@69 (Branch, defining the Ok value), let
			// n: i32 = …;@56 (Uses it), Ok(n + 1)@81. Succ: s@5→s.parse()?→{EXIT, let}; let→Ok→EXIT. IPDom:
			// s.parse()? → EXIT; let → Ok → EXIT.
			name:     "the question mark operator on a Result is a branch to the exit",
			protects: "an Err propagated by `?` leaves the function, so everything after `e?` on a Result is control dependent on it",
			mutation: "lower `?` as a plain operand without its edge to Exit (s.parse()?@69 controls nothing and both control dependences vanish), or let the let re-read s instead of the `?`'s result (s.parse()?@69 -> let n becomes s@5 -> let n)",
			src:      "fn f(s: &str) -> Result<i32, std::num::ParseIntError> { let n: i32 = s.parse()?; Ok(n + 1) }",
			cd:       []string{"s.parse()?@69 -> let n: i32 = s.parse()?;@56", "s.parse()?@69 -> Ok(n + 1)@81"},
			du: []string{"s@5 -> s.parse()?@69", "s.parse()?@69 -> let n: i32 = s.parse()?;@56",
				"let n: i32 = s.parse()?;@56 -> Ok(n + 1)@81"},
		},
		{
			// The Rust Unstable Book, Language features › try_blocks (The
			// Rust Reference does not define try blocks: a `?` in one
			// completes the block rather than the function; valid on nightly
			// behind `#![feature(try_blocks)]`), and
			// Expressions › Operator expressions › The try propagation
			// expression. Nodes: a@5, a?@57 (Branch Using a; it defines its
			// Ok value and, completing the block, the block's result, both
			// killing), a? + 1@57 (the block's tail, Using the Ok value and
			// defining the block's result), let r: … = try { … };@30 (Uses
			// the block's result), r.unwrap_or(0)@67. Succ: a?→{a? + 1, let} (its break lands
			// after the block); a? + 1→let→r.unwrap_or(0). IPDom: a? → let.
			name:     "a question mark inside a try block leaves the block, not the function",
			protects: "`?` in a try block breaks to the block's end, so what follows the block does not depend on it",
			mutation: "send `?` to Exit even inside a try block (a?@57 then also controls the let and r.unwrap_or(0)@67), or give a `?` completing a try block no definition of the block's result (loses a?@57 -> let r: Option<i32> = try { a? + 1 };@30)",
			src:      "fn f(a: Option<i32>) -> i32 { let r: Option<i32> = try { a? + 1 }; r.unwrap_or(0) }",
			cd:       []string{"a?@57 -> a? + 1@57"},
			du: []string{"a@5 -> a?@57", "a?@57 -> a? + 1@57", "a?@57 -> let r: Option<i32> = try { a? + 1 };@30",
				"a? + 1@57 -> let r: Option<i32> = try { a? + 1 };@30",
				"let r: Option<i32> = try { a? + 1 };@30 -> r.unwrap_or(0)@67"},
		},
		{
			// Expressions › `if` expressions › Chains of
			// conditions: the operands of a let chain are evaluated in
			// order, each only when the ones before it hold, and a let
			// operand's bindings are in scope for the later operands and the
			// consequence. Nodes: o@5, k@21, let Some(x) = o@41 (Branch),
			// x@50 (the binding, Using the owned variable the let member
			// defines), x > k@60 (Branch), x@68, k@79.
			// Succ: let→{x@50, k@79}; x@50→x > k→{x@68, k@79}. IPDom: let →
			// EXIT; x@50 → x > k → EXIT.
			name:     "each member of a let chain is its own branch and every false edge reaches the else",
			protects: "a later chain member runs only when the let member matched, and reads the binding the let made",
			mutation: "lower the chain as one Branch spanning it (x@50 and x > k@60 vanish as nodes, and x@68 pairs with the chain node)",
			src:      "fn f(o: Option<i32>, k: i32) -> i32 { if let Some(x) = o && x > k { x } else { k } }",
			cd: []string{"let Some(x) = o@41 -> x@50", "let Some(x) = o@41 -> x > k@60", "let Some(x) = o@41 -> k@79",
				"x > k@60 -> x@68", "x > k@60 -> k@79"},
			du: []string{"o@5 -> let Some(x) = o@41", "let Some(x) = o@41 -> x@50", "k@21 -> x > k@60", "x@50 -> x > k@60",
				"x@50 -> x@68", "k@21 -> k@79"},
		},
		{
			// Expressions › Operator expressions › Lazy boolean operators:
			// `a && b` evaluates b only when a is true, `a || b` only when a
			// is false; `&&` binds tighter, so the condition is (a && b) ||
			// c. Nodes: a@5, b@14, c@23, a@44 (Branch), b@49, a && b@44
			// (Branch), c@54, a && b || c@44 (Branch), 1@58, 0@69. a@44 and
			// b@49 define the result of `a && b`, which the Branch a && b@44
			// Uses; that Branch and c@54 define the result of the `||`, which
			// the condition Uses. Succ:
			// a@44→{b@49, a && b}; b@49→a && b→{c@54, whole}; c@54→whole→{1,
			// 0}. IPDom: a@44 → a && b → whole → EXIT.
			name:     "the right operand of a lazy boolean operator depends on the left",
			protects: "the short-circuit edges of `&&` and `||` make each right operand control dependent on its left operand",
			mutation: "lower `&&` and `||` as plain binary operators (a@44, b@49, a && b@44 and c@54 vanish, and only the whole condition branches), or let a node spanning an operator re-read its operands' names (gains a@5 and b@14 -> a && b@44 and a && b || c@44, and c@23 -> a && b || c@44)",
			src:      "fn f(a: bool, b: bool, c: bool) -> i32 { if a && b || c { 1 } else { 0 } }",
			cd: []string{"a@44 -> b@49", "a && b@44 -> c@54", "a && b || c@44 -> 1@58",
				"a && b || c@44 -> 0@69"},
			du: []string{"a@5 -> a@44", "b@14 -> b@49", "a@44 -> a && b@44", "b@49 -> a && b@44",
				"a && b@44 -> a && b || c@44", "c@23 -> c@54", "c@54 -> a && b || c@44"},
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
			// Patterns › Wildcard pattern, and Expressions › `match`
			// expressions: `_` matches any value, so the match cannot fall
			// out and a later arm is unreachable. Nodes: o@5, o@36 (the
			// scrutinee, defining the owned variable Some(x) and x@45 Use), Some(x)@40 (Branch), x@45, x@51, 0@59; the None
			// arm lowers to nothing. Succ: Some(x)→{x@45, 0}; x@45→x@51.
			// IPDom: Some(x) → EXIT.
			name:     "a wildcard arm leaves no false edge and ends the arms",
			protects: "after `_` no arm is reachable and the match has no edge past its arms",
			mutation: "treat `_` as refutable, giving it a Branch and a false edge to the next arm (a `_` Branch then controls 0@59, and the None arm is lowered)",
			src:      "fn f(o: Option<i32>) -> i32 { match o { Some(x) => x, _ => 0, None => 1 } }",
			cd:       []string{"Some(x)@40 -> x@45", "Some(x)@40 -> x@51", "Some(x)@40 -> 0@59"},
			du:       []string{"o@5 -> o@36", "o@36 -> Some(x)@40", "o@36 -> x@45", "x@45 -> x@51"},
		},
		{
			// Expressions › Assignment expressions › Destructuring
			// assignments: a tuple struct `P(a)` and a struct `S { x, y }`
			// on the left are assignee expressions, and each assignee
			// inside is assigned. Nodes: p@48, s@54, let mut a = 0;@69 (its
			// definition is killed before any read), P(a) = p@98 (defines an
			// owned variable holding p's value, which a@100 Uses), a@100,
			// S { x, y } = s@108 (likewise for x@112 and y@115), x@112, y@115, a + x + y@124 (the let x; and
			// let y; declarations make no node). Straight line.
			name:     "tuple-struct and struct assignees define every field they name",
			protects: "`P(a) = p` and `S { x, y } = s` kill the earlier definitions of a, x and y, and read nothing on the left",
			mutation: "lower a call or struct expression on the left as a place (P(a) = p@98 then reads a and defines nothing, so let mut a = 0;@69 reaches a + x + y@124)",
			src:      "struct P(i32); struct S { x: i32, y: i32 } fn f(p: P, s: S) -> i32 { let mut a = 0; let x; let y; P(a) = p; S { x, y } = s; a + x + y }",
			du: []string{"p@48 -> P(a) = p@98", "P(a) = p@98 -> a@100", "s@54 -> S { x, y } = s@108",
				"S { x, y } = s@108 -> x@112", "S { x, y } = s@108 -> y@115", "a@100 -> a + x + y@124", "x@112 -> a + x + y@124", "y@115 -> a + x + y@124"},
		},
		{
			// Expressions › Block expressions › `async` blocks: an async block
			// captures what it uses and runs only when its future is polled,
			// so it is its own function; a gen block is lowered the same way.
			// Nodes: n@9, async { n += 1; }@36 (Uses n, may-defines n), let
			// fut = …;@26 (Uses the creating node's result, the future),
			// drop(fut)@55, n@66.
			name:     "an async block is its own function and its write is a may-definition where it is created",
			protects: "the async block's body is not inline code of the enclosing function: its write kills nothing there",
			mutation: "lower an async block's body inline as a plain block (n += 1@44 becomes a killing definition, so n@9 -> n@66 is lost), or let the let re-read the captures instead of the creating node's result (async { n += 1; }@36 -> let fut becomes n@9 -> let fut)",
			src:      "fn f(mut n: i32) -> i32 { let fut = async { n += 1; }; drop(fut); n }",
			du: []string{"n@9 -> async { n += 1; }@36", "async { n += 1; }@36 -> let fut = async { n += 1; };@26",
				"let fut = async { n += 1; };@26 -> drop(fut)@55", "n@9 -> n@66", "async { n += 1; }@36 -> n@66"},
		},
		{
			// Expressions › Closure expressions: a closure's parameters are
			// patterns binding in its body, so a parameter x shadows the
			// enclosing x there. Nodes: x@5, y@13, |x: i32| x + y@38 (Uses
			// y only), let g = …;@30 (Uses the creating node's result),
			// g(1) + x@54.
			name:     "a closure parameter shadows the enclosing variable of its name",
			protects: "a closure's use of its own parameter is not a capture of the enclosing variable",
			mutation: "resolve a closure's parameter names through the enclosing scope (x@5 -> |x: i32| x + y@38 appears)",
			src:      "fn f(x: i32, y: i32) -> i32 { let g = |x: i32| x + y; g(1) + x }",
			du: []string{"y@13 -> |x: i32| x + y@38", "|x: i32| x + y@38 -> let g = |x: i32| x + y;@30",
				"let g = |x: i32| x + y;@30 -> g(1) + x@54", "x@5 -> g(1) + x@54"},
		},
		{
			// Types › Closure types › Capture modes, and Expressions ›
			// Closure expressions: a `move`
			// closure captures by value, so `n += 1` writes the closure's
			// copy of n, never the enclosing n. Nodes: n@9, move || n +=
			// 1@38 (Uses n, may-defines nothing), let mut g = …;@26 (Uses
			// the creating node's result), g()@54, n@59.
			name:     "a move closure's write is not a definition of the enclosing variable",
			protects: "only the definitions before a move closure reach a later read of a variable it writes",
			mutation: "keep a move closure's writes as may-definitions (gains move || n += 1@38 -> n@59)",
			src:      "fn f(mut n: i32) -> i32 { let mut g = move || n += 1; g(); n }",
			du: []string{"n@9 -> move || n += 1@38", "move || n += 1@38 -> let mut g = move || n += 1;@26",
				"let mut g = move || n += 1;@26 -> g()@54", "n@9 -> n@59"},
		},
		{
			// Statements › Expression statements, and Expressions › `if`
			// expressions: `s = 1` inside the if's block is a statement
			// of its own, not the block's value, and reads nothing. The arm
			// tails 2 and 3 define the if's result, which the let Uses; the
			// condition's read of s is the condition node's own. Nodes: s@9,
			// s > 0@37 (Branch), s = 1@45, 2@52, 3@63, let t = …;@26 (Uses
			// the if's result), t + s@68. Succ: s >
			// 0→{s = 1, 3}; s = 1→2→let; 3→let. IPDom: s > 0 → let.
			name:     "a statement inside a valued expression reads only its own operands",
			protects: "the let consuming a valued if takes its value from the arm tails through the if's result, never the condition's read, so no false pair joins the assignment s = 1 to the let",
			mutation: "give the arm tails no definition of the if's result (loses 2@52 -> let t and 3@63 -> let t), or let the let re-read the condition's s (gains s@9 -> let t and the false pair s = 1@45 -> let t, although the let takes 2 or 3, never s)",
			src:      "fn f(mut s: i32) -> i32 { let t = if s > 0 { s = 1; 2 } else { 3 }; t + s }",
			cd:       []string{"s > 0@37 -> s = 1@45", "s > 0@37 -> 2@52", "s > 0@37 -> 3@63"},
			du: []string{"s@9 -> s > 0@37", "2@52 -> let t = if s > 0 { s = 1; 2 } else { 3 };@26",
				"3@63 -> let t = if s > 0 { s = 1; 2 } else { 3 };@26", "s@9 -> t + s@68", "s = 1@45 -> t + s@68",
				"let t = if s > 0 { s = 1; 2 } else { 3 };@26 -> t + s@68"},
		},
		{
			// Patterns › Identifier patterns: `ref mut v` binds v to a mutable
			// reference into the matched place o, through which `*v += 1`
			// writes o. Nodes: o@9, let Some(ref mut v) = o@45 (Branch,
			// defining an owned variable holding o's value), v@62 (Uses it,
			// defines v, may-defines o, so it pairs with o@9, the definition
			// of o reaching it, although it reads only the owned variable),
			// *v += 1@71 (may-defines v, the variable it writes through),
			// o@82. Succ: let→{v, o@82}; v→*v += 1→o@82. IPDom: let → o@82.
			name:     "a ref mut binding is a may-definition of the matched variable",
			protects: "a write through a `ref mut` binding reaches later reads of the matched variable",
			mutation: "record no may-definition for a ref mut binding (loses v@62 -> o@82 and o@9 -> v@62)",
			src:      "fn f(mut o: Option<i32>) -> Option<i32> { if let Some(ref mut v) = o { *v += 1; } o }",
			cd:       []string{"let Some(ref mut v) = o@45 -> v@62", "let Some(ref mut v) = o@45 -> *v += 1@71"},
			du: []string{"o@9 -> let Some(ref mut v) = o@45", "let Some(ref mut v) = o@45 -> v@62", "o@9 -> o@82", "v@62 -> *v += 1@71",
				"v@62 -> o@82", "o@9 -> v@62"},
		},
		{
			// Expressions › `match` expressions, and › Match guards: the
			// match's value is the value of the arm that matched. Nodes: o@5,
			// k@21, o@52 (the scrutinee, defining an owned variable),
			// Some(x)@56 (Branch, Using it), x@61 (the binding, Using it),
			// x > k@67 (the guard, a Branch Using x and k), x@76 (the arm's
			// result, Using the arm's x and defining the match's result),
			// k@84 (the `_` arm's result, defining it too; `_` makes no
			// node), let r = …;@38 (Uses the match's result), r@89. Succ:
			// o@52→Some(x)→{x@61, k@84}; x@61→x > k→{x@76, k@84};
			// x@76→let r; k@84→let r; let r→r. IPDom: Some(x) → let r; x@61
			// → x > k → let r; x@76 → let r; k@84 → let r. Frontier walks:
			// Some(x) over x@61, x > k and k@84; x > k over x@76 and k@84.
			name:     "a consumed match hands each arm's result to its consumer through one result variable",
			protects: "the let consuming a match takes its value from each arm's result node, whose reads resolve in the arm's scope, and never re-reads the scrutinee's or the arms' names",
			mutation: "give an arm's result node no definition of the match's result (loses x@76 -> let r and k@84 -> let r), or let the let re-read the names inside the match (gains o@5, x@61 and k@21 -> let r)",
			src:      "fn f(o: Option<i32>, k: i32) -> i32 { let r = match o { Some(x) if x > k => x, _ => k }; r }",
			cd: []string{"Some(x)@56 -> x@61", "Some(x)@56 -> x > k@67", "Some(x)@56 -> k@84", "x > k@67 -> x@76",
				"x > k@67 -> k@84"},
			du: []string{"o@5 -> o@52", "o@52 -> Some(x)@56", "o@52 -> x@61", "x@61 -> x > k@67", "k@21 -> x > k@67",
				"x@61 -> x@76", "k@21 -> k@84",
				"x@76 -> let r = match o { Some(x) if x > k => x, _ => k };@38",
				"k@84 -> let r = match o { Some(x) if x > k => x, _ => k };@38",
				"let r = match o { Some(x) if x > k => x, _ => k };@38 -> r@89"},
		},
		{
			// Expressions › Assignment expressions › Destructuring
			// assignments: the right operand is evaluated once, then each
			// assignee is assigned its part. Nodes: a@9, b@21, (a, b) = (b,
			// a)@38 (Uses b and a, defines an owned variable holding the
			// tuple), a@39 and b@42 (each Uses that variable and defines its
			// name), a - b@55. Straight line.
			name:     "a destructuring assignment's targets take their parts from the value evaluated once",
			protects: "each target of `(a, b) = (b, a)` Uses the assignment's value, never the names the assignment read, so a swap pairs the parameters only with the assignment",
			mutation: "let a target defining a name its statement read also Use that name (gains a@9 -> a@39 and b@21 -> b@42), or give the targets no use of the assignment's value (loses (a, b) = (b, a)@38 -> a@39 and -> b@42)",
			src:      "fn f(mut a: i32, mut b: i32) -> i32 { (a, b) = (b, a); a - b }",
			du: []string{"a@9 -> (a, b) = (b, a)@38", "b@21 -> (a, b) = (b, a)@38", "(a, b) = (b, a)@38 -> a@39",
				"(a, b) = (b, a)@38 -> b@42", "a@39 -> a - b@55", "b@42 -> a - b@55"},
		},
		{
			// Expressions › Operator expressions › Borrow operators, and
			// Expressions › Place expressions and value expressions: `*p` is
			// a place, so `&mut *p` may write through p, but `!b` is a value, so `&mut !b` borrows a temporary and never
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
			// The try propagation expression: `g(s)?` returns from the function
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
			// and › Operator expressions › Lazy boolean operators (`c || return 0` returns when c is
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
			// Expressions › Operator expressions › The try propagation
			// expression (it returns from the enclosing function or closure),
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
			// Expressions › Loops and other breakable expressions › `break`
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
			// Macros › Macro invocation, with format strings by the std::fmt
			// library documentation (The Rust Reference does not define
			// them): Named parameters (an implicit `{x}` captures
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
			// Expressions › Field access expressions and › Method-call
			// expressions (the
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
		{
			// Items › Static items, and Statements › Declaration statements ›
			// Item declarations: G is a static item of the block, no local of
			// f, so predeclare binds it to no variable; Macros By Example ›
			// Hygiene: `make!(m)` may declare m at the invocation's site, but
			// the expansion is not lowered, so m resolves to nothing; H, J, P,
			// g and k are named by no declaration in scope, each used as one
			// type allows: H an integer place (borrowed, captured by the
			// format string and assigned by the closure), J an iterable, P a
			// pair whose first element a `ref mut` binding matches. G is
			// written only as a place (a reference to a `static mut` is
			// refused by the 2024 edition's static_mut_refs lint). Nodes: v@5, make!(m)@66,
			// G = (v, 0)@83 (Uses v, defines nothing), m += v@95 (Uses v
			// only), m = v@112 (the embedded assignment, Uses v), let e = (m =
			// v);@103 (defines e, Uses nothing), (m, G.1, w) = (v, 1, 2)@120
			// (Uses v, defines an owned variable), m@121, G.1@124 and w@129
			// (each Uses it; only w@129 defines, and G.1 may-defines nothing),
			// G.0 = v@145 (Uses v), g(&mut H)@154 and the println!@165
			// (a token-tree read of m, a `&mut m` borrow and a `{H:?}`
			// capture, each of `{H:?} {:?}`'s arguments used: nothing),
			// || { m += v; H = 1; }@205 (Uses the captured v, may-defines
			// nothing), let c = …;@197, c()@228, J@242 (the iterated value,
			// defining the iteration variable), for@233, i@237, let (ref mut
			// y, z) = P;@247 (Uses nothing, defines an owned variable), y@260
			// (a `ref mut` binding of an unresolved place: no
			// may-definition), z@263, k(e, w, y, z)@271. Succ: straight line
			// to for@233→{i@237, let (ref mut y, z) = P;@247}; i@237→for@233.
			// IPDom: for@233 → let (ref mut y, z) = P;@247.
			name:     "a name that resolves to no local defines, may-defines and uses nothing in every position",
			protects: "a static item, a name a macro declares and an undeclared name, as an assignment, compound, embedded or destructuring target, a place's base, a borrowed or ref-mut-matched place, a captured read or write, a token-tree read, borrow or format capture, and an iterated value, never reach flow.Builder as -1 and pair with nothing",
			mutation: "remove any one -1 filter (read's or def's; the baseVar >= 0 check of assign, targets or compound; value's or tokenBorrow's borrow check; bindName's matched check; capWrite's) and flow.Builder panics on G, m, H, J or P; or let predeclare bind an item's name to a variable (gains G = (v, 0)@83 -> G.1@124 and -> G.0 = v@145, among others)",
			src:      "fn f(v: i32) -> i32 { static mut G: (i32, i32) = (0, 0); unsafe { make!(m); let w; G = (v, 0); m += v; let e = (m = v); (m, G.1, w) = (v, 1, 2); G.0 = v; g(&mut H); println!(\"{H:?} {:?}\", &mut m); let c = || { m += v; H = 1; }; c(); for i in J {} let (ref mut y, z) = P; k(e, w, y, z) } }",
			cd:       []string{"for@233 -> i@237", "for@233 -> for@233"},
			du: []string{"v@5 -> G = (v, 0)@83", "v@5 -> m += v@95", "v@5 -> m = v@112",
				"v@5 -> (m, G.1, w) = (v, 1, 2)@120", "v@5 -> G.0 = v@145", "v@5 -> || { m += v; H = 1; }@205",
				"(m, G.1, w) = (v, 1, 2)@120 -> m@121", "(m, G.1, w) = (v, 1, 2)@120 -> G.1@124",
				"(m, G.1, w) = (v, 1, 2)@120 -> w@129",
				"|| { m += v; H = 1; }@205 -> let c = || { m += v; H = 1; };@197",
				"let c = || { m += v; H = 1; };@197 -> c()@228", "J@242 -> for@233", "J@242 -> i@237",
				"let (ref mut y, z) = P;@247 -> y@260", "let (ref mut y, z) = P;@247 -> z@263",
				"let e = (m = v);@103 -> k(e, w, y, z)@271", "w@129 -> k(e, w, y, z)@271",
				"y@260 -> k(e, w, y, z)@271", "z@263 -> k(e, w, y, z)@271"},
		},
		{
			// The Rust Unstable Book, Language features › try_blocks (valid
			// on nightly behind `#![feature(try_blocks)]`), and
			// Expressions › Operator expressions › The try propagation
			// expression: the operands of `a? + b?` are evaluated left to
			// right, and whichever `?` meets an error first completes the
			// block with it. Nodes: a@5, b@21, a?@73 (Branch Using a; defines
			// its Ok value and the block's result), b?@78 (Branch Using b;
			// likewise, killing a?'s definition of the result on the path
			// through it), a? + b?@73 (the tail, Using both Ok values and
			// defining the block's result), let r: … = try { … };@46 (Uses
			// the block's result), r.unwrap_or(0)@84. Succ: a?→{b?, let};
			// b?→{a? + b?, let}; a? + b?→let→r.unwrap_or(0). IPDom: a? →
			// let; b? → let. Frontier walks: a? over b? (the walk from b?
			// reaches a?'s post-dominator let at once, and a? + b? does not
			// post-dominate b?, which can leave straight to let); b? over
			// a? + b?.
			name:     "each question mark completing a try block kills the block's result, reading nothing of it",
			protects: "b?'s definition of the try block's result is its own value, never a consumer of a?'s, and the let pairs with each `?` whose break reaches it",
			mutation: "record a `?`'s definition of the try block's result, beside its Ok value, as a may-definition (gains a?@73 -> b?@78, the claim that b? consumed a?'s value), or give a `?` completing a try block no definition of it (loses b?@78 -> let r: Option<i32> = try { a? + b? };@46)",
			src:      "fn f(a: Option<i32>, b: Option<i32>) -> i32 { let r: Option<i32> = try { a? + b? }; r.unwrap_or(0) }",
			cd:       []string{"a?@73 -> b?@78", "b?@78 -> a? + b?@73"},
			du: []string{"a@5 -> a?@73", "b@21 -> b?@78", "a?@73 -> a? + b?@73", "b?@78 -> a? + b?@73",
				"a?@73 -> let r: Option<i32> = try { a? + b? };@46", "b?@78 -> let r: Option<i32> = try { a? + b? };@46",
				"a? + b?@73 -> let r: Option<i32> = try { a? + b? };@46",
				"let r: Option<i32> = try { a? + b? };@46 -> r.unwrap_or(0)@84"},
		},
		{
			// Expressions › `if` expressions (the condition is any
			// expression, a parenthesized one included), and Expressions ›
			// Grouped expressions: `(c)` is c. Nodes: c@5 (param), c@27 (the
			// condition's Branch, spanning c without its parentheses), 1@32,
			// 2@43. Succ: c@27→{1, 2}; 1→EXIT; 2→EXIT. IPDom: c@27 → EXIT.
			name:     "a parenthesized condition's branch spans the condition without its parentheses",
			protects: "an if, while or guard condition is spanned with every enclosing pair of parentheses stripped, as every lowering spans it",
			mutation: "span the condition as written (the Branch renders (c)@26: every pair naming c@27 becomes one naming (c)@26)",
			src:      "fn f(c: bool) -> i32 { if (c) { 1 } else { 2 } }",
			cd:       []string{"c@27 -> 1@32", "c@27 -> 2@43"},
			du:       []string{"c@5 -> c@27"},
		},
		{
			// Expressions › Evaluation order of operands: `x + { … }` reads
			// x before the block runs, and Expressions › Block expressions:
			// the block's statements run whenever the let does. Nodes: x@9
			// (param), x = 1@40 (takes the let's held read: Uses x, defines
			// x and an owned variable holding the earlier x), x = 2@47
			// (defines x; the read is no longer x's, so it takes nothing),
			// 0@54 (the tail, defining the block's result), let y = …;@26
			// (Uses the handed-on variable and the block's result, defines
			// y), y + x@59. Straight line.
			name:     "a read before a nested assignment pairs with the definition that reached it",
			protects: "the let's read of x, made before the block's assignments, pairs with the parameter through the first assignment's hand-off, never with a later assignment",
			mutation: "make no hand-off (the let's read of x stays on it and pairs with x = 2@47: loses x@9 -> x = 1@40 and x = 1@40 -> let y = x + { x = 1; x = 2; 0 };@26, gains x = 2@47 -> let y = x + { x = 1; x = 2; 0 };@26), or hand the read to every later assignment (gains x = 1@40 -> x = 2@47)",
			src:      "fn f(mut x: i32) -> i32 { let y = x + { x = 1; x = 2; 0 }; y + x }",
			du: []string{"x@9 -> x = 1@40", "x = 1@40 -> let y = x + { x = 1; x = 2; 0 };@26",
				"0@54 -> let y = x + { x = 1; x = 2; 0 };@26", "let y = x + { x = 1; x = 2; 0 };@26 -> y + x@59",
				"x = 2@47 -> y + x@59"},
		},
		{
			// Expressions › Evaluation order of operands, and Expressions ›
			// `if` expressions: the consequence runs only when c holds, so
			// the assignment in it does not run whenever the let does and
			// takes none of its reads (lower.go, the hand-off is made only
			// when the assignment's node runs whenever the consumer does).
			// Nodes: c@5, x@18, c@50 (Branch), x = 1@54, 0@61 and 0@72 (the
			// arm tails, defining the if's result), let y = …;@35 (Uses x and
			// the if's result), y + x@77. Succ: c@50→{x = 1, 0@72}; x = 1→0@61
			// →let; 0@72→let; let→y + x. IPDom: c@50 → let.
			name:     "an assignment in a conditional arm leaves an earlier read on its consumer",
			protects: "a read held for the let stays on it when the redefining assignment sits in an if arm, so the let pairs with the parameter on the path that skips the assignment",
			mutation: "hand off reads to an assignment in an if arm (the let then Uses a variable only the consequence defines: gains x@18 -> x = 1@54, loses x@18 -> let y = x + if c { x = 1; 0 } else { 0 };@35)",
			src:      "fn f(c: bool, mut x: i32) -> i32 { let y = x + if c { x = 1; 0 } else { 0 }; y + x }",
			cd:       []string{"c@50 -> x = 1@54", "c@50 -> 0@61", "c@50 -> 0@72"},
			du: []string{"c@5 -> c@50", "x@18 -> let y = x + if c { x = 1; 0 } else { 0 };@35",
				"x = 1@54 -> let y = x + if c { x = 1; 0 } else { 0 };@35",
				"0@61 -> let y = x + if c { x = 1; 0 } else { 0 };@35", "0@72 -> let y = x + if c { x = 1; 0 } else { 0 };@35",
				"let y = x + if c { x = 1; 0 } else { 0 };@35 -> y + x@77", "x@18 -> y + x@77", "x = 1@54 -> y + x@77"},
		},
		{
			// Expressions › Grouped expressions: `(x)` is x, and lower.go's
			// Spans: a node spanning an operand or tail strips its
			// parentheses. Nodes: x@5 (param), x@23 (the tail, spanning x).
			name:     "a parenthesized tail spans its expression without the parentheses",
			protects: "a folded statement or tail given as a parenthesized expression renders and spans as the expression inside",
			mutation: "drop the strip in into (the tail renders (x)@22: loses x@5 -> x@23, gains x@5 -> (x)@22)",
			src:      "fn f(x: i32) -> i32 { (x) }",
			du:       []string{"x@5 -> x@23"},
		},
		{
			// Statements › Let statements: `let x: i32;` declares x and
			// assigns nothing, so it runs no code. Nodes: c@5, c@19 (Branch). The
			// consequence makes no node, so both of c@19's edges reach EXIT.
			name:     "a let without an initializer makes no node",
			protects: "a declaration with no initializer is no statement node, so nothing depends on a condition through it",
			mutation: "make a node for `let x: i32;` (gains c@19 -> let x: i32;@23)",
			src:      "fn f(c: bool) { if c { let x: i32; } }",
			du:       []string{"c@5 -> c@19"},
		},
		{
			// Items › Functions › Function parameters: a parameter's pattern
			// is irrefutable and binds every name in it. Nodes: a@6, b@9 (one
			// defining node per bound name), a - b@34.
			name:     "a destructuring parameter binds each name at a node of its own",
			protects: "every name a tuple parameter pattern binds is defined after Entry and reaches the body's reads",
			mutation: "bind only a bare identifier parameter (loses a@6 -> a - b@34 and b@9 -> a - b@34)",
			src:      "fn f((a, b): (i32, i32)) -> i32 { a - b }",
			du:       []string{"a@6 -> a - b@34", "b@9 -> a - b@34"},
		},
		{
			// Expressions › Closure expressions: an untyped closure parameter
			// is a bare pattern, and binds (and shadows) in the closure body.
			// Nodes: x@5, y@13, |x| x + y@38 (Uses y only), let g = …;@30
			// (Uses the creating node's result), g(1) + x@49.
			name:     "an untyped closure parameter shadows the enclosing variable of its name",
			protects: "a closure parameter written without a type is still a binding, so its reads are no capture of the enclosing variable",
			mutation: "bind closure parameters only through a typed parameter node (gains x@5 -> |x| x + y@38)",
			src:      "fn f(x: i32, y: i32) -> i32 { let g = |x| x + y; g(1) + x }",
			du: []string{"y@13 -> |x| x + y@38", "|x| x + y@38 -> let g = |x| x + y;@30",
				"let g = |x| x + y;@30 -> g(1) + x@49", "x@5 -> g(1) + x@49"},
		},
		{
			// Expressions › Loops and other breakable expressions ›
			// `continue` expressions: an unlabelled continue ends the
			// current iteration of the innermost loop. Nodes: v@5, let mut n
			// = 0;@25, v@49 (the iterated value), for@40 (head), x@44, *x <
			// 0@56 (Branch), continue@65, n += x@77, n@87. Succ:
			// let→v@49→for@40→{x@44, n@87}; x@44→*x < 0→{continue, n += x};
			// continue→for@40; n += x→for@40. IPDom: for@40 → n@87; x@44 →
			// *x < 0 → for@40; continue → for@40; n += x → for@40. Frontier
			// walks: for@40 over x@44, *x < 0 and itself; *x < 0 over
			// continue and n += x.
			name:     "an unlabelled continue re-enters the innermost loop's head",
			protects: "a continue without a label targets the innermost loop, so its head controls the rest of the body",
			mutation: "resolve an unlabelled continue to no open frame (unresolved becomes 1 and continue@65 reaches EXIT: n += x@77 no longer carries control back to for@40)",
			src:      "fn f(v: &[i32]) -> i32 { let mut n = 0; for x in v { if *x < 0 { continue; } n += x; } n }",
			cd: []string{"for@40 -> x@44", "for@40 -> *x < 0@56", "for@40 -> for@40",
				"*x < 0@56 -> continue@65", "*x < 0@56 -> n += x@77"},
			du: []string{"v@5 -> v@49", "v@49 -> for@40", "v@49 -> x@44", "x@44 -> *x < 0@56", "x@44 -> n += x@77",
				"let mut n = 0;@25 -> n += x@77", "n += x@77 -> n += x@77", "let mut n = 0;@25 -> n@87", "n += x@77 -> n@87"},
		},
		{
			// Macros › Macro invocation: the expansion is not lowered, so a
			// panicking macro is a plain node, as a call to a diverging
			// function is. Nodes: c@5, x@14, c@34 (Branch), panic!("no")@38,
			// x@54. Succ: c@34→{panic!, x@54}; panic!→x@54. IPDom: c@34 → x@54.
			name:     "a panicking macro is a plain node that falls through",
			protects: "a statement after an if whose arm ends in panic! is not control dependent on the if's condition",
			mutation: "lower a panicking macro as a jump to Exit (gains c@34 -> x@54)",
			src:      "fn f(c: bool, x: i32) -> i32 { if c { panic!(\"no\"); } x }",
			cd:       []string{"c@34 -> panic!(\"no\")@38"},
			du:       []string{"c@5 -> c@34", "x@14 -> x@54"},
		},
		{
			// Expressions › `if` expressions: `else if` chains a second if as
			// the else block, whose value is the outer if's. Nodes: a@5,
			// b@14, a@43 (Branch), 1@47, b@59 (Branch), 2@63, 3@74, let y =
			// …;@32 (Uses the if's result), y@79. Succ: a@43→{1, b@59};
			// b@59→{2, 3}; 1, 2, 3→let. IPDom: a@43 → let; b@59 → let.
			name:     "an else-if chain's arms each define the outer if's result",
			protects: "every arm tail of an else-if chain hands its value to the let consuming the whole chain",
			mutation: "lower the else-if with no result of its own consumer (loses 2@63 -> let y and 3@74 -> let y)",
			src:      "fn f(a: bool, b: bool) -> i32 { let y = if a { 1 } else if b { 2 } else { 3 }; y }",
			cd:       []string{"a@43 -> 1@47", "a@43 -> b@59", "b@59 -> 2@63", "b@59 -> 3@74"},
			du: []string{"a@5 -> a@43", "b@14 -> b@59",
				"1@47 -> let y = if a { 1 } else if b { 2 } else { 3 };@32",
				"2@63 -> let y = if a { 1 } else if b { 2 } else { 3 };@32",
				"3@74 -> let y = if a { 1 } else if b { 2 } else { 3 };@32",
				"let y = if a { 1 } else if b { 2 } else { 3 };@32 -> y@79"},
		},
		{
			// Expressions › Operator expressions › Lazy boolean operators:
			// `a || b` evaluates b only when a is false, and its value is the
			// deciding operand's. Nodes: a@5, b@14, a@41 (Branch, defining
			// the result), b@46 (defining it again), let y = a || b;@33 (Uses
			// the result), y@49. Succ: a@41→{b@46, let}; b@46→let.
			name:     "a lazy operator consumed by a let hands each operand's value through its result",
			protects: "the let consuming `a || b` reads the operator's result, defined by the left operand on its deciding edge and by the right operand",
			mutation: "fold a consumed lazy operator into its consumer (a@41 and b@46 vanish and a@5 and b@14 pair with the let), or let the let re-read the operands (gains a@5 and b@14 -> let y = a || b;@33)",
			src:      "fn f(a: bool, b: bool) -> bool { let y = a || b; y }",
			cd:       []string{"a@41 -> b@46"},
			du: []string{"a@5 -> a@41", "b@14 -> b@46", "a@41 -> let y = a || b;@33", "b@46 -> let y = a || b;@33",
				"let y = a || b;@33 -> y@49"},
		},
		{
			// Expressions › Operator expressions › Lazy boolean operators, in
			// tail position: the operand nodes are all; no node spans the
			// operator. Nodes: a@5, b@14, a@33 (Branch), b@38.
			name:     "a lazy operator in tail position makes only its operand nodes",
			protects: "a tail `a && b` makes no node spanning the operator, and its right operand depends on its left",
			mutation: "make a node spanning the operator in tail position (a && b@33 appears, gaining a@33 -> a && b@33 and b@38 -> a && b@33)",
			src:      "fn f(a: bool, b: bool) -> bool { a && b }",
			cd:       []string{"a@33 -> b@38"},
			du:       []string{"a@5 -> a@33", "b@14 -> b@38"},
		},
		{
			// Expressions › Operator expressions › Borrow operators: `&x` is a
			// shared borrow, through which the callee cannot write x (lower.go,
			// Address-taking, Given up: interior mutability). Nodes: x@5,
			// g(&x)@22 (Uses x), x@29.
			name:     "a shared borrow is a use only",
			protects: "passing `&x` to a call is no may-definition of x",
			mutation: "treat a shared borrow as mutable (gains g(&x)@22 -> x@29)",
			src:      "fn f(x: i32) -> i32 { g(&x); x }",
			du:       []string{"x@5 -> g(&x)@22", "x@5 -> x@29"},
		},
		{
			// Names › Scopes: a block is a scope, so the inner let's x ends
			// with it. Nodes: x@5, let x = 1;@24 (a new variable), g(x)@35,
			// x@43 (the parameter).
			name:     "a block's let binding ends with the block",
			protects: "a read after an inner block resolves to the variable in scope outside it, not the block's shadowing let",
			mutation: "keep a block's bindings in scope after it (loses x@5 -> x@43, gains let x = 1;@24 -> x@43)",
			src:      "fn f(x: i32) -> i32 { { let x = 1; g(x); } x }",
			du:       []string{"let x = 1;@24 -> g(x)@35", "x@5 -> x@43"},
		},
		{
			// Expressions › Loops and other breakable expressions › Loop
			// labels: a break naming a label no enclosing loop or block
			// declares is an error (E0426) but parses: ill-formed on purpose,
			// since how a lowering counts a jump no frame opens is only
			// observable on a program no compiler accepts. Nodes: loop@9 (head),
			// break 'nope@16 (Jump; it defines nothing). The break's target
			// names no frame, so it keeps no successor and augmentation sends
			// it to EXIT.
			name:       "a break naming no open label is unresolved",
			protects:   "a jump whose label names no open frame is counted unresolved rather than bound to some other loop",
			mutation:   "resolve an unknown label to the innermost loop (unresolved becomes 0)",
			src:        "fn f() { loop { break 'nope; } }",
			unresolved: 1,
			illFormed:  true,
		},
		{
			// Expressions › Loops and other breakable expressions › Loop
			// labels: `break 'o` leaves the loop labelled 'o from inside an
			// inner loop, and a valueless break defines nothing. Nodes: n@5,
			// let mut i = 0;@22, loop@41 (outer head), loop@48 (inner head),
			// i += 1@55, i > n@66 (Branch), break 'o@74, i@90. Succ:
			// let→loop@41→loop@48→i += 1→i > n→{break, loop@48}; break→i@90.
			// Neither loop has another exit. IPDom: loop@48 → i += 1 → i > n →
			// break → i@90. Frontier walk from i > n→loop@48: loop@48, i +=
			// 1, i > n.
			name:     "a labelled break leaves the outer loop from an inner one",
			protects: "`break 'o` lands after the loop its label names, not after the innermost loop, and defines no result",
			mutation: "resolve a labelled break to the innermost loop (it lands on the outer loop's back edge, i@90 has no path from ENTRY: loses i += 1@55 -> i@90)",
			src:      "fn f(n: i32) -> i32 { let mut i = 0; 'o: loop { loop { i += 1; if i > n { break 'o; } } } i }",
			cd:       []string{"i > n@66 -> loop@48", "i > n@66 -> i += 1@55", "i > n@66 -> i > n@66"},
			du: []string{"n@5 -> i > n@66", "let mut i = 0;@22 -> i += 1@55", "i += 1@55 -> i += 1@55",
				"i += 1@55 -> i > n@66", "i += 1@55 -> i@90"},
		},
		{
			// Statements › Expression statements: a trailing `if … else …`
			// without `;` is the block's tail, so its value is the block's.
			// Nodes: c@5, c@36 (Branch), 1@40, 2@51 (each defining the block's
			// result), let y = …;@23 (Uses it), y@58. Succ: c@36→{1, 2}; 1,
			// 2→let. IPDom: c@36 → let.
			name:     "a trailing if in a consumed block is the block's tail",
			protects: "the arms of a trailing if hand their values to the let consuming the enclosing block",
			mutation: "take a trailing if expression statement as a statement, not the tail (loses 1@40 -> let y and 2@51 -> let y)",
			src:      "fn f(c: bool) -> i32 { let y = { if c { 1 } else { 2 } }; y }",
			cd:       []string{"c@36 -> 1@40", "c@36 -> 2@51"},
			du: []string{"c@5 -> c@36", "1@40 -> let y = { if c { 1 } else { 2 } };@23",
				"2@51 -> let y = { if c { 1 } else { 2 } };@23", "let y = { if c { 1 } else { 2 } };@23 -> y@58"},
		},
		{
			// Expressions › Loops and other breakable expressions › Iterator
			// loops: the iterator is made once, so the body's write to n does
			// not change the iteration. Nodes: n@9, 0..n@35 (the iterated
			// value, defining the iteration variable), for@26 (head), i@30,
			// n = i@42, n@51. Succ: n@9→0..n→for→{i, n@51}; i→n = i→for.
			// IPDom: for → n@51; i → n = i → for.
			name:     "a write in a for body to an iterated name never reaches the head",
			protects: "the for head and binding read the value iterated before the loop, so `n = i` in the body pairs only with later reads of n",
			mutation: "let the head re-read the iterated expression's names (gains n@9 -> for@26 and n = i@42 -> for@26)",
			src:      "fn f(mut n: i32) -> i32 { for i in 0..n { n = i; } n }",
			cd:       []string{"for@26 -> i@30", "for@26 -> n = i@42", "for@26 -> for@26"},
			du: []string{"n@9 -> 0..n@35", "0..n@35 -> for@26", "0..n@35 -> i@30", "i@30 -> n = i@42",
				"n@9 -> n@51", "n = i@42 -> n@51"},
		},
		{
			// Expressions › Patterns › Identifier patterns: an identifier in
			// a non-last arm may be a binding or a constant, so it is a Branch
			// that also defines the name. Nodes: o@5, o@28 (scrutinee), n@32
			// (Branch, Using the scrutinee's variable and defining n), n@37,
			// 0@45. Succ: o@28→n@32→{n@37, 0}. IPDom: n@32 → EXIT.
			name:     "an earlier arm's bare identifier is a branch that binds the name",
			protects: "the arm's result reads the name its identifier pattern binds, and a later arm stays reachable on the pattern's false edge",
			mutation: "define nothing at an earlier arm's bare identifier (loses n@32 -> n@37), or take it as irrefutable (loses n@32 -> 0@45)",
			src:      "fn f(o: i32) -> i32 { match o { n => n, _ => 0 } }",
			cd:       []string{"n@32 -> n@37", "n@32 -> 0@45"},
			du:       []string{"o@5 -> o@28", "o@28 -> n@32", "n@32 -> n@37"},
		},
		{
			// Expressions › Assignment expressions › Compound assignment
			// expressions, and › Index expressions: `a[i] += 1` writes one
			// element of a, a may-definition of a. Nodes: a@9, i@22, a[i] +=
			// 1@41 (Uses a and i, may-defines a), a[0]@52.
			name:     "an index write is a may-definition of its base",
			protects: "a read of the array after `a[i] += 1` sees the write and the definition before it",
			mutation: "record no may-definition of an index target's base (loses a[i] += 1@41 -> a[0]@52), or a killing one (loses a@9 -> a[0]@52)",
			src:      "fn f(mut a: [i32; 2], i: usize) -> i32 { a[i] += 1; a[0] }",
			du: []string{"a@9 -> a[i] += 1@41", "i@22 -> a[i] += 1@41", "a@9 -> a[0]@52",
				"a[i] += 1@41 -> a[0]@52"},
		},
		{
			// Expressions › Operator expressions › The dereference operator:
			// `*p = 2` writes through p; lower.go, Address-taking: the write
			// is a may-definition of the variable written through. Nodes:
			// p@5, *p = 2@27 (Uses p, may-defines p), *p@35.
			name:     "a write through a dereference is a may-definition of the variable written through",
			protects: "a later read through p depends on the write through p",
			mutation: "record no may-definition for a dereference target (loses *p = 2@27 -> *p@35)",
			src:      "fn f(p: &mut i32) -> i32 { *p = 2; *p }",
			du:       []string{"p@5 -> *p = 2@27", "p@5 -> *p@35", "*p = 2@27 -> *p@35"},
		},
		{
			// Patterns › Or-patterns: every alternative binds the same names,
			// so the first alternative's bindings stand for them. Nodes:
			// o@5, o@41, Ok(x) | Err(x)@45 (Branch; the last arm is
			// refutable by syntax, so it keeps its false edge), x@48, x@63.
			// Succ: o@41→pattern→{x@48, EXIT}; x@48→x@63.
			name:     "an or-pattern binds its first alternative's names",
			protects: "an or-pattern's bound name is defined once, at the first alternative's name, and reaches the arm's result",
			mutation: "bind every alternative's names (x@57 becomes a second binding node: loses x@48 -> x@63, gains x@57 -> x@63)",
			src:      "fn f(o: Result<i32, i32>) -> i32 { match o { Ok(x) | Err(x) => x } }",
			cd:       []string{"Ok(x) | Err(x)@45 -> x@48", "Ok(x) | Err(x)@45 -> x@63"},
			du:       []string{"o@5 -> o@41", "o@41 -> Ok(x) | Err(x)@45", "o@41 -> x@48", "x@48 -> x@63"},
		},
		{
			// Patterns › Identifier patterns: `x @ q` binds x to the matched
			// value and q's names as q binds them. Nodes: o@5, o@36, x @
			// Some(y)@40 (Branch), x@40, y@49, g(x, y)@55, 0@69. Succ:
			// o@36→pattern→{x@40, 0}; x@40→y@49→g. IPDom: pattern → EXIT.
			name:     "a bound pattern binds its name and the names of the pattern after @",
			protects: "both x and y of `x @ Some(y)` are defined from the scrutinee's variable and read by the arm",
			mutation: "bind only the name before @ (loses y@49 and y@49 -> g(x, y)@55), or only the pattern after it (loses x@40 and x@40 -> g(x, y)@55)",
			src:      "fn f(o: Option<i32>) -> i32 { match o { x @ Some(y) => g(x, y), _ => 0 } }",
			cd: []string{"x @ Some(y)@40 -> x@40", "x @ Some(y)@40 -> y@49", "x @ Some(y)@40 -> g(x, y)@55",
				"x @ Some(y)@40 -> 0@69"},
			du: []string{"o@5 -> o@36", "o@36 -> x @ Some(y)@40", "o@36 -> x@40", "o@36 -> y@49",
				"x@40 -> g(x, y)@55", "y@49 -> g(x, y)@55"},
		},
		{
			// The Rust Unstable Book, Language features › gen_blocks (valid
			// on nightly behind `#![feature(gen_blocks)]`): a gen
			// block's body runs only as its iterator is advanced, so it is
			// its own function. Nodes: n@9, gen { n += 1; yield n; }@34 (Uses
			// n, may-defines n), let g = …;@26 (Uses the creating node's
			// result), drop(g)@60, n@69.
			name:     "a gen block is its own function and its write is a may-definition where it is created",
			protects: "a gen block's body is not inline code of the enclosing function: its write kills nothing there",
			mutation: "lower a gen block's body inline (n += 1 kills the parameter's definition: loses n@9 -> n@69)",
			src:      "fn f(mut n: i32) -> i32 { let g = gen { n += 1; yield n; }; drop(g); n }",
			du: []string{"n@9 -> gen { n += 1; yield n; }@34",
				"gen { n += 1; yield n; }@34 -> let g = gen { n += 1; yield n; };@26",
				"let g = gen { n += 1; yield n; };@26 -> drop(g)@60", "n@9 -> n@69", "gen { n += 1; yield n; }@34 -> n@69"},
		},
		{
			// Expressions › Await expressions: `a.await` is folded into the
			// node consuming it. Nodes: a@11, b@17, let y = a.await + b;@34
			// (Uses a and b), y@55.
			name:     "an await expression is folded into its consumer",
			protects: "the let consuming `a.await + b` reads a itself, with no node of the await's own",
			mutation: "read nothing under an await expression (loses a@11 -> let y = a.await + b;@34)",
			src:      "async fn f(a: F, b: i32) -> i32 { let y = a.await + b; y }",
			du: []string{"a@11 -> let y = a.await + b;@34", "b@17 -> let y = a.await + b;@34",
				"let y = a.await + b;@34 -> y@55"},
		},
		{
			// Expressions › Index, Range, Array, Struct and Type cast
			// expressions, and Paths › Expressions (a generic function path
			// names an item): all are folded into the tail. Nodes: a@5, i@18,
			// n@28, the tail@45 (Uses a, i and n; g names no local).
			name:     "index, range, array, struct, cast and generic-call operands are folded into their consumer",
			protects: "the reads inside a struct expression's fields, an index, a range, an array and a cast operand are the consuming node's own",
			mutation: "read nothing inside a struct expression's body (loses a@5 and i@18 -> the tail), or inside a range or array (loses n@28 -> the tail)",
			src:      "fn f(a: [i32; 2], i: usize, n: i32) -> i64 { g::<i32>(S { x: a[i] }, 0..n, [n]) as i64 }",
			du: []string{"a@5 -> g::<i32>(S { x: a[i] }, 0..n, [n]) as i64@45",
				"i@18 -> g::<i32>(S { x: a[i] }, 0..n, [n]) as i64@45",
				"n@28 -> g::<i32>(S { x: a[i] }, 0..n, [n]) as i64@45"},
		},
		{
			// Expressions › Block expressions, `unsafe` blocks and `const`
			// blocks: each consumed as a value yields its tail. Nodes: x@5,
			// let z = x;@32, z + 1@43 (the tail, defining the block's
			// result), let a = …;@22, a@69 (the unsafe block's tail), let b =
			// …;@52, 2@86 (the const block's tail), b + const { 2 }@74.
			// Straight line.
			name:     "a block, an unsafe block and a const block consumed as a value hand over their tails",
			protects: "each consumer of a block reads the block's result, defined by its tail node, never the names the block read",
			mutation: "give a consumed block's tail no definition of its result (loses z + 1@43 -> let a, a@69 -> let b and 2@86 -> b + const { 2 }@74), or let a consumer re-read a block's names (gains x@5 -> let a)",
			src:      "fn f(x: i32) -> i32 { let a = { let z = x; z + 1 }; let b = unsafe { a }; b + const { 2 } }",
			du: []string{"x@5 -> let z = x;@32", "let z = x;@32 -> z + 1@43",
				"z + 1@43 -> let a = { let z = x; z + 1 };@22", "let a = { let z = x; z + 1 };@22 -> a@69",
				"a@69 -> let b = unsafe { a };@52", "let b = unsafe { a };@52 -> b + const { 2 }@74",
				"2@86 -> b + const { 2 }@74"},
		},
		{
			// Macros › Macro invocation, and The try propagation expression:
			// the invocation a `?` can leave is a Branch whose value reaches
			// its consumer through a result. Nodes: o@5, vec![o?]@46 (Branch
			// Using o, defining the result), let v = vec![o?];@38 (Uses it),
			// Some(v[0])@56. Succ: vec!→{EXIT, let}; let→Some. IPDom: vec! → EXIT.
			name:     "a jumping macro's value reaches its consumer through a result",
			protects: "the let consuming a macro whose tree holds `?` reads the invocation's result, never the tree's names, and depends on the invocation",
			mutation: "let the consumer re-read the tree's reads (gains o@5 -> let v = vec![o?];@38, loses vec![o?]@46 -> let v = vec![o?];@38)",
			src:      "fn f(o: Option<i32>) -> Option<i32> { let v = vec![o?]; Some(v[0]) }",
			cd:       []string{"vec![o?]@46 -> let v = vec![o?];@38", "vec![o?]@46 -> Some(v[0])@56"},
			du: []string{"o@5 -> vec![o?]@46", "vec![o?]@46 -> let v = vec![o?];@38",
				"let v = vec![o?];@38 -> Some(v[0])@56"},
		},
		{
			// Macros › Macro invocation, and Expressions › `break` and loop
			// values: which tokens make the jumped value is known only to the
			// expansion, so a break in a tree defines no result. Nodes: x@5,
			// loop@30, m!(break x)@37 (Branch Using x; its break leaves the
			// loop, its fall-through returns to the head), let r = …;@22, r@53.
			// Succ: loop→m!→{let, loop}; let→r. IPDom: loop → m! → let.
			name:     "a jump in a macro's token tree defines no result",
			protects: "a break inside a token tree leaves the loop but hands the let no value of the loop",
			mutation: "let a jump in a token tree define its loop's result (gains m!(break x)@37 -> let r = loop { m!(break x); };@22)",
			src:      "fn f(x: i32) -> i32 { let r = loop { m!(break x); }; r }",
			cd:       []string{"m!(break x)@37 -> loop@30", "m!(break x)@37 -> m!(break x)@37"},
			du:       []string{"x@5 -> m!(break x)@37", "let r = loop { m!(break x); };@22 -> r@53"},
		},
		{
			// Expressions › `if` expressions › Chains of conditions: a let
			// chain's bindings are in scope for later members and the
			// consequence only. Nodes: o@5, x@21, let Some(x) = o@41 (Branch),
			// x@50, x > 0@60 (Branch), x@68, x@79 (the parameter). Succ:
			// let→{x@50, x@79}; x@50→x > 0→{x@68, x@79}. IPDom: let → EXIT;
			// x@50 → x > 0 → EXIT.
			name:     "a let chain's binding is not in scope in the else",
			protects: "the else of an if-let chain reads the enclosing variable, never the chain's binding of the same name",
			mutation: "keep a let chain's bindings in scope in the else (loses x@21 -> x@79, gains x@50 -> x@79)",
			src:      "fn f(o: Option<i32>, x: i32) -> i32 { if let Some(x) = o && x > 0 { x } else { x } }",
			cd: []string{"let Some(x) = o@41 -> x@50", "let Some(x) = o@41 -> x > 0@60", "let Some(x) = o@41 -> x@79",
				"x > 0@60 -> x@68", "x > 0@60 -> x@79"},
			du: []string{"o@5 -> let Some(x) = o@41", "let Some(x) = o@41 -> x@50", "x@50 -> x > 0@60", "x@50 -> x@68",
				"x@21 -> x@79"},
		},
		{
			// Expressions › Loops and other breakable expressions › Iterator
			// loops, and The try propagation expression: `o?` is the
			// iterated value, so its node is the iteration variable's
			// definition. Nodes: o@5, let mut n = 0;@43, o?@67 (Branch to
			// EXIT and on, defining the iteration variable), for@58, x@62, n
			// += x@72, Some(n)@82. Succ: let→o?→{EXIT, for}; for→{x, Some};
			// x→n += x→for. IPDom: o? → EXIT; for → Some; x → n += x → for.
			name:     "a question mark as a for iterable is the iteration variable's node",
			protects: "the for head and binding read the `?`'s Ok value, and the loop and what follows depend on the `?`",
			mutation: "let the head Use the `?`'s operand instead of its value (gains o@5 -> for@58, loses o?@67 -> for@58)",
			src:      "fn f(o: Option<Vec<i32>>) -> Option<i32> { let mut n = 0; for x in o? { n += x; } Some(n) }",
			cd: []string{"o?@67 -> for@58", "o?@67 -> Some(n)@82", "for@58 -> x@62", "for@58 -> n += x@72",
				"for@58 -> for@58"},
			du: []string{"o@5 -> o?@67", "o?@67 -> for@58", "o?@67 -> x@62", "x@62 -> n += x@72",
				"let mut n = 0;@43 -> n += x@72", "n += x@72 -> n += x@72", "let mut n = 0;@43 -> Some(n)@82",
				"n += x@72 -> Some(n)@82"},
		},
		{
			// Expressions › `match` expressions, and The try propagation
			// expression: the scrutinee `g(s)?` is evaluated once, at the `?`'s
			// Branch, whose Ok value the patterns test. Nodes: s@5, g(s)?@40,
			// 0@48 (Branch), Ok(1)@53, Ok(0)@65. Succ: g(s)?→{EXIT, 0@48};
			// 0@48→{Ok(1), Ok(0)}. IPDom: every node → EXIT.
			name:     "a question mark as the match scrutinee is the scrutinee's node",
			protects: "the arms test the `?`'s Ok value and depend on the `?`",
			mutation: "let the patterns re-read the scrutinee's names (gains s@5 -> 0@48, loses g(s)?@40 -> 0@48)",
			src:      "fn f(s: &str) -> Result<i32, E> { match g(s)? { 0 => Ok(1), _ => Ok(0) } }",
			cd:       []string{"g(s)?@40 -> 0@48", "0@48 -> Ok(1)@53", "0@48 -> Ok(0)@65"},
			du:       []string{"s@5 -> g(s)?@40", "g(s)?@40 -> 0@48"},
		},
		{
			// Expressions › `match` expressions › Match guards: a `_` arm
			// with a guard stays refutable, so a failed pattern reaches its
			// guard. Nodes: o@5, c@21, o@45, Some(x)@49 (Branch), x@54, x@60,
			// c@68 (the guard's Branch), 1@73, 0@81. Succ: Some(x)→{x@54,
			// c@68}; x@54→x@60; c@68→{1, 0}. IPDom: every Branch → EXIT.
			name:     "a failed pattern reaches a guarded wildcard arm's guard",
			protects: "the arm after `_ if c` stays reachable, and the guard, not the wildcard's result, follows a failed pattern",
			mutation: "treat a guarded `_` as irrefutable (the last arm lowers to nothing: loses c@68 -> 0@81)",
			src:      "fn f(o: Option<i32>, c: bool) -> i32 { match o { Some(x) => x, _ if c => 1, _ => 0 } }",
			cd: []string{"Some(x)@49 -> x@54", "Some(x)@49 -> x@60", "Some(x)@49 -> c@68", "c@68 -> 1@73",
				"c@68 -> 0@81"},
			du: []string{"o@5 -> o@45", "o@45 -> Some(x)@49", "o@45 -> x@54", "x@54 -> x@60", "c@21 -> c@68"},
		},
		{
			// Expressions › `break` expressions: a match is no loop, so a
			// break in an arm leaves the enclosing for. Nodes: v@5, let mut n
			// = 0;@25, v@49, for@40, x@44, *x@59 (the scrutinee), 0@64
			// (Branch), break@69, n += 1@81, n@92. Succ: for→{x, n@92};
			// x→*x→0@64→{break, n += 1}; break→n@92; n += 1→for. IPDom: for →
			// n@92; x → *x → 0@64 → n@92; break → n@92; n += 1 → for.
			// Frontier walks: for over x, *x and 0@64; 0@64 over break, and
			// over n += 1 and for.
			name:     "a break in a match arm leaves the enclosing loop",
			protects: "a match opens no jump frame, so an arm's break exits the loop around it",
			mutation: "open a jump frame for a match (break@69 lands after the match and returns to for@40: ipdom(0@64) becomes for@40, so 0@64 loses for@40 as a dependent)",
			src:      "fn f(v: &[i32]) -> i32 { let mut n = 0; for x in v { match *x { 0 => break, _ => n += 1 } } n }",
			cd: []string{"for@40 -> x@44", "for@40 -> *x@59", "for@40 -> 0@64", "0@64 -> break@69",
				"0@64 -> n += 1@81", "0@64 -> for@40"},
			du: []string{"v@5 -> v@49", "v@49 -> for@40", "v@49 -> x@44", "x@44 -> *x@59", "*x@59 -> 0@64",
				"let mut n = 0;@25 -> n += 1@81", "n += 1@81 -> n += 1@81", "let mut n = 0;@25 -> n@92",
				"n += 1@81 -> n@92"},
		},
		{
			// Patterns › Identifier patterns: a bare identifier in the last
			// arm is irrefutable by syntax, so it is a Stmt defining the name.
			// Nodes: o@5, o@36, Some(x)@40 (Branch), x@45, x@51, n@54 (Stmt,
			// Using the scrutinee's variable, defining n), g(n)@59. Succ:
			// Some(x)→{x@45, n@54}; x@45→x@51; n@54→g(n). IPDom: Some(x) → EXIT.
			name:     "a last-arm bare identifier is a statement that binds the name",
			protects: "the last arm's identifier makes no false edge out of the match and defines the name its result reads",
			mutation: "make the last arm's bare identifier a Branch (n@54 gains a false edge to EXIT and controls g(n)@59)",
			src:      "fn f(o: Option<i32>) -> i32 { match o { Some(x) => x, n => g(n) } }",
			cd: []string{"Some(x)@40 -> x@45", "Some(x)@40 -> x@51", "Some(x)@40 -> n@54",
				"Some(x)@40 -> g(n)@59"},
			du: []string{"o@5 -> o@36", "o@36 -> Some(x)@40", "o@36 -> x@45", "o@36 -> n@54", "x@45 -> x@51",
				"n@54 -> g(n)@59"},
		},
		{
			// Expressions › Assignment expressions › Destructuring
			// assignments: the elements of an array assignee and a named
			// field's value are assignees; a rest `..` and the field name are
			// not. Nodes: v@5, s@18, [a, .., b] = v@54 (defines an owned
			// variable), a@55, b@62, S { f: c } = s@70 (likewise), c@77, a + b
			// + c@86 (the `let a;` declarations make no node). Straight line.
			name:     "an array assignee's elements and a named field's value are assigned, a rest never",
			protects: "`[a, .., b] = v` defines a and b and `S { f: c } = s` defines c, each from the value evaluated once",
			mutation: "take the rest `..` as an assignee (a node ..@58 appears), or take a named field's name instead of its value (loses c@77 -> a + b + c@86)",
			src:      "fn f(v: [i32; 3], s: S) -> i32 { let a; let b; let c; [a, .., b] = v; S { f: c } = s; a + b + c }",
			du: []string{"v@5 -> [a, .., b] = v@54", "[a, .., b] = v@54 -> a@55", "[a, .., b] = v@54 -> b@62",
				"s@18 -> S { f: c } = s@70", "S { f: c } = s@70 -> c@77", "a@55 -> a + b + c@86",
				"b@62 -> a + b + c@86", "c@77 -> a + b + c@86"},
		},
		{
			// Expressions › Assignment expressions › Destructuring
			// assignments: a place assignee is assigned its part, which may
			// leave the rest of t in place. Nodes: t@9, v@24, (t.0, t.1) = (v,
			// 1)@41 (Uses v, defines an owned variable), t.0@42 and t.1@47
			// (each Uses that variable and t, may-defines t), t.0@62.
			name:     "a place target of a destructuring assignment may-defines its base",
			protects: "each field assignee Uses its own operands and is a may-definition of t, so a later read of t sees both writes' chain",
			mutation: "give a place target no may-definition of its base (loses t.0@42 -> t.1@47 and t.1@47 -> t.0@62)",
			src:      "fn f(mut t: (i32, i32), v: i32) -> i32 { (t.0, t.1) = (v, 1); t.0 }",
			du: []string{"v@24 -> (t.0, t.1) = (v, 1)@41", "(t.0, t.1) = (v, 1)@41 -> t.0@42",
				"(t.0, t.1) = (v, 1)@41 -> t.1@47", "t@9 -> t.0@42", "t@9 -> t.1@47", "t.0@42 -> t.1@47",
				"t@9 -> t.0@62", "t.1@47 -> t.0@62"},
		},
		{
			// Types › Closure types › Capture modes: a closure writing s.f and
			// a[0] captures s and a by unique borrow. Nodes: s@9, a@19, the
			// closure@53 (Uses s and a, may-defines both), let mut g = …;@41,
			// g()@81, s.f + a[0]@86.
			name:     "a callable's field and index writes are may-definitions of their bases where it is created",
			protects: "a read after a closure that writes through a field or an index sees the closure's write",
			mutation: "record no write for a callable's field or index target (loses || { s.f = 1; a[0] += 1; }@53 -> s.f + a[0]@86)",
			src:      "fn f(mut s: S, mut a: [i32; 2]) -> i32 { let mut g = || { s.f = 1; a[0] += 1; }; g(); s.f + a[0] }",
			du: []string{"s@9 -> || { s.f = 1; a[0] += 1; }@53", "a@19 -> || { s.f = 1; a[0] += 1; }@53", "|| { s.f = 1; a[0] += 1; }@53 -> let mut g = || { s.f = 1; a[0] += 1; };@41", "let mut g = || { s.f = 1; a[0] += 1; };@41 -> g()@81",
				"s@9 -> s.f + a[0]@86", "a@19 -> s.f + a[0]@86", "|| { s.f = 1; a[0] += 1; }@53 -> s.f + a[0]@86"},
		},
		{
			// Types › Closure types › Capture modes, and lower.go,
			// Address-taking: a callable's `&mut n` of an enclosing local is
			// one of its writes. Nodes: n@9, || h(&mut n)@38 (Uses n,
			// may-defines n), let mut g = …;@26, g()@52, n@57.
			name:     "a callable's mutable borrow of an enclosing local is a may-definition where it is created",
			protects: "a read after a closure that lends n mutably sees what the borrow may have written",
			mutation: "record a callable's `&mut` as a read only (loses || h(&mut n)@38 -> n@57)",
			src:      "fn f(mut n: i32) -> i32 { let mut g = || h(&mut n); g(); n }",
			du: []string{"n@9 -> || h(&mut n)@38", "|| h(&mut n)@38 -> let mut g = || h(&mut n);@26",
				"let mut g = || h(&mut n);@26 -> g()@52", "n@9 -> n@57", "|| h(&mut n)@38 -> n@57"},
		},
		{
			// Types › Closure types › Capture modes: a move closure owns its
			// copies, including the ones the closures inside it write, so
			// it mutates its own state and is called through `let mut g`.
			// Nodes: n@9, the move closure@38 (Uses n, may-defines nothing),
			// let mut g = …;@26, g()@79, n@84.
			name:     "a move closure drops the writes of the callables nested in it",
			protects: "a write by a closure nested in a move closure never reaches a read of the outer variable",
			mutation: "keep a nested callable's writes inside a move closure (gains move || { let mut h = || n += 1; h(); }@38 -> n@84)",
			src:      "fn f(mut n: i32) -> i32 { let mut g = move || { let mut h = || n += 1; h(); }; g(); n }",
			du:       []string{"n@9 -> move || { let mut h = || n += 1; h(); }@38", "move || { let mut h = || n += 1; h(); }@38 -> let mut g = move || { let mut h = || n += 1; h(); };@26", "let mut g = move || { let mut h = || n += 1; h(); };@26 -> g()@79", "n@9 -> n@84"},
		},
		{
			// Expressions › Operator expressions › Borrow operators: `&mut
			// x.0` and `&mut a[1]` borrow their base variables. Nodes: x@9,
			// a@28, g(&mut x.0, &mut a[1])@50 (Uses x and a, may-defines both),
			// x.1 + a[0]@74.
			name:     "a mutable borrow of a field or element may-defines its base",
			protects: "a read of x or a after a call receiving `&mut x.0` and `&mut a[1]` sees the call's possible writes",
			mutation: "borrow only a bare name (loses g(&mut x.0, &mut a[1])@50 -> x.1 + a[0]@74)",
			src:      "fn f(mut x: (i32, i32), mut a: [i32; 2]) -> i32 { g(&mut x.0, &mut a[1]); x.1 + a[0] }",
			du: []string{"x@9 -> g(&mut x.0, &mut a[1])@50", "a@28 -> g(&mut x.0, &mut a[1])@50",
				"x@9 -> x.1 + a[0]@74", "a@28 -> x.1 + a[0]@74", "g(&mut x.0, &mut a[1])@50 -> x.1 + a[0]@74"},
		},
		{
			// lower.go, Address-taking: a pending borrow attaches to the node
			// evaluating it, by position. Nodes: x@9, c@17, c@48 (Branch of
			// the if operand, made after the borrow but not spanning it), 1@52,
			// 2@63 (the if's result), g(&mut x, if c { 1 } else { 2 })@35
			// (Uses x and the result, may-defines x), x@69. Succ: c@48→{1, 2};
			// 1, 2→the call→x@69. IPDom: c@48 → the call.
			name:     "a pending mutable borrow attaches by position, not to the next node made",
			protects: "the call spanning `&mut x` carries its may-definition, never the if operand's Branch made before the call node",
			mutation: "attach a pending borrow to the next node made (c@48 may-defines x: gains x@9 -> c@48 and c@48 -> x@69, loses g(&mut x, if c { 1 } else { 2 })@35 -> x@69)",
			src:      "fn f(mut x: i32, c: bool) -> i32 { g(&mut x, if c { 1 } else { 2 }); x }",
			cd:       []string{"c@48 -> 1@52", "c@48 -> 2@63"},
			du: []string{"c@17 -> c@48", "x@9 -> g(&mut x, if c { 1 } else { 2 })@35",
				"1@52 -> g(&mut x, if c { 1 } else { 2 })@35", "2@63 -> g(&mut x, if c { 1 } else { 2 })@35",
				"x@9 -> x@69", "g(&mut x, if c { 1 } else { 2 })@35 -> x@69"},
		},
		{
			// Statements › Declaration statements › Item declarations: items
			// are not ordered, so a const or fn item names its item from the
			// start of its block, shadowing the parameters n and g there.
			// Nodes: n@5, g@13, let a = n + g();@40 (Uses nothing), a@95 (the
			// block's tail), let b = …;@30, b + n + g@100 (the parameters).
			name:     "an item names its item from the start of its block, shadowing a local",
			protects: "a read before a block's const or fn item resolves to the item, never the enclosing local of that name",
			mutation: "bind an item's name only from its declaration (gains n@5 and g@13 -> let a = n + g();@40)",
			src:      "fn f(n: i32, g: i32) -> i32 { let b = { let a = n + g(); const n: i32 = 3; fn g() -> i32 { 1 } a }; b + n + g }",
			du: []string{"let a = n + g();@40 -> a@95", "a@95 -> let b = { let a = n + g(); const n: i32 = 3; fn g() -> i32 { 1 } a };@30", "let b = { let a = n + g(); const n: i32 = 3; fn g() -> i32 { 1 } a };@30 -> b + n + g@100",
				"n@5 -> b + n + g@100", "g@13 -> b + n + g@100"},
		},
		{
			// Statements › Declaration statements › Item declarations: use
			// and enum items run nothing. Nodes: c@5, x@14, c@34 (Branch),
			// return x@65, 0@77. Succ: c@34→{return, 0}; return→EXIT.
			name:     "use and enum items make no node",
			protects: "a use declaration or an enum item in a block is no statement node",
			mutation: "make a node for a use or enum item (c@34 gains use std::mem;@38 or enum E { A }@52 as a dependent)",
			src:      "fn f(c: bool, x: i32) -> i32 { if c { use std::mem; enum E { A } return x; } 0 }",
			cd:       []string{"c@34 -> return x@65", "c@34 -> 0@77"},
			du:       []string{"c@5 -> c@34", "x@14 -> return x@65"},
		},
		{
			// Names › Scopes › Pattern binding scopes: a match arm's, a
			// while-let's and a for's bindings end with their construct.
			// Nodes: o@61, Some(x)@65, x@70, g(x)@76, 0@87, the while-let
			// head@98, x@107, g(x)@122, 0..2@139, for@130, x@134, g(x)@146,
			// x@154 (the parameter). Succ: Some(x)→{x@70, 0@87}; x@70→g(x)→head;
			// 0→head; head→{x@107, 0..2}; x@107→g(x)@122→head; 0..2→for→{x@134,
			// x@154}; x@134→g(x)@146→for. IPDom: Some(x) → head → 0..2 → for →
			// x@154.
			name:     "match-arm, while-let and for bindings end with their construct",
			protects: "a read after the three constructs resolves to the parameter, never to a binding of the same name",
			mutation: "keep a construct's bindings in scope after it (x@154 pairs with x@134 instead: loses x@38 -> x@154)",
			src:      "fn f(o: Option<i32>, mut v: Vec<i32>, x: i32) -> i32 { match o { Some(x) => g(x), _ => 0 }; while let Some(x) = v.pop() { g(x); } for x in 0..2 { g(x); } x }",
			cd: []string{"Some(x)@65 -> x@70", "Some(x)@65 -> g(x)@76", "Some(x)@65 -> 0@87",
				"let Some(x) = v.pop()@98 -> x@107", "let Some(x) = v.pop()@98 -> g(x)@122", "let Some(x) = v.pop()@98 -> let Some(x) = v.pop()@98",
				"for@130 -> x@134", "for@130 -> g(x)@146", "for@130 -> for@130"},
			du: []string{"o@5 -> o@61", "o@61 -> Some(x)@65", "o@61 -> x@70", "x@70 -> g(x)@76",
				"v@25 -> let Some(x) = v.pop()@98", "let Some(x) = v.pop()@98 -> x@107", "x@107 -> g(x)@122",
				"0..2@139 -> for@130", "0..2@139 -> x@134", "x@134 -> g(x)@146", "x@38 -> x@154"},
		},
		{
			// Patterns › Identifier patterns, and Types › Closure types ›
			// Capture modes: a `ref mut` binding in a closure borrows the
			// matched enclosing o, one of the closure's writes. Nodes: o@9,
			// the closure@54 (Uses o, may-defines o), let mut g = …;@42,
			// g()@102, o@107.
			name:     "a ref mut binding inside a callable is a may-definition of the matched place where it is created",
			protects: "a read after a closure whose pattern mutably borrows o sees what the closure may write",
			mutation: "record no write for a ref mut binding inside a callable (loses || { if let Some(ref mut v) = o { *v += 1; } }@54 -> o@107)",
			src:      "fn f(mut o: Option<i32>) -> Option<i32> { let mut g = || { if let Some(ref mut v) = o { *v += 1; } }; g(); o }",
			du:       []string{"o@9 -> || { if let Some(ref mut v) = o { *v += 1; } }@54", "|| { if let Some(ref mut v) = o { *v += 1; } }@54 -> let mut g = || { if let Some(ref mut v) = o { *v += 1; } };@42", "let mut g = || { if let Some(ref mut v) = o { *v += 1; } };@42 -> g()@102", "o@9 -> o@107", "|| { if let Some(ref mut v) = o { *v += 1; } }@54 -> o@107"},
		},
		{
			// The Rust Unstable Book, Language features › try_blocks (valid
			// on nightly behind `#![feature(try_blocks)]`): a `?`
			// completes the innermost try block, whatever loops lie between.
			// Nodes: v@5, let mut n = 0;@68, v@92, for@83, o@87, (*o)?@101
			// (Branch; its break lands after the try block, defining its
			// result), n += (*o)?@96, n@110 (the tail, defining the result),
			// let r …;@41, r@115. Succ: for→{o, n@110}; o→(*o)?→{let r,
			// n += …}; n += …→for; n@110→let r→r. IPDom: for → let r; o →
			// (*o)? → let r; n += … → for; n@110 → let r.
			name:     "a question mark in a loop inside a try block completes the block",
			protects: "`?` breaks to the try block's end across an enclosing loop, defining the block's result, and never to the loop or the function's exit",
			mutation: "send a `?` inside a loop to Exit (loses (*o)?@101 -> let r: Option<i32> = try { let mut n = 0; for o in v { n += (*o)?; } n };@41; (*o)?@101 then controls let r: Option<i32> = try { let mut n = 0; for o in v { n += (*o)?; } n };@41 and r@115)",
			src:      "fn f(v: &[Option<i32>]) -> Option<i32> { let r: Option<i32> = try { let mut n = 0; for o in v { n += (*o)?; } n }; r }",
			cd: []string{"for@83 -> o@87", "for@83 -> (*o)?@101", "for@83 -> n@110", "(*o)?@101 -> n += (*o)?@96",
				"(*o)?@101 -> for@83"},
			du: []string{"v@5 -> v@92", "v@92 -> for@83", "v@92 -> o@87", "o@87 -> (*o)?@101",
				"(*o)?@101 -> n += (*o)?@96", "(*o)?@101 -> let r: Option<i32> = try { let mut n = 0; for o in v { n += (*o)?; } n };@41", "let mut n = 0;@68 -> n += (*o)?@96",
				"n += (*o)?@96 -> n += (*o)?@96", "let mut n = 0;@68 -> n@110", "n += (*o)?@96 -> n@110",
				"n@110 -> let r: Option<i32> = try { let mut n = 0; for o in v { n += (*o)?; } n };@41", "let r: Option<i32> = try { let mut n = 0; for o in v { n += (*o)?; } n };@41 -> r@115"},
		},
		{
			// Patterns › Identifier patterns (`ref mut x @ q`) and Struct
			// patterns (the shorthand `ref mut f`): each borrows the matched
			// place. Nodes: o@9, s@29, let Some(ref mut x @ 1..=3) = o@47 (Branch), x@64 (Uses the owned
			// variable, defines x, may-defines o), *x = 0@81, let S { ref mut f } = s;@91, f@107
			// (likewise, may-defines s), *f = 1@116, g(o, s)@124 (it pairs with
			// o@9 and s@29, the killing definitions reaching it through the
			// chis x@64 and f@107, and with those chis). Succ:
			// let Some(ref mut x @ 1..=3) = o@47→{x@64, let S { ref mut f } = s;@91}; x@64→*x = 0→let S { ref mut f } = s;@91→f→*f = 1→g. IPDom: let Some(ref mut x @ 1..=3) = o@47 → let S { ref mut f } = s;@91.
			name:     "a bound ref mut pattern and a struct shorthand ref mut borrow the matched place",
			protects: "`ref mut x @ q` and `S { ref mut f }` are may-definitions of o and s, reaching the later reads of both",
			mutation: "bind `ref mut x @ q` or the shorthand `ref mut f` as a plain binding (loses o@9 -> x@64 and x@64 -> g(o, s)@124, or s@29 -> f@107 and f@107 -> g(o, s)@124)",
			src:      "fn f(mut o: Option<i32>, mut s: S) -> i32 { if let Some(ref mut x @ 1..=3) = o { *x = 0; } let S { ref mut f } = s; *f = 1; g(o, s) }",
			cd:       []string{"let Some(ref mut x @ 1..=3) = o@47 -> x@64", "let Some(ref mut x @ 1..=3) = o@47 -> *x = 0@81"},
			du: []string{"o@9 -> let Some(ref mut x @ 1..=3) = o@47", "let Some(ref mut x @ 1..=3) = o@47 -> x@64", "o@9 -> x@64", "x@64 -> *x = 0@81", "s@29 -> let S { ref mut f } = s;@91", "let S { ref mut f } = s;@91 -> f@107",
				"s@29 -> f@107", "f@107 -> *f = 1@116", "o@9 -> g(o, s)@124", "x@64 -> g(o, s)@124",
				"s@29 -> g(o, s)@124", "f@107 -> g(o, s)@124"},
		},
		{
			// Macros › Macro invocation: in a token tree, a name after a
			// label's `'`, before `!` or before `:` is a label, a macro's
			// name or a field name; `'a:` declares a label the tree's break
			// uses, so it stays in the tree. Nodes: m!(…)@38 (a Stmt reading
			// nothing), 0@…. No pair.
			name:     "a name after a quote or before a bang or a colon in a token tree is not read",
			protects: "a local named like a token-tree label, macro or field is not read by the invocation, and a break to a label the tree declares does not leave it",
			mutation: "read a name after `'` or before `!` or `:` (gains a@5, g@13 or x@21 -> m!('a: loop { break 'a; } g!() S { x: 1 })@38)",
			src:      "fn f(a: i32, g: i32, x: i32) -> i32 { m!('a: loop { break 'a; } g!() S { x: 1 }); 0 }",
		},
		{
			// Macros › Macro invocation, and Borrow operators: in a token
			// tree, `&&mut x`, `&mut s.f` and `&mut *p` each borrow their base
			// variable. Nodes: x@9, s@21, p@27, m!(&&mut x, &mut s.f, &mut *p)@49 (Uses x, s and p,
			// may-defines each), x + s.f + *p@81.
			name:     "a token tree's double-ampersand, field and dereference borrows may-define their bases",
			protects: "a read after the invocation sees the borrows `&&mut x`, `&mut s.f` and `&mut *p` may have written",
			mutation: "borrow only after a single `&` and before a bare name (loses m!(&&mut x, &mut s.f, &mut *p)@49 -> x + s.f + *p@81)",
			src:      "fn f(mut x: i32, mut s: S, p: &mut i32) -> i32 { m!(&&mut x, &mut s.f, &mut *p); x + s.f + *p }",
			du: []string{"x@9 -> m!(&&mut x, &mut s.f, &mut *p)@49", "s@21 -> m!(&&mut x, &mut s.f, &mut *p)@49", "p@27 -> m!(&&mut x, &mut s.f, &mut *p)@49", "x@9 -> x + s.f + *p@81",
				"s@21 -> x + s.f + *p@81", "p@27 -> x + s.f + *p@81", "m!(&&mut x, &mut s.f, &mut *p)@49 -> x + s.f + *p@81"},
		},
		{
			// The std::fmt library documentation, Precision: `.p$` takes the
			// precision from the variable p. Nodes: p@5, format!(…)@27.
			name:     "a precision capture in a format string reads its variable",
			protects: "`{:.p$}` reads p",
			mutation: "read only width captures (loses p@5 -> format!(\"{:.p$}\", 1.5)@27)",
			src:      "fn f(p: usize) -> String { format!(\"{:.p$}\", 1.5) }",
			du:       []string{"p@5 -> format!(\"{:.p$}\", 1.5)@27"},
		},
		{
			// The std::fmt library documentation: byte strings and C strings
			// are no format strings. Nodes: m!(…)@22 (reads nothing), 0@….
			name:     "byte and C string literals in a token tree are not format strings",
			protects: "`b\"{x}\"` and `c\"{x}\"` capture nothing",
			mutation: "read byte or C strings as format strings (gains x@5 -> m!(b\"{x}\", c\"{x}\")@22)",
			src:      "fn f(x: i32) -> i32 { m!(b\"{x}\", c\"{x}\"); 0 }",
		},
		{
			// The std::fmt library documentation: which macros format is not
			// known here, so any macro's plain string literal is read as a
			// format string. Nodes: x@5, m!(\"{x}\")@22.
			name:     "a string literal in any macro is read as a format string",
			protects: "a capture in a user macro's literal is a read, since the macro may format it",
			mutation: "read format strings only in the standard formatting macros (loses x@5 -> m!(\"{x}\")@22)",
			src:      "fn f(x: i32) -> i32 { m!(\"{x}\"); 0 }",
			du:       []string{"x@5 -> m!(\"{x}\")@22"},
		},
		{
			// Macros › Macro invocation, and Expressions › `break` and
			// `continue` expressions: a break or continue in a tree with no
			// loop of its own leaves the invocation to the source loop. Nodes:
			// v@5, let mut n = 0;@27, v@51, for@42, x@46, m!(if x > 0 { continue } else { break })@55 (Branch Using x:
			// to the head, past the loop and on), n += 1@97, n@107. Succ:
			// for→{x, n@107}; x→m!(if x > 0 { continue } else { break })@55→{for, n@107, n += 1}; n += 1→for. IPDom:
			// for → n@107; x → m!(if x > 0 { continue } else { break })@55 → n@107; n += 1 → for.
			name:     "a break or continue in a token tree leaves to the enclosing source loop",
			protects: "the invocation branches to the loop's head and past it, so the loop's rest depends on it",
			mutation: "leave break and continue out of the token-tree jumps (m!(if x > 0 { continue } else { break })@55 is a Stmt: loses m!(if x > 0 { continue } else { break })@55 -> n += 1@97 and m!(if x > 0 { continue } else { break })@55 -> for@42)",
			src:      "fn f(v: Vec<i32>) -> i32 { let mut n = 0; for x in v { m!(if x > 0 { continue } else { break }); n += 1; } n }",
			cd:       []string{"for@42 -> x@46", "for@42 -> m!(if x > 0 { continue } else { break })@55", "m!(if x > 0 { continue } else { break })@55 -> for@42", "m!(if x > 0 { continue } else { break })@55 -> n += 1@97"},
			du: []string{"v@5 -> v@51", "v@51 -> for@42", "v@51 -> x@46", "x@46 -> m!(if x > 0 { continue } else { break })@55",
				"let mut n = 0;@27 -> n += 1@97", "n += 1@97 -> n += 1@97", "let mut n = 0;@27 -> n@107",
				"n += 1@97 -> n@107"},
		},
		{
			// The Rust Unstable Book, try_blocks (valid on nightly behind
			// `#![feature(try_blocks)]`), and Macros › Macro
			// invocation: a `?` in a tree inside a source try block breaks to
			// the block's end, defining no result. Nodes: a@5, m!(a?)@57
			// (Branch), 1@65 (the tail, defining the block's result), let r: Option<i32> = try { m!(a?); 1 };@30,
			// r.unwrap_or(0)@70. Succ: m!→{1, let r: Option<i32> = try { m!(a?); 1 };@30}; 1→let r: Option<i32> = try { m!(a?); 1 };@30→r.unwrap_or(0).
			// IPDom: m! → let r: Option<i32> = try { m!(a?); 1 };@30.
			name:     "a question mark in a token tree inside a try block breaks to the block's end",
			protects: "what follows the try block does not depend on a `?` in a macro inside it",
			mutation: "send a tree's `?` to Exit inside a try block (m!(a?)@57 then also controls let r: Option<i32> = try { m!(a?); 1 };@30 and r.unwrap_or(0)@70)",
			src:      "fn f(a: Option<i32>) -> i32 { let r: Option<i32> = try { m!(a?); 1 }; r.unwrap_or(0) }",
			cd:       []string{"m!(a?)@57 -> 1@65"},
			du:       []string{"a@5 -> m!(a?)@57", "1@65 -> let r: Option<i32> = try { m!(a?); 1 };@30", "let r: Option<i32> = try { m!(a?); 1 };@30 -> r.unwrap_or(0)@70"},
		},
		{
			// The Rust Reference, Trait and lifetime bounds: `?Sized` is a
			// bound, not the try propagation operator, since its `?` follows
			// no operand. Nodes: m!(T: ?Sized)@22 (a Stmt), x@37.
			name:     "a question mark after a colon in a token tree is no jump",
			protects: "a `?Sized` bound in a macro's arguments does not make the invocation a branch",
			mutation: "take every `?` token as the operator (m!(T: ?Sized)@22 becomes a Branch controlling x@37)",
			src:      "fn f(x: i32) -> i32 { m!(T: ?Sized); x }",
			du:       []string{"x@5 -> x@37"},
		},
		{
			// Macros › Macro invocation: the `{…}` after `fn`, and an async
			// block, hold their own jumps. Nodes: o@5, m!(…)@38 (a Stmt Using
			// o), Some(1)@76.
			name:     "a fn body or async block in a token tree contains its jumps",
			protects: "a return in a tree's fn item and a `?` in a tree's async block are no jumps of the enclosing function",
			mutation: "let a tree's fn body or async block leave (m!(fn g() { return; }, async { o? })@38 becomes a Branch controlling Some(1)@76)",
			src:      "fn f(o: Option<i32>) -> Option<i32> { m!(fn g() { return; }, async { o? }); Some(1) }",
			du:       []string{"o@5 -> m!(fn g() { return; }, async { o? })@38"},
		},
		{
			// Expressions › Loops and other breakable expressions › Loop
			// labels, and Macros › Macro invocation: a labelled jump leaves
			// the tree unless the tree declares its label. Nodes: loop@13,
			// m!(break 'o)@20 (Branch: past the loop and on), m!('i: …)@34 (a
			// Stmt). Succ: loop→m!@20→{EXIT, m!@34}; m!@34→loop. IPDom: m!@20
			// → EXIT; m!@34 → loop → m!@20.
			name:     "a labelled jump in a token tree leaves unless the tree declares its label",
			protects: "`break 'o` in a tree leaves to the source loop 'o, and `break 'i` stays in the tree declaring 'i",
			mutation: "keep every labelled jump in its tree (m!(break 'o)@20 controls nothing), or let every one leave (break 'i names no open frame: unresolved becomes 1)",
			src:      "fn f() { 'o: loop { m!(break 'o); m!('i: loop { break 'i; }); } }",
			cd: []string{"m!(break 'o)@20 -> m!('i: loop { break 'i; })@34", "m!(break 'o)@20 -> loop@13",
				"m!(break 'o)@20 -> m!(break 'o)@20"},
		},
		{
			// The Rust Unstable Book, try_blocks, and Macros › Macro
			// invocation: a `?` inside a try block written in the tree stays
			// there. Nodes: o@5, m!(try { o? })@38 (a Stmt Using o), Some(1)@54.
			name:     "a question mark inside a try block in a token tree stays in the tree",
			protects: "the invocation holding `try { o? }` is no branch of the enclosing function",
			mutation: "let a `?` in a tree's try block leave (m!(try { o? })@38 becomes a Branch controlling Some(1)@54)",
			src:      "fn f(o: Option<i32>) -> Option<i32> { m!(try { o? }); Some(1) }",
			du:       []string{"o@5 -> m!(try { o? })@38"},
		},
		{
			// Macros › Macro invocation: a name a tree binds is not scoped,
			// so a later token of that name still reads the enclosing
			// variable (an over-approximation). Nodes: x@5, m!(let x = 1;
			// x)@22 (Uses x: the x before `=` is a binding name, the later x
			// the parameter), x@40.
			name:     "a name a token tree binds is not scoped",
			protects: "the tree's later read of x resolves to the enclosing x, never to nothing",
			mutation: "scope a tree's let (the later x resolves to the tree's binding: loses x@5 -> m!(let x = 1; x)@22)",
			src:      "fn f(x: i32) -> i32 { m!(let x = 1; x); x }",
			du:       []string{"x@5 -> m!(let x = 1; x)@22", "x@5 -> x@40"},
		},
		{
			// Macros › Macro invocation, and Expressions › Closure
			// expressions: a macro inside a closure walked for its captures
			// makes no jump of the enclosing function, and its reads are
			// captures. Nodes: o@5, || { m!(o?); }@38 (Uses o), let c = …;@30,
			// c()@54, 0@59.
			name:     "a macro inside a callable walked for captures makes no jump and its reads are captures",
			protects: "a `?` in a macro inside a closure is the closure's, and the tree's reads are the creating node's",
			mutation: "let a macro inside a callable walked for captures make its jumps (a Branch m!(o?)@43 appears in f and controls c()@54 and 0@59)",
			src:      "fn f(o: Option<i32>) -> i32 { let c = || { m!(o?); }; c(); 0 }",
			du: []string{"o@5 -> || { m!(o?); }@38", "|| { m!(o?); }@38 -> let c = || { m!(o?); };@30",
				"let c = || { m!(o?); };@30 -> c()@54"},
		},
		{
			// Expressions › `if` expressions › Chains of conditions: a later
			// member of a let chain runs only when the members before it
			// hold, and Expressions › Evaluation order of operands: the let
			// reads x before the if runs. Nodes: o@5, x@25, let Some(_) =
			// o@57 (Branch, the first member), x = 2@78 (defines x; it runs
			// only when o is Some, so it takes none of the let's reads), true@85
			// (the block's tail, defining its result), { x = 2; true }@76 (the
			// second member's Branch, Using that result), 1@94 and 0@105 (the
			// arm tails, defining the if's result), let y = …;@42 (Uses x and
			// the if's result), y + x@110. Succ: let Some(_) = o→{x = 2, 0};
			// x = 2→true→{ x = 2; true }→{1, 0}; 1, 0→let y→y + x. IPDom: both
			// Branches → let y; x = 2 → true → { x = 2; true }.
			name:     "an assignment in a later let-chain member leaves an earlier read on its consumer",
			protects: "the let's read of x pairs with the parameter on the path where the first member fails and the second never runs",
			mutation: "lower a later let-chain member in no region (x = 2@78 takes the let's read: gains x@25 -> x = 2@78, loses x@25 -> let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 };@42)",
			src:      "fn f(o: Option<i32>, mut x: i32) -> i32 { let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 }; y + x }",
			cd: []string{"let Some(_) = o@57 -> x = 2@78", "let Some(_) = o@57 -> true@85",
				"let Some(_) = o@57 -> { x = 2; true }@76", "let Some(_) = o@57 -> 0@105",
				"{ x = 2; true }@76 -> 1@94", "{ x = 2; true }@76 -> 0@105"},
			du: []string{"o@5 -> let Some(_) = o@57", "true@85 -> { x = 2; true }@76",
				"x@25 -> let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 };@42", "x = 2@78 -> let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 };@42", "1@94 -> let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 };@42", "0@105 -> let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 };@42",
				"let y = x + if let Some(_) = o && { x = 2; true } { 1 } else { 0 };@42 -> y + x@110", "x@25 -> y + x@110", "x = 2@78 -> y + x@110"},
		},
		{
			// Expressions › Loops and other breakable expressions › Infinite
			// loops: a `loop` is left only by a break, so the statements of
			// its body before the first break run whenever the let consuming
			// its value does. Nodes: x@9, loop@38 (head), x = 1@45 (takes the
			// let's held read: Uses x, defines x and an owned variable holding
			// the earlier x), break 0@52 (defines the loop's result), let y =
			// …;@26 (Uses the handed-on variable and the loop's result), y +
			// x@63. The break leaves nothing on the back edge: straight line.
			name:     "a loop body's statements before its first break take the consumer's earlier read",
			protects: "the let's read of x, made before the loop, pairs with the parameter through the hand-off of the assignment that precedes every break",
			mutation: "open the loop's region at its body's start (x = 1@45 takes nothing: loses x@9 -> x = 1@45, and the let pairs with x = 1@45 as if it read the assignment's x)",
			src:      "fn f(mut x: i32) -> i32 { let y = x + loop { x = 1; break 0 }; y + x }",
			du: []string{"x@9 -> x = 1@45", "x = 1@45 -> let y = x + loop { x = 1; break 0 };@26", "break 0@52 -> let y = x + loop { x = 1; break 0 };@26", "let y = x + loop { x = 1; break 0 };@26 -> y + x@63",
				"x = 1@45 -> y + x@63"},
		},
		{
			// Expressions › Loops and other breakable expressions › Labeled
			// block expressions: `break 'a` leaves the block, skipping its
			// later statements. Nodes: c@5, x@18, z@30, x = 1@69 (before any
			// break: takes the let's held read of x), c@79 (Branch), break 'a
			// 0@83 (defines the block's result), z = 2@97 (after the break,
			// which reaches the let past it, so it takes nothing, although the
			// if holding the break has ended), 1@104 (the tail, defining the
			// block's result), let y = …;@47 (Uses the handed-on variable, z
			// and the block's result), y + x + z@109. Succ: c@79→{break,
			// z = 2}; break→let y; z = 2→1→let y. IPDom: c@79 → let y.
			name:     "a labelled block is a region from its first break to its end",
			protects: "an assignment before the block's first break takes the consumer's earlier read, and one after it leaves the read on the consumer, which pairs with the parameter on the break's path",
			mutation: "open the region at the block's start (loses x@18 -> x = 1@69), or let it end with the if holding the break (z = 2@97 takes the read: gains z@30 -> z = 2@97, loses z@30 -> let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47)",
			src:      "fn f(c: bool, mut x: i32, mut z: i32) -> i32 { let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 }; y + x + z }",
			cd:       []string{"c@79 -> break 'a 0@83", "c@79 -> z = 2@97", "c@79 -> 1@104"},
			du: []string{"c@5 -> c@79", "x@18 -> x = 1@69", "x = 1@69 -> let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47", "z@30 -> let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47", "z = 2@97 -> let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47",
				"break 'a 0@83 -> let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47", "1@104 -> let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47", "let y = x + z + 'a: { x = 1; if c { break 'a 0; } z = 2; 1 };@47 -> y + x + z@109", "x = 1@69 -> y + x + z@109",
				"z@30 -> y + x + z@109", "z = 2@97 -> y + x + z@109"},
		},
		{
			// Ill-formed on purpose, and recovered: y=1 is followed by return with no `;` (The Rust
			// Reference, Statements: an expression statement that is not a block ends with `;`); the
			// parser recovers by wrapping the reduced assignment y=1 in an ERROR node it marks as an extra
			// among the block's statements (one tree of 3 bytes costs the recovery less than a missing
			// terminator does), and the return statement after it parses whole. The rule (children and into in lower_rust.go): an ERROR node the
			// parser made an extra is kept and lowered as a kind not named, for its value, so the
			// assignment inside it is the node y=1@45 defining y, and the ERROR node makes no second node
			// over the same span. Nodes: x@5, let mut y = 0;@23, the Branch x@41, y=1@45, return y@49,
			// the tail y@60.
			name:      "an assignment inside an ERROR node the parser made an extra defines its target",
			protects:  "a statement the parser wrapped in an extra ERROR node still makes its one node, and the definition inside it reaches the read after it",
			mutation:  "drop the extra ERROR node's content with the comments (y=1@45 vanishes: x@41 -> y=1@45 and y=1@45 -> return y@49 are lost, and let mut y = 0;@23 -> return y@49 appears); or give the ERROR node a Stmt node of its own beside the assignment's (a second y=1@45 appears, controlled by x@41)",
			src:       "fn f(x: bool) -> i32 { let mut y = 0; if x { y=1 return y } y }",
			recovered: true,
			cd:        []string{"x@41 -> y=1@45", "x@41 -> return y@49"},
			du:        []string{"x@5 -> x@41", "y=1@45 -> return y@49", "let mut y = 0;@23 -> y@60"},
		},
	})
}
