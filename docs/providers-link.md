# The `link` provider

`internal/provider/link` is the package-scoped linking provider of ADR-0012 decision 9. It publishes `calls` for
the nine languages the structural provider lowers: C, C++, Go, Java, JavaScript, TypeScript, TSX, Python and Rust.
It never parses a file. It reads the link tables the structural provider stored with each file's unit, binds every
call site to the declarations it may call, and publishes each binding as a `calls` edge between the structural
provider's own nodes.

The structural provider still publishes its syntax-tier `calls` (docs/providers-treesitter.md, Unit scope and node
identity): a call it resolves inside the file, and the callee reference node of every call it cannot. The linker
publishes every binding it can justify, inside the file as well as across files: a site the structural provider bound
in its file is bound again when the linker reaches the same declaration, and the two edges have one relation id (the
caller, the kind and the target decide it), so the pair is one relation with both providers' evidence. A site the
linker cannot bind publishes nothing here, and the structural call stands alone.

## Descriptor

```
ID                link
Version           1
Capabilities      calls
DependsOn         treesitter
InvalidationScope package
Required          false
```

`ProviderID`, `Version` and `Capabilities` are in `internal/provider/link/provider.go:41-57`, and the descriptor is
`Provider.Descriptor` (`provider.go:123-126`). The id is not `dependence`, which is never reused (decision 9). The
provider is not required: a failed link degrades `calls` and nothing else. It runs when the structural provider is
enabled and reports disabled with it; it has no configuration key of its own.

## What it publishes

| Capability | Relation | From → to | Evidence detail |
|---|---|---|---|
| `calls` | `calls` | caller → each bound declaration | `direct`, `hierarchy`, `address_taken`, `flow`, `field` |
| `calls` | `calls` | caller → ambiguous callee node | `ambiguous` |
| `calls` | `may_refer_to` | caller → each candidate of an ambiguous site | `ambiguous call target` |

The details are the `Detail*` constants (`provider.go:61-82`). Each one names the technique that bound the site:
- `direct`: the callee names the declaration by its package, module, import or scope;
- `hierarchy`: class-hierarchy analysis over the receiver's static type and its subtypes;
- `address_taken`: an indirect site joined to an address-taken function of the same signature;
- `flow`: the receiver's type was recovered by flow-based type inference;
- `field`: a function written to the field the site calls through.

**Caller and target.** The caller is the declaration enclosing the site, or the file's module node when no
declaration encloses it (`Site.Caller`, -1 for the module, `internal/provider/link/table.go:106-127`). A target is a
declaration of any member file of the unit.

**Evidence.** Every fact is `static_analysis`. A site publishes one evidence row per edge, at the call expression's
range (`Site.Range`). Its native key is the call-site key, spelled as the structural provider spells it:
`callsite:<path>:<first byte>-<last byte>` of the callee token, one-based and inclusive (`Site.NameStart`,
`Site.NameEnd`).

**Endpoints are the structural nodes.** The linker mints no second identity for a declaration. Each table carries
an `Endpoint` for each declaration and for the module (`table.go:71-78`): the node id, canonical key, kind, names
and range the structural unit resolved. The linker publishes an occurrence of every endpoint an edge names, with its
own evidence at the declaration's range. So the edge joins the structural node, and through it the cross-provider
`decl:` key and the call-site key of docs/providers-treesitter.md.

**Ambiguous sites.** A site bound to two or more declarations publishes into the ambiguous family the structural
provider already uses. The linker mints a callee node in scope `file:<path>` with native key
`link:<call-site key>`, located in the site's file at the site's range, and metadata `{"resolution":"ambiguous","candidates":n,"callee":"<name>"}`. It publishes a
`calls` edge to that node and one `may_refer_to` edge per candidate. Nothing is capped: every candidate is published
and counted in `candidates`. This differs from the structural provider's in-file family, which keeps at most
`model.MaxAmbiguousCandidates` candidates and counts the rest as dropped.

A site bound to exactly one declaration publishes one `calls` edge to it.

## Endpoint identity per family

