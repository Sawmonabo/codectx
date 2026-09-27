package worker

import (
	"slices"
	"testing"
)

// TestJavaScriptLoweringGolden pins the control-dependence and def-use pairs
// of hand-derived JavaScript functions. Each case is callable 1 in Functions
// preorder unless its fn says otherwise (callable 0 is the program). Every
// pair was derived by hand from the lowering's documented granularity and
// runGolden's derivation and rendering rules, self-pairs included; a `break
// outer` that leaves past a loop head can take the head's control of itself
// from it.
func TestJavaScriptLoweringGolden(t *testing.T) {
	runGolden(t, "javascript", slices.Concat(javascriptShared, javascriptJSX))
}

// javascriptShared holds the JavaScript cases whose sources parse clean in the
// javascript, typescript and tsx grammars, which share one lowering: each runs
// under all three (TestTypeScriptLoweringGolden runs them under the other
// two) with the one expected set it states, so a source here carries neither
// type syntax nor JSX, and a construct whose source would need either has a
// case of its own in the grammar that has it.
var javascriptShared = []goldenCase{
	{
		// ECMA-262 §14.7.3 The while Statement (WhileLoopEvaluation: only a
		// break leaves `while (true)`) and §14.6 The if Statement. h(a)@45 is
		// unreachable; the sink component {true@23, a@35, g()@38} is augmented
		// at true@23, which controls itself and a@35.
		name:     "infinite loop with a branch inside",
		protects: "an infinite loop's nodes get post-dominators only through exit augmentation, and `while (true)` has no exit edge",
		mutation: "drop the sink-component augmentation (every control dependence here is lost), or give `while (true)` a false edge (h(a) becomes reachable and gains a def-use pair)",
		src:      "function f(a) { while (true) { if (a) g(); } h(a); }",
		fn:       1,
		cd:       []string{"true@23 -> true@23", "true@23 -> a@35", "a@35 -> g()@38"},
		du:       []string{"a@11 -> a@35"},
	},
	{
		// ECMA-262 §14.7.4 The for Statement (ForBodyEvaluation: a missing test
		// never ends the loop), §14.6 The if Statement and §14.10 The return
		// Statement.
		name:     "region unreachable from exit",
		protects: "a sink loop inside a branch is augmented at its smallest-reverse-post-order member, the loop head",
		mutation: "augment every member of the sink component, or its largest member (g() becomes a controller)",
		src:      "function f(c) { if (c) { for (;;) { g(); } } return c; }",
		fn:       1,
		cd:       []string{"c@20 -> for@25", "c@20 -> return c;@45", "for@25 -> for@25", "for@25 -> g()@36"},
		du:       []string{"c@11 -> c@20", "c@11 -> return c;@45"},
	},
	{
		// ECMA-262 §14.13 Labelled Statements (LabelledEvaluation hands the
		// label set to the loop), §14.8 The continue Statement, §14.9 The break
		// Statement and §14.7.1.1 LoopContinues. `break outer` leaves past a@33,
		// so ipdom(b@45) = ipdom(g()@54) = ipdom(h()@79) = Exit and a@33, whose
		// back edge b@45 and g()@54 control, does not depend on itself.
		name:     "labelled break and continue out of nested loops",
		protects: "a label on a loop names the loop's own frame, so a labelled continue targets that loop's condition and a labelled break leaves that loop",
		mutation: "open a block frame for every labelled statement instead of handing a loop its labels (continue outer then names no loop: unresolved becomes 1)",
		src:      "function f(a, b) { outer: while (a) { while (b) { if (g()) continue outer; if (h()) break outer; } k(); } }",
		fn:       1,
		cd: []string{
			"a@33 -> b@45",
			"b@45 -> g()@54", "b@45 -> k()@99", "b@45 -> a@33",
			"g()@54 -> continue outer;@59", "g()@54 -> a@33", "g()@54 -> h()@79",
			"h()@79 -> break outer;@84", "h()@79 -> b@45",
		},
		du: []string{"a@11 -> a@33", "b@14 -> b@45"},
	},
	{
		// ECMA-262 §8.3.2 ContainsUndefinedBreakTarget: the label names no
		// statement, an early error the lowering does not reject; the jump is
		// counted and augmented to Exit.
		name:       "break to a missing label",
		protects:   "a jump to no label is counted, keeps no successor and is augmented to Exit",
		mutation:   "wire an unresolved break to the innermost loop, or drop the count",
		src:        "function f() { while (g()) { break missing; } }",
		fn:         1,
		cd:         []string{"g()@22 -> break missing;@29"},
		unresolved: 1,
	},
	{
		// ECMA-262 §14.15.3 The try Statement, Runtime Semantics: Evaluation
		// (TryStatement : try Block Finally runs the Finally on a return
		// completion) and §14.10 The return Statement. No node throws, so there
		// is no Handler: return 1;@40 flows into c = b@69, then Exit.
		name:     "return inside try reaches Exit through the finally",
		protects: "a return inside a try is intercepted by the finally, which then runs with the definitions reaching the return",
		mutation: "send the return straight to Exit (b = 0 no longer reaches the finally, and the finally becomes control dependent on c)",
		src:      "function f(c) { let b = 0; try { if (c) return 1; b = 1; } finally { c = b; } }",
		fn:       1,
		cd:       []string{"c@37 -> return 1;@40", "c@37 -> b = 1@50"},
		du:       []string{"c@11 -> c@37", "b = 0@20 -> c = b@69", "b = 1@50 -> c = b@69"},
	},
	{
		// ECMA-262 §14.14 The throw Statement, §14.15.2 CatchClauseEvaluation
		// and §13.3.6 Function Calls (a call may throw).
		name:     "a throw from the first of several try statements reaches the handler",
		protects: "an explicit throw inside a try reaches its handler, and every throwing node, not only the last, has an exceptional edge to it",
		mutation: "send the throw statement to Exit instead of the catch (e and r = e lose their control dependence on c), or skip MayThrow on a call statement (h() loses its three control dependences)",
		src:      "function f(c) { let r = 0; try { if (c) throw c; r = 1; h(); } catch (e) { r = e; } return r; }",
		fn:       1,
		cd: []string{
			"c@37 -> throw c;@40", "c@37 -> e@70", "c@37 -> r = e@75", "c@37 -> r = 1@49", "c@37 -> h()@56",
			"h()@56 -> catch@63", "h()@56 -> e@70", "h()@56 -> r = e@75",
		},
		du: []string{
			"c@11 -> c@37", "c@11 -> throw c;@40",
			"r = 1@49 -> return r;@84", "r = e@75 -> return r;@84", "e@70 -> r = e@75",
		},
	},
	{
		// ECMA-262 §13.13 Binary Logical Operators: either operand can be the
		// value. Nodes: a@11, b@14; the Branch a@29 (Uses a) defines the
		// result on the path where it decides it, g(b)@34 (Uses b) on the
		// other; the declarator x = a && g(b)@25 Uses the result and defines
		// x; the Branch x@50 (Uses x) and b@55 (Uses b) define the second
		// result, which y = x ?? b@46 Uses; return y;@58. Each right operand
		// depends on its Branch.
		name:     "short-circuit && and ?? dependences",
		protects: "the right operand of && and ?? is control dependent on the left, and the declarator Uses the operator's result, which both operand nodes define",
		mutation: "lower && or ?? as a plain binary operator (no decision node), define the result only on the right operand's node (a@29 -> x = a && g(b)@25 and x@50 -> y = x ?? b@46 vanish), or give the declarator the operands' reads instead of the result (a@11 -> x = a && g(b)@25 appears)",
		src:      "function f(a, b) { const x = a && g(b); const y = x ?? b; return y; }",
		fn:       1,
		cd:       []string{"a@29 -> g(b)@34", "x@50 -> b@55"},
		du: []string{
			"a@11 -> a@29", "b@14 -> g(b)@34", "a@29 -> x = a && g(b)@25", "g(b)@34 -> x = a && g(b)@25",
			"x = a && g(b)@25 -> x@50", "b@14 -> b@55", "x@50 -> y = x ?? b@46", "b@55 -> y = x ?? b@46",
			"y = x ?? b@46 -> return y;@58",
		},
	},
	{
		// ECMA-262 §14.7.3 The while Statement, §14.6 The if Statement and §13.4
		// Update Expressions.
		name:     "while re-enters its condition and if/else merges both arms",
		protects: "a while loop's back edge re-enters its condition node and both if/else arms flow on, so both arms' definitions reach the loop, each other and the return, self-pairs included",
		mutation: "drop the Merge of the then arm's end in ifStmt (s = s + i@67 keeps no successor and every pair from it is lost), or drop the loop's back edge (i < n@45 loses its self-dependence and i++@100 its pair to i < n@45)",
		src:      "function f(n) { let s = 0; let i = 0; while (i < n) { if (i > 2) { s = s + i; } else { s = s - 1; } i++; } return s; }",
		fn:       1,
		cd: []string{
			"i < n@45 -> i < n@45", "i < n@45 -> i > 2@58", "i < n@45 -> i++@100",
			"i > 2@58 -> s = s + i@67", "i > 2@58 -> s = s - 1@87",
		},
		du: []string{
			"n@11 -> i < n@45",
			"s = 0@20 -> s = s + i@67", "s = 0@20 -> s = s - 1@87", "s = 0@20 -> return s;@107",
			"s = s + i@67 -> s = s + i@67", "s = s + i@67 -> s = s - 1@87", "s = s + i@67 -> return s;@107",
			"s = s - 1@87 -> s = s + i@67", "s = s - 1@87 -> s = s - 1@87", "s = s - 1@87 -> return s;@107",
			"i = 0@31 -> i < n@45", "i = 0@31 -> i > 2@58", "i = 0@31 -> s = s + i@67", "i = 0@31 -> i++@100",
			"i++@100 -> i < n@45", "i++@100 -> i > 2@58", "i++@100 -> s = s + i@67", "i++@100 -> i++@100",
		},
	},
	{
		// ECMA-262 §14.3.3 Destructuring Binding Patterns (the initializer is
		// evaluated once; a default only when the value is undefined) and
		// §15.3 Arrow Function Definitions. Nodes: o@11; the initializer o@37
		// (Uses o, defines the incoming value); a@24 (Uses it, defines a); the
		// default's Branch b = a@27 (Uses it, defines b from it); a@31 (Uses
		// the a of a@24, defines b from the default); the arrow (a) => a +
		// b@50 (Uses its capture b, its own a shadowing; defines its result);
		// the declarator g = (a) => a + b@46 (Uses the result, defines g);
		// return g;@64. a@31 depends on b = a@27.
		name:     "destructuring default and a closure that shadows",
		protects: "the initializer is evaluated once at its own node, one defining node per bound name, a default as a conditional definition, a closure's own parameter shadowing the enclosing name, and the declarator reading the created closure through its result",
		mutation: "resolve names inside the arrow against the enclosing scope only (a@24 -> (a) => a + b@50 appears), bind the default unconditionally, give each element the initializer's reads instead of its result (o@11 -> a@24 and o@11 -> b = a@27 appear), or drop the creating node's result ((a) => a + b@50 -> g = (a) => a + b@46 vanishes)",
		src:      "function f(o) { const { a, b = a } = o; const g = (a) => a + b; return g; }",
		fn:       1,
		cd:       []string{"b = a@27 -> a@31"},
		du: []string{
			"o@11 -> o@37", "o@37 -> a@24", "o@37 -> b = a@27", "a@24 -> a@31", "b = a@27 -> (a) => a + b@50",
			"a@31 -> (a) => a + b@50", "(a) => a + b@50 -> g = (a) => a + b@46",
			"g = (a) => a + b@46 -> return g;@64",
		},
	},
	{
		// ECMA-262 §13.15.5 Destructuring Assignment: the right side is
		// evaluated once, then the elements are assigned in order. Nodes:
		// x@11, y@14; the right side [y, x]@28 (Uses y and x, defines the
		// incoming value); x@20 and y@23 (each Uses it and defines its name);
		// return x + y;@36.
		name:     "destructuring swap reads the values from before the statement",
		protects: "a destructuring's right side is evaluated once, at its own node, so every element reads the values from before the statement through that node's result, never a variable an earlier element wrote",
		mutation: "give each element the right side's reads instead of its result (x@20 -> y@23 appears, with x@11 and y@14 pairing with both elements, and [y, x]@28 vanishes)",
		src:      "function f(x, y) { [x, y] = [y, x]; return x + y; }",
		fn:       1,
		du: []string{
			"x@11 -> [y, x]@28", "y@14 -> [y, x]@28", "[y, x]@28 -> x@20", "[y, x]@28 -> y@23",
			"x@20 -> return x + y;@36", "y@23 -> return x + y;@36",
		},
	},
	{
		// ECMA-262 §13.15.2 Assignment Operators, Runtime Semantics: Evaluation
		// (the right side is evaluated before PutValue) and §14.15.3 The try
		// Statement.
		name:     "a throwing assignment in a try leaves the old value to the catch",
		protects: "an assignment whose right side throws lands on the catch's Handler, which sees the value from before the assignment, not the assignment's",
		mutation: "carry a node's own definition along its exceptional edge (return x pairs with x = g() only, not with x = 1)",
		src:      "function f() { let x = 1; try { x = g(); } catch (e) { return x; } }",
		fn:       1,
		cd:       []string{"x = g()@32 -> catch@43", "x = g()@32 -> e@50", "x = g()@32 -> return x;@55"},
		du:       []string{"x = 1@19 -> return x;@55"},
	},
	{
		// ECMA-262 §14.15.3 The try Statement (TryStatement : try Block Finally)
		// and §13.15.2 Assignment Operators.
		name:     "a finally sees both a throwing assignment's old value and its new one",
		protects: "a finally is entered normally with the assignment's value and exceptionally, through its Handler, with the value from before it",
		mutation: "carry a node's own definition along its exceptional edge (h(x) loses x = 1), or land a MayThrow source on the finally's first node directly (there is no Handler, x = g() controls nothing and h(x) loses x = 1)",
		src:      "function f() { let x = 1; try { x = g(); } finally { h(x); } }",
		fn:       1,
		cd:       []string{"x = g()@32 -> finally@43"},
		du:       []string{"x = 1@19 -> h(x)@53", "x = g()@32 -> h(x)@53"},
	},
	{
		// ECMA-262 §14.7.3 The while Statement (the condition is evaluated on
		// every iteration) and §13.15.2 (an assignment's value is the value
		// assigned). The embedded assignment m = re.exec(s)@35 Uses re and s
		// and defines m, which carries its value to the condition's Branch (m
		// = re.exec(s)) !== null@34; the back edge from g(m)@61 re-enters the
		// assignment.
		name:     "an assignment embedded in a loop condition flows into the condition",
		protects: "the node evaluating an expression that embeds `m = e` reads the value through m, not the reads of e, and the loop's back edge re-enters that assignment",
		mutation: "drop the result of an embedded assignment (the condition loses m = re.exec(s)@35), or give the condition the assignment's reads (re@11 and s@15 pair with the condition)",
		src:      "function f(re, s) { let m; while ((m = re.exec(s)) !== null) g(m); }",
		fn:       1,
		cd: []string{
			"(m = re.exec(s)) !== null@34 -> (m = re.exec(s)) !== null@34",
			"(m = re.exec(s)) !== null@34 -> m = re.exec(s)@35", "(m = re.exec(s)) !== null@34 -> g(m)@61",
		},
		du: []string{
			"re@11 -> m = re.exec(s)@35", "s@15 -> m = re.exec(s)@35",
			"m = re.exec(s)@35 -> (m = re.exec(s)) !== null@34", "m = re.exec(s)@35 -> g(m)@61",
		},
	},
	{
		// ECMA-262 §13.15.2 Assignment Operators and §14.3.1 (lexical
		// declarations). Nodes: x@11, x = g()@27 (defines x and its
		// result), y = (x = g()) + 1@22 (Uses the result, defines y),
		// return y;@41.
		name:     "an assignment embedded in an initializer flows into the declared name",
		protects: "a declaration whose initializer embeds `x = e` reads the value assigned through the assignment's result",
		mutation: "drop the result of an embedded assignment (y's definition uses nothing)",
		src:      "function f(x) { const y = (x = g()) + 1; return y; }",
		fn:       1,
		du:       []string{"x = g()@27 -> y = (x = g()) + 1@22", "y = (x = g()) + 1@22 -> return y;@41"},
	},
	{
		// ECMA-262 §14.7.5.6 ForIn/OfHeadEvaluation evaluates arr once;
		// §14.7.5.7 ForIn/OfBodyEvaluation assigns last only once next
		// yields a value. Nodes: arr@11, last = null@22, arr@48 (Uses arr,
		// defines the iteration variable), the head last of arr@40
		// (Uses the iteration variable), last@40 on the body path (Uses
		// it, defines last), return last;@56 on the exit edge. The empty
		// body returns to the head, which controls last@40 and itself.
		name:     "a for…of over a bare identifier defines it only on the body path",
		protects: "the loop's exit edge carries the definition from before the loop, since an empty iterable assigns nothing, and the head is its own node, distinct from the definition",
		mutation: "define the identifier on the loop head (return last loses last = null@22), span the head by the left side alone (it renders as last@40, like the definition), or give the head and the binding the iterated expression's reads (arr@11 -> last of arr@40 and arr@11 -> last@40 appear)",
		src:      "function f(arr) { let last = null; for (last of arr) {} return last; }",
		fn:       1,
		cd:       []string{"last of arr@40 -> last of arr@40", "last of arr@40 -> last@40"},
		du: []string{
			"arr@11 -> arr@48", "arr@48 -> last of arr@40", "arr@48 -> last@40",
			"last = null@22 -> return last;@56", "last@40 -> return last;@56",
		},
	},
	{
		// ECMA-262 §14.7.5.6 ForIn/OfHeadEvaluation: the iterated expression
		// is evaluated once, before the first iteration, so a body that writes
		// the iterated name does not change what is iterated. Nodes: d@11,
		// xs@14; d@36 (Uses d, defines the first iteration variable), the head
		// k in d@31 (Uses it), k@31 (Uses it, defines k), d[k] = g(k)@39 (a
		// property write: Uses d and k, may-defines d, so it reaches its own
		// use of d through the back edge); xs@68 (Uses xs, defines the second
		// iteration variable), the head x of xs@63 (Uses it), x@63 (Uses it,
		// defines x), xs = h(x)@72 (Uses x, defines xs); return xs;@83,
		// reached by xs@14 on the path where the second loop never runs its
		// body and by xs = h(x)@72. Each head controls its binding, its body
		// and itself.
		name:     "a body that writes the iterated name does not reach the loop head",
		protects: "the head and the binding read the iteration variable the iterated expression defined once, so a write to the iterated name in the body, a property write's may-definition included, pairs only with later reads of that name",
		mutation: "give the head and the binding the iterated expression's reads (xs = h(x)@72 -> x of xs@63, xs = h(x)@72 -> x@63, d[k] = g(k)@39 -> k in d@31 and d[k] = g(k)@39 -> k@31 appear), or drop a property write's may-definition of its base (d[k] = g(k)@39 -> d[k] = g(k)@39 vanishes)",
		src:      "function f(d, xs) { for (const k in d) d[k] = g(k); for (const x of xs) xs = h(x); return xs; }",
		fn:       1,
		cd: []string{
			"k in d@31 -> k in d@31", "k in d@31 -> k@31", "k in d@31 -> d[k] = g(k)@39",
			"x of xs@63 -> x of xs@63", "x of xs@63 -> x@63", "x of xs@63 -> xs = h(x)@72",
		},
		du: []string{
			"d@11 -> d@36", "d@11 -> d[k] = g(k)@39", "d[k] = g(k)@39 -> d[k] = g(k)@39", "d@36 -> k in d@31",
			"d@36 -> k@31", "k@31 -> d[k] = g(k)@39", "xs@14 -> xs@68", "xs@14 -> return xs;@83",
			"xs@68 -> x of xs@63", "xs@68 -> x@63", "x@63 -> xs = h(x)@72", "xs = h(x)@72 -> return xs;@83",
		},
	},
	{
		// ECMA-262 §13.15.2 Assignment Operators (the reference, then the
		// right side, then PutValue, which may throw) and §13.14 Conditional
		// Operator. Nodes: o@11, c@14; the Branch c@31 (Uses c) and its arms
		// 1@35 and 2@39, each defining the conditional's result; the write o.p
		// = c ? 1 : 2@25 (Uses o and the result, may-defines o, may throw);
		// the catch's Handler catch@43, e@50, h()@55. The write's successors
		// are the Handler and Exit, so it controls catch, e and h().
		name:     "a property write whose right side branches is its own throwing node",
		protects: "a property write is one node after its right side, and only it throws for the store, so the right side's decision does not control the catch; the write reads the conditional through its result, not its condition",
		mutation: "count the store's throw before the right side (c@31 gains control of catch@43, e@50 and h()@55), skip the write node when the right side made a node (the write node and its pairs vanish), or give the write the condition's reads instead of the result (c@14 -> o.p = c ? 1 : 2@25 appears, 1@35 and 2@39 lose their pairs)",
		src:      "function f(o, c) { try { o.p = c ? 1 : 2 } catch (e) { h() } }",
		fn:       1,
		cd: []string{
			"c@31 -> 1@35", "c@31 -> 2@39", "o.p = c ? 1 : 2@25 -> catch@43", "o.p = c ? 1 : 2@25 -> e@50",
			"o.p = c ? 1 : 2@25 -> h()@55",
		},
		du: []string{
			"c@14 -> c@31", "o@11 -> o.p = c ? 1 : 2@25", "1@35 -> o.p = c ? 1 : 2@25",
			"2@39 -> o.p = c ? 1 : 2@25",
		},
	},
	{
		// ECMA-262 §10.2.11 FunctionDeclarationInstantiation (a function
		// declaration is instantiated before the body runs) and §15.2 Function
		// Definitions. Nodes: a@11, the hoisted g@38 (defines g), b = a@22,
		// the declaration function g() { return b }@29 (Uses its capture b,
		// may-defines g, so it pairs with g@38), return g;@55, reached by the
		// declaration's may-definition and, through it, by g@38.
		name:     "a hoisted function declaration captures at its own position",
		protects: "the hoisted name is defined at the block's start, and the declaration's captures are read where it stands, so a definition before it flows into later uses of the name",
		mutation: "read the captures on the hoisted node at the block's start (b = a@22 flows nowhere)",
		src:      "function f(a) { const b = a; function g() { return b } return g; }",
		fn:       1,
		du: []string{
			"a@11 -> b = a@22", "b = a@22 -> function g() { return b }@29",
			"g@38 -> function g() { return b }@29", "g@38 -> return g;@55",
			"function g() { return b }@29 -> return g;@55",
		},
	},
	{
		// ECMA-262 §15.3 Arrow Function Definitions (the arrow closes over the
		// enclosing environment) and §13.15.2 (a compound assignment reads,
		// then writes). Nodes: xs@11, n = 0@21, the arrow x => { n += x }@39
		// (Uses its capture n, may-defines n, defines its result), the call
		// xs.forEach(x => { n += x })@28 (Uses xs and the result), return
		// n;@57, reached by the arrow's may-definition and, through it, by
		// n = 0.
		name:     "a closure's write to an enclosing variable is a may-definition where it is created",
		protects: "a use after a closure's creation sees both the closure's write and the definition reaching the creation, the closure node reads only its captures, and the call reads the closure through the creating node's result",
		mutation: "record a closure's write as a use only (x => { n += x }@39 loses its pair to return n), as a killing definition (return n loses n = 0@21), attach a read no node evaluated to the next node made (xs@11 -> x => { n += x }@39 appears), or give the call the arrow's captures instead of its result (n = 0@21 -> xs.forEach(x => { n += x })@28 appears)",
		src:      "function f(xs) { let n = 0; xs.forEach(x => { n += x }); return n; }",
		fn:       1,
		du: []string{
			"xs@11 -> xs.forEach(x => { n += x })@28", "n = 0@21 -> x => { n += x }@39",
			"x => { n += x }@39 -> xs.forEach(x => { n += x })@28", "n = 0@21 -> return n;@57",
			"x => { n += x }@39 -> return n;@57",
		},
	},
	{
		// ECMA-262 §13.3.8.1 ArgumentListEvaluation (arguments are evaluated
		// left to right) and §13.15.2 Assignment Operators. Nodes: x@11,
		// x = 1@28 (reads the x the first argument read before writing
		// it, hands that value on through a variable of its own, defines
		// x and its result), return g(x, x = 1);@16 (Uses both).
		name:     "a definition carries its statement's earlier read of the variable",
		protects: "a read of x the enclosing call consumes only after `x = 1` overwrote it still reaches the call, through the definition, which reads it before writing",
		mutation: "drop the Use a defining node records for its statement's earlier read of its variable (x@11 pairs with nothing)",
		src:      "function f(x) { return g(x, x = 1); }",
		fn:       1,
		du:       []string{"x@11 -> x = 1@28", "x = 1@28 -> return g(x, x = 1);@16"},
	},
	{
		// ECMA-262 §15.7.14 ClassDefinitionEvaluation, §15.7.11
		// ClassStaticBlockDefinitionEvaluation (a static block has its own var
		// scope) and §13.14 Conditional Operator. c, f, g and h are free in
		// the unit, so c is no Use. Nodes: the Branch c@14, its arms f()@18
		// and g()@24, each defining the conditional's result, the field x = c
		// ? f() : g()@10 (Uses the result); y = c@42, the Branch y@53,
		// h(y)@56.
		name:     "class field initializers and static blocks are lowered as the class body's unit",
		protects: "a field initializer's decision controls its arms, the field reads the conditional through its result, and a static block's statements are lowered in the same unit with their own scope",
		mutation: "leave field initializers to the class node's captures (the arms lose their control dependence on c@14), keep static blocks as their own callables (the static block's pairs leave this unit), or drop the conditional's result (f()@18 and g()@24 lose their pairs with the field)",
		src:      "class A { x = c ? f() : g(); static { let y = c; if (y) h(y); } }",
		fn:       1,
		cd:       []string{"c@14 -> f()@18", "c@14 -> g()@24", "y@53 -> h(y)@56"},
		du: []string{
			"f()@18 -> x = c ? f() : g()@10", "g()@24 -> x = c ? f() : g()@10", "y = c@42 -> y@53",
			"y = c@42 -> h(y)@56",
		},
	},
	{
		// ECMA-262 §13.3.9 Optional Chains, §13.14 Conditional Operator and
		// §15.3 Arrow Function Definitions. Nodes: a@11, c@14; the Branch a@19
		// before `?.` (Uses a, defines the chain's result, which carries the
		// receiver), the chain a?.b()@19 (Uses it); the Branch c@34 (Uses c),
		// its arms the arrow () => a@38 (Uses its capture a, yields the
		// created closure) and 0@48, each defining the conditional's result;
		// return c ? () => a : 0;@27, which Uses that result.
		name:     "an optional call and a callable operand are one node each",
		protects: "an expression whose own lowering already made the node spanning it takes no second node with the same span, a chain reads its receiver through the Branch before `?.`, and a return reads a conditional through its result",
		mutation: "always end an expression statement or a conditional operand with its own node (a?.b()@19 -> a?.b()@19 and () => a@38 -> () => a@38 appear), give the chain node the receiver's reads (a@11 -> a?.b()@19 appears), or give the return the condition's reads (c@14 -> return c ? () => a : 0;@27 appears)",
		src:      "function f(a, c) { a?.b(); return c ? () => a : 0; }",
		fn:       1,
		cd:       []string{"a@19 -> a?.b()@19", "c@34 -> () => a@38", "c@34 -> 0@48"},
		du: []string{
			"a@11 -> a@19", "a@19 -> a?.b()@19", "c@14 -> c@34", "a@11 -> () => a@38",
			"() => a@38 -> return c ? () => a : 0;@27", "0@48 -> return c ? () => a : 0;@27",
		},
	},
	{
		// ECMA-262 Annex B.3.3 FunctionDeclarations in IfStatement Statement
		// Clauses: the declaration behaves as if it were in a block of its own.
		// Nodes: c@11, g = 0@20, the Branch c@31, the hoisted g@43 (defines
		// the inner g), the declaration function g() { return c }@34 (Uses
		// its capture c, may-defines the inner g, so it pairs with g@43),
		// return g;@60, which reads the outer g.
		name:     "a function declaration as an if body is scoped to that body",
		protects: "the declaration binds and defines its own name in an implicit block, so it never may-defines an enclosing variable of the same name",
		mutation: "lower an if body with stmt alone (the hoisted g@43 vanishes with g@43 -> function g() { return c }@34, and function g() { return c }@34 may-defines the outer g: g = 0@20 -> function g() { return c }@34 and function g() { return c }@34 -> return g;@60 appear)",
		src:      "function f(c) { let g = 0; if (c) function g() { return c } return g; }",
		fn:       1,
		cd:       []string{"c@31 -> g@43", "c@31 -> function g() { return c }@34"},
		du: []string{
			"c@11 -> c@31", "c@11 -> function g() { return c }@34", "g@43 -> function g() { return c }@34",
			"g = 0@20 -> return g;@60",
		},
	},
	{
		// ECMA-262 §14.12.3 CaseClauseIsSelected: each test compares the
		// discriminant's value, evaluated once (§14.12.4), with the test's.
		// Nodes: x@11, the discriminant x@24 (Uses x, defines its value),
		// Branch 1@34 (Uses the value; true: g()@37, break;@42), Branch 2@54
		// (Uses the value; true: h()@57), no default. g() and break depend on
		// 1@34, as does 2@54 on its false edge; h() on 2@54 only.
		name:     "every case test reads the discriminant",
		protects: "a case test's decision reads the discriminant's value through the node that evaluated it once",
		mutation: "give each case test the discriminant's reads instead of its value (x@11 -> 1@34 and x@11 -> 2@54 appear, x@24 -> 1@34 and x@24 -> 2@54 vanish)",
		src:      "function f(x) { switch (x) { case 1: g(); break; case 2: h(); } }",
		fn:       1,
		cd:       []string{"1@34 -> g()@37", "1@34 -> break;@42", "1@34 -> 2@54", "2@54 -> h()@57"},
		du:       []string{"x@11 -> x@24", "x@24 -> 1@34", "x@24 -> 2@54"},
	},
	{
		// ECMA-262 §13.15.2 (AssignmentExpression: LeftHandSideExpression
		// AssignmentOperator AssignmentExpression): the reference is evaluated
		// and its value read (GetValue) before the right side, and PutValue
		// stores after it. Nodes: the property read o.p@25 (Uses o, may throw,
		// defines the reference's value), the right side's Branch c@32 and its
		// arms 1@36 and 2@40, each defining the conditional's result, then the
		// write o.p += c ? 1 : 2@25 (Uses both values, may-defines o, so it
		// pairs with o@11, may throw). Both throwing nodes reach the catch's Handler catch@44, then
		// e@51 and h()@56. The read's successors are c@32 and the Handler;
		// c@32 is post-dominated by the write, the Handler by e and h(), so
		// the read controls those five; the write controls the Handler's
		// three; c controls its arms.
		name:     "a compound property assignment reads the property before its right side",
		protects: "the read of a compound property assignment throws before the right side is evaluated, and the store after it, each on its own node, the store reading the old value through the read's result",
		mutation: "count the read's throw with the store, after the right side (o.p@25 vanishes with its five control dependences and o@11 -> o.p@25), or give the write the reads of o and of the condition instead of the two results (c@14 -> o.p += c ? 1 : 2@25 appears)",
		src:      "function f(o, c) { try { o.p += c ? 1 : 2 } catch (e) { h() } }",
		fn:       1,
		cd: []string{
			"o.p@25 -> c@32", "o.p@25 -> o.p += c ? 1 : 2@25", "o.p@25 -> catch@44", "o.p@25 -> e@51",
			"o.p@25 -> h()@56", "c@32 -> 1@36", "c@32 -> 2@40", "o.p += c ? 1 : 2@25 -> catch@44",
			"o.p += c ? 1 : 2@25 -> e@51", "o.p += c ? 1 : 2@25 -> h()@56",
		},
		du: []string{
			"o@11 -> o.p@25", "c@14 -> c@32", "o.p@25 -> o.p += c ? 1 : 2@25", "1@36 -> o.p += c ? 1 : 2@25",
			"2@40 -> o.p += c ? 1 : 2@25", "o@11 -> o.p += c ? 1 : 2@25",
		},
	},
	{
		// ECMA-262 §16.2.2 Imports: each ImportSpecifier of an ImportDeclaration binds
		// its local name; a comment is not a specifier. Callable 0, the
		// program: a@9 and b@20 each define their import binding, and
		// g(a, b)@34 reads both. Straight-line code, so no control
		// dependence.
		name:     "a comment between import specifiers binds nothing",
		protects: "the lowering walks an import clause as the extraction does and never takes a comment for a specifier (a comment has no name field, so reading one dereferences a nil node)",
		mutation: "end the named-imports walk in importClauseBindings at the first extra (break when the cursor's node IsExtra): b is never bound, so b@20 -> g(a, b)@34 vanishes",
		src:      "import { a, /* c */ b } from \"m\"; g(a, b);",
		fn:       0,
		du:       []string{"a@9 -> g(a, b)@34", "b@20 -> g(a, b)@34"},
	},
	{
		// ECMA-262 §13.15.2: a plain assignment evaluates its right side and
		// stores it (PutValue); it never reads the target's value. Nodes:
		// x@11, the arrow () => { x = 1; }@26 (Uses nothing, may-defines x,
		// so it pairs with x@11, defines its result), the declarator g = ()
		// => { x = 1; }@22 (Uses the result, defines g), return x;@44,
		// reached by the arrow's may-definition and, through it, by x@11.
		// Straight line, no control dependence.
		name:     "a closure's plain assignment writes an enclosing variable without reading it",
		protects: "a closure's plain assignment to an enclosing variable is a may-definition on the creating node, which pairs with the definition reaching it, and a later use pairs with both",
		mutation: "record the closure's write as a killing definition (x@11 -> () => { x = 1; }@26 and x@11 -> return x;@44 vanish), or drop a plain assignment's target from capTarget (x@11 -> () => { x = 1; }@26 and () => { x = 1; }@26 -> return x;@44 vanish)",
		src:      "function f(x) { const g = () => { x = 1; }; return x; }",
		fn:       1,
		du: []string{
			"x@11 -> return x;@44", "() => { x = 1; }@26 -> return x;@44",
			"() => { x = 1; }@26 -> g = () => { x = 1; }@22", "x@11 -> () => { x = 1; }@26",
		},
	},
	{
		// ECMA-262 §15.7.14 ClassDefinitionEvaluation: a static field's
		// initializer runs while the class is created, so the class
		// declaration's node class A { static x = g(); }@21 may throw and
		// is the catch's only way in: it controls the Handler catch@51,
		// e@58 and h()@63. No variable is read.
		name:     "a class whose static initializer runs at its creation may throw there",
		protects: "a static field initializer, a static block or a computed key is a throw point of the class's creation",
		mutation: "count only a heritage and a decorator as a class-level throw (the class node controls nothing and the catch is unreachable)",
		src:      "function f() { try { class A { static x = g(); } } catch (e) { h(); } }",
		fn:       1,
		cd: []string{
			"class A { static x = g(); }@21 -> catch@51", "class A { static x = g(); }@21 -> e@58",
			"class A { static x = g(); }@21 -> h()@63",
		},
	},
	{
		// ECMA-262 §13.15.5 Destructuring Assignment: the assignment's value
		// is its right side's value, evaluated once. Nodes: s@11, k@14, a@23
		// (let a defines a); the right side s@48 (Uses s, defines the incoming
		// value); the element a@42 (Uses its computed key's k and the incoming
		// value, defines a); return g({ [k]: a } = s);@26, which Uses the
		// incoming value only.
		name:     "an embedded destructuring assignment hands on its right side's value",
		protects: "the node consuming an embedded destructuring assignment reads the right side's value through its node's result, never the reads of the elements' keys, defaults or targets",
		mutation: "give the consumer the right side's reads instead of its result (s@11 -> return g({ [k]: a } = s);@26 appears and s@48 -> return g({ [k]: a } = s);@26 vanishes), or leave the elements' reads to the consumer (k@14 -> return g({ [k]: a } = s);@26 appears)",
		src:      "function f(s, k) { let a; return g({ [k]: a } = s); }",
		fn:       1,
		du:       []string{"s@11 -> s@48", "s@48 -> a@42", "k@14 -> a@42", "s@48 -> return g({ [k]: a } = s);@26"},
	},
	{
		// ECMA-262 §14.7.5.7 ForIn/OfBodyEvaluation: a property target is
		// evaluated anew on every iteration, after the next value exists, and
		// PutValue writes through it. Nodes: o@11, xs@14; xs@32 (Uses xs,
		// defines the iteration variable), the head o.p of xs@25 (Uses it); on
		// the body path o.p@25 (Uses it and o, may-defines o), g()@36; return
		// o;@41 on the exit edge. The head controls o.p, g() and itself.
		name:     "a for…of property target is written on the body path each iteration",
		protects: "a for…of property target is its own per-iteration node, off the head, and a write through it may-defines its base",
		mutation: "read the target's object on the loop head (o@11 -> o.p of xs@25 and o.p@25 -> o.p of xs@25 appear), or drop the base may-definition of a property write (o.p@25 -> o.p@25 and o.p@25 -> return o;@41 vanish)",
		src:      "function f(o, xs) { for (o.p of xs) g(); return o; }",
		fn:       1,
		cd:       []string{"o.p of xs@25 -> o.p of xs@25", "o.p of xs@25 -> o.p@25", "o.p of xs@25 -> g()@36"},
		du: []string{
			"xs@14 -> xs@32", "xs@32 -> o.p of xs@25", "xs@32 -> o.p@25", "o@11 -> o.p@25", "o.p@25 -> o.p@25",
			"o@11 -> return o;@41", "o.p@25 -> return o;@41",
		},
	},
	{
		// ECMA-262 §13.3.8.1 ArgumentListEvaluation (arguments left to right)
		// and §13.14 Conditional Operator. The condition c is read by its
		// Branch c@28 alone, so it is no earlier read of the statement when c
		// = x@39 overwrites c. Nodes: c@11, x@14, the Branch c@28, its arms
		// 1@32 and 2@36, each defining the conditional's result, c = x@39
		// (Uses x, defines c), return g(c ? 1 : 2, c = x);@19 (Uses the result
		// and c).
		name:     "a read the condition carried is not an earlier read of the statement",
		protects: "a definition later in a statement takes over only the reads the statement still holds, never one a condition's Branch carried",
		mutation: "keep a read another node carried as one of the statement's reads (c@11 -> c = x@39 appears)",
		src:      "function f(c, x) { return g(c ? 1 : 2, c = x); }",
		fn:       1,
		cd:       []string{"c@28 -> 1@32", "c@28 -> 2@36"},
		du: []string{
			"c@11 -> c@28", "x@14 -> c = x@39", "1@32 -> return g(c ? 1 : 2, c = x);@19",
			"2@36 -> return g(c ? 1 : 2, c = x);@19", "c = x@39 -> return g(c ? 1 : 2, c = x);@19",
		},
	},
	{
		// ECMA-262 §13.3.9 Optional Chains: a nullish receiver makes the whole
		// chain undefined. Nodes: o@11; the Branch o@26 (Uses o) defines the
		// chain's result on its nullish exit and hands the receiver on; the
		// chain o?.p@26 (Uses it, defines the result); the declarator v =
		// o?.p@22 (Uses the result, reached from both); return v;@32.
		name:     "an optional chain's value reaches its consumer from both exits",
		protects: "the chain's result is defined on the nullish exit by the Branch and on the other by the chain's node, so its consumer depends on both",
		mutation: "define the chain's result only on the chain's node (o@26 -> v = o?.p@22 vanishes), or give the chain node the receiver's reads (o@11 -> o?.p@26 appears)",
		src:      "function f(o) { const v = o?.p; return v; }",
		fn:       1,
		cd:       []string{"o@26 -> o?.p@26"},
		du: []string{
			"o@11 -> o@26", "o@26 -> o?.p@26", "o@26 -> v = o?.p@22", "o?.p@26 -> v = o?.p@22",
			"v = o?.p@22 -> return v;@32",
		},
	},
	{
		// ECMA-262 §13.15.2 Assignment Operators: an embedded property write
		// stores (PutValue) and its value is the value assigned. Nodes: o@11,
		// x@14, the write o.p = x@21 (Uses o and x, may-defines o, defines its
		// result), the call g(o.p = x)@19 (Uses the result), return o;@31,
		// reached by the write's may-definition and, through it, by o@11.
		name:     "an embedded property write is its own node and may-defines its base",
		protects: "a property write inside a larger expression is a node of its own that may-defines its base variable and hands its value to the consumer through a result",
		mutation: "fold an embedded property write into its consumer (o.p = x@21 vanishes; o@11 and x@14 pair with g(o.p = x)@19), or drop the base may-definition (o.p = x@21 -> return o;@31 vanishes)",
		src:      "function f(o, x) { g(o.p = x); return o; }",
		fn:       1,
		du: []string{
			"o@11 -> o.p = x@21", "x@14 -> o.p = x@21", "o.p = x@21 -> g(o.p = x)@19", "o@11 -> return o;@31",
			"o.p = x@21 -> return o;@31",
		},
	},
	{
		// ECMA-262 §13.15.2 Assignment Operators (an assignment's value is the
		// value assigned) and §13.15.4 EvaluateStringOrNumericBinaryExpression
		// (both operands are evaluated before the operator). Nodes: x@11,
		// x = 1@24 and x = 2@34 (each defines x and its own result), return
		// (x = 1) + (x = 2);@16, which Uses both results.
		name:     "two assignments to one variable in an expression each hand on their own value",
		protects: "an embedded assignment hands its value to the consumer through a result of its own, so a later assignment to the same variable in the expression does not hide the earlier value",
		mutation: "let the consumer read the assigned variable instead of the assignment's result (x = 1@24 -> return (x = 1) + (x = 2);@16 vanishes)",
		src:      "function f(x) { return (x = 1) + (x = 2); }",
		fn:       1,
		du:       []string{"x = 1@24 -> return (x = 1) + (x = 2);@16", "x = 2@34 -> return (x = 1) + (x = 2);@16"},
	},
	{
		// ECMA-262 §14.11.2 The with Statement, Runtime Semantics: Evaluation:
		// the object is evaluated, then the body runs in an object
		// environment. Nodes: o@11, x@14, the object o@25 (Uses o), g(x)@30
		// (Uses the parameter x: a name's resolution to one of the object's
		// properties is given up), return x;@38.
		name:     "a with statement evaluates its object and its body reads the variables it names",
		protects: "a with statement's object is a node of its own that reads it, and a name in the body is the variable it names",
		mutation: "drop the with statement's object node (o@11 -> o@25 vanishes), or bind the body's names to nothing inside a with (x@14 -> g(x)@30 vanishes)",
		src:      "function f(o, x) { with (o) { g(x); } return x; }",
		fn:       1,
		du:       []string{"o@11 -> o@25", "x@14 -> g(x)@30", "x@14 -> return x;@38"},
	},
	{
		// ECMA-262 §13.13 Binary Logical Operators (the right operand is
		// evaluated only when c is truthy) and §13.15.4
		// EvaluateStringOrNumericBinaryExpression (x is read before the right
		// operand). Nodes: c@11, x@14; the Branch c@34 (Uses c) defines the
		// operator's result; x = 1@40, on the true path only, defines x and
		// the result; the declarator y = x + (c && (x = 1))@25 Uses x, still
		// reached by x@14 on the false path, and the result; return y;@49. x =
		// 1 depends on c@34.
		name:     "an assignment in a short-circuit operand leaves an earlier read on its consumer",
		protects: "a definition inside a conditionally evaluated operand takes over no read made before the operand, so the consumer keeps the earlier definition on the path that skips the assignment",
		mutation: "hand the earlier read off to the assignment unconditionally (x@14 -> y = x + (c && (x = 1))@25 vanishes and x@14 -> x = 1@40 appears)",
		src:      "function f(c, x) { const y = x + (c && (x = 1)); return y; }",
		fn:       1,
		cd:       []string{"c@34 -> x = 1@40"},
		du: []string{
			"c@11 -> c@34", "x@14 -> y = x + (c && (x = 1))@25", "x = 1@40 -> y = x + (c && (x = 1))@25",
			"c@34 -> y = x + (c && (x = 1))@25", "y = x + (c && (x = 1))@25 -> return y;@49",
		},
	},
	{
		// ECMA-262 §9.4.2 ResolveBinding and §9.1.2.1
		// GetIdentifierReference: f declares no g, so every g resolves
		// past f's environments to the global one (in sloppy code an
		// assignment to an unresolvable reference creates a global
		// property, §6.2.5.6 PutValue, and `delete g` is allowed,
		// §13.5.1.1), never to a variable of f; h is free too. Nodes: a@11;
		// g = a@16, g += a@23 and g++@31 (each Uses its reads, defines
		// nothing); the Branch g@36 and the write g ??= a@36 (Uses a); the
		// embedded g = a@47 (Uses a, defines its result) and h(g = a)@45
		// (Uses the result); g.p = a@55 (Uses a, may-defines no base); the
		// right side a@74 (Uses a, defines the incoming value), the
		// default's Branch g = a@65 (Uses it) and the default a@69 (Uses
		// a); the iterated a@87 (Uses a, defines the iteration variable),
		// the head g of a@82 and the binding g@82 (each Uses it), delete
		// g@90 (Uses nothing); the arrow () => g += a@110 (Uses its
		// capture a, may-defines nothing, defines its result), the
		// declarator c = () => g += a@106 (Uses the result, defines c);
		// return c;@124.
		name:     "a name that resolves to no variable is read and written as nothing in every position",
		protects: "an unresolved name as an assignment, compound, update, logical, embedded, property-base, destructuring-default, iteration, deletion or capture target defines and reads nothing, while its node keeps its other reads",
		mutation: "drop the v < 0 return in def (flow.Builder.Def panics on g's -1 at g = a@16), in read (seen[-1] panics at g += a@23), in mayDefBase (MayDef(-1) at g.p = a@55) or in capTarget (MayDef(-1) on the arrow's node)",
		src:      "function f(a) { g = a; g += a; g++; g ??= a; h(g = a); g.p = a; [g = a] = a; for (g of a) delete g; const c = () => g += a; return c; }",
		fn:       1,
		cd: []string{
			"g@36 -> g ??= a@36", "g = a@65 -> a@69",
			"g of a@82 -> g of a@82", "g of a@82 -> g@82", "g of a@82 -> delete g@90",
		},
		du: []string{
			"a@11 -> g = a@16", "a@11 -> g += a@23", "a@11 -> g ??= a@36", "a@11 -> g = a@47",
			"g = a@47 -> h(g = a)@45", "a@11 -> g.p = a@55", "a@11 -> a@74", "a@74 -> g = a@65",
			"a@11 -> a@69", "a@11 -> a@87", "a@87 -> g of a@82", "a@87 -> g@82",
			"a@11 -> () => g += a@110", "() => g += a@110 -> c = () => g += a@106",
			"c = () => g += a@106 -> return c;@124",
		},
	},
	{
		// ECMA-262 §10.2.11 FunctionDeclarationInstantiation and §14.2.2
		// BlockDeclarationInstantiation: a function declaration in a block is
		// bound in that block. The Annex B.3.2.1 var binding sloppy code adds is
		// given up (lowerJavaScript, the implicit-block rule), so the g after the
		// block names no variable of f. Nodes: c@11, the Branch c@20, the
		// hoisted g@34 (defines the block's g), the declaration function g() {
		// return c }@25 (Uses its capture c, may-defines the block's g, so it
		// pairs with g@34), return g;@53 (Uses nothing). return post-dominates
		// the Branch, which controls the block's two nodes.
		name:     "a function declaration in a block binds no function-scoped name",
		protects: "a block's function declaration binds its name in the block only, so a use after the block does not pair with the declaration",
		mutation: "bind a block's function declarations in the function scope as well (function g() { return c }@25 -> return g;@53 and g@34 -> return g;@53 appear)",
		src:      "function f(c) { if (c) { function g() { return c } } return g; }",
		fn:       1,
		cd:       []string{"c@20 -> g@34", "c@20 -> function g() { return c }@25"},
		du: []string{
			"c@11 -> c@20", "c@11 -> function g() { return c }@25", "g@34 -> function g() { return c }@25",
		},
	},
	{
		// ECMA-262 §14.3.2 Variable Statement and §10.2.11
		// FunctionDeclarationInstantiation step 27 (a var of a parameter's name
		// is the parameter's binding): `var x;` without an initializer
		// evaluates nothing. Nodes: x@11, return x;@23.
		name:     "a var declaration without an initializer makes no node",
		protects: "`var x;` is a hoisted declaration only, merged into the parameter of its name, and kills nothing",
		mutation: "make `var x;` a defining node like `let x;` (x@20 -> return x;@23 replaces x@11 -> return x;@23)",
		src:      "function f(x) { var x; return x; }",
		fn:       1,
		du:       []string{"x@11 -> return x;@23"},
	},
	{
		// ECMA-262 §14.3.1.2 LexicalBinding : BindingIdentifier (initialized to
		// undefined) and §14.6 The if Statement. Nodes: c@11, x@20 (defines x),
		// the Branch c@27, x = 1@30 (defines x), return x;@37, reached by x@20
		// on the false path and by x = 1 on the true one.
		name:     "a let declaration without an initializer defines its name",
		protects: "`let x;` is a defining node, so the path that skips a later assignment carries it to the use",
		mutation: "make `let x;` no node like `var x;` (x@20 -> return x;@37 vanishes)",
		src:      "function f(c) { let x; if (c) x = 1; return x; }",
		fn:       1,
		cd:       []string{"c@27 -> x = 1@30"},
		du:       []string{"c@11 -> c@27", "x@20 -> return x;@37", "x = 1@30 -> return x;@37"},
	},
	{
		// ECMA-262 §15.7.15 BindingClassDeclarationEvaluation: the declaration
		// creates the class and initializes its name. Nodes: class C {}@15
		// (defines C), return C;@26.
		name:     "a class declaration defines its name",
		protects: "the class declaration's node is the definition a later read of the class name pairs with",
		mutation: "drop the definition of the class's name at its node (class C {}@15 -> return C;@26 vanishes)",
		src:      "function f() { class C {} return C; }",
		fn:       1,
		du:       []string{"class C {}@15 -> return C;@26"},
	},
	{
		// ECMA-262 §14.12.3 CaseClauseIsSelected: the test's own expression is
		// evaluated and compared with the discriminant's value. Nodes: x@11,
		// y@14, the discriminant x@27 (Uses x, defines its value), the Branch
		// y@37 (Uses the value and y), g()@40; no default, so the Branch's false
		// edge leaves the switch.
		name:     "a case test reads its own expression and the discriminant's value",
		protects: "a case test's Branch Uses the variables its expression reads besides the discriminant's value",
		mutation: "give a case test only the discriminant's value (y@14 -> y@37 vanishes)",
		src:      "function f(x, y) { switch (x) { case y: g(); } }",
		fn:       1,
		cd:       []string{"y@37 -> g()@40"},
		du:       []string{"x@11 -> x@27", "x@27 -> y@37", "y@14 -> y@37"},
	},
	{
		// ECMA-262 §13.15.2 (AssignmentExpression : LeftHandSideExpression
		// AssignmentOperator AssignmentExpression): the target's value is read,
		// then the result is stored. Nodes: v@11, s = 0@20, s += v@27 (Uses s
		// and v, kills s), return s;@35.
		name:     "a compound assignment to a local reads and kills it",
		protects: "a compound assignment's node Uses its target's old value and is the killing definition later uses pair with",
		mutation: "lower a compound assignment to a local as a plain one (s = 0@20 -> s += v@27 vanishes), or as a may-definition (s = 0@20 -> return s;@35 appears)",
		src:      "function f(v) { let s = 0; s += v; return s; }",
		fn:       1,
		du:       []string{"v@11 -> s += v@27", "s = 0@20 -> s += v@27", "s += v@27 -> return s;@35"},
	},
	{
		// ECMA-262 §13.4.2 Postfix Increment Operator: the property is read and
		// written through the reference. Nodes: o@11, o.n++@16 (Uses o,
		// may-defines it, so it pairs with o@11), return o;@23, reached by the
		// may-definition and, through it, by o@11.
		name:     "a property update may-defines its base",
		protects: "an update of a property is one node that reads its base and may-defines it",
		mutation: "kill the base at a property update (o@11 -> return o;@23 vanishes), or define nothing there (o.n++@16 -> return o;@23 vanishes)",
		src:      "function f(o) { o.n++; return o; }",
		fn:       1,
		du:       []string{"o@11 -> o.n++@16", "o@11 -> return o;@23", "o.n++@16 -> return o;@23"},
	},
	{
		// ECMA-262 §10.2.11 FunctionDeclarationInstantiation
		// (IteratorBindingInitialization of the formals): each bound name of a
		// parameter pattern is defined from the argument, which no variable of
		// f holds. Nodes: a@13 and b@16 (each Uses nothing, defines its name),
		// return a + b;@23.
		name:     "a destructured parameter defines each bound name from the argument",
		protects: "a parameter pattern makes one defining node per bound name, and its incoming value reads no variable",
		mutation: "bind only an identifier parameter (a@13 and b@16 vanish with their pairs)",
		src:      "function f({ a, b }) { return a + b; }",
		fn:       1,
		du:       []string{"a@13 -> return a + b;@23", "b@16 -> return a + b;@23"},
	},
	{
		// ECMA-262 §10.2.11 FunctionDeclarationInstantiation: a default is
		// evaluated only for an undefined argument, in the parameters' scope.
		// Nodes: a@11; the default's Branch b = a@14 (defines b from the
		// argument, Uses nothing); a@18 (Uses a, defines b from the default);
		// return b;@23, which post-dominates both.
		name:     "a parameter default is a conditional definition",
		protects: "a JavaScript assignment_pattern parameter is a Branch defining the name from the argument and a default node defining it from the default",
		mutation: "bind a parameter default unconditionally (the Branch b = a@14 vanishes with its control of a@18)",
		src:      "function f(a, b = a) { return b; }",
		fn:       1,
		cd:       []string{"b = a@14 -> a@18"},
		du:       []string{"a@11 -> a@18", "b = a@14 -> return b;@23", "a@18 -> return b;@23"},
	},
	{
		// ECMA-262 §13.16 Comma Operator: both operands are evaluated, the value
		// is the last. A comma in value position is folded, so return (g(x),
		// x);@16 Uses x once, as its own evaluation.
		name:     "a comma sequence in value position is folded into its consumer",
		protects: "a comma sequence that is not a statement makes no node of its own",
		mutation: "lower a value-position sequence as one statement per element (g(x)@24 appears as a node reading x)",
		src:      "function f(x) { return (g(x), x); }",
		fn:       1,
		du:       []string{"x@11 -> return (g(x), x);@16"},
	},
	{
		// ECMA-262 §13.2.9 The Grouping Operator: parentheses evaluate their
		// expression. lower.go (Spans) strips every enclosing pair, the if's
		// own included, so the Branch spans c@22.
		name:     "a condition's span strips every nested pair of parentheses",
		protects: "a Branch spans its condition with all enclosing parentheses removed, however deeply they nest",
		mutation: "strip only the statement's own parentheses (the Branch renders as ((c))@20)",
		src:      "function f(c) { if (((c))) g(); }",
		fn:       1,
		cd:       []string{"c@22 -> g()@27"},
		du:       []string{"c@11 -> c@22"},
	},
	{
		// ECMA-262 §14.3.2 Variable Statement and §10.2.11 VarDeclaredNames: a
		// var in a block is function-scoped. Nodes: c@11, the Branch c@20, x =
		// 1@29 (defines x), return x;@38, which reads the same x.
		name:     "a var in a block is the function's variable",
		protects: "var is hoisted to the function scope, so a use after the block pairs with its definition",
		mutation: "bind var in its block like let (x = 1@29 -> return x;@38 vanishes)",
		src:      "function f(c) { if (c) { var x = 1; } return x; }",
		fn:       1,
		cd:       []string{"c@20 -> x = 1@29"},
		du:       []string{"c@11 -> c@20", "x = 1@29 -> return x;@38"},
	},
	{
		// ECMA-262 §14.2.2 BlockDeclarationInstantiation: a const in a block
		// shadows the parameter from the block's start. Nodes: a@11, a = 1@24
		// (defines the block's a), g(a)@31 (Uses it), return a;@39 (Uses the
		// parameter).
		name:     "a block-scoped declaration shadows the enclosing name inside its block",
		protects: "let and const bind in their block, so the block's reads take the inner variable and the reads after it the outer one",
		mutation: "bind const in the function scope (a = 1@24 -> return a;@39 replaces a@11 -> return a;@39)",
		src:      "function f(a) { { const a = 1; g(a); } return a; }",
		fn:       1,
		du:       []string{"a = 1@24 -> g(a)@31", "a@11 -> return a;@39"},
	},
	{
		// ECMA-262 §14.15.2 CatchClauseEvaluation: the catch parameter is bound
		// in the clause's own environment, and §13.3.6 (a call may throw).
		// Nodes: e@11, g()@22 (may throw), the Handler catch@29, the catch
		// parameter e@36, h(e)@41 (Uses the clause's e), return e;@49 (Uses the
		// parameter). g() leaves to the Handler or to the return, which
		// post-dominates it, so it controls the clause's three nodes.
		name:     "a catch parameter is scoped to its clause",
		protects: "the catch parameter shadows an enclosing name inside the clause only",
		mutation: "bind the catch parameter in the enclosing scope (e@36 -> return e;@49 appears)",
		src:      "function f(e) { try { g(); } catch (e) { h(e); } return e; }",
		fn:       1,
		cd:       []string{"g()@22 -> catch@29", "g()@22 -> e@36", "g()@22 -> h(e)@41"},
		du:       []string{"e@36 -> h(e)@41", "e@11 -> return e;@49"},
	},
	{
		// ECMA-262 §14.7.4.2 ForLoopEvaluation: a `for (let …)` binding is
		// scoped to the loop. Nodes: i@11, i = 0@25 (defines the loop's i), the
		// Branch i < 2@32, g(i)@44, the update i++@39 (Uses and kills the
		// loop's i), whose back edge re-enters the Branch; return i;@50 on the
		// exit edge reads the parameter. The Branch controls the body, the
		// update and itself.
		name:     "a for-let binding is scoped to its loop",
		protects: "the loop's let binding shadows the parameter inside the loop only, and the update's definition reaches the next iteration's condition, body and update",
		mutation: "bind a for loop's let in the function scope (i++@39 -> return i;@50 and i = 0@25 -> return i;@50 replace i@11 -> return i;@50)",
		src:      "function f(i) { for (let i = 0; i < 2; i++) g(i); return i; }",
		fn:       1,
		cd:       []string{"i < 2@32 -> i < 2@32", "i < 2@32 -> g(i)@44", "i < 2@32 -> i++@39"},
		du: []string{
			"i = 0@25 -> i < 2@32", "i = 0@25 -> g(i)@44", "i = 0@25 -> i++@39",
			"i++@39 -> i < 2@32", "i++@39 -> g(i)@44", "i++@39 -> i++@39", "i@11 -> return i;@50",
		},
	},
}

