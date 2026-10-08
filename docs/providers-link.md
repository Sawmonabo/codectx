# The `link` provider

`internal/provider/link` is the package-scoped linking provider of ADR-0012 decision 9. It publishes `calls` for
the nine languages the structural provider lowers: C, C++, Go, Java, JavaScript, TypeScript, TSX, Python and Rust.
It never parses a file. It reads the link tables the structural provider stored with each file's unit, binds every
call site to the declarations it may call, and publishes each binding as a `calls` edge between the structural
provider's own nodes.

The structural provider still publishes its syntax-tier `calls` (docs/providers-treesitter.md, Unit scope and node
identity): a call it resolves inside the file, and the callee reference node of every call it cannot. The linker adds
the bindings that need another file, a type or a hierarchy. A site it cannot bind publishes nothing here, and the
structural call stands alone.

## Descriptor

```
ID                link
Version           1
Capabilities      calls
DependsOn         treesitter
InvalidationScope package
Required          false
```

`ProviderID`, `Version` and `Capabilities` are in `internal/provider/link/provider.go:37-50`, and the descriptor is
`Provider.Descriptor` (`provider.go:98-101`). The id is not `dependence`, which is never reused (decision 9). The
provider is not required: a failed link degrades `calls` and nothing else. It runs when the structural provider is
enabled and reports disabled with it; it has no configuration key of its own.

## What it publishes

| Capability | Relation | From → to | Evidence detail |
|---|---|---|---|
| `calls` | `calls` | caller → each bound declaration | `direct`, `hierarchy`, `address_taken`, `flow`, `field` |
| `calls` | `calls` | caller → ambiguous callee node | `ambiguous` |
| `calls` | `may_refer_to` | caller → each candidate of an ambiguous site | `ambiguous call target` |

The details are the `Detail*` constants (`provider.go:54-75`). Each one names the technique that bound the site:
- `direct`: the callee names the declaration by its package, module, import or scope;
- `hierarchy`: class-hierarchy analysis over the receiver's static type and its subtypes;
- `address_taken`: an indirect site joined to an address-taken function of the same signature;
- `flow`: the receiver's type was recovered by flow-based type inference;
- `field`: a function written to the field the site calls through.

**Caller and target.** The caller is the declaration enclosing the site, or the file's module node when no
declaration encloses it (`Site.Caller`, -1 for the module, `internal/provider/link/table.go:74-95`). A target is a
declaration of any member file of the unit.

**Evidence.** Every fact is `static_analysis`. A site publishes one evidence row per edge, at the call expression's
range (`Site.Range`). Its native key is the call-site key, spelled as the structural provider spells it:
`callsite:<path>:<first byte>-<last byte>` of the callee token, one-based and inclusive (`Site.NameStart`,
`Site.NameEnd`).

**Endpoints are the structural nodes.** The linker mints no second identity for a declaration. Each table carries
an `Endpoint` for each declaration and for the module (`table.go:45-52`): the node id, canonical key, kind, names
and range the structural unit resolved. The linker publishes an occurrence of every endpoint an edge names, with its
own evidence at the declaration's range. So the edge joins the structural node, and through it the cross-provider
`decl:` key and the call-site key of docs/providers-treesitter.md.

**Ambiguous sites.** A site bound to two or more declarations publishes into the ambiguous family the structural
provider already uses. The linker mints a callee node in scope `file:<path>` with native key
`link:<call-site key>` and metadata `{"resolution":"ambiguous","candidates":n,"callee":"<name>"}`. It publishes a
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
| C, C++ | functions and methods, in-class and out-of-class; `Abstract` for a pure virtual function or a declaration a subtype supplies | the enclosing class or struct, or the qualifier of an out-of-class definition |
| JavaScript, TypeScript, TSX | functions, class methods, object-literal methods | the class |
| Python | functions and methods; `Static` for `@staticmethod` and `@classmethod` | the class |

A declaration the worker sent no type facts for is still a target by its name (`Decl.Described` is false).

## Units and inputs

A unit is one project of the repository, the scope `dependence.PlanUnits` derives
(`internal/provider/dependence/units.go:136`). A file belongs to the unit that owns it (`Unit.OwnsInput`,
`units.go:359`). The planner is reused rather than copied.

The unit's inputs are its member files. Each input records `model.UnitInput.Digest`, the digest of the link table
the unit read for that file (`internal/model/storage.go:89-104`). The digest is outside the unit's identity:
`UnitInputHasher` does not read it, because the content hash already decides the table.

