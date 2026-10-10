package link

import (
	"cmp"
	"context"
	"encoding/binary"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// types is the flow-based type inference of one ECMAScript or Python unit:
// the types a site's receiver may hold, propagated from what the worker
// recovered inside each function (wire.Type) through assignments, parameters
// and returns across the unit's functions and files, and the function values
// field-based resolution reads.
//
// The inference is an inclusion-based propagation of abstract values over
// four kinds of variable: a callable's parameter, a site's result, a field
// (every field of one name, field-based), and the constants the worker
// recovered (an instance of a type declaration, a function declaration, a
// module). Binding a site and propagating values are one fixed point,
// because a site's targets depend on the values its receiver holds and a
// bound target connects the site's arguments to its parameters and its
// returns to the site's result:
//
//   - a bare site binds the in-file candidates its callee names that the
//     call's scope can see, those of the innermost scope declaring the name
//     (Table.Visible); a candidate that is a type declaration is a
//     construction;
//   - a constructor site (`new C()`) constructs every type its callee names;
//   - a construction binds the constructor the class declares or inherits
//     (`constructor`, Python's `__init__`), otherwise the class itself, and
//     its result holds an instance of exactly that class;
//   - a static site binds the method its qualifier's type declares or
//     inherits, and through a module receiver the module's export;
//   - a member site binds, for each instance its receiver holds, the method
//     the instance's type declares or inherits (DetailFlow) and, unless the
//     instance is a construction's result, every override in a subtype
//     (hierarchy.dispatch, DetailHierarchy); through a `super`
//     receiver or a class object the method the type declares or inherits,
//     and through a module the module's export (DetailDirect); and, only when
//     its receiver binds nothing once the inference settles, every function
//     written to the field it calls (DetailField);
//   - an indirect site binds every function its called value holds
//     (DetailFlow, or DetailField when the value is a field's).
//
// A module's exports are its exported declarations, the names its own
// `export` statements give them, and what it re-exports from other modules
// (`export { x } from`, `export *`, `export * as ns from`), each name
// resolved as ECMA-262 ResolveExport does (hierarchy.resolveExport).
//
// No binding joins on the callee name alone: the name is looked up only in
// the declarations its scope, its import or its receiver's type selects.
//
// Every relation is a sorted run (pagination.ExternalSort) and every join
// between relations an ordered merge of two runs, iterated semi-naively
// until a round adds no value: the least fixed point, with no fixed round
// count. The class hierarchy and the modules' exports are read by point
// lookups into the hierarchy's index. Nothing the size of the unit is
// resident beyond one merge group.
type types struct {
	// bindingRun is every binding the fixed point found, sorted by site and
	// target, one per pair with its best technique.
	bindingRun *pagination.SortedRun[binding]
	// recvRun is every value a site's receiver or called value held, sorted
	// by site, which functions answers from.
	recvRun *pagination.SortedRun[recv]
	cursor  *seeker[recv]
}

// inferTypes runs the inference over the unit's tables.
func inferTypes(ctx context.Context, u *unit, h *hierarchy) (*types, error) {
	in := &inference{u: u, h: h}
	t, err := in.run(ctx)
	in.closeAll()
	return t, err
}

// functions yields the function declarations the called value of site may
// hold: a function value that flowed to it, or one written to the field it
// calls through, each once with the technique that found it. Sites asked in
// ascending order are answered by one forward pass.
func (t *types) functions(ctx context.Context, site Ref, yield func(target Ref, detail string) error) error {
	return t.seek(site, func(r recv) error {
		if r.X.Kind != valFunction {
			return nil
		}
		switch r.Role {
		case roleCalled:
			return yield(r.X.Ref, DetailFlow)
		case roleField:
			return yield(r.X.Ref, DetailField)
		}
		return nil
	})
}

// bindings yields every binding the inference found, sorted by site and
// target, each pair once with the technique that found it first in rank.
func (t *types) bindings(yield func(Binding) error) error {
	return t.bindingRun.Each(func(b binding) error {
		return yield(Binding{Site: b.Site, Target: b.Target, Detail: rankDetail[b.Rank]})
	})
}

// seek yields the receiver values recorded for site, each once.
func (t *types) seek(site Ref, yield func(recv) error) error {
	if t.cursor == nil {
		t.cursor = newSeeker(t.recvRun, func(r recv) Ref { return r.Call.Site })
	}
	var last recv
	first := true
	return t.cursor.each(site, func(r recv) error {
		if !first && r.Role == last.Role && cmpValue(r.X, last.X) == 0 {
			return nil
		}
		first, last = false, r
		return yield(r)
	})
}

// close releases what the inference spilled.
func (t *types) close() error {
	if t == nil {
		return nil
	}
	if t.cursor != nil {
		t.cursor.stop()
	}
	t.bindingRun.Close()
	return t.recvRun.Close()
}

// ---- the records ----

// The kinds of variable values flow through.
const (
	varParam  byte = 1 // parameter Index of the callable Decl in File
	varResult byte = 2 // the value site Index of File returns
	varField  byte = 3 // every field or property called Name
	// varImport is a name an import binds: Name is the module path and
	// the name the module declares, joined by a NUL. The exports join
	// seeds it with the declaration the module exports under that name.
	varImport byte = 4
)

// variable is one place values flow into.
type variable struct {
	Kind              byte
	File, Decl, Index int32
	Name              string
}

// The kinds of abstract value.
const (
	valInstance byte = 1 // an instance of the type declaration Ref, or of a subtype
	valExact    byte = 2 // the type declaration Ref itself as a `super` receiver
	valFunction byte = 3 // the function or method declaration Ref
	valModule   byte = 4 // the module whose path, without extension, is Path
	// valClass is the class object of the type declaration Ref: a call on it
	// binds what the type declares or inherits, and in Python calls an
	// instance method unbound, its first argument the instance.
	valClass byte = 5
	// valObject is an instance of exactly the type declaration Ref, what a
	// construction of it returns: a call on it binds what the type declares
	// or inherits, and no subtype's override.
	valObject byte = 6
)

// value is one abstract value.
type value struct {
	Kind byte
	Ref  Ref
	Path string
}

// operand is an evaluated wire.Type: a variable, or a constant value.
type operand struct {
	IsVar bool
	V     variable
	X     value
}

// fact is one value a variable may hold.
type fact struct {
	V variable
	X value
}

// edge says that To holds every value From holds.
type edge struct{ From, To variable }

// call is the site a value or a binding belongs to.
type call struct {
	Site   Ref
	Callee string
}

// The roles a value plays at a site.
const (
	roleReceiver  byte = 1 // the receiver of a member or static site
	roleCalled    byte = 2 // the called value of an indirect site
	roleField     byte = 3 // a function written to the field a site calls
	roleConstruct byte = 4 // a type a construction creates
)

// watch says that the values V holds play Role at Call.
type watch struct {
	V    variable
	Role byte
	Call call
}

// recv is one value playing Role at Call.
type recv struct {
	Call call
	Role byte
	X    value
}

// The techniques in the order a pair found by several keeps the first.
const (
	rankDirect byte = iota
	rankFlow
	rankHierarchy
	rankField
)

var rankDetail = [...]string{rankDirect: DetailDirect, rankFlow: DetailFlow, rankHierarchy: DetailHierarchy, rankField: DetailField}

// binding is one target found for a site. Flow reports that the site's
// arguments and result connect to the target's parameters and returns (a
// class bound for want of a constructor does not); Shift that the site's
// first argument is the target's receiver (a Python instance method called
// through its class), so argument i+1 is parameter i.
type binding struct {
	Site, Target Ref
	Rank         byte
	Flow, Shift  bool
}

// lookup asks for the method Name the type Class declares, or else inherits
// (hierarchy.declared: the nearest along each supertype path, Python's
// method resolution order for a Python class). Unbound reports a Python call
// through the class object, which passes an instance method its receiver as
// the first argument.
type lookup struct {
	Class   Ref
	Name    string
	Call    call
	Rank    byte
	Unbound bool
}

// exportReq asks for the export Name of the module Base for Call.
type exportReq struct {
	Base, Name string
	Call       call
}

// construct is the creation of an instance of Class at Call; Ctor is the
// constructor's name in the class's language.
type construct struct {
	Class Ref
	Ctor  string
	Call  call
}

// declRec is one value a callable returns.
type declRec struct {
	Decl Ref
	Ret  operand
}

// argRec is one value argument Arg of Site may pass.
type argRec struct {
	Site Ref
	Arg  int32
	Op   operand
}

// ---- the inference ----

type inference struct {
	u *unit
	h *hierarchy
	// python reports a Python unit. A unit is one language family, and
	// Python's constructor is `__init__`.
	python bool

	// The relations the file pass fills and the rounds read.
	watches *pagination.SortedRun[watch]
	decls   *pagination.SortedRun[declRec]
	args    *pagination.SortedRun[argRec]

	facts *pagination.SortedRun[fact]
	// delta is the facts the last round added.
	delta   *pagination.SortedRun[fact]
	edges   *pagination.SortedRun[edge]
	bound   *pagination.SortedRun[binding]
	recvLog *pagination.ExternalSort[recv]

	// Field-based resolution of a member site waits for the fixed point:
	// fieldWatches are the member sites' watches of the fields they call,
	// and fielded reports that the watches of the sites their receivers
	// bound nothing for have been added.
	fieldWatches *pagination.ExternalSort[watch]
	fielded      bool

	open []interface{ Close() error }
}

// round is what one round of the fixed point collects before it joins.
type round struct {
	bindings   *pagination.ExternalSort[binding]
	lookups    *pagination.ExternalSort[lookup]
	exportReqs *pagination.ExternalSort[exportReq]
	// imports are the imported names the round's file scan evaluated,
	// whose variables the exports join seeds.
	imports    *pagination.ExternalSort[exportReq]
	constructs *pagination.ExternalSort[construct]
	facts      *pagination.ExternalSort[fact]
	edges      *pagination.ExternalSort[edge]
}

func (in *inference) run(ctx context.Context) (*types, error) {
	var err error
	if in.recvLog, err = newSort(in, recvCodec); err != nil {
		return nil, err
	}
	in.recvLog.WithFold(keepFirst[recv])
	if in.fieldWatches, err = newSort(in, watchByCall); err != nil {
		return nil, err
	}
	in.fieldWatches.WithFold(keepFirst[watch])
	r, err := in.newRound()
	if err != nil {
		return nil, err
	}
	if err := in.scan(ctx, r); err != nil {
		return nil, err
	}
	if in.facts, err = emptyRun(in, factCodec); err != nil {
		return nil, err
	}
	in.delta = in.facts
	if in.edges, err = emptyRun(in, edgeCodec); err != nil {
		return nil, err
	}
	if in.bound, err = emptyRun(in, bindingCodec); err != nil {
		return nil, err
	}
	mark := len(in.open)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prev := []interface{ Close() error }{in.facts, in.delta, in.edges, in.bound}
		delta, err := in.step(ctx, r)
		if err != nil {
			return nil, err
		}
		// What the round superseded is released now, so the spill holds the
		// current relations and nothing of earlier rounds.
		in.release(mark, prev, in.facts, in.delta, in.edges, in.bound, in.watches)
		if delta.Len() == 0 && in.fielded {
			break
		}
		mark = len(in.open)
		if r, err = in.newRound(); err != nil {
			return nil, err
		}
		if delta.Len() == 0 {
			// The values have settled: the member sites whose receivers
			// bound no method now read the functions written to the fields
			// they call, and the fixed point continues from there.
			if err := in.addFieldWatches(ctx, r); err != nil {
				return nil, err
			}
			continue
		}
		// The values new this round reach the sites that watch their
		// variables.
		err = join(delta, in.watches, func(f fact, w watch) int { return cmpVar(f.V, w.V) },
			func(f fact, ws []watch) error {
				for _, w := range ws {
					if err := in.receive(ctx, r, recv{Call: w.Call, Role: w.Role, X: f.X}); err != nil {
						return err
					}
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	recvs, err := in.recvLog.Sorted()
	if err != nil {
		return nil, err
	}
	// The answer's runs belong to the types the inference returns.
	in.open = slices.DeleteFunc(in.open, func(c interface{ Close() error }) bool { return c == in.bound })
	return &types{bindingRun: in.bound, recvRun: recvs}, nil
}

// addFieldWatches adds the field watch of every member site that has no
// binding once the values settle, and hands each the values its field
// already holds. Until then a member site is bound only through its
// receiver, so a site with no binding is one whose receiver's values, if
// any reached it, declare or inherit no method of its name: an instance
// attribute (`self.cb = f; self.cb()`) that only the field can tell.
func (in *inference) addFieldWatches(ctx context.Context, r *round) error {
	in.fielded = true
	pending, err := sorted(in, in.fieldWatches)
	if err != nil {
		return err
	}
	untyped, err := newSort(in, watchCodec)
	if err != nil {
		return err
	}
	err = join(pending, in.bound, func(w watch, b binding) int { return compareRef(w.Call.Site, b.Site) }, func(w watch, bs []binding) error {
		if len(bs) > 0 {
			return nil
		}
		return untyped.Add(w)
	})
	if err != nil {
		return err
	}
	added, err := sorted(in, untyped)
	if err != nil {
		return err
	}
	err = join(in.facts, added, func(f fact, w watch) int { return cmpVar(f.V, w.V) }, func(f fact, ws []watch) error {
		for _, w := range ws {
			if err := in.receive(ctx, r, recv{Call: w.Call, Role: w.Role, X: f.X}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	in.watches, err = unionRuns(in, watchCodec, in.watches, added)
	return err
}

// release closes every relation opened since mark except live, which stays
// open and registered, and closes each of prev that is not live.
func (in *inference) release(mark int, prev []interface{ Close() error }, live ...interface{ Close() error }) {
	isLive := func(c interface{ Close() error }) bool {
		for _, l := range live {
			if l == c {
				return true
			}
		}
		return false
	}
	kept := in.open[:mark]
	for _, c := range in.open[mark:] {
		if isLive(c) {
			kept = append(kept, c)
		} else {
			c.Close()
		}
	}
	in.open = kept
	for _, c := range prev {
		if !isLive(c) {
			c.Close()
		}
	}
}

// closeAll releases every relation the inference opened and still holds.
func (in *inference) closeAll() {
	for _, c := range in.open {
		c.Close()
	}
	in.open = nil
}

// keep registers c to be released when the inference ends.
func (in *inference) keep(c interface{ Close() error }) { in.open = append(in.open, c) }

func (in *inference) newRound() (*round, error) {
	var r round
	var err error
	if r.bindings, err = newSort(in, bindingCodec); err != nil {
		return nil, err
	}
	r.bindings.WithFold(bestBinding)
	if r.lookups, err = newSort(in, lookupCodec); err != nil {
		return nil, err
	}
	if r.exportReqs, err = newSort(in, exportReqCodec); err != nil {
		return nil, err
	}
	if r.imports, err = newSort(in, exportReqCodec); err != nil {
		return nil, err
	}
	r.imports.WithFold(keepFirst[exportReq])
	if r.constructs, err = newSort(in, constructCodec); err != nil {
		return nil, err
	}
	if r.facts, err = newSort(in, factCodec); err != nil {
		return nil, err
	}
	r.facts.WithFold(keepFirst[fact])
	if r.edges, err = newSort(in, edgeCodec); err != nil {
		return nil, err
	}
	r.edges.WithFold(keepFirst[edge])
	return &r, nil
}

// scan reads every member table once: the relations the rounds join, the
// values and bindings each site's own table already decides, and the
// watches that bind the rest as values arrive.
func (in *inference) scan(ctx context.Context, r *round) error {
	watches, err := newSort(in, watchCodec)
	if err != nil {
		return err
	}
	watches.WithFold(keepFirst[watch])
	decls, err := newSort(in, declRecCodec)
	if err != nil {
		return err
	}
	args, err := newSort(in, argRecCodec)
	if err != nil {
		return err
	}
	err = in.u.Files(ctx, func(file int32, _ model.FileVersion, t *Table) error {
		f := &fileScan{in: in, r: r, file: file, t: t, python: t.Language == "python"}
		in.python = in.python || f.python
		return f.scan(ctx, r, watches, decls, args)
	})
	if err != nil {
		return err
	}
	if in.watches, err = sorted(in, watches); err != nil {
		return err
	}
	if in.decls, err = sorted(in, decls); err != nil {
		return err
	}
	in.args, err = sorted(in, args)
	return err
}

// fileScan reads one member table.
type fileScan struct {
	in     *inference
	r      *round
	file   int32
	t      *Table
	python bool
	// callables indexes the file's callable declarations by name, built
	// when a function value is first evaluated.
	callables map[string][]int32
}

func (f *fileScan) scan(ctx context.Context, r *round, watches *pagination.ExternalSort[watch],
	decls *pagination.ExternalSort[declRec], args *pagination.ExternalSort[argRec],
) error {
	t := f.t
	for i := range t.Decls {
		d := &t.Decls[i]
		ref := Ref{File: f.file, Index: int32(i)}
		if !callable(d) {
			continue
		}
		err := f.eval(ctx, int32(i), d.Returns, func(op operand) error {
			return decls.Add(declRec{Decl: ref, Ret: op})
		})
		if err != nil {
			return err
		}
	}
	for i := range t.Sites {
		if err := f.site(ctx, r, watches, args, int32(i)); err != nil {
			return err
		}
	}
	for _, v := range t.Values {
		if v.Field == "" {
			continue
		}
		dst := variable{Kind: varField, Name: v.Field}
		err := f.eval(ctx, v.Caller, v.Type, func(op operand) error {
			if op.IsVar {
				return r.edges.Add(edge{From: op.V, To: dst})
			}
			return r.facts.Add(fact{V: dst, X: op.X})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// site records one site: its arguments, the bindings and values its own
// table decides, and the watches for the values that arrive later.
func (f *fileScan) site(ctx context.Context, r *round, watches *pagination.ExternalSort[watch],
	args *pagination.ExternalSort[argRec], i int32,
) error {
	t, in := f.t, f.in
	s := &t.Sites[i]
	ref := Ref{File: f.file, Index: i}
	c := call{Site: ref, Callee: s.Callee}
	for j, a := range s.Args {
		err := f.eval(ctx, s.Caller, a, func(op operand) error { return args.Add(argRec{Site: ref, Arg: int32(j), Op: op}) })
		if err != nil {
			return err
		}
	}
	// route sends each value of the site's receiver term to role, now for a
	// constant and through a watch for a variable.
	route := func(role byte) error {
		return f.eval(ctx, s.Caller, s.Receiver, func(op operand) error {
			if op.IsVar {
				return watches.Add(watch{V: op.V, Role: role, Call: c})
			}
			return in.receive(ctx, r, recv{Call: c, Role: role, X: op.X})
		})
	}
	switch s.Shape {
	case wire.ShapeBare:
		// A bare call of an imported name carries the module it was
		// imported from and the name the module declares it by, which the
		// local name an import may rename it to is not.
		for _, m := range f.members(s.Receiver) {
			if m.Kind != wire.TypeModule {
				continue
			}
			spec, name, ok := importedName(m.Name, f.python)
			if base, found := moduleBase(t.Path, spec, f.python); ok && found {
				if err := r.exportReqs.Add(exportReq{Base: base, Name: name, Call: c}); err != nil {
					return err
				}
			}
		}
		for _, k := range scoped(t, s.Caller, s.Candidates) {
			d := &t.Decls[k]
			target := Ref{File: f.file, Index: k}
			switch {
			case isType(d.Kind):
				if err := in.receive(ctx, r, recv{Call: c, Role: roleConstruct, X: value{Kind: valInstance, Ref: target}}); err != nil {
					return err
				}
			case isCallable(d.Kind):
				if err := r.bindings.Add(binding{Site: ref, Target: target, Rank: rankDirect, Flow: true}); err != nil {
					return err
				}
			}
		}
	case wire.ShapeConstructor:
		construct := func(target Ref) error {
			return in.receive(ctx, r, recv{Call: c, Role: roleConstruct, X: value{Kind: valInstance, Ref: target}})
		}
		for _, k := range scoped(t, s.Caller, s.Candidates) {
			if isType(t.Decls[k].Kind) {
				if err := construct(Ref{File: f.file, Index: k}); err != nil {
					return err
				}
			}
		}
		if err := in.h.types(ctx, f.file, s.Callee, construct); err != nil {
			return err
		}
	case wire.ShapeStatic:
		if s.Qualifier != "" {
			err := in.h.types(ctx, f.file, s.Qualifier, func(class Ref) error {
				return r.lookups.Add(lookup{Class: class, Name: s.Callee, Call: c, Rank: rankDirect, Unbound: f.python})
			})
			if err != nil {
				return err
			}
		}
		if slices.ContainsFunc(f.members(s.Receiver), func(m wire.Type) bool { return m.Kind == wire.TypeModule }) {
			return route(roleReceiver)
		}
	case wire.ShapeMember:
		if err := route(roleReceiver); err != nil {
			return err
		}
		// Field-based resolution waits until the values settle, and binds
		// only when the receiver bound nothing (addFieldWatches).
		return in.fieldWatches.Add(watch{V: variable{Kind: varField, Name: s.Callee}, Role: roleField, Call: c})
	case wire.ShapeIndirect:
		role := roleCalled
		if s.Receiver.Kind == wire.TypeField {
			role = roleField
		}
		return route(role)
	}
	return nil
}

// scoped is the candidates of a bare name a call in caller makes that its
// scope binds the name to: those Table.Visible admits, declared in the
// innermost scope enclosing caller that declares any (a nested function
// shadows a module-level one of its name).
func scoped(t *Table, caller int32, candidates []int32) []int32 {
	var out []int32
	best := -1
	for _, k := range candidates {
		if !t.Visible(k, caller) {
			continue
		}
		depth := 0
		for i := caller; i >= 0 && int(i) < len(t.Decls) && i != t.Decls[k].Parent; i = t.Decls[i].Parent {
			depth++
		}
		switch {
		case best < 0 || depth < best:
			best, out = depth, append(out[:0], k)
		case depth == best:
			out = append(out, k)
		}
	}
	return out
}

// members is the types ty stands for: a union's members, else ty.
func (f *fileScan) members(ty wire.Type) []wire.Type {
	if ty.Kind != wire.TypeUnion {
		return []wire.Type{ty}
	}
	if int(ty.Index) >= len(f.t.Unions) {
		return nil
	}
	return f.t.Unions[ty.Index]
}

// eval evaluates ty, written in the callable caller of the scanned file (-1
// for the module), to the operands it denotes.
func (f *fileScan) eval(ctx context.Context, caller int32, ty wire.Type, yield func(operand) error) error {
	t := f.t
	constant := func(kind byte) func(Ref) error {
		return func(r Ref) error { return yield(operand{X: value{Kind: kind, Ref: r}}) }
	}
	switch ty.Kind {
	case wire.TypeUnion:
		for _, m := range f.members(ty) {
			if m.Kind == wire.TypeUnion {
				continue
			}
			if err := f.eval(ctx, caller, m, yield); err != nil {
				return err
			}
		}
	case wire.TypeNamed:
		return f.in.h.types(ctx, f.file, ty.Name, constant(valInstance))
	case wire.TypeClass:
		return f.in.h.types(ctx, f.file, ty.Name, constant(valClass))
	case wire.TypeSuper:
		for _, st := range t.Supertypes {
			if st.Type == ty.Name {
				if err := f.in.h.types(ctx, f.file, st.Super, constant(valExact)); err != nil {
					return err
				}
			}
		}
	case wire.TypeParam:
		if caller >= 0 && int(caller) < len(t.Decls) {
			return yield(operand{IsVar: true, V: variable{Kind: varParam, File: f.file, Decl: caller, Index: int32(ty.Index)}})
		}
	case wire.TypeResult:
		if int(ty.Index) < len(t.Sites) {
			return yield(operand{IsVar: true, V: variable{Kind: varResult, File: f.file, Index: int32(ty.Index)}})
		}
	case wire.TypeField:
		return yield(operand{IsVar: true, V: variable{Kind: varField, Name: ty.Name}})
	case wire.TypeModule:
		// An imported name (ECMAScript `spec#name`, Python `module.name`)
		// denotes the declaration its module exports under that name. A
		// Python dotted name may instead be a module (`import a.b`), so it
		// denotes both; the one no member file declares joins nothing.
		if spec, name, ok := importedName(ty.Name, f.python); ok {
			if base, found := moduleBase(t.Path, spec, f.python); found {
				if err := f.r.imports.Add(exportReq{Base: base, Name: name}); err != nil {
					return err
				}
				if err := yield(operand{IsVar: true, V: importVar(base, name)}); err != nil {
					return err
				}
			}
			if !f.python {
				return nil
			}
		}
		if base, ok := moduleBase(t.Path, ty.Name, f.python); ok {
			return yield(operand{X: value{Kind: valModule, Path: base}})
		}
	case wire.TypeFunction:
		return f.function(ctx, caller, ty.Name, constant(valFunction))
	}
	return nil
}

// function yields the callable declarations the function value written as
// name in caller denotes. A bare name is read as a call's bare callee is
// (scoped), so it never denotes a method. A member name `C.m` or `o.m` is
// the method m that the class or object C declares in the file; failing
// one, the method m the type C names declares or inherits.
func (f *fileScan) function(ctx context.Context, caller int32, name string, yield func(Ref) error) error {
	t := f.t
	if f.callables == nil {
		f.callables = make(map[string][]int32)
		for i := range t.Decls {
			if d := &t.Decls[i]; callable(d) {
				f.callables[d.Name] = append(f.callables[d.Name], int32(i))
			}
		}
	}
	holder, member, dotted := cutLast(name, ".")
	if !dotted {
		for _, i := range scoped(t, caller, f.callables[name]) {
			if err := yield(Ref{File: f.file, Index: i}); err != nil {
				return err
			}
		}
		return nil
	}
	found := false
	for _, i := range f.callables[member] {
		p := t.Decls[i].Parent
		if p < 0 || int(p) >= len(t.Decls) || t.Decls[p].Name != holder && t.Decls[p].Qualified != holder {
			continue
		}
		found = true
		if err := yield(Ref{File: f.file, Index: i}); err != nil {
			return err
		}
	}
	if found {
		return nil
	}
	return f.in.h.types(ctx, f.file, holder, func(typ Ref) error {
		return f.in.h.declared(typ, member, func(e entry) error { return yield(e.Ref) })
	})
}

// receive turns one value playing a role at a site into what the round
// joins: a binding, a method lookup, an export request or a construction.
func (in *inference) receive(ctx context.Context, r *round, v recv) error {
	if err := in.recvLog.Add(v); err != nil {
		return err
	}
	c := v.Call
	switch v.Role {
	case roleReceiver:
		switch v.X.Kind {
		case valInstance:
			if err := r.lookups.Add(lookup{Class: v.X.Ref, Name: c.Callee, Call: c, Rank: rankFlow}); err != nil {
				return err
			}
			return in.h.dispatch(ctx, v.X.Ref, c.Callee, func(m Ref) error {
				return r.bindings.Add(binding{Site: c.Site, Target: m, Rank: rankHierarchy, Flow: true})
			})
		case valObject:
			return r.lookups.Add(lookup{Class: v.X.Ref, Name: c.Callee, Call: c, Rank: rankFlow})
		case valExact:
			return r.lookups.Add(lookup{Class: v.X.Ref, Name: c.Callee, Call: c, Rank: rankDirect})
		case valClass:
			return r.lookups.Add(lookup{Class: v.X.Ref, Name: c.Callee, Call: c, Rank: rankDirect, Unbound: in.python})
		case valModule:
			return r.exportReqs.Add(exportReq{Base: v.X.Path, Name: c.Callee, Call: c})
		}
	case roleCalled, roleField:
		if v.X.Kind == valClass && in.python {
			// Calling a Python class object constructs an instance.
			return r.constructs.Add(construct{Class: v.X.Ref, Ctor: "__init__", Call: c})
		}
		if v.X.Kind == valFunction {
			rank := rankFlow
			if v.Role == roleField {
				rank = rankField
			}
			return r.bindings.Add(binding{Site: c.Site, Target: v.X.Ref, Rank: rank, Flow: true})
		}
	case roleConstruct:
		if v.X.Kind == valInstance {
			ctor := "constructor"
			if in.python {
				ctor = "__init__"
			}
			return r.constructs.Add(construct{Class: v.X.Ref, Ctor: ctor, Call: c})
		}
	}
	return nil
}

// step joins one round: export requests to what the modules export,
// constructions to constructors and lookups to methods (both read from the
// class hierarchy, in method resolution order), the round's bindings against
// every earlier one, the new bindings' arguments and returns into edges and
// values, and the values through the edges. It answers the values new this
// round.
func (in *inference) step(ctx context.Context, r *round) (*pagination.SortedRun[fact], error) {
	reqs, err := sorted(in, r.exportReqs)
	if err != nil {
		return nil, err
	}
	err = reqs.Each(func(q exportReq) error {
		return in.h.moduleExport(q.Base, q.Name, func(e entry) error {
			switch e.Aux {
			case exportType:
				return in.receive(ctx, r, recv{Call: q.Call, Role: roleConstruct, X: value{Kind: valInstance, Ref: e.Ref}})
			case exportFunc:
				return r.bindings.Add(binding{Site: q.Call.Site, Target: e.Ref, Rank: rankDirect, Flow: true})
			}
			// A module is no callable.
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if err := in.seedImports(r); err != nil {
		return nil, err
	}
	cons, err := sorted(in, r.constructs)
	if err != nil {
		return nil, err
	}
	// A class without a constructor of its own runs the one it inherits
	// (an ECMAScript class's implicit constructor passes its arguments to
	// its superclass's; Python looks `__init__` up the bases), and one
	// that inherits none either is bound as the class itself.
	err = cons.Each(func(c construct) error {
		if err := r.facts.Add(fact{V: variable{Kind: varResult, File: c.Call.Site.File, Index: c.Call.Site.Index}, X: value{Kind: valObject, Ref: c.Class}}); err != nil {
			return err
		}
		found := false
		err := in.h.declared(c.Class, c.Ctor, func(e entry) error {
			found = true
			return r.bindings.Add(binding{Site: c.Call.Site, Target: e.Ref, Rank: rankDirect, Flow: true})
		})
		if err != nil || found {
			return err
		}
		return r.bindings.Add(binding{Site: c.Call.Site, Target: c.Class, Rank: rankDirect})
	})
	if err != nil {
		return nil, err
	}
	if err := in.resolveLookups(r); err != nil {
		return nil, err
	}
	fresh, err := in.mergeBindings(r)
	if err != nil {
		return nil, err
	}
	if err := in.flow(r, fresh); err != nil {
		return nil, err
	}
	return in.propagate(r)
}

// seedImports gives each imported name's variable the declaration its
// module exports under that name: a function value for a callable, for a
// type its class object, so a call through it (`Foo.bar()`) binds the
// method the type declares or inherits, and for a module re-exported as a
// namespace (`export * as ns`) that module.
func (in *inference) seedImports(r *round) error {
	names, err := sorted(in, r.imports)
	if err != nil {
		return err
	}
	return names.Each(func(q exportReq) error {
		return in.h.moduleExport(q.Base, q.Name, func(e entry) error {
			x := value{Kind: valFunction, Ref: e.Ref}
			switch e.Aux {
			case exportModule:
				x = value{Kind: valModule, Path: e.Sig}
			case exportType:
				x.Kind = valClass
			}
			return r.facts.Add(fact{V: importVar(q.Base, q.Name), X: x})
		})
	})
}

// importVar is the variable of the name a module path exports.
func importVar(base, name string) variable {
	return variable{Kind: varImport, Name: base + "\x00" + name}
}

// resolveLookups answers each lookup by the method its type declares or
// inherits (hierarchy.declared).
func (in *inference) resolveLookups(r *round) error {
	ls, err := sorted(in, r.lookups)
	if err != nil {
		return err
	}
	return ls.Each(func(l lookup) error {
		return in.h.declared(l.Class, l.Name, func(e entry) error {
			b := binding{Site: l.Call.Site, Target: e.Ref, Rank: l.Rank, Flow: true, Shift: l.Unbound && e.Flags&flagStatic == 0}
			return r.bindings.Add(b)
		})
	})
}

// mergeBindings folds the round's bindings into every earlier one and
// answers the pairs no earlier round found.
func (in *inference) mergeBindings(r *round) (*pagination.SortedRun[binding], error) {
	now, err := sorted(in, r.bindings)
	if err != nil {
		return nil, err
	}
	fresh, err := newSort(in, bindingCodec)
	if err != nil {
		return nil, err
	}
	err = join(now, in.bound, bindingCodec.cmp, func(b binding, old []binding) error {
		if len(old) == 0 {
			return fresh.Add(b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	all, err := newSort(in, bindingCodec)
	if err != nil {
		return nil, err
	}
	all.WithFold(bestBinding)
	if err := addAll(all, in.bound); err != nil {
		return nil, err
	}
	if err := addAll(all, now); err != nil {
		return nil, err
	}
	if in.bound, err = sorted(in, all); err != nil {
		return nil, err
	}
	return sorted(in, fresh)
}

// flow connects each new binding's site to its target: the target's
// returns to the site's result, and argument i to parameter i. Every
// lowering counts parameters from the first one after an implicit receiver
// (`this`, Python's `self` and `cls`), so a call's arguments and its
// target's parameters line up for every shape but one: a Python instance
// method called through its class takes its receiver as the first
// argument, so its argument i+1 is parameter i (Shift).
func (in *inference) flow(r *round, fresh *pagination.SortedRun[binding]) error {
	byTarget, err := newSort(in, codec[binding]{put: bindingCodec.put, get: bindingCodec.get,
		cmp: func(a, b binding) int { return cmp.Or(compareRef(a.Target, b.Target), compareRef(a.Site, b.Site)) }})
	if err != nil {
		return err
	}
	err = fresh.Each(func(b binding) error {
		if !b.Flow {
			return nil
		}
		return byTarget.Add(b)
	})
	if err != nil {
		return err
	}
	targets, err := sorted(in, byTarget)
	if err != nil {
		return err
	}
	err = join(targets, in.decls, func(b binding, d declRec) int { return compareRef(b.Target, d.Decl) },
		func(b binding, ds []declRec) error {
			result := variable{Kind: varResult, File: b.Site.File, Index: b.Site.Index}
			for _, d := range ds {
				if err := r.into(result, d.Ret); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return err
	}
	// fresh is sorted by site, the order the arguments are.
	return join(fresh, in.args, func(b binding, a argRec) int { return compareRef(b.Site, a.Site) },
		func(b binding, as []argRec) error {
			if !b.Flow {
				return nil
			}
			for _, a := range as {
				i := a.Arg
				if b.Shift {
					if i == 0 {
						continue
					}
					i--
				}
				p := variable{Kind: varParam, File: b.Target.File, Decl: b.Target.Index, Index: i}
				if err := r.into(p, a.Op); err != nil {
					return err
				}
			}
			return nil
		})
}

// into records that dst holds what op denotes.
func (r *round) into(dst variable, op operand) error {
	if op.IsVar {
		return r.edges.Add(edge{From: op.V, To: dst})
	}
	return r.facts.Add(fact{V: dst, X: op.X})
}

// propagate is one semi-naive step: the round's constant values, the
// previous round's new values through every earlier edge, and every value
// through the round's new edges; it keeps what was not known and answers it.
func (in *inference) propagate(r *round) (*pagination.SortedRun[fact], error) {
	newEdges, err := sorted(in, r.edges)
	if err != nil {
		return nil, err
	}
	err = join(in.delta, in.edges, func(f fact, e edge) int { return cmpVar(f.V, e.From) },
		func(f fact, es []edge) error {
			for _, e := range es {
				if err := r.facts.Add(fact{V: e.To, X: f.X}); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = join(in.facts, newEdges, func(f fact, e edge) int { return cmpVar(f.V, e.From) },
		func(f fact, es []edge) error {
			for _, e := range es {
				if err := r.facts.Add(fact{V: e.To, X: f.X}); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	found, err := sorted(in, r.facts)
	if err != nil {
		return nil, err
	}
	delta, err := newSort(in, factCodec)
	if err != nil {
		return nil, err
	}
	err = join(found, in.facts, factCodec.cmp, func(f fact, old []fact) error {
		if len(old) == 0 {
			return delta.Add(f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	d, err := sorted(in, delta)
	if err != nil {
		return nil, err
	}
	facts, err := unionRuns(in, factCodec, in.facts, d)
	if err != nil {
		return nil, err
	}
	edges, err := unionRuns(in, edgeCodec, in.edges, newEdges)
	if err != nil {
		return nil, err
	}
	in.facts, in.edges, in.delta = facts, edges, d
	return d, nil
}

// ---- language rules ----

// ecmaScriptExtensions are the extensions an ECMAScript import may leave
// out, so a module's path is compared without them.
var ecmaScriptExtensions = []string{".d.ts", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".mts", ".cts", ".tsx"}

// moduleBases are the module paths, without extension, an import may name
// the member file p by: ECMAScript's file path and, for an index file, its
// directory; Python's dotted module path as a path, from every directory
// that may be a source root, and for a package's `__init__.py` the package.
func moduleBases(p string, python bool) []string {
	var base string
	if python {
		base = strings.TrimSuffix(strings.TrimSuffix(p, ".py"), ".pyi")
		if path.Base(base) == "__init__" {
			base = path.Dir(base)
		}
		var out []string
		for s := base; ; {
			out = append(out, s)
			i := strings.IndexByte(s, '/')
			if i < 0 {
				return out
			}
			s = s[i+1:]
		}
	}
	base = stripExtension(p)
	if path.Base(base) == "index" {
		return []string{base, path.Dir(base)}
	}
	return []string{base}
}

// moduleBase is the module path, without extension, the import spec written
// in the file at p denotes, and false for a spec no member file can be: an
// ECMAScript package specifier.
func moduleBase(p, spec string, python bool) (string, bool) {
	dir := path.Dir(p)
	if python {
		dots := len(spec) - len(strings.TrimLeft(spec, "."))
		rest := strings.ReplaceAll(spec[dots:], ".", "/")
		if dots == 0 {
			return rest, rest != ""
		}
		for range dots - 1 {
			dir = path.Dir(dir)
		}
		return path.Join(dir, rest), true
	}
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return "", false
	}
	return stripExtension(path.Join(dir, spec)), true
}

// importedName splits the module receiver of a bare call of an imported
// name into the module as written and the name the module declares: an
// ECMAScript `spec#name` (`import {name as local} from "spec"`), a Python
// `module.name` split at its last dot (`from module import name as local`,
// a relative module keeping its dots).
func importedName(written string, python bool) (spec, name string, ok bool) {
	sep := "#"
	if python {
		sep = "."
	}
	i := strings.LastIndex(written, sep)
	if i < 0 || i == len(written)-1 {
		return "", "", false
	}
	spec, name = written[:i], written[i+1:]
	if python && strings.Trim(spec, ".") == "" {
		// `from . import name`: the module is the package the dots name.
		spec = written[:i+1]
	}
	return spec, name, spec != ""
}

func stripExtension(p string) string {
	for _, ext := range ecmaScriptExtensions {
		if strings.HasSuffix(p, ext) {
			return strings.TrimSuffix(p, ext)
		}
	}
	return p
}

// ---- sorted runs and ordered merges ----

// codec is a record's binary form and its total order.
type codec[T any] struct {
	put func(*wbuf, T)
	get func(*rbuf) T
	cmp func(a, b T) int
}

func newSort[T any](in *inference, c codec[T]) (*pagination.ExternalSort[T], error) {
	s, err := pagination.NewExternalSort(in.u.dir, 0,
		func(v T) ([]byte, error) { var w wbuf; c.put(&w, v); return w.b, nil },
		func(b []byte) (T, error) {
			r := rbuf{b: b}
			v := c.get(&r)
			if r.bad || len(r.b) != 0 {
				var zero T
				return zero, &model.Error{Code: model.CodeInternal, Message: "link: a spilled inference record does not decode"}
			}
			return v, nil
		}, c.cmp)
	if err != nil {
		return nil, err
	}
	in.keep(s)
	return s, nil
}

// sorted ends s and keeps its run for release with the inference.
func sorted[T any](in *inference, s *pagination.ExternalSort[T]) (*pagination.SortedRun[T], error) {
	run, err := s.Sorted()
	if err != nil {
		return nil, err
	}
	in.keep(run)
	return run, nil
}

func emptyRun[T any](in *inference, c codec[T]) (*pagination.SortedRun[T], error) {
	s, err := newSort(in, c)
	if err != nil {
		return nil, err
	}
	return sorted(in, s)
}

// unionRuns is the sorted union of two runs, each record once.
func unionRuns[T any](in *inference, c codec[T], a, b *pagination.SortedRun[T]) (*pagination.SortedRun[T], error) {
	s, err := newSort(in, c)
	if err != nil {
		return nil, err
	}
	s.WithFold(keepFirst[T])
	if err := addAll(s, a); err != nil {
		return nil, err
	}
	if err := addAll(s, b); err != nil {
		return nil, err
	}
	return sorted(in, s)
}

func addAll[T any](s *pagination.ExternalSort[T], run *pagination.SortedRun[T]) error {
	return run.Each(s.Add)
}

func keepFirst[T any](a, _ T) (T, error) { return a, nil }

// bestBinding keeps the technique first in rank for one site and target.
func bestBinding(a, b binding) (binding, error) {
	if b.Rank < a.Rank {
		return b, nil
	}
	return a, nil
}

// join is an ordered merge of a and b, both sorted by the key order
// compare(a, b) reads: fn receives each record of a with the group of b
// records of an equal key, empty when none has it.
func join[A, B any](a *pagination.SortedRun[A], b *pagination.SortedRun[B], compare func(A, B) int,
	fn func(A, []B) error,
) error {
	next, stop := pull(b)
	defer stop()
	var group []B
	have := false
	head, haveHead, err := next()
	if err != nil {
		return err
	}
	return a.Each(func(x A) error {
		if have && len(group) > 0 && compare(x, group[0]) == 0 {
			return fn(x, group)
		}
		group, have = group[:0], true
		var err error
		for haveHead && compare(x, head) > 0 {
			if head, haveHead, err = next(); err != nil {
				return err
			}
		}
		for haveHead && compare(x, head) == 0 {
			group = append(group, head)
			if head, haveHead, err = next(); err != nil {
				return err
			}
		}
		return fn(x, group)
	})
}

// seeker answers the records of one key at a time from a run sorted by that
// key, in one forward pass while the keys asked do not descend: the records
// of the last key asked are held, so asking it again answers them again, and
// an earlier key restarts the pass.
type seeker[T any] struct {
	run      *pagination.SortedRun[T]
	key      func(T) Ref
	next     func() (T, bool, error)
	stopFn   func()
	head     T
	haveHead bool
	last     Ref
	group    []T
	started  bool
}

func newSeeker[T any](run *pagination.SortedRun[T], key func(T) Ref) *seeker[T] {
	return &seeker[T]{run: run, key: key}
}

func (s *seeker[T]) each(k Ref, yield func(T) error) error {
	var err error
	if !s.started || compareRef(k, s.last) < 0 {
		s.stop()
		s.next, s.stopFn = pull(s.run)
		if s.head, s.haveHead, err = s.next(); err != nil {
			return err
		}
		s.started = true
	} else if compareRef(k, s.last) == 0 {
		return yieldAll(s.group, yield)
	}
	s.last, s.group = k, s.group[:0]
	for s.haveHead && compareRef(s.key(s.head), k) < 0 {
		if s.head, s.haveHead, err = s.next(); err != nil {
			return err
		}
	}
	for s.haveHead && compareRef(s.key(s.head), k) == 0 {
		s.group = append(s.group, s.head)
		if s.head, s.haveHead, err = s.next(); err != nil {
			return err
		}
	}
	return yieldAll(s.group, yield)
}

func yieldAll[T any](vs []T, yield func(T) error) error {
	for _, v := range vs {
		if err := yield(v); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeker[T]) stop() {
	if s.stopFn != nil {
		s.stopFn()
		s.stopFn = nil
	}
	s.started = false
}

// ---- the binary forms ----

type wbuf struct{ b []byte }

func (w *wbuf) u8(v byte)   { w.b = append(w.b, v) }
func (w *wbuf) i32(v int32) { w.b = binary.BigEndian.AppendUint32(w.b, uint32(v)) }
func (w *wbuf) flag(v bool) {
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}
func (w *wbuf) str(s string) {
	w.b = binary.AppendUvarint(w.b, uint64(len(s)))
	w.b = append(w.b, s...)
}
func (w *wbuf) ref(r Ref) { w.i32(r.File); w.i32(r.Index) }
func (w *wbuf) variable(v variable) {
	w.u8(v.Kind)
	w.i32(v.File)
	w.i32(v.Decl)
	w.i32(v.Index)
	w.str(v.Name)
}
func (w *wbuf) value(v value) { w.u8(v.Kind); w.ref(v.Ref); w.str(v.Path) }
func (w *wbuf) operand(o operand) {
	w.flag(o.IsVar)
	if o.IsVar {
		w.variable(o.V)
	} else {
		w.value(o.X)
	}
}
func (w *wbuf) call(c call) { w.ref(c.Site); w.str(c.Callee) }

type rbuf struct {
	b   []byte
	bad bool
}

func (r *rbuf) u8() byte {
	if len(r.b) < 1 {
		r.bad = true
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}
func (r *rbuf) i32() int32 {
	if len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b))
	r.b = r.b[4:]
	return v
}
func (r *rbuf) flag() bool { return r.u8() == 1 }
func (r *rbuf) str() string {
	n, k := binary.Uvarint(r.b)
	if k <= 0 || uint64(len(r.b)-k) < n {
		r.bad = true
		return ""
	}
	s := string(r.b[k : k+int(n)])
	r.b = r.b[k+int(n):]
	return s
}
func (r *rbuf) ref() Ref { return Ref{File: r.i32(), Index: r.i32()} }
func (r *rbuf) variable() variable {
	return variable{Kind: r.u8(), File: r.i32(), Decl: r.i32(), Index: r.i32(), Name: r.str()}
}
func (r *rbuf) value() value { return value{Kind: r.u8(), Ref: r.ref(), Path: r.str()} }
func (r *rbuf) operand() operand {
	if r.flag() {
		return operand{IsVar: true, V: r.variable()}
	}
	return operand{X: r.value()}
}
func (r *rbuf) call() call { return call{Site: r.ref(), Callee: r.str()} }

func cmpVar(a, b variable) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.File, b.File), cmp.Compare(a.Decl, b.Decl),
		cmp.Compare(a.Index, b.Index), cmp.Compare(a.Name, b.Name))
}

func cmpValue(a, b value) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), compareRef(a.Ref, b.Ref), cmp.Compare(a.Path, b.Path))
}

func cmpOperand(a, b operand) int {
	if a.IsVar != b.IsVar {
		if a.IsVar {
			return 1
		}
		return -1
	}
	if a.IsVar {
		return cmpVar(a.V, b.V)
	}
	return cmpValue(a.X, b.X)
}

func cmpCall(a, b call) int {
	return cmp.Or(compareRef(a.Site, b.Site), cmp.Compare(a.Callee, b.Callee))
}

var factCodec = codec[fact]{
	put: func(w *wbuf, f fact) { w.variable(f.V); w.value(f.X) },
	get: func(r *rbuf) fact { return fact{V: r.variable(), X: r.value()} },
	cmp: func(a, b fact) int { return cmp.Or(cmpVar(a.V, b.V), cmpValue(a.X, b.X)) },
}

var edgeCodec = codec[edge]{
	put: func(w *wbuf, e edge) { w.variable(e.From); w.variable(e.To) },
	get: func(r *rbuf) edge { return edge{From: r.variable(), To: r.variable()} },
	cmp: func(a, b edge) int { return cmp.Or(cmpVar(a.From, b.From), cmpVar(a.To, b.To)) },
}

var watchCodec = codec[watch]{
	put: func(w *wbuf, x watch) { w.variable(x.V); w.u8(x.Role); w.call(x.Call) },
	get: func(r *rbuf) watch { return watch{V: r.variable(), Role: r.u8(), Call: r.call()} },
	cmp: func(a, b watch) int {
		return cmp.Or(cmpVar(a.V, b.V), cmpCall(a.Call, b.Call), cmp.Compare(a.Role, b.Role))
	},
}

// watchByCall is watchCodec ordered by the watching site first.
var watchByCall = codec[watch]{put: watchCodec.put, get: watchCodec.get,
	cmp: func(a, b watch) int {
		return cmp.Or(cmpCall(a.Call, b.Call), cmpVar(a.V, b.V), cmp.Compare(a.Role, b.Role))
	},
}

var recvCodec = codec[recv]{
	put: func(w *wbuf, x recv) { w.call(x.Call); w.u8(x.Role); w.value(x.X) },
	get: func(r *rbuf) recv { return recv{Call: r.call(), Role: r.u8(), X: r.value()} },
	cmp: func(a, b recv) int {
		return cmp.Or(cmpCall(a.Call, b.Call), cmp.Compare(a.Role, b.Role), cmpValue(a.X, b.X))
	},
}

var bindingCodec = codec[binding]{
	put: func(w *wbuf, b binding) {
		w.ref(b.Site)
		w.ref(b.Target)
		w.u8(b.Rank)
		w.flag(b.Flow)
		w.flag(b.Shift)
	},
	get: func(r *rbuf) binding {
		return binding{Site: r.ref(), Target: r.ref(), Rank: r.u8(), Flow: r.flag(), Shift: r.flag()}
	},
	cmp: func(a, b binding) int { return cmp.Or(compareRef(a.Site, b.Site), compareRef(a.Target, b.Target)) },
}

var lookupCodec = codec[lookup]{
	put: func(w *wbuf, l lookup) {
		w.ref(l.Class)
		w.str(l.Name)
		w.call(l.Call)
		w.u8(l.Rank)
		w.flag(l.Unbound)
	},
	get: func(r *rbuf) lookup {
		return lookup{Class: r.ref(), Name: r.str(), Call: r.call(), Rank: r.u8(), Unbound: r.flag()}
	},
	cmp: func(a, b lookup) int {
		return cmp.Or(compareRef(a.Class, b.Class), cmp.Compare(a.Name, b.Name), cmpCall(a.Call, b.Call),
			cmp.Compare(a.Rank, b.Rank), cmpBool(a.Unbound, b.Unbound))
	},
}

var exportReqCodec = codec[exportReq]{
	put: func(w *wbuf, q exportReq) { w.str(q.Base); w.str(q.Name); w.call(q.Call) },
	get: func(r *rbuf) exportReq { return exportReq{Base: r.str(), Name: r.str(), Call: r.call()} },
	cmp: func(a, b exportReq) int {
		return cmp.Or(cmp.Compare(a.Base, b.Base), cmp.Compare(a.Name, b.Name), cmpCall(a.Call, b.Call))
	},
}

var constructCodec = codec[construct]{
	put: func(w *wbuf, c construct) { w.ref(c.Class); w.str(c.Ctor); w.call(c.Call) },
	get: func(r *rbuf) construct { return construct{Class: r.ref(), Ctor: r.str(), Call: r.call()} },
	cmp: func(a, b construct) int {
		return cmp.Or(compareRef(a.Class, b.Class), cmp.Compare(a.Ctor, b.Ctor), cmpCall(a.Call, b.Call))
	},
}

var declRecCodec = codec[declRec]{
	put: func(w *wbuf, d declRec) { w.ref(d.Decl); w.operand(d.Ret) },
	get: func(r *rbuf) declRec {
		return declRec{Decl: r.ref(), Ret: r.operand()}
	},
	cmp: func(a, b declRec) int {
		return cmp.Or(compareRef(a.Decl, b.Decl), cmpOperand(a.Ret, b.Ret))
	},
}

var argRecCodec = codec[argRec]{
	put: func(w *wbuf, a argRec) { w.ref(a.Site); w.i32(a.Arg); w.operand(a.Op) },
	get: func(r *rbuf) argRec { return argRec{Site: r.ref(), Arg: r.i32(), Op: r.operand()} },
	cmp: func(a, b argRec) int {
		return cmp.Or(compareRef(a.Site, b.Site), cmp.Compare(a.Arg, b.Arg), cmpOperand(a.Op, b.Op))
	},
}
