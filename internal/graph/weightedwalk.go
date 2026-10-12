package graph

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// The weighted walk: ONE walk from roots that each carry a weight, in which
// every node keeps the labels the roots lend it rather than the first route
// that reached it.
//
// A label is what one route from one root lends the node it ends at: the
// root's weight and class (named or not), the route's depth, the route
// factor -- the product over its hops of the rule's per-kind hop factor,
// decayed once per hop after the first, the arithmetic a ranking pass scores
// a route by -- and the score, the root's weight times that factor. Because a
// heavier root farther away can lend a node more than a lighter one next to
// it, the walk is BEST-FIRST on the score, never breadth-first by depth, and a
// node may hold a label from a farther root alongside, or instead of, one from
// a nearer root.
//
// What a node keeps is every label no other label of it DOMINATES: label A
// dominates B when A's depth class is no deeper, A's class is at least as
// strong (named over unnamed), A's weight and route factor are both at least
// B's, and A may still walk at least as far as B (its remaining derived depth).
// Each of those is a quantity a ranking pass can only improve with -- a
// shallower depth is a requirement no weaker, a larger weight scales every
// boost no smaller, a larger factor scores no lower -- so the label a ranking
// pass would choose for the node, whatever boosts it adds afterwards, is among
// the kept ones, and so is every label that leads to one. The caller ranks the
// kept labels and keeps each node's best. Depth past the rule's class count is
// one class: a requirement demoted by distance stops changing there.
//
// The search is a label-setting one over an external-memory state (the same
// scratch database the shortest-path search keeps, pathscratch.go), so heap
// holds one batch and the database's fixed page cache, never the reachable
// set. Labels are settled in BATCHES of descending score: after every settled
// label is expanded, any label not yet created descends from a queued one and
// scores below the batch bound (batchBound), so every queued label above that
// bound is final the moment it is read, and the batch is settled in score
// order, each label checked against the ones settled before it.

// WeightedRoot is one root of a weighted walk.
type WeightedRoot struct {
	Node model.NodeID `json:"node"`
	// Weight scales what the root lends: a label's score is Weight times its
	// route factor. It is at most the rule's Scale.
	Weight int64 `json:"weight"`
	// Named marks a root the caller ranks as named; a named label dominates
	// an unnamed one of the same depth, weight and factor, never the reverse.
	Named bool `json:"named,omitempty"`
	// MaxDepth is the root's own derived depth bound, zero for none. A route
	// that reaches it ends there silently: it is the caller's derived bound,
	// not a cut it discloses. The request's MaxDepth applies to every root and
	// is disclosed when it ends a route.
	MaxDepth int `json:"max_depth,omitempty"`
}

// LabelRule is the arithmetic a label is computed by. It is the caller's: the
// walk applies it and knows nothing of what the numbers mean.
type LabelRule struct {
	// Scale is the fixed-point one every factor and weight is expressed in.
	Scale int64
	// Hop is each walked relation kind's per-hop factor, at most Scale. Every
	// requested kind must carry one.
	Hop map[model.RelationKind]int64
	// Decay multiplies a route once per hop after its first, at most Scale.
	Decay int64
	// Classes is the number of depth classes: depths at or past it are one.
	Classes int
	// Policy names the caller's rule. It is bound into the continuation, so a
	// cursor minted under one rule is refused under another.
	Policy string
}

// WeightedWalkRequest is one page of a weighted walk.
type WeightedWalkRequest struct {
	Roots      []WeightedRoot
	Relations  []model.RelationKind
	Direction  model.Direction
	MaxDepth   int
	MaxVisited int
	MaxEdges   int
	Rule       LabelRule
	Page       model.PageRequest
}

// WeightedEntry is one kept label: the node it ends at, hydrated as an impact
// entry is, with the route that lends it, and the weight and class of the
// root the route starts at. A node appears once per kept label.
type WeightedEntry struct {
	model.ImpactEntry
	RootWeight int64
	Named      bool
}

// WeightedWalkResult is one page of kept labels.
type WeightedWalkResult struct {
	Meta    model.QueryMeta
	Entries []WeightedEntry
	// VisitedCount and EdgeCount are the walk's cumulative spend: the distinct
	// nodes it settled a label at, and the adjacency entries it read.
	VisitedCount int64
	EdgeCount    int64
}

// weightedEndpoint is what a weighted walk's continuation is bound to.
const weightedEndpoint = "graph.weighted"

// reachUnbounded is the remaining derived depth of a label whose root carries
// no derived bound. It is larger than any depth a walk can reach, so the
// dominance test's "may walk at least as far" is one integer comparison.
const reachUnbounded = math.MaxInt32

