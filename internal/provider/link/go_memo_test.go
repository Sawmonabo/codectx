package link

import (
	"strconv"
	"testing"
)

// TestGoMemoAnswersEachKeyItsOwnValue protects the memo of Go lookups: every
// key read back answers the value put under it, across the move of an entry
// from the previous generation to the current one, and no generation grows
// past its bound.
//
// Mutation: answer a key from the other generation's entry of another key
// (a cross-key result) -> some read differs. Mutation: keep a stale value
// after a key is put again -> the rewritten key answers its old value.
// Mutation: never drop the oldest generation -> the bound check fails.
func TestGoMemoAnswersEachKeyItsOwnValue(t *testing.T) {
	var m goMemo[int]
	n := goMemoEntries + goMemoEntries/2
	for i := range n {
		m.put("k"+strconv.Itoa(i), i)
	}
	m.put("k7", -7)
	if v, ok := m.get("k" + strconv.Itoa(n-1)); !ok || v != n-1 {
		t.Fatalf("the last key put answers %d, %v", v, ok)
	}
	for i := range n {
		k := "k" + strconv.Itoa(i)
		v, ok := m.get(k)
		want := i
		if i == 7 {
			want = -7
		}
		if ok && v != want {
			t.Fatalf("get(%s) = %d, want %d", k, v, want)
		}
		if len(m.cur) > goMemoEntries || len(m.old) > goMemoEntries {
			t.Fatalf("generations of %d and %d entries exceed the bound %d", len(m.cur), len(m.old), goMemoEntries)
		}
	}
	if _, ok := m.get("absent"); ok {
		t.Fatal("a key never put is answered")
	}
}