// javascriptJSX holds the JSX cases, which run under the javascript and tsx
// grammars; the typescript grammar has no JSX.
var javascriptJSX = []goldenCase{
	{
		// The JSX specification: `<B />` is a call whose first argument is its
		// tag, so a capitalized tag name is a read; ECMA-262 §13.13 Binary
		// Logical Operators. Nodes: a@11, B@14, the Branch a@26 (Uses a,
		// defines the result), <B />@31 (Uses B, defines it), the return@19,
		// which Uses the result.
		name:     "a JSX element reads its component and is a conditional operand",
		protects: "a capitalized JSX tag is a read, and a JSX operand of && is a node defining the operator's result",
		mutation: "treat every JSX tag as intrinsic (B@14 -> <B />@31 vanishes), or give the return the operands' reads (a@11 -> return a && <B />;@19 appears)",
		src:      "function f(a, B) { return a && <B />; }",
		fn:       1,
		cd:       []string{"a@26 -> <B />@31"},
		du: []string{
			"a@11 -> a@26", "B@14 -> <B />@31", "a@26 -> return a && <B />;@19", "<B />@31 -> return a && <B />;@19",
		},
	},
	{
		// The JSX specification (intrinsic elements): a lowercase tag names a
		// host element, emitted as the string "div", never the variable div.
		// The return@18 reads nothing, so there is no pair.
		name:     "an intrinsic JSX tag reads no variable",
		protects: "a lowercase JSX tag name is not a read of a variable of that name",
		mutation: "read every identifier tag (div@11 -> return <div />;@18 appears)",
		src:      "function f(div) { return <div />; }",
		fn:       1,
	},
	{
		// The JSX specification: a member-expression tag is evaluated, and
		// attribute values and child expressions are read; the closing tag
		// repeats the name and evaluates nothing. Nodes: M@11, a@14, b@17,
		// the return@22, which Uses M, a and b.
		name:     "a JSX member tag, its attributes and its children are read",
		protects: "a member-expression tag reads its object, and attribute and child expressions are reads of the element",
		mutation: "skip every tag that is not a capitalized identifier (M@11 -> the return vanishes), or skip attributes or children (a@14 or b@17 -> the return vanishes)",
		src:      "function f(M, a, b) { return <M.C p={a}>{b}</M.C>; }",
		fn:       1,
		du: []string{
			"M@11 -> return <M.C p={a}>{b}</M.C>;@22", "a@14 -> return <M.C p={a}>{b}</M.C>;@22",
			"b@17 -> return <M.C p={a}>{b}</M.C>;@22",
		},
	},
}
