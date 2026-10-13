package link

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// A Rust declaration written in an item-position macro's body is the
// macro's input (Decl.MacroInput). The structural provider judges it against
// a `macro_rules!` of its own file in textual scope there (Decl.MacroLocal);
// every other one names a macro the file does not define, which a crate
// brings into textual scope from another of its files (a `#[macro_use]`
// module declared before the one invoking it) or exports at its root (a
// `#[macro_export]` definition, named `crate::m`; the Rust Reference's
// "Macros By Example", scoping, exporting and importing). The linker matches
// that name to the unit's `macro_rules!` definitions of it in other files
// and publishes a `generates` from each to the declaration, every one with
// the number of definitions matched as its set size when there are several:
// the macro is one of them. The occurrence's detail says whether that definition transcribes its
// item input unchanged (Decl.MacroTranscribes, DetailMacroTranscribes), so
// the declaration is exactly an item of its expansion, or may transcribe it
// renamed, repeated or not at all (DetailMacroInput). A path through another
// crate (`other::m`) names no definition of the unit, and a name no other
// file defines publishes nothing.

// keyRustMacroRules + name: a Rust `macro_rules!` of that name, its file's
// path in Aux and, when it transcribes its item input unchanged
// (Decl.MacroTranscribes), "transcribes" in Sig.
const keyRustMacroRules = "rmr\x00"

// rustMacroTranscribes is the Sig of a keyRustMacroRules entry whose
// definition transcribes its item input unchanged.
const rustMacroTranscribes = "transcribes"

// addRustMacro adds the macro_rules definition d at ref of the Rust file t to
// the declarations index; any other declaration adds nothing.
func addRustMacro(s *pagination.ExternalSort[entry], fc fileCtx, ref Ref, t *Table, d *Decl) error {
	if fc.family != "rust" || !d.Macro || d.Name == "" {
		return nil
	}
	e := entry{Key: keyRustMacroRules + d.Name, Ref: ref, Aux: t.Path}
	if d.MacroTranscribes {
		e.Sig = rustMacroTranscribes
	}
	return s.Add(e)
}

// rustMacroName is the macro_rules name the macro path a crate's own
// definition may answer to: a single identifier, or one after `crate::` or
// `$crate::`, the crate root where an exported macro is named; empty for a
// path through any other module or crate.
func rustMacroName(path string) string {
	for _, root := range []string{"crate::", "$crate::"} {
		if rest, ok := strings.CutPrefix(path, root); ok {
			path = rest
			break
		}
	}
	if path == "" || strings.Contains(path, "::") {
		return ""
	}
	return path
}

// macroInputFacts emits, for the declaration d at ref of the Rust file of
// context fc, the input of a macro its own file does not define in scope,
// one FactGenerates from each macro_rules of the unit's other files that the
// macro's path names.
func (h *hierarchy) macroInputFacts(fc fileCtx, ref Ref, d *Decl, emit func(Binding) error) error {
	if fc.family != "rust" || d.MacroInput == "" || d.MacroLocal {
		return nil
	}
	name := rustMacroName(d.MacroInput)
	if name == "" {
		return nil
	}
	return h.decls.lookup(keyRustMacroRules+name, func(e entry) error {
		if e.Ref.File == ref.File {
			// The worker found none of its file's definitions in scope.
			return nil
		}
		detail := DetailMacroInput
		if e.Sig == rustMacroTranscribes {
			detail = DetailMacroTranscribes
		}
		return emit(Binding{Kind: FactGenerates, Site: ref, Target: e.Ref, Detail: detail})
	})
}

// generates publishes declaration d's `generates` facts of group, each from
// the macro definition it targets to d, located at d, with the number of
// definitions matched as its set size when there are several.
func (f *filePublication) generates(d *Decl, group []idBinding) {
	var set int64
	for _, b := range group {
		if b.Kind == FactGenerates {
			set++
		}
	}
	if set == 0 {
		return
	}
	if set == 1 {
		// One definition is no set.
		set = 0
	}
	f.node(occurrenceOf(f.fv, f.t, d.Endpoint))
	for _, b := range group {
		if b.Kind == FactGenerates {
			f.relation(b.Target, model.RelGenerates, d.Node, located(d.Range), "", b.Detail, "", set)
		}
	}
}
