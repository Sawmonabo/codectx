# The `link` provider

`internal/provider/link` is the package-scoped linking provider of ADR-0012 decision 9. It publishes `calls`,
`imports`, `data_flows_to`, `extends`, `implements`, `overrides` and `generates` for the nine languages the structural provider
lowers: C, C++, Go, Java, JavaScript, TypeScript, TSX, Python and Rust. It never parses a file. It reads the link
tables the structural provider stored with each file's unit, binds every call site to the declarations it may call,
resolves imports to files, follows arguments into the parameters they bind, and reads the class hierarchy, and
publishes each fact as an edge between the structural provider's own nodes.

The structural provider still publishes its syntax-tier `calls` (docs/providers-treesitter.md, Unit scope and node
identity): a call it resolves inside the file, and the callee reference node of every call it cannot. The linker
publishes every binding it can justify, inside the file as well as across files: a site the structural provider bound
in its file is bound again when the linker reaches the same declaration, and the two edges have one relation id (the
caller, the kind and the target decide it), so the pair is one relation with both providers' evidence. A site the
linker cannot bind publishes an open-site node (Open sites, below).

## Descriptor

```
ID                link
Version           1
Capabilities      calls, imports, data_flows_to, extends, implements, overrides, generates
DependsOn         treesitter
InvalidationScope package
Required          false
```

`ProviderID`, `Version` and `Capabilities` are in `internal/provider/link/provider.go`, and the descriptor is
`Provider.Descriptor`. The id is not `dependence`, which is never reused (decision 9). The provider is not required:
a failed link degrades its seven capabilities and nothing else. It runs when the structural provider is
enabled and reports disabled with it; it has no configuration key of its own.

## What it publishes

| Capability | Relation | From → to | Evidence detail |
|---|---|---|---|
| `calls` | `calls` | caller → each bound declaration, or an external method node | `direct`, `hierarchy`, `address_taken`, `flow`, `field` |
| `calls` | `calls` | caller → the site's open-site node | none |
| `imports` | `imports` | file module → each file module the import resolves to | `import` |
| `imports` | `imports` | file module → the import's open-import-site node | none |
| `data_flows_to` | `data_flows_to` | each variable an argument reads → the parameter it binds | `argument` |
| `data_flows_to` | `data_flows_to` | each variable an argument reads → each variable the site's result is written into, when the parameter it binds reaches the callee's return | `summary:<position>` |
| `data_flows_to` | `data_flows_to` | the callee's return node → each variable the site's result is written into, and the return node of the callable that returns the call | `return` |
| `extends` | `extends` | type → a supertype of its own kind | `supertype`, `supertype_class`, `supertype_interface` |
| `implements` | `implements` | type → an interface or trait it declares, or (Go) satisfies | `supertype`, `supertype_interface`, `method_set` |
| `overrides` | `overrides` | method → the method it directly overrides or implements | `supertype`, `method_set`, `supertype_candidate` |
| `generates` | `generates` | Rust `macro_rules!` → each declaration written in an item-position invocation of it in another file of the unit | `macro_transcribes`, `macro_input` |
| none of its own | `defines` | file module → a route endpoint at its full path, a declared entry point, or an inherited Java `main` | none |
| none of its own | `routes_to` | route endpoint → each handler; declared entry point → its declaration or module | `import` |
| none of its own | `routes_to` | inherited Java `main` endpoint → the method JLS §12.1.4 launches its class by | `hierarchy` |
| none | `references` | C or C++ declaration → each definition of a macro among its specifiers that its file sees and that states a visibility | `visibility_<value>` |

The details are the `Detail*` constants (`provider.go`). Each one names the technique that found the fact:
- `direct`: the callee names the declaration by its package, module, import or scope;
- `hierarchy`: the method the receiver's static type declares or inherits, found by walking its supertypes;
- `address_taken`: an indirect site joined to an address-taken function of the same signature;
- `flow`: the receiver's type was recovered by flow-based type inference;
- `field`: a function written to the field the site calls through;
- `import`: an import resolved to a file of the unit;
- `argument`: an argument of a bound call site, matched to its parameter;
- `summary:<position>`: an argument of a bound call site flowing into the site's result through the callee's
  parameter at that position (`Parameter.Index`, 0 the receiver), whose value reaches the callee's return;
- `return`: the bound callee's return node flowing into the site's result;
- `supertype`: a supertype or an override the source writes;
- `method_set`: a Go interface a type satisfies by its method set, which the source never writes;
- `supertype_interface`, `supertype_class`: a supertype the source writes that is outside the project (a node of
  kind `type`), with the kind the clause that writes it proves: an interface or trait (an `implements` clause, an
  interface's `extends`, a Rust trait, a Go interface's embedded interface) or a class (a class's `extends` clause or
  base list).
- `supertype_candidate`: an `overrides` into a method of a type outside the project that a Java `@Override` or C++
  `override` method may override: one of the external supertypes on a path along which no type of the project
  declares the method, read with its set size as "at least one of n" (several may hold at once), or one published
  beside an override in the project, which the mark already proves.
- `macro_transcribes`, `macro_input`: a `generates` from a Rust `macro_rules!` to a declaration written in an
  invocation's input, `macro_transcribes` when every rule of the definition transcribes its item input once, unchanged (the
  declaration is exactly an item of the expansion), `macro_input` when it may transcribe it renamed, repeated or not
  at all (docs/providers-treesitter.md, Rust).
- `visibility_default`, `visibility_hidden`, `visibility_protected`, `visibility_internal`, `visibility_dllexport`,
  `visibility_dllimport`: the visibility a C or C++ macro definition's replacement states, as declared in source and
  never the binary's export table (compiler flags such as `-fvisibility=hidden` decide that);
  `visibility_unspecified`: a definition that states none (an empty replacement, keywords only);
  `visibility_unresolved`: one that names another macro among other tokens, which is not expanded. A declaration's
  macros are read through the definitions its file sees: its own and those of a header it includes directly (the
  include lookup), following a one-identifier replacement to the macro it names. Each definition reached is one
  occurrence (`resolve_c.go` `visibilityFacts`), so a macro defined in several `#if` arms publishes each arm's value,
  with the set size their number; no definition seen publishes nothing. These details are declared in `resolve_c.go`.

**Caller and target.** The caller is the anonymous route handler a site is written in when the structural provider
publishes that handler as its own function node (`Site.Handler`, `Table.Handlers`), else the declaration enclosing
the site, else the file's module node (`Site.Caller`, -1 for the module, `internal/provider/link/table.go`). So a
call inside `app.get("/x", (req, res) => helper())` is a call of the handler, as the structural provider's own
`calls` are. A target is a declaration of any member file of the unit, a file's module, a parameter, or an external
node.

**Evidence.** Every fact is `static_analysis`. A call publishes one evidence row per edge, at the call expression's
range (`Site.Range`). Its native key is the call-site key, spelled as the structural provider spells it:
`callsite:<path>:<first byte>-<last byte>` of the callee token, one-based and inclusive (`Site.NameStart`,
`Site.NameEnd`). An `imports` edge is located at the import statement, a `data_flows_to` edge at the argument with
the site's call-site key, and a hierarchy edge at the declaration that states it.

**Endpoints are the structural nodes.** The linker mints no second identity for a declaration. Each table carries
an `Endpoint` for each declaration, each parameter, each callable's return node and the module (`table.go`): the node id, canonical key, kind,
names and range the structural unit resolved. The linker publishes an occurrence of every endpoint an edge names,
with its own evidence at the declaration's range. So the edge joins the structural node, and through it the
cross-provider `decl:` key and the call-site key of docs/providers-treesitter.md.

**Several targets.** A site bound to two or more targets publishes one `calls` edge to each, and every one of them
carries the same `set_size`, the number of targets, read as "1 of n" (ADR-0015 decision 1). That is a value-flow set
with no declared receiver type (`speak(Dog())` and `speak(Cat())` reach `a.sound()` in `speak(a)` with two types),
the address-taken join (a function pointer and every address-taken function of its signature), several functions
written to one field, and a macro defined in several `#if` arms. An import matching several files (a quoted
`#include` two headers end in) publishes one `imports` per file with `set_size` the same way. A site bound to one
target carries no `set_size`. Nothing is capped: every target is published.

**Dispatch.** Every `calls` occurrence to a declaration or an external method carries `dispatch`: `virtual` when the
call can run an override of its target, `exact` when it cannot. It is computed per occurrence, from the site, never
from the technique that bound it: a `super` call, a qualified call (`a.Animal::sound()`, `Base.m(self)`), a call on a
constructed object (`Dog().sound()`, `new C().m()`), a static, final, private or non-virtual method, a C++ call
through a declaration that is not `virtual`, on an object declared by value (`Dog d; d.sound()`) or on the result of
a function that returns a class by value (`Dog make(); make().sound()`; a pointer or a reference, declared or
returned, stays `virtual`, and an `auto` return names no type), and every function an address-taken or field set reaches are `exact`. A
member call on a receiver with a declared static type publishes one `calls` to the method that type declares or
inherits, even when it is abstract or an interface or trait method, and never one edge per override:
`speak(a: Animal) { a.sound() }` with `Dog` and `Cat` overriding `sound` publishes `speak calls Animal.sound`,
`virtual`, and nothing to `Dog.sound` or `Cat.sound`. The overrides a virtual call may run are the `overrides`
edges into its target, which queries read.

