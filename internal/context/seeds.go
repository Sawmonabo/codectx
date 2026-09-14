// This file is owned by Task 15 lane L1. It holds
// Section 15.2 seed extraction: explicit seeds, backticked identifiers, path tokens, qualified identifiers, exact resolution, lexical terms and changed files, in that order, preserving ambiguity.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// seedSet is the Section 15.2 result: the candidates the task named, the
// identities it named that nothing in the pinned snapshot answers, and whether
// the seed pass saw everything it would have admitted. Unresolved is what
// forces ScopeComplete=false downstream — it is set both by a named identity
// nothing answers and by a discovery step that stopped at one of its own
// bounds; an empty or wholly ambiguous scope is a discovery answer, never an
// error (Section 15.2).
type seedSet struct {
	Candidates []candidate
	Excluded   []candidate
	Unresolved bool
}

// seedToken is one identity the task named, tagged with the Section 15.2 step
// that found it. The token text is never lowercased or rewritten: an identity
// the caller typed is compared as typed.
type seedToken struct {
	Text   string
	Origin originKind
}

// extractSeeds runs the Section 15.2 steps in order over one pinned
// generation:
//
//	0 explicit req.Seeds, 1 backticked identifiers and paths, 2 path-like
//	tokens, 3 qualified identifiers, 4 exact resolution of the remaining free
//	terms, 5 lexical resolution of the task text, 6 low-priority changed files.
//
// Steps 0-3 name identities, so an identity none of them resolves is recorded
// as an exclusion with a reason and the compile continues. Steps 4-5 mine free
// prose, where a word that matches nothing is not a failed identity and leaves
// no exclusion.
//
// gen is the generation Compile pinned once; it is passed into every
// downstream request so that an activation mid-compile can never split one
// manifest across two generations (Section 15.1). reader is that same pinned
// reader, so a path token is checked against visible facts only.
func (c *Compiler) extractSeeds(ctx context.Context, reader *sqlite.PinnedReader, gen model.GenerationID, req model.ContextRequest) (seedSet, error) {
	var out seedSet
	seen := map[string]bool{}
	excluded := map[string]bool{}

	tokens := make([]seedToken, 0, len(req.Seeds)+model.MaxSeeds)
	for _, s := range req.Seeds {
		if t := strings.TrimSpace(s); t != "" {
			tokens = append(tokens, seedToken{Text: t, Origin: originExplicitSeed})
		}
	}
	task := boundTask(req.Task)
	tokens = append(tokens, taskTokens(task)...)

	for _, tok := range tokens {
		if len(out.Candidates) >= model.MaxSeeds {
			break
		}
		found, err := c.resolveIdentity(ctx, reader, gen, tok, seen, &out)
		if err != nil {
			return seedSet{}, err
		}
		if found {
			continue
		}
		// Section 15.2: an identity the caller named that the pinned snapshot
		// does not answer is reported, never dropped silently, and never
		// repaired by a later pass.
		out.Unresolved = true
		if key := "token:" + tok.Text; !excluded[key] {
			excluded[key] = true
			out.Excluded = append(out.Excluded, candidate{
				Path:     tok.Text,
				Origin:   tok.Origin,
				Excluded: boundReason(fmt.Sprintf("the task names %q, which resolves to nothing in the pinned snapshot", tok.Text)),
			})
		}
	}

	if err := c.freeTermSeeds(ctx, gen, task, tokens, seen, &out); err != nil {
		return seedSet{}, err
	}
	if err := c.changedFileSeeds(ctx, reader, seen, &out); err != nil {
		return seedSet{}, err
	}
	return out, nil
}

// resolveIdentity resolves one named identity from steps 0-3 and reports
// whether anything answered it. A path token that names a visible snapshot file
// resolves to that file; otherwise the token goes to Search.Resolve, whose every
// candidate is kept. Ambiguity is preserved: two declarations of one name are
// two candidates, each carrying the ambiguity in its reason, never a silent
// first pick (Section 14.1).
func (c *Compiler) resolveIdentity(ctx context.Context, reader *sqlite.PinnedReader, gen model.GenerationID, tok seedToken, seen map[string]bool, out *seedSet) (bool, error) {
	if p := normalizeSeedPath(tok.Text); p != "" {
		fv, err := c.snapshotFile(ctx, reader, p)
		if err != nil {
			return false, err
		}
		if fv.ID != "" {
			add(out, seen, candidate{
				FileID:      fv.ID,
				Path:        fv.Path,
				Requirement: model.RequirementFull,
				Origin:      tok.Origin,
				Status:      fv.Status,
				SizeBytes:   fv.Size,
				Reasons:     []string{boundReason(fmt.Sprintf("the task names the path %q", fv.Path))},
			})
			return true, nil
		}
	}

	nodes, err := c.resolveSymbol(ctx, gen, tok.Text)
	if err != nil {
		return false, err
	}
	if len(nodes) == 0 {
		return false, nil
	}
	for _, n := range nodes {
		reason := fmt.Sprintf("the task names the symbol %q", tok.Text)
		if len(nodes) > 1 {
			// Every declaration of an ambiguous name is carried forward with
			// its own reason so the plan states the ambiguity instead of
			// hiding it behind one arbitrary winner.
			reason = fmt.Sprintf("the task names the symbol %q, which has %d declarations in the pinned snapshot; this is one of them", tok.Text, len(nodes))
		}
		add(out, seen, nodeCandidate(n, tok.Origin, reason))
	}
	return true, nil
}

