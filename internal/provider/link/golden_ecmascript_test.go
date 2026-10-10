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
			"r1 foo -> foo foo exact",
			"r2 C -> cls C exact",
			"r2 m -> m m exact",
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
			"r1 doWork -> work work exact",
			"r2 other -> other other exact",
			"rns work -> work work exact",
			"r4 w -> work work exact",
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
			"a1 helper -> ha helper exact",
			"b1 helper -> hb helper exact",
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
			"call cb -> tick tick exact",
			"top run -> run run exact",
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
			"n1 Square -> sq Square exact",
			"n2 Circle -> ci Circle exact",
			"a area -> sq area exact set=2",
			"a area -> ci area exact set=2",
			"n3 Repo -> repo Repo exact",
			"sv save -> repo save exact",
			"b1 Square -> sq Square exact",
			"b1 area -> sq area exact",
			"b2 Circle -> ci Circle exact",
			"b2 area -> ci area exact",
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
			"n Kid -> bc constructor exact",
			"h hello -> bh hello exact",
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
			"c1 f -> mf f exact",
			"c2 h -> zh h exact",
			"c3 k -> yk k exact",
			"c6 f -> mf f exact",
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
			"c1 b -> a a exact",
			"c3 b -> a a exact",
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
		// the binding rules alone); the two agree on every binding. `this` is a C or an instance of a
		// subclass, which may redefine the field, so the call through it is virtual.
		want: []string{
			"r handler -> fh handler virtual",
			"n C -> cls C exact",
			"g run -> run run exact",
		},
		fails: "a class field holding a function not recorded as a value of that field leaves `this.handler()` unbound",
	},
	{
		name:   "value-flow-and-a-constructed-object",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors), §13.3.5 (The new Operator: the object `new C()` returns has C.prototype as its prototype), §13.3.7 (The super Keyword: the home object's prototype's method), §15.7 (Class Definitions)",
		files: map[string]string{
			"src/animals.js": `class Animal {
  sound() {}
}
class Dog extends Animal { // @dog
  sound() {} // @ds
}
class Cat extends Animal { // @cat
  sound() {} // @cs
}
class Puppy extends Dog {
  sound() {
    super.sound(); // @sup
  }
}
function speak(a) { // @speak
  a.sound(); // @s1
}
export function main() {
  speak(new Dog()); // @m1
  speak(new Cat()); // @m2
  new Dog().sound(); // @m3
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. `a` holds exactly a Dog and a Cat, so
		// `a.sound()` calls each one's method, exact, two targets; `new Dog().sound()` is Dog's own, never
		// Puppy's override; `super.sound()` in Puppy is Dog's.
		want: []string{
			"s1 sound -> ds sound exact set=2",
			"s1 sound -> cs sound exact set=2",
			"sup sound -> ds sound exact",
			"m1 speak -> speak speak exact",
			"m1 Dog -> dog Dog exact",
			"m2 speak -> speak speak exact",
			"m2 Cat -> cat Cat exact",
			"m3 Dog -> dog Dog exact",
			"m3 sound -> ds sound exact",
		},
		fails: "a construction's result dispatched into subclasses binds Puppy.sound from `new Dog().sound()`; a placeholder for the value set binds no method; dispatch read from the technique makes `super.sound()` or a flow-bound call virtual",
	},
	{
		name:   "declared-types",
		anchor: "TypeScript Language Specification §3.11 (Type relationships: assignment compatibility admits subclass instances), §4.15 (Property access); ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors), §13.3.5 (The new Operator)",
		files: map[string]string{
			"animals.ts": `class Animal {
    sound(): void {} // @as
}

class Dog extends Animal { // @dog
    sound(): void {} // @ds
}

class Cat extends Animal { // @cat
    sound(): void {}
}

function speak(a: Animal): void { // @speak
    a.sound(); // @s1
}

function direct(d: Dog): void { // @direct
    d.sound(); // @s2
}

interface Speaker {
    talk(): void; // @st
}

class Parrot implements Speaker { // @parrot
    talk(): void {}
}

class Robot implements Speaker { // @robot
    talk(): void {}
}

function chat(s: Speaker): void { // @chat
    s.talk(); // @s3
}

function main(): void { // @main
    speak(new Dog()); // @m1
    speak(new Cat()); // @m2
    direct(new Dog()); // @m3
    chat(new Parrot()); // @m4
    chat(new Robot()); // @m5
}

main(); // @top
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. A receiver of a declared type calls the
		// method that type declares, once, virtual: Animal's for `a`, Dog's for `d`, the interface's own
		// signature for `s`; the overrides are reached at query time.
		want: []string{
			"s1 sound -> as sound virtual",
			"s2 sound -> ds sound virtual",
			"s3 talk -> st talk virtual",
			"m1 speak -> speak speak exact",
			"m1 Dog -> dog Dog exact",
			"m2 speak -> speak speak exact",
			"m2 Cat -> cat Cat exact",
			"m3 direct -> direct direct exact",
			"m3 Dog -> dog Dog exact",
			"m4 chat -> chat chat exact",
			"m4 Parrot -> parrot Parrot exact",
			"m5 chat -> chat chat exact",
			"m5 Robot -> robot Robot exact",
			"top main -> main main exact",
		},
		fails: "a declared type published per override (the subtype walk) gives `a.sound()` three targets and `s.talk()` the two classes' methods; a declared type's call made exact loses the override",
	},
	{
		name:   "typescript-external-implements-and-extends",
		anchor: "TypeScript Language Specification §8.1.1 (Class Heritage Specification: the `extends` clause names the base class, the `implements` clause the interfaces the class implements); TypeScript 4.3 `override` modifier (a member that overrides a base class member); ECMA-262 (15th edition, 2024) §16.2.2 (Imports)",
		files: map[string]string{
			"tasks.ts": `import { Runnable } from "tasks";
import { Thread } from "threads";

class A implements Runnable { // @ta
    run(): void {} // @arun
}

class B extends Thread { // @tb
    override run(): void {} // @brun
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. Both modules are outside the project, so
		// each imported name is its module's: an `implements` clause names an interface, an `extends`
		// clause a class. A's run carries no `override` modifier, so it publishes no override of a type
		// outside the project. Unprobed: no TypeScript compiler checked the source.
		want: []string{},
		facts: []string{
			"ta A implements external tasks.Runnable",
			"tb B extends external threads.Thread",
			"brun run overrides external threads.Thread.run",
		},
		// Mutation: read every external supertype of a class as a class -> `A implements Runnable` is
		// published as `A extends Runnable`.
		fails: "a class's `implements` clause naming a type outside the project published as `extends` of an external class",
	},
	{
		name:   "super-under-declared-types",
		anchor: "TypeScript Language Specification §4.9.2 (Super property access), §3.11; ECMA-262 (15th edition, 2024) §13.3.7 (The super Keyword), §13.3.5 (The new Operator)",
		files: map[string]string{
			"deep.ts": `class Animal {
    sound(): void {} // @as
}

class Dog extends Animal { // @dog
    sound(): void {} // @ds
}

class Puppy extends Dog { // @puppy
    sound(): void {
        super.sound(); // @sup
    }
}

function speak(a: Animal): void { // @speak
    a.sound(); // @s1
}

function walk(d: Dog): void { // @walk
    d.sound(); // @s2
}

speak(new Puppy()); // @t1
walk(new Puppy()); // @t2
new Dog().sound(); // @t3
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"sup sound -> ds sound exact",
			"s1 sound -> as sound virtual",
			"s2 sound -> ds sound virtual",
			"t1 speak -> speak speak exact",
			"t1 Puppy -> puppy Puppy exact",
			"t2 walk -> walk walk exact",
			"t2 Puppy -> puppy Puppy exact",
			"t3 Dog -> dog Dog exact",
			"t3 sound -> ds sound exact",
		},
		fails: "dispatch read from the technique makes `super.sound()` virtual; `new Dog().sound()` dispatched into Dog's subclasses binds Puppy.sound",
	},
	{
		name:   "imports-between-files",
		anchor: "ECMA-262 (15th edition, 2024) §16.2.2 (Imports), §16.2.1.7 (HostLoadImportedModule: a relative specifier names a file)",
		files: map[string]string{
			"src/a.js": `export function f() {} // @f
`,
			"src/main.js": `import { f } from './a.js';

export function run() {
  f(); // @r
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding and every import.
		want: []string{
			"r f -> f f exact",
		},
		facts: []string{
			"src/main.js imports src/a.js",
		},
		fails: "an import published to the imported declaration's node, or to nothing, instead of the file of its module loses the `imports`",
	},
	{
		name:   "spread-arguments",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.8.1 (ArgumentListEvaluation: a SpreadElement expands into the remaining arguments), §15.1 (FunctionRestParameter collects them)",
		files: map[string]string{
			"src/sum.js": `function sum(first, ...rest) { // @sum
  return first;
}
export function run(a, b, c) { // @run
  sum(a, ...b); // @c1
  sum(...c); // @c2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding and every flow. A spread's elements fill
		// the parameters from its position on, whose count the source does not say, so it binds the rest
		// parameter that collects the excess.
		want: []string{
			"c1 sum -> sum sum exact",
			"c2 sum -> sum sum exact",
		},
		facts: []string{
			"run a flows sum first",
			"run b flows sum rest",
			"run c flows sum rest",
		},
		fails: "a spread matched by position flows c into first and b into nothing",
	},
	{
		name:   "required-modules",
		anchor: "Node.js Modules: CommonJS modules (`require(id)` loads the module id names; a relative id is a file); ECMA-262 (15th edition, 2024) §13.15 (an expression that does not fold names no module)",
		files: map[string]string{
			"src/load.js": `export function load(name) {
  const a = require("./x/" + name);
  const b = require("./x");
  return [a, b];
}
`,
			"src/x.js": `export const v = 1;
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every import. The first path is an expression whose only
		// literal part is "./x/"; the second a constant naming src/x.js. Neither `require` call names a
		// declaration of the unit.
		want: []string{},
		facts: []string{
			"src/load.js imports open ./x/",
			"src/load.js imports src/x.js",
		},
		fails: "every require treated as open loses the import of src/x.js; a computed path resolved by its fragment imports a file it may not name",
	},
	{
		name:   "open-and-computed-sites",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.6 (Function Calls), §13.3.2 (Property Accessors: a computed member), §9.4.2 (ResolveBinding: an unresolvable reference)",
		files: map[string]string{
			"src/open.js": `class Box { // @box
  open() {} // @bo
}
export function run(obj, k, cb) {
  obj[k](); // @comp
  cb(); // @ind
  obj.close(); // @mem
  missing(); // @bare
  new Box().open(); // @ok
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every site. Nothing reaches run's parameters and nothing
		// declares missing: each of those calls is open for the reason its shape gives; the computed member's
		// callee names nothing; the construction and its method are bound.
		want: []string{
			"ok Box -> box Box exact",
			"ok open -> bo open exact",
		},
		sites: []string{
			"comp obj[k] open computed value 0",
			"ind cb open value_untracked value 0",
			"mem close open receiver_untyped member 0",
			"bare missing open global_unbound bare 0",
		},
		counts: map[string]string{
			"sites": "6", "bound_sites": "2", "open_sites": "3", "computed_sites": "1", "external_sites": "0",
		},
		fails: "a computed callee counted as open, or a bound site counted open, breaks the counts; every open site given one reason misnames three",
	},
}
