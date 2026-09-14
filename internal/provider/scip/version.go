package scip

import (
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// satisfies evaluates a version constraint: an exact version ("0.1.24",
// "v0.1.24") or space-separated comparators (">=0.1.0 <0.2.0"). Versions
// compare by their dotted numeric components; a leading "v" and any
// pre-release or build suffix are ignored.
//
// Its one consumer is toolPositionEncoding, which asks whether an index was
// written by a tool *build* whose column encoding was measured. It is not a
// trust decision: nothing here decides whether a payload may run — the lock
// and the store already did (Section 20.2).
func satisfies(version, constraint string) (bool, error) {
	got, ok := parseVersion(version)
	if !ok {
		return false, outputInvalidf("tool version %q is not a dotted version", version)
	}
	for _, term := range strings.Fields(constraint) {
		op := "="
		for _, candidate := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(term, candidate) {
				op, term = candidate, term[len(candidate):]
				break
			}
		}
		want, ok := parseVersion(term)
		if !ok {
			return false, &model.Error{Code: model.CodeInternal, Message: "version constraint " + strconv.Quote(constraint) + " is not understood"}
		}
		c := compareVersions(got, want)
		switch op {
		case "=":
			ok = c == 0
		case ">=":
			ok = c >= 0
		case "<=":
			ok = c <= 0
		case ">":
			ok = c > 0
		case "<":
			ok = c < 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func compareVersions(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
