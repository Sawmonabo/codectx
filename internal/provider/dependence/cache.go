package dependence

// The parsed-graph cache (Section 11.6). A graph is kept per unit under the
// private data directory and reused across generations, so unchanged code
// pays nothing on a refresh — the engine has no incremental mode, so a
// refreshed unit is otherwise a whole parse and export every time.
//
// The key is the complete semantic closure, never a path or a timestamp: the
// unit's source file hashes, its manifest and lock files, the pinned frontend
// argv including the definition cap and the project layout, the engine payload
// digest and the runtime payload digest. Any of these changing invalidates the
// entry. A cached graph that does not match the closure exactly is not reused,
// because a graph built by another engine release or another argument array is
// a different analysis with the same name.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
)

// maxCacheEntries bounds the directory scan retention performs, so a corrupted
// or hand-filled cache directory can never turn a refresh into an unbounded
// walk.
const maxCacheEntries = 4096

// cacheKeyDomain separates this hash from every other domain-separated hash in
// the product.
const cacheKeyDomain = "dependence-graph-cache"

// CacheKey is the semantic closure of one unit's parsed graph.
func CacheKey(ctx context.Context, view model.SnapshotView, u Unit, argv []string, e Engine) (string, error) {
	h := model.NewHasher(cacheKeyDomain)
	h.AddString(u.ScopeKey)
	h.AddString(string(u.Family))
	h.AddString(u.Root)
	for _, a := range argv {
		h.AddString(a)
	}
	for _, ex := range u.Excluded {
		h.AddString("exclude")
		h.AddString(ex)
	}
	h.AddString(e.Digest)
	h.AddString(e.RuntimeDigest)
	markers := slices.Clone(u.Markers)
	slices.Sort(markers)
	// The manifest is served in ascending path order, so folding it in stream
	// order is deterministic without retaining it.
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		source := FamilyOf(lang.Of(fv.Path)) == u.Family && u.Contains(fv.Path)
		if !source && !(slices.Contains(markers, fv.Path) && u.Contains(fv.Path)) {
			return nil
		}
		h.AddString(fv.Path)
		h.AddString(fv.ContentHash)
		return nil
	})
	if err != nil {
		return "", err
	}
	return h.Sum(), nil
}

// Cache is the byte-bounded store of parsed graphs under the data directory.
// A zero Budget disables caching entirely, which is what a user asking for no
// graph cache means; it is never an unbounded cache.
type Cache struct {
	dir    string
	budget int64
}

// OpenCache prepares the cache directory. Mode 0o700: a parsed graph carries
// the shape of private source and is never world-readable.
func OpenCache(dataDir string, budget int64) (*Cache, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, invalid("the dependence cache needs an absolute data directory")
	}
	if budget < 0 {
		return nil, invalid("the dependence cache budget may not be negative")
	}
	dir := filepath.Join(dataDir, "dependence", "graphs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, internalErr("the dependence cache directory could not be created: " + err.Error())
	}
	return &Cache{dir: dir, budget: budget}, nil
}

// path is the entry file for a key. The key is a hex digest, so it can never
// contain a separator.
func (c *Cache) path(key string) string { return filepath.Join(c.dir, key+".graph") }

// Lookup returns the cached graph's path and whether it is present. A present
// entry is touched so retention evicts the least recently used first.
func (c *Cache) Lookup(key string) (string, bool) {
	if c == nil || c.budget == 0 || !model.ValidHexID(key) {
		return "", false
	}
	p := c.path(key)
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return p, true
}

// Put moves a freshly produced graph into the cache and enforces the budget.
// A graph larger than the whole budget is not cached: it would evict
// everything else to hold one entry. Failing to cache is never an error the
// unit fails on — the graph was produced, and the only cost is the next
// refresh reparsing it — so Put reports the outcome and the caller logs it.
func (c *Cache) Put(key, graph string) bool {
	if c == nil || c.budget == 0 || !model.ValidHexID(key) {
		return false
	}
	info, err := os.Stat(graph)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > c.budget {
		return false
	}
	dst := c.path(key)
	if err := os.Rename(graph, dst); err != nil {
		return false
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		_ = os.Remove(dst)
		return false
	}
	c.evict(dst)
	return true
}

// entry is one cached graph's retention record.
type entry struct {
	path    string
	size    int64
	modTime int64
}

// evict removes least recently used entries until the directory is within the
// budget. keep is never evicted: it is the entry just written, and evicting it
// would make the cache silently useless under pressure.
func (c *Cache) evict(keep string) {
	dirents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	if len(dirents) > maxCacheEntries {
		dirents = dirents[:maxCacheEntries]
	}
	entries := make([]entry, 0, len(dirents))
	var total int64
	for _, d := range dirents {
		if !strings.HasSuffix(d.Name(), ".graph") {
			continue
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		entries = append(entries, entry{path: filepath.Join(c.dir, d.Name()), size: info.Size(), modTime: info.ModTime().UnixNano()})
	}
	if total <= c.budget {
		return
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.modTime != b.modTime {
			return int(a.modTime - b.modTime)
		}
		return strings.Compare(a.path, b.path)
	})
	for _, e := range entries {
		if total <= c.budget {
			return
		}
		if e.path == keep {
			continue
		}
		if err := os.Remove(e.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		total -= e.size
	}
}
