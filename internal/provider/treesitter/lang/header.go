package lang

import (
	"path"
	"slices"
)

// The two grammars a header can be parsed with.
const (
	nameC   = "c"
	nameCPP = "cpp"
)

// Census counts a repository's C and C++ translation units: the files whose
// extension one of the two grammars declares and that are not headers. It is
// the evidence a header's grammar is decided from, since the repository, not
// the file name, knows which language its headers are written in. The zero
// value is the census of a repository with neither.
type Census struct {
	C   uint64
	CPP uint64
}

// Add counts one repository path. A header, and a path of any other
// language, counts nothing. The extension is matched exactly as a grammar
// declares it: by the compilers' convention `.C` is C++, so folding case
// would count it as C, and a unit the registry does not spell is counted as
// neither.
func (c *Census) Add(p string) {
	ext := path.Ext(p)
	for _, l := range All {
		if !slices.Contains(l.Extensions, ext) || slices.Contains(l.Headers, ext) {
			continue
		}
		switch l.Name {
		case nameC:
			c.C++
		case nameCPP:
			c.CPP++
		}
	}
}

// HeaderPlan is the repository's decision for every header: the grammar each
// is parsed with first, and the one a parse with errors falls back to.
type HeaderPlan struct {
	First    string
	Fallback string
}

// Header applies the census rule. C translation units and no C++ ones make
// headers C. C++ ones and no C ones make them C++. Both, or neither, parse
// headers as C++ first: the closer superset of the two dialects that occur
// in headers, where nothing in the repository singles out C.
func (c Census) Header() HeaderPlan {
	if c.C > 0 && c.CPP == 0 {
		return HeaderPlan{First: nameC, Fallback: nameCPP}
	}
	return HeaderPlan{First: nameCPP, Fallback: nameC}
}

// Within is the plan restricted to the grammars a parser enables: with both,
// the plan as it stands; with one, that grammar alone and no fallback; with
// neither, false. It is the one answer to which grammar a header is parsed
// with first, read by the parse and by the header unit's identity alike, so
// the two cannot disagree.
func (p HeaderPlan) Within(enabled func(name string) bool) (first, fallback string, ok bool) {
	switch f, b := enabled(p.First), enabled(p.Fallback); {
	case f && b:
		return p.First, p.Fallback, true
	case f:
		return p.First, "", true
	case b:
		return p.Fallback, "", true
	}
	return "", "", false
}

// ParseErrors is what one parse of a header reports about its errors.
type ParseErrors struct {
	// Any reports that the tree has an error or a missing node.
	Any bool `json:"any"`
	// Bytes is the source bytes the tree's error nodes cover, each byte
	// counted once: an error node inside another adds nothing. A missing
	// node covers no bytes, so Any can hold with Bytes zero; Bytes above
	// zero implies Any.
	Bytes uint64 `json:"bytes"`
}

// HeaderReason says why a header's kept parse was kept.
type HeaderReason string

const (
	// HeaderClean: the first parse had no errors; the fallback never ran.
	HeaderClean HeaderReason = "clean"
	// HeaderFirstKept: the first parse had errors and the fallback's were no
	// fewer.
	HeaderFirstKept HeaderReason = "first_kept"
	// HeaderFallbackKept: the fallback's parse had fewer error bytes, or as
	// many and no error at all.
	HeaderFallbackKept HeaderReason = "fallback_kept"
)

// HeaderChoice is the per-file disclosure of a header's grammar: which it was
// parsed with first, which parse was kept and why, and the error figures that
// decided it. Fallback is meaningful only when the reason is not HeaderClean.
// It names grammars and byte counts only, never source.
type HeaderChoice struct {
	First    string       `json:"first"`
	Kept     string       `json:"kept"`
	Reason   HeaderReason `json:"reason"`
	FirstErr ParseErrors  `json:"first_errors"`
	Fallback ParseErrors  `json:"fallback_errors"`
}

// Choose decides one header from its first parse. A clean first parse is
// kept and parseFallback is never called; otherwise the header is parsed
// once with the fallback grammar and the parse with fewer error bytes is
// kept. On equal bytes the first is kept unless the fallback alone is free of
// errors, which only a first parse whose errors are all missing nodes can
// lose to. A failing fallback parse fails the choice.
func (p HeaderPlan) Choose(first ParseErrors, parseFallback func() (ParseErrors, error)) (HeaderChoice, error) {
	choice := HeaderChoice{First: p.First, Kept: p.First, Reason: HeaderClean, FirstErr: first}
	if !first.Any {
		return choice, nil
	}
	fallback, err := parseFallback()
	if err != nil {
		return HeaderChoice{}, err
	}
	choice.Fallback, choice.Reason = fallback, HeaderFirstKept
	if keepFallback(first, fallback) {
		choice.Kept, choice.Reason = p.Fallback, HeaderFallbackKept
	}
	return choice, nil
}

// keepFallback is the fallback decision over two parses' error figures.
func keepFallback(first, fallback ParseErrors) bool {
	if fallback.Bytes != first.Bytes {
		return fallback.Bytes < first.Bytes
	}
	return first.Any && !fallback.Any
}