// weightedSchema is the walk's state. label holds every label the walk has
// created: state 0 is queued, 1 settled and owed its expansion, 2 settled and
// expanded. A queued label is unique on its key (node, class, named, weight,
// reach), so a better route to the same key replaces it in place; a settled
// one is never touched again. progress is the resume position and the
// answer-level disclosures, which must survive the page that met them.
const weightedSchema = `
CREATE TABLE label(id INTEGER PRIMARY KEY, node TEXT NOT NULL, depth INTEGER NOT NULL, cls INTEGER NOT NULL,
	named INTEGER NOT NULL, weight INTEGER NOT NULL, reach INTEGER NOT NULL, route INTEGER NOT NULL,
	score INTEGER NOT NULL, parent INTEGER NOT NULL, rel TEXT NOT NULL, kind TEXT NOT NULL,
	outgoing INTEGER NOT NULL, state INTEGER NOT NULL);
CREATE UNIQUE INDEX label_queued ON label(node, cls, named, weight, reach) WHERE state = 0;
CREATE INDEX label_queue ON label(state, score DESC, depth, node, named DESC, weight DESC, reach DESC, route DESC, id);
CREATE INDEX label_pending ON label(state, node, id);
CREATE INDEX label_node ON label(node, state, cls);
CREATE INDEX label_serve ON label(node, cls, named, weight, reach) WHERE state > 0 AND depth > 0;
CREATE TABLE progress(k INTEGER PRIMARY KEY, expand_after TEXT NOT NULL, depth_limited INTEGER NOT NULL,
	edges_hit INTEGER NOT NULL, visited_hit INTEGER NOT NULL, serving INTEGER NOT NULL,
	after_node TEXT NOT NULL, after_cls INTEGER NOT NULL, after_named INTEGER NOT NULL,
	after_weight INTEGER NOT NULL, after_reach INTEGER NOT NULL);
INSERT INTO progress VALUES(0, '', 0, 0, 0, 0, '', 0, 0, 0, 0);
`

// WeightedWalk answers one page of a weighted walk. The request that mints the
// answer runs the walk to completion -- or to the deadline, which mints a
// continuation of the walk itself -- and then serves the kept labels a page at
// a time behind a continuation of the listing. Every page of one answer repeats
// the request byte for byte apart from the cursor.
func (e *Engine) WeightedWalk(ctx context.Context, req WeightedWalkRequest) (res WeightedWalkResult, err error) {
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.validate(); err != nil {
		return WeightedWalkResult{}, err
	}
	ctx, deadline, done, err := beginImpactQuery(ctx, e)
	if err != nil {
		return WeightedWalkResult{}, err
	}
	defer done()

	maxDepth, depthNotice := resolveLimit("max_depth", req.MaxDepth, e.limits.Depth())
	maxVisited, visitedNotice := resolveLimit("max_visited", req.MaxVisited, e.limits.Visited())
	maxEdges, edgeNotice := resolveLimit("max_edges", req.MaxEdges, e.limits.Edges())
	limit := resolveBound(req.Page.Limit, e.limits.MaxPageItems)
	queryHash := weightedQueryHash(req, maxDepth.Int(), limit)

	caps, err := e.completeness(ctx)
	if err != nil {
		return WeightedWalkResult{}, err
	}
	res.Meta = model.QueryMeta{Binding: e.adjacency.Binding(), Completeness: caps,
		Notices: appendNotice(appendNotice(appendNotice(nil, depthNotice), visitedNotice), edgeNotice)}

	var resume *weightedResume
	// stalled is a resumed page that met the deadline before it moved the
	// walk: it ends with no continuation of its own, and the cursor the caller
	// holds stays adoptable, which is what the reason's remedy relies on.
	stalled := false
	if req.Page.Cursor != "" {
		if resume, err = e.resumeWeighted(ctx, req.Page.Cursor, queryHash); err != nil {
			return WeightedWalkResult{}, err
		}
		defer func() {
			if terminalOutcome(err) && !stalled {
				resume.Release()
			}
		}()
	}
	scCtx := context.WithoutCancel(ctx)
	var sc *pathScratch
	if resume != nil {
		sc, err = reopenPathScratch(scCtx, resume.Dir)
	} else {
		sc, err = openScratch(scCtx, e.scratchDir(), weightedSchema)
	}
	if err != nil {
		return WeightedWalkResult{}, err
	}
	// The shortest-path search's exit rule (path.go): a resumed page that
	// fails retryably, or stalls, rolls its writes back and leaves the
	// directory as the cursor names it; every other exit gives the state back.
	defer func() {
		if resume != nil && (stalled || !terminalOutcome(err)) {
			if rerr := sc.retain(); rerr != nil {
				err = terminalRetention(errors.Join(err, rerr))
			} else {
				return
			}
		}
		sc.close()
	}()

	codes, absent := kindCodesFor(e.reader, req.Relations)
	w := &weightedWalk{
		reader: e.reader, rule: req.Rule, codes: codes, absent: absent,
		direction: req.Direction, maxDepth: maxDepth, maxVisited: maxVisited, maxEdges: maxEdges,
		frontierBytes: e.limits.FrontierBytes, sc: sc, scCtx: scCtx, deadline: deadline, now: e.now,
	}
	for _, k := range req.Relations {
		w.maxHop = max(w.maxHop, req.Rule.Hop[k])
	}
	if resume != nil {
		w.visited, w.edges = resume.Cursor.Visited, resume.Cursor.Edges
		if err := w.loadProgress(); err != nil {
			return WeightedWalkResult{}, err
		}
	} else if err := w.seed(req.Roots); err != nil {
		return WeightedWalkResult{}, err
	}

	if !w.serving {
		if err := w.run(ctx); err != nil {
			return WeightedWalkResult{}, err
		}
		if w.deadlineHit {
			res.VisitedCount, res.EdgeCount = w.visited, w.edges
			if resume != nil && w.pageEdges == 0 && w.pageSettled == 0 {
				stalled = true
				markTruncated(&res.Meta, reasonDeadlineStalled)
				return res, nil
			}
			markTruncated(&res.Meta, reasonDeadline)
			if err := w.saveProgress(); err != nil {
				return WeightedWalkResult{}, err
			}
			res.Meta.NextCursor, err = e.nextWeightedCursor(ctx, sc, queryHash, w)
			return res, err
		}
		w.serving = true
	}
	// The walk is over. Its answer-level stops are reported on every page of
	// the listing, as an impact answer's are.
	switch {
	case w.edgesHit:
		markTruncated(&res.Meta, reasonEdgeBudget)
	case w.visitedHit:
		markTruncated(&res.Meta, reasonVisitedBudget)
	case w.depthLimited:
		markTruncated(&res.Meta, reasonDepth)
	}
	page, more, err := w.page(limit)
	if err != nil {
		return WeightedWalkResult{}, err
	}
	if res.Entries, err = e.weightedEntries(ctx, w, page, &res.Meta); err != nil {
		return WeightedWalkResult{}, err
	}
	res.VisitedCount, res.EdgeCount = w.visited, w.edges
	if !more {
		return res, nil
	}
	if err := w.saveProgress(); err != nil {
		return WeightedWalkResult{}, err
	}
	if res.Meta.NextCursor, err = e.nextWeightedCursor(ctx, sc, queryHash, w); err != nil {
		return WeightedWalkResult{}, err
	}
	if res.Meta.NextCursor == "" {
		markTruncated(&res.Meta, reasonNoContinuation)
	}
	return res, nil
}

