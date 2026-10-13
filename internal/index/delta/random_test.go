package delta_test

import (
	"maps"
	"math/rand/v2"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// randomSteps is how many random edits each language's sequence makes.
const randomSteps = 8

// TestRandomEditSequencesEqualFullBuild refreshes every language's project
// through a random sequence of edits and compares each refresh with a full
// build of the same tree, fact for fact (session.check). Each step draws,
// from one seeded generator, either one of the language script's edits that
// applies to the current tree (in any order and any number of times, so
// edits meet in combinations no fixed order holds), or a file copied to a
// new path, a file removed, or a file renamed. The seed is the test start's
// time, printed with every failure; a failure is reproduced by pinning the
// printed seed in randomSeed.
//
// Mutations that fail it: any refresh that keeps a stale fact or drops a
// fresh one under some order of edits (TestIncrementalLinkEqualsFullBuild's
// mutations, reached here through orders that script does not hold).
func TestRandomEditSequencesEqualFullBuild(t *testing.T) {
	seed := randomSeed()
	t.Logf("seed %d", seed)
	for i, sc := range differentialScripts {
		t.Run(sc.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, uint64(i)))
			s := newSession(t)
			files := sc.base
			s.check("base", s.advance(files))
			for step := range randomSteps {
				name, next := randomEdit(rng, sc, files)
				if next == nil {
					break
				}
				files = next
				label := "seed " + strconv.FormatUint(seed, 10) + " step " + strconv.Itoa(step) + ": " + name
				s.check(label, s.advance(files))
				if t.Failed() {
					t.Fatalf("%s failed; pin seed %d in randomSeed to reproduce", label, seed)
				}
			}
		})
	}
}

// randomSeed is the seed of one run.
func randomSeed() uint64 { return uint64(time.Now().UnixNano()) }

// randomEdit is one edit of files drawn from rng: a script edit that applies,
// or a source file copied, removed or renamed. It answers no tree when none
// applies.
func randomEdit(rng *rand.Rand, sc script, files map[string]string) (string, map[string]string) {
	var sources []string
	for p := range files {
		if link.FamilyOf(srclang.Of(p)) != "" {
			sources = append(sources, p)
		}
	}
	slices.Sort(sources)
	var applies []edit
	for _, e := range sc.edits {
		if next, ok := tryApply(e, files); ok && !maps.Equal(next, files) {
			applies = append(applies, e)
		}
	}
	for range 8 {
		switch k := rng.IntN(4); {
		case k == 0 && len(sources) > 0:
			p := sources[rng.IntN(len(sources))]
			to := copyPath(p, files)
			out := maps.Clone(files)
			out[to] = files[p]
			return "copy " + p + " to " + to, out
		case k == 1 && len(sources) > 1:
			p := sources[rng.IntN(len(sources))]
			out := maps.Clone(files)
			delete(out, p)
			return "remove " + p, out
		case k == 2 && len(sources) > 0:
			p := sources[rng.IntN(len(sources))]
			to := copyPath(p, files)
			out := maps.Clone(files)
			delete(out, p)
			out[to] = files[p]
			return "rename " + p + " to " + to, out
		case k == 3 && len(applies) > 0:
			e := applies[rng.IntN(len(applies))]
			next, _ := tryApply(e, files)
			return e.name, next
		}
	}
	return "", nil
}

// copyPath is a path beside p, in its directory with its extension, that
// files does not hold.
func copyPath(p string, files map[string]string) string {
	dir, ext := path.Dir(p), path.Ext(p)
	base := strings.TrimSuffix(path.Base(p), ext)
	for n := 2; ; n++ {
		to := path.Join(dir, base+"_"+strconv.Itoa(n)+ext)
		if _, ok := files[to]; !ok {
			return to
		}
	}
}

// tryApply is files after e, or false when e does not apply to files: a
// substitution whose text is not there exactly once, or a file to move or
// remove that is not there.
func tryApply(e edit, files map[string]string) (map[string]string, bool) {
	out := maps.Clone(files)
	for _, r := range e.replace {
		src, ok := out[r.path]
		if !ok || strings.Count(src, r.old) != 1 {
			return nil, false
		}
		out[r.path] = strings.Replace(src, r.old, r.new, 1)
	}
	maps.Copy(out, e.write)
	for _, m := range e.move {
		src, ok := out[m[0]]
		if !ok {
			return nil, false
		}
		delete(out, m[0])
		out[m[1]] = src
	}
	for _, p := range e.remove {
		if _, ok := out[p]; !ok {
			return nil, false
		}
		delete(out, p)
	}
	return out, true
}
