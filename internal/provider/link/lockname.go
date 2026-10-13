package link

import "strings"

// namedPylock reports whether base names a PEP 751 lock file other than
// pylock.toml: `pylock.<name>.toml`, the name non-empty and without a dot
// (the specification's `^pylock\.([^.]+)\.toml$`).
func namedPylock(base string) bool {
	name, ok := strings.CutPrefix(base, "pylock.")
	if !ok {
		return false
	}
	name, ok = strings.CutSuffix(name, ".toml")
	return ok && name != "" && !strings.Contains(name, ".")
}
