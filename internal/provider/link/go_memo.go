package link

// goMemoEntries bounds each generation of a goMemo: two generations of it at
// most are held, whatever the unit's size.
const goMemoEntries = 1 << 14

// goMemo remembers, while a Go unit's hierarchy is built, the answers of
// lookups that read only the unit's immutable indexes and the packages read
// outside it (goUnitTypes.lookup and .constant, goClause), keyed by
// everything the answer depends on, so each distinct question reads the
// indexes once instead of once per type name of every method signature.
// It holds two generations: a lookup missing the current one and found in
// the previous one is copied forward, and when the current one is full it
// becomes the previous one and the oldest is dropped. Memory is bounded by
// 2 x goMemoEntries entries; an answer is the index's own whichever
// generation serves it.
type goMemo[V any] struct {
	cur, old map[string]V
}

func (m *goMemo[V]) get(key string) (V, bool) {
	if v, ok := m.cur[key]; ok {
		return v, true
	}
	v, ok := m.old[key]
	if ok {
		m.put(key, v)
	}
	return v, ok
}

func (m *goMemo[V]) put(key string, v V) {
	if m.cur == nil || len(m.cur) >= goMemoEntries {
		m.old, m.cur = m.cur, make(map[string]V)
	}
	m.cur[key] = v
}

// goMemos are a hierarchy's memos of Go lookups.
type goMemos struct {
	types  goMemo[goTypeMemo]
	consts goMemo[[]goDecl]
	clause goMemo[goClauseMemo]
}

type goTypeMemo struct {
	answer goTypeAnswer
	known  bool
}

type goClauseMemo struct {
	clause string
	ok     bool
}

// goMemoOn reports whether h's lookups may be served from its memos: only
// while no member's reads are recorded (record), since a memoized answer
// reads no index key and its trace would miss the read.
func (h *hierarchy) goMemoOn() bool { return h.note == nil }
