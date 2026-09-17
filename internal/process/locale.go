package process

// The locale every child of this product runs under.
//
// A child that inherits nothing also inherits no locale, and a C locale makes
// the platform's path encoding ASCII. A runtime that encodes a file name
// through that encoding then cannot name a source file holding a letter
// outside ASCII at all: measured against the pinned analysis payload, a
// JavaScript project holding one such file failed the whole unit with
// `java.nio.file.InvalidPathException: Malformed input or input contains
// unmappable characters`, and the same unit parsed and exported with the
// locale variables below present and nothing else changed.
//
// The locale is set here rather than by each caller because it is a property
// of every child this product starts, not of one analyzer: the precise
// indexers walk the same paths. Callers keep the last word — Spec.Env is
// appended after these, and a duplicate later in the environment is what a
// child's runtime reads — so a run that must pin a different locale can.
//
// It is not a setting and cannot be one: a host locale would decide which of
// a repository's files can be analysed, which is the observation this
// constant exists to remove.

import "runtime"

// utf8Locale is the locale name the child is given. It must be a locale the
// platform's runtime actually has: an unknown name leaves the C locale in
// place, which was measured to reproduce the failure exactly. "C.UTF-8" is
// the locale-independent UTF-8 locale, and it is the measured value: the
// failing unit above was reproduced and then passed under it on linux.
//
// The darwin value is not a measurement and is not presented as one. That
// platform does not carry "C.UTF-8", so naming it there would leave the C
// locale in place; "en_US.UTF-8" is chosen because it is the UTF-8 locale
// that platform does carry. No run on darwin has been observed here, so the
// value stands unverified on it until a run on one measures it.
//
// On windows the variables are inert — that runtime names files through the
// wide character interface and never encodes a path — and are set for the
// non-windows tools such a host may still run.
func utf8Locale() string {
	if runtime.GOOS == "darwin" {
		return "en_US.UTF-8"
	}
	return "C.UTF-8"
}

// localeEnv is the locale part of every child environment. Both variables are
// set: LC_ALL is what overrides a per-category locale, and LANG is what a tool
// that reads only that variable sees.
func localeEnv() []string {
	l := utf8Locale()
	return []string{"LANG=" + l, "LC_ALL=" + l}
}
