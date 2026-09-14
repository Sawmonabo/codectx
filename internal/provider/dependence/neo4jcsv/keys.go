package neo4jcsv

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

// keyAlgebraVersion is the version of the fact-key pre-image below. It is the
// first component of every key, so a change to the algebra makes every stored
// key differ from every fresh key and the next refresh rewrites the unit
// instead of silently mis-diffing against keys built by another algebra.
const keyAlgebraVersion = "1"

// keySetMagic is the first line of a key-set file.
const keySetMagic = "codectx-dependence-keyset v1"

// keySep separates the components of a key pre-image. It is the ASCII unit
// separator, which no engine identifier, path or operator name contains.
const keySep = "\x1f"

// FactKey derives the id-independent key of one published fact.
//
// The engine renumbers node ids on every run, so a key may not contain one.
// The pre-image is the components below joined with the ASCII unit separator
// and hashed with SHA-256; the file stores the lowercase hex digest, which is
// fixed width and sorts lexicographically:
//
//	keyAlgebraVersion   "1"
//	label               the fact label: "node:method", "node:decl",
//	                    "rel:calls", "rel:control_depends_on",
//	                    "rel:data_flows_to", "rel:reads", "rel:writes",
//	                    "rel:may_refer_to"
//	owner               the FULL_NAME of the method the fact belongs to (for a
//	                    node, its own full name)
//	file                the root-relative snapshot path, or "" when the fact
//	                    has none
//	operator            the operator METHOD_FULL_NAME a reads/writes fact was
//	                    lowered from, or ""
//	target              the syntactic name of the fact's target, or ""
//	positional          the fact's ordered byte ranges, "start-end" joined
//	                    with "|", in the order (from, to, occurrence)
//
// Normalization. The Go frontend appends a package's file-level declarations
// to a synthetic per-package initializer in a nondeterministic order, so the
// coordinates of everything that initializer owns shuffle between two parses
// of identical source (the incremental-engine research round, §4.2: 20% of
// the unit's data-dependence rows). For a fact owned by an initializer the
// positional component is therefore replaced by a digest of the ordered
// source text of its endpoints: content, which is stable, instead of
// position, which is not.
func FactKey(label, owner, file, operator, target, positional string) string {
	h := sha256.New()
	for i, part := range [...]string{keyAlgebraVersion, label, owner, file, operator, target, positional} {
		if i > 0 {
			io.WriteString(h, keySep)
		}
		io.WriteString(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isInitializer reports whether a method full name is a synthetic class or
// package initializer, whose member order the frontend does not fix.
func isInitializer(fullName string) bool {
	return strings.Contains(fullName, "<clinit>")
}

// contentDigest is the position-free stand-in for an initializer-owned fact's
// coordinates: a digest of its endpoints' source text in a fixed order.
func contentDigest(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			io.WriteString(h, keySep)
		}
		io.WriteString(h, p)
	}
	return "c:" + hex.EncodeToString(h.Sum(nil)[:16])
}

// KeySet is an on-disk, sorted, bounded set of fact keys: one 64-character
// hex digest per line after a magic line, in ascending order. It is never
// held in memory; Diff streams two files in one merge pass.
//
// The zero value is the absent set. Passing it as Options.PreviousKeys means a
// full import.
type KeySet struct {
	path  string
	count int
}

// Empty reports whether this is the absent set.
func (k KeySet) Empty() bool { return k.path == "" }

// Path is the file the set is stored in.
func (k KeySet) Path() string { return k.path }

// Count is the number of keys in the set.
func (k KeySet) Count() int { return k.count }

// LoadKeySet opens a key set written by a previous import and validates that
// it is this format and sorted; an unsorted or foreign file is refused rather
// than silently producing a wrong delta.
func LoadKeySet(path string) (KeySet, error) {
	f, err := os.Open(path)
	if err != nil {
		return KeySet{}, outputInvalid("the previous fact key set cannot be opened: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 128), 128)
	if !sc.Scan() || sc.Text() != keySetMagic {
		return KeySet{}, outputInvalid("the previous fact key set is not a %s file", keySetMagic)
	}
	prev, n := "", 0
	for sc.Scan() {
		line := sc.Text()
		if len(line) != 2*sha256.Size {
			return KeySet{}, outputInvalid("the previous fact key set holds a line that is not a fact key")
		}
		if line <= prev {
			return KeySet{}, outputInvalid("the previous fact key set is not sorted")
		}
		prev = line
		n++
	}
	if err := sc.Err(); err != nil {
		return KeySet{}, outputInvalid("the previous fact key set cannot be read: %v", err)
	}
	return KeySet{path: path, count: n}, nil
}

// Delta is the result of comparing two key sets.
type Delta struct {
	// Changed counts the keys this set has and prev did not: the facts a
	// delta import publishes.
	Changed int
	// Unchanged counts the keys both sets hold: the rows storage may keep.
	Unchanged int
	// Removed counts the keys prev held and this set does not: the rows
	// storage must delete.
	Removed int
}

// Diff streams k against prev in one merge pass and calls visit for every key
// that is in exactly one of them: removed is false for a key k adds and true
// for a key prev held and k does not. visit may be nil. Diffing against the
// absent set reports every key as changed.
func (k KeySet) Diff(prev KeySet, visit func(key string, removed bool) error) (Delta, error) {
	var d Delta
	fresh, err := openKeyLines(k)
	if err != nil {
		return d, err
	}
	defer fresh.close()
	old, err := openKeyLines(prev)
	if err != nil {
		return d, err
	}
	defer old.close()
	a, aok, err := fresh.next()
	if err != nil {
		return d, err
	}
	b, bok, err := old.next()
	if err != nil {
		return d, err
	}
	for aok || bok {
		switch {
		case aok && (!bok || a < b):
			d.Changed++
			if visit != nil {
				if err := visit(a, false); err != nil {
					return d, err
				}
			}
			a, aok, err = fresh.next()
		case bok && (!aok || b < a):
			d.Removed++
			if visit != nil {
				if err := visit(b, true); err != nil {
					return d, err
				}
			}
			b, bok, err = old.next()
		default:
			d.Unchanged++
			if a, aok, err = fresh.next(); err == nil {
				b, bok, err = old.next()
			}
		}
		if err != nil {
			return d, err
		}
	}
	return d, nil
}

// keyLines streams the keys of one set; the absent set streams nothing.
type keyLines struct {
	f  *os.File
	sc *bufio.Scanner
}

func openKeyLines(k KeySet) (*keyLines, error) {
	if k.Empty() {
		return &keyLines{}, nil
	}
	f, err := os.Open(k.path)
	if err != nil {
		return nil, outputInvalid("a fact key set cannot be opened: %v", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 128), 128)
	if !sc.Scan() || sc.Text() != keySetMagic {
		f.Close()
		return nil, outputInvalid("a fact key set is not a %s file", keySetMagic)
	}
	return &keyLines{f: f, sc: sc}, nil
}

func (l *keyLines) next() (string, bool, error) {
	if l.sc == nil {
		return "", false, nil
	}
	if !l.sc.Scan() {
		if err := l.sc.Err(); err != nil {
			return "", false, outputInvalid("a fact key set cannot be read: %v", err)
		}
		return "", false, nil
	}
	return l.sc.Text(), true, nil
}

func (l *keyLines) close() {
	if l.f != nil {
		l.f.Close()
	}
}

// saveKeys writes the import's key set: the magic line and every distinct
// fact key in ascending order, streamed from the scratch so the set is never
// materialized in the heap.
func (s *scratch) saveKeys(ctx context.Context, path string) (KeySet, error) {
	if err := s.commit(ctx); err != nil {
		return KeySet{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO keys(key) SELECT key FROM ident WHERE key <> ''
		UNION SELECT key FROM rels WHERE key <> ''`); err != nil {
		return KeySet{}, internalErr("import keys: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return KeySet{}, internalErr("import keys: %v", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if _, err := w.WriteString(keySetMagic + "\n"); err != nil {
		return KeySet{}, internalErr("import keys: %v", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM keys ORDER BY key`)
	if err != nil {
		return KeySet{}, internalErr("import keys: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return KeySet{}, internalErr("import keys: %v", err)
		}
		if _, err := w.WriteString(key + "\n"); err != nil {
			return KeySet{}, internalErr("import keys: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return KeySet{}, internalErr("import keys: %v", err)
	}
	if err := w.Flush(); err != nil {
		return KeySet{}, internalErr("import keys: %v", err)
	}
	return KeySet{path: path, count: n}, nil
}

// markDelta records, for every fresh key, whether the previous run already
// published it. A delta import emits only the relations whose key is marked
// changed; the removed count is what storage must delete.
func (s *scratch) markDelta(ctx context.Context, fresh, prev KeySet) (Delta, error) {
	if prev.Empty() {
		return Delta{Changed: fresh.Count()}, nil
	}
	// Diff visits only the keys that differ; a key in both sets is unchanged,
	// so mark every fresh key unchanged first and re-mark the ones Diff
	// reports as added.
	if _, err := s.db.ExecContext(ctx, `UPDATE keys SET changed = 0`); err != nil {
		return Delta{}, internalErr("import delta: %v", err)
	}
	delta, err := fresh.Diff(prev, func(key string, removed bool) error {
		if removed {
			return nil
		}
		return s.exec(ctx, `UPDATE keys SET changed = 1 WHERE key = ?`, key)
	})
	if err != nil {
		return Delta{}, err
	}
	if err := s.commit(ctx); err != nil {
		return Delta{}, err
	}
	return delta, nil
}