// validate screens the request with the impact request's own contract for the
// fields the two share, and the rule's arithmetic for the rest: a factor or a
// weight above Scale would let a longer route score higher than its prefix,
// which is the one property the batch bound rests on.
func (r WeightedWalkRequest) validate() error {
	start := make([]model.NodeID, len(r.Roots))
	for i, root := range r.Roots {
		start[i] = root.Node
	}
	if err := (model.ImpactRequest{Start: start, Relations: r.Relations, Direction: r.Direction,
		MaxDepth: r.MaxDepth, MaxVisited: r.MaxVisited, MaxEdges: r.MaxEdges, Page: r.Page}).Validate(); err != nil {
		return err
	}
	rule := r.Rule
	switch {
	case len(r.Relations) == 0:
		return argumentInvalid("a weighted walk names the relation kinds it walks")
	case rule.Scale <= 0:
		return argumentInvalid("a weighted walk's rule needs a positive scale")
	case rule.Decay < 0 || rule.Decay > rule.Scale:
		return argumentInvalid("a weighted walk's decay must lie between zero and its scale")
	case rule.Classes <= 0:
		return argumentInvalid("a weighted walk's rule needs at least one depth class")
	}
	for _, k := range r.Relations {
		if k == model.RelMayCall {
			return argumentInvalid("a weighted walk crosses precise relations only; may_call is not one")
		}
		hop, ok := rule.Hop[k]
		if !ok || hop < 0 || hop > rule.Scale {
			return argumentInvalid(fmt.Sprintf("a weighted walk's rule carries no factor between zero and its scale for %s", k))
		}
	}
	for _, root := range r.Roots {
		if root.Weight < 0 || root.Weight > rule.Scale || root.MaxDepth < 0 {
			return argumentInvalid(fmt.Sprintf("weighted root %s carries a weight outside zero to the scale, or a negative depth bound", root.Node))
		}
	}
	return nil
}

// argumentInvalid is the typed refusal of a malformed weighted request.
func argumentInvalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}

// weightedQueryHash binds a continuation to the whole normalized request: the
// roots in their given order with everything each carries, the kinds, the
// direction, the depth bound, the page limit and the rule.
func weightedQueryHash(r WeightedWalkRequest, maxDepth, limit int) string {
	kinds := make([]string, len(r.Relations))
	for i, k := range r.Relations {
		kinds[i] = string(k)
	}
	sort.Strings(kinds)
	h := model.NewHasher(queryHashDomain)
	h.AddString(weightedEndpoint)
	h.AddString(string(r.Direction))
	h.AddString(strconv.Itoa(maxDepth))
	h.AddString(strconv.Itoa(limit))
	h.AddString(r.Rule.Policy)
	h.AddString(strconv.FormatInt(r.Rule.Scale, 10))
	h.AddString(strconv.FormatInt(r.Rule.Decay, 10))
	h.AddString(strconv.Itoa(r.Rule.Classes))
	for _, k := range kinds {
		h.AddString(k)
		h.AddString(strconv.FormatInt(r.Rule.Hop[model.RelationKind(k)], 10))
	}
	for _, root := range r.Roots {
		h.AddString(string(root.Node))
		h.AddString(strconv.FormatInt(root.Weight, 10))
		h.AddString(strconv.FormatBool(root.Named))
		h.AddString(strconv.Itoa(root.MaxDepth))
	}
	return h.Sum()
}

// weightedWalk is one request's leg of a weighted walk.
type weightedWalk struct {
	reader    GraphReader
	rule      LabelRule
	codes     []KindCode
	absent    bool
	direction model.Direction
	maxDepth  config.Limit
	// maxVisited and maxEdges are ANSWER-level bounds, as they are for impact:
	// spending one ends the walk, and the labels settled so far are served
	// with the bound named as the truncation reason.
	maxVisited    config.Limit
	maxEdges      config.Limit
	frontierBytes int64
	// maxHop is the largest factor any walked kind carries: the batch bound's
	// one-hop ceiling.
	maxHop int64
	sc     *pathScratch
	scCtx  context.Context
	// deadline is the request's, on the engine clock now reads, zero for a
	// request with none: the impact walk's clock (checkWalk), so one clock
	// decides both walks' pages.
	deadline time.Time
	now      func() time.Time

	// visited and edges are cumulative across the pages of the walk; the
	// page counters are this request's own and decide a stalled page.
	visited, edges          int64
	pageEdges, pageSettled  int64
	expandAfter             EdgePos
	depthLimited, edgesHit  bool
	visitedHit, deadlineHit bool
	serving                 bool
	after                   labelKey
}