// freeTermSeeds runs Section 15.2 steps 4 and 5 over the prose the earlier
// steps did not claim: each remaining term goes to Search.Resolve for an exact
// declaration, then the task text goes to Search.Search once for lexical hits.
// A word that answers nothing is prose, not a failed identity, so it leaves no
// exclusion and does not make the scope incomplete.
func (c *Compiler) freeTermSeeds(ctx context.Context, gen model.GenerationID, task string, claimed []seedToken, seen map[string]bool, out *seedSet) error {
	taken := make(map[string]bool, len(claimed))
	for _, tok := range claimed {
		taken[tok.Text] = true
	}
	for _, term := range freeTerms(task) {
		if len(out.Candidates) >= model.MaxSeeds {
			return nil
		}
		if taken[term] {
			continue
		}
		nodes, err := c.resolveSymbol(ctx, gen, term)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			add(out, seen, nodeCandidate(n, originExactResolve,
				fmt.Sprintf("the task mentions %q, which names a declaration in the pinned snapshot", term)))
		}
	}
	if len(out.Candidates) >= model.MaxSeeds || strings.TrimSpace(task) == "" {
		return nil
	}

	page, err := c.search.Search(ctx, model.SearchRequest{
		GenerationID: gen,
		Query:        clipUTF8(task, model.MaxQueryTextBytes),
		Page:         model.PageRequest{Limit: c.pageLimit()},
	})
	if err != nil {
		// A bound the lexical tier could not serve within is a capability
		// reduction, not a failed compile: the weakest discovery step stops
		// contributing and the scope is reported incomplete (Section 14.4).
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeResourceLimit {
			out.Unresolved = true
			return nil
		}
		return contextErr(ctx, err)
	}
	for _, hit := range page.Items {
		if len(out.Candidates) >= model.MaxSeeds {
			return nil
		}
		cnd := candidate{
			NodeID:      hit.NodeID,
			FileID:      hit.FileID,
			Path:        hit.Path,
			Requirement: model.RequirementRecommended,
			Origin:      originLexical,
			Reasons:     []string{boundReason(fmt.Sprintf("the task text matches %q lexically", hit.Path))},
		}
		if hit.Range != nil {
			cnd.StartByte = int64(hit.Range.Start.Byte)
		}
		add(out, seen, cnd)
	}
	return nil
}

// changedFileSeeds is Section 15.2 step 6: the captured working-tree changes,
// admitted last and at the lowest priority, so an active edit informs the plan
// without displacing an identity the task named.
//
// It reads changed rows only, through PinnedReader.ChangedFiles, rather than
// paging every file row of the snapshot and discarding the unchanged ones. That
// is what bounds it: a page holds only admissible rows, so the loop stops after
// at most model.MaxSeeds/pageLimit + 1 reads whatever the workspace's file
// count is. The page ceiling this used to carry bounded the scan by declaring a
// 250,000-file workspace's scope unresolvable, which a supported workspace must
// never be.
func (c *Compiler) changedFileSeeds(ctx context.Context, reader *sqlite.PinnedReader, seen map[string]bool, out *seedSet) error {
	var after model.FileID
	// Compiler.pageLimit only ever yields a value in (0, model.MaxPageItems],
	// which is exactly the window sqlite.pageLimit passes through unchanged, so
	// a page shorter than this limit is genuinely the last page and not a
	// clamped read that still has rows behind it.
	limit := c.pageLimit()
	for len(out.Candidates) < model.MaxSeeds {
		files, err := reader.ChangedFiles(ctx, after, changedStatuses, limit)
		if err != nil {
			return contextErr(ctx, err)
		}
		if len(files) == 0 {
			return nil
		}
		for _, fv := range files {
			after = fv.ID
			if len(out.Candidates) >= model.MaxSeeds {
				// Changed files remain that this plan never saw, exactly like a
				// discovery step stopped at any other of its own bounds, so the
				// scope is reported incomplete rather than silently partial.
				out.Unresolved = true
				return nil
			}
			add(out, seen, candidate{
				FileID:      fv.ID,
				Path:        fv.Path,
				Requirement: model.RequirementOptional,
				Origin:      originChangedFile,
				Status:      fv.Status,
				SizeBytes:   fv.Size,
				Reasons:     []string{boundReason(fmt.Sprintf("%q is a captured change with status %q", fv.Path, fv.Status))},
			})
		}
		if len(files) < limit {
			// A short page is the end of the keyset walk; asking for the page
			// after it would cost one more read that can only come back empty.
			return nil
		}
	}
	// The loop condition failed on entry or after a full page: seeds filled up
	// before every captured change was admitted, and the unseen ones make this
	// a partial view of the working tree.
	out.Unresolved = true
	return nil
}

