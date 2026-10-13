package manifest

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"strings"
)

// The text lock files (gradle.lockfile, yarn.lock, pnpm-lock.yaml) are read
// line by line over the retained bytes: each format puts one package per
// block of lines, so a block is read and turned into its entry before the
// next begins, and no document tree is ever built.

// textLine is one line of a lock file: its byte span without the newline,
// its indentation in spaces, and its text after the indentation with
// trailing whitespace removed.
type textLine struct {
	start, end int
	indent     int
	text       string
}

// eachLine yields every line of data that is not blank and not a `#`
// comment.
func eachLine(data []byte, yield func(textLine) error) error {
	for off := 0; off < len(data); {
		next := bytes.IndexByte(data[off:], '\n')
		end := len(data)
		if next >= 0 {
			end = off + next
		}
		raw := bytes.TrimRight(data[off:end], " \t\r")
		trimmed := bytes.TrimLeft(raw, " ")
		if len(trimmed) > 0 && trimmed[0] != '#' {
			if err := yield(textLine{start: off, end: off + len(raw), indent: len(raw) - len(trimmed), text: string(trimmed)}); err != nil {
				return err
			}
		}
		off = end + 1
	}
	return nil
}

// unquote removes one pair of matching double or single quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// gradleLock reads gradle.lockfile: one `group:artifact:version=conf,...`
// line per resolved module, listing the configurations that resolve it,
// and an `empty=` line naming the configurations that resolve nothing. The
// format records no dependency graph and no hash.
func (u *unit) gradleLock(ctx context.Context) error {
	var entries []lockEntry
	err := eachLine(u.data, func(l textLine) error {
		coord, confs, _ := strings.Cut(l.text, "=")
		if coord == "empty" {
			return nil
		}
		parts := strings.Split(coord, ":")
		if len(parts) < 3 {
			u.malformedEntry()
			return nil
		}
		entries = append(entries, lockEntry{name: parts[0] + ":" + parts[1], version: strings.Join(parts[2:], ":"),
			group: strings.ReplaceAll(confs, " ", ""), start: l.start, end: l.end})
		return nil
	})
	if err != nil {
		return err
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystemMaven, language: languageJava, entries: entries})
}

