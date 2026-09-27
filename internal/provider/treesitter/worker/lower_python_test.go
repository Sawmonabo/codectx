package worker

import "testing"

// TestPythonLoweringGolden pins the Python lowering's control-dependence and
// def-use pairs on hand-derived functions. A wrong pair here is a wrong
// dependence fact served to every consumer. Callable 0 is the module; a
// function defined at the top is callable 1, and a lambda or comprehension
// inside it callable 2. Each source is indented by one space per level, and
// each case's comment gives the byte offset every line starts at.
//
// The derivation and rendering rules are runGolden's. Every case opens with the section it turns on; section numbers are those
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
			// may-definition is a χ, so around the loop it pairs with d@6
			// and with itself, the nearest may-definition, and return d
			// pairs with both.
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
			// r@19 (enter, defines r and the manager), c@32,
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
			// exit reads the manager its enter node defines.
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
			// §6.2.4, the comprehension itself (callable 2): its first
			// iterable's iterator is an implicit argument. Nodes: xs@38 (the
			// parameter-like node defining the iteration variable), for x in
			// xs@29 (head, Using it), x@33 (target, Using it), x@44 (if
			// clause), x + k@23 (element). Succ: xs@38→head; head→{x@33,
			// EXIT}; x@33→x@44→{x + k, head}; x + k→head. IPDom: xs@38 →
			// head; x@33 → x@44 → head → EXIT; x + k → head.
			name:     "a comprehension is its own callable with its clauses as nested loops",
			protects: "each for clause is a loop head, an if clause's false edge continues with the next element, and the element is control dependent on the if clause, which is control dependent on the head",
			mutation: "lower an if clause's false edge to the comprehension's exit (the head no longer post-dominates x@44, and x@44 -> for x in xs@29 appears), or give the first iterable no node in the comprehension's graph (xs@38 -> for x in xs@29 and xs@38 -> x@33 disappear)",
			src:      "def f(xs, k):\n return [x + k for x in xs if x]\n",
			fn:       2,
			cd: []string{"for x in xs@29 -> x@33", "for x in xs@29 -> x@44", "for x in xs@29 -> for x in xs@29",
				"x@44 -> x + k@23"},
			du: []string{"xs@38 -> for x in xs@29", "xs@38 -> x@33", "x@33 -> x@44", "x@33 -> x + k@23"},
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
			// reads it, and the χ pairs the node with y = 0@12, the version
			// it may leave in place), return y@41. The expression statement
			// is the comprehension's node.
			name:     "an assignment expression in a comprehension binds in the enclosing function",
			protects: "`:=` inside a comprehension writes the enclosing function's variable, a may-definition at the comprehension's creation, so the earlier definition reaches both that node and the later use",
			mutation: "bind a `:=` target as the comprehension's own local (the comprehension node loses its may-definition: y = 0@12 -> [y := x for x in xs]@19 and [y := x for x in xs]@19 -> return y@41 disappear), or make the write a killing definition (y = 0@12 -> return y@41 disappears)",
			src:      "def f(xs):\n y = 0\n [y := x for x in xs]\n return y\n",
			fn:       1,
			du: []string{"xs@6 -> [y := x for x in xs]@19", "y = 0@12 -> [y := x for x in xs]@19",
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
			// a variable holding b), c@32 (evaluated only when a
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
			protects: "`del(a)`, the list element of `del [b], (c)` and its parenthesized target delete names like `del x`, rather than failing to lower the function",
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
			// def g(): …@17 (defines g, may-defines n through the nonlocal:
			// `n = 1` only writes n, and the χ pairs the node with n = 0@10,
			// the version it may leave in place), g()@48, return n@53.
			name:     "a nested callable's plain assignment is a may-definition of the enclosing variable",
			protects: "the node creating a callable that assigns an enclosing variable may-defines it, so the definition before it reaches both that node and the later use",
			mutation: "record no write for a nested callable's plain assignment to a nonlocal name (def g(): nonlocal n n = 1@17 loses its pairs with n = 0@10 and return n@53), or make it a killing definition (n = 0@10 -> return n@53 disappears)",
			src:      "def f():\n n = 0\n def g():\n  nonlocal n\n  n = 1\n g()\n return n\n",
			fn:       1,
			du: []string{"n = 0@10 -> def g(): nonlocal n n = 1@17",
				"n = 0@10 -> return n@53", "def g(): nonlocal n n = 1@17 -> return n@53",
				"def g(): nonlocal n n = 1@17 -> g()@48"},
		},
		{
			// §8.5: a failing assignment to the target runs __exit__. Lines
			// at 0, 13, 29, 36. Nodes: m@6, o@9, m as o.p@19 (enter, reads
			// m, defines the entered value's variable and the manager's),
			// o.p@24 (the target's write: Uses the entered value
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
			// 0, 10, 19, 27. Nodes: m@6, m@16 (enter, defines the
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
			// defines x and the result variable), y = (x := a) *
			// 2@11 (Uses the result variable, defines y), return x + y@29
			// (Uses x and y).
			name:     "an assignment expression's value travels through its result variable",
			protects: "the node consuming `x := a` reads the result variable the assignment expression's node defines, and does not re-read the value's names",
			mutation: "let the consumer re-read the assigned value's reads (a@6 -> y = (x := a) * 2@11 appears)",
			src:      "def f(a):\n y = (x := a) * 2\n return x + y\n",
			fn:       1,
			du: []string{"a@6 -> x := a@16", "x := a@16 -> y = (x := a) * 2@11", "x := a@16 -> return x + y@29",
				"y = (x := a) * 2@11 -> return x + y@29"},
		},
		{
			// §6.12: each assignment expression's value is the value it
			// assigns when it is evaluated. Lines at 0, 9, 34. Nodes: x :=
			// 1@15 and x := 2@26 (each defines x and its own result
			// variable), y = (x := 1) + (x := 2)@10 (Uses the two
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
			// §6.12, §6.16: operands are evaluated left to right, so the
			// first x is read before either assignment expression. Lines at
			// 0, 10, 39. Nodes: x@6, x := 1@20 (Uses the earlier x, defines
			// x, a variable carrying the earlier value and its own result
			// variable), x := 2@31 (defines x and its result variable; the held read of x is no longer the name),
			// y = x + (x := 1) + (x := 2)@11 (Uses the carried value and
			// the two result variables), return y@40.
			name:     "a read made before an assignment expression reaches its consumer through the assignment's node",
			protects: "the x the consumer read first pairs with the node that carried it, and a later assignment expression to x in the same statement does not re-read it",
			mutation: "keep the name in the held reads after the first assignment expression reads it (x := 1@20 -> x := 2@31 appears: the second one re-reads the x the first one wrote)",
			src:      "def f(x):\n y = x + (x := 1) + (x := 2)\n return y\n",
			fn:       1,
			du: []string{"x@6 -> x := 1@20", "x := 1@20 -> y = x + (x := 1) + (x := 2)@11",
				"x := 2@31 -> y = x + (x := 1) + (x := 2)@11", "y = x + (x := 1) + (x := 2)@11 -> return y@40"},
		},
		{
			// §6.11, §6.12: the operand after `and`'s deciding one runs only
			// when c is true. Lines at 0, 13, 39. Nodes: x@6, c@9, c@23
			// (Branch: Uses c, defines the result variable), x := 1@30
			// (defines x and the result variable; it does not run
			// whenever the consumer does, so the earlier read of x stays on
			// the consumer), y = x + (c and (x := 1))@14 (Uses x and the
			// result variable), return y@40. Succ: c@23→{x := 1, y = …};
			// x := 1→y = …. IPDom: c@23, x := 1 → y = ….
			name:     "a conditionally evaluated assignment expression leaves an earlier read on its consumer",
			protects: "when the operand holding `x := 1` is skipped, the consumer's earlier read of x still pairs with the x before it, and with the assignment when it runs",
			mutation: "hand the earlier read off to the assignment expression unconditionally (x@6 -> y = x + (c and (x := 1))@14 disappears and x@6 -> x := 1@30 appears)",
			src:      "def f(x, c):\n y = x + (c and (x := 1))\n return y\n",
			fn:       1,
			cd:       []string{"c@23 -> x := 1@30"},
			du: []string{"c@9 -> c@23", "x@6 -> y = x + (c and (x := 1))@14", "x := 1@30 -> y = x + (c and (x := 1))@14",
				"c@23 -> y = x + (c and (x := 1))@14", "y = x + (c and (x := 1))@14 -> return y@40"},
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
			// before it. Lines at 0, 11. Nodes: xs@31 (the first iterable's
			// parameter-like node, defining its iteration variable), for x in
			// xs@22 (head, Using it), x@26 (target, Using it), x@43 (the second clause's iterable: Uses x, defines
			// its iteration variable), for y in x@34 (head, Using that
			// variable), y@38 (target, Using it), y@20 (element). Succ:
			// xs@31→for x; for x→{x@26, EXIT}; x@26→x@43→for y; for y→{y@38, for x};
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
			du: []string{"xs@31 -> for x in xs@22", "xs@31 -> x@26",
				"x@26 -> x@43", "x@43 -> for y in x@34", "x@43 -> y@38", "y@38 -> y@20"},
		},
		{
			// §7.12: a name declared global in a function is no variable of
			// it, so g resolves to no variable in every position here (k's own
			// global declaration makes k's write one too). Lines at 0, 14, 24,
			// 31, 40, 49, 63, 78, 85, 99, 106, 120, 127, 133, 139, 155, 162,
			// 172, 183, 191. Nodes: xs@6, m@10 (params), g = 1@25 (assignment
			// target), g += xs@32 (compound: Uses xs only), g.p = m@41 (the
			// base of a written attribute: Uses m, may-defines nothing), g :=
			// 2@55 (embedded: defines the result variable), y = (g := 2)@50,
			// lambda: g@68 (captures nothing), h = lambda: g@64, del g@79
			// (deleted), xs@95 (the iterable), g in xs@90 (head), g@90
			// (iterated target), m as g@112 (with target: defines nothing
			// but the manager), with m as g@107 (the exit), h()@135
			// (may throw), except@140 (Handler), E@147 (a free name: Uses
			// nothing), g@152 (the except target), except E as g: pass@140
			// (the deletion of g, still made), def k(): global g g = 3@163
			// (defines k, may-defines nothing), return y, h, k@192. Succ:
			// head→{g@90, m as g}; g@90→head; h()→{def k, except};
			// except→E→{g@152, EXIT (the re-raise)}; g@152→deletion→def k.
			// IPDom: head → m as g; E, h() → EXIT; g@152 → deletion → def k.
			name:     "a name that resolves to no variable is a node in every position and defines and reads nothing",
			protects: "an unresolved assignment, compound, attribute-base, embedded, captured, deleted, iterated, with, except and nested-write name makes its node and no definition or use, and never reaches the Builder or the read tables as -1",
			mutation: "drop def's or read's v < 0 return (Builder.Def or Use panics on -1, or seen is indexed by -1), mayDefBase's v >= 0 test (Builder.MayDef panics on -1), or site's v >= 0 filter on a nested callable's writes (Builder.MayDef panics on -1); or make the except clause's finally and deleting node only for a variable of the function (except E as g: pass@140 disappears, and E@147 -> except E as g: pass@140 with it)",
			src: "def f(xs, m):\n global g\n g = 1\n g += xs\n g.p = m\n y = (g := 2)\n h = lambda: g\n del g\n" +
				" for g in xs:\n  pass\n with m as g:\n  pass\n try:\n  h()\n except E as g:\n  pass\n" +
				" def k():\n  global g\n  g = 3\n return y, h, k\n",
			fn: 1,
			cd: []string{"g in xs@90 -> g@90", "g in xs@90 -> g in xs@90",
				"h()@135 -> except@140", "h()@135 -> E@147", "h()@135 -> def k(): global g g = 3@163",
				"h()@135 -> return y, h, k@192",
				"E@147 -> g@152", "E@147 -> except E as g: pass@140", "E@147 -> def k(): global g g = 3@163",
				"E@147 -> return y, h, k@192"},
			du: []string{"xs@6 -> g += xs@32", "m@10 -> g.p = m@41", "g := 2@55 -> y = (g := 2)@50",
				"lambda: g@68 -> h = lambda: g@64", "xs@6 -> xs@95", "xs@95 -> g in xs@90", "xs@95 -> g@90",
				"m@10 -> m as g@112", "m as g@112 -> with m as g@107", "h = lambda: g@64 -> h()@135",
				"y = (g := 2)@50 -> return y, h, k@192", "h = lambda: g@64 -> return y, h, k@192",
				"def k(): global g g = 3@163 -> return y, h, k@192"},
		},
		{
			// §8.7: a default is evaluated when the definition runs. Lines at
			// 0, 10, 23, 30. Nodes: a@6, def g(x=a): pass@11 (reads a, the
			// default, and defines g), return g@31.
			name:     "a parameter default is a read of the defining node",
			protects: "a default value belongs to the enclosing function's creating node, not to the nested function's graph",
			mutation: "evaluate defaults inside the nested function (a@6 -> def g(x=a): pass@11 disappears)",
			src:      "def f(a):\n def g(x=a):\n  pass\n return g\n",
			fn:       1,
			du:       []string{"a@6 -> def g(x=a): pass@11", "def g(x=a): pass@11 -> return g@31"},
		},
		{
			// §8.8: the base list is evaluated when the class statement runs.
			// Lines at 0, 10, 23, 30. Nodes: B@6, class C(B): pass@11 (reads
			// B, defines C), return C@31.
			name:     "a class's bases are reads of the creating node",
			protects: "a class's bases belong to the enclosing function's creating node, not to the class body's graph",
			mutation: "evaluate the bases in the class body (B@6 -> class C(B): pass@11 disappears)",
			src:      "def f(B):\n class C(B):\n  pass\n return C\n",
			fn:       1,
			du:       []string{"B@6 -> class C(B): pass@11", "class C(B): pass@11 -> return C@31"},
		},
		{
			// §7.1. Lines at 0, 13. Nodes: a@6, b@9, a, b@14 (one node
			// spanning the statement, reading both expressions).
			name:     "a bare tuple statement is one node spanning the statement",
			protects: "an expression statement of several comma-separated expressions evaluates each of them at one node",
			mutation: "lower only the first expression (b@9 -> a, b@14 disappears), or span the first expression (the node renders a@14)",
			src:      "def f(a, b):\n a, b\n",
			fn:       1,
			du:       []string{"a@6 -> a, b@14", "b@9 -> a, b@14"},
		},
		{
			// §7.11. Lines at 0, 9, 21, 43. Nodes: a@17 (`import a.b` binds
			// a, the first component), d@41 (the alias binds d, not c), return
			// a, d@44.
			name:     "an import binds the first component of a dotted name and an alias",
			protects: "each import clause defines the local name it binds, and only that name",
			mutation: "bind the imported name instead of its alias (d is no local and d@41 -> return a, d@44 disappears), or no name for a dotted import (a@17 -> return a, d@44 disappears)",
			src:      "def f():\n import a.b\n from m import c as d\n return a, d\n",
			fn:       1,
			du:       []string{"a@17 -> return a, d@44", "d@41 -> return a, d@44"},
		},
		{
			// §7.11. Lines at 0, 13, 20, 38. Nodes: c@6, x@9, c@17 (Branch),
			// from m import *@22 (a Stmt that defines nothing), return x@39.
			// Succ: c→{from m import *, return x}; from m import *→return x.
			name:     "a wildcard import is one node that defines nothing",
			protects: "`from m import *` is a node of its own on its path and kills no local",
			mutation: "make no node for it (c@17 -> from m import *@22 disappears), or let it kill every local (x@9 -> return x@39 becomes from m import *@22 -> return x@39)",
			src:      "def f(c, x):\n if c:\n  from m import *\n return x\n",
			fn:       1,
			cd:       []string{"c@17 -> from m import *@22"},
			du:       []string{"c@6 -> c@17", "x@9 -> return x@39"},
		},
		{
			// §7.14. Lines at 0, 9, 23. Nodes: type T = int@10 (spans the
			// statement, defines T), return T@24.
			name:     "a type alias statement defines its name",
			protects: "a type alias binds a local that later reads see",
			mutation: "make the alias define nothing (type T = int@10 -> return T@24 disappears)",
			src:      "def f():\n type T = int\n return T\n",
			fn:       1,
			du:       []string{"type T = int@10 -> return T@24"},
		},
		{
			// §7.2: the right side is evaluated once and assigned to each
			// target from left to right. Lines at 0, 10, 21. Nodes: e@6,
			// e@19 (Uses e, defines a variable of the lowering's own),
			// a@11 and b@15 (each Using that variable), return a, b@22.
			name:     "a chained assignment evaluates its value once and binds each target from it",
			protects: "every target of `a = b = e` is bound from the one evaluation of e, not from e's reads",
			mutation: "bind each target from the right side's reads (e@6 -> a@11 and e@6 -> b@15 replace the pairs from e@19)",
			src:      "def f(e):\n a = b = e\n return a, b\n",
			fn:       1,
			du: []string{"e@6 -> e@19", "e@19 -> a@11", "e@19 -> b@15",
				"a@11 -> return a, b@22", "b@15 -> return a, b@22"},
		},
		{
			// §7.2.2: an annotation without a value evaluates nothing. Lines
			// at 0, 13, 20, 29. Nodes: c@6, x@9, c@17 (Branch), return x@30.
			// The if's body makes no node, so c controls nothing.
			name:     "an annotation without a value makes no node",
			protects: "`x: int` is neither a node nor a definition, so x's parameter value reaches the return and nothing depends on the condition",
			mutation: "make a node for the annotation (c@17 -> x: int@22 appears), or a definition of x (x: int@22 -> return x@30 appears)",
			src:      "def f(c, x):\n if c:\n  x: int\n return x\n",
			fn:       1,
			du:       []string{"c@6 -> c@17", "x@9 -> return x@30"},
		},
		{
			// §7.12. Lines at 0, 10, 17, 28. Nodes: c@6, c@14 (Branch),
			// return 1@29. The global declaration in the if's body makes no
			// node, so c controls nothing.
			name:     "a global declaration makes no node",
			protects: "a global or nonlocal statement is a declaration, not code on the path",
			mutation: "make a node for the declaration (c@14 -> global g@19 appears)",
			src:      "def f(c):\n if c:\n  global g\n return 1\n",
			fn:       1,
			du:       []string{"c@6 -> c@14"},
		},
		{
			// §7.5. Lines at 0, 16, 31. Nodes: o@6, a@9, i@12, o.p@21 (the
			// first of two targets, spanning itself: Uses o, may-defines o),
			// a[i]@26 (Uses a and i, may-defines a), return o, a@32. Each χ
			// leaves the parameter's definition in place.
			name:     "deleting an attribute or subscript is a write through its base",
			protects: "`del o.p, a[i]` reads the object and index and may-defines the base variables without killing them",
			mutation: "kill the base (o@6 -> return o, a@32 and a@9 -> return o, a@32 disappear), or record no write (o.p@21 -> return o, a@32 and a[i]@26 -> return o, a@32 disappear)",
			src:      "def f(o, a, i):\n del o.p, a[i]\n return o, a\n",
			fn:       1,
			du: []string{"o@6 -> o.p@21", "a@9 -> a[i]@26", "i@12 -> a[i]@26",
				"o@6 -> return o, a@32", "o.p@21 -> return o, a@32", "a@9 -> return o, a@32", "a[i]@26 -> return o, a@32"},
		},
		{
			// §8.1, lower.go (Spans). Lines at 0, 10, 19, 30. Nodes: a@6,
			// a@15 (the Branch, spanning a without its parentheses), return
			// 1@21, return 0@31. Succ: a@15→{return 1, return 0}.
			name:     "a parenthesized condition spans the expression inside the parentheses",
			protects: "an if condition's node spans its expression with the enclosing parentheses stripped",
			mutation: "span the condition as written (the Branch renders (a)@14)",
			src:      "def f(a):\n if (a):\n  return 1\n return 0\n",
			fn:       1,
			cd:       []string{"a@15 -> return 1@21", "a@15 -> return 0@31"},
			du:       []string{"a@6 -> a@15"},
		},
		{
			// §8.3. Lines at 0, 11, 28. Nodes: ps@6, ps@24 (the iterable,
			// defining the iteration variable), a, b in ps@16 (head), a@16 and
			// b@19 (one binding node per name, each Using the iteration
			// variable), g(a, b)@30. Succ: ps@24→head→{a@16, EXIT};
			// a@16→b@19→g(a, b)→head. IPDom: a@16 → b@19 → g(a, b) → head →
			// EXIT.
			name:     "a for loop's unpacking target makes one binding node per name",
			protects: "each name an unpacking for target binds is defined on the body path from the iteration variable",
			mutation: "bind the target as one node (a@16 and b@19 become one node a, b@16), or bind from the iterable's reads (ps@6 -> a@16 and ps@6 -> b@19 appear)",
			src:      "def f(ps):\n for a, b in ps:\n  g(a, b)\n",
			fn:       1,
			cd: []string{"a, b in ps@16 -> a@16", "a, b in ps@16 -> b@19", "a, b in ps@16 -> g(a, b)@30",
				"a, b in ps@16 -> a, b in ps@16"},
			du: []string{"ps@6 -> ps@24", "ps@24 -> a, b in ps@16", "ps@24 -> a@16", "ps@24 -> b@19",
				"a@16 -> g(a, b)@30", "b@19 -> g(a, b)@30"},
		},
		{
			// §8.9.2: async for is lowered as for. Lines at 0, 17, 37. Nodes:
			// xs@12, xs@33 (the iterable), x in xs@28 (head), x@28 (target),
			// g(x)@39. Succ: xs@33→head→{x@28, EXIT}; x@28→g(x)→head.
			name:     "an async for loop is lowered as a for loop",
			protects: "async for evaluates its iterable once and binds its target on the body path, as for does",
			mutation: "lower async for as a plain statement (the head, the target and the loop's control dependence disappear)",
			src:      "async def f(xs):\n async for x in xs:\n  g(x)\n",
			fn:       1,
			cd:       []string{"x in xs@28 -> x@28", "x in xs@28 -> g(x)@39", "x in xs@28 -> x in xs@28"},
			du:       []string{"xs@12 -> xs@33", "xs@33 -> x in xs@28", "xs@33 -> x@28", "x@28 -> g(x)@39"},
		},
		{
			// §6.11, §8.1. Lines at 0, 13, 26. Nodes: a@6, b@9, a@17 (Branch:
			// defines the result variable), b@23 (defines it again), a and
			// b@17 (the condition's Branch, Using the result variable only),
			// g()@28. Succ: a@17→{b@23, a and b}; b@23→a and b; a and
			// b→{g(), EXIT}.
			name:     "a condition over a short-circuit operator reads its result variable",
			protects: "an if over `a and b` is a Branch of its own that reads the operator's result, not the operands' names",
			mutation: "let the condition re-read the operands (a@6 -> a and b@17 and b@9 -> a and b@17 replace the pairs from a@17 and b@23)",
			src:      "def f(a, b):\n if a and b:\n  g()\n",
			fn:       1,
			cd:       []string{"a@17 -> b@23", "a and b@17 -> g()@28"},
			du:       []string{"a@6 -> a@17", "b@9 -> b@23", "a@17 -> a and b@17", "b@23 -> a and b@17"},
		},
		{
			// §7.1, §6.11. Lines at 0, 10. Nodes: a@6, a@11 (Branch), g()@17.
			// The statement is the operator's nodes alone.
			name:     "an expression statement over a short-circuit operator is the operator's nodes alone",
			protects: "a discarded `a and g()` makes the Branch and the operand's node, and no node consuming a result",
			mutation: "make a node for the whole statement (a and g()@11 appears, after g()@17 and controlled by a@11)",
			src:      "def f(a):\n a and g()\n",
			fn:       1,
			cd:       []string{"a@11 -> g()@17"},
			du:       []string{"a@6 -> a@11"},
		},
		{
			// §8.4.4. Lines at 0, 9, 15, 21, 31. Nodes: g()@17 (may throw,
			// into the finally), finally@22 (the Handler), h()@33 (the
			// finally's body, reached from g()'s normal end and the
			// Handler, then re-raising and completing to EXIT). IPDom: g(),
			// finally → h() → EXIT.
			name:     "a finally that something may throw into has a Handler spanning finally",
			protects: "an exception part-way through the try body enters the finally through its Handler",
			mutation: "enter the finally from the normal end alone (finally@22 disappears and g()@17 controls nothing)",
			src:      "def f():\n try:\n  g()\n finally:\n  h()\n",
			fn:       1,
			cd:       []string{"g()@17 -> finally@22"},
		},
		{
			// §8.5, §7.9. Lines at 0, 13, 23, 33, 42. Nodes: m@6, a@9, a@20
			// (while head), m@30 (enter), break@36 (intercepted by the
			// with's finally), with m@25 (the exit, re-issuing the break),
			// return a@43. No exception reaches the finally, so nothing falls
			// through the exit to the loop head. Succ: a@20→{m@30, return a};
			// m@30→break→with m→return a. IPDom: a@20, m@30, break, with m
			// → return a.
			name:     "a break inside with is re-issued from the exit to its loop",
			protects: "a break leaving a with body runs the exit and then leaves the loop, never returning to the loop head",
			mutation: "let the exit fall through after a break (with m@25 reaches a@20, and a@20 -> a@20 appears), or break straight past the exit (with m@25 loses its predecessor and a@20's control of it)",
			src:      "def f(m, a):\n while a:\n  with m:\n   break\n return a\n",
			fn:       1,
			cd:       []string{"a@20 -> m@30", "a@20 -> break@36", "a@20 -> with m@25"},
			du:       []string{"m@6 -> m@30", "m@30 -> with m@25", "a@9 -> a@20", "a@9 -> return a@43"},
		},
		{
			// §8.9.3: async with is lowered as with. Lines at 0, 16, 36.
			// Nodes: m@12, m as r@28 (enter: defines r and the manager),
			// g(r)@38 (may throw into the item's finally), with@23 (the
			// Handler), with m as r@23 (the exit). Succ: g(r)→{exit, with};
			// with→exit→EXIT.
			name:     "an async with statement is lowered as a with statement",
			protects: "async with enters the manager, binds the target and runs the exit on every path, as with does",
			mutation: "lower async with as a plain statement (the enter, the exit and the Handler disappear)",
			src:      "async def f(m):\n async with m as r:\n  g(r)\n",
			fn:       1,
			cd:       []string{"g(r)@38 -> with@23"},
			du:       []string{"m@12 -> m as r@28", "m as r@28 -> g(r)@38", "m as r@28 -> with m as r@23"},
		},
		{
			// §8.6. Lines at 0, 13, 26, 41, 53. Nodes: a@6, b@9, a, b@20 (the
			// subjects, one node from the first to the last: Uses a and b,
			// defines the subject variable), (x, y)@33 (Branch), x@34 and
			// y@37 (captures), return x@44, return 0@54. The case is
			// refutable and the last, so its false edge falls out of the
			// match. Succ: a, b→(x, y)→{x@34, return 0}; x@34→y@37→return x.
			name:     "several match subjects are one node and a refutable last case falls out",
			protects: "`match a, b` evaluates both subjects once at one node that every case and capture reads, and a failed last case continues after the match",
			mutation: "make one node per subject (a@20 and b@23 replace a, b@20), or end the match at a refutable last case (return 0@54 becomes unreachable and (x, y)@33 no longer controls it)",
			src:      "def f(a, b):\n match a, b:\n  case (x, y):\n   return x\n return 0\n",
			fn:       1,
			cd: []string{"(x, y)@33 -> x@34", "(x, y)@33 -> y@37", "(x, y)@33 -> return x@44",
				"(x, y)@33 -> return 0@54"},
			du: []string{"a@6 -> a, b@20", "b@9 -> a, b@20", "a, b@20 -> (x, y)@33", "a, b@20 -> x@34",
				"a, b@20 -> y@37", "x@34 -> return x@44"},
		},
		{
			// §8.6.3, §8.6.4.2, §8.6.4.4, §8.6.4.7. A bare capture case
			// `case y:` cannot render apart from its capture: the case node
			// spans the pattern and the capture node the captured name, the
			// same bytes. So the capture sits inside a group and an
			// as-pattern, each irrefutable exactly when what it wraps is,
			// and the case node spans (y as z). Lines at 0, 10, 20, 30, 39,
			// 56, 65. Nodes: s@6, s@17 (the subject), 1@27 (Branch), r =
			// 1@33, (y as z)@46 (the case: a Stmt, no false edge), y@47 and
			// z@52 (captures, Using the subject's variable), r = z@59, return
			// r@66. Succ: 1@27→{r = 1, (y as z)}; (y as z)→y@47→z@52→r = z;
			// both arms→return r. IPDom: 1@27 → return r.
			name:     "a capture, a group of it and an as-pattern over it are irrefutable",
			protects: "a case whose pattern always matches has no false edge, so its case node controls nothing and nothing falls out of the match after it",
			mutation: "make a capture, a one-element group or an as-pattern refutable ((y as z)@46 becomes a Branch, and (y as z)@46 -> y@47, (y as z)@46 -> z@52 and (y as z)@46 -> r = z@59 appear)",
			src:      "def f(s):\n match s:\n  case 1:\n   r = 1\n  case (y as z):\n   r = z\n return r\n",
			fn:       1,
			cd: []string{"1@27 -> r = 1@33", "1@27 -> (y as z)@46", "1@27 -> y@47", "1@27 -> z@52",
				"1@27 -> r = z@59"},
			du: []string{"s@6 -> s@17", "s@17 -> 1@27", "s@17 -> (y as z)@46", "s@17 -> y@47", "s@17 -> z@52",
				"z@52 -> r = z@59", "r = 1@33 -> return r@66", "r = z@59 -> return r@66"},
		},
		{
			// §8.6.4.2: an or-pattern with an irrefutable alternative is
			// irrefutable. Lines at 0, 10, 20, 30, 39, 53, 62. Nodes: s@6,
			// s@17, 1@27 (Branch), r = 1@33, 2 | _@46 (the case: a Stmt),
			// r = 2@56, return r@63. Succ: 1@27→{r = 1, 2 | _}; 2 | _→r = 2;
			// both arms→return r.
			name:     "an or-pattern with a wildcard alternative is irrefutable",
			protects: "`case 2 | _` always matches, so its case node has no false edge and controls nothing",
			mutation: "make an or-pattern refutable whatever its alternatives (2 | _@46 becomes a Branch and 2 | _@46 -> r = 2@56 appears)",
			src:      "def f(s):\n match s:\n  case 1:\n   r = 1\n  case 2 | _:\n   r = 2\n return r\n",
			fn:       1,
			cd:       []string{"1@27 -> r = 1@33", "1@27 -> 2 | _@46", "1@27 -> r = 2@56"},
			du: []string{"s@6 -> s@17", "s@17 -> 1@27", "s@17 -> 2 | _@46",
				"r = 1@33 -> return r@63", "r = 2@56 -> return r@63"},
		},
		{
			// §7.3. Lines at 0, 10, 20. Nodes: a@6, a@18 (Branch; its false
			// edge raises straight to EXIT), return a@21.
			name:     "an assert without a message raises from the condition's false edge",
			protects: "a failed message-less assert leaves the function from its condition, so the code after it depends on the condition",
			mutation: "lower a message-less assert as a plain statement (a@18 controls nothing)",
			src:      "def f(a):\n assert a\n return a\n",
			fn:       1,
			cd:       []string{"a@18 -> return a@21"},
			du:       []string{"a@6 -> a@18", "a@6 -> return a@21"},
		},
		{
			// §6.2.7, §6.2.4, the comprehension (callable 2). Lines at 0, 10.
			// Nodes: d@36 (the first iterable's parameter-like node), for k,
			// v in d@24 (head), k@28 and v@31 (the unpacking target's binding
			// nodes), k: v@19 (the element, spanning the key: value pair).
			// Succ: d@36→head→{k@28, EXIT}; k@28→v@31→k: v→head.
			name:     "a dictionary comprehension's element is its key: value pair",
			protects: "the element node spans and reads both the key and the value, and an unpacking clause target binds each name",
			mutation: "span or read the key alone (v@31 -> k: v@19 disappears and the element renders k@19)",
			src:      "def f(d):\n return {k: v for k, v in d}\n",
			fn:       2,
			cd: []string{"for k, v in d@24 -> k@28", "for k, v in d@24 -> v@31", "for k, v in d@24 -> k: v@19",
				"for k, v in d@24 -> for k, v in d@24"},
			du: []string{"d@36 -> for k, v in d@24", "d@36 -> k@28", "d@36 -> v@31", "k@28 -> k: v@19", "v@31 -> k: v@19"},
		},
		{
			// §6.2.8: a generator expression is a callable of its own. Lines
			// at 0, 14. Nodes: xs@6, k@10, (x + k for x in xs)@22 (the
			// creating node: its first iterable and its capture k), return
			// …@15 (Uses the result variable). Callables: the module, f and
			// the generator.
			name:      "a generator expression is its own callable",
			protects:  "a generator expression's creation reads its first iterable and captures, and its body is not the enclosing function's code",
			mutation:  "drop generator expressions from the callables (callables = 2, and the return reads xs and k itself)",
			src:       "def f(xs, k):\n return (x + k for x in xs)\n",
			fn:        1,
			callables: 3,
			du: []string{"xs@6 -> (x + k for x in xs)@22", "k@10 -> (x + k for x in xs)@22",
				"(x + k for x in xs)@22 -> return (x + k for x in xs)@15"},
		},
		{
			// §6.4, §6.2.9. Lines at 0, 16, 29. Nodes: a@12, x = await a@17
			// (await folds into the assignment: Uses a, defines x), yield
			// x@30 (one Stmt node reading x).
			name:     "await and yield fold into their statement's node",
			protects: "await and yield are plain expressions, evaluated by the node of the statement holding them",
			mutation: "make await a node of its own (await a@21 appears and a@12 pairs with it instead of the assignment)",
			src:      "async def f(a):\n x = await a\n yield x\n",
			fn:       1,
			du:       []string{"a@12 -> x = await a@17", "x = await a@17 -> yield x@30"},
		},
		{
			// §6.3.1, §6.3.4. Lines at 0, 16. Nodes: o@6, p@9, a@12, return
			// g(o.p, a=1)@17 (reads o only: the attribute name p and the
			// keyword name a are not reads).
			name:     "attribute names and keyword names are not reads",
			protects: "an attribute's name and a keyword argument's name never read a variable that shares their spelling",
			mutation: "read the attribute name (p@9 -> return g(o.p, a=1)@17 appears), or the keyword name (a@12 -> return g(o.p, a=1)@17 appears)",
			src:      "def f(o, p, a):\n return g(o.p, a=1)\n",
			fn:       1,
			du:       []string{"o@6 -> return g(o.p, a=1)@17"},
		},
		{
			// §7.2.1. Lines at 0, 16, 27. Nodes: a@6, i@9, v@12, a[i] +=
			// v@17 (Uses a, i and v, may-defines a), return a@28.
			name:     "an augmented subscript write reads its operands and may-defines the base",
			protects: "`a[i] += v` reads the object, index and value and leaves a's earlier definition reaching later uses",
			mutation: "kill the base (a@6 -> return a@28 disappears), or record no write (a[i] += v@17 -> return a@28 disappears)",
			src:      "def f(a, i, v):\n a[i] += v\n return a\n",
			fn:       1,
			du: []string{"a@6 -> a[i] += v@17", "i@9 -> a[i] += v@17", "v@12 -> a[i] += v@17",
				"a@6 -> return a@28", "a[i] += v@17 -> return a@28"},
		},
		{
			// §6.3.1, §8.4. Lines at 0, 10, 16, 26, 37, 45. Nodes: o@6, y =
			// o.p@18 (the attribute read may throw), except@27 (Handler), E@34,
			// y = 0@39, return y@46. Succ: y = o.p→{return y, except};
			// except→E→{y = 0, EXIT}; y = 0→return y. IPDom: y = o.p, E →
			// EXIT; except → E.
			name:     "an attribute read may throw",
			protects: "a node reading an attribute inside a try reaches the handler",
			mutation: "count no throw for an attribute read (except@27 disappears, E@34 loses its predecessor and y = o.p@18 controls nothing)",
			src:      "def f(o):\n try:\n  y = o.p\n except E:\n  y = 0\n return y\n",
			fn:       1,
			cd: []string{"y = o.p@18 -> return y@46", "y = o.p@18 -> except@27", "y = o.p@18 -> E@34",
				"E@34 -> y = 0@39", "E@34 -> return y@46"},
			du: []string{"o@6 -> y = o.p@18", "y = o.p@18 -> return y@46", "y = 0@39 -> return y@46"},
		},
		{
			// §7.2, §8.4. Lines at 0, 10, 16, 27, 38, 46. Nodes: e@6, e@25 (the
			// right side, defining the unpacked value's variable), a@18 (the
			// first target: unpacking may throw, before any binding), b@21,
			// except@28 (Handler), E@35, a = 0@40, return a@47. Succ:
			// e@25→a@18→{b@21, except}; b@21→return a; except→E→{a = 0,
			// EXIT}. IPDom: e@25 → a@18 → EXIT; E → EXIT.
			name:     "an unpacking assignment may throw",
			protects: "unpacking a value inside a try reaches the handler",
			mutation: "count no throw for unpacking (except@28 disappears and a@18 controls nothing)",
			src:      "def f(e):\n try:\n  a, b = e\n except E:\n  a = 0\n return a\n",
			fn:       1,
			cd: []string{"a@18 -> b@21", "a@18 -> return a@47", "a@18 -> except@28", "a@18 -> E@35",
				"E@35 -> a = 0@40", "E@35 -> return a@47"},
			du: []string{"e@6 -> e@25", "e@25 -> a@18", "e@25 -> b@21", "a@18 -> return a@47", "a = 0@40 -> return a@47"},
		},
		{
			// §7.11, §8.4. Lines at 0, 9, 15, 26, 37, 45. Nodes: m@24 (the
			// import's binding node, which may throw), except@27 (Handler),
			// E@34, m = 0@39, return m@46. Succ: m@24→{return m, except};
			// except→E→{m = 0, EXIT}.
			name:     "an import may throw",
			protects: "an import inside a try reaches the handler",
			mutation: "count no throw for an import (except@27 disappears and m@24 controls nothing)",
			src:      "def f():\n try:\n  import m\n except E:\n  m = 0\n return m\n",
			fn:       1,
			cd: []string{"m@24 -> return m@46", "m@24 -> except@27", "m@24 -> E@34",
				"E@34 -> m = 0@39", "E@34 -> return m@46"},
			du: []string{"m@24 -> return m@46", "m = 0@39 -> return m@46"},
		},
		{
			// §7.8. Lines at 0, 9, 15, 25, 36, 44. Nodes: raise E@17 (a Throw
			// alone: its source holds no call, so it is not MayThrow and no
			// Handler is made), E@33 (the test, reached from the raise),
			// x = 1@38, return x@45. Succ: raise E→E@33→{x = 1, EXIT};
			// x = 1→return x.
			name:     "raise of a bare class is a Throw alone and makes no Handler",
			protects: "`raise E` joins the clause chain as a Throw source, with no Handler node before the first test",
			mutation: "make `raise E` MayThrow (except@26 appears between the raise and E@33, and raise E@17 -> except@26 with it)",
			src:      "def f():\n try:\n  raise E\n except E:\n  x = 1\n return x\n",
			fn:       1,
			cd:       []string{"E@33 -> x = 1@38", "E@33 -> return x@45"},
			du:       []string{"x = 1@38 -> return x@45"},
		},
		{
			// §7.8. Lines at 0, 9, 15, 27, 38, 46. Nodes: raise E()@17 (a
			// Throw and MayThrow: the call may throw), except@28 (the Handler,
			// reached by the MayThrow), E@35 (reached from the Handler and
			// the Throw), x = 1@40, return x@47. Succ: raise E()→{except,
			// E}; except→E→{x = 1, EXIT}. IPDom: raise E() → E → EXIT.
			name:     "raise of a call is a Throw that may throw",
			protects: "`raise E()` evaluates a call, so it reaches the handler part-way as well as by its Throw",
			mutation: "treat every raise as a Throw alone (except@28 disappears, and raise E()@17 -> except@28 with it)",
			src:      "def f():\n try:\n  raise E()\n except E:\n  x = 1\n return x\n",
			fn:       1,
			cd:       []string{"raise E()@17 -> except@28", "E@35 -> x = 1@40", "E@35 -> return x@47"},
			du:       []string{"x = 1@40 -> return x@47"},
		},
		{
			// §6.7, §6.10, §8.4. Lines at 0, 13, 19, 35, 41, 52, 60. Nodes:
			// a@6, b@9, y = a + b < a@21 (arithmetic and a comparison: not
			// MayThrow), g()@37 (may throw), except@42 (Handler, from g()
			// alone), E@49, y = 0@54, return y@61. Succ: y = …→g()→{return y,
			// except}; except→E→{y = 0, EXIT}. IPDom: y = … → g() → EXIT.
			name:     "arithmetic and comparisons do not throw",
			protects: "an operator's node inside a try has no edge to the handler, so only the call before it is a controller",
			mutation: "count an arithmetic operator or a comparison toward MayThrow (y = a + b < a@21 gains an edge to except@42 and controls g()@37)",
			src:      "def f(a, b):\n try:\n  y = a + b < a\n  g()\n except E:\n  y = 0\n return y\n",
			fn:       1,
			cd: []string{"g()@37 -> return y@61", "g()@37 -> except@42", "g()@37 -> E@49",
				"E@49 -> y = 0@54", "E@49 -> return y@61"},
			du: []string{"a@6 -> y = a + b < a@21", "b@9 -> y = a + b < a@21",
				"y = a + b < a@21 -> return y@61", "y = 0@54 -> return y@61"},
		},
		{
			// §7.2.2, §8.7: annotations are not evaluated where they stand.
			// Lines at 0, 13, 23, 42, 49. Nodes: T@6, a@9, y: T = a@14 (reads
			// a only), def g(p: T) -> T: pass@24 (defines g, reads nothing:
			// neither the parameter's nor the return annotation), return y,
			// g@50.
			name:     "annotations are read by no node",
			protects: "an annotated assignment's, a parameter's and a function's type are not reads of the variables they name",
			mutation: "read an annotation (T@6 -> y: T = a@14 or T@6 -> def g(p: T) -> T: pass@24 appears)",
			src:      "def f(T, a):\n y: T = a\n def g(p: T) -> T:\n  pass\n return y, g\n",
			fn:       1,
			du: []string{"a@9 -> y: T = a@14", "y: T = a@14 -> return y, g@50",
				"def g(p: T) -> T: pass@24 -> return y, g@50"},
		},
		{
			// §6.11, §6.12. Lines at 0, 10, 28. Nodes: a@6, a@15 (Branch:
			// defines the result variable), x := 1@20 (defines x and the
			// operator's result variable), y = a or (x := 1)@11 (Uses the
			// result variable only), return y@29. y reads no x, so the pair
			// from x := 1 exists only through the result.
			name:     "an assignment expression as an operand defines the operator's result",
			protects: "`x := 1` as the operand after `or`'s deciding one yields the operator's value through its result variable",
			mutation: "let an assignment expression operand define only its own target (x := 1@20 -> y = a or (x := 1)@11 disappears)",
			src:      "def f(a):\n y = a or (x := 1)\n return y\n",
			fn:       1,
			cd:       []string{"a@15 -> x := 1@20"},
			du: []string{"a@6 -> a@15", "a@15 -> y = a or (x := 1)@11", "x := 1@20 -> y = a or (x := 1)@11",
				"y = a or (x := 1)@11 -> return y@29"},
		},
		{
			// §8.5: a pattern target is bound inside the item's finally.
			// Lines at 0, 10, 29, 36. Nodes: m@6, m as (a, b)@16 (enter:
			// defines the manager and the entered value's variable), a@22
			// (the first binding: unpacking may throw, into the item's
			// finally), b@25, with@11 (the Handler), with m as (a, b)@11 (the
			// exit), return a@37. Succ: a@22→{b@25, with@11}; b@25, with@11 →
			// exit→{EXIT (the re-raise), return a}. IPDom: a@22 → exit → EXIT.
			name:     "a with item's pattern target is bound inside the item's finally from the entered value",
			protects: "a failing unpacking into a with item's pattern target runs the exit, and each name is bound from the entered value, not from the context expression's reads",
			mutation: "bind a pattern target before opening the item's finally (with@11 disappears and the exit controls nothing), or from the context expression's reads (m@6 -> a@22 and m@6 -> b@25 replace the pairs from m as (a, b)@16)",
			src:      "def f(m):\n with m as (a, b):\n  pass\n return a\n",
			fn:       1,
			cd:       []string{"a@22 -> b@25", "a@22 -> with@11", "with m as (a, b)@11 -> return a@37"},
			du: []string{"m@6 -> m as (a, b)@16", "m as (a, b)@16 -> a@22", "m as (a, b)@16 -> b@25",
				"m as (a, b)@16 -> with m as (a, b)@11", "a@22 -> return a@37"},
		},
		{
			// §8.5: the items nest. Lines at 0, 13, 35, 41. Nodes: a@6, b@9,
			// a as x@19 (enter x), b as y@27 (enter y: may throw, into x's
			// finally), g()@37 (may throw, into y's finally), ,@25 (y's
			// finally's Handler, spanning the comma before the item), with a
			// as x, b as y@14 (y's exit: may throw, into x's finally), with@14
			// (x's finally's Handler), with a as x@14 (x's exit), return x@42.
			// Succ: a as x→b as y→{g(), with@14}; g()→{y's exit, ,@25};
			// ,@25→y's exit; y's exit→{x's exit, with@14}; with@14→x's exit;
			// x's exit→{EXIT, return x}. IPDom: g(), ,@25 → y's exit; b as y,
			// y's exit, with@14 → x's exit → EXIT.
			name:     "a later with item's Handler spans the comma before it",
			protects: "an exception in the body enters the innermost item's finally through a Handler of its own, spanning that item's comma",
			mutation: "span every item's Handler at the with keyword (,@25 renders with@14, and g()@37 -> ,@25 becomes g()@37 -> with@14), or give the later item no finally Handler (,@25 disappears)",
			src:      "def f(a, b):\n with a as x, b as y:\n  g()\n return x\n",
			fn:       1,
			cd: []string{"b as y@27 -> g()@37", "b as y@27 -> with a as x, b as y@14", "b as y@27 -> with@14",
				"g()@37 -> ,@25", "with a as x, b as y@14 -> with@14", "with a as x@14 -> return x@42"},
			du: []string{"a@6 -> a as x@19", "b@9 -> b as y@27", "a as x@19 -> with a as x@14",
				"b as y@27 -> with a as x, b as y@14", "a as x@19 -> return x@42"},
		},
		{
			// §8.6.4.6, §8.6.4.10. Lines at 0, 16, 26, 39, 51, 63, 75. Nodes:
			// s@6, C@9, k@12, s@23 (the subject), C(x)@33 (Branch: Uses the
			// subject's variable and C, the class it names), x@35 (capture),
			// return x@42, k.V@58 (Branch: Uses the subject's variable and k,
			// the dotted value's first name), return 1@66, return 0@76. Succ:
			// s@23→C(x)→{x@35, k.V}; x@35→return x; k.V→{return 1, return 0}.
			// IPDom: C(x), k.V → EXIT; x@35 → return x.
			name:     "a case node reads the class and the dotted value its pattern names",
			protects: "a class pattern's class and a value pattern's dotted name are reads of the case node, and a class pattern's argument is a capture",
			mutation: "read nothing for a pattern's class or dotted value (C@9 -> C(x)@33 and k@12 -> k.V@58 disappear), or take the class name for a capture (C@9 -> C(x)@33 disappears and a node C@33 appears)",
			src:      "def f(s, C, k):\n match s:\n  case C(x):\n   return x\n  case k.V:\n   return 1\n return 0\n",
			fn:       1,
			cd: []string{"C(x)@33 -> x@35", "C(x)@33 -> return x@42", "C(x)@33 -> k.V@58",
				"k.V@58 -> return 1@66", "k.V@58 -> return 0@76"},
			du: []string{"s@6 -> s@23", "s@23 -> C(x)@33", "C@9 -> C(x)@33", "s@23 -> x@35", "x@35 -> return x@42",
				"s@23 -> k.V@58", "k@12 -> k.V@58"},
		},
		{
			// print_statement of the pinned grammar (the Python 2 form, which
			// no section of the 3.13 reference defines). Lines at 0, 10.
			// Nodes: a@6, print a@11 (a plain Stmt node reading a).
			name:     "a print statement is a plain node",
			protects: "a kind the lowering does not name is still one node with its reads",
			mutation: "make no node for a kind the lowering does not name (a@6 -> print a@11 disappears)",
			src:      "def f(a):\n print a\n",
			fn:       1,
			du:       []string{"a@6 -> print a@11"},
		},
		{
			// §6.13, §6.12, lower.go (the hand-off). Lines at 0, 13, 45.
			// Nodes: x@6, c@9, c@35 (Branch), x := 1@24 (the true arm: defines
			// x and the result variable; it does not run whenever the
			// consumer does, so the earlier read of x stays on the consumer),
			// 2@42 (the false arm, defining the result variable), y = x + ((x
			// := 1) if c else 2)@14 (Uses x and the result variable), return
			// y@46. Succ: c→{x := 1, 2}→y = …→return y. x := 1 pairs with the
			// consumer through x and the result: one pair.
			name:     "an assignment expression in a conditional arm leaves an earlier read on its consumer",
			protects: "when the arm holding `x := 1` is not chosen, the consumer's earlier read of x still pairs with the x before it",
			mutation: "hand the earlier read off to an assignment expression in an arm (x@6 -> y = x + ((x := 1) if c else 2)@14 disappears and x@6 -> x := 1@24 appears)",
			src:      "def f(x, c):\n y = x + ((x := 1) if c else 2)\n return y\n",
			fn:       1,
			cd:       []string{"c@35 -> x := 1@24", "c@35 -> 2@42"},
			du: []string{"c@9 -> c@35", "x@6 -> y = x + ((x := 1) if c else 2)@14",
				"x := 1@24 -> y = x + ((x := 1) if c else 2)@14", "2@42 -> y = x + ((x := 1) if c else 2)@14",
				"y = x + ((x := 1) if c else 2)@14 -> return y@46"},
		},
		{
			// §6.10, §6.12, lower.go (the hand-off). The assignment
			// expression sits inside the last operand, so that operand's node
			// and the assignment's render apart. Lines at 0, 16, 48. Nodes:
			// x@6, a@9, b@12, a < b@26 (Branch: defines the result variable
			// and holds b), x := 1@35 (defines x and its own result variable;
			// it runs only when a < b holds, so the earlier read of x stays on
			// the consumer), (x := 1) + 0@34 (the last operand: Uses that
			// result and the held b, defines the chain's result), y = …@17
			// (Uses x and the chain's result), return y@49. Succ: a < b→{x :=
			// 1, y = …}; x := 1→(x := 1) + 0→y = …→return y.
			name:     "an assignment expression in a later chain operand leaves an earlier read on its consumer",
			protects: "when the chain fails before the operand holding `x := 1`, the consumer's earlier read of x still pairs with the x before it",
			mutation: "hand the earlier read off to an assignment expression in a later chain operand (x@6 -> y = x + (a < b < (x := 1) + 0)@17 disappears and x@6 -> x := 1@35 appears)",
			src:      "def f(x, a, b):\n y = x + (a < b < (x := 1) + 0)\n return y\n",
			fn:       1,
			cd:       []string{"a < b@26 -> x := 1@35", "a < b@26 -> (x := 1) + 0@34"},
			du: []string{"a@9 -> a < b@26", "b@12 -> a < b@26", "a < b@26 -> (x := 1) + 0@34",
				"x := 1@35 -> (x := 1) + 0@34", "a < b@26 -> y = x + (a < b < (x := 1) + 0)@17",
				"(x := 1) + 0@34 -> y = x + (a < b < (x := 1) + 0)@17", "x@6 -> y = x + (a < b < (x := 1) + 0)@17",
				"x := 1@35 -> y = x + (a < b < (x := 1) + 0)@17", "y = x + (a < b < (x := 1) + 0)@17 -> return y@49"},
		},
		{
			// §6.2.5, §8.4. Lines at 0, 10, 16, 27, 38, 46. Nodes: a@6, y =
			// [*a]@18 (the star unpacking may throw), except@28 (Handler),
			// E@35, y = 0@40, return y@47. Succ: y = [*a]→{return y, except};
			// except→E→{y = 0, EXIT}.
			name:     "a star unpacking may throw",
			protects: "a node unpacking a value with * inside a try reaches the handler",
			mutation: "count no throw for a star unpacking (except@28 disappears and y = [*a]@18 controls nothing)",
			src:      "def f(a):\n try:\n  y = [*a]\n except E:\n  y = 0\n return y\n",
			fn:       1,
			cd: []string{"y = [*a]@18 -> return y@47", "y = [*a]@18 -> except@28", "y = [*a]@18 -> E@35",
				"E@35 -> y = 0@40", "E@35 -> return y@47"},
			du: []string{"a@6 -> y = [*a]@18", "y = [*a]@18 -> return y@47", "y = 0@40 -> return y@47"},
		},
		{
			// §8.3, §8.4. Lines at 0, 11, 17, 32, 41, 52, 60. Nodes: xs@6,
			// xs@28 (the iterable: creating the iterator may throw), x in
			// xs@23 (head: each step may throw), x@23 (target), except@42
			// (Handler, from the iterable and the head), E@49, x = 0@54,
			// return x@61. Succ: xs@28→{head, except}; head→{x@23, return x,
			// except}; x@23→head; except→E→{x = 0, EXIT}. IPDom: xs@28, head,
			// E → EXIT; x@23 → head; except → E.
			name:     "a for loop's iterator creation and step may throw",
			protects: "the iterable's node and the loop head inside a try each reach the handler",
			mutation: "count no throw for iter() (xs@28 -> except@42 and xs@28 -> E@49 disappear), or for next() (x in xs@23 -> except@42 and x in xs@23 -> E@49 disappear)",
			src:      "def f(xs):\n try:\n  for x in xs:\n   pass\n except E:\n  x = 0\n return x\n",
			fn:       1,
			cd: []string{"xs@28 -> x in xs@23", "xs@28 -> except@42", "xs@28 -> E@49",
				"x in xs@23 -> x@23", "x in xs@23 -> x in xs@23", "x in xs@23 -> return x@61",
				"x in xs@23 -> except@42", "x in xs@23 -> E@49", "E@49 -> x = 0@54", "E@49 -> return x@61"},
			du: []string{"xs@6 -> xs@28", "xs@28 -> x in xs@23", "xs@28 -> x@23",
				"x@23 -> return x@61", "x = 0@54 -> return x@61"},
		},
		{
			// §8.8, §6.2.4, §8.4. Lines at 0, 11, 17, 28, 37, 59, 70, 78.
			// Nodes: xs@6, class C: pass@19 (class creation may throw),
			// [x for x in xs]@43 (the comprehension's creation may throw;
			// reads xs, defines the result variable), y = [x for x in
			// xs]@39 (Uses the result variable; throws nothing of its own),
			// except@60 (Handler), E@67, y = 0@72, return y@79. Succ: class→
			// {[…], except}; […]→{y = […], except}; y = […]→return y;
			// except→E→{y = 0, EXIT}. IPDom: class, […], E → EXIT; y = […]
			// → return y.
			name:     "creating a class or a comprehension may throw",
			protects: "a class statement's node and a comprehension's creating node inside a try each reach the handler",
			mutation: "count no throw for a class creation (class C: pass@19 controls nothing), or for a comprehension's creation (the comprehension's node controls only y = [x for x in xs]@39 and return y@79 through it)",
			src:      "def f(xs):\n try:\n  class C:\n   pass\n  y = [x for x in xs]\n except E:\n  y = 0\n return y\n",
			fn:       1,
			cd: []string{"class C: pass@19 -> [x for x in xs]@43", "class C: pass@19 -> except@60", "class C: pass@19 -> E@67",
				"[x for x in xs]@43 -> y = [x for x in xs]@39", "[x for x in xs]@43 -> return y@79",
				"[x for x in xs]@43 -> except@60", "[x for x in xs]@43 -> E@67",
				"E@67 -> y = 0@72", "E@67 -> return y@79"},
			du: []string{"xs@6 -> [x for x in xs]@43", "[x for x in xs]@43 -> y = [x for x in xs]@39",
				"y = [x for x in xs]@39 -> return y@79", "y = 0@72 -> return y@79"},
		},
		{
			// §8.6.4.8, §8.4. Lines at 0, 10, 16, 27, 40, 49, 60, 68. Nodes:
			// s@6, s@24 (the subject), [x]@35 (Branch: a sequence pattern
			// may throw), x@36 (capture), except@50 (Handler), E@57, x =
			// 0@62, return x@69. Succ: s@24→[x]→{x@36, return x (no case
			// matched), except}; x@36→return x; except→E→{x = 0, EXIT}.
			// IPDom: [x], E → EXIT; x@36 → return x.
			name:     "a sequence pattern may throw",
			protects: "a case node testing a sequence pattern inside a try reaches the handler",
			mutation: "count no throw for a sequence pattern (except@50 disappears and [x]@35 controls only x@36 and return x@69)",
			src:      "def f(s):\n try:\n  match s:\n   case [x]:\n    pass\n except E:\n  x = 0\n return x\n",
			fn:       1,
			cd: []string{"[x]@35 -> x@36", "[x]@35 -> return x@69", "[x]@35 -> except@50", "[x]@35 -> E@57",
				"E@57 -> x = 0@62", "E@57 -> return x@69"},
			du: []string{"s@6 -> s@24", "s@24 -> [x]@35", "s@24 -> x@36", "x@36 -> return x@69", "x = 0@62 -> return x@69"},
		},
		{
			// §6.2.4, §4.2.2. Lines at 0, 14, 34. Nodes: xs@6, x@10, [x for x
			// in xs]@19 (the creating node: reads xs, its first iterable; its
			// element's x is its own target, so it captures nothing), y =
			// …@15, return y, x@35 (reads f's x, the parameter).
			name:     "a comprehension's for target is its own variable",
			protects: "a comprehension's target neither reads nor rebinds the enclosing function's variable of the same name",
			mutation: "resolve the element's x in the enclosing function (x@10 -> [x for x in xs]@19 appears), or bind the target there (x@10 -> return y, x@35 is replaced by a pair from the comprehension's node)",
			src:      "def f(xs, x):\n y = [x for x in xs]\n return y, x\n",
			fn:       1,
			du: []string{"xs@6 -> [x for x in xs]@19", "[x for x in xs]@19 -> y = [x for x in xs]@15",
				"y = [x for x in xs]@15 -> return y, x@35", "x@10 -> return y, x@35"},
		},
		{
			// §8.8, §4.2.2, the class body (callable 1). Lines at 0, 9, 16.
			// Nodes: a = 1@10, b = a@17. The class body is its own callable
			// and its names are visible to its own code.
			name:     "a class body is its own callable and reads its own names",
			protects: "a class body's statements are lowered as a function whose names resolve in the class scope",
			mutation: "hide the class scope from the class body too (a = 1@10 -> b = a@17 disappears)",
			src:      "class C:\n a = 1\n b = a\n",
			fn:       1,
			du:       []string{"a = 1@10 -> b = a@17"},
		},
		{
			// §7.12, §4.2.2. Every name the statements bind is declared
			// global, so each resolves to no variable. Lines at 0, 10, 28,
			// 38, 48, 55, 69, 79, 91, 100. Nodes: s@6, a@36 (the import's
			// node, defining nothing), def g(): pass@39 (defining nothing),
			// type T = int@56 (defining nothing), s@76 (the subject), [c]@86
			// (Branch), c@87 (the capture, defining nothing), return a, g, T,
			// c@101 (reading nothing). The sequence pattern keeps the case
			// and capture nodes apart. Succ: s@76→[c]→{c@87, return};
			// c@87→return.
			name:     "an unresolved import, def name, type alias or capture makes its node and defines nothing",
			protects: "a name declared global is no variable in these binding positions either, and each construct still makes its node",
			mutation: "drop def's v < 0 return (Builder.Def panics on -1), or bind these names as locals (a@36, def g(): pass@39, type T = int@56 and c@87 each pair with return a, g, T, c@101)",
			src:      "def f(s):\n global a, g, T, c\n import a\n def g():\n  pass\n type T = int\n match s:\n  case [c]:\n   pass\n return a, g, T, c\n",
			fn:       1,
			cd:       []string{"[c]@86 -> c@87"},
			du:       []string{"s@6 -> s@76", "s@76 -> [c]@86", "s@76 -> c@87"},
		},
	})
}