// resolveSymbol is the ONE exact-resolution call in this package. It always
// names the pinned generation explicitly and asks for canonical facts only:
// Section 15.3 admits sealed facts, so the ephemeral LSP overlay is never a
// seed source.
func (c *Compiler) resolveSymbol(ctx context.Context, gen model.GenerationID, query string) ([]model.Node, error) {
	if query == "" {
		return nil, nil
	}
	page, err := c.search.Resolve(ctx, model.SymbolRequest{
		GenerationID:   gen,
		Query:          query,
		Operation:      model.SymbolResolve,
		SemanticSource: model.SemanticCanonical,
		Page:           model.PageRequest{Limit: c.pageLimit()},
	})
	if err != nil {
		var typed *model.Error
		// A token that is not a legal query, or one the resolver could not
		// answer within its bounds, is an unresolved identity rather than a
		// failed compile: it falls through to its exclusion reason, which is
		// what leaves the scope incomplete.
		if errors.As(err, &typed) && (typed.Code == model.CodeArgumentInvalid || typed.Code == model.CodeResourceLimit) {
			return nil, nil
		}
		return nil, contextErr(ctx, err)
	}
	return page.Items, nil
}

// snapshotFile returns the visible snapshot row for a normalized path, or the
// zero value when the pinned snapshot holds no such file. Absence is an answer
// here, not an error: the caller turns it into an exclusion with a reason.
func (c *Compiler) snapshotFile(ctx context.Context, reader *sqlite.PinnedReader, normalized string) (model.FileVersion, error) {
	fv, err := reader.File(ctx, model.NewFileID(c.repo, normalized))
	if err == nil {
		return fv, nil
	}
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
		return model.FileVersion{}, nil
	}
	return model.FileVersion{}, contextErr(ctx, err)
}

// pageLimit is the Section 20.1 resources.max_page_items bound every bounded
// read in this package is issued under -- seed batches, the evidence batch
// behind per-edge precision, and the file and edge hydrations the compile runs
// once each. One helper, so no two passes can page at different sizes.
func (c *Compiler) pageLimit() int {
	if n := c.cfg.Resources.MaxPageItems; n > 0 && n <= model.MaxPageItems {
		return n
	}
	return model.MaxPageItems
}

// nodeCandidate builds the seed candidate for one resolved declaration. A
// declaration the task named is required in full: Section 15.2 makes every
// selected implementation file required, and no later pass may demote it.
func nodeCandidate(n model.Node, origin originKind, reason string) candidate {
	c := candidate{
		NodeID:      n.ID,
		FileID:      n.FileID,
		Requirement: model.RequirementFull,
		Origin:      origin,
		Reasons:     []string{boundReason(reason)},
	}
	if n.Range != nil {
		c.StartByte = int64(n.Range.Start.Byte)
	}
	return c
}

// add records a candidate once. The key is the entity identity, so the earliest
// Section 15.2 step that found an entity keeps it: the order of the steps IS
// the seed priority, and a later, weaker origin never overwrites a stronger
// one.
func add(out *seedSet, seen map[string]bool, c candidate) {
	key := c.entityID()
	if key == "" || seen[key] {
		return
	}
	seen[key] = true
	out.Candidates = append(out.Candidates, c)
}

// boundTask clips the task text to model.MaxTaskBytes before any scanning, so
// an oversized task bounds the work instead of the work bounding the task.
func boundTask(task string) string { return clipUTF8(task, model.MaxTaskBytes) }

