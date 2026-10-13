package treesitter

import (
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// nestInto records that site s's value is consumed directly by site o, the
// innermost site containing it, inside the node that evaluates both
// (siteResults): s is part of one of o's arguments, which then binds s's
// value to the parameter it binds, or is the receiver of o, a member call
// whose callee name follows it. A site in o's callee expression otherwise
// (`f()()`) is neither and is recorded nowhere.
func nestInto(t *link.Table, s, o int) {
	in, out := &t.Sites[s], &t.Sites[o]
	start, end := in.Range.Start.Byte, in.Range.End.Byte
	arg := int32(-2)
	for k := range out.Arguments {
		r := out.Arguments[k].Range
		if r.Start.Byte <= start && end <= r.End.Byte {
			arg = int32(k)
			break
		}
	}
	if arg < 0 && out.Shape == wire.ShapeMember && end <= uint64(out.NameStart) {
		arg = link.NestedReceiver
	}
	if arg < -1 {
		return
	}
	out.Nested = append(out.Nested, link.Nested{Site: int32(s), Arg: arg})
	in.Feeds = int32(o) + 1
}
