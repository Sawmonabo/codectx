package link

import (
	"encoding/json"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// PendingInput is one request-input read the structural provider found but
// whose source another file decides, which the linker resolves through the
// unit's indexes and publishes (FactInput): a FastAPI handler parameter
// typed by a class the file imports, whose source follows that class
// (FastAPI's analyze_param: a model or a dataclass is a body, an UploadFile
// a file, a Request the request object, any other class a scalar), unless
// the route writes the parameter's name (a path read); or, under Rule
// RuleSpring, a Spring handler parameter no binding annotation marks, which
// reaches Spring's catch-all only when the project registers no argument
// resolver of its own (route_input_spring.go).
type PendingInput struct {
	// Owner is the declaration the read is made from, an index of Decls.
	Owner int32 `json:"owner"`
	// Range is the reading expression, the parameter as written, and Name
	// that expression as the read's node is named.
	Range model.SourceRange `json:"range"`
	Name  string            `json:"name"`
	// Framework names the framework whose rule decides the source; Param
	// is the parameter's name, the read's key.
	Framework string `json:"framework"`
	Param     string `json:"param"`
	// Path reports a name the route writes, which FastAPI reads from the
	// path unless the class is a request, response or other special type.
	Path bool `json:"path,omitempty"`
	// Class is the annotation's class, as the import that binds it writes
	// it: the module (a relative one keeps its dots) and the name in it,
	// joined by a NUL.
	Class string `json:"class,omitempty"`
	// Rule is the framework rule that decides the read: empty for
	// FastAPI's, RuleSpring for Spring's default resolution.
	Rule string `json:"rule,omitempty"`
	// Kind is, under RuleSpring, ClassSimple when the file decides the type
	// is a simple value type; Class is then empty, else the type as written.
	Kind uint8 `json:"kind,omitempty"`
	// Source and Key are, under RuleHandler, the read itself, which Param
	// then tells apart from the owner's other reads.
	Source model.InputSource `json:"source,omitempty"`
	Key    string            `json:"key,omitempty"`
}

// RuleSpring is the Rule of a Spring handler parameter left to Spring's
// default resolution.
const RuleSpring = "spring"

// RuleHandler is the Rule of a read of a function's first parameter under
// Framework's request table, which holds only when a route of another file
// names the function as a handler of that framework (route_handler.go).
const RuleHandler = "handler"

// ClassKind is what a type of the file says, by itself, about how a
// framework reads a parameter it types: for a Python class, how FastAPI
// does (Kind), and the bases it imports from another module that decide the
// rest (Bases, each a module as the import writes it and a name in it,
// joined by a NUL); for a Java type, how Spring's catch-all binds one
// (ClassSimple, ClassBody or ClassSkip).
type ClassKind struct {
	Decl  int32    `json:"decl"`
	Kind  uint8    `json:"kind"`
	Bases []string `json:"bases,omitempty"`
}

// The kinds of a class (ClassKind.Kind), as FastAPI reads a parameter it
// types. ClassOpen is a class the file proves nothing of: a scalar unless a
// base in Bases is something else.
const (
	ClassOpen uint8 = iota
	ClassBody
	ClassFile
	ClassRequest
	ClassSkip
	// classUnknown is a class the walk cannot decide (a base no member
	// declares, several declarations that disagree); never stored.
	classUnknown
	// ClassSimple is a Spring simple value type (BeanUtils
	// .isSimpleProperty), which the catch-all binds as a request parameter.
	ClassSimple
)

// keyPyClass + a dotted module name + NUL + a class's qualified name: the
// class's ClassKind payload, Aux the directory the name is read from.
const keyPyClass = "pc\x00"

// classPayload is a class's ClassKind as the module index carries it, with
// the path its bases' relative modules are read from.
type classPayload struct {
	Path  string   `json:"p"`
	Kind  uint8    `json:"k"`
	Bases []string `json:"b,omitempty"`
}

// indexPythonClasses adds each class of a Python member that a parameter
// annotation may name to the module index, under every dotted name its
// file can be imported as.
func indexPythonClasses(file int32, t *Table, add func(entry) error) error {
	if len(t.Classes) == 0 {
		return nil
	}
	payloads := make([]string, len(t.Classes))
	for i, c := range t.Classes {
		raw, err := json.Marshal(classPayload{Path: t.Path, Kind: c.Kind, Bases: c.Bases})
		if err != nil {
			return internalErr("class payload: " + err.Error())
		}
		payloads[i] = string(raw)
	}
	return pythonModuleNames(t.Path, func(name, root string) error {
		for i, c := range t.Classes {
			if c.Decl < 0 || int(c.Decl) >= len(t.Decls) || t.Decls[c.Decl].Qualified == "" {
				continue
			}
			e := entry{Key: keyPyClass + name + "\x00" + t.Decls[c.Decl].Qualified, Ref: Ref{File: file, Index: c.Decl},
				Aux: root, Sig: payloads[i]}
			if err := add(e); err != nil {
				return err
			}
		}
		return nil
	})
}

// inputFact is the payload of one FactInput (Binding.External): the read
// the pending input Site resolves to.
type inputFact = model.RequestInput

// emitPythonInputs emits one FactInput for each pending input of the
// member at ordinal file that a route of another file makes a handler's
// read (RuleHandler), and each whose class the walk decides, by FastAPI's rule:
// a path read when the route writes the parameter's name and the class is
// no special type, else a body for a model or a dataclass, a file for an
// UploadFile and a query for any other class. A request, a response or
// other special type, and a class the walk cannot decide, publish nothing.
func emitPythonInputs(file int32, t *Table, h *hierarchy, mods *index, emit func(Binding) error) error {
	handlers := pyHandlerCheck{mods: mods, file: file, t: t, named: map[pyNamedKey]bool{}}
	for i := range t.Inputs {
		p := &t.Inputs[i]
		if p.Rule == RuleHandler {
			named, err := handlers.isNamed(p.Owner, p.Framework)
			if err != nil {
				return err
			}
			if named {
				if err := emitHandlerInput(file, int32(i), p, emit); err != nil {
					return err
				}
			}
			continue
		}
		if p.Rule != "" {
			continue
		}
		kind, err := classOf(h, mods, t.Path, p.Class, nil)
		if err != nil {
			return err
		}
		var src model.InputSource
		switch {
		case kind == ClassRequest || kind == ClassSkip || kind == classUnknown:
			continue
		case p.Path:
			src = model.InputPath
		case kind == ClassFile:
			src = model.InputFile
		case kind == ClassBody:
			src = model.InputBody
		default:
			src = model.InputQuery
		}
		raw, err := json.Marshal(inputFact{Framework: p.Framework, Source: src, Key: p.Param})
		if err != nil {
			return internalErr("input fact: " + err.Error())
		}
		b := Binding{Kind: FactInput, Site: Ref{File: file, Index: int32(i)}, Target: Ref{File: -1, Index: -1},
			Detail: DetailImport, External: string(raw)}
		if err := emit(b); err != nil {
			return err
		}
	}
	return nil
}

// classOf is the kind of the class ref (a module as an import in the file
// at from writes it, NUL, a name) through the module index: the nearest
// declarations the import's lookup keeps, each its own kind or, for an open
// class, the kind its bases give (a body, file, request or special type
// when any base is one; unknown when any base is unknown or two disagree).
// A module that declares no class of the name may bind it by an import
// (reexportedClass). way holds the classes and modules the walk is inside,
// so a cycle ends it.
func classOf(h *hierarchy, mods *index, from, ref string, way []Ref) (uint8, error) {
	mod, name, ok := strings.Cut(ref, "\x00")
	if !ok || name == "" {
		return classUnknown, nil
	}
	exact := strings.HasPrefix(mod, ".")
	if exact {
		if !pythonRelativeFits(from, mod) {
			return classUnknown, nil
		}
		mod = absolutePythonModule(from, mod)
	}
	if mod == "" {
		return classUnknown, nil
	}
	var hits []entry
	near := false
	err := mods.lookup(keyPyClass+mod+"\x00"+name, func(e entry) error {
		hits = append(hits, e)
		near = near || hitNear(from, e.Aux, exact)
		return nil
	})
	if err != nil {
		return classUnknown, err
	}
	if len(hits) == 0 {
		return reexportedClass(h, mods, from, mod, name, exact, way)
	}
	out, found := classUnknown, false
	for _, e := range hits {
		if !(hitNear(from, e.Aux, exact) || !near && !exact) {
			continue
		}
		for _, w := range way {
			if w == e.Ref {
				return classUnknown, nil
			}
		}
		var p classPayload
		if err := json.Unmarshal([]byte(e.Sig), &p); err != nil {
			return classUnknown, internalErr("class payload: " + err.Error())
		}
		kind := p.Kind
		if kind == ClassOpen {
			next := append(append([]Ref(nil), way...), e.Ref)
			for _, b := range p.Bases {
				k, err := classOf(h, mods, p.Path, b, next)
				if err != nil {
					return classUnknown, err
				}
				kind = joinClass(kind, k)
			}
		}
		if found && kind != out {
			return classUnknown, nil
		}
		out, found = kind, true
	}
	return out, nil
}

// joinClass is the kind of a class with a base of kind b, the class's own
// kind so far a: an open class takes its base's kind; two bases of
// different decided kinds, or an unknown one, leave it unknown.
func joinClass(a, b uint8) uint8 {
	switch {
	case a == classUnknown || b == classUnknown:
		return classUnknown
	case b == ClassOpen:
		return a
	case a == ClassOpen || a == b:
		return b
	}
	return classUnknown
}

// inputs publishes the reads the FactInputs of pending input p, group,
// found: one `variable` node at the parameter, keyed by its owner's node
// and the parameter's name, with the read as its
// MetaInput, which the owner `reads`, detail request_input.
func (f *filePublication) inputs(p *PendingInput, group []idBinding) error {
	for _, b := range group {
		if b.Kind != FactInput {
			continue
		}
		var in inputFact
		if err := json.Unmarshal([]byte(b.External), &in); err != nil {
			return internalErr("input fact: " + err.Error())
		}
		if err := in.Validate(); err != nil {
			return internalErr("input fact: " + err.Error())
		}
		owner := f.t.Module
		if p.Owner >= 0 && int(p.Owner) < len(f.t.Decls) {
			owner = f.t.Decls[p.Owner].Endpoint
		}
		meta, err := json.Marshal(map[string]any{model.MetaInput: in})
		if err != nil {
			return internalErr("input metadata: " + err.Error())
		}
		name := nodeName(p.Name, p.Param)
		// The owner's node, not its name, keys the read: two overloads of
		// one method share a qualified name.
		key := fitKey("input:" + string(owner.Node) + "#" + p.Param)
		n, err := f.mint(model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + f.fv.Path,
			NativeKey: key, Kind: model.NodeVariable, Language: f.t.Language, Name: name, QualifiedName: name,
			FileID: f.fv.ID, ContentHash: f.fv.ContentHash, Range: located(p.Range)}, p.Range, meta)
		if err != nil {
			return err
		}
		f.node(occurrenceOf(f.fv, f.t, owner))
		f.relation(owner.Node, model.RelReads, n, located(p.Range), key, detailRequestInput, "", 0)
	}
	return nil
}

// detailRequestInput is the evidence detail of a request-input read's
// `reads` relation, as the structural provider spells it.
const detailRequestInput = "request_input"