**Open sites.** Every site no fact binds publishes one open-site node and the caller's `calls` to it (ADR-0015
decision 3). The node is in the file's scope `file:<path>`, keyed `open:` and the site's call-site key, or, for a
computed callee, which has no callee token, `open:computed:<path>:<first byte>-<last byte>`; it is located at the
site, named by the callee as written (`call` when there is none), of kind `method` for a member or static site and
`function` otherwise, with metadata `{"resolution":"unresolved","reason":…,"shape":…,"arguments":n}`
(`model.OpenSiteMetadata`). Its reason is the site's form:
- `computed`: the callee is an expression that names nothing (`obj[k]()`);
- `receiver_untyped`: a member or static call whose receiver has no recovered type;
- `value_untracked`: a call through a value whose flow the analysis does not follow;
- `global_unbound`: a bare name or a construction no visible scope declares.

Its shape is `member`, `value`, `construct` or `bare`, and `arguments` is the number of arguments the site passes.
The candidates of an open site are computed per query as `may_call`, never stored. A key longer than
`model.MaxNativeKeyBytes` is replaced by its digest, so every open site has a node.

**External nodes.** A supertype outside the project, and a method of one, get a node so a hierarchy edge into it has
an endpoint and a call on a receiver of that type meets the override of that method at one node (ADR-0015 decision
2, `internal/provider/link/external.go`). An external node has no file: it is minted in the scope `link:external`,
keyed by the language family, the type's qualified name as the family resolves it (`hierarchy.externalName`) and, for
a method, the method's name, with metadata `{"resolution":"external"}`. An external type's node is of kind `type`,
never `class` or `interface`: whether it is a class, an interface, a trait or a struct is not known from the
project's code, and one project can write it both ways (TypeScript lets a class implement a class), so a fixed kind
keeps one qualified name one node in every unit. Each `extends` or `implements` into it carries the kind its own
clause proves as its detail (`supertype_interface`, `supertype_class`). A Java anonymous class body is its own class
node (`new T`, below); its one supertype edge into a type outside the project is an `extends` with the detail
`supertype_anonymous`, since the creation proves neither a class nor an interface (JLS §15.9.5: the body extends a
class, or extends `Object` and implements an interface), and into a type of the unit it is `extends` or `implements`
by that type's kind. Its methods' overrides reach the external method node like any class's. A call site does not know the parameter types
an external method declares, so one node stands for all of a type's methods of one name. A Java `@Override` of
`toString` and `o.toString()` on an `Object` receiver both target `java.lang.Object.toString`. Every run of every
unit that names an external node republishes it with its own evidence, and the delta build replaces the index-level
bucket that holds them (`Replaced{IndexLevel: true}`, `internal/index/delta/link.go`). A call proven to leave the
project with no node to target (a call through a package outside the module, a method several external supertypes
may supply) is counted external and publishes no edge.

**Imports.** An import the linker resolves publishes `imports` from the file's module to the module of each file it
names: a JavaScript, TypeScript or TSX specifier the module resolution reaches, a Python `import a.b` or
`from a import b` (to the file that declares `b`, else the submodule `a.b`, else the module `a`), and a quoted C or C++ `#include` (the
file beside the including one, and every member file whose path ends in the header's at a component boundary). An
angled include, a package outside the unit, and an import naming no member file publish nothing. An ECMAScript
import of a module of the repository that resolves to no member file (a workspace package whose `exports` name an
unbuilt output no config maps back to its source) publishes nothing either, and is counted in the `imports` row's
`open_import_sites` (`FactOpenImport`), never as external.

An import whose path does not fold to a constant publishes one open-import-site node and the module's `imports` to it
(ADR-0015 decision 6): `require(expr)` and `import(expr)` in ECMAScript, `importlib.import_module(expr)` and
`__import__(expr)` in Python, and `#include MACRO` with the macro undefined in the file. The node is in the file's
scope, keyed `openimport:<path>:<first byte>-<last byte>`, named `import`, of kind `module`, with metadata
`{"resolution":"unresolved","reason":"computed","fragments":[…]}`, every non-empty literal part of the path in source
order (`require("./x/" + name)` gives `["./x/"]`); none is dropped or cut, and a list too long for the node's metadata
bound is stored as metadata rows on the site, with `fragments_rows` in place of `fragments`, which the graph reads
when it computes `may_import` (node metadata rows: docs/providers-filesystem.md, Node metadata). The files whose paths match the fragments are computed per query as
`may_import`. A `require("./x")` or `import("./x")` of a string literal is an import record of the structural query,
and a path that folds to a constant (`import("./" + "x")`) is an ordinary import too.

**Argument flows.** A bound call site publishes `data_flows_to` from each variable an argument reads to the parameter
of the target the argument binds (ADR-0017), and an unbound site publishes none. Arguments are matched as the
target's language binds them (`argumentFlows`, `internal/provider/link/args.go`): by position, then a keyword
argument by its parameter's name, a spread or `*args` to the rest from its position, and the arguments past the last
named parameter to a variadic one; a parameter an omitted default fills receives nothing. A call through a function value flows into
every function the address-taken join binds it to. A receiver passed first (a
Go method expression, a Rust path naming a method, a Java unbound method reference, Python `C.m(obj)`) binds the
receiver parameter, and nothing where the callee declares none (a Java method's receiver). A parameter with no
node of its own receives no edge.

**Return flows.** A call's value goes back to the caller by labelled edges, a summary per argument and a return
edge from the callee's return node, never by an unlabelled edge from inside the callee to the call (ADR-0017
decision 7), for every family that publishes argument flows, the address-taken join
included (`internal/provider/link/returns.go`). A site's result is where the structural provider found its value
goes: `Site.Results`, the variables the innermost node evaluating the call defines, and `Site.Into`, the callable
whose return that node is (`return f()`); a call whose value another call consumes first (`g(f())`, `f().m()`)
has neither, and a site with neither publishes no return flow. A bound site publishes:
- for each argument bound to a parameter marked `Returned` (its value reaches the callee's return), one
  `data_flows_to` from each variable the argument reads to each variable of `Site.Results`, at the argument, detail
  `summary:<position>` (`FactSummary`);
- from the callee's return node (`Decl.Return`) one `data_flows_to` to each variable of `Site.Results` and to the
  return node of the callable `Site.Into` names, at the call expression, detail `return` (`FactReturn`). A callee
  with no return node (a type, a constructor that returns no value) publishes none.

Every value a returning node reads flows into the callable's return node (docs/providers-treesitter.md, Return
flow), parameter-reached values included, so a value created inside the callee (`k = 1; if p: return k`) reaches
every bound site's result. Realizability is the walk's: each `data_flows_to` carries its role in its evidence
detail, `argument`, `return`, `summary:<position>`, or the structural `reaching_def`, and the impact walk follows
the two-phase rule of interprocedural slicing (ADR-0017 [S10]). A path that has taken an `argument` edge never
takes a `return` edge; a summary edge stays in the caller and carries what the parameter does to the result. So a
path that enters a callee through one call's argument never leaves through another call's result, and a path that
starts inside the callee leaves through every call's `return` edge, a partially balanced path. Only the linker
publishes `argument` and `return` evidence; a stored relation may join the structural `reaching_def` of a folded
call and the linker's `summary`, both free. A summary is the dependence facts' reach: a call folded into a node passes
its arguments' values to what the node defines, so a parameter passed through another call reaches the return
whatever that callee returns. The summary is part of the callee's interface (`Parameter.Returned`, the
`paramReturned` mark of its hierarchy entry): a body edit that changes whether a parameter reaches the return
changes the entry every bound site's argument flows read, so a refresh re-runs exactly those sites. The return node
is keyed by the callable's identity and exists for every analysed callable, so its flows do not depend on the body.

**Hierarchy.** Each type publishes `extends` to each declared supertype that is a class, or that is an interface when
the type is one itself (a Java or TypeScript interface's extended interfaces, a Rust supertrait, a Go interface's
embedded interfaces), and `implements` to each other interface or trait it declares, or, in Go, satisfies by its
method set. A Go struct's embedded types are promotion and no supertype. Each method publishes `overrides` to the
method it directly overrides along each direct supertype, never a deeper one: in Java one of the same signature that
is neither static, final nor private; in C++ a virtual one of the same signature; in Python the first along the
method resolution order; in Rust the trait's, from the trait's impl; in ECMAScript one of the same name that is not private (a `#p` method's name is its class body's alone, so it
overrides nothing and a call of it is `exact`); in Go each
method of an interface its type satisfies. A supertype outside the project is its external node. A method overrides
an external one only where the language marks it (Java `@Override`, C++ `override` or `final`, TypeScript
`override`, a Rust trait impl item, Python `typing.override`), toward what the mark proves
(`hierarchy.externalOverrides`, `internal/provider/link/hierarchy_external.go`):
- Java and C++: the mark proves at least one overridden method, and a method overrides every supertype's of its
  signature at once. The candidates are the supertypes outside the project reached along paths on which no type of
  the project declares the method (and, in Java, `java.lang.Object` for one of its methods when no candidate is a
  class). With no override in the project and one candidate, that one is published (`supertype`); otherwise each
  candidate is published with detail `supertype_candidate` and, when there are several, the set size n, read as "at
  least one of n". A candidate is published beside an override in the project too, so nothing the mark may name is
  dropped: `class C extends Base implements Task` with `@Override run()`, both outside the project, publishes
  `C.run` overrides `Base.run` and `Task.run`, each `supertype_candidate` with set size 2.