// clipUTF8 clips s to at most max bytes on a rune boundary, so a truncated
// identity is never a mangled one.
func clipUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	clipped := s[:max]
	for len(clipped) > 0 {
		r, size := utf8.DecodeLastRuneInString(clipped)
		if r != utf8.RuneError || size > 1 {
			break
		}
		clipped = clipped[:len(clipped)-1]
	}
	return clipped
}

// taskTokens extracts the Section 15.2 steps 1-3 identities from the task text,
// in that order: backtick-delimited identifiers and paths first, then bare
// path-like tokens, then bare qualified identifiers. A token found by an
// earlier step is not offered again by a later one, so the origin a candidate
// carries is the strongest step that named it.
func taskTokens(task string) []seedToken {
	var out []seedToken
	taken := map[string]bool{}
	emit := func(text string, origin originKind) {
		if text == "" || taken[text] || len(out) >= model.MaxSeeds {
			return
		}
		taken[text] = true
		out = append(out, seedToken{Text: text, Origin: origin})
	}

	backticked := map[string]bool{}
	for _, tok := range backtickTokens(task) {
		backticked[tok] = true
		emit(tok, originBacktick)
	}
	words := splitWords(task)
	for _, w := range words {
		if !backticked[w] && looksLikePath(w) {
			emit(w, originPathToken)
		}
	}
	for _, w := range words {
		if !backticked[w] && !looksLikePath(w) && looksQualified(w) {
			emit(w, originQualified)
		}
	}
	return out
}

// backtickTokens returns the contents of every backtick-delimited span, which
// Section 15.2 reads as the caller quoting an identity verbatim.
func backtickTokens(task string) []string {
	var out []string
	rest := task
	for len(out) < model.MaxSeeds {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			return out
		}
		rest = rest[open+1:]
		closeAt := strings.IndexByte(rest, '`')
		if closeAt < 0 {
			return out
		}
		if tok := strings.TrimSpace(rest[:closeAt]); tok != "" && len(tok) <= model.MaxPathBytes {
			out = append(out, tok)
		}
		rest = rest[closeAt+1:]
	}
	return out
}

// splitWords cuts the task into whitespace-separated words with surrounding
// prose punctuation trimmed. Interior punctuation is kept, because it is what
// makes a token a path or a qualified name.
func splitWords(task string) []string {
	fields := strings.FieldsFunc(task, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '`' || r == '"' || r == '\''
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		w := strings.Trim(f, ".,;:!?()[]{}<>")
		if w != "" && len(w) <= model.MaxPathBytes {
			out = append(out, w)
		}
	}
	return out
}

// freeTerms is the Section 15.2 step 4/5 input: the plain words of the task,
// which are mined for declarations and lexical hits. One-character words carry
// no identity, so they are not queried.
func freeTerms(task string) []string {
	words := splitWords(task)
	out := make([]string, 0, len(words))
	seen := map[string]bool{}
	for _, w := range words {
		if len(w) < 2 || seen[w] || len(out) >= model.MaxSeeds {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// looksLikePath reports whether a token reads as a repository path: it has a
// separator and a file extension, or it is an existing-looking directory path.
func looksLikePath(tok string) bool {
	if !strings.Contains(tok, "/") {
		return false
	}
	return path.Ext(path.Base(strings.TrimSuffix(tok, "/"))) != ""
}

// looksQualified reports whether a token reads as a qualified identifier:
// Section 15.2 names the "." and "::" separators, plus the "/" form a
// package-qualified name takes.
func looksQualified(tok string) bool {
	return strings.Contains(tok, "::") || strings.Contains(tok, ".") || strings.Contains(tok, "/")
}

// normalizeSeedPath turns a path-like token into the root-relative, slash
// separated form file identity is derived from, or "" when the token is not a
// path at all. It never resolves outside the root: a token that climbs above it
// is not a path here.
func normalizeSeedPath(tok string) string {
	if !strings.Contains(tok, "/") && path.Ext(tok) == "" {
		return ""
	}
	p := path.Clean(strings.TrimPrefix(strings.ReplaceAll(tok, "\\", "/"), "/"))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || len(p) > model.MaxPathBytes {
		return ""
	}
	return p
}

// changedStatuses is the ONE definition of "an active working-tree change": the
// Section 15.2 step 6 admission set. It is passed to PinnedReader.ChangedFiles
// rather than re-spelled as a SQL predicate there, so the status set cannot
// drift between the reader and the step that consumes it.
var changedStatuses = []model.FileStatus{model.FileModified, model.FileAdded, model.FileUntracked}

// boundReason clips one reason to model.MaxReasonBytes, which every stored
// entry and exclusion is validated against.
func boundReason(reason string) string { return clipUTF8(reason, model.MaxReasonBytes) }
