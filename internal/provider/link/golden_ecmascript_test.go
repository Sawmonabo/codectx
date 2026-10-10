package link_test

import "testing"

// TestECMAScriptGoldens runs the JavaScript goldens through the real structural provider and the
// linker (runGoldens). Each case names the wrong answer it catches in fails.
func TestECMAScriptGoldens(t *testing.T) { runGoldens(t, ecmascriptGoldens) }

var ecmascriptGoldens = []golden{
	{
		name:   "default-exports",
		anchor: "ECMA-262 (15th edition, 2024) §16.2.2 (Imports), §16.2.3 (Exports: `export default`), §13.3.5 (The new Operator), §13.3.6 (Function Calls)",
		files: map[string]string{
			"src/a.js": `function foo() {} // @foo
export default foo;
`,
			"src/b.js": `export default class C { // @cls
  m() {} // @m
}
`,
			"src/main.js": `import foo from './a.js';
import C from './b.js';

export function run() {
  foo(); // @r1
  new C().m(); // @r2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r1 foo -> foo foo",
			"r2 C -> cls C",
			"r2 m -> m m",
		},
		fails: "a default export not followed from the importer's local name leaves foo, C and m unbound",
	},
	{
		name:   "reexports-and-renames",
		anchor: "ECMA-262 (15th edition, 2024) §16.2.2 (Imports: ImportSpecifier `as`, NameSpaceImport), §16.2.3 (Exports: ExportSpecifier `as`, `export * from`), §16.2.1.6.3 (ResolveExport)",
		files: map[string]string{
			"src/lib/impl.js": `export function work() {} // @work
export function other() {} // @other
`,
			"src/lib/index.js": `export { work as doWork } from './impl.js';
export * from './impl.js';
`,
			"src/main.js": `import { doWork, other } from './lib/index.js';
import * as lib from './lib/index.js';
import { work as w } from './lib/impl.js';

export function run() {
  doWork(); // @r1
  other(); // @r2
  lib.work(); // @rns
  w(); // @r4
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r1 doWork -> work work",
			"r2 other -> other other",
			"rns work -> work work",
			"r4 w -> work work",
		},
		fails: "re-exports, `export *`, renamed exports and renamed imports not followed leave the four calls unbound or bound by local name",
	},
	{
		name:   "lexical-scope",
		anchor: "ECMA-262 (15th edition, 2024) §9.1 (Environment Records), §9.4.2 (ResolveBinding), §13.3.6 (Function Calls)",
		files: map[string]string{
			"src/scope.js": `function a() {
  function helper() { return 1; } // @ha
  return helper(); // @a1
}
function b() {
  function helper() { return 2; } // @hb
  return helper(); // @b1
}
function c() {
  return helper(); // @c1
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"a1 helper -> ha helper",
			"b1 helper -> hb helper",
		},
		fails: "a candidate kept by name alone, outside the caller's scope, binds each helper call to both inner helpers and c's to either",
	},
	{
		name:   "function-values",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.6 (Function Calls: EvaluateCall of a function value), §15.7 (Class Definitions: methods are properties of the prototype, not bindings)",
		files: map[string]string{
			"src/tick.js": `function tick() {} // @tick
class Clock {
  tick() {} // @mtick
}
function run(cb) { // @run
  cb(); // @call
}
run(tick); // @top
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"call cb -> tick tick",
			"top run -> run run",
		},
		fails: "a function value matched by name binds `cb()` to the method Clock.tick",
	},
	{
		name:   "reaching-types-and-blocks",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors), §13.3.6 (Function Calls), §14.2 (Block: a block's lexical environment), §14.3.1 (let and const declarations)",
		files: map[string]string{
			"src/shapes.js": `class Square { area() { return 4; } } // @sq
class Circle { area() { return 3; } } // @ci
class Repo { save() {} } // @repo
const cache = { save() {} }; // @cache
function shapes(flag) {
  let s = new Square(); // @n1
  if (flag) { s = new Circle(); } // @n2
  return s.area(); // @a
}
function store() {
  const repo = new Repo(); // @n3
  repo.save(); // @sv
}
function blocks() {
  { const x = new Square(); x.area(); } // @b1
  { const x = new Circle(); x.area(); } // @b2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"n1 Square -> sq Square",
			"n2 Circle -> ci Circle",
			"a area -> sq area",
			"a area -> ci area",
			"n3 Repo -> repo Repo",
			"sv save -> repo save",
			"b1 Square -> sq Square",
			"b1 area -> sq area",
			"b2 Circle -> ci Circle",
			"b2 area -> ci area",
		},
		fails: "two reaching definitions merged to no type lose s.area(); field-based binding beside a known type binds the object literal's save; flat block scopes cross the two x",
	},
	{
		name:   "inherited-constructor",
		anchor: "ECMA-262 (15th edition, 2024) §15.7.14 (ClassDefinitionEvaluation: a derived class's default constructor calls super(...args)), §13.3.5 (The new Operator)",
		files: map[string]string{
			"src/kid.js": `class Base {
  constructor(x) { this.x = x; } // @bc
  hello() {} // @bh
}
class Kid extends Base {}
function make() {
  const k = new Kid(1); // @n
  k.hello(); // @h
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"n Kid -> bc constructor",
			"h hello -> bh hello",
		},
		fails: "a constructor not inherited by a derived class binds `new Kid(1)` to the class instead of Base's constructor",
	},
	{
		name:   "resolve-export-precedence",
		anchor: "ECMA-262 (15th edition, 2024) §16.2.1.6.3 (ResolveExport: own export, then indirect export, then star exports; `default` never through `export *`; two star sources disagreeing are ambiguous), §10.4.6 (Module Namespace Exotic Objects)",
		files: map[string]string{
			"src/x.js": `export function f() {} // @xf
export function g() {} // @xg
export function h() {} // @xh
export default function dx() {} // @dx
`,
			"src/y.js": `export function g() {} // @yg
export function h() {} // @yh
export function k() {} // @yk
`,
			"src/z.js": `export function h() {} // @zh
`,
			"src/m.js": `export function f() {} // @mf
export { h } from './z.js';
export * from './x.js';
export * from './y.js';
`,
			"src/main.js": `import { f, h, k } from './m.js';
import * as mm from './m.js';

export function run() {
  f(); // @c1
  h(); // @c2
  k(); // @c3
  mm.g(); // @c4
  mm.default(); // @c5
  mm.f(); // @c6
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"c1 f -> mf f",
			"c2 h -> zh h",
			"c3 k -> yk k",
			"c6 f -> mf f",
		},
		fails: "star exports read before own and named exports, `default` passed through `export *`, or a disagreeing pair resolved to either side gives f, h, g or default the wrong target",
	},
	{
		name:   "export-as-exports-only-the-new-name",
		anchor: "ECMA-262 (15th edition, 2024) §16.2.3 (Exports: ExportSpecifier `a as b` exports the name `b`), §16.2.1.6.3 (ResolveExport), §10.4.6",
		files: map[string]string{
			"src/a.js": `function a() {} // @a
export { a as b };
`,
			"src/main.js": `import { b } from './a.js';
import * as ns from './a.js';

export function run() {
  b(); // @c1
  ns.a(); // @c2
  ns.b(); // @c3
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"c1 b -> a a",
			"c3 b -> a a",
		},
		fails: "an `export {a as b}` that also exports the local name binds `ns.a()`",
	},
	{
		name:   "class-field-arrow",
		anchor: "ECMA-262 (15th edition, 2024) §15.7 (Class Definitions: FieldDefinition, ClassFieldDefinitionEvaluation), §13.3.2 (Property Accessors), §13.3.6 (Function Calls)",
		files: map[string]string{
			"src/c.js": `class C { // @cls
  handler = () => {}; // @fh
  run() { // @run
    this.handler(); // @r
  }
}
function go() {
  const c = new C(); // @n
  c.run(); // @g
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r handler -> fh handler",
			"n C -> cls C",
			"g run -> run run",
		},
		fails: "a class field holding a function not recorded as a value of that field leaves `this.handler()` unbound",
	},
}