**The link tables.** For each file with a lowering, the worker sends one `KindLink` message (`wire.Link`). The
structural provider joins it to the declarations and call references it validated, and stores the result as a
`link.Table` with the file's structural unit, under `link.TableKind` (`provider.go:40-42`). It stores it through
`provider.StateSink` (`internal/provider/sink.go:240-249`), so the table shares the unit's lifetime. The linker
reads a table back through `link.Tables` (`internal/provider/link/tables.go:14-50`): the structural unit the
generation selects for the file's scope, then that unit's state. A file with no lowering, or whose dependence or link
pass failed, has no table.

**The digest** (`Table.Digest`, `table.go:118-149`) covers everything resolution reads. That is every name, kind,
shape, type fact, candidate and index, and the order of every list. It covers no byte range, position, node identity
or dropped count. An edit that shifts a range moves no name and no type. So two tables with one digest resolve every
site to the same declarations, by index; only the ranges and identities the edges are published with differ.

## The fast path

The delta build compares each member's fresh digest with its predecessor's, in FileID order, as one merge join
against the predecessor unit's stored inputs. When membership and every digest are equal, it hands `Link` the
predecessor's resolution state (`LinkOptions.Previous`, stored under `link.ResolutionKind`, `provider.go:43-46` and
`provider.go:77-85`). The linker then resolves nothing. It re-emits the stored bindings with the ranges and
identities of the fresh tables, because the state names each site and target by position (`Ref`,
`internal/provider/link/unit.go:38-41`). So an edit inside a function body that changes neither table relinks
nothing. Any other build resolves in full and stores its state for the next.

## Resolution

Each resolver reads the unit's tables one at a time, in FileID order (`unit.Files`, `unit.go:24-34`), and emits each
`Binding` it finds (`unit.go:43-54`). The core sorts every binding by site and target in an external sort that
spills to disk, and publishes from the sorted stream. Every join is an ordered merge over sorted runs. Nothing the
size of the project is held in memory. No binding is justified by the callee's name alone: each rests on a scope,
an import, a type or a signature.

The members' language family picks the resolver:
- **Java** (`resolveStatic`, `internal/provider/link/resolve_static.go`): class-hierarchy analysis. A static,
  constructor or `super` site binds directly. A member site binds by its receiver's static type to that type's
  method and to every override in a subtype (`hierarchy.dispatch`, `internal/provider/link/hierarchy.go:20-25`).
- **Go, Rust, C, C++** (`resolveStatic`): direct binding and class-hierarchy analysis over interfaces and traits.
  Bare and static sites bind by the language's scoping: same package, import, `use`, namespace, or a header
  prototype to its definition. Go's implicit interface satisfaction is derived from method sets. An indirect site
  joins every function taken as a value whose declared signature equals the site's (`wire.Export.Signature`,
  `wire/link.go:42-49`). This is the address-taken × indirect-site cross-product, with no reachability fixed point.
- **JavaScript, TypeScript, TSX** (`resolveECMAScript`, `internal/provider/link/resolve_ecmascript.go`): direct
  binding through imports and scopes. Member sites bind through receivers recovered by flow-based type inference,
  then `hierarchy.dispatch`. Field-based resolution binds a site to the functions written to the property it calls
  through. Class-hierarchy analysis applies where `extends` declares a subtype.
- **Python** (`resolvePython`, `internal/provider/link/resolve_python.go`): direct binding through `import`,
  `from … import` and modules. A call of a class is its construction and binds its `__init__` when declared.
  Receivers come from flow-based type inference, then dispatch in method resolution order. Field-based resolution
  covers attribute calls. `self` and `cls` are the enclosing class.

**Flow-based type inference** (`inferTypes`, `internal/provider/link/infer.go`) starts from the types the worker
recovered inside each function (`wire.Type`, `wire/link.go:134-167`):
- a `new C()` or an annotation is `TypeNamed`;
- a parameter is `TypeParam`;
- another call's result is `TypeResult`;
- a field read is `TypeField`;
- an imported module is `TypeModule`.

It then propagates them across functions and files, iterated to the least fixed point, with no fixed round count:
- a parameter takes the types every bound caller passes;
- a result takes the `Returns` of the declarations its site binds;
- a field takes the types written to it (`Values`).

## Capability states

The `calls` row is published at the unit's scope:
- `fresh` when every member had a table and no table dropped an entry;
- `partial` when a member has no table, or a table's `Dropped` is non-zero (`table.go:35-39`), with the counts in the
  row's details. A dropped entry is a worker entry the structural provider could not join to a declaration or a call
  reference of the file, or whose range did not fit the pinned bytes. It is never fatal.

A file whose link pass failed in the worker is disclosed as `wire.Done.LinkFailure`
(`internal/provider/treesitter/wire/wire.go:235-239`) on that file's structural capability rows. It stores no table,
so the linking unit that owns it is `partial`.
