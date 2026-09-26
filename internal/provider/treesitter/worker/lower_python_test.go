package worker

import "testing"

// TestPythonLoweringGolden pins the Python lowering's control-dependence and
// def-use pairs on hand-derived functions. A wrong pair here is a wrong
// dependence fact served to every consumer. Callable 0 is the module; a
// function defined at the top is callable 1, and a lambda or comprehension
// inside it callable 2. Each source is indented by one space per level, and
// each case's comment gives the byte offset every line starts at.
//
// Derivation rules, as in the other goldens: augmentation adds an edge to
// EXIT from every node with no successor and from the smallest-reverse-
// post-order member of each sink strongly connected component that cannot
// reach EXIT; control dependence is the post-dominance frontier over the
// augmented graph with no entry-to-exit edge; a def-use pair is (defining
// node, using node) with every φ resolved; a may-definition kills nothing; a
// Handler node carries the values on entry to each node that threw to it.
// Every case opens with the section it turns on; section numbers are those
// of The Python Language Reference, version 3.13.
func TestPythonLoweringGolden(t *testing.T) {
	runGolden(t, "python", []goldenCase{
		{
			// §8.1. Lines at 0, 13, 20, 28, 37, 45, 52, 60. Nodes: a@6,
			// b@9 (params), a@17 (if), x = 1@22, b@34 (elif, reached only
			// from a's false edge), x = 2@39, x = 3@54, return x@61. Every
			// arm flows to return x, the IPDom of both conditions.
			name:     "an elif chain tests each condition only when the ones before it failed",
			protects: "an elif condition hangs off the previous condition's false edge, and every arm's definition reaches the join",
			mutation: "lower elif as an independent if after the chain (b@34 loses its control dependence on a@17, and x = 1@22 stops reaching return x)",
			src:      "def f(a, b):\n if a:\n  x = 1\n elif b:\n  x = 2\n else:\n  x = 3\n return x\n",
			fn:       1,
			cd:       []string{"a@17 -> x = 1@22", "a@17 -> b@34", "b@34 -> x = 2@39", "b@34 -> x = 3@54"},
			du: []string{"a@6 -> a@17", "b@9 -> b@34",
				"x = 1@22 -> return x@61", "x = 2@39 -> return x@61", "x = 3@54 -> return x@61"},
		},
		{
			// §8.2. Lines at 0, 10, 17, 31, 42, 51, 60, 67, 76. Nodes: n@6,
			// i = 0@11, i < n@24 (head), g(i)@36, break@45, i += 1@53,
			// i = -1@69 (else), return i@77. Succ: head→{g(i), i = -1};
			// g(i)→{break, i += 1}; break→return i; i += 1→head;
			// i = -1→return i. IPDom: head, g(i), break, i = -1 → return i;
			// i += 1 → head. Frontier walks: head over g(i) and i = -1;
			// g(i) over break, i += 1 and head.
			name:     "a while loop's else runs from the head's false edge and a break skips it",
			protects: "the else body is on the head's false path only, so the value defined before a break reaches the code after the loop",
			mutation: "lower the else body from the breaks too (break@45 then flows into i = -1@69, killing i = 0@11 and i += 1@53 at return i@77)",
			src:      "def f(n):\n i = 0\n while i < n:\n  if g(i):\n   break\n  i += 1\n else:\n  i = -1\n return i\n",
			fn:       1,
			cd: []string{"i < n@24 -> g(i)@36", "i < n@24 -> i = -1@69",
				"g(i)@36 -> break@45", "g(i)@36 -> i += 1@53", "g(i)@36 -> i < n@24"},
			du: []string{"n@6 -> i < n@24",
				"i = 0@11 -> i < n@24", "i = 0@11 -> g(i)@36", "i = 0@11 -> i += 1@53", "i = 0@11 -> return i@77",
				"i += 1@53 -> i < n@24", "i += 1@53 -> g(i)@36", "i += 1@53 -> i += 1@53", "i += 1@53 -> return i@77",
				"i = -1@69 -> return i@77"},
		},
		{
			// §8.3: the iterable is evaluated once, then each item is
			// assigned to the target. Lines at 0, 11, 25, 33, 42, 49, 57.
			// Nodes: xs@6, xs@21 (the iterable: Uses xs, defines the
			// iteration variable), x in xs@16 (head, Using only the
			// iteration variable), x@16 (the target, on the body path, Using
			// the iteration variable), x@30 (if), break@36, x = 0@51 (else),
			// return x@58. Succ: head→{x@16, x = 0}; x@16→x@30; x@30→{break,
			// head}; break→return x; x = 0→return x. IPDom: x@16 → x@30;
			// x@30, head, x = 0 → return x.
			name:     "a for loop's target is defined on the body path and its else on the exhausted path",
			protects: "the head defines nothing, the head and the target read the iteration variable the iterable's node defines, not the iterable's names, and a break carries the target's value past the else",
			mutation: "define the target on the head (x = 0@51 no longer kills it and x@16 -> x@30 becomes x in xs@16 -> x@30), or make the head and the target Use the iterable's reads (xs@6 -> x in xs@16 and xs@6 -> x@16 appear), or run the else after a break",
			src:      "def f(xs):\n for x in xs:\n  if x:\n   break\n else:\n  x = 0\n return x\n",
			fn:       1,
			cd: []string{"x in xs@16 -> x@16", "x in xs@16 -> x@30", "x in xs@16 -> x = 0@51",
				"x@30 -> break@36", "x@30 -> x in xs@16"},
			du: []string{"xs@6 -> xs@21", "xs@21 -> x in xs@16", "xs@21 -> x@16",
				"x@16 -> x@30", "x@16 -> return x@58", "x = 0@51 -> return x@58"},
		},
		{
			// §8.3: the iterable is evaluated once and an iterator created
			// for it, so a body that changes d does not change what the head
			// steps. Lines at 0, 10, 23, 37. Nodes: d@6, d@20 (the iterable:
			// Uses d, defines the iteration variable), k in d@15 (head,
			// Using only the iteration variable), k@15 (the target, Using
			// the iteration variable), d[k] = g(k)@25 (Uses k and d,
			// may-defines d, its base variable, §7.2), return d@38. Succ:
			// d@20→head→{k@15, return d}; k@15→d[k] = g(k)→head. IPDom:
			// k@15 → d[k] = g(k) → head → return d. The subscript write's
			// may-definition kills nothing, so d@6 and it both reach it
			// around the loop and reach return d.
			name:     "a body that writes through the iterated name does not reach the loop head",
			protects: "the head reads the iterator made once from d, so a subscript write to d in the body, a may-definition of d, pairs with the later reads of d and never with the head",
			mutation: "make the head Use the iterable's reads (d@6 -> k in d@15 and d[k] = g(k)@25 -> k in d@15 appear)",
			src:      "def f(d):\n for k in d:\n  d[k] = g(k)\n return d\n",
			fn:       1,
			cd:       []string{"k in d@15 -> k@15", "k in d@15 -> d[k] = g(k)@25", "k in d@15 -> k in d@15"},
			du: []string{"d@6 -> d@20", "d@20 -> k in d@15", "d@20 -> k@15", "k@15 -> d[k] = g(k)@25",
				"d@6 -> d[k] = g(k)@25", "d[k] = g(k)@25 -> d[k] = g(k)@25",
				"d@6 -> return d@38", "d[k] = g(k)@25 -> return d@38"},
		},
		{
			// §8.2. Lines at 0, 9, 22, 32, 41. Nodes: True@16 (a Stmt head
			// with no false edge), g()@27, break@35, h()@42. Succ:
			// True→g(); g()→{break, True}; break→h(). IPDom: True → g() →
			// break → h(). g()'s walk from True passes True and g() itself.
			name:     "while True leaves only by break",
			protects: "`while True` has no exit edge, so the code after it is reached only through the break",
			mutation: "give the True head a false edge (g()@27 no longer post-dominates True@16, and True@16 becomes a controller of g()@27 and break@35)",
			src:      "def f():\n while True:\n  if g():\n   break\n h()\n",
			fn:       1,
			cd:       []string{"g()@27 -> True@16", "g()@27 -> g()@27"},
		},
		{
			// §8.4.1. Lines at 0, 10, 16, 23, 34, 42, 53, 61. Nodes: a@6,
			// g(a)@18 (may throw), except@24 (Handler), E@31, a = 1@36,
			// F@50, a = 2@55, return a@62. Succ: g(a)→{return a, except};
			// except→E→{a = 1, F}; F→{a = 2, EXIT (the re-raise)}; both
			// arms → return a. IPDom: g(a), E, F → EXIT; except → E.
			name:     "an exception no typed clause matches is re-raised from the last test",
			protects: "the typed clauses are tested in order and the last test's false edge leaves the function, so return a depends on every test",
			mutation: "drop the Throw at the end of a clause chain without a bare except (F@50 then falls into return a@62 and controls nothing but a = 2@55)",
			src:      "def f(a):\n try:\n  g(a)\n except E:\n  a = 1\n except F:\n  a = 2\n return a\n",
			fn:       1,
			cd: []string{"g(a)@18 -> return a@62", "g(a)@18 -> except@24", "g(a)@18 -> E@31",
				"E@31 -> a = 1@36", "E@31 -> return a@62", "E@31 -> F@50",
				"F@50 -> a = 2@55", "F@50 -> return a@62"},
			du: []string{"a@6 -> g(a)@18", "a@6 -> return a@62", "a = 1@36 -> return a@62", "a = 2@55 -> return a@62"},
		},
		{
			// §8.4.1. Lines at 0, 10, 16, 22, 33, 41, 50, 58. Nodes: a@6,
			// g()@18, except@23 (Handler), E@30, a = 1@35, a = 2@52 (the
			// bare clause, no test), return a@59. E→{a = 1, a = 2}; every
			// path reaches return a, the IPDom of g() and E.
			name:     "a bare except ends the clause chain",
			protects: "a bare except catches whatever the typed clauses before it did not, so nothing is re-raised and the code after the try post-dominates the handler",
			mutation: "treat a bare except as a typed clause (its body gains a test and a re-raise, and return a@59 becomes control dependent on the handler)",
			src:      "def f(a):\n try:\n  g()\n except E:\n  a = 1\n except:\n  a = 2\n return a\n",
			fn:       1,
			cd:       []string{"g()@18 -> except@23", "g()@18 -> E@30", "E@30 -> a = 1@35", "E@30 -> a = 2@52"},
			du:       []string{"a@6 -> return a@59", "a = 1@35 -> return a@59", "a = 2@52 -> return a@59"},
		},
		{
			// §8.4.3, §8.4.4. Lines at 0, 10, 16, 26, 37, 48, 55, 67, 77, 84. Nodes:
			// a@6, a = g()@18 (may throw), except@27 (Handler), E@34,
			// return 0@39, a = a + 1@57 (else), h(a)@79 (finally; no
			// finally Handler: nothing throws to it part-way), return a@85.
			// Succ: a = g()→{except, a = a + 1}; except→E→{return 0, h(a)
			// (the re-raise, intercepted)}; return 0, a = a + 1 → h(a);
			// h(a)→{EXIT (re-issued return and raise), return a}. IPDom:
			// a = g(), E → h(a) → EXIT. The Handler carries a@6.
			name:     "try else runs only on the body's normal end and every exit passes the finally",
			protects: "the else body is control dependent on the try body's completion, and a handler's return and an unmatched exception both run the finally with the values they carry",
			mutation: "lower the else body after the handler exits merge (a = a + 1@57 then follows return 0 and loses its dependence on a = g()@18), or skip the finally for the return (return 0@39 leaves straight to EXIT, h(a)@79 no longer post-dominates E@34 or a = g()@18, and E@34 -> h(a)@79 and a = g()@18 -> h(a)@79 appear)",
			src:      "def f(a):\n try:\n  a = g()\n except E:\n  return 0\n else:\n  a = a + 1\n finally:\n  h(a)\n return a\n",
			fn:       1,
			cd: []string{"a = g()@18 -> except@27", "a = g()@18 -> E@34", "a = g()@18 -> a = a + 1@57",
				"E@34 -> return 0@39", "h(a)@79 -> return a@85"},
			du: []string{"a = g()@18 -> a = a + 1@57", "a@6 -> h(a)@79", "a = a + 1@57 -> h(a)@79",
				"a@6 -> return a@85", "a = a + 1@57 -> return a@85"},
		},
		{
			// §8.5. Lines at 0, 13, 27, 35, 47, 57. Nodes: m@6, c@9, m as
			// r@19 (enter, defines r and may-defines the manager), c@32,
			// return r@38, raise E@49, with m as r@14 (the exit, reading the
			// manager), return 0@58. Both jumps are intercepted by the with's
			// finally and re-issued from the exit to EXIT; the raise reached
			// it, so the exit also flows on, since __exit__ may suppress.
			// Succ: c@32→{return r, raise E}; both → exit; exit→{EXIT,
			// return 0}. IPDom: c@32 → exit → EXIT.
			name:     "a with statement's exit runs on return and on raise, and may suppress",
			protects: "a return and a raise inside a with body both reach the exit node, and the code after the with stays reachable from the exit",
			mutation: "close the with's finally as abnormal when only a raise reached it (return 0@58 becomes unreachable and the exit controls nothing), or skip the exit for a return (return r@38 goes straight to EXIT)",
			src:      "def f(m, c):\n with m as r:\n  if c:\n   return r\n  raise E\n return 0\n",
			fn:       1,
			cd:       []string{"c@32 -> return r@38", "c@32 -> raise E@49", "with m as r@14 -> return 0@58"},
			du: []string{"m@6 -> m as r@19", "m as r@19 -> with m as r@14", "c@9 -> c@32",
				"m as r@19 -> return r@38"},
		},
		{
			// §8.5: the items nest, so y's exit runs before x's on every
			// path. Lines at 0, 10, 36, 44, 56. Nodes: c@6, a() as x@16
			// (enter x), b() as y@26 (enter y; its call may throw, into x's
			// finally), c@41, return x@47, y's exit (from `with` to the end
			// of item 2)@11, x's finally Handler with@11 (from b() as y and
			// y's exit, which may throw), x's exit with a() as x@11, return
			// y@57. The return is intercepted by y's finally, re-issued
			// into x's, then to EXIT. Succ: b() as y→{c@41, with@11};
			// c@41→{return x, y's exit}; return x→y's exit; y's exit→{x's
			// exit, with@11}; with@11→x's exit; x's exit→{EXIT, return y}.
			// IPDom: b() as y, y's exit, with@11 → x's exit → EXIT; c@41,
			// return x → y's exit. The Handler carries the values on entry
			// to b() as y (no y) and to y's exit (y from b() as y). Each
			// exit reads the manager its enter node may-defines.
			name:     "two with items close in reverse on the return path and the normal path",
			protects: "the second item's exit runs before the first's whether the body returns or completes, and a failure entering the second item runs only the first item's exit",
			mutation: "close the items in source order (x's exit then precedes y's, b() as y@26 -> with a() as x, b() as y@11 is lost and x's exit controls y's), or open one finally for both items (b() as y no longer controls with@11)",
			src:      "def f(c):\n with a() as x, b() as y:\n  if c:\n   return x\n return y\n",
			fn:       1,
			cd: []string{"b() as y@26 -> c@41", "b() as y@26 -> with a() as x, b() as y@11", "b() as y@26 -> with@11",
				"c@41 -> return x@47", "with a() as x, b() as y@11 -> with@11", "with a() as x@11 -> return y@57"},
			du: []string{"c@6 -> c@41", "a() as x@16 -> return x@47", "b() as y@26 -> return y@57",
				"a() as x@16 -> with a() as x@11", "b() as y@26 -> with a() as x, b() as y@11"},
		},
		{
			// §8.6.2, §8.6.3. Lines at 0, 10, 20, 40, 49, 59, 68. Nodes: p@6,
			// p@17 (the subject, evaluated once: Uses p and defines the
			// subject's variable), (x, y)@27 (a Branch Using the subject's
			// variable), x@28 and y@31 (captures, Using it too), x@37 (guard), r = y@43, _@56 (irrefutable: a
			// Stmt, no false edge), r = 0@62, return r@69. Succ: (x, y)→
			// {x@28, _}; x@28→y@31→x@37→{r = y, _}; _→r = 0; both arms →
			// return r. IPDom: (x, y), x@37 → return r; x@28 → y@31 → x@37.
			name:     "a failed guard reaches the next case and the wildcard has no false edge",
			protects: "captures are defined on the taken path from the subject, a guard's false edge continues with the next case, and an irrefutable last case never falls out of the match",
			mutation: "give the wildcard case a false edge (_@56 gains control of r = 0@62), or send a failed guard past the match (x@37 loses x@37 -> _@56), or make the case and capture nodes Use the subject's reads (p@6 -> (x, y)@27, p@6 -> x@28, p@6 -> y@31 and p@6 -> _@56 replace the pairs from p@17)",
			src:      "def f(p):\n match p:\n  case (x, y) if x:\n   r = y\n  case _:\n   r = 0\n return r\n",
			fn:       1,
			cd: []string{"(x, y)@27 -> x@28", "(x, y)@27 -> y@31", "(x, y)@27 -> x@37", "(x, y)@27 -> _@56",
				"(x, y)@27 -> r = 0@62", "x@37 -> r = y@43", "x@37 -> _@56", "x@37 -> r = 0@62"},
			du: []string{"p@6 -> p@17", "p@17 -> (x, y)@27", "p@17 -> x@28", "p@17 -> y@31", "p@17 -> _@56",
				"x@28 -> x@37", "y@31 -> r = y@43", "r = y@43 -> return r@69", "r = 0@62 -> return r@69"},
		},
		{
			// §6.2.4: the leftmost for clause's iterable is evaluated in the
			// enclosing scope. Lines at 0, 14. Nodes: xs@6, k@10, the
			// comprehension node@22 (its first iterable xs is evaluated
			// here; k is a capture; x is the comprehension's own; it
			// defines the created value's result variable), return …@15,
			// which Uses that variable only.
			name:     "a comprehension's first iterable and captures are uses of its creating node",
			protects: "the enclosing function evaluates the first iterable, the comprehension's own target shadows nothing outside it, and the created value reaches the return through the creating node's result variable, not through the creating node's reads",
			mutation: "leave the first iterable to the comprehension's own graph (xs@6 no longer reaches the comprehension node), or let the return re-read the creating node's reads (xs@6 -> return …@15 and k@10 -> return …@15 replace [x + k for x in xs if x]@22 -> return …@15)",
			src:      "def f(xs, k):\n return [x + k for x in xs if x]\n",
			fn:       1,
			du: []string{"xs@6 -> [x + k for x in xs if x]@22", "k@10 -> [x + k for x in xs if x]@22",
				"[x + k for x in xs if x]@22 -> return [x + k for x in xs if x]@15"},
		},
		{
			// §6.2.4, the comprehension itself (callable 2). Nodes: for x in
			// xs@29 (head, reading nothing of its own), x@33 (target),
			// x@44 (if clause), x + k@23 (element). Succ: head→{x@33,
			// EXIT}; x@33→x@44→{x + k, head}; x + k→head. IPDom: x@33 →
			// x@44 → head → EXIT; x + k → head.
			name:     "a comprehension is its own callable with its clauses as nested loops",
			protects: "each for clause is a loop head, an if clause's false edge continues with the next element, and the element is control dependent on the if clause, which is control dependent on the head",
			mutation: "lower an if clause's false edge to the comprehension's exit (the head no longer post-dominates x@44, and x@44 -> for x in xs@29 appears)",
			src:      "def f(xs, k):\n return [x + k for x in xs if x]\n",
			fn:       2,
			cd: []string{"for x in xs@29 -> x@33", "for x in xs@29 -> x@44", "for x in xs@29 -> for x in xs@29",
				"x@44 -> x + k@23"},
			du: []string{"x@33 -> x@44", "x@33 -> x + k@23"},
		},
		{
			// §6.14. Lines at 0, 10, 31. Nodes: a@6, lambda b: a + b@15
			// (captures a; defines the created value's result variable),
			// g = lambda b: a + b@11 (Uses the result variable), return g@32.
			name:     "a lambda's creating node uses its captures",
			protects: "a lambda is its own callable and the enclosing function sees only its creation, whose node reads the enclosing variables it references and hands the created value to its consumer through a variable",
			mutation: "resolve a lambda's body against the enclosing scope as plain reads of the assignment (the lambda node loses a@6), or let the assignment re-read the captures (a@6 -> g = lambda b: a + b@11 replaces lambda b: a + b@15 -> g = lambda b: a + b@11)",
			src:      "def f(a):\n g = lambda b: a + b\n return g\n",
			fn:       1,
			du: []string{"a@6 -> lambda b: a + b@15", "lambda b: a + b@15 -> g = lambda b: a + b@11",
				"g = lambda b: a + b@11 -> return g@32"},
		},
		{
			// §6.14, the lambda itself (callable 2). Nodes: b@22 (param),
			// a + b@25 (the body, its value). a is free: no variable.
			name:     "a lambda's body is one node reading its parameters",
			protects: "a lambda lowers its parameters as definitions and its body as one value node",
			mutation: "lower a lambda's parameters as uses only (b@22 -> a + b@25 disappears)",
			src:      "def f(a):\n g = lambda b: a + b\n return g\n",
			fn:       2,
			du:       []string{"b@22 -> a + b@25"},
		},
		{
			// §7.13. Lines at 0, 9, 16, 26, 39, 48, 53. Nodes: n = 0@10,
			// def g(): …@17 (defines g, reads n, may-defines n through the
			// nonlocal), g()@49, return n@54.
			name:     "a nonlocal write is a may-definition where the function is defined",
			protects: "a nonlocal name is the enclosing variable, so a write to it in the nested function reaches a later use without killing the earlier definition",
			mutation: "treat a nonlocal name as the nested function's local (the def node loses its may-definition and its read: def g(): … -> return n@54 and n = 0@10 -> def g(): …@17 disappear)",
			src:      "def f():\n n = 0\n def g():\n  nonlocal n\n  n += 1\n g()\n return n\n",
			fn:       1,
			du: []string{"n = 0@10 -> def g(): nonlocal n n += 1@17", "n = 0@10 -> return n@54",
				"def g(): nonlocal n n += 1@17 -> return n@54", "def g(): nonlocal n n += 1@17 -> g()@49"},
		},
		{
			// §7.12. Lines at 0, 9, 19, 26. x is declared global: no
			// variable, so x = 1@20 defines nothing and return x@27 reads
			// nothing.
			name:     "a global name is not a local",
			protects: "a name declared global is never a variable of the function, however it is assigned",
			mutation: "declare global names as locals (x = 1@20 -> return x@27 appears)",
			src:      "def f():\n global x\n x = 1\n return x\n",
			fn:       1,
		},
		{
			// §4.2.2, the module (callable 0). Lines at 0, 6, 15, 21, 28.
			// Nodes: x = 1@0, def f(): …@6 (defines f), h(x)@28. f assigns
			// x, so x is f's local for its whole body, including the read
			// before the assignment: the def node captures nothing.
			name:     "a name bound anywhere in a function is local to its whole body",
			protects: "a read of a name before the function's own assignment to it is a read of the local, never of the enclosing variable",
			mutation: "bind a name only from its first assignment onward (x = 1@0 -> def f(): g(x) x = 2@6 appears)",
			src:      "x = 1\ndef f():\n g(x)\n x = 2\nh(x)\n",
			du:       []string{"x = 1@0 -> h(x)@28"},
		},
		{
			// §6.12. Lines at 0, 11, 18, 40. Nodes: xs@6, y = 0@12, the
			// comprehension node@19 (reads xs, its first iterable, and
			// may-defines y: the assignment expression writes y and never
			// reads it), return y@41. The expression statement is the
			// comprehension's node.
			name:     "an assignment expression in a comprehension binds in the enclosing function",
			protects: "`:=` inside a comprehension writes the enclosing function's variable, a may-definition at the comprehension's creation that reads nothing of it",
			mutation: "bind a `:=` target as the comprehension's own local (the comprehension node loses its may-definition, and y = 0@12 alone reaches return y@41), or record the target as a read (y = 0@12 -> [y := x for x in xs]@19 appears)",
			src:      "def f(xs):\n y = 0\n [y := x for x in xs]\n return y\n",
			fn:       1,
			du: []string{"xs@6 -> [y := x for x in xs]@19",
				"y = 0@12 -> return y@41", "[y := x for x in xs]@19 -> return y@41"},
		},
		{
			// §7.5. Lines at 0, 10, 17, 24. Nodes: a@6, b = a@11, del b@18
			// (kills b), return b@25. The use after del pairs with the del
			// node alone.
			name:     "del is a definition kill",
			protects: "a use after `del b` sees no definition from before the del",
			mutation: "lower del as a plain node (b = a@11 -> return b@25 replaces del b@18 -> return b@25)",
			src:      "def f(a):\n b = a\n del b\n return b\n",
			fn:       1,
			du:       []string{"a@6 -> b = a@11", "del b@18 -> return b@25"},
		},
		{
			// §7.3. Lines at 0, 10, 30. Nodes: a@6, a > 0@18 (Branch),
			// m(a)@25 (the message, then the raise to EXIT), return a@31.
			// IPDom: a > 0 → EXIT.
			name:     "a failed assert evaluates its message and raises",
			protects: "the message is evaluated only on the false path, which raises, so the code after the assert depends on the condition",
			mutation: "lower assert as a plain statement (a > 0@18 controls nothing)",
			src:      "def f(a):\n assert a > 0, m(a)\n return a\n",
			fn:       1,
			cd:       []string{"a > 0@18 -> m(a)@25", "a > 0@18 -> return a@31"},
			du:       []string{"a@6 -> a > 0@18", "a@6 -> m(a)@25", "a@6 -> return a@31"},
		},
		{
			// §8.4.1. Lines at 0, 9, 16, 22, 28, 44, 51. Nodes: e = 0@10,
			// g()@24 (may throw), except@29 (Handler), E@36, e@41 (the
			// binding), h(e)@46 (may throw, into the clause's finally),
			// as@38 (that finally's Handler), the deletion of e (a Stmt
			// spanning the clause)@29, the finally's body, return e@52.
			// Succ: g()→{return e, except}; except→E→{e@41, EXIT};
			// e@41→h(e)→{as, deletion}; as→deletion; deletion→{return e,
			// EXIT (h(e)'s exception, re-raised)}. IPDom: g(), E, deletion →
			// EXIT; e@41 → h(e) → deletion; as → deletion.
			name:     "an except clause's name is deleted however the clause ends",
			protects: "the exception bound by `as e` does not outlive its clause, on the normal end and on an exception leaving the body, so the code after the try sees the deletion, not the exception",
			mutation: "delete the name only at the clause's normal end (as@38 disappears, h(e)@46 controls nothing and the deletion no longer controls return e@52), or drop the deletion (e@41 -> return e@52 replaces except E as e: h(e)@29 -> return e@52)",
			src:      "def f():\n e = 0\n try:\n  g()\n except E as e:\n  h(e)\n return e\n",
			fn:       1,
			cd: []string{"g()@24 -> return e@52", "g()@24 -> except@29", "g()@24 -> E@36",
				"E@36 -> e@41", "E@36 -> h(e)@46", "E@36 -> except E as e: h(e)@29",
				"h(e)@46 -> as@38", "except E as e: h(e)@29 -> return e@52"},
			du: []string{"e@41 -> h(e)@46", "except E as e: h(e)@29 -> return e@52", "e = 0@10 -> return e@52"},
		},
		{
			// §4.2.2. Lines at 0, 10, 20, 28, 43, 55. Nodes: a@6, the class
			// node@11 (defines C), return C@56. The class body's a = 1 binds
			// the class's own a; the method's a skips the class scope and
			// is f's a, a capture of the class node.
			name:     "a class body's names are not visible to its methods",
			protects: "a method's free name resolves past the class scope to the enclosing function",
			mutation: "let a method see the class scope (a in m resolves to the class's a, and a@6 -> class C: …@11 disappears)",
			src:      "def f(a):\n class C:\n  a = 1\n  def m(self):\n   return a\n return C\n",
			fn:       1,
			du: []string{"a@6 -> class C: a = 1 def m(self): return a@11",
				"class C: a = 1 def m(self): return a@11 -> return C@56"},
		},
		{
			// §6.10: b is evaluated once, and c only when a < b holds. Lines
			// at 0, 16. Nodes: a@6, b@9, c@12, a < b@24 (Branch: Uses a and
			// b, defines the result variable, the value when it fails, and
			// may-defines a variable holding b), c@32 (evaluated only when a
			// < b holds: Uses c and the held b, makes b < c and defines the
			// result variable), return a < b < c@17 (Uses the result
			// variable). Succ: a < b→{c, return}; c→return. IPDom: a < b, c
			// → return.
			name:     "a chained comparison evaluates its later operand only when the earlier comparison holds",
			protects: "the operand after a comparison in a chain is control dependent on that comparison, the middle operand's value reaches the last comparison through a variable, and the chain's value reaches the return from both comparisons",
			mutation: "lower a chained comparison as a plain expression (a < b@24 and c@32 are not made and the control dependence disappears), or let the return re-read the operands (a@6, b@9 and c@12 -> return a < b < c@17 replace the pairs from a < b@24 and c@32)",
			src:      "def f(a, b, c):\n return a < b < c\n",
			fn:       1,
			cd:       []string{"a < b@24 -> c@32"},
			du: []string{"a@6 -> a < b@24", "b@9 -> a < b@24", "c@12 -> c@32", "a < b@24 -> c@32",
				"a < b@24 -> return a < b < c@17", "c@32 -> return a < b < c@17"},
		},
		{
			// §7.2. Lines at 0, 13, 22. Nodes: o@6, v@9, o.p = v@14 (reads
			// v and o, may-defines o), return o@23.
			name:     "an attribute write updates its base variable without killing it",
			protects: "a write to o.p reaches a later use of o, and so does o's earlier definition",
			mutation: "record no definition of the base (loses o.p = v@14 -> return o@23), or a killing one (loses o@6 -> return o@23)",
			src:      "def f(o, v):\n o.p = v\n return o\n",
			fn:       1,
			du:       []string{"v@9 -> o.p = v@14", "o@6 -> o.p = v@14", "o.p = v@14 -> return o@23", "o@6 -> return o@23"},
		},
		{
			// §8.7: the decorator expressions are evaluated when the
			// definition runs. Lines at 0, 10, 14, 24, 31. Nodes: d@6, the
			// decorated definition@11 (reads d, defines g), return g@32.
			name:     "a decorated definition's node uses its decorators' reads",
			protects: "a decorator is evaluated where the function is defined, so the variables it reads reach the definition's node",
			mutation: "take the definition's read mark after the decorators are evaluated (d@6 -> @d def g(): pass@11 disappears)",
			src:      "def f(d):\n @d\n def g():\n  pass\n return g\n",
			fn:       1,
			du:       []string{"d@6 -> @d def g(): pass@11", "@d def g(): pass@11 -> return g@32"},
		},
		{
			// §7.5. Lines at 0, 16, 24, 38. Nodes: del(a)@17 (the one
			// target, parenthesized: spans the statement, kills a), b@30
			// (an element of the list target, spanning itself, kills b),
			// (c)@34 (one of two targets, spanning itself, kills c), return
			// a, b, c@39. The parameters' definitions are all killed.
			name:     "parenthesized and list del targets kill the names they hold",
			protects: "`del(x)`, `del [x]` and `del (a), b` delete names like `del x`, rather than failing to lower the function",
			mutation: "recurse into a parenthesized target with no statement span (the lowering panics spanning nil), or treat a list target as a plain value (b@9 -> return a, b, c@39 replaces b@30 -> return a, b, c@39)",
			src:      "def f(a, b, c):\n del(a)\n del [b], (c)\n return a, b, c\n",
			fn:       1,
			du:       []string{"del(a)@17 -> return a, b, c@39", "b@30 -> return a, b, c@39", "(c)@34 -> return a, b, c@39"},
		},
		{
			// §8.3, §8.2. Lines at 0, 10, 20, 34, 43, 51, 63, 71. Nodes:
			// a@6, a@17 (while head), a@31 (the iterable: Uses a, defines
			// the iteration variable), y in a@26 (for head) and y@26
			// (target), each Using only the iteration variable, break@37, continue@54 (the for's else),
			// a = 0@65, return a@72. The for's frame is closed when its else
			// runs, so the continue targets the while. Succ: a@17→{a@31,
			// return a}; a@31→y in a→{y@26, continue}; y@26→break→a = 0→
			// a@17; continue→a@17. IPDom: a@17 → return a; a@31 → y in a →
			// a@17; y@26 → break → a = 0 → a@17; continue → a@17.
			name:     "a continue in a for loop's else continues the enclosing loop",
			protects: "a loop's else body runs after its frame closes, so a continue there reaches the enclosing loop's head instead of failing to lower the function",
			mutation: "lower the else body before closing the loop's frame (the continue binds to the for loop, is still pending at CloseFrame, and the lowering panics)",
			src:      "def f(a):\n while a:\n  for y in a:\n   break\n  else:\n   continue\n  a = 0\n return a\n",
			fn:       1,
			cd: []string{"a@17 -> a@31", "a@17 -> y in a@26", "a@17 -> a@17",
				"y in a@26 -> y@26", "y in a@26 -> break@37", "y in a@26 -> a = 0@65", "y in a@26 -> continue@54"},
			du: []string{"a@6 -> a@17", "a@6 -> a@31", "a@6 -> return a@72",
				"a@31 -> y in a@26", "a@31 -> y@26",
				"a = 0@65 -> a@17", "a = 0@65 -> a@31", "a = 0@65 -> return a@72"},
		},
		{
			// §8.3, §8.2. Lines at 0, 10, 20, 34, 42, 50, 59. Nodes: a@6,
			// a@17 (while head), a@31 (the iterable, defining the iteration
			// variable), y in a@26 (for head) and y@26, each Using only the
			// iteration variable, break@53
			// (the for's else), return a@60. Succ: a@17→{a@31, return a};
			// a@31→y in a→{y@26, break}; y@26→y in a; break→return a.
			// IPDom: a@17 → return a; a@31 → y in a → break → return a;
			// y@26 → y in a.
			name:     "a break in a for loop's else leaves the enclosing loop",
			protects: "a break in a loop's else body targets the enclosing loop, so it reaches the code after that loop, not the enclosing loop's head",
			mutation: "lower the else body inside the for loop's frame (break@53 then flows back to a@17, and a@17 -> a@17 appears)",
			src:      "def f(a):\n while a:\n  for y in a:\n   pass\n  else:\n   break\n return a\n",
			fn:       1,
			cd: []string{"a@17 -> a@31", "a@17 -> y in a@26", "a@17 -> break@53",
				"y in a@26 -> y@26", "y in a@26 -> y in a@26"},
			du: []string{"a@6 -> a@17", "a@6 -> a@31", "a@31 -> y in a@26", "a@31 -> y@26", "a@6 -> return a@60"},
		},
		{
			// §8.4.2. Lines at 0, 9, 16, 22, 28, 40, 48, 60, 67. Nodes:
			// x = 0@10, g()@24 (may throw), except@29 (Handler), E@37,
			// x = 1@42, F@57 (reached from E's false edge and from x = 1),
			// h(x)@62, return x@68. After the last clause, both F's false
			// edge and h(x) re-raise what no clause matched (EXIT) and
			// complete (return x). IPDom: g(), F, h(x) → EXIT; E, x = 1 → F;
			// except → E.
			name:     "every matching except* clause runs, and what none matched is re-raised",
			protects: "an except* clause's body continues to the next clause's test, so a definition in one clause reaches the next, and the statement both completes and re-raises after the last",
			mutation: "lower except* as except, first match wins (x = 1@42 -> h(x)@62 disappears and x = 1@42 reaches return x@68 past F@57), or drop the final re-raise (h(x)@62 -> return x@68 disappears)",
			src:      "def f():\n x = 0\n try:\n  g()\n except* E:\n  x = 1\n except* F:\n  h(x)\n return x\n",
			fn:       1,
			cd: []string{"g()@24 -> except@29", "g()@24 -> E@37", "g()@24 -> F@57", "g()@24 -> return x@68",
				"E@37 -> x = 1@42", "F@57 -> h(x)@62", "F@57 -> return x@68", "h(x)@62 -> return x@68"},
			du: []string{"x = 0@10 -> h(x)@62", "x = 1@42 -> h(x)@62", "x = 0@10 -> return x@68", "x = 1@42 -> return x@68"},
		},
		{
			// §7.13, §7.2. Lines at 0, 9, 16, 26, 39, 47, 52. Nodes: n = 0@10,
			// def g(): …@17 (defines g, may-defines n through the nonlocal,
			// reads nothing: `n = 1` only writes n), g()@48, return n@53.
			name:     "a nested callable's plain assignment writes an enclosing variable without reading it",
			protects: "the node creating a callable that assigns an enclosing variable may-defines it and does not use it",
			mutation: "collect an assignment target in a nested callable as a read (n = 0@10 -> def g(): nonlocal n n = 1@17 appears)",
			src:      "def f():\n n = 0\n def g():\n  nonlocal n\n  n = 1\n g()\n return n\n",
			fn:       1,
			du: []string{"n = 0@10 -> return n@53", "def g(): nonlocal n n = 1@17 -> return n@53",
				"def g(): nonlocal n n = 1@17 -> g()@48"},
		},
		{
			// §8.5: a failing assignment to the target runs __exit__. Lines
			// at 0, 13, 29, 36. Nodes: m@6, o@9, m as o.p@19 (enter, reads
			// m, defines the entered value's variable and may-defines the
			// manager's), o.p@24 (the target's write: Uses the entered value
			// and o, may-defines o, may throw into the item's finally),
			// with@14 (that finally's Handler), with m as o.p@14 (the exit),
			// return o@37. Succ: o.p→{with@14, exit}; with@14→exit;
			// exit→{EXIT (the re-raise), return o}. IPDom: o.p, with@14 →
			// exit → EXIT.
			name:     "a with item's reference target is bound inside the item's finally",
			protects: "an exception assigning a with item's target reaches the manager's exit, as one in the body does, and the target is bound from the entered value, not from the context expression's names",
			mutation: "bind a reference target before opening the item's finally (with@14 disappears and the exit controls nothing), or bind it from the context expression's reads (m@6 -> o.p@24 replaces m as o.p@19 -> o.p@24)",
			src:      "def f(m, o):\n with m as o.p:\n  pass\n return o\n",
			fn:       1,
			cd:       []string{"o.p@24 -> with@14", "with m as o.p@14 -> return o@37"},
			du: []string{"m@6 -> m as o.p@19", "m as o.p@19 -> o.p@24", "o@9 -> o.p@24", "m as o.p@19 -> with m as o.p@14",
				"o@9 -> return o@37", "o.p@24 -> return o@37"},
		},
		{
			// §7.12, the module (callable 0). Lines at 0, 12, 24, 33, 40.
			// cfg is bound only by init's global declaration, so it is a
			// module variable. Nodes: def init(): …@0 (defines init,
			// may-defines cfg), init()@33, use(cfg)@40.
			name:     "a global assigned only in a nested function is a module variable",
			protects: "a module variable written only through a function's global declaration reaches the module code that reads it",
			mutation: "bind only the module's own binding sites (cfg is no variable and def init(): … -> use(cfg)@40 disappears)",
			src:      "def init():\n global cfg\n cfg = 1\ninit()\nuse(cfg)\n",
			du: []string{"def init(): global cfg cfg = 1@0 -> init()@33",
				"def init(): global cfg cfg = 1@0 -> use(cfg)@40"},
		},
		{
			// §8.5. Lines at 0, 10, 19, 30. Nodes: m@6, m@16 (enter),
			// return 1@21 (intercepted by the with's finally), with m@11
			// (the exit, re-issuing the return to EXIT), g()@31, which only
			// normal completion or a suppressed exception would reach: it
			// has no predecessor.
			name:     "a return inside with does not fall through past the with",
			protects: "the statement after a with is reached from the exit only when the body completed or an exception reached the exit, never by a return alone",
			mutation: "close the with's finally as normal whatever reached it (the exit gains an edge to g()@31 and controls it)",
			src:      "def f(m):\n with m:\n  return 1\n g()\n",
			fn:       1,
			du:       []string{"m@6 -> m@16", "m@16 -> with m@11"},
		},
		{
			// §8.5: the exit calls the manager the enter produced. Lines at
			// 0, 10, 19, 27. Nodes: m@6, m@16 (enter, may-defines the
			// manager), m = 0@21, with m@11 (the exit, reading the manager),
			// return m@28.
			name:     "a with exit reads the manager entered, not the context variable at exit time",
			protects: "rebinding the context expression's variable in the body does not change which value the exit uses",
			mutation: "make the exit read the context expression's variables again (m = 0@21 -> with m@11 replaces m@16 -> with m@11)",
			src:      "def f(m):\n with m:\n  m = 0\n return m\n",
			fn:       1,
			du:       []string{"m@6 -> m@16", "m@16 -> with m@11", "m = 0@21 -> return m@28"},
		},
		{
			// §6.13: the condition is evaluated first, then one arm, whose
			// value is the expression's. Lines at 0, 16, 35. Nodes: a@6,
			// b@9, c@12, c@26 (the condition's Branch), a@21 and b@33 (the
			// arms, each defining the result variable), y = a if c else
			// b@17 (Uses the result variable, defines y), return y@36.
			// Succ: c@26→{a@21, b@33}→y = …→return y. IPDom: c@26, a@21,
			// b@33 → y = ….
			name:     "a conditional expression hands the chosen arm's value to its consumer through a variable",
			protects: "the assignment depends on each arm's node through the result variable and on the condition only through control of the arms, never by re-reading the condition's or the arms' names",
			mutation: "let the consumer re-read the condition and the arms (c@12 -> y = a if c else b@17, a@6 -> y = a if c else b@17 and b@9 -> y = a if c else b@17 replace the pairs from a@21 and b@33)",
			src:      "def f(a, b, c):\n y = a if c else b\n return y\n",
			fn:       1,
			cd:       []string{"c@26 -> a@21", "c@26 -> b@33"},
			du: []string{"a@6 -> a@21", "b@9 -> b@33", "c@12 -> c@26",
				"a@21 -> y = a if c else b@17", "b@33 -> y = a if c else b@17", "y = a if c else b@17 -> return y@36"},
		},
		{
			// §6.11: the left operand decides, and the right one is
			// evaluated only when it does not. Lines at 0, 13. Nodes: a@6,
			// b@9, a@21 (Branch: Uses a, defines the result variable), b@26
			// (defines it again), return a or b@14 (Uses it). Succ:
			// a@21→{b@26, return}; b@26→return. IPDom: a@21, b@26 → return.
			name:     "a short-circuit operator's value reaches its consumer from each operand that can decide it",
			protects: "the return depends on the deciding operand's node and on the operand after it through the result variable, not on the operands' names",
			mutation: "let the consumer re-read the operands (a@6 -> return a or b@14 and b@9 -> return a or b@14 replace the pairs from a@21 and b@26)",
			src:      "def f(a, b):\n return a or b\n",
			fn:       1,
			cd:       []string{"a@21 -> b@26"},
			du:       []string{"a@6 -> a@21", "b@9 -> b@26", "a@21 -> return a or b@14", "b@26 -> return a or b@14"},
		},
		{
			// §6.10: every operand is evaluated at most once. Lines at 0, 19.
			// Nodes: a@6, b@9, c@12, d@15, a < b@27 (Branch: Uses a and b,
			// defines the result variable, holds b), c@35 (Uses c, defines
			// a variable holding c), b < c@31 (Branch: Uses the two held
			// variables, defines the result variable), d@39 (Uses d and the
			// held c, makes c < d, defines the result variable), return a <
			// b < c < d@20. Succ: a < b→{c@35, return}; c@35→b < c→{d@39,
			// return}; d@39→return. IPDom: a < b, b < c, d@39 → return;
			// c@35 → b < c.
			name:     "a middle comparison of a chain reads the operands held for it",
			protects: "an operand of a chain is read once, at the node evaluating it, and a later comparison reads its value through the held variable, so b is never re-read by name",
			mutation: "let a later comparison re-read its operands (b@9 -> b < c@31 and c@12 -> b < c@31 replace a < b@27 -> b < c@31 and c@35 -> b < c@31)",
			src:      "def f(a, b, c, d):\n return a < b < c < d\n",
			fn:       1,
			cd:       []string{"a < b@27 -> c@35", "a < b@27 -> b < c@31", "b < c@31 -> d@39"},
			du: []string{"a@6 -> a < b@27", "b@9 -> a < b@27", "c@12 -> c@35", "d@15 -> d@39",
				"a < b@27 -> b < c@31", "c@35 -> b < c@31", "c@35 -> d@39",
				"a < b@27 -> return a < b < c < d@20", "b < c@31 -> return a < b < c < d@20", "d@39 -> return a < b < c < d@20"},
		},
		{
			// §6.12: the assignment expression's value is the value it
			// assigns. Lines at 0, 10, 28. Nodes: a@6, x := a@16 (Uses a,
			// defines x, may-defines the result variable), y = (x := a) *
			// 2@11 (Uses the result variable, defines y), return x + y@29
			// (Uses x and y).
			name:     "an assignment expression's value travels through its result variable",
			protects: "the node consuming `x := a` reads the result variable the assignment expression's node may-defines, and does not re-read the value's names",
			mutation: "let the consumer re-read the assigned value's reads (a@6 -> y = (x := a) * 2@11 appears)",
			src:      "def f(a):\n y = (x := a) * 2\n return x + y\n",
			fn:       1,
			du: []string{"a@6 -> x := a@16", "x := a@16 -> y = (x := a) * 2@11", "x := a@16 -> return x + y@29",
				"y = (x := a) * 2@11 -> return x + y@29"},
		},
		{
			// §6.12: each assignment expression's value is the value it
			// assigns when it is evaluated. Lines at 0, 9, 34. Nodes: x :=
			// 1@15 and x := 2@26 (each defines x and may-defines its own
			// result variable), y = (x := 1) + (x := 2)@10 (Uses the two
			// result variables, defines y), return y@35.
			name:     "two assignment expressions to one name each reach their consumer",
			protects: "the consumer of `(x := 1) + (x := 2)` reads each assignment expression's result variable, so it depends on the first assignment although the second rebinds x before it",
			mutation: "let the consumer read the assigned name instead of the result variable (x := 1@15 -> y = (x := 1) + (x := 2)@10 disappears)",
			src:      "def f():\n y = (x := 1) + (x := 2)\n return y\n",
			fn:       1,
			du: []string{"x := 1@15 -> y = (x := 1) + (x := 2)@10", "x := 2@26 -> y = (x := 1) + (x := 2)@10",
				"y = (x := 1) + (x := 2)@10 -> return y@35"},
		},
		{
			// §6.14, §7.2. Lines at 0, 10, 25. Nodes: k@6, lambda: k@15 (the
			// creating node: captures k, defines the result variable), k =
			// lambda: k@11 (Uses the result variable, defines k), return
			// k@26. The statement read k before its assignment, but the
			// creating node carries that read, so the assignment does not
			// repeat it.
			name:     "a defining node does not repeat a read a nested node carries",
			protects: "an assignment defining a name the statement read earlier Uses it only when no node of its own carries that read, so a lambda capturing the name it is assigned to pairs its capture with the creating node alone",
			mutation: "make a defining node repeat every earlier read of its variable in the statement (k@6 -> k = lambda: k@11 appears)",
			src:      "def f(k):\n k = lambda: k\n return k\n",
			fn:       1,
			du:       []string{"k@6 -> lambda: k@15", "lambda: k@15 -> k = lambda: k@11", "k = lambda: k@11 -> return k@26"},
		},
		{
			// §7.2: the right side is evaluated once, then assigned to the
			// targets from left to right. Lines at 0, 13, 26. Nodes: a@6,
			// b@9, b, a@21 (the right side: Uses b and a, defines a
			// variable holding the tuple), a@14 and b@17 (the targets, each
			// Using that variable), return a - b@27.
			name:     "an unpacking assignment binds every target from the value evaluated once",
			protects: "a swap's second target reads the tuple, not the name the first target just rebound",
			mutation: "bind each target from the right side's reads (a@14 -> b@17 appears: the second target reads the a the first one wrote)",
			src:      "def f(a, b):\n a, b = b, a\n return a - b\n",
			fn:       1,
			du: []string{"a@6 -> b, a@21", "b@9 -> b, a@21", "b, a@21 -> a@14", "b, a@21 -> b@17",
				"a@14 -> return a - b@27", "b@17 -> return a - b@27"},
		},
		{
			// §6.2.4, the comprehension itself (callable 2): a later for
			// clause's iterable is evaluated once per step of the clause
			// before it. Lines at 0, 11. Nodes: for x in xs@22 (head, Using
			// nothing: its iterator is the enclosing function's), x@26
			// (target), x@43 (the second clause's iterable: Uses x, defines
			// its iteration variable), for y in x@34 (head, Using that
			// variable), y@38 (target, Using it), y@20 (element). Succ: for
			// x→{x@26, EXIT}; x@26→x@43→for y; for y→{y@38, for x};
			// y@38→y@20→for y. IPDom: for x → EXIT; x@26 → x@43 → for y →
			// for x; y@38 → y@20 → for y.
			name:     "a later comprehension clause's head and target read its iteration variable",
			protects: "a later for clause's iterable is evaluated at its own node, and its head and target read the iterator made there, not the names the iterable reads",
			mutation: "make a later clause's head and target Use its iterable's reads (x@26 -> for y in x@34 and x@26 -> y@38 appear)",
			src:      "def f(xs):\n return [y for x in xs for y in x]\n",
			fn:       2,
			cd: []string{"for x in xs@22 -> x@26", "for x in xs@22 -> x@43", "for x in xs@22 -> for y in x@34",
				"for x in xs@22 -> for x in xs@22", "for y in x@34 -> y@38", "for y in x@34 -> y@20",
				"for y in x@34 -> for y in x@34"},
			du: []string{"x@26 -> x@43", "x@43 -> for y in x@34", "x@43 -> y@38", "y@38 -> y@20"},
		},
	})
}
