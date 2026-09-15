package config

import (
	"fmt"
	"math"
	"strconv"
)

// Limit is a count or size bound from Section 20.1. Zero means unlimited and is
// the default for every such bound: the product must index a repository of any
// size, so no built-in value may refuse a repository, skip a file, fail an
// analysis unit, drop a row or truncate an answer. A negative value is not a
// third meaning and is refused by validate.
//
// A user-set (non-zero) limit is still honoured, but exceeding one is always
// REPORTED at the enforcement site -- never a silent clamp and never a silent
// drop. Enforcement sites ask Exceeded; they must not compare the underlying
// integer, because `limit > 0` reads as "a limit is configured" at one site and
// as "the bound is satisfied" at the next.
type Limit int64

// Unlimited is the zero Limit: no bound at all.
const Unlimited Limit = 0

// unlimitedSpelling is the TOML and fingerprint spelling of Unlimited. It is
// stable: it is hashed into SourcePolicyHash, AnalysisConfigHash and
// ContextPolicyHash, so changing it would invalidate every stored identity.
const unlimitedSpelling = "unlimited"

// IsUnlimited reports whether the bound is absent.
func (l Limit) IsUnlimited() bool { return l == Unlimited }

// Exceeded reports whether n is over a user-set bound. The comparison is
// strict: n exceeds the limit only when n is greater than it, so a site that
// admits exactly the limit keeps admitting it. An unlimited bound is never
// exceeded.
func (l Limit) Exceeded(n int64) bool { return !l.IsUnlimited() && n > int64(l) }

// Value is the configured bound, or 0 when unlimited. Prefer ValueOr at any
// site that needs a finite number: an unlimited bound rendered as 0 into a
// buffer size, a page size or a SQL LIMIT is a zero-sized budget, which is the
// opposite of what it means.
func (l Limit) Value() int64 { return int64(l) }

// ValueOr is the configured bound, or fallback when unlimited. It is how a
// caller that genuinely needs a finite number (a pre-allocation, a batch size,
// a wire ceiling) reads a Limit.
func (l Limit) ValueOr(fallback int64) int64 {
	if l.IsUnlimited() {
		return fallback
	}
	return int64(l)
}

// Int is Value narrowed to int for the int-shaped call sites, saturating rather
// than wrapping on a 32-bit build.
func (l Limit) Int() int {
	if int64(l) > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(l)
}

// Min is the narrower of two bounds. Unlimited is the top of the lattice, so
// any finite bound lowers it and Min of two unlimited bounds is unlimited.
func (l Limit) Min(other Limit) Limit {
	switch {
	case l.IsUnlimited():
		return other
	case other.IsUnlimited():
		return l
	case other < l:
		return other
	default:
		return l
	}
}

// String is the canonical spelling, used in diagnostics and as the fingerprint
// component for every bound that feeds a policy hash.
func (l Limit) String() string {
	if l.IsUnlimited() {
		return unlimitedSpelling
	}
	return strconv.FormatInt(int64(l), 10)
}

// UnmarshalTOML accepts the two spellings Section 20.1 uses for a bound: an
// integer, or the string "unlimited" for the absent bound that 0 also spells.
// TOML integers never reach a TextUnmarshaler, so this type implements
// toml.Unmarshaler rather than encoding.TextUnmarshaler.
func (l *Limit) UnmarshalTOML(v any) error {
	switch value := v.(type) {
	case int64:
		*l = Limit(value)
		return nil
	case string:
		if value == unlimitedSpelling {
			*l = Unlimited
			return nil
		}
	}
	return fmt.Errorf("want an integer or %q, got %v", unlimitedSpelling, v)
}
