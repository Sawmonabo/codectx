package reconcile

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Disambiguation (ADR-0014 decision 1). Declarations of one file that share a
// kind and a descriptor path are told apart, in order, by the parameter types
// as written where the language overloads, by the conditional branch that
// encloses them, by whether a C or C++ declaration is a prototype or the
// definition, and, for the exact duplicates left, by their content. None of
// these is a position: inserting a declaration renumbers nothing.

// Disambiguator frames the parts that separate declarations sharing a
// descriptor path into the one string a candidate carries: params is the
// parameter-type list as written where the language overloads ("(String,int)"),
// branch the enclosing conditional branches as the worker spells them, and
// prototype marks a C or C++ declaration without a body, which is a separate
// entity from its definition in the same file. Empty parts are left out, so
// a declaration with none has the empty disambiguator. A part over the
// candidate's bound is replaced by its digest, which keeps it distinct and
// stable.
func Disambiguator(params, branch string, prototype bool) string {
	var parts []string
	if params != "" {
		parts = append(parts, "params="+params)
	}
	if branch != "" {
		parts = append(parts, "branch="+branch)
	}
	if prototype {
		parts = append(parts, "prototype")
	}
	return bounded(strings.Join(parts, "\x00"))
}

// Declared is one declaration of a file as SettleDuplicates reads it.
type Declared struct {
	Kind          model.NodeKind
	Descriptor    string
	Disambiguator string
	// Start and End are the declaration's byte range in the file, and
	// Content the digest of those bytes.
	Start, End uint64
	Content    string
}

// SettleDuplicates returns the disambiguator of each of decls, a file's
// declarations, once the exact duplicates are told apart. Declarations that
// still share a kind, a descriptor path and a disambiguator (a Python or
// ECMAScript name defined twice at one level, two Go `init` functions) are
// matched by content: the first in source order keeps its disambiguator, and
// every later one is keyed by the digest of its bytes, so an edit to one of
// them, or a duplicate added after them, leaves the others' identities where
// they were. Duplicates with identical content but different ranges are
// counted among themselves in source order, the one place order is left,
// since nothing else tells them apart. Two declarations with one range are
// one declaration (C's `int x, x;`, whose declarators share the
// declaration's span) and get one identity.
//
// A declaration with no descriptor is file-scoped and is returned unchanged.
// The result is a pure function of decls, so a full build and a refresh of
// the same bytes settle the same identities.
func SettleDuplicates(decls []Declared) []string {
	out := make([]string, len(decls))
	order := make([]int, 0, len(decls))
	for i, d := range decls {
		out[i] = d.Disambiguator
		if d.Descriptor != "" {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		x, y := decls[a], decls[b]
		return cmp.Or(cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.Descriptor, y.Descriptor),
			cmp.Compare(x.Disambiguator, y.Disambiguator), cmp.Compare(x.Start, y.Start), cmp.Compare(y.End, x.End))
	})
	for lo := 0; lo < len(order); {
		hi := lo + 1
		for hi < len(order) && sameIdentity(decls[order[lo]], decls[order[hi]]) {
			hi++
		}
		settleGroup(decls, order[lo:hi], out)
		lo = hi
	}
	return out
}

func sameIdentity(a, b Declared) bool {
	return a.Kind == b.Kind && a.Descriptor == b.Descriptor && a.Disambiguator == b.Disambiguator
}

// settleGroup keys every member of one duplicate group, in source order,
// after the first: by content, then by count among equal content.
func settleGroup(decls []Declared, group []int, out []string) {
	first := decls[group[0]]
	// The first keeps the plain disambiguator, which no content key equals,
	// so only later members with equal bytes are counted.
	seen := map[string]int{}
	ranges := map[[2]uint64]string{{first.Start, first.End}: out[group[0]]}
	for _, i := range group[1:] {
		d := decls[i]
		r := [2]uint64{d.Start, d.End}
		if prior, ok := ranges[r]; ok {
			out[i] = prior
			continue
		}
		key := "content=" + d.Content
		if n := seen[d.Content]; n > 0 {
			key += "\x00copy=" + strconv.Itoa(n)
		}
		seen[d.Content]++
		out[i] = bounded(join(d.Disambiguator, key))
		ranges[r] = out[i]
	}
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\x00" + b
}

// ContentDigest is the digest of a declaration's bytes that SettleDuplicates
// matches duplicates by.
func ContentDigest(src []byte) string { return model.H("declaration-content", string(src)) }

// ParameterDisambiguator is a parameter's disambiguator: its declaring
// callable's, then its position (wire.Parameter.Index), so another file's
// argument flow into it survives any edit that keeps the callable and the
// position.
func ParameterDisambiguator(callable string, index uint32) string {
	return bounded(join(callable, "parameter="+strconv.FormatUint(uint64(index), 10)))
}

// bounded keeps a disambiguator within its candidate bound: one that does not
// fit is replaced by its digest, distinct and stable.
func bounded(s string) string {
	if len(s) <= model.MaxSignatureBytes {
		return s
	}
	return "digest=" + model.H("disambiguator", s)
}
