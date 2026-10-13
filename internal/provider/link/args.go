package link

import "github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"

// paramInfo is one declared parameter of a callable as resolution reads it
// (hierarchy.parameters): Pos is its index in the callable's
// Decl.Parameters, and the rest is that Parameter's name, marks and return
// summary.
type paramInfo struct {
	Pos         int32
	Name        string
	Index       uint32
	Variadic    bool
	KeywordOnly bool
	Returned    bool
	// Unknown is Parameter.Unknown.
	Unknown bool
}

// argumentFlows yields each argument of the bound call site s with the
// parameter it binds among params, the callee's declared parameters in
// declaration order: the pairs whose FactFlow carries the argument's value
// into the parameter (ADR-0017 decision 7). A site with no argument
// positions (no Site.Arguments) yields nothing.
//
// Arguments bind by position up to the first keyword argument, then by
// keyword name; a keyword no parameter names binds the callee's variadic
// keyword parameter (Python `**kwargs`). A positional spread (`*xs`,
// `...xs`, Go `xs...`) binds the callee's variadic parameter, and so does
// every positional argument after it, whose position is no longer known; a
// keyword spread (`**kw`) binds the variadic keyword parameter. A
// positional argument past the declared positional parameters binds the
// variadic parameter. A parameter no argument reaches, left at its default,
// is never yielded.
//
// The positional parameters are those of Index 1 and up, neither variadic
// nor keyword-only. The receiver or self parameter (Index 0) is bound only
// when receiverFirst says the site passes the receiver as its first argument
// (a Python method called through its class, a Go method expression, a Rust
// path naming a method): the first argument then binds it, or nothing when
// the callee declares none.
func argumentFlows(s *Site, params []paramInfo, receiverFirst bool, yield func(arg, pos int32) error) error {
	var receiver, variadic, keywords *paramInfo
	var positional []*paramInfo
	for i := range params {
		p := &params[i]
		switch {
		case p.Index == 0:
			receiver = p
		case p.Variadic && p.KeywordOnly:
			keywords = p
		case p.Variadic:
			variadic = p
		case !p.KeywordOnly:
			positional = append(positional, p)
		}
	}
	named := func(name string) *paramInfo {
		for i := range params {
			p := &params[i]
			if p.Index > 0 && !p.Variadic && p.Name == name {
				return p
			}
		}
		return nil
	}
	bind := func(arg int, p *paramInfo) error {
		if p == nil {
			return nil
		}
		return yield(int32(arg), p.Pos)
	}
	next, spread := 0, false
	for i := range s.Arguments {
		a := &s.Arguments[i]
		if i == 0 && receiverFirst && a.Keyword == "" && a.Spread == wire.SpreadNone {
			if err := bind(i, receiver); err != nil {
				return err
			}
			continue
		}
		var err error
		switch {
		case a.Spread == wire.SpreadKeyword:
			err = bind(i, keywords)
		case a.Spread == wire.SpreadPositional:
			spread = true
			err = bind(i, variadic)
		case a.Keyword != "":
			if p := named(a.Keyword); p != nil {
				err = bind(i, p)
			} else {
				err = bind(i, keywords)
			}
		case spread || next >= len(positional):
			err = bind(i, variadic)
		default:
			err = bind(i, positional[next])
			next++
		}
		if err != nil {
			return err
		}
	}
	return nil
}