// labelKey is a settled label's identity: unique among settled labels, because
// a queued label meeting a settled one of its key is dominated by it.
type labelKey struct {
	node   string
	cls    int
	named  int
	weight int64
	reach  int64
}

// label is one row of the walk's state as the walk reads it back.
type label struct {
	id     int64
	node   model.NodeID
	depth  int
	named  int
	weight int64
	reach  int64
	route  int64
}

// seed queues one depth-0 label per root. A root lends itself nothing -- a
// root is never served as an entry -- but its label dominates what a lighter
// or equal route of its own kind would lend it back, which is what keeps a
// root of one weight from being listed as reached by its own walk.
func (w *weightedWalk) seed(roots []WeightedRoot) error {
	for _, root := range roots {
		reach := int64(reachUnbounded)
		if root.MaxDepth > 0 {
			reach = int64(root.MaxDepth)
		}
		if err := w.queue(string(root.Node), 0, boolInt(root.Named), root.Weight, reach, w.rule.Scale,
			root.Weight, 0, "", "", false); err != nil {
			return err
		}
	}
	return nil
}

// run settles and expands labels until none is queued, a bound ends the walk
// or the deadline ends the page.
func (w *weightedWalk) run(ctx context.Context) error {
	for {
		if err := w.drain(ctx); err != nil {
			return err
		}
		if w.stopped() {
			return nil
		}
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return err
		}
		var top int64
		found, err := w.sc.row(w.scCtx, `SELECT score FROM label WHERE state = 0 ORDER BY score DESC LIMIT 1`, nil, &top)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		batch, err := w.readBatch(top)
		if err != nil {
			return err
		}
		for _, l := range batch {
			// Checked per label: the rest of the batch stays queued and the
			// next page reads it again, so the latency is one label's work.
			if stop, err := w.checkDeadline(ctx); stop || err != nil {
				return err
			}
			if err := w.settle(l); err != nil {
				return err
			}
			if w.stopped() {
				return nil
			}
		}
	}
}

// batchBound is the score no label created from here on can exceed, given the
// best queued score top. Every label not yet created descends from a queued
// one, whose score is at most top; one hop multiplies the route by at most
// maxHop and the score follows it, and the half-up rounding of each product,
// with a weight no larger than Scale, adds less than two. So a child of a
// label scoring s scores below s*maxHop/Scale + 2.
func batchBound(top, maxHop, scale int64) int64 {
	hi, lo := bitsMul(top, maxHop)
	if hi != 0 {
		return top
	}
	return int64(lo/uint64(scale)) + 2
}

// readBatch reads the queued labels that are final now: every one scoring
// above the batch bound, best first. When the bound leaves none -- scores so
// small the rounding margin covers them -- the batch is the best score's
// shallowest labels, which every label created later is deeper than.
func (w *weightedWalk) readBatch(top int64) ([]label, error) {
	bound := batchBound(top, w.maxHop, w.rule.Scale)
	query := `SELECT id, node, depth, named, weight, reach, route FROM label WHERE state = 0 AND score > ?
		ORDER BY score DESC, depth, node, named DESC, weight DESC, reach DESC, route DESC, id LIMIT ?`
	args := []any{bound, w.batchRows()}
	if bound >= top {
		var depth int
		if _, err := w.sc.row(w.scCtx, `SELECT MIN(depth) FROM label WHERE state = 0 AND score = ?`,
			[]any{top}, &depth); err != nil {
			return nil, err
		}
		query = `SELECT id, node, depth, named, weight, reach, route FROM label WHERE state = 0 AND score = ? AND depth = ?
			ORDER BY node, named DESC, weight DESC, reach DESC, route DESC, id LIMIT ?`
		args = []any{top, depth, w.batchRows()}
	}
	var out []label
	err := w.sc.each(w.scCtx, query, args, func(scan func(...any) error) error {
		var l label
		var node string
		if err := scan(&l.id, &node, &l.depth, &l.named, &l.weight, &l.reach, &l.route); err != nil {
			return internalErr("weighted walk read: " + err.Error())
		}
		l.node = model.NodeID(node)
		out = append(out, l)
		return nil
	})
	return out, err
}

// batchRows is how many labels one read takes: the frontier byte ceiling in
// label rows, never less than one. It bounds heap, not the walk -- the rest of
// a batch is the next read's.
func (w *weightedWalk) batchRows() int64 {
	return max(1, w.frontierBytes/labelRowBytes)
}

// labelRowBytes charges one label row read into heap: the frontier
// estimator's per-row overhead plus its node id.
const labelRowBytes = edgeRowOverheadBytes + 64