Every endpoint is a structural declaration node. Its scope is `file:<path>` and its native key is the qualified
name (docs/providers-treesitter.md). What a call can bind to differs by family, and so does what the table's
`Decl.Receiver` holds (`wire.Export`, `internal/provider/treesitter/wire/link.go:33-65`):

| Family | Targets | `Receiver` |
|---|---|---|
| Java | methods and constructors; `Static` for static methods; `Abstract` for interface and abstract methods | the enclosing class, interface, record or enum |
| Go | functions and methods; interface methods, `Abstract` | a method's receiver base type (`*T` is `T`); the interface for an interface method |
| Rust | functions, associated functions (`Static` when there is no `self`), methods, trait methods (`Abstract` without a default body) | the impl's self type; the trait for a trait method |
| C, C++ | functions, methods (in-class and out-of-class) and function-like macros; `Abstract` for a pure virtual function or a declaration a subtype supplies; `Virtual` for a member declared `virtual`, `override` or `final`, `Final` for one declared `final` or of a `final` class, as the declaration says (an out-of-class definition repeats neither); `Internal` for a namespace-scope function declared `static` anywhere in its file or in an unnamed namespace, which only its own file, a file including its header directly, or (with the functions of external linkage) a file that may include its header through another binds; `Optional` counts default arguments and every macro parameter, `...` and a pack are `Variadic`, and so is a C prototype's `()` | the enclosing class or struct, or the qualifier of an out-of-class definition |
| JavaScript, TypeScript, TSX | functions, class methods, object-literal methods | the class |
| Python | functions and methods; `Static` for `@staticmethod` and `@classmethod` | the class |

A declaration the worker sent no type facts for is still a target by its name (`Decl.Described` is false).

## Units and inputs

A unit is one project of the repository, the scope `dependence.PlanUnits` derives
(`internal/provider/dependence/units.go:136`). A file belongs to the unit that owns it (`Unit.OwnsInput`,
`units.go:359`). The planner is reused rather than copied.

`Link` plans only its own unit, through `dependence.PlanUnit`, in one read of the manifest, and its members are the
candidates `Unit.Contains` admits. The delta build hands it the unit's inputs as file versions in FileID order
(`LinkOptions.Members`), so it keeps its members of them and sorts nothing.

The unit's inputs are its member files. The unit's identity is their content, as for every unit: the facts it
publishes bind their evidence to each file's bytes and ranges (`model.NewEvidenceID`), and a declaration's node id
is its qualified name, so a unit reused on a match of anything less would serve evidence about bytes that are gone.
Each input also records `model.UnitInput.Digest`, the member digest of the file's link table (`link.MemberDigest`,
`internal/provider/link/tables.go:39-50`). The digest is outside the unit's identity: `UnitInputHasher` does not
read it. It decides what a rebuild costs (the fast path below).

**The link tables.** For each file with a lowering, the worker sends one `KindLink` message (`wire.Link`). The
structural provider joins it to the declarations and call references it validated, and stores the result as a
`link.Table` with the file's structural unit, under `link.TableKind`, and the table's member digest beside it, under
`link.DigestKind` (`storeTable`, `internal/provider/treesitter/link.go:319-340`). It stores both through
`provider.StateSink` (`internal/provider/sink.go:240-249`), so they share the unit's lifetime. The linker reads a
table back through `link.Tables`, and the delta build a digest alone through `link.Digests` (`tables.go:14-108`): the
structural unit the generation selects for the file's scope, then that unit's state of the kind. A file with no
lowering, or whose dependence or link pass failed, has neither.

Besides the joined declarations, sites and values, the table carries what the resolvers read a name in
(`internal/provider/link/table.go`, filled by `internal/provider/treesitter/link.go`):
- `Package`: the file's package or module clause (`wire.Done.Package`);
- `Imports`: one `Import{Name, Path, Original}` per name an import introduces, `Path` being what the name denotes as
  the language writes it and `Original` the name it denotes in that module when the import renames it. A Rust use
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

