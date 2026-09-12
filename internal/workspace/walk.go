package workspace

import (
	"cmp"
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Traversal bounds. Section 6 requires an explicit finite bound on every
// traversal; these are the structural ones, independent of the configured
// file budget.
const (
	// maxDirEntries bounds one directory listing. A directory larger than this
	// is reported rather than silently truncated.
	maxDirEntries = 200000
	// maxWalkDepth bounds nesting, which also bounds the ancestor set the
	// symlink cycle check compares against.
	maxWalkDepth = 128
	// cancelCheckInterval is how often the walk polls cancellation while
	// emitting files from one directory.
	cancelCheckInterval = 256
)

// vendorDirs are the directory names excluded when index_vendor is false. The
// list is deliberately small, fixed and documented in docs/configuration.md:
// it is the definition of what that setting means, so it may not drift into an
// undocumented heuristic.
var vendorDirs = map[string]bool{
	"vendor":           true,
	"node_modules":     true,
	"third_party":      true,
	"bower_components": true,
	"Godeps":           true,
}

// generatedDirs and generatedSuffixes are the built-in classification used when
// index_generated is false. Classification is by path only: no file is opened
// to guess at its provenance, because opening every candidate to sniff for a
// generated-code banner would read the whole repository before capture.
var generatedDirs = map[string]bool{
	"__pycache__": true,
	".next":       true,
}

var generatedSuffixes = []string{
	".pb.go",
	".pb.cc",
	".pb.h",
	"_pb2.py",
	"_pb2_grpc.py",
	".generated.go",
	"_generated.go",
	".g.dart",
	".freezed.dart",
	".min.js",
	".min.css",
}

// Policy is the traversal policy for one walk. The toggles come from
// configuration; the two hooks come from the Git owner, which is the only
// component that knows what the repository tracks and ignores.
type Policy struct {
	// FollowSymlinks opts into in-root symlink traversal. Even then a symlink
	// that resolves outside the workspace is rejected, not skipped.
	FollowSymlinks bool
	IndexVendor    bool
	IndexGenerated bool
	// MaxFiles bounds the number of emitted files. Exceeding it is a typed
	// resource limit, never a silently truncated result.
	MaxFiles int64
	// DataDir is the absolute data directory. When it lies inside the
	// workspace it is excluded unconditionally, along with ".git": neither is
	// repository source, and indexing the store while writing it would be a
	// feedback loop.
	DataDir string
	// Ignore reports whether a path is ignored. It is supplied by the Git
	// owner, which evaluates real ignore rules through Git plumbing; this
	// package deliberately contains no ignore-pattern parser.
	Ignore func(rel string, isDir bool) bool
	// ForceInclude reports whether a path must be emitted regardless of Ignore
	// and of the vendor and generated toggles. Section 10.2 requires tracked
	// files to win over an ignore match. It cannot override the unconditional
	// exclusions or any safety rule: neither .git, nor the data directory, nor
	// a symlink escape, nor the file budget is negotiable.
	ForceInclude func(rel string) bool
	// ForceIncludeDir answers the same question for a whole directory: may
	// anything beneath relDir be forced back in? An excluded directory is read
	// only when this says yes, so "vendor" with index_vendor disabled is not
	// traversed at all unless a tracked path actually lies inside it. Without
	// it, a single ForceInclude hook would turn every excluded directory into a
	// full traversal, and a huge ignored directory could abort the walk on the
	// entry cap alone.
	ForceIncludeDir func(relDir string) bool
	// IncludeUntracked is carried for the Git owner, which decides from it
	// whether an untracked file is eligible. Walk does not interpret it: it
	// cannot know what Git tracks, and guessing would contradict the snapshot.
	IncludeUntracked bool
}

// Walk streams every eligible file in deterministic order, calling visit once
// per file. It retains no repository-wide file list: only the entries of the
// directories on the current path are held at any moment.
//
// Order is the lexicographic order of the root-relative paths themselves. That
// is not the same as sorting each directory's entry names: "a.txt" precedes
// "a/b" because "." sorts before "/", while the names "a" and "a.txt" sort the
// other way. Each level is therefore sorted by the name a directory contributes
// to a path, which is its name followed by the separator.
func Walk(ctx context.Context, root Root, policy Policy, visit func(File) error) error {
	if root.root == nil {
		return notFound("the workspace is not open")
	}
	if policy.MaxFiles <= 0 {
		return resourceLimit("the walk file budget is %d; no zero or negative bound means unlimited", policy.MaxFiles)
	}
	w := &walker{root: root, policy: policy, visit: visit, dataDirRel: dataDirRelative(root.Path, policy.DataDir)}
	if w.dataDirRel == "." {
		return resourceLimit("the data directory is the workspace root; no file would be eligible")
	}
	return w.walkDir(ctx, ".", nil, false)
}

type walker struct {
	root       Root
	policy     Policy
	visit      func(File) error
	dataDirRel string
	emitted    int64
	seen       int
	// ancestors holds the directories on the current path so a followed symlink
	// that re-enters one is detected instead of looping forever.
	ancestors []fs.FileInfo
}

// entry is one directory child with the ordering key its path contributes.
type entry struct {
	name    string
	sortKey string
	isDir   bool
	info    fs.FileInfo
}

// walkDir emits one directory level. excluded marks a subtree that policy
// excluded but that is still traversed because a forced include may live in it.
func (w *walker) walkDir(ctx context.Context, rel string, dirInfo fs.FileInfo, excluded bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(w.ancestors) >= maxWalkDepth {
		return resourceLimit("%q is nested deeper than %d directories", rel, maxWalkDepth)
	}
	if dirInfo != nil {
		for _, a := range w.ancestors {
			if os.SameFile(a, dirInfo) {
				return pathError("%q re-enters a directory already on the path; the workspace contains a symlink cycle", rel)
			}
		}
		w.ancestors = append(w.ancestors, dirInfo)
		defer func() { w.ancestors = w.ancestors[:len(w.ancestors)-1] }()
	}

	entries, err := w.readDir(rel)
	if err != nil {
		return err
	}
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(a.sortKey, b.sortKey) })

	for _, e := range entries {
		childRel := e.name
		if rel != "." {
			childRel = rel + "/" + e.name
		}
		if err := w.visitEntry(ctx, childRel, e, excluded); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) visitEntry(ctx context.Context, rel string, e entry, excluded bool) error {
	if e.name == gitDirName || rel == w.dataDirRel {
		// Unconditional: not source, and not subject to any include hook.
		return nil
	}
	if e.isDir {
		childExcluded := excluded || w.dirExcluded(rel, e.name)
		if childExcluded && !w.mayForceInside(rel) {
			// Nothing inside can be forced back in, so the subtree is not read
			// at all: not listed, not counted against the entry cap, not a
			// source of errors.
			return nil
		}
		return w.walkDir(ctx, rel, e.info, childExcluded)
	}
	forced := w.policy.ForceInclude != nil && w.policy.ForceInclude(rel)
	if !e.info.Mode().IsRegular() {
		// Devices, sockets, pipes and unresolved links are not source bytes.
		return nil
	}
	if !forced && (excluded || w.fileExcluded(rel)) {
		return nil
	}
	w.seen++
	if w.seen%cancelCheckInterval == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	w.emitted++
	if w.emitted > w.policy.MaxFiles {
		return resourceLimit("the workspace holds more than the configured %d files", w.policy.MaxFiles)
	}
	return w.visit(File{
		Path:    rel,
		Size:    e.info.Size(),
		Mode:    e.info.Mode(),
		ModTime: e.info.ModTime().UnixNano(),
	})
}