// settle makes one queued label final, or drops it when a label already
// settled at its node dominates it.
func (w *weightedWalk) settle(l label) error {
	cls := min(l.depth, w.rule.Classes)
	dominated, err := w.dominated(string(l.node), cls, l.named, l.weight, l.reach, l.route)
	if err != nil {
		return err
	}
	if dominated {
		return w.sc.exec(w.scCtx, `DELETE FROM label WHERE id = ?`, l.id)
	}
	if l.depth > 0 {
		var seen int
		found, err := w.sc.row(w.scCtx, `SELECT 1 FROM label WHERE node = ? AND state > 0 AND depth > 0 LIMIT 1`,
			[]any{string(l.node)}, &seen)
		if err != nil {
			return err
		}
		if !found {
			// A node the walk has not settled a route to before: it is what
			// the visited bound counts.
			if w.maxVisited.Exceeded(w.visited + 1) {
				w.visitedHit = true
				return w.saveProgress()
			}
			w.visited++
		}
	}
	w.pageSettled++
	return w.sc.exec(w.scCtx, `UPDATE label SET state = 1 WHERE id = ?`, l.id)
}

// dominated reports whether a settled label at node is no deeper in class, of
// a class at least as strong, of a weight and route factor at least as large,
// and may walk at least as far as the label described.
func (w *weightedWalk) dominated(node string, cls, named int, weight, reach, route int64) (bool, error) {
	var one int
	return w.sc.row(w.scCtx, `SELECT 1 FROM label WHERE node = ? AND state > 0 AND cls <= ? AND named >= ?
		AND weight >= ? AND reach >= ? AND route >= ? LIMIT 1`,
		[]any{node, cls, named, weight, reach, route}, &one)
}

// drain expands every settled label still owed its expansion, a batch of
// nodes at a time, in one resumable adjacency scan per batch.
func (w *weightedWalk) drain(ctx context.Context) error {
	for {
		if stop, err := w.checkDeadline(ctx); stop || err != nil {
			return err
		}
		owners, err := w.pending()
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			return nil
		}
		stopped, err := w.expand(ctx, owners)
		if err != nil || stopped {
			return err
		}
		for _, labels := range owners {
			for _, l := range labels {
				if err := w.sc.exec(w.scCtx, `UPDATE label SET state = 2 WHERE id = ?`, l.id); err != nil {
					return err
				}
			}
		}
		w.expandAfter = EdgePos{}
		if err := w.saveProgress(); err != nil {
			return err
		}
	}
}

// pending reads the settled labels of the first adjacencyBatch nodes still
// owed an expansion, by node. The set is stable for the page: nothing is
// settled while a batch is expanding, so a resumed scan reads the same batch
// its saved position belongs to.
func (w *weightedWalk) pending() (map[model.NodeID][]label, error) {
	out := map[model.NodeID][]label{}
	var last string
	err := w.sc.each(w.scCtx, `SELECT id, node, depth, named, weight, reach, route FROM label
		WHERE state = 1 ORDER BY node, id`, nil, func(scan func(...any) error) error {
		var l label
		var node string
		if err := scan(&l.id, &node, &l.depth, &l.named, &l.weight, &l.reach, &l.route); err != nil {
			return internalErr("weighted walk read: " + err.Error())
		}
		if node != last && len(out) == adjacencyBatch {
			return errPathChunkFull
		}
		last = node
		l.node = model.NodeID(node)
		out[l.node] = append(out[l.node], l)
		return nil
	})
	if err != nil && !errors.Is(err, errPathChunkFull) {
		return nil, err
	}
	return out, nil
}

// weightedEdge is one delivered adjacency entry awaiting its canonical ids.
type weightedEdge struct {
	edge   Edge
	labels []label
}

