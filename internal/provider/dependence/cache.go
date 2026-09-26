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
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
)

// cacheDirBatch is how many directory entries one read of the cache
// directory requests. It bounds the memory of a scan, not its reach: every
// scan reads until the directory is exhausted, so no entry is ever out of
// retention's sight.
const cacheDirBatch = 256

// cacheKeyDomain separates this hash from every other domain-separated hash in
// the product.
const cacheKeyDomain = "dependence-graph-cache"

// CacheKey is the semantic closure of one unit's parsed graph.
//
// providerVersion and analysisConfigHash are the two components the unit's own
// identity is derived from that nothing else here carries (model.NewUnitID).
// Without them a provider upgrade or an analysis-configuration edit rebuilds
// the unit and the rebuild reuses a graph produced under the version and the
// configuration it replaced -- a different analysis published under the new
// unit's name. The cost is the other direction of the same rule: a
// configuration edit that cannot change what the parse produces still reparses.
// A reuse boundary that disagrees with unit identity is the worse of the two.
func CacheKey(ctx context.Context, view model.SnapshotView, u Unit, argv []string, e Engine,
	providerVersion, analysisConfigHash string) (string, error) {

	h := model.NewHasher(cacheKeyDomain)
	h.AddString(providerVersion)
	h.AddString(analysisConfigHash)
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
	// The manifest is served in ascending path order, so folding it in stream
	// order is deterministic without retaining it. A tombstone is skipped
	// before membership is asked: the key folds live content hashes, and a
	// deleted file has none. The unit is still invalidated by the deletion,
	// because the file's hash leaves the fold.
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		if !u.OwnsInput(fv.Path) {
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
		_ = paced.Remove(dst)
		return false
	}
	c.evict(dst)
	return true
}

// Drop removes an entry the caller has proved useless -- a graph whose export
// the engine cannot complete, or one that exports no method at all. Such an
// entry is not merely stale: Lookup touches what it returns, so it would stay
// the most recently used graph in the directory and hold its bytes inside the
// budget for as long as the unit is refreshed, while every refresh skipped the
// parse and re-paid the dead export. An entry that is not there is not an
// error, and a removal that fails costs only the space.
func (c *Cache) Drop(key string) {
	if c == nil || c.budget == 0 || !model.ValidHexID(key) {
		return
	}
	if err := paced.Remove(c.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("a dependence graph proved dead was not removed from the cache",
			"component", component, "error", err)
	}
}

// entry is one cached graph's retention record.
type entry struct {
	path    string
	size    int64
	modTime int64
}

// older orders entries least recently used first. cmp.Compare, not
// int(a.modTime-b.modTime): the difference of two nanosecond timestamps
// overflows a 32-bit int, which would make the order arbitrary on a 32-bit
// build and let eviction drop the most recently used graph.
func older(a, b entry) int {
	if a.modTime != b.modTime {
		return cmp.Compare(a.modTime, b.modTime)
	}
	return strings.Compare(a.path, b.path)
}

// scan streams every cached graph in the directory to fn, a bounded batch at
// a time, so a directory of any size is read whole without being held.
func (c *Cache) scan(fn func(entry)) error {
	d, err := os.Open(c.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	for {
		batch, err := d.ReadDir(cacheDirBatch)
		for _, de := range batch {
			if !strings.HasSuffix(de.Name(), ".graph") {
				continue
			}
			info, err := de.Info()
			if err != nil || !info.Mode().IsRegular() {
				// Removed since the read listed it, or not a graph at all.
				continue
			}
			fn(entry{path: filepath.Join(c.dir, de.Name()), size: info.Size(), modTime: info.ModTime().UnixNano()})
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// evict removes least recently used entries until the directory is within the
// budget. keep is never evicted: it is the entry just written, and evicting it
// would make the cache silently useless under pressure.
//
// Two streaming passes, so memory never grows with the number of entries. The
// first sums the directory. The second keeps only the oldest entries whose
// sizes cover the excess: each entry is added to a set ordered newest first,
// and the newest is dropped from the set while the rest still cover it, so
// what is held is the set eviction removes and nothing else. A directory that
// cannot be read, or that is still over the budget once eviction has removed
// what it could, is reported: the cache is then holding more than the
// configured bytes, and the operator is told rather than the disk filling
// quietly.
func (c *Cache) evict(keep string) {
	var total int64
	if err := c.scan(func(e entry) { total += e.size }); err != nil {
		c.overBudget("the dependence graph cache directory could not be read for retention", total, err)
		return
	}
	if total <= c.budget {
		return
	}
	excess := total - c.budget
	var victims []entry
	var held int64
	err := c.scan(func(e entry) {
		if e.path == keep {
			return
		}
		i, _ := slices.BinarySearchFunc(victims, e, older)
		victims = slices.Insert(victims, i, e)
		held += e.size
		for len(victims) > 1 && held-victims[len(victims)-1].size >= excess {
			held -= victims[len(victims)-1].size
			victims = victims[:len(victims)-1]
		}
	})
	if err != nil {
		c.overBudget("the dependence graph cache directory could not be read for retention", total, err)
		return
	}
	for _, e := range victims {
		if total <= c.budget {
			break
		}
		if err := paced.Remove(e.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		total -= e.size
	}
	if total > c.budget {
		c.overBudget("the dependence graph cache is over its byte budget after retention", total, nil)
	}
}

// overBudget reports a cache that holds, or may hold, more than its budget.
func (c *Cache) overBudget(msg string, total int64, err error) {
	args := []any{"component", component, "cache_bytes", c.budget, "held_bytes", total}
	if err != nil {
		args = append(args, "error", err)
	}
	slog.Warn(msg, args...)
}