// mayForceInside reports whether the policy claims some path beneath an
// excluded directory must still be visited.
func (w *walker) mayForceInside(relDir string) bool {
	return w.policy.ForceIncludeDir != nil && w.policy.ForceIncludeDir(relDir)
}

func (w *walker) dirExcluded(rel, name string) bool {
	if !w.policy.IndexVendor && vendorDirs[name] {
		return true
	}
	if !w.policy.IndexGenerated && generatedDirs[name] {
		return true
	}
	return w.policy.Ignore != nil && w.policy.Ignore(rel, true)
}

func (w *walker) fileExcluded(rel string) bool {
	if !w.policy.IndexGenerated && isGeneratedPath(rel) {
		return true
	}
	return w.policy.Ignore != nil && w.policy.Ignore(rel, false)
}

func isGeneratedPath(rel string) bool {
	base := path.Base(rel)
	for _, suffix := range generatedSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// readDir lists one directory through the confined handle and resolves each
// child's kind. A symlink is skipped unless the policy opts into following one,
// and a followed symlink that leaves the workspace fails the walk.
func (w *walker) readDir(rel string) ([]entry, error) {
	native := "."
	if rel != "." {
		native = filepath.FromSlash(rel)
	}
	f, err := w.root.root.Open(native)
	if err != nil {
		return nil, pathError("%q cannot be listed: %v", rel, err)
	}
	defer f.Close()
	names, err := f.ReadDir(maxDirEntries + 1)
	if err != nil {
		return nil, pathError("%q cannot be listed: %v", rel, err)
	}
	if len(names) > maxDirEntries {
		return nil, resourceLimit("%q holds more than %d entries", rel, maxDirEntries)
	}

	out := make([]entry, 0, len(names))
	for _, d := range names {
		childRel := d.Name()
		if rel != "." {
			childRel = rel + "/" + d.Name()
		}
		if len(childRel) > model.MaxPathBytes {
			return nil, pathError("path %q is longer than the %d-byte limit", childRel, model.MaxPathBytes)
		}
		info, err := w.resolve(childRel, d)
		if err != nil {
			return nil, err
		}
		if info == nil {
			continue
		}
		key := d.Name()
		if info.IsDir() {
			key += "/"
		}
		out = append(out, entry{name: d.Name(), sortKey: key, isDir: info.IsDir(), info: info})
	}
	return out, nil
}

// resolve returns the metadata the walk should use for one child, or nil when
// the child is skipped. A nil result is never an error the caller should hide:
// it means the entry is not eligible source, such as a symlink under the
// default policy.
func (w *walker) resolve(rel string, d fs.DirEntry) (fs.FileInfo, error) {
	if d.Type()&fs.ModeSymlink == 0 {
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				// The entry disappeared between the listing and the stat.
				// Capture reconciliation detects that; the walk does not.
				return nil, nil
			}
			return nil, pathError("%q cannot be inspected: %v", rel, err)
		}
		return info, nil
	}
	if !w.policy.FollowSymlinks {
		return nil, nil
	}
	// Stat through the confined handle: it resolves the link only within the
	// workspace, so a target outside it fails here rather than being read.
	info, err := w.root.root.Stat(filepath.FromSlash(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, pathError("%q is a symbolic link that does not resolve inside the workspace: %v", rel, err)
	}
	return info, nil
}