// expand scans the batch's adjacency and relaxes every entry from every label
// of its owner. It reports whether the page or the walk ended inside it.
func (w *weightedWalk) expand(ctx context.Context, owners map[model.NodeID][]label) (bool, error) {
	if w.absent {
		return false, nil
	}
	nodes := make([]model.NodeID, 0, len(owners))
	for n := range owners {
		nodes = append(nodes, n)
	}
	slices.Sort(nodes)
	resolved, err := w.reader.Resolve(ctx, nodes)
	if err != nil {
		return false, err
	}
	byRef := make(map[NodeRef][]label, len(resolved))
	refs := make([]NodeRef, 0, len(resolved))
	for i, ref := range resolved {
		if ref == 0 {
			continue
		}
		// A label its root's derived bound ends walks no further, silently: a
		// derived bound is not a cut. Where the request's depth bound ends it
		// at the same depth, that bound is the one that set it, so the label
		// is scanned and relax discloses the cut when an entry exists to cut.
		var walk []label
		for _, l := range owners[nodes[i]] {
			if l.reach != reachUnbounded && l.reach <= 0 && !w.maxDepth.Exceeded(int64(l.depth)+1) {
				continue
			}
			walk = append(walk, l)
		}
		if len(walk) == 0 {
			continue
		}
		if _, ok := byRef[ref]; !ok {
			refs = append(refs, ref)
		}
		byRef[ref] = append(byRef[ref], walk...)
	}
	if len(refs) == 0 {
		return false, nil
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	pend := make([]weightedEdge, 0, adjacencyBatch)
	stopped := false
	var ferr error
	pos, err := w.reader.Neighbours(ctx, refs, w.direction, w.codes, w.expandAfter, func(e Edge) error {
		labels := byRef[e.Owner]
		if len(labels) == 0 {
			return nil
		}
		if w.maxEdges.Exceeded(w.edges + 1) {
			w.edgesHit, stopped = true, true
			return ErrStopScan
		}
		if len(pend) == 0 {
			stop, derr := w.checkDeadline(ctx)
			if derr != nil {
				ferr = derr
				return ErrStopScan
			}
			if stop {
				stopped = true
				return ErrStopScan
			}
		}
		w.edges++
		w.pageEdges++
		pend = append(pend, weightedEdge{edge: e, labels: labels})
		if len(pend) < adjacencyBatch {
			return nil
		}
		if ferr = w.relax(ctx, pend); ferr != nil {
			return ErrStopScan
		}
		pend = pend[:0]
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			return false, err
		}
		stop, derr := w.checkDeadline(ctx)
		if derr != nil {
			return false, derr
		}
		if !stop {
			return false, err
		}
		stopped = true
	}
	w.expandAfter = pos
	if ferr != nil {
		return false, ferr
	}
	if err := w.relax(ctx, pend); err != nil {
		return false, err
	}
	return stopped, w.saveProgress()
}

// relax turns one batch of delivered entries into queued labels, one per
// label of the entry's owner, with the canonical ids resolved for the whole
// batch in two reads.
func (w *weightedWalk) relax(ctx context.Context, pend []weightedEdge) error {
	if len(pend) == 0 {
		return nil
	}
	rels := make([]RelRef, len(pend))
	tos := make([]NodeRef, len(pend))
	for i, p := range pend {
		rels[i], tos[i] = p.edge.Rel, p.edge.Neighbour
	}
	relIDs, err := w.reader.RelationIDs(ctx, rels)
	if err != nil {
		return err
	}
	toIDs, err := w.reader.NodeIDs(ctx, tos)
	if err != nil {
		return err
	}
	for i, p := range pend {
		to, rel := toIDs[i], relIDs[i]
		if to == "" || rel == "" {
			continue
		}
		kind, ok := w.reader.Kinds().Kind(p.edge.Kind)
		if !ok {
			continue
		}
		hop := w.rule.Hop[kind]
		for _, l := range p.labels {
			if w.maxDepth.Exceeded(int64(l.depth) + 1) {
				w.depthLimited = true
				continue
			}
			if l.reach != reachUnbounded && l.reach <= 0 {
				continue
			}
			route, err := scaledProduct(l.route, hop, w.rule.Scale)
			if err != nil {
				return err
			}
			if l.depth > 0 {
				if route, err = scaledProduct(route, w.rule.Decay, w.rule.Scale); err != nil {
					return err
				}
			}
			score, err := scaledProduct(l.weight, route, w.rule.Scale)
			if err != nil {
				return err
			}
			reach := l.reach
			if reach != reachUnbounded {
				reach--
			}
			depth := l.depth + 1
			if err := w.queue(string(to), depth, l.named, l.weight, reach, route, score, l.id,
				string(rel), string(kind), p.edge.Outgoing); err != nil {
				return err
			}
		}
	}
	return nil
}

// queue offers one label. One already settled at its node that dominates it
// keeps it out; a queued one of its key keeps the better route -- the larger
// factor, then the shallower depth, then the smaller admitting relation id,
// which is content-derived, so two builds of one tree keep the same route.
func (w *weightedWalk) queue(node string, depth, named int, weight, reach, route, score, parent int64,
	rel, kind string, outgoing bool) error {
	cls := min(depth, w.rule.Classes)
	dominated, err := w.dominated(node, cls, named, weight, reach, route)
	if err != nil || dominated {
		return err
	}
	return w.sc.exec(w.scCtx, `INSERT INTO label(node, depth, cls, named, weight, reach, route, score, parent, rel,
		kind, outgoing, state) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(node, cls, named, weight, reach) WHERE state = 0 DO UPDATE SET
			depth = excluded.depth, route = excluded.route, score = excluded.score, parent = excluded.parent,
			rel = excluded.rel, kind = excluded.kind, outgoing = excluded.outgoing
		WHERE excluded.route > label.route OR (excluded.route = label.route AND
			(excluded.depth < label.depth OR (excluded.depth = label.depth AND excluded.rel < label.rel)))`,
		node, depth, cls, named, weight, reach, route, score, parent, rel, kind, boolInt(outgoing))
}

// stopped reports that the walk ends here: a bound ended it or the deadline
// ended the page.
func (w *weightedWalk) stopped() bool { return w.edgesHit || w.visitedHit || w.deadlineHit }

// checkDeadline ends the page on the deadline and fails on a cancellation.
// The deadline is read on the engine clock as well as off the context, as the
// impact walk reads it.
func (w *weightedWalk) checkDeadline(ctx context.Context) (bool, error) {
	err := ctx.Err()
	if err == nil {
		if !w.deadline.IsZero() && w.now != nil && !w.now().Before(w.deadline) {
			w.deadlineHit = true
			return true, nil
		}
		return false, nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return true, model.Canceled(err)
	}
	w.deadlineHit = true
	return true, nil
}

func (w *weightedWalk) loadProgress() error {
	var after string
	var depthLimited, edgesHit, visitedHit, serving int
	if _, err := w.sc.row(w.scCtx, `SELECT expand_after, depth_limited, edges_hit, visited_hit, serving,
		after_node, after_cls, after_named, after_weight, after_reach FROM progress WHERE k = 0`, nil,
		&after, &depthLimited, &edgesHit, &visitedHit, &serving,
		&w.after.node, &w.after.cls, &w.after.named, &w.after.weight, &w.after.reach); err != nil {
		return err
	}
	pos, err := decodeEdgePos(after)
	if err != nil {
		return err
	}
	w.expandAfter = pos
	w.depthLimited, w.edgesHit, w.visitedHit = depthLimited != 0, edgesHit != 0, visitedHit != 0
	w.serving = serving != 0
	return nil
}

func (w *weightedWalk) saveProgress() error {
	return w.sc.exec(w.scCtx, `UPDATE progress SET expand_after = ?, depth_limited = ?, edges_hit = ?,
		visited_hit = ?, serving = ?, after_node = ?, after_cls = ?, after_named = ?, after_weight = ?,
		after_reach = ? WHERE k = 0`,
		encodeEdgePos(w.expandAfter), boolInt(w.depthLimited), boolInt(w.edgesHit), boolInt(w.visitedHit),
		boolInt(w.serving), w.after.node, w.after.cls, w.after.named, w.after.weight, w.after.reach)
}

// servedLabel is one kept label as the listing reads it.
type servedLabel struct {
	id       int64
	key      labelKey
	depth    int
	score    int64
	kind     string
	outgoing bool
}

// page reads the next limit kept labels -- every settled label past depth 0 --
// in key order after the last one served, and reports whether more remain.
func (w *weightedWalk) page(limit int) ([]servedLabel, bool, error) {
	var out []servedLabel
	more := false
	a := w.after
	err := w.sc.each(w.scCtx, `SELECT id, node, cls, named, weight, reach, depth, score, kind, outgoing FROM label
		WHERE state > 0 AND depth > 0 AND (node, cls, named, weight, reach) > (?, ?, ?, ?, ?)
		ORDER BY node, cls, named, weight, reach LIMIT ?`,
		[]any{a.node, a.cls, a.named, a.weight, a.reach, limit + 1}, func(scan func(...any) error) error {
			var s servedLabel
			var outgoing int
			if err := scan(&s.id, &s.key.node, &s.key.cls, &s.key.named, &s.key.weight, &s.key.reach,
				&s.depth, &s.score, &s.kind, &outgoing); err != nil {
				return internalErr("weighted walk read: " + err.Error())
			}
			if len(out) == limit {
				more = true
				return errPathChunkFull
			}
			s.outgoing = outgoing != 0
			out = append(out, s)
			return nil
		})
	if err != nil && !errors.Is(err, errPathChunkFull) {
		return nil, false, err
	}
	if len(out) > 0 {
		w.after = out[len(out)-1].key
	}
	return out, more, nil
}

// route reads a label's route back through its parents, root first, with the
// kind of each hop. A route longer than a path may carry is not read: the
// entry is served without one, as an impact entry past that bound is.
func (w *weightedWalk) route(l servedLabel) ([]model.RelationID, int64, error) {
	if l.depth > model.MaxRelationsPerPath {
		return nil, 0, nil
	}
	rels := make([]model.RelationID, 0, l.depth)
	var cost int64
	err := w.sc.each(w.scCtx, `WITH RECURSIVE chain(id, parent, rel, kind, depth) AS (
			SELECT id, parent, rel, kind, depth FROM label WHERE id = ?
			UNION ALL SELECT l.id, l.parent, l.rel, l.kind, l.depth FROM label l JOIN chain c ON l.id = c.parent)
		SELECT rel, kind FROM chain WHERE rel != '' ORDER BY depth`, []any{l.id},
		func(scan func(...any) error) error {
			var rel, kind string
			if err := scan(&rel, &kind); err != nil {
				return internalErr("weighted walk read: " + err.Error())
			}
			rels = append(rels, model.RelationID(rel))
			cost += Cost(model.RelationKind(kind))
			return nil
		})
	return rels, cost, err
}

// weightedEntries hydrates one page of kept labels into entries, with the
// route behind each and its evidence. A label at a node the pinned generation
// cannot hydrate is counted and disclosed, as impact does.
func (e *Engine) weightedEntries(ctx context.Context, w *weightedWalk, page []servedLabel,
	meta *model.QueryMeta) ([]WeightedEntry, error) {
	if len(page) == 0 {
		return nil, nil
	}
	ids := make([]model.NodeID, 0, len(page))
	for _, l := range page {
		ids = append(ids, model.NodeID(l.key.node))
	}
	nodes, err := e.nodesByID(deliverCtx(ctx), ids)
	if err != nil {
		return nil, err
	}
	explained, err := w.explain(ctx, page)
	if err != nil {
		return nil, err
	}
	entries := make([]model.ImpactEntry, 0, len(page))
	kept := make([]servedLabel, 0, len(page))
	dropped := 0
	for _, l := range page {
		n, ok := nodes[model.NodeID(l.key.node)]
		if !ok {
			dropped++
			continue
		}
		direction := model.DirectionIncoming
		if l.outgoing {
			direction = model.DirectionOutgoing
		}
		// The label's own last hop explains it when the breadth-first rule
		// delivers no relation into it from its weight's walk: a route that
		// rule never takes still needs its reason.
		reasons := []string{impactReason(model.RelationKind(l.kind), direction, l.depth)}
		if why, ok := explained[l.id]; ok && len(why.reasons) > 0 {
			reasons, direction = why.reasons, model.DirectionOutgoing
			if why.incoming {
				direction = model.DirectionIncoming
			}
		}
		entry := model.ImpactEntry{NodeID: n.ID, FileID: n.FileID, Kind: n.Kind,
			Name: clipTo(n.QualifiedName, model.MaxQualifiedNameBytes), Direction: direction,
			Depth: l.depth, ScoreMicros: l.score, Reasons: reasons}
		rels, cost, err := w.route(l)
		if err != nil {
			return nil, err
		}
		if len(rels) > 0 {
			entry.Paths = []model.RelationPath{{Relations: rels, CostUnits: cost}}
		}
		entries = append(entries, entry)
		kept = append(kept, l)
	}
	if dropped > 0 {
		meta.Notices = appendNotice(meta.Notices, fmt.Sprintf(
			"weighted walk: %d reached label(s) could not be hydrated from the pinned generation and are not listed", dropped))
	}
	if err := impactPhaseError(ctx, e.attachImpactEvidence(deliverCtx(ctx), entries), meta); err != nil {
		return nil, err
	}
	out := make([]WeightedEntry, len(entries))
	for i, entry := range entries {
		out[i] = WeightedEntry{ImpactEntry: entry, RootWeight: kept[i].key.weight, Named: kept[i].key.named != 0}
	}
	return out, nil
}

// scaledProduct is a*b/scale rounded half up, the fixed-point product a
// ranking pass multiplies route factors with. The operands are non-negative
// and at most scale squared fits the walk's bounds; an overflow is a defect in
// the caller's rule, reported rather than wrapped.
func scaledProduct(a, b, scale int64) (int64, error) {
	hi, lo := bitsMul(a, b)
	half := uint64(scale / 2)
	if hi != 0 || lo > math.MaxInt64-half {
		return 0, internalErr(fmt.Sprintf("weighted walk: %d x %d overflows its fixed-point product", a, b))
	}
	return int64((lo + half) / uint64(scale)), nil
}

// bitsMul is the 128-bit product of two non-negative int64s.
func bitsMul(a, b int64) (uint64, uint64) {
	return bits.Mul64(uint64(a), uint64(b))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// explanation is the reasons and the sticky incoming direction one served
// label carries beside its own route's.
type explanation struct {
	reasons  []string
	incoming bool
}

// explain gives each served label the reasons an impact walk from its root's
// weight and class alone would give the same node: one per relation the walk
// of that weight delivers into it. A breadth-first walk delivers a relation
// toward the endpoint it had not yet reached -- from a neighbour one level
// shallower, or from one on the same level when the relation leaves that
// neighbour -- so the shallowest settled label of each endpoint, among the
// labels of that weight and class, decides it. The neighbour must be one the
// walk expands: within its root's derived bound and the request's depth bound.
//
// It reads the page's nodes' adjacency once, so it costs the page.
func (w *weightedWalk) explain(ctx context.Context, page []servedLabel) (map[int64]explanation, error) {
	out := map[int64]explanation{}
	if w.absent || len(page) == 0 {
		return out, nil
	}
	ids := make([]model.NodeID, 0, len(page))
	for _, l := range page {
		if len(ids) == 0 || ids[len(ids)-1] != model.NodeID(l.key.node) {
			ids = append(ids, model.NodeID(l.key.node))
		}
	}
	refs, err := w.reader.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	byRef := map[NodeRef][]servedLabel{}
	var scan []NodeRef
	for i, ref := range refs {
		if ref == 0 {
			continue
		}
		if _, ok := byRef[ref]; !ok {
			scan = append(scan, ref)
		}
		for _, l := range page {
			if l.key.node == string(ids[i]) {
				byRef[ref] = append(byRef[ref], l)
			}
		}
	}
	if len(scan) == 0 {
		return out, nil
	}
	sort.Slice(scan, func(i, j int) bool { return scan[i] < scan[j] })
	var edges []Edge
	if _, err := w.reader.Neighbours(ctx, scan, w.direction, w.codes, EdgePos{}, func(e Edge) error {
		edges = append(edges, e)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return out, nil
	}
	others := make([]NodeRef, len(edges))
	for i, e := range edges {
		others[i] = e.Neighbour
	}
	otherIDs, err := w.reader.NodeIDs(ctx, others)
	if err != nil {
		return nil, err
	}
	for i, e := range edges {
		kind, ok := w.reader.Kinds().Kind(e.Kind)
		if !ok || otherIDs[i] == "" {
			continue
		}
		for _, l := range byRef[e.Owner] {
			near, _, found, err := w.shallowest(l.key.node, l.key.weight, l.key.named)
			if err != nil {
				return nil, err
			}
			far, reach, ok, err := w.shallowest(string(otherIDs[i]), l.key.weight, l.key.named)
			if err != nil {
				return nil, err
			}
			if !found || !ok || (reach != reachUnbounded && reach <= 0) || w.maxDepth.Exceeded(int64(far)+1) {
				continue
			}
			// The relation leaves the neighbour when this node's copy of it
			// is incoming.
			leaves := !e.Outgoing
			if near != far+1 && !(near == far && leaves) {
				continue
			}
			direction := model.DirectionIncoming
			if leaves {
				direction = model.DirectionOutgoing
			}
			x := out[l.id]
			x.reasons = mergeReasons(x.reasons, []string{impactReason(kind, direction, far+1)})
			x.incoming = x.incoming || direction == model.DirectionIncoming
			out[l.id] = x
		}
	}
	return out, nil
}

// shallowest is the depth and remaining derived reach of node's shallowest
// settled label of one root weight and class.
func (w *weightedWalk) shallowest(node string, weight int64, named int) (int, int64, bool, error) {
	var depth int
	var reach int64
	found, err := w.sc.row(w.scCtx, `SELECT depth, reach FROM label WHERE node = ? AND state > 0 AND weight = ?
		AND named = ? ORDER BY depth, reach DESC LIMIT 1`, []any{node, weight, named}, &depth, &reach)
	return depth, reach, found, err
}
