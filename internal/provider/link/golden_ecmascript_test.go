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
		// signature for `s`; the overrides are reached at query time. The TypeScript compiler (strict)
		// reports no diagnostic and resolves each call to the declaration its line names here.
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
			"app.ts": `import { Runnable } from "tasks";
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
		// outside the project. The TypeScript compiler (strict), given declarations of both modules, reports
		// no diagnostic; without them it reports the two missing modules and, its base then unknown, the
		// `override` on B's run.
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
		// the binding rules alone); the two agree on every binding. The TypeScript compiler (strict) reports
		// no diagnostic and resolves each call to the declaration its line names here.
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
		name:   "dynamic-imports",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.10 (Import Calls: `import(specifier)` loads the module the specifier names), §16.2.1.7 (HostLoadImportedModule: a relative specifier names a file)",
		files: map[string]string{
			"src/load.js": `export async function load(name) {
  const a = await import("./x.js");
  const b = await import("./x/" + name);
  return [a, b];
}
`,
			"src/x.js": `export const v = 1;
`,
		},
		// Derived from the source and the anchor. The first specifier is a string literal naming src/x.js, an
		// import as plain as a static one; the second is an expression whose only literal part is "./x/".
		want: []string{},
		facts: []string{
			"src/load.js imports open ./x/",
			"src/load.js imports src/x.js",
		},
		fails: "the literal `import(\"./x.js\")` left uncaptured loses the import of src/x.js",
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
	{
		name:   "property-declared-functions",
		anchor: "ECMA-262 (15th edition, 2024) §13.15.2 (Assignment: `a.b = f` stores f under the property b; `f = g` binds the name f), §13.2.5 (Object Initializer: a property definition stores its value under its key), §13.3.6 (Function Calls)",
		files: map[string]string{
			"src/obj.js": `const obj = {};
obj.run = () => 1; // @orun
obj.run(); // @c1
run(); // @c4
const o = { k: () => 2 }; // @ok
o.k(); // @c2
let f;
f = (a) => a; // @fa
export function go(v) { // @go
  f(v); // @c3
}
`,
		},
		// Derived from the source and the anchor. obj and o hold objects whose run and k properties the
		// file writes with the two arrow functions, and nothing else types either receiver, so each call
		// binds the function its property was written with (the field). No name run is bound anywhere, so
		// the bare call of it is open. f is a name the assignment binds to the third arrow function, so the
		// call in go binds it by scope and passes v to a.
		want: []string{
			"c1 run -> orun run exact",
			"c2 k -> ok k exact",
			"c3 f -> fa f exact",
		},
		sites: []string{
			"c4 run open global_unbound bare 0",
		},
		facts: []string{
			"go v flows fa a",
		},
		fails: "property-assigned declarations skipped leave c1 and c2 unbound; `f = () => …` read as an untyped value leaves c3 open; a property function taken as the binding of its bare name binds c4 to obj.run",
	},
	{
		name:   "commonjs-exports",
		anchor: "Node.js Modules: CommonJS modules (`module.exports` and its `exports` shortcut are what `require(id)` returns); ECMA-262 (15th edition, 2024) §14.3.3 (Destructuring Binding Patterns)",
		files: map[string]string{
			"src/m.js": `module.exports.run = () => 1; // @mrun
exports.stop = (code) => code; // @mstop
`,
			"src/use.js": `const { run, stop: halt } = require("./m");
const m = require("./m");
export function main(n) { // @main
  run(); // @u1
  halt(n); // @u2
  m.run(); // @u3
}
`,
		},
		// Derived from the source and the anchor. src/m.js exports run and stop through module.exports and
		// exports; the destructured names and the required module's member reach them through the module,
		// and n reaches code.
		want: []string{
			"u1 run -> mrun run exact",
			"u2 halt -> mstop stop exact",
			"u3 run -> mrun run exact",
		},
		facts: []string{
			"main n flows mstop code",
			"src/use.js imports src/m.js",
		},
		fails: "a CommonJS export left out of the module's exports leaves every call unbound; a destructured require read as a field binds through no module",
	},
	{
		name:   "an-untyped-receiver-is-not-bound-by-name",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors: the property is looked up on the receiver's value at run time); Node.js Modules: CommonJS modules",
		files: map[string]string{
			"src/m.js": `module.exports.run = () => 1; // @mrun
`,
			"src/other.js": `export function other(x) {
  x.run(); // @u4
}
`,
		},
		// Derived from the source and the anchor. Nothing tells what x holds, and run is written to no
		// property of an object x could hold: src/m.js exports it, which only an import of that module
		// reaches. The call is open.
		want: []string{},
		sites: []string{
			"u4 run open receiver_untyped member 0",
		},
		fails: "a member call bound by its name alone, or a CommonJS export treated as a field write, binds u4 to src/m.js's run",
	},
	{
		name:   "typescript-untyped-annotations",
		anchor: "TypeScript Handbook, Everyday Types (`any`, `unknown` and `object` name no class whose members a call reaches)",
		files: map[string]string{
			"src/a.ts": `export function run(p: object) {
  const o: any = {};
  o.m(); // @u5
  const u: unknown = 1;
  (u as any).n(); // @u6
  p.q(); // @u7
}
`,
		},
		// Derived from the source and the anchor. None of the annotations names a class, so each receiver is
		// untyped and each call open, never a call into an external type of that name.
		want: []string{},
		sites: []string{
			"u5 m open receiver_untyped member 0",
			"u6 n open receiver_untyped member 0",
			"u7 q open receiver_untyped member 0",
		},
		fails: "`any`, `unknown` or `object` read as a named type outside the project publishes an external call or external site for each",
	},
	{
		name: "workspace-package-exports-into-build-output",
		anchor: "Node.js Packages (`exports`, conditional exports in object order); npm Workspaces (a workspace is linked by its `name`); " +
			"TypeScript Modules Reference (local packages: an outDir target maps back to rootDir)",
		files: map[string]string{
			"package.json": `{"name": "root", "private": true, "workspaces": ["packages/*"]}
`,
			"packages/util/package.json": `{
  // JSONC is not valid in package.json, but the reader tolerates it.
  "name": "@acme/util",
  "exports": {".": {"types": "./dist/index.d.ts", "import": "./dist/index.js"}},
}
`,
			"packages/util/tsconfig.json": `{
  // The build emits src into dist.
  "compilerOptions": {"outDir": "dist", "rootDir": "src",},
}
`,
			"packages/util/src/index.ts": `export function helper() {} // @helper
`,
			"packages/app/package.json": `{"name": "app"}
`,
			"packages/app/src/main.ts": `import { helper } from "@acme/util";

export function run() {
  helper(); // @r1
}
`,
		},
		// Derived from the anchors: the import names the workspace package @acme/util, whose "." export's first
		// matching condition is "types" (always matched) -> ./dist/index.d.ts, not a member; the package's own
		// tsconfig emits src into dist, so dist/index.d.ts maps back to src/index.ts.
		want: []string{
			"r1 helper -> helper helper exact",
		},
		facts: []string{
			"packages/app/src/main.ts imports packages/util/src/index.ts",
		},
		// Mutation: drop the outDir -> rootDir remap (or never read the workspace map) -> the import and r1 resolve
		// to nothing.
		fails: "a bare workspace specifier treated as a package outside the repository, or an exports target into dist left unmapped, leaves r1 unbound",
	},
	{
		name:   "tsconfig-paths-longest-prefix",
		anchor: "TypeScript tsconfig reference (`paths`: the longest matching prefix wins; targets resolve against `baseUrl`)",
		files: map[string]string{
			"tsconfig.json": `{"compilerOptions": {"baseUrl": ".", "paths": {"@/*": ["src/*"], "@/lib/*": ["lib/*"]}}}
`,
			"src/a.ts": `export function a() {} // @a
`,
			"lib/b.ts": `export function b() {} // @b
`,
			"src/main.ts": `import { a } from "@/a";
import { b } from "@/lib/b";

export function run() {
  a(); // @ra
  b(); // @rb
}
`,
		},
		// Derived from the anchor: "@/lib/b" matches both keys; "@/lib/*" has the longer prefix, so it names
		// lib/b.ts, never src/lib/b.
		want: []string{
			"ra a -> a a exact",
			"rb b -> b b exact",
		},
		facts: []string{
			"src/main.ts imports lib/b.ts",
			"src/main.ts imports src/a.ts",
		},
		// Mutation: take the first matching `paths` key instead of the longest prefix -> "@/lib/b" resolves to
		// src/lib/b, which no member is, and rb is unbound.
		fails: "a paths alias resolved by the first matching key rather than the longest prefix leaves rb unbound",
	},
	{
		name:   "duplicate-workspace-names",
		anchor: "npm Workspaces (a workspace is linked by its declared `name`); two packages of one name are the repository's own ambiguity",
		files: map[string]string{
			"package.json": `{"name": "root", "workspaces": ["packages/*"]}
`,
			"packages/x1/package.json": `{"name": "dup", "main": "index.js"}
`,
			"packages/x1/index.js": `export function f() {} // @f1
`,
			"packages/x2/package.json": `{"name": "dup", "main": "index.js"}
`,
			"packages/x2/index.js": `export function f() {} // @f2
`,
			"main.js": `import { f } from "dup";

export function run() {
  f(); // @r
}
`,
		},
		// Derived from the anchor: both packages declare "dup", so the import names both, each through its main.
		want: []string{
			"r f -> f1 f exact set=2",
			"r f -> f2 f exact set=2",
		},
		facts: []string{
			"main.js imports packages/x1/index.js set=2",
			"main.js imports packages/x2/index.js set=2",
		},
		// Mutation: keep one directory per package name (the last one read) -> one target and no set size.
		fails: "two workspace packages of one name resolved by preference publish one target instead of the set",
	},
	{
		name: "nodenext-esm-directory-import",
		anchor: "TypeScript Modules Reference (node16/nodenext: directory modules and extensionless paths are supported only in require calls; " +
			"a .ts file is an ES module when its package.json says \"type\": \"module\")",
		files: map[string]string{
			"package.json": `{"name": "m", "type": "module"}
`,
			"tsconfig.json": `{"compilerOptions": {"module": "nodenext"}}
`,
			"src/lib/index.ts": `export function g() {} // @g
`,
			"src/main.ts": `import { g } from "./lib";
import { g as h } from "./lib/index.js";

export function run() {
  g(); // @r1
  h(); // @r2
}
`,
		},
		// Derived from the anchor: under nodenext an ES module's `import "./lib"` names no file; "./lib/index.js"
		// names src/lib/index.ts by extension substitution.
		want: []string{
			"r2 h -> g g exact",
		},
		facts: []string{
			"src/main.ts imports src/lib/index.ts",
		},
		// Mutation: add a directory's index form for every import whatever the mode -> r1 binds to g.
		fails: "the directory index form admitted under nodenext ESM binds r1 to src/lib/index.ts's g",
	},
	{
		name:   "require-condition",
		anchor: "Node.js Packages (conditional exports: \"require\" matches a require call, \"import\" an import)",
		files: map[string]string{
			"package.json": `{"name": "root", "workspaces": ["packages/*"]}
`,
			"packages/dual/package.json": `{"name": "dual", "exports": {"import": "./esm.js", "require": "./cjs.js"}}
`,
			"packages/dual/esm.js": `export function f() {} // @fe
`,
			"packages/dual/cjs.js": `export function f() {} // @fc
`,
			"main.js": `const { f } = require("dual");

export function run() {
  f(); // @r
}
`,
		},
		// Derived from the anchor: a require call matches the "require" condition, so it names cjs.js alone.
		want: []string{
			"r f -> fc f exact",
		},
		facts: []string{
			"main.js imports packages/dual/cjs.js",
		},
		// Mutation: ignore the import's form and match "import" for every import -> the require names esm.js.
		fails: "a require resolved under the import condition binds r to esm.js's f",
	},
	{
		name:   "exports-into-unbuilt-output",
		anchor: "Node.js Packages (`exports`); an in-repository package whose exports name no member is an open import, not an external one",
		files: map[string]string{
			"package.json": `{"name": "root", "workspaces": ["packages/*"]}
`,
			"packages/u/package.json": `{"name": "u", "exports": "./dist/u.js"}
`,
			"packages/u/src/u.js": `export function x() {} // @x
`,
			"main.js": `import { x } from "u";

export function run() {
  x(); // @r
}
`,
		},
		// Derived from the anchor: the package is the repository's, its only export target is unbuilt output and
		// no config maps it back, so the import resolves to no member and the call is open, not external.
		want: []string{},
		counts: map[string]string{
			"sites": "1", "bound_sites": "0", "external_sites": "0", "open_sites": "1",
		},
		// Mutation: treat an in-repository import that reaches no member as a package outside the repository ->
		// external_sites is 1.
		fails: "an in-repository package whose exports name no member counted as an external call",
	},
	{
		name:   "pnpm-workspace-without-root-package",
		anchor: "pnpm pnpm-workspace.yaml (`packages` globs declare the workspace; packages link by name)",
		files: map[string]string{
			"pnpm-workspace.yaml": `packages:
  # every library
  - 'libs/*'
`,
			"libs/k/package.json": `{"name": "k", "main": "src/index.js"}
`,
			"libs/k/src/index.js": `export function k() {} // @k
`,
			"apps/w/package.json": `{"name": "w"}
`,
			"apps/w/main.js": `import { k } from "k";

export function run() {
  k(); // @r
}
`,
		},
		// Derived from the anchor: the workspace file is the project's root, so libs/k and apps/w are one unit and
		// "k" names libs/k through its main.
		want: []string{
			"r k -> k k exact",
		},
		facts: []string{
			"apps/w/main.js imports libs/k/src/index.js",
		},
		// Mutation: drop pnpm-workspace.yaml from the project markers -> apps/w is a unit of its own and r is
		// unbound.
		fails: "a pnpm workspace with no root package.json planned as one unit per package leaves r unbound",
	},
	{
		name:   "typescript-import-equals-require",
		anchor: "TypeScript Handbook, Modules (`import x = require(\"m\")` imports the module m)",
		files: map[string]string{
			"src/a.ts": `export function f() {} // @f
`,
			"src/main.ts": `import a = require("./a");

export function run() {
  return a;
}
`,
		},
		// Derived from the anchor: the clause imports src/a.ts.
		want: []string{},
		facts: []string{
			"src/main.ts imports src/a.ts",
		},
		// Mutation: drop the import_require_clause pattern from typescript.scm -> the import is no import record
		// and publishes nothing.
		fails: "`import x = require()` read as no import loses the import of src/a.ts",
	},
	{
		name: "wrapped-and-private-callees",
		anchor: "ECMA-262 (15th edition, 2024) §13.2.9 (the grouping operator keeps the reference), §15.7 (PrivateName); " +
			"TypeScript Handbook (non-null assertion `!`, `as` and `satisfies` change no value)",
		files: map[string]string{
			"src/a.ts": `export function f() {} // @f

export class C {
  #p() {} // @p
  m() {} // @m
  run() {
    this.#p(); // @r1
    (this.m)(); // @r2
  }
}

export function go(c: C) {
  f!(); // @rnn
  (f as any)(); // @r4
  (c.m satisfies () => void)(); // @r5
  (f)(); // @r6
}
`,
		},
		// Derived from the anchors: each wrapper leaves the callee's reference, so every call names its
		// declaration; a private name is its class body's alone, so `this.#p()` runs no override.
		want: []string{
			"r1 #p -> p #p exact",
			"r2 m -> m m virtual",
			"rnn f -> f f exact",
			"r4 f -> f f exact",
			"r5 m -> m m virtual",
			"r6 f -> f f exact",
		},
		// Mutation: drop the wrapped and private callee patterns -> every call is a computed site and binds
		// nothing; dispatch a private method like any other -> r1 is virtual.
		fails: "a wrapped or private callee read as computed leaves the six calls unbound; a private method dispatched as overridable makes r1 virtual",
	},
	{
		name:   "escaped-specifiers",
		anchor: "ECMA-262 (15th edition, 2024) §12.9.4 (String Literals: a string's value decodes its escape sequences); §16.2.2 (Imports)",
		files: map[string]string{
			"src/a.js": `export function f() {} // @fa
`,
			"src/b.js": `export function g() {} // @gb
`,
			"src/main.js": `import { f } from "./\u0061.js";
const { g } = require('./\x62');

export function run() {
  f(); // @ra
  g(); // @rb
}
`,
		},
		// Derived from the anchor: "./\u0061.js" is "./a.js" and './\x62' is './b'.
		want: []string{
			"ra f -> fa f exact",
			"rb g -> gb g exact",
		},
		facts: []string{
			"src/main.js imports src/a.js",
			"src/main.js imports src/b.js",
		},
		// Mutation: read a specifier as its raw text -> "./\u0061.js" names no file and both calls are unbound.
		fails: "a specifier's escapes left undecoded name files that do not exist and leave ra and rb unbound",
	},
	{
		name: "commonjs-export-forms",
		anchor: "Node.js Modules: CommonJS modules (`module.exports`, the `exports` shortcut and its reassignment); " +
			"TypeScript binder (ModuleExports, ExportsProperty, ObjectDefinePropertyExports; an ES module ignores them)",
		files: map[string]string{
			"src/m.js": `module.exports = function () {}; // @mf
`,
			"src/lib.js": `function helper() {} // @h
function other() {} // @o
module.exports = { helper, run: other };
`,
			"src/a.js": `function a() {} // @a
exports.x = a;
`,
			"src/b.js": `function b() {} // @b
exports.y = b;
exports = {};
`,
			"src/c.js": `function impl() {} // @i
function def() {} // @d
Object.defineProperty(exports, "__esModule", { value: true });
Object.defineProperty(exports, "impl", { enumerable: true, get: function () { return impl; } });
exports.default = def;
`,
			"src/proxy.js": `module.exports = require("./m");
`,
			"src/esm.js": `export function q() {}
function z() {} // @z
module.exports = z;
`,
			"src/main.js": `const f = require("./m");
const { helper, run } = require("./lib");
const { x } = require("./a");
const { y } = require("./b");
const p = require("./proxy");
const e = require("./esm");

export function go() {
  f(); // @rf
  helper(); // @rh
  run(); // @rr
  x(); // @rx
  y(); // @ry
  p(); // @rp
  e(); // @re
}
`,
			"src/esmmain.js": `import d, { impl } from "./c.js";
import mf from "./m.js";

export function go2() {
  d(); // @rd
  impl(); // @ri
  mf(); // @rm
}
`,
		},
		// Derived from the anchors: a call of a required module calls its whole value; an object literal's
		// keys alias the names they hold; `exports = {}` unbinds the shortcut, so y is no export of b.js; a
		// getter returning impl exports impl; with __esModule, the default import is exports.default; without
		// it, the whole value; a module re-exporting another's whole value is that value; an ES module's
		// `module.exports` is no export.
		want: []string{
			"rf f -> mf exports exact",
			"rh helper -> h helper exact",
			"rr run -> o other exact",
			"rx x -> a a exact",
			"rp p -> mf exports exact",
			"rd d -> d def exact",
			"ri impl -> i impl exact",
			"rm mf -> mf exports exact",
		},
		// Mutation: read only function values (or drop the whole-value export) -> rf, rh, rr, rx, rp and rm are
		// unbound; ignore `exports = {}` -> ry binds b; read CommonJS forms in an ES module -> re binds z.
		fails: "CommonJS whole-value, alias, getter, default and re-export forms unread leave the calls unbound; " +
			"a rebound `exports` or an ES module's `module.exports` read as exports binds ry or re",
	},
	{
		name: "required-class-constructed",
		anchor: "Node.js Modules: CommonJS modules (`require` returns `module.exports`); " +
			"ECMA-262 (15th edition, 2024) §13.3.5 (The new Operator)",
		files: map[string]string{
			"src/c.js": `class C {
  constructor() {} // @cc
  hi() {} // @ch
}
module.exports = C;
`,
			"src/d.js": `class D {
  constructor() {} // @dc
  hi() {} // @dh
}
module.exports = { D };
`,
			"src/main.js": `const C = require("./c");
const { D } = require("./d");

function go() {
  new C().hi(); // @nc
  new D().hi(); // @nd
}
`,
		},
		// Derived from the anchors: C is c.js's whole value and D the value of d.js's key D, so each `new`
		// runs that class's constructor and the object it makes has the class's methods.
		want: []string{
			"nc C -> cc constructor exact",
			"nc hi -> ch hi exact",
			"nd D -> dc constructor exact",
			"nd hi -> dh hi exact",
		},
		// Mutation: read a `require` namespace binding as no type (the whole value unread) -> nc is unbound;
		// skip a re-export under a declaration's own name that the provider does not mark exported -> nd is
		// unbound. With no instance, neither hi binds.
		fails: "a class taken from a required module and constructed binds neither its constructor nor its methods",
	},
	{
		name: "commonjs-exported-class-and-methods",
		anchor: "Node.js Modules: CommonJS modules (`module.exports` holds any value); " +
			"ECMA-262 (15th edition, 2024) §15.7 (Class Definitions: a class expression), §13.2.5 (Object Initializer: a method definition)",
		files: map[string]string{
			"src/k.js": `module.exports = class {
  constructor() {} // @kc
  m() {} // @km
};
`,
			"src/o.js": `module.exports = {
  m() {}, // @om
  n: function () {}, // @on
};
`,
			"src/main.js": `const K = require("./k");
const { m, n } = require("./o");

function go() {
  new K().m(); // @nk
  m(); // @mo
  n(); // @no
}
`,
		},
		// Derived from the anchors: k.js's whole value is the class, so `new K()` runs its constructor and
		// the object has its method m; o.js's value is the object, whose method m and function-valued key n
		// are what the destructured names hold.
		want: []string{
			"nk K -> kc constructor exact",
			"nk m -> km m exact",
			"mo m -> om m exact",
			"no n -> on n exact",
		},
		// Mutation: leave a class expression unnamed (drop its declaration by the property it is stored
		// under) -> both nk bindings are lost; leave an object's method definitions unexported -> mo is unbound.
		fails: "a class expression assigned to `module.exports`, or a method of the object assigned to it, is no export",
	},
	{
		name:   "typescript-import-equals-require-constructed",
		anchor: "TypeScript Handbook, Modules (`export =` assigns the module's value; `import x = require()` imports it)",
		files: map[string]string{
			"src/c.ts": `class C {
  constructor() {} // @cc
  hi() {} // @ch
}
export = C;
`,
			"src/main.ts": `import X = require("./c");

export function go() {
  new X().hi(); // @nx
}
`,
		},
		// Derived from the anchor: X is c.ts's whole value, the class C, so `new X()` runs C's constructor and
		// the object it makes has C's method hi.
		want: []string{
			"nx X -> cc constructor exact",
			"nx hi -> ch hi exact",
		},
		// Mutation: record no local name for an `import x = require()` clause -> X denotes no type of the unit:
		// nx binds no constructor and hi binds an external X.hi.
		fails: "a class `import x = require()` takes binds neither its constructor nor its method",
	},
	{
		name: "constructor-function-prototype-methods",
		anchor: "ECMA-262 (15th edition, 2024) §13.3.5.1.1 (EvaluateNew), §10.1.13 (OrdinaryCreateFromConstructor: the " +
			"object's prototype is the constructor's `prototype`); TypeScript, JavaScript checking (constructor functions are " +
			"classes; AssignmentDeclarationKind PrototypeProperty `C.prototype.m = …` and Prototype `C.prototype = { … }`)",
		files: map[string]string{
			"src/animal.js": `function Animal(name) { this.name = name; } // @an
Animal.prototype.speak = function () {}; // @sp
module.exports = Animal;
`,
			"src/dog.js": `function Dog() {} // @dg
Dog.prototype = {
  bark() {}, // @bk
  sit: function () {}, // @st
};
module.exports = Dog;
`,
			"src/main.js": `const Animal = require("./animal");
const Dog = require("./dog");

function go() {
  new Animal("a").speak(); // @na
  new Dog().bark(); // @nb
  new Dog().sit(); // @ns
}
`,
		},
		// Derived from the anchors: each module's whole value is a constructor function, which `new` runs, and
		// the object it makes inherits what the function's prototype holds.
		want: []string{
			"na Animal -> an Animal exact",
			"na speak -> sp speak exact",
			"nb Dog -> dg Dog exact",
			"nb bark -> bk bark exact",
			"ns Dog -> dg Dog exact",
			"ns sit -> st sit exact",
		},
		// Mutation: leave a function whose prototype holds methods a function (no class) -> every row is lost;
		// give a prototype's function no receiver -> speak, bark and sit are unbound.
		fails: "a constructor function and the methods its prototype holds are no class and no methods",
	},
	{
		name:   "typescript-export-assignment",
		anchor: "TypeScript Handbook, Modules (`export =` assigns the module's value; `import x = require()` imports it)",
		files: map[string]string{
			"src/m.ts": `function f() {} // @f
export = f;
`,
			"src/main.ts": `import g = require("./m");

export function run() {
  g(); // @r
}
`,
		},
		// Derived from the anchor: `export = f` is m's whole value, which g names.
		want: []string{
			"r g -> f f exact",
		},
		// Mutation: no `export =` branch in the export reader -> r is unbound.
		fails: "`export = f` not read as the module's whole value leaves r unbound",
	},
}