**The digest.** `Table.Digest` (`table.go:185-218`) covers everything resolution reads. That is every name, kind,
shape, type fact, candidate and index, the package, the imports and re-exports, and the order of every list. It
covers no byte range, position or node identity. An edit that shifts a range moves no name and no type. So two tables
with one digest resolve every site to the same declarations, by index; only the ranges and identities the edges are
published with differ. The member digest adds the table's `Dropped` count, which decides the unit's coverage.

## The fast path

The delta build reads each member's digest, one small read per member and no table decoded, and compares it with
its predecessor's in FileID order, as one merge join against the predecessor unit's stored inputs
(`internal/index/delta/link.go`). An input with no table (a manifest such as `go.mod`, or a member whose link pass failed) is compared by its bytes, because a resolver may read it: a Go module path decides which imports are the project's own. When membership, every digest and every tableless input are equal, it hands `Link` the predecessor's
resolution state (`LinkOptions.Previous`, stored under `link.ResolutionKind`) and the stream of members whose bytes
changed (`LinkOptions.Edited`, `provider.go:84-110`). The state names each site and target by position (`Ref`,
`internal/provider/link/unit.go`) and holds the unit's coverage, so the linker resolves nothing. It republishes only
the facts whose evidence lies in an edited member: the occurrences of its declarations that an edge names, and its
sites' `calls`, `may_refer_to` and ambiguous callee nodes. Every other fact of the predecessor is carried over
(`sqlite.CarryOver`) with the edited members named in `Replaced.Files`.

A table is read only when what is republished needs it: an edited member's, and an untouched member's when a
republished site binds one of its declarations. An untouched member whose path is long enough that an ambiguous
site's `link:` key could exceed `model.MaxNativeKeyBytes` is also walked, to count `unkeyed_ambiguous_sites`, and
publishes nothing. So an edit inside a function body resolves nothing and republishes one file's facts.

A predecessor that stored no state, or a state the build cannot read (`link.ValidateState`), resolves in full,
with `Result.FullReason` `no_previous_state` or `unreadable_previous_state`. Any other build resolves in full,
publishes every fact, carries nothing, and stores its state for the next.

## Resolution

