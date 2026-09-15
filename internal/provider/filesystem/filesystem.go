// Package filesystem is the base provider of Section 11.2: repository,
// directory, file and recognized document/configuration/build-target nodes,
// the `contains` and `defines` relations between them, the README-style
// `documents` relation, and the per-file lexical search chunks stored once in
// the file's own unit. It also owns the single path classification table
// (R7-4) and the fact emitter every other base provider reuses.
//
// One unit is one file (R7-1). The unit reads exactly its file through the
// pinned snapshot view, mints its identities through the resolver and emits
// its ancestors as idempotent node facts (R7-2), so no unit ever needs a
// repository-wide list and an unchanged file reuses its sealed unit.
package filesystem

import (
	"context"
	"github.com/Sawmonabo/codectx/internal/config"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Provider identity and capabilities.
const (
	ID = "filesystem"
	// Version is part of every unit key, so it is bumped whenever a unit's
	// emitted facts change. 4: the binary decision is a proportion of the
	// sniffed head rather than a single NUL byte, so a text file carrying a
	// stray NUL is indexed (with the NUL substituted) instead of left
	// without a lexical index; and a chunk boundary that used to fall inside
	// a UTF-8 sequence now moves, which changes the byte ranges a file's
	// documents are keyed by. Units sealed by version 3 must be rebuilt.
	Version = "4"

	// CapabilityStructure is the repository/directory/file graph.
	CapabilityStructure = "structure"
	// CapabilitySearch is the lexical chunk index of a file. It is reported
	// per file: unavailable for binary or oversize content (an admission
	// decision that never affects CAS retention), and fresh for every other
	// file, including one whose bytes are not UTF-8 throughout and one
	// carrying a stray NUL -- those chunks are indexed with the offending
	// bytes substituted, and the substitution is disclosed in the capability
	// detail rather than costing the file its content.
	CapabilitySearch = "search"
)

// Options are the analysis admission settings this provider honours. They
// come from configuration (workspace.max_search_file_bytes) and are part of
// the analysis configuration hash, so changing them re-keys every unit.
type Options struct {
	// MaxSearchFileBytes is the largest file admitted to search indexing.
	// Unlimited (0) admits every file; a user-set value excludes the larger
	// ones and each exclusion is reported on the search capability, naming
	// the limit that excluded it.
	MaxSearchFileBytes config.Limit
	// MaxEvidencePerFact is the effective per-fact evidence clip: the operator's
	// index.max_evidence_per_fact, or the model's record ceiling when they set
	// none. Zero selects the ceiling. Occurrences past it are counted and
	// disclosed, never dropped in silence.
	MaxEvidencePerFact int
}

// Provider is the filesystem provider.
type Provider struct {
	opts Options
}

// New validates the options and returns the provider.
func New(opts Options) (*Provider, error) {
	return &Provider{opts: opts}, nil
}

// Descriptor is the static contract: a required, file-scoped base provider
// with no dependencies.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: ID, Version: Version, Capabilities: []string{CapabilityStructure, CapabilitySearch},
		InvalidationScope: model.InvalidationFile, Required: true}
}

// Detect always succeeds: every workspace has files. Nothing is walked.
func (p *Provider) Detect(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
	return provider.Detection{Available: true, Capabilities: []string{CapabilityStructure, CapabilitySearch}}, nil
}

