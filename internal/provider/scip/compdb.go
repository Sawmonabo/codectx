package scip

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// compileCommandsName is the compilation database the C/C++ profile reads.
const compileCommandsName = "compile_commands.json"

// normalizeCompileCommands rewrites the `directory` field of every entry of
// the compilation database **inside the private materialization** so it names
// a directory of that copy rather than the tree the database was generated in.
//
// Why this exists. A compilation database records absolute paths: the fixture
// measured here carries `"directory": "<original project>"`. The provider runs
// the indexer against a private copy of the pinned snapshot, where that path
// does not exist, and scip-clang's indexing worker then crashes with a stack
// trace and the driver keeps waiting — so the unit burns its whole timeout and
// reports nothing useful. Rewriting the private copy is the only way a C or
// C++ index can describe the pinned bytes at all.
//
// What it does not do. It never touches the repository: the file it opens is
// inside the materialization the provider created. It never invents a
// directory: entries are moved by their common prefix, so a database whose
// entries live in several subdirectories keeps that shape. A database that is
// absent, unreadable, not the expected JSON array, or whose entries are
// already relative is left exactly as it is — the run then fails or
// succeeds on the tool's own terms rather than on a guess made here.
//
// There is no bound on the number of entries. The file is parsed whole, so
// what bounds the work is bytes, and that is providers.scip.max_manifest_bytes
// — unlimited by default, so no repository's database is skipped unless an
// operator asks for it, and a database the operator's own bound excludes is
// REPORTED rather than silently left unnormalized. That distinction is the
// whole cost of the skip: an un-normalized database makes the indexer's worker
// crash on paths that do not exist in the private copy while its driver waits,
// so the unit burns its whole timeout for nothing.
func normalizeCompileCommands(matRoot string, maxBytes config.Limit, seen *limitSeen) error {
	path := filepath.Join(matRoot, compileCommandsName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if maxBytes.Exceeded(info.Size()) {
		seen.note(limitManifestBytes, info.Size())
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) == 0 {
		return nil
	}
	dirs := make([]string, len(entries))
	for i, e := range entries {
		raw, ok := e["directory"]
		if !ok {
			return nil
		}
		var dir string
		if err := json.Unmarshal(raw, &dir); err != nil {
			return nil
		}
		if !filepath.IsAbs(dir) {
			// A relative directory is already resolved against the database's
			// own location, which is the materialization root. Nothing to do.
			return nil
		}
		dirs[i] = filepath.Clean(dir)
	}
	prefix := commonDirPrefix(dirs)
	if prefix == "" {
		return nil
	}
	for i, e := range entries {
		rel, err := filepath.Rel(prefix, dirs[i])
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
		moved, err := json.Marshal(filepath.Join(matRoot, rel))
		if err != nil {
			return nil
		}
		e["directory"] = moved
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return nil
	}
	// The materialization is the provider's own private tree; writing the
	// normalized database back into it is a write to a copy, never to a source.
	if err := writeFileInPlace(path, out, info.Mode().Perm()); err != nil {
		return &model.Error{Code: model.CodeInternal,
			Message: "the private compilation database could not be normalized: " + err.Error()}
	}
	return nil
}

// commonDirPrefix is the longest directory prefix shared by every path, or the
// empty string when they share nothing but the root.
func commonDirPrefix(dirs []string) string {
	prefix := dirs[0]
	for _, d := range dirs[1:] {
		for prefix != string(filepath.Separator) && prefix != "" {
			if d == prefix || strings.HasPrefix(d, prefix+string(filepath.Separator)) {
				break
			}
			prefix = filepath.Dir(prefix)
		}
	}
	if prefix == string(filepath.Separator) || prefix == "." {
		return ""
	}
	return prefix
}

// writeFileInPlace replaces one file's contents, keeping its mode.
func writeFileInPlace(path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