// yarnLock reads yarn.lock in both of its formats: the classic one
// (`# yarn lockfile v1`, fields written `key "value"`) and the YAML one of
// later releases (`__metadata:`, fields written `key: value`). A block
// starts at an unindented line listing the descriptors (`name@range`) it
// satisfies; its fields give the version, where it was resolved from, the
// integrity (classic) or checksum (later), and its dependency tables. A
// dependency resolves to the block whose descriptors include it, which is
// exactly how the lock was written. A descriptor whose range is an npm
// alias (`alias@npm:name@range`) names the package name, installed under
// the alias.
func (u *unit) yarnLock(ctx context.Context) error {
	var (
		entries []lockEntry
		cur     = -1
		table   string
		version string
		berry   bool
	)
	byDescriptor := map[string]int{}
	var descriptors [][]string
	err := eachLine(u.data, func(l textLine) error {
		switch {
		case l.indent == 0:
			table, cur = "", -1
			head := strings.TrimSuffix(l.text, ":")
			if head == "__metadata" {
				berry = true
				return nil
			}
			var descs []string
			for d := range strings.SplitSeq(head, ",") {
				if d = strings.Trim(d, ` "'`); d != "" {
					descs = append(descs, d)
				}
			}
			if len(descs) == 0 {
				u.malformedEntry()
				return nil
			}
			name, rng := yarnSplit(descs[0])
			alias := ""
			if own, _, ok := npmAlias(rng); ok {
				name, alias = own, name
			}
			entries = append(entries, lockEntry{name: name, alias: alias, start: l.start, end: l.end})
			descriptors = append(descriptors, descs)
			cur = len(entries) - 1
		case cur < 0:
			if key, value := yarnField(l.text); key == "version" && berry {
				version = value
			}
		case l.indent <= 2:
			e := &entries[cur]
			e.end = l.end
			key, value := yarnField(l.text)
			table = ""
			switch key {
			case "version":
				e.version = value
			case "resolved", "resolution":
				e.source = value
				if strings.Contains(value, "@workspace:") || strings.Contains(value, "@link:") || strings.Contains(value, "@portal:") ||
					strings.Contains(value, "@file:") {
					e.local = true
				}
			case "integrity", "checksum":
				e.integrity = value
			case "dependencies", "optionalDependencies", "peerDependencies":
				if value == "" {
					table = key
				}
			}
		default:
			e := &entries[cur]
			e.end = l.end
			if table == "" {
				return nil
			}
			name, req := yarnField(l.text)
			kind := KindRuntime
			switch table {
			case "optionalDependencies":
				kind = KindOptional
			case "peerDependencies":
				kind = KindPeer
			}
			e.deps = append(e.deps, lockDep{name: name, requirement: req, kind: kind})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range entries {
		// A descriptor that names the package by an alias alone
		// (`alias@npm:name`, no range) is told by where the block was
		// resolved from, which names the package itself.
		if e := &entries[i]; e.alias == "" {
			if own := yarnResolvedName(e.source); own != "" && own != e.name {
				e.name, e.alias = own, e.name
			}
		}
	}
	for i, descs := range descriptors {
		for _, d := range descs {
			byDescriptor[d] = i
		}
	}
	for i := range entries {
		for j := range entries[i].deps {
			d := &entries[i].deps[j]
			k, ok := byDescriptor[d.name+"@"+d.requirement]
			if !ok && berry {
				k, ok = byDescriptor[d.name+"@npm:"+d.requirement]
			}
			if ok {
				d.version = entries[k].version
				if entries[k].name != d.name {
					d.target = entries[k].name
				}
			}
		}
	}
	if version == "" && !berry {
		version = "1"
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystemNPM, language: languageJS, version: version, entries: entries})
}

// yarnResolvedName is the package a yarn block was resolved as: the name
// of a later yarn's `resolution` (`name@npm:version`), or the package path
// of a classic `resolved` registry tarball URL (`<registry>/<name>/-/<file>`,
// the name its last path component, or its last two under a scope, so a
// registry served under a path prefix adds nothing to it); "" for any other
// source.
func yarnResolvedName(source string) string {
	if name, rng := yarnSplit(source); strings.HasPrefix(rng, "npm:") {
		return name
	}
	before, _, ok := strings.Cut(source, "/-/")
	if !ok || !strings.Contains(before, "://") {
		return ""
	}
	_, path, _ := strings.Cut(before, "://")
	if _, path, ok = strings.Cut(path, "/"); !ok || path == "" {
		return ""
	}
	return bunName(path)
}

// yarnSplit splits a descriptor `name@range` at the first `@` after the
// first character, which a scoped name (`@scope/name`) begins with.
func yarnSplit(d string) (name, rng string) {
	if i := strings.IndexByte(d[min(1, len(d)):], '@'); i >= 0 {
		return d[:i+1], d[i+2:]
	}
	return d, ""
}

// yarnField splits a field line of either yarn format or of YAML:
// `key "value"`, `key value`, `key: value`, `"key": value` or
// `'key': value`, the value unquoted.
func yarnField(text string) (key, value string) {
	if strings.HasPrefix(text, `"`) || strings.HasPrefix(text, "'") {
		if end := strings.IndexByte(text[1:], text[0]); end >= 0 {
			key, value = text[1:end+1], text[end+2:]
		}
	} else if i := strings.IndexAny(text, ": "); i >= 0 {
		key, value = text[:i], text[i:]
	} else {
		key = text
	}
	value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
	return key, unquote(value)
}

// pnpmLock reads pnpm-lock.yaml. Every format version keys its packages
// under `packages:` (`/name/version` before version 6, `/name@version` in
// 6, `name@version` from 9, a peer suffix `(peer@1)` after the version);
// each has `resolution: {integrity: ...}` (or a tarball, directory or
// commit) and, before version 9, its `dependencies` with the exact version
// each resolves to. From version 9 the dependencies are in `snapshots:`,
// keyed like the packages with their peer suffix; a snapshot's dependencies
// join the package it is a snapshot of.
func (u *unit) pnpmLock(ctx context.Context) error {
	var (
		entries []lockEntry
		cur     = -1
		section string
		table   string
		version string
	)
	byKey := map[string]int{}
	err := eachLine(u.data, func(l textLine) error {
		switch {
		case l.indent == 0:
			cur, table = -1, ""
			key, value := yarnField(l.text)
			section = key
			if key == "lockfileVersion" {
				version = value
			}
		case section != "packages" && section != "snapshots":
		case l.indent == 2:
			table = ""
			key := unquote(strings.TrimSuffix(strings.TrimSuffix(l.text, " {}"), ":"))
			name, ver := pnpmSplit(key, version)
			base := name + "@" + ver
			if i, ok := byKey[base]; ok {
				cur = i
				return nil
			}
			entries = append(entries, lockEntry{name: name, version: ver, start: l.start, end: l.end})
			cur = len(entries) - 1
			byKey[base] = cur
		case cur < 0:
		case l.indent == 4:
			e := &entries[cur]
			if section == "packages" {
				e.end = l.end
			}
			key, value := yarnField(l.text)
			table = ""
			switch key {
			case "resolution":
				fields := pnpmFlow(value)
				e.integrity = fields["integrity"]
				switch {
				case fields["tarball"] != "":
					e.source = fields["tarball"]
				case fields["directory"] != "":
					e.source, e.local = fields["directory"], true
				case fields["repo"] != "":
					e.source = fields["repo"] + "@" + fields["commit"]
				}
			case "dependencies", "optionalDependencies":
				if value == "" {
					table = key
				}
			}
		case table != "":
			e := &entries[cur]
			if section == "packages" {
				e.end = l.end
			}
			name, ver := yarnField(l.text)
			kind := KindRuntime
			if table == "optionalDependencies" {
				kind = KindOptional
			}
			if i := strings.IndexByte(ver, '('); i > 0 {
				ver = ver[:i]
			}
			if pnpmBefore6(version) {
				// Before format 6 a peer suffix follows `_`.
				if i := strings.IndexByte(ver, '_'); i > 0 {
					ver = ver[:i]
				}
			}
			dep := lockDep{name: name, version: ver, kind: kind}
			if slices.Contains(e.deps, dep) {
				return nil
			}
			if strings.HasPrefix(ver, "link:") {
				dep.version, dep.requirement = "", ver
			} else if pnpmDepPath(ver, version) {
				// An alias: the value is the dependency path of the
				// package installed under the name.
				if target, v := pnpmSplit(ver, version); target != "" && target != name {
					dep.target, dep.version = target, v
				}
			}
			e.deps = append(e.deps, dep)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return u.lock(ctx, lockFile{ecosystem: ecosystemNPM, language: languageJS, version: version, entries: entries})
}

// pnpmSplit splits a package key into name and version: `/name/version`
// before format 6, `/name@version` or `name@version` after, dropping the
// peer suffix (`(peer@1)` from 6, `_peer@1` before). Before format 9 a
// path not beginning with `/` is absolute: its first component is the
// registry's host (a dotted name), which is no part of the name; a
// `file:` or `link:` path is no registry path and is kept whole.
func pnpmSplit(key, format string) (name, version string) {
	if pnpmMajor(format) < 9 && !strings.HasPrefix(key, "/") {
		if host, rest, ok := strings.Cut(key, "/"); ok && strings.Contains(host, ".") && !strings.Contains(host, ":") {
			key = rest
		}
	}
	key = strings.TrimPrefix(key, "/")
	if i := strings.IndexByte(key, '('); i > 0 {
		key = key[:i]
	}
	if pnpmBefore6(format) {
		if i := strings.LastIndexByte(key, '/'); i > 0 {
			name, version = key[:i], key[i+1:]
			if j := strings.IndexByte(version, '_'); j > 0 {
				version = version[:j]
			}
			return name, version
		}
	}
	return yarnSplit(key)
}

// pnpmFlow reads a one-line YAML flow mapping (`{integrity: sha512-...}`)
// into its keys and unquoted values.
func pnpmFlow(s string) map[string]string {
	out := map[string]string{}
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "{"), "}")
	for part := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(part, ":")
		if ok {
			out[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
		}
	}
	return out
}

// pnpmDepPath reports whether a dependency's value is a dependency path
// rather than a version (the peer suffix already removed): from format 9 a
// value beginning with `@` or holding an `@` before any `:`; before it a
// value holding a `/`. The value is then the path of the package installed
// under the dependency's name, which is another package's when the name is
// an alias.
func pnpmDepPath(v, format string) bool {
	if strings.HasPrefix(v, "link:") || strings.HasPrefix(v, "file:") {
		return false
	}
	if pnpmMajor(format) < 9 {
		return strings.Contains(v, "/")
	}
	if strings.HasPrefix(v, "@") {
		return true
	}
	at, colon := strings.IndexByte(v, '@'), strings.IndexByte(v, ':')
	return at > 0 && (colon < 0 || at < colon)
}

// pnpmBefore6 reports a lock format version before 6, which spells package
// keys `/name/version` and peer suffixes after `_`.
func pnpmBefore6(format string) bool {
	return pnpmMajor(format) < 6
}

// pnpmMajor is the major number of a lock format version, 9 (the current
// format's) when the file states none it can be read from.
func pnpmMajor(format string) int {
	major, _, _ := strings.Cut(format, ".")
	n, err := strconv.Atoi(strings.Trim(major, `'"`))
	if err != nil {
		return 9
	}
	return n
}