// IndexUnit produces the unit of one file.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	rel, err := PathFromScope(req.Unit.ScopeKey)
	if err != nil {
		return model.ProviderResult{}, err
	}
	rc, fv, err := req.Content.Open(ctx, model.NewFileID(req.Binding.RepositoryID, rel))
	if err != nil {
		return model.ProviderResult{}, err
	}
	defer rc.Close()
	if fv.Path != rel {
		return model.ProviderResult{}, &model.Error{Code: model.CodeInternal, Message: "snapshot returned a different path than the unit scope names"}
	}
	e := NewEmitter(req, sink, fv, p.opts.MaxEvidencePerFact)
	cls := Classify(rel)

	// The head is read before any fact is emitted so the binary decision can
	// travel on the file node; the same window then seeds the chunker.
	admitted := !p.opts.MaxSearchFileBytes.Exceeded(fv.Size)
	var ch *chunker
	binary := false
	if admitted {
		if ch, err = newChunker(rc, uint64(fv.Size)); err != nil {
			return model.ProviderResult{}, err
		}
		binary = looksBinary(ch.head())
	}

	fileNode, err := p.structure(ctx, e, cls, binary)
	if err != nil {
		return model.ProviderResult{}, err
	}
	e.Capability(CapabilityStructure, model.CapabilityFresh, "")
	if err := e.Flush(ctx); err != nil {
		return model.ProviderResult{}, err
	}

	switch {
	case !admitted:
		e.Capability(CapabilitySearch, model.CapabilityUnavailable, model.CodeResourceLimit)
	case binary:
		e.Capability(CapabilitySearch, model.CapabilityUnavailable, model.CodeProviderUnavailable)
	default:
		lost, err := ch.each(ctx, func(r model.ByteRange, body []byte) error {
			return e.PutSearch(ctx, chunkDocument(fv, fileNode.ID, r, body))
		})
		if err != nil {
			return model.ProviderResult{}, err
		}
		e.AddBytes(ch.consumed)
		// Every byte of the file is indexed, so the capability is fresh. A
		// file that is not UTF-8 throughout says so in the detail -- the
		// state must be recorded first, because a detail without its state
		// is dropped.
		e.Capability(CapabilitySearch, model.CapabilityFresh, "")
		if lost.Bytes > 0 {
			e.CapabilityDetail(CapabilitySearch, "lossy_utf8_chunks", strconv.Itoa(lost.Chunks))
			e.CapabilityDetail(CapabilitySearch, "lossy_utf8_bytes", strconv.Itoa(lost.Bytes))
		}
		if lost.NULs > 0 {
			e.CapabilityDetail(CapabilitySearch, "nul_bytes", strconv.Itoa(lost.NULs))
		}
	}
	return e.Result(), nil
}

// structure emits the repository, every ancestor directory, the file node
// and the `contains` chain between them, then the node a recognized format
// defines and, for a README, the `documents` relation to its directory.
func (p *Provider) structure(ctx context.Context, e *Emitter, cls Classification, binary bool) (model.Node, error) {
	fv := e.File()
	syntax := Attrs{Precision: model.PrecisionSyntax}
	parent, err := e.Node(ctx, PathCandidate(ID, model.NodeRepository, "."), syntax)
	if err != nil {
		return model.Node{}, err
	}
	for _, dir := range ancestors(fv.Path) {
		d, err := e.Node(ctx, PathCandidate(ID, model.NodeDirectory, dir), syntax)
		if err != nil {
			return model.Node{}, err
		}
		if err := e.Relation(parent.ID, model.RelContains, d.ID, syntax); err != nil {
			return model.Node{}, err
		}
		parent = d
	}
	meta := map[string]any{"size": fv.Size, "executable": fv.Executable, "binary": binary}
	if cls.Format != "" {
		meta["format"] = cls.Format
	}
	file, err := e.Node(ctx, PathCandidate(ID, model.NodeFile, fv.Path), Attrs{Precision: model.PrecisionSyntax, Located: true,
		Detail: Detail("size", strconv.FormatInt(fv.Size, 10)), Metadata: Metadata(meta)})
	if err != nil {
		return model.Node{}, err
	}
	if err := e.Relation(parent.ID, model.RelContains, file.ID, syntax); err != nil {
		return model.Node{}, err
	}
	if cls.Node == "" {
		return file, nil
	}
	// Recognition by path is a heuristic about what the file is, not a parse
	// of it; the precision says so.
	heuristic := Attrs{Precision: model.PrecisionHeuristic}
	defined, err := e.Node(ctx, PathCandidate(ID, cls.Node, fv.Path), Attrs{Precision: model.PrecisionHeuristic, Located: true,
		Metadata: Metadata(map[string]any{"format": cls.Format})})
	if err != nil {
		return model.Node{}, err
	}
	if err := e.Relation(file.ID, model.RelDefines, defined.ID, heuristic); err != nil {
		return model.Node{}, err
	}
	e.Symbol(defined, nil)
	if cls.Node == model.NodeDocument && IsReadme(fv.Path) {
		if err := e.Relation(defined.ID, model.RelDocuments, parent.ID, heuristic); err != nil {
			return model.Node{}, err
		}
	}
	return file, nil
}

// ancestors lists the directories above a path, outermost first.
func ancestors(rel string) []string {
	var out []string
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			out = append(out, rel[:i])
		}
	}
	return out
}