- TypeScript: `override` proves a member of the class `extends` names, never of an interface `implements` lists, so
  the one candidate is the extended class outside the project when no class of the project along the chain declares
  the member.
- Python: `typing.override` proves the first ancestor along the method resolution order that defines the method.
  When no class of the project along the order defines it, it is one of the external bases of the classes along the
  order, each published (`supertype`) with the set size of their number when there are several.
- Rust: an impl item implements its block's trait's item of its name (`Decl.Trait`), the external trait's method
  when the trait is outside the project (`hierarchy_rust.go`).

An unmarked method with an external supertype publishes no `overrides`.

A Go type outside the unit is read from its package's source (`goExternal`, `internal/provider/link/go_external.go`):
the transitive closure of the unit's imports outside it, each package found as the go command finds it without
running it (`goModuleInfo.source`, `go_module.go`): the standard library of the toolchain the project builds with
(`GOROOT`, else the `go` on `PATH`, else the toolchain the `go.mod` `toolchain` or `go` line selects in the module
cache, `go_host.go`); a module of the governing `go.work` or a local `replace` inside the repository from the
snapshot; a required module, after `replace`, from the module cache at its exact version (`GOMODCACHE`, else the
first `GOPATH` entry's `pkg/mod`). A package none of these holds (a local `replace` outside the repository, a
module the cache does not hold) is unavailable, and the `implements` and `overrides` capability rows count both
(`go_external_packages`, `go_external_packages_unavailable`). Each package's types, aliases and method forms are read
with `go/parser` from every non-test file (every build variant; a type or method two variants declare differently is
left out) and cached on disk by its source's identity (`go_external_cache.go`). A Go type implements an interface
outside the unit by its method set, which publishes `implements` (`method_set`) and an `overrides` from each method to
the interface's of its name; an interface with an unexported method implements only within its own package. A
conversion `var _ io.Reader = (*T)(nil)` (or `&T{}`, `T{}`, `new(T)`) proves `T` implements that interface without its
source, and publishes `implements` (`supertype_interface`) and no `overrides` (`go_conversions.go`).

## Route prefixes across files

A route's full path is often made in several files: a router declares its routes in one, and another includes it
under a prefix (`app.include_router(users.router, prefix="/v1")`, `app.register_blueprint(views.bp)`, Django's
`path("blog/", include("blog.urls"))`). The structural provider publishes such a route with `prefix: unresolved`
(docs/providers-treesitter.md, Framework routes), and carries what the linker needs in the file's link table,
`Table.Routing` (`internal/provider/link/route.go`):

- `Objects`: each router or application object the file binds at module level, its framework and its own prefix
  (`APIRouter(prefix=…)`, `Blueprint(url_prefix=…)`), or that it is not a literal;
- `Routes`: each route registered on one of them, its method, its route as written and its literal value below the
  object, and its handlers, as declarations of the file or as names the file imports;
- `Mounts`: each mount of an object the file does not build, with the argument as written (`users.router`), the module
  and name it names, the mount's prefix and whether it replaces the object's own (a Flask registration's
  `url_prefix=`), the object it is made on, and that object's prefix below an application root when the file knows it.
  A Django settings module's `ROOT_URLCONF` literal is a mount of the module it names at `/`, anchored by the
  settings rather than an object.

Every object is indexed in the module index under each dotted name its file can be imported as. The linker walks
down from each mount anchored at an application root (a mount on a root object of the file, or a settings module's
`ROOT_URLCONF` when some file names that settings module in `os.environ.setdefault("DJANGO_SETTINGS_MODULE", …)` or
`os.environ["DJANGO_SETTINGS_MODULE"] = …`). Each hop looks up the module and name the mount names, keeps the hits
an import's lookup keeps (the nearest source root, as for `imports`), joins the mount's prefix with the object's own,
and descends into the mounts recorded on that object; a binding already on the hop's way ends it, so a cycle of
routers including each other ends. The walk is bounded by the mounts the unit holds, never by a depth. Each route
reached becomes one `endpoint` in the scope of the file the root mount is in, located at that mount, keyed
`endpoint:<method route>@<full path>`, with the endpoint metadata `framework`, `method`, `route`, `prefix: resolved`
and `path`, defined by the file's module and routing to each handler (`routes_to`, detail `import`). Like every endpoint of
the linker (`internal/provider/link/route_search.go`), it carries the search document the structural provider gives an
endpoint: its framework, method, route and path as its body, located at the endpoint, so a kind filter on endpoints
finds it. A handler the route's file imports is resolved through the module index to its declaration.

A route several mounts reach is several endpoints, one per path, and every `routes_to` of each carries `set_size`,
the number of paths the walks of the file's root mounts gave that route. A hop whose prefix is not a literal (an
`include_router(r, prefix=PREFIX)`) leaves every route below it `prefix: unresolved` with no `path`, keyed by the
route's file and object instead of a path; it is still counted in the set. A mount whose argument names no indexed
object publishes nothing. Every lookup is traced on the mount's file, so a refresh that changes a router, a route or a
settings module republishes the endpoints that reach it, and the routing is part of the interface summary, so a
change to it rebuilds the index.

