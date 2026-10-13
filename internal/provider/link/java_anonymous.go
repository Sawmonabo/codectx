package link

import (
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The Java worker names each anonymous class body (and each enum constant's
// body) `#n`, n the byte offset of the name token of the declaration the
// structural provider publishes for it: a name no Java identifier can spell
// (JLS §3.8), and one that moves with every edit above the body. What the
// linker keeps of it never holds the offset: a body with a declaration is
// that declaration (its index in the table), and one without is a synthetic
// type numbered by its rank among the file's bodies with no declaration, in
// source order. An edit that only moves code keeps both, so it keeps the
// file's summaries, its hierarchy entries and its trace keys.

// javaAnonymous is how a Java table's anonymous body names resolve: decl
// maps the offset of each declaration a body can be named after (a class
// named `new T`, or an enum constant) to its index, rank each offset a name
// of the table carries that no declaration has to its rank among those.
type javaAnonymous struct {
	decl map[uint64]int32
	rank map[uint64]int32
}

// anonymousNames reads t's anonymous body names (nothing for a table of any
// other language).
func anonymousNames(t *Table) javaAnonymous {
	var a javaAnonymous
	if t.Language != "java" {
		return a
	}
	for i := range t.Decls {
		d := &t.Decls[i]
		if d.NameRange != nil && (d.Kind == model.NodeConstant || anonymousClass(d)) {
			if a.decl == nil {
				a.decl = map[uint64]int32{}
			}
			a.decl[d.NameRange.Start.Byte] = int32(i)
		}
	}
	var open []uint64
	t.eachTypeName(func(s *string) {
		eachAnonymous(*s, func(n uint64) {
			if _, ok := a.decl[n]; !ok {
				open = append(open, n)
			}
		})
	})
	if len(open) == 0 {
		return a
	}
	slices.Sort(open)
	open = slices.Compact(open)
	a.rank = make(map[uint64]int32, len(open))
	for k, n := range open {
		a.rank[n] = int32(k)
	}
	return a
}

// eachAnonymous calls yield with the offset of each anonymous body name
// (`#` and decimal digits) written in s.
func eachAnonymous(s string, yield func(uint64)) {
	for i := 0; i < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j > i+1 {
			if n, err := strconv.ParseUint(s[i+1:j], 10, 31); err == nil {
				yield(n)
			}
		}
		i = j - 1
	}
}

// stable is s with each anonymous body name spelled without its offset: `#d`
// and the declaration's index, or `#s` and the synthetic type's rank.
func (a javaAnonymous) stable(s string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		j := i + 1
		for s[i] == '#' && j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i+1 {
			b.WriteByte(s[i])
			continue
		}
		n, err := strconv.ParseUint(s[i+1:j], 10, 31)
		switch d, isDecl := a.decl[n]; {
		case err != nil:
			b.WriteString(s[i:j])
		case isDecl:
			b.WriteString("#d" + strconv.Itoa(int(d)))
		default:
			b.WriteString("#s" + strconv.Itoa(int(a.rank[n])))
		}
		i = j - 1
	}
	return b.String()
}

// eachTypeName calls f with every type name t carries, each a pointer into
// t: what a declaration's receiver and return name, each supertype row, each
// site's qualifier, receiver and arguments, each value and each union.
func (t *Table) eachTypeName(f func(*string)) {
	for i := range t.Decls {
		f(&t.Decls[i].Receiver)
		f(&t.Decls[i].Returns.Name)
	}
	for i := range t.Supertypes {
		f(&t.Supertypes[i].Type)
		f(&t.Supertypes[i].Super)
	}
	for i := range t.Sites {
		s := &t.Sites[i]
		f(&s.Qualifier)
		f(&s.Receiver.Name)
		for j := range s.Args {
			f(&s.Args[j].Name)
		}
	}
	for i := range t.Values {
		f(&t.Values[i].Type.Name)
	}
	for i := range t.Unions {
		for j := range t.Unions[i] {
			f(&t.Unions[i][j].Name)
		}
	}
}