// dataDirRelative returns the data directory's root-relative path when it lies
// inside the workspace, or the empty string when it does not.
func dataDirRelative(rootPath, dataDir string) string {
	if dataDir == "" {
		return ""
	}
	rel, err := filepath.Rel(rootPath, dataDir)
	if err != nil {
		return ""
	}
	slashed := filepath.ToSlash(rel)
	if slashed == ".." || strings.HasPrefix(slashed, "../") {
		return ""
	}
	return slashed
}

// ExclusionDigest is a stable digest of the built-in vendor and generated
// classification lists.
//
// These lists decide which files exist as far as the rest of the system is
// concerned, exactly like the configured toggles that switch them on and off.
// A build that ships a different list therefore produces a different capture
// from the same bytes and the same configuration, and the source fingerprint
// has to say so; otherwise a stored snapshot would be reused under a policy it
// was not captured under.
func ExclusionDigest() string {
	h := model.NewHasher(domainExclusions)
	for _, set := range []map[string]bool{vendorDirs, generatedDirs} {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		slices.Sort(names)
		h.AddString(strconv.Itoa(len(names)))
		for _, name := range names {
			h.AddString(name)
		}
	}
	suffixes := append([]string(nil), generatedSuffixes...)
	slices.Sort(suffixes)
	h.AddString(strconv.Itoa(len(suffixes)))
	for _, suffix := range suffixes {
		h.AddString(suffix)
	}
	return h.Sum()
}

// domainExclusions keeps the digest in its own hash domain, so it can never
// collide with another fingerprint over the same strings.
const domainExclusions = "workspace-exclusions-v1"