Each resolver reads the unit's tables one at a time, in FileID order (`unit.Files`, `unit.go:27-41`), and emits each
`Binding` it finds (`unit.go:54-60`). The core sorts every binding by site and target in an external sort that
spills to disk, and publishes from the sorted stream. The type and declaration questions a resolver asks (which
declarations a name, a package or a module holds; a type's supertypes, subtypes and members) are point lookups into
sorted index files the hierarchy writes to scratch: a lookup reads from the 32 KiB block that holds its key to the
key's last entry, located by the first key of every block, which is the one part of an index held in memory (`index`, `internal/provider/link/hierarchy.go`).
Nothing else the size of the project is held in memory. No binding is justified by the callee's name alone: each rests on a scope,
an import, a type or a signature.

The members' language family picks the resolver:
- **Java** (`resolveStatic`, `internal/provider/link/resolve_static.go`): class-hierarchy analysis. A static,
  constructor or `super` site binds directly, and a bare call a static import names binds the imported method. A
  member site binds by its receiver's static type to that type's method and to every override in a subtype
  (`hierarchy.dispatch`, `internal/provider/link/hierarchy.go:814-821`). A call of an abstract method (a functional
  interface's) also joins every method reference of that method's signature.
- **Go, Rust, C, C++** (`resolveStatic`): direct binding and class-hierarchy analysis over interfaces and traits.
  Bare and static sites bind by the language's scoping: same package, import, `use`, namespace and
  using-directive, and a prototype yields to the definition it announces (`Decl.Prototype`). A Go import path names
  a package of the unit only through the module path of the `go.mod` at the unit's root: the module path is the
  root, a path under it the directory it continues with, any other path the root's `vendor` copy; a unit with no
  `go.mod` binds no import path (`hierarchy.goPackages`, `internal/provider/link/hierarchy_go.go`). A Go type's method
  set is its own methods and, depth by depth, those its embedded fields and embedded interfaces promote; a name two
  embedding paths reach at one depth of a struct is ambiguous and promotes nothing. Embedding is promotion, never
  subtyping: a call on an embedded type never dispatches into a type that embeds it. A promoted method is listed
  under the embedding type, and implicit interface satisfaction compares whole method sets (`hierarchy.satisfy`). An
  indirect site joins every function taken as a value whose declared signature equals the site's
  (`wire.Export.Signature`, `wire/link.go:42-49`); a Go method expression `T.M` taken as a value has the method's
  signature with `T` as its first parameter (`goMethodExpression`). This is the address-taken × indirect-site
  cross-product, with no reachability fixed point.

A type name is read in the file's scopes, then through the import that names it (which decides alone), then
through the file's imports of every member. Only a name that no scope or import of the file can supply falls back
to every type of that name in the unit (`hierarchy.typesIn`).

- **JavaScript, TypeScript, TSX** (`resolveECMAScript`, `internal/provider/link/resolve_ecmascript.go`): direct
  binding through imports and scopes. A bare name binds only the declarations its scope sees, those of the innermost
  scope declaring it (`Table.Visible`), never a class member. A module's exports follow its `export` statements and
  re-exports, closed over chains of re-exports (`export *` passes on every name but `default`), and a renamed or
  default import reads the name it denotes. Member sites bind through receivers recovered by flow-based type
  inference, then `hierarchy.dispatch`; a class object binds the method its class declares or inherits, never an
  override. A construction binds the constructor the class declares or inherits. Field-based resolution binds a
  member site to the functions written to the property it calls through, a class's methods included, only when no
  value reached its receiver once the inference settled. Class-hierarchy analysis applies where `extends` declares
  a subtype.
- **Python** (`resolvePython`, `internal/provider/link/resolve_python.go`): direct binding through `import`,
  `from … import` and modules. A bare name binds only the in-file declarations its scope can name: those of the
  innermost function enclosing the call that declares one, else the module's (`Table.Visible`); never a method, nor
  a function nested in another function. A call of a class is its construction and binds its `__init__` when
  declared. An instance receiver (`self`, `cls`, an annotation, or a type flow-based inference recovered, every type
  of a receiver two disagreeing definitions reach included) dispatches in method resolution order: for its class and
  each subclass, the first declaration of the method along that class's C3 linearisation (`hierarchy.pythonMRO`, over
  the bases in the order each class lists them). A class object (`A.make()`, `mod.A.make()`) binds the method `A`
  declares or inherits alone, never a subclass's override. `super().m()` in class `B` binds, for `B` and each class
  below it, the first `m` after `B` in that class's linearisation, so no override below `B` is reached. Field-based
  resolution (a function written to the attribute, or a method of that name, which the worker sends as its class's
  attribute) answers an attribute call only when its receiver's type bound nothing.

**Flow-based type inference** (`inferTypes`, `internal/provider/link/infer.go`) starts from the types the worker
recovered inside each function (`wire.Type`, `wire/link.go:136-169`):
- a `new C()` or an annotation is `TypeNamed`;
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

The `calls` row is published at the unit's scope:
- `fresh` when every member had a table, no table dropped an entry, and every bound site was published;
- `partial`, with `coverage_incomplete` (`publisher.capability`, `internal/provider/link/provider.go:243-260`), when
  a member has no table (`members_without_table`), a table's `Dropped` is non-zero (`dropped_table_entries`,
  `table.go:49-53`; a callable whose analysis failed in the worker counts there, since its values and its sites'
  recovered types are missing), or an ambiguous site's `link:` key would exceed `model.MaxNativeKeyBytes` and so has no callee
  node to mint (`unkeyed_ambiguous_sites`). The row also counts the unit's `members`. A dropped entry is a worker
  entry the structural provider could not join to a declaration or a call reference of the file, or whose range did
  not fit the pinned bytes. None of these is fatal.

A file whose link pass failed in the worker is disclosed as `wire.Done.LinkFailure`
(`internal/provider/treesitter/wire/wire.go:235-239`) on that file's structural capability rows. It stores no table,
so the linking unit that owns it is `partial`.
