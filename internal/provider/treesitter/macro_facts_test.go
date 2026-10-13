package treesitter_test

import "testing"

// TestRustMacroInputItemsCarryTheirOrigin indexes a Rust file whose items
// are written in two item-position macros' bodies and asserts that each is
// a node marked as its macro's input, with the macro's path, one node per
// macro, while an item outside any macro carries no
// origin.
//
// Mutation: drop the origin from the metadata -> block_on has no origin.
// Mutation: name the outermost macro, or none, as origin_macro -> the two
// block_on are not one per macro.
func TestRustMacroInputItemsCarryTheirOrigin(t *testing.T) {
	p := newProvider(t)
	src := "cfg_rt! {\n    pub(crate) fn block_on() {}\n}\ncfg_not_rt! {\n    pub(crate) fn block_on() {}\n}\npub fn plain() {}\n"
	nodes := indexed(t, p, "src/lib.rs", src)
	got := map[string]string{}
	for _, n := range declared(nodes, "block_on") {
		m := metadata(t, n)
		if m["origin"] != "macro_input" {
			t.Fatalf("block_on %s has origin %v, want macro_input", n.ID, m["origin"])
		}
		macro, _ := m["origin_macro"].(string)
		got[macro] = string(n.ID)
	}
	if len(got) != 2 || got["cfg_rt"] == "" || got["cfg_not_rt"] == "" || got["cfg_rt"] == got["cfg_not_rt"] {
		t.Fatalf("the block_on nodes by macro are %v, want one per macro", got)
	}
	for _, n := range declared(nodes, "plain") {
		if o, ok := metadata(t, n)["origin"]; ok {
			t.Fatalf("plain, outside every macro, has origin %v", o)
		}
	}
}