An ASGI mount (Starlette's `Mount(path, app=…, routes=…)` and FastAPI's or Starlette's `app.mount(path, app)`) mounts
any object of either ASGI framework, a FastAPI application being a Starlette application: an application root, a
router, or a route list a module binds (`routes = [Route(…), Mount(…)]`), which the structural provider records as an
object of its own when no list the file passes holds it. Every FastAPI and Starlette application root bound at module
level is indexed as an object too, with its routes below it and the mounts made on it, so a file mounting it walks on
through the mounts the application itself makes. Only an ASGI mount mounts an application; `include_router` and
`register_blueprint` mount only routers and blueprints of their own framework. A keyword argument, which the worker
types by no position, is read as written: a dotted name whose head an import binds names that module's attribute
(`RouteMount.Target`).

An Express router reaches another file through an import (`internal/provider/link/route_express.go`): a router a
module binds at module level and exports, by its own name (`export const admin = Router()`), by a local export
(`export default router`) or by a CommonJS assignment (`module.exports = router`, `module.exports = { router }`), is
indexed under `keyJSRouting` by its module and each name it is exported by. A `use([path,] x)` on an object of the
file whose argument is an import binding records the module as written and the export it reads (`RouteMount.Target`:
the named or `default` export, or the module's whole value for a `require`), at the literal path, or at an unknown
prefix when the first argument is no literal (a path or a middleware, which Express tells apart only at run time).
The walk resolves the module as the file's imports resolve, follows re-exports as ECMA-262's ResolveExport does (an
`export *` decides only when one module supplies the name), and descends into the router's own `use` mounts. A
handler the router's file imports is resolved as that file's import resolves. A router a function receives
(`module.exports = (app) => { app.get(…) }`) in a file that imports no framework is read by no table, and publishes
nothing.

A Go router reaches another file through a call, never a module attribute (`internal/provider/link/route_go.go`). The
structural provider records, as an object of the callable, a router a function receives as a parameter of a router
type of a framework the file imports (`func Register(r chi.Router)`, `rg *gin.RouterGroup`), when every use is that
parameter's value (no local of the name, no assignment to it, not inside a function literal), and a router it builds
and returns (`r := chi.NewRouter(); …; return r`, every return of it); the routes on each, with the groups and
closures below them (`v1.Group("/users")`), carry their path below the object. Three Go mounts join them
(`RouteMount.Join`), each with the call as written and no position (`MountCall`): a router passed as an argument of a
call that is no framework's own (`users.Register(v1)`) is the callee's parameter the argument binds, by the callee's
declared parameters (`argumentFlows`); a chi `Mount(prefix, f())` mounts the router `f` returns; a chi
`Route(prefix, f)` or `Group(f)` with a function value hands `f` the router as its first parameter. Each callable's
objects are indexed under `keyGoRouting` by the callable. The walk starts at each Go mount anchored at an application
root or at a router the program serves, resolves the mount's call as the file's own resolution does, in the file's
context, and descends through the callee's mounts the same way, ending at a callable already on its way; a callee of
another framework, a call whose receiver is a call's result, a parameter written inside a function literal and
`http.StripPrefix` around a returned handler publish nothing. A handler the callee's file names but does not declare
(`handlers.Show`) is resolved in that file's context; a handler written inline gives the endpoint no `routes_to`. Every lookup is traced on the mount's
file.

## NestJS paths

A NestJS route's path is joined from three files' worth of facts (`internal/provider/link/route_nest.go`): the
controller's path and the route decorator's in the controller's file, and the global prefix and versioning the file
creating the application sets on it (`NestFactory.create`, then `app.setGlobalPrefix(…)`, `app.enableVersioning(…)`).
The structural provider publishes every NestJS route `unresolved` (docs/providers-treesitter.md, Framework routes) and
records, in the controller's link table, each route (`Routing.Nest`: method, route, the controller's paths and the
route's as written in literals, the method's `@Version` and the controller's `version`, the handler) on a mount of its
own located at the decorator (`RouteMount.Nest`); and, in any ECMAScript file that creates an application or calls
`setGlobalPrefix` or `enableVersioning`, what it sets (`Routing.NestApp`, indexed under `keyNestApp`). A call is
attributed by its receiver: one on an application the file creates applies to it, one on a testing application (a
`createNestApplication` result, or any application in a file importing `@nestjs/testing` that NestFactory did not
create) is not read, and any other receiver (a helper's parameter) is an application no table ties to one.

The linker builds each path as Nest's `RoutePathFactory.create` does (packages/core/router/route-path-factory.ts):
the URI version segment (`/` + the versioning's `prefix`, `v` unless it says otherwise or `false`, + the version; the
method's `@Version`, else the controller's `version`, else the `defaultVersion`; `VERSION_NEUTRAL` adds none; an
array is one path per version), then the controller's path and the route's, each joined as `stripEndSlash(a) +
addLeadingSlash(b)` with an array one path per element (`@Controller()` and a route with no path are `/`), then the
global prefix with its trailing slash removed, and a final leading slash with no trailing one. Only URI versioning
puts the version in the path; header, media-type and custom versioning leave it out. Each path is one `endpoint` in
the controller's file, located at the decorator, keyed `endpoint:<method route>@<path>`, with `prefix: resolved`,
routing to the method (detail `direct`). The paths are computed for every application the unit creates; distinct
paths are published together, each `routes_to` carrying their number as `set_size`.

Nothing is published, and the structural `unresolved` endpoint stays the route's only one, when the unit creates no
application (the bootstrap is another unit's); when a setting is made on an application no table ties to one; when a
file creating several applications sets either; when one file's settings disagree; when a prefix or a versioning
option is no literal, or `setGlobalPrefix` passes options (its `exclude` list is matched against each route at run
time); when the route's or the controller's path or the version it needs is no literal; and when any file names
`RouterModule` from `@nestjs/core`, whose module paths join below the version through the module tree
`NestFactory.create` starts from, which this design does not follow. Every application is read through one traced
lookup, and the settings are part of the creating file's interface summary, so an edit to `main.ts` re-resolves every
NestJS route of the unit.

## Request inputs across files

A request-input read whose source another file decides is published by the linker
(`internal/provider/link/route_input.go`). The structural provider records it in the file's link table as a pending
input (`Table.Inputs`): a FastAPI handler parameter typed by a class the file imports (`user: schemas.User`,
`item: Item | None` after `from .schemas import Item`), or by a class of the file whose bases it imports, with the
parameter, its range, its owner and whether the route writes its name. Each Python file also records, for every
module-level class whose name is unique in the file, what the class says by itself (`Table.Classes`): a body for a
dataclass, a Pydantic model, a `TypedDict` or a `NamedTuple`; the request for FastAPI's and Starlette's `Request`,
`WebSocket` and `HTTPConnection`; a file for `UploadFile`; a special type for a `*Response`, `BackgroundTasks` or
`SecurityScopes`; nothing for `Enum`, `Generic`, `Protocol`, `ABC`, `object` and the builtin scalars; and the bases it
imports from other modules, which decide the rest. A class with a base the file cannot read is not recorded.

The classes are indexed under each dotted name their file can be imported as (`keyPyClass`). For each pending input
the linker looks the class up as an import's lookup does (the nearest source root), follows its imported bases
through further files, a class already on the walk's way ending it. A module that declares no class of the name may
bind it by an import of its own, as a package's `__init__.py` re-exports `from .user import User`
(`internal/provider/link/route_input_reexport.go`): FastAPI reads the module's attribute, which that import binds
(Python Language Reference, The import statement), so the walk follows the one module-level import that binds the
name, or, when none does, the one `from m import *` whose module declares it; a module that declares the name itself,
binds it by several imports or by a whole-module import, or whose star imports supply it from several modules or
none, decides nothing. The walk then decides the source by FastAPI's own rule
(`fastapi/dependencies/utils.py` `analyze_param`): a name the route writes is a path read unless the class is a
special type; else a body for a model or a dataclass, a file for an `UploadFile`, a query for any other class. A
request subclass, a special type, a class no member declares, several declarations that disagree and bases of
different kinds publish nothing. The read is one `variable` node in the file's scope, keyed
`input:<owner's qualified name>#<parameter>`, located at the parameter, with the `input` metadata the structural
provider's reads carry (key the parameter's name), and its owner `reads` it (detail `request_input`). The pending
inputs and the classes are part of the interface summary beside the routing, and every class lookup is traced on the
handler's file, so an edit to a model, to a base of it or to a re-export of it in another file re-resolves the
parameters it types. A class a library declares that FastAPI's own table does not name (any base outside the unit
other than Pydantic's, `typing`'s, `abc`'s, `enum`'s, FastAPI's and Starlette's) is undecidable from the project:
`analyze_param` reads it as a body only when it subclasses `BaseModel`, a mapping or `UploadFile`, or is a sequence or
a dataclass (`fastapi/_compat/shared.py` `field_annotation_is_complex`), which the library's own source decides, so it publishes
nothing.

A Spring handler parameter that no binding annotation marks and that no resolver Spring runs before the project's own
decides is a pending input under the rule `spring` (`internal/provider/link/route_input_spring.go`). Spring resolves
it with the project's own argument resolvers before its catch-all (`RequestMappingHandlerAdapter`
`getDefaultArgumentResolvers`), and those are code no table answers, so the linker publishes only in a unit where no
Java file names an argument resolver: every API a project registers one through names one in its types
(`WebMvcConfigurer.addArgumentResolvers(List<HandlerMethodArgumentResolver>)`, the adapter's setters). Each such file
is indexed under `keyJavaResolvers`. Each Java type's kind is indexed under `keyJavaKind`: an enum is a simple value
type, a concrete class a model attribute, an interface, an annotation type and an abstract class nothing. The
catch-all binds a simple value type (`BeanUtils.isSimpleProperty`: the primitives and their wrappers, `String` and
`CharSequence`, `Number`, `Class`, the dates and `Temporal` types, `UUID`, `URI`, `URL`, `File`, `Path`, `Charset`,
`Currency`, `InetAddress`, `Pattern`, enums, and arrays of them) as `@RequestParam`, a combined read keyed by the
parameter's name, and any other class as `@ModelAttribute`, a combined read with no key. A simple type the handler's
file decides is resolved there; any other type is looked up as the hierarchy reads a type name, and a type no member
declares (a library may resolve its own), or declarations that disagree, publish nothing. Both lookups are traced, so
adding a resolver, or an edit that turns an enum into a class or makes a class abstract, re-resolves the handlers.

A handler whose own file no route names (a Django view module `urls.py` names, a Starlette endpoint module, an
Express controller module) is told a handler only by a route of another file
(`internal/provider/link/route_handler.go`). The structural provider records, for each module-level function of a Python or ECMAScript file,
the reads of its first parameter under each request table of its language that the file's own scans did not publish
(Django's and Starlette's, Express's with its error-middleware arity), as pending inputs under the rule `handler`, and
each route records the handlers it names through an import with its framework (`Table.RouteHandlers`). Each such
handler is indexed under `keyRouteHandler` by the module it names: a Python import's absolute module, an ECMAScript
specifier's resolved module. The handler's file looks itself up under every name it can be imported or exported by,
and a Python entry's import is resolved again from the route's file as the route walk resolves a handler (the
nearest source root), so a function of the same name in another root is not taken; the reads of a function a route
of that framework names are published as the FastAPI reads are, keyed `input:<owner's node>#<framework:source:key#n>`.
The lookups are traced, so naming a function in a route publishes its reads. A handler reached only through a
re-export, a method, a function written inline in another call, and the `data_flows_to` of the read to the variable
it defines, which only the structural provider publishes, are not read.

## Declared entry points

The code a project's manifest declares as its entry points is an `endpoint` of the file it resolves to (ADR-0017
decision 8, `internal/provider/link/route_entry.go`). The linker reads the manifest at the unit's root:

| Manifest | Entries | Resolved to |
|---|---|---|
| `pyproject.toml` | `[project.scripts]` (group `console_scripts`), `[project.gui-scripts]` (`gui_scripts`), `[project.entry-points.<group>]`, each `module:attr` or `module` | the declaration `attr` names in the member file the module can be imported as (from the root or `src/`), or that file's module |
| `package.json` | `bin` (a path, or a map of command name to path), `main` | the member file Node's `require` loads for the path (the path, then `.js`, `.mjs`, `.cjs`, then its `index.js`), its module |
| `Cargo.toml` | `[[bin]]` with its `path`, else `src/bin/<name>.rs` or `src/bin/<name>/main.rs` | the file's top-level `fn main`, else its module |

Each is keyed `endpoint:declared:<group>:<name>@<manifest>` (the group alone when it is the name, as for `main`),
located at its target, with a search document of its descriptor, defined by the file's module and routing to the target
(detail `import`). A name several member files can supply publishes one endpoint in each, each with the set size; a
value that names no member publishes nothing. The entries are manifest fields of the unit, so a change to one
re-resolves the unit.

## Inherited Java `main`

A Java class is launched by the `main` it declares or inherits (JLS SE 25 §12.1.4): the one with a `String[]`
parameter before the one with none, a declaration overriding an inherited `main` of the same parameters. The
structural provider decides every class whose answer its file holds; a class whose superclass chain leaves its file,
with no `String[]` main declared nearer, it marks for the linker (`Decl.MainInherited`), and it marks every
launchable method with its form (`Decl.Main`, indexed under `keyJavaMain` by its class). The linker walks the
class's superclasses through the unit (`keySuper`, the one supertype that is no interface) and chooses the nearest
`String[]` main, else the nearest main with no parameter; a chain that reaches a class outside the project ends
there, with what the project declares (`internal/provider/link/java_main.go`). The class's file publishes one
`endpoint` named `main` with the identity the structural provider gives the main of a class it resolves itself (the
file, the descriptor `main` and the class's qualified name as disambiguator), so an edit that moves the decision
between the two providers moves no endpoint. It is located at the class, defined by the file's module and routing to
the method (detail `hierarchy`), and carries the search document the structural provider gives that endpoint (the same
document identity, body `main`, located at the class). A superclass the file names ambiguously is a path of its own; when the paths choose several
methods each `routes_to` carries their number as its set size. The reads are index lookups of the hierarchy, so an edit to a superclass's `main` or to its
`extends` re-resolves the classes that read it.

## Endpoint identity per family

Every endpoint is a structural declaration node. Its scope is `file:<path>` and its native key is the qualified
name (docs/providers-treesitter.md). Its identity is its file, kind, language, descriptor path (the qualified name
along its ancestry) and disambiguator, never its byte range (ADR-0014, `reconcile.CanonicalKey`). The disambiguator
is, in order, the parameter types as written where the language overloads (Java, C++, TypeScript), the conditional
branches that enclose the declaration (`wire.Decl.Branch`: an `if`, `try`, `switch` or `match` arm, a preprocessor
condition, a Rust `#[cfg]`), the parent's disambiguator for a nested declaration, the prototype mark of a C or C++
declaration without a body, and, for exact duplicates left at one level, their content: the first keeps the plain
identity and each later one is keyed by the digest of its bytes (`reconcile.SettleDuplicates`). A parameter is keyed
by its callable's identity and its position (`wire.Link.Parameters`), the file's module by its path. Locals,
anonymous functions and call-site nodes keep file-scoped identities that include their range. So a body edit, or a
declaration inserted before another (an overload included), moves no endpoint, and an edge another file published
toward it stays valid. What a call can bind to differs by family, and so does what the table's
`Decl.Receiver` holds (`wire.Export`, `internal/provider/treesitter/wire/link.go`):

| Family | Targets | `Receiver` |
|---|---|---|
| Java | methods and constructors; `Static` for static methods; `Abstract` for interface and abstract methods; `Final` for a `final` or `private` method, or one of a `final` class, a record, an enum without constant bodies or an anonymous body | the enclosing class, interface, record or enum; `#n` for an anonymous class body or an enum constant's body, n the byte offset of the name token of its declaration (the class `new T`, or the enum constant), sent as a subtype of the type it instantiates |
| Go | functions and methods; interface methods, `Abstract` | a method's receiver base type (`*T` is `T`); the interface for an interface method |
| Rust | functions, associated functions (`Static` when there is no `self`), methods, trait methods (`Abstract` without a default body); `self` is not one of `Params` | the impl's self type; the trait for a trait method |
| C, C++ | functions, methods (in-class and out-of-class) and function-like macros; `Abstract` for a pure virtual function or a declaration a subtype supplies; `Virtual` for a member declared `virtual`, `override` or `final`, `Final` for one declared `final` or of a `final` class, as the declaration says (an out-of-class definition repeats neither); `Internal` for a namespace-scope function declared `static` anywhere in its file or in an unnamed namespace, which only its own file, a file including its header directly, or (with the functions of external linkage) a file that may include its header through another binds, and for a class, struct, union or enum defined in an unnamed namespace, which its own file's type names reach alone; `Optional` counts default arguments and every macro parameter, `...` and a pack are `Variadic`, and so is a C prototype's `()` | the enclosing class or struct, or the qualifier of an out-of-class definition |
| JavaScript, TypeScript, TSX | functions, class methods, object-literal methods | the class |
| Python | functions and methods; `Static` for `@staticmethod` and `@classmethod` | the class |

A declaration the worker sent no type facts for is still a target by its name (`Decl.Described` is false).

## Units and inputs

A unit is one project of the repository, the scope `link.PlanUnits` derives
(`internal/provider/link/projects.go`). A file belongs to the unit that owns it (`Unit.OwnsInput`,
`projects.go`). The planner is this provider's own.

`Link` plans only its own unit, through `PlanUnit` (`projects.go`), in one read of the manifest, and its members are the
candidates `Unit.Contains` admits. The delta build hands it the unit's inputs as file versions in FileID order
(`LinkOptions.Members`), so it keeps its members of them and sorts nothing.

The unit's inputs are its member files and the manifests it owns; an ECMAScript unit also owns every
`tsconfig*.json` and `jsconfig*.json` (a `tsconfig.base.json`, a referenced `tsconfig.app.json`), `package.json`,
`pnpm-workspace.yaml` and `lerna.json` under its root (`esConfigFile`), each of which its resolution reads. A
`pnpm-workspace.yaml` or `lerna.json` marks a JavaScript project as `package.json` does, so a workspace with no root
`package.json` is one unit, whose packages import each other by name. The unit's identity is their content, as for every unit: the facts it
publishes bind their evidence to each file's bytes and ranges (`model.NewEvidenceID`), and a declaration's node id
is its qualified name, so a unit reused on a match of anything less would serve evidence about bytes that are gone.
Each input also records `model.UnitInput.Digest`, the member digest of the file's link table (`link.MemberDigest`,
`internal/provider/link/tables.go`). The digest is outside the unit's identity: `UnitInputHasher` does not
read it. It decides what a rebuild costs (Refresh, below).

**The link tables.** For each file with a lowering, the worker sends one `KindLink` message (`wire.Link`). The
structural provider joins it to the declarations and call references it validated, and stores the result as a
`link.Table` with the file's structural unit, under `link.TableKind`, and the table's member digest beside it, under
`link.DigestKind` (`storeTable`, `internal/provider/treesitter/link.go`). It stores both through
`provider.StateSink` (`internal/provider/sink.go`), so they share the unit's lifetime. The linker reads a
table back through `link.Tables`, and the delta build a digest alone through `link.Digests` (`tables.go`): the
structural unit the generation selects for the file's scope, then that unit's state of the kind. A file with no
lowering, or whose dependence or link pass failed, has neither.

Besides the joined declarations, sites and values, the table carries what the resolvers read a name in
(`internal/provider/link/table.go`, filled by `internal/provider/treesitter/link.go`):
- `Package`: the file's package or module clause (`wire.Done.Package`);
- `Imports`: one `Import{Name, Path, Original, Syntax}` per name an import introduces, `Path` being what the name
  denotes as the language writes it and `Original` the name it denotes in that module when the import renames it.
  `Syntax` is an ECMAScript import's form (`require` for `require("m")` and `import x = require("m")`, `dynamic` for
  `import("m")`, empty for a declaration), which selects a package's `import` or `require` condition. A Rust use
  tree is split into one path per leaf (`use a::{b, c as d}` gives `b → a::b` and `d → a::c`). An import of every
  member of a path (a Go dot import, a Java on-demand import, a C++ using-directive, a Rust glob, a Python
  `from m import *`) is named `ImportAll`. An include or a side-effect import has no name;
- `Decl.Prototype`: a C or C++ function declaration without a body (`wire.Decl.Prototype`);
- `Import.Original`: the name a local import name denotes in the imported module when the import renames it (`a`
  for `import { a as b }`, `default` for a default import, `*` for a namespace import, `A` for `from m import A as B`);
- `Reexports`: what an ECMAScript module's `export` statements export under a name of their own or from another
  module (`wire.Reexport`): `export default foo` and a default-exported declaration, `export { a as b }`,
  `export { x } from`, `export * from`, `export * as ns from`;
- `Unions`: the type sets a `TypeUnion` names, every type the worker recovered for a value that disagreeing reaching
  definitions or arms reach.
- `Import.Range`: where the import is written, the location of its `imports` edges;
- `DynamicImports`: each import whose path does not fold to a constant, with its range and its literal fragments;
- `Decl.Parameters`: each parameter's node, position, its variadic and keyword-only marks, which argument flows
  bind to, and its `Returned` mark, the callable's return summary;
- `Decl.Return`: the callable's return node, which return flows leave from;
- `Decl.Override`: the language marks the method as overriding (`@Override`, `override`, `typing.override`, a trait
  impl item); `Decl.Trait`: for a Rust trait impl item, the block's trait, the one trait whose item it implements:
  the trait's method of that name when the trait is the unit's, else the external trait's method, the self type
  never consulted, so a blanket impl over a type parameter implements its trait's item too;
- `Decl.Alias`: the one identifier an object-like macro is replaced by (`#define LOG log_impl`), which a call of the
  macro calls;
- `Decl.Visibility` and `Decl.Macros`: the visibility an object-like macro's replacement states, and the macros a C or
  C++ declaration is written with, which its `references` visibility occurrences read;
- `Site.Arguments`: each argument's range, keyword name, spread mark and the variables it reads;
- `Site.Results` and `Site.Into`: the variables the call's value is written into, and the callable whose return it
  reaches;
- `Site.Computed`: the callee is an expression that names nothing (`wire.Link.Computed`);
- `Handlers` and `Site.Handler`: the anonymous route handlers the structural provider publishes as function nodes,
  and the one a site is written in.

**Two summaries.** Each table has an interface summary and a sites summary, each with its own digest
(`Table.Interface`, `InterfaceDigest`, `SitesDigest`, `internal/provider/link/summary.go`). The interface is
everything another file's resolution reads from the file: its declarations with their node identities, signatures,
flags and returns, its supertypes, imports, package, re-exports, the unions they name, and its values (the functions
it takes as values and the fields it writes). Its entries are keyed by stable identity (a declaration's node, an
export's name, a field's name) and hashed, so two interfaces differ entry by entry. The sites summary is the file's own
call sites. Neither covers a byte range. The member digest folds both and the table's `Dropped` count
(`MemberDigest`, `tables.go`).

## Refresh

A linking unit stores its resolution state under `link.ResolutionKind` (`internal/provider/link/state.go`): the
manifest fields the linker reads, one record per member (its content hash, member, interface and sites digests, and
its coverage: whether it has a table, the entries dropped, and its site and import counts), and every fact by
identity: a site by its file and its index in that file's table, a target by its file and its declaration, module or
parameter node, or by its external key. A
site's index is valid across builds because a member whose bytes did not change has the same table, and a member
whose bytes changed re-resolves whole. For Go, Java, Rust, C and C++ the state also holds what the refresh re-runs
from: a hash of each hierarchy index key's entries, the reverse index from every key a file's resolution read to that
file, the functions each member takes as values by signature, and the hierarchy's two index files with the table of
identities their positions denote and each member's projection (`projection.go`). For Python, JavaScript, TypeScript and TSX it holds the same hierarchy and
identity table, a module index (each Python declaration and module under every dotted name its file can be imported
as; each ECMAScript file under its module path, its path without extension) with the hash of each of its keys'
entries, and, for every node of the value-flow fixed point (a site, or a file's node owning its callables' returns,
its field writes, its imports and its hierarchy facts): each value it put into a variable (a parameter, a site's
result, a callable's returns, a field) with the stratum, before or after field-based resolution, the value arrives at;
each variable it read; and each key of the hierarchy or the module index it looked up, found or not. All of it is
named by identity (`internal/provider/link/flowstate.go`): a node by its file and site index, a parameter by its
callable's node and position, a value by its declaration's node, so an added or removed file renumbers nothing.
Everything is a sorted run on disk; nothing the size of the unit is held in memory.

The delta build hands `Link` the predecessor's state whenever the predecessor stored one this build can read
(`LinkOptions.Previous`, `internal/index/delta/link.go`), with the member digests the structural units stored. `Link`
classifies every member against its record (unchanged, content changed with the same table, table changed, added,
removed) and every manifest by the fields the linker reads (`manifestFields`, `refresh.go`): a Go module's `module`
path, its `go` and `toolchain` lines, each `require` and `replace`, the governing `go.work` and its `use` and
`replace` directives, the vendor directory's digest, and the host's Go root and the presence of each required module
version in the module cache (`goManifestFields`, `manifest_go.go`); a Cargo manifest's crate names; and an ECMAScript unit's configs after `extends` (every option module
resolution and ownership read), each `package.json`'s `name`, `type`, `exports`, `imports`, `types`/`typings`,
`module`, `main`, `tsconfig` and `workspaces`, and each workspace file's globs (`jsManifestFields`, `js_manifest.go`).
A changed ECMAScript field rebuilds the hierarchy, whose specifier answers (`keyESSpec`) are diffed key by key like
every other index group, so only the nodes whose imports now resolve differently re-run, in any file of the unit.
Nothing else of a manifest is read: the Python and Java project files decide only which unit a file belongs to, so a
change to them is a change of membership or nothing. A change to the Go fields other than the `module` path, which only
say where a package outside the unit is read from, keeps the layout (`goExternalField`, `go_refresh.go`): the packages outside the unit are read
again over the stored hierarchy, and the hierarchy is kept when the digest of everything they declare
(`goExternal.surface`, stored as a group) is the predecessor's, an early cutoff, so a dependency bump that changes no
declaration the unit reads re-runs nothing and reads no table. One that does rebuilds the hierarchy, and only the
members whose reads changed re-run. A new nested `go.mod` moves its files out of one unit into another, a removal
from one and an addition to the other.

What re-resolves:
- **Nothing**, when no table, member or field moved: the predecessor's bindings stand.
- **Go, Java, Rust, C, C++** (`rerunStatic`, `static.go`): every resolution read is recorded at the hierarchy's one
  lookup point (`index.lookup`, lookups that found nothing included), at the Go module path (`goPackages`) and at the
  signature cross-product, as a trace key that names files and declaration nodes, never positions
  (`internal/provider/link/trace.go`). A refresh re-runs the members whose table moved, the added members, the members
  whose reverse-index keys name a changed index group or a changed manifest field, and then the members whose
  indirect sites read a signature whose set of taken functions changed. A file re-runs whole, since its sites read
  each other's results. When the layout holds (the same members, the same fields but those saying where a Go package outside the unit is read from, and every member whose table moved
  keeps its interface digest) the refresh reads the hierarchy from the state and decodes only the tables it re-runs
  and republishes; otherwise it rebuilds the hierarchy and diffs it group by group. The rebuild reads the tables of
  the members whose digest moved and of the added members only: for every other member it reads the projection the
  state keeps (`internal/provider/link/projection.go`), the table without its sites, values, handlers and dynamic
  imports but with each routing mount's argument and the Java anonymous bodies' offsets, which is everything the
  hierarchy is built from. A full build reads each table once and builds its second pass from the projections.
- **Python, JavaScript, TypeScript, TSX** (`resolveFlow`, `refresh.go`): their resolution is one least fixed point of
  value flow and binding, decomposed into nodes (each site, and each file's node), which a refresh maintains by
  deleting and re-deriving at the granularity of nodes (Gupta, Mumick and Subrahmanian, "Maintaining Views
  Incrementally", 1993). When the layout holds the refresh reuses the stored hierarchy and module index; otherwise it
  rebuilds both from the stored projections and the tables of the members whose digest moved or that are added,
  and diffs them group by group. It then re-derives a region of nodes, starting from
  every node of each member whose table moved, that is added or that is removed, and every node that looked up a key
  whose group changed (a declaration, export, parameter or module added, removed or changed under it, so an added
  file's names re-run exactly the sites whose lookups now see them). Until the region settles:
  - the nodes the stored graph reaches from the region (each reads a variable a region node contributed to) are set
    aside, and the region runs on the contributions of the remaining, untouched nodes alone. A value the region
    contributed before that this isolated run and the untouched nodes no longer give its variable (or give only in a
    later stratum) is removed: its readers outside the region, and every node they reach, join the region, which runs
    again. A value that cycled from the region through untouched files back into it is never an input of the isolated
    run, so it cannot keep supporting itself once its source is gone;
  - with nothing removed, the region runs on the contributions of every node outside it, and each variable whose
    values or strata now differ from the stored ones adds its readers outside the region, which runs again; when none
    differ the refresh stops (early cutoff).

  Every node outside the final region read exactly the values and keys it read before, so it keeps its facts,
  contributions, reads and lookups; the region's are found again, and when the layout holds only the region's tables
  are read. A body edit whose values come out the same re-runs its own file's sites only; a changed returned value or
  parameter list re-runs the sites that read it, not their whole files. A cycle whose values reach the edited file
  only through other files re-runs the cycle's readers of what the edit removed, which is conservative and never
  unsound.

  The stored contributions and reads are written once per refresh to block-indexed files, by node, by entry and by
  this build's ordinals (`flow_seek.go`), and each iteration reads them by key range: the region's nodes, the entries
  those touch, the variables its run reads and the returns of each callable a round of the run binds, read when it
  binds it. A node whose stored contribution names a
  member or declaration that is gone joins the first region. No iteration passes over the unit's stored state; only
  carrying the untouched nodes' state into the new one does, once.

What is republished (`refresh.diff`): the members whose bytes changed, the added members, the members whose bindings
changed, and the members whose set of targeted declarations changed (a target's occurrence node is published in its
own file's facts, so a declaration that becomes or stops being a target republishes its file). Those and the removed
members are named replaced (`LinkOptions.Replaced`); every other fact is carried over (`sqlite.CarryOver`). So a body
edit, with or without a new call, republishes the edited file; a rename republishes the edited file and the files
whose sites named the old name; an inserted overload republishes its file and keeps every existing overload's node.

A predecessor that stored no state, or a state this build cannot read (its format tag, `link.ValidateState`), resolves
in full, with `Result.FullReason` `no_previous_state` or `unreadable_previous_state`, publishes every fact, carries
nothing, and stores its state for the next.

## Resolution

Each resolver reads the unit's tables one at a time, in FileID order (`unit.Files`, `unit.go`), and emits each
`Binding` it finds (`unit.go`). The core sorts every binding by site and target in an external sort that
spills to disk, and publishes from the sorted stream. The type and declaration questions a resolver asks (which
declarations a name, a package or a module holds; a type's supertypes, subtypes and members) are point lookups into
sorted index files the hierarchy writes to scratch: a lookup reads from the 32 KiB block that holds its key to the
key's last entry, located by the first key of every block, which is the one part of an index held in memory (`index`, `internal/provider/link/hierarchy.go`).
Nothing else the size of the project is held in memory. No binding is justified by the callee's name alone: each rests on a scope,
an import, a type or a signature.

A site none of these reach is not guessed at: it is an open site (What it publishes, Open sites), and the candidates
a name alone would give are computed when a query asks (`may_call`), never stored as facts. So `x.run()` with an
untyped `x` publishes one `calls` to its open-site node, reason `receiver_untyped`, and no edge to any `run` the unit
declares.

The members' language family picks the resolver:
- **Java** (`resolveStatic`, `internal/provider/link/resolve_static.go`): class-hierarchy analysis. A static,
  constructor or `super` site binds directly, and a bare call a static import names binds the imported method. A
  bare call binds in the innermost enclosing class that has the method, declared or inherited, walking out from an
  inner, local or anonymous class (JLS §15.12.1). A member site binds by its receiver's static type to the one
  method that type declares or inherits, `virtual` when that method can be overridden (not static and not `Final`,
  `hierarchy.overridable`) and `exact` otherwise; a call on a newly created object (`new Dog().sound()`, no class
  body) binds exactly, the object being exactly its class (JLS §15.9.4). An anonymous class body is a subtype of
  the type it instantiates, read as its declaration (`anonymousType`: the class `new T`, or the enum constant whose
  body it is), whose methods override that type's. A call of an abstract method (a functional
  interface's) also joins every method reference of that method's signature. A method reference `T::m` to an
  instance method, `T` a type, is unbound (JLS §15.13.1): its signature is `m`'s with `T` as its first parameter, and
  a call through it passes its first argument to the receiver and the rest to `m`'s parameters in order. A static
  method's, a constructor's and a bound one's (`this::m`) take the arguments as their own parameters. A receiver that is a field of a class,
  in its own file or another, is typed by the field's declared type, inherited and enclosing-class fields included.
- **Go, Rust, C, C++** (`resolveStatic`): direct binding and class-hierarchy analysis over interfaces and traits.
  Bare and static sites bind by the language's scoping: same package, import, `use`, namespace and
  using-directive, and a prototype yields to the definition it announces (`Decl.Prototype`). A Go import path names
  a package of the unit only through the module path of the `go.mod` at the unit's root: the module path is the
  root, a path under it the directory it continues with, any other path the root's `vendor` copy; a unit with no
  `go.mod` binds no import path (`hierarchy.goPackages`, `internal/provider/link/hierarchy_go.go`). A Go type's method
  set is its own methods and, depth by depth, those its embedded fields and embedded interfaces promote. A name a
  shallower depth declares, as a method or as a named or embedded field, is not promoted; a name a struct reaches at
  one depth along two embedding paths, or as a field, promotes nothing. Embedding is promotion, never subtyping: a call on an embedded type never dispatches into a type that embeds it. A promoted method is listed
  under the embedding type, and implicit interface satisfaction compares whole method sets (`hierarchy.satisfy`). A Go
  member site on a field (`x.cfg.Load()`) is typed by the declared type of the field that selector denotes on the
  owner's type, its own or promoted, read in the file that declares it, and a chain (`x.a.b.Load()`) step by
  step the same way (`fieldGo`). An
  indirect site joins every function taken as a value whose declared signature equals the site's
  (`wire.Export.Signature`, `wire/link.go`); a Go method expression `T.M` taken as a value has the method's
  signature with `T` as its first parameter (`goMethodExpression`). This is the address-taken × indirect-site
  cross-product, with no reachability fixed point. A C++ member call is `virtual` only through a member that is not
  `Final` and that the receiver's type or a supertype declares `virtual` ([class.virtual]). In C++ a bare call inside
  a class walks out to the enclosing classes' static members. A Go method set compares methods by
  name and type identity (`goIdentity`, `internal/provider/link/go_identity.go`): the worker sends each method's and
  alias's form (`wire.Export.Identity`), and the linker names each type by its package's import path and name,
  however the file writes it (a renamed or dot import, a vendored copy), follows aliases (`byte` is `uint8`, `rune`
  `int32`, `any` `interface{}`, a user alias its type), and qualifies an unexported method name by its package. Type
  parameters compare by position. A form it cannot decide (a struct or interface literal with members, an alias it
  cannot follow, a name two dot imports declare) matches nothing, never a guess. A Go interface embedded from a
  package outside the unit contributes its methods as read from that package (above, Hierarchy). `#define NAME func` makes a call of `NAME` a call of `func`,
  one per definition when `#if` arms define it differently, in the file and in the headers it includes.

In every static family a site binds only a target its arguments fit: at least `Params - Optional` (one fewer for
a `Variadic` one, whose last parameter may take none) and, unless `Variadic`, at most `Params` (`staticFile.fits`).
A C or C++ definition takes `virtual`, `final` and default arguments from its prototypes of the same signature
(`preferDefinitions`). A Go method expression, a Rust path naming a method or a Java unbound method reference
passes its receiver as the first argument. A call the worker did not describe (`Site.Described` false) carries no argument list and is not filtered. A receiver that is
another call's result (`TypeResult`) has the types that call's targets return, each read in its target's file
(`staticFile.results`), exactly that type when the target returns it by value (a C++ class, `wire.ExactObject` on
`wire.Export.Returns`).

A Rust path is read through the module it names in its own crate (`internal/provider/link/resolve_rust.go`). A
crate is the directory Cargo's layout makes its root: the nearest `src` (`lib.rs` and `main.rs` are the root,
`a/b.rs` is `a::b`, `a/mod.rs` is `a`), or `src/bin`, `tests`, `examples` or `benches`, each of whose files is a
crate root. Every function and type is keyed by its module, inline `mod` items included. A path is read from the
module it is written in, an inline `mod` included (each `link.Import` carries the declaration it is written in,
`Import.Scope`): `crate` is the root, `self` the module, each `super` one module up, `::name` a crate, and any other
first component a module the current one declares, else a crate of the unit by the name its `Cargo.toml` gives its
library (`[lib] name`, else `[package] name` with `-` as `_`; `rustCrates`), once a `use` binding it has been read. A
bare name sees the functions and `use`s of its own module and enclosing blocks, never those of an enclosing module,
so `mod tests { use super::*; }` reads `super` from `tests`. A path that names no module of the unit binds nothing,
and no Rust declaration is reached by a suffix of its qualified name. For example, `use crate::a::util; util::f()`
binds `src/a/util.rs`'s `f` and never `src/b/util.rs`'s. A method path taken as a value (`let g: fn(&Foo, i32) =
Foo::m`) joins on its receiver-first signature. A call's result declared as a smart pointer (`-> Rc<Foo>`) reaches
`Foo`'s methods, except those the pointer answers itself (`rustReceiver`). A `macro_rules!` is never a call target.

A type name is read in the file's scopes, then through the import that names it (which decides alone), then
through the file's imports of every member. A name none of them can supply falls back to every type of that name
in the unit only in Go, Rust, C and C++, and only when every import of the file names a declaration or file of the
unit (`hierarchy.closedImports`); never in Java, ECMAScript or Python, where such a name is the package's,
`java.lang`'s or a global (`hierarchy.typesIn`).

- **JavaScript, TypeScript, TSX** (`resolveECMAScript`, `internal/provider/link/resolve_ecmascript.go`): direct
  binding through imports and scopes. A bare name binds only the declarations its scope sees, those of the innermost
  scope declaring it (`Table.Visible`), never a class member nor a function stored under a property (`obj.run = () =>
  …`, `{ k: () => … }`, `Decl.Property`); `f = () => …` binds the name `f` as a declarator does. A function a
  property assignment or a pair declares is the value written to that property, named by its declaration exactly
  (`Value.Declares`), so field-based resolution reaches that declaration's node. A module's exports follow its
  `export` statements and its CommonJS exports, each a module export record read by syntax at any depth
  (`internal/provider/treesitter/worker/js_commonjs.go`): a function stored as `module.exports.run = …`, `exports.run = …` or a key of
  `module.exports = { run: … }`, exported under its own name and written to no field; `exports.x = v`,
  `module.exports.x = v`, `exports["x"] = v` and `Object.defineProperty(exports, "x", { value: v })` or
  `{ get() { return v } }` with v a name, the export x aliasing v; a key of `module.exports = { a, b: c }` whose
  value is a name; `...require("./x")` in that object and `module.exports = require("./x")`, a re-export of x.
  `module.exports = v` (the right-most value of a chain such as `module.exports = exports = f`) and TypeScript's
  `export = v` are the module's whole value (`export=`): a call of the required module itself (`const f =
  require("./m"); f()`, `import f = require("./m")`) calls it, and an ES module's default import of the module
  reads it unless the module marks itself `__esModule`, when `exports.default` is the default (the compiler's and
  bundlers' reading; Node at run time gives the whole value, a documented difference). `exports = …`, or
  `module.exports = …` outside a chain assigning both, unbinds `exports`, so no `exports.x` of that file is an
  export. A file with an `import` or `export` statement is an ES module and none of these forms is read in it. A
  class expression assigned to a name or property (`module.exports = class { … }`) is a class of that name
  (`exports`), holding its methods, so the whole value is that class; a method of the object `module.exports` is
  assigned (`{ run() {} }`) is exported under its own name, as a function-valued key is. A function declaration,
  or a variable a function expression initializes (`const C = function () {}`), whose prototype the file stores
  a function under (`C.prototype.run = function () {}`, a method or function-valued key of `C.prototype = { …
  }`, the `value`, `get` or `set` function of `Object.defineProperty(C.prototype, "run", { … })`) is a class, a
  constructor function, and each such function a method of it, as TypeScript's JavaScript checking reads them;
  a getter and setter so defined are one declaration, the call. A class expression a declarator binds (`const
  C = class { … }`) is a class of that name. A function stored on the prototype of an imported function (`const
  C = require("./c"); C.prototype.run = …`) is a method of that function, and `new C()` of an imported function
  makes an object of it: every ordinary function is a constructor, and every importer shares the one function. A name a pattern binds from a literal
  `require("./m")` (`const { run } = require("./m")`) is the module's export, as an import's is, and a name a
  `require` call or an `import x = require("./c")` clause binds whole is the module's whole value, so `new x()`
  runs the constructor of the class the module assigns, as it would an imported class's. The object `new C()`
  makes is exactly a C, so a call on it is `exact`. A
  declaration only `export { a as b }` names is exported as `b` alone, and each imported name is resolved as
  ECMA-262 §16.2.1.6.3 ResolveExport does (`hierarchy.resolveExport`): the module's own export first, then a named
  re-export, then the `export *` modules, which pass on no `default` and nothing two of them resolve differently.
  A renamed or default import reads the name it denotes. A member site binds, for each type flow-based type
  inference recovered for its receiver, the method that type declares or inherits, never a subclass's override; a
  class object binds the method its class declares or inherits. A class field holding a function is a member a call through the class binds. A
  construction binds the constructor the class declares or inherits, and a call on its result binds the method that
  class declares or inherits, never a subclass's override. Field-based resolution binds a member site to the
  functions written to the property it calls through, only when its receiver bound nothing once the inference
  settled; a class's methods are written to no property, so they are reached only through a receiver's type. A
  receiver that reaches the method only through a supertype outside the project calls that supertype's external
  method, and the field-based fallback stays closed for it.
  **Module resolution** (`esResolver`, `internal/provider/link/js_resolve.go`, `js_config.go`): a specifier resolves
  as TypeScript resolves it, from the repository's own configuration in the snapshot, never `node_modules`. Every
  `tsconfig*.json` and `jsconfig*.json` is read as JSONC; `extends` (a path, an array applied in order with the
  later winning, or a workspace package's `tsconfig` field or `tsconfig.json`) is flattened with `compilerOptions`
  merged key by key, `files`, `include` and `exclude` replaced whole, every path relative to the config that wrote
  it, and `references` not inherited. A file's governing configs are those TypeScript's editor service picks: walking up from its directory to the
  unit root, the first `tsconfig.json` or `jsconfig.json` whose `files`/`include` minus `exclude` covers it (`.js`
  files only with `allowJs` or in a `jsconfig.json`), else the configs that config's `references` reach that do, all
  of them; with none covering it, the nearest config by directory; with no config, none. The module mode is the
  config's `moduleResolution`, else TypeScript's default for its `module` (CommonJS: Node10; node16/nodenext:
  Node16; preserve: Bundler); a config that sets neither, or no config, resolves under both Bundler and Node16. The
  order, under each governing config and mode: a relative or absolute path (TypeScript's extension substitution:
  `./a.js` names `a.ts`; a directory's `index` file except for an ES module's import under Node16, which names its
  file with its extension; then `rootDirs`); a `#` specifier through the `imports` of the file's package scope; the
  config's `paths` (an exact key, else the longest prefix, each fallback in order, the first naming a member
  winning; relative to `baseUrl`, else to the config that wrote `paths`); `baseUrl`; the package's own name when it
  has `exports`; a workspace package by `name` (the `package.json` files the root `workspaces`, a
  `pnpm-workspace.yaml` or a `lerna.json` declare, every `package.json` of the unit when none does) through
  `exports` where the mode reads it (an exact subpath key, else Node's `PATTERN_KEY_COMPARE` order; conditions
  `types`, `default`, `import` or `require` by the import's form and, under Node16, the file's module format, `node`
  under Node16, `customConditions`, matched in object order; a `null` target excludes the subpath), else `types`
  or `typings`, `module`, `main`, `index`; otherwise a package outside the repository, whose calls are external. A
  target that is no member (an `exports` entry into a build output the snapshot ignores) maps back to its source
  through a config at or above it that emits there: `outDir` or `declarationDir` replaced by `rootDir`, or, with no
  `rootDir`, by the config's directory and by the common directory of its `include` and `files`. Failing that, every
  branch of the entry that names a member is the answer, as a set; failing that, the import reaches no member and
  is counted as an open import (`FactOpenImport`), and a call through it is open, never external. Several
  governing configs, two workspace packages of one name, both modes: each is resolved and the union of the targets
  is published, with its size. Each import's answer is computed once per build, while the hierarchy is built, and
  written to the decls index under `keyESSpec` (the importing file's path and the specifier); the import facts, the
  export resolution and the inference each look that one key up.

  Limits. Aliases defined in program code (a bundler's alias table, a test runner's module mapper, a compiler plugin)
  are not data and are not read. A package outside the repository (an ignored `node_modules`, a package manager's
  own cache) stays external, and an `extends` base inside one leaves the options it may set unknown, so a config
  that does not set its own mode resolves under both. `typesVersions` and `types@` conditions depend on the TypeScript version and are not
  read. A pnpm-workspace.yaml is read for its top-level `packages` sequence alone. An `extends` base under a name
  other than `tsconfig*.json`/`jsconfig*.json` is read but is not an input of the unit, so an edit to it alone is
  seen at the unit's next rebuild.
- **Python** (`resolvePython`, `internal/provider/link/resolve_python.go`): direct binding through `import`,
  `from … import` and modules. A bare name binds only the in-file declarations its scope can name: those of the
  innermost function enclosing the call that declares one, else the module's (`Table.Visible`); never a method, nor
  a function nested in another function. A call of a class is its construction and binds its `__init__` when
  declared, and a call on its result (`C().m()`) binds the method `C` declares or inherits alone. An instance
  receiver (`self`, `cls`, an annotation, or a type flow-based inference recovered, every type of a receiver two
  disagreeing definitions reach included) binds, for each of its types, the first declaration of the method along
  that type's C3 linearisation (`hierarchy.pythonMRO`, over the bases in the order each class lists them), never a
  subclass's override. A class object (`A.make()`, `mod.A.make()`) binds the method `A`
  declares or inherits alone, never a subclass's override. `super().m()` in class `B` binds, for `B` and each class
  below it, the first `m` after `B` in that class's linearisation, so no override below `B` is reached. Field-based
  resolution (a function written to the attribute; a method is written to no attribute, so it is reached only through
  a receiver's type) answers an attribute call only when its receiver's type bound nothing and reaches the method
  through no supertype outside the project: `class L(list)` calling `self.append(1)` calls the external
  `builtins.list.append`, never a function written to `append` elsewhere. A method taken as a value through its
  class (`f = C.m`) is unbound: `f(obj, n)` passes `obj` as `self` and `n` to the next parameter. One taken through
  an instance (`g = C().m`, `obj.m`, `self.m`) is bound: `g(k)` passes `k` to the first parameter after `self`. A
  static or class method taken through its class takes no receiver argument.

**Flow-based type inference** (`inferTypes`, `internal/provider/link/infer.go`) starts from the types the worker
recovered inside each function (`wire.Type`, `wire/link.go`):
- a `new C()` or an annotation is `TypeNamed` (a TypeScript `any`, `unknown`, `object`, `never` or `void` names no
  class and types nothing);
- a parameter is `TypeParam`;
- another call's result is `TypeResult`;
- a field read is `TypeField`;
- an imported module is `TypeModule`;
- a class object (a class name as a value) is `TypeClass`;
- a value two or more disagreeing types reach is `TypeUnion`, each member evaluated as itself.

It then propagates them across functions and files, iterated to the least fixed point, with no fixed round count:
- a parameter takes the types every bound caller passes;
- a result takes the `Returns` of the declarations its site binds;
- a field takes the types written to it (`Values`).

## Capability states

The provider publishes one row of each of its six capabilities per unit, at the unit's scope:
- `fresh` when every member had a table and no table dropped an entry;
- `partial`, with `coverage_incomplete` (`publisher.capabilities`, `internal/provider/link/provider.go`), when a
  member has no table (`members_without_table`) or a table's `Dropped` is non-zero (`dropped_table_entries`; a
  callable whose analysis failed in the worker counts there, since its values and its sites' recovered types are
  missing). The row then also counts the unit's `members`. A dropped entry is a worker entry the structural provider
  could not join to a declaration or a call reference of the file, or whose range did not fit the pinned bytes. None
  of these is fatal.
- `partial`, with `coverage_incomplete`, on the row of each relation a fact was found for whose target the structural
  provider minted no node (`facts_without_node`, counted by `externalsOf`, `state.go`): a call or hierarchy fact into
  a declaration with no node, or an argument's flow into a parameter with none (one whose key did not fit, a C or C++
  `...`) of a callable with a body. Such a fact publishes no relation; it is kept in the state with its target's file
  and no node (`idBinding.unnamed`), so a refresh carries and counts it like any other. A flow into a callable with
  no body (an abstract method, a prototype) and a return flow from a callee with no return node are not facts. A fact
  whose relation is no capability of its own (a route's handler, a declared entry point, a launchable `main`, a
  visibility macro) is counted on the `calls` row.

The `calls` row always carries the unit's site counts, which add up: `sites` = `bound_sites` + `open_sites` +
`computed_sites` + `external_sites`. A bound site has at least one target of the unit; an external site reached only
external nodes or was proven to leave the project; a computed site has a callee that names nothing; every other site
is open. The `imports` row carries `resolved_imports`, the imports resolved to at least one file, and
`open_import_sites`.

A file whose link pass failed in the worker is disclosed as `wire.Done.LinkFailure`
(`internal/provider/treesitter/wire/wire.go`) on that file's structural capability rows. It stores no table,
so the linking unit that owns it is `partial`.
