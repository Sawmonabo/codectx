package config

import (
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Hash domains for the three separate fingerprints of Section 20.2. The version
// suffix is part of the domain, so changing what a fingerprint covers yields a
// disjoint identity space instead of silently reinterpreting stored hashes.
const (
	domainSourcePolicy   = "source-policy-v1"
	domainAnalysisConfig = "analysis-config-v1"
	domainContextPolicy  = "context-policy-v1"
	domainWorkspaceKey   = "workspace-key-v1"
)

// The domains keep their v1 suffix through the unlimited-defaults change: the
// SET of components each fingerprint covers is unchanged, only the value of
// some of them and the spelling an absent bound renders to. That is a
// configuration change like any other, which is exactly what these hashes exist
// to detect; a new domain would instead declare the old hashes uninterpretable.

// SourcePolicyHash is the source eligibility and byte policy that feeds
// SnapshotID: exactly the settings that decide which files are captured. An
// analysis admission limit is deliberately absent, because raising it must not
// invalidate a capture whose bytes are unchanged.
func (c Config) SourcePolicyHash() string {
	return model.H(domainSourcePolicy,
		quoteBool(c.Workspace.FollowSymlinks),
		quoteBool(c.Workspace.IncludeUntracked),
		quoteBool(c.Workspace.IndexGenerated),
		quoteBool(c.Workspace.IndexVendor),
		quoteLimit(c.Workspace.MaxFiles),
		// The toggles above select built-in classification lists, so the lists
		// themselves are policy: a build shipping a different one captures a
		// different set of files from the same bytes.
		workspace.ExclusionDigest(),
	)
}

// AnalysisConfigHash is the analyzer, grammar and admission policy that feeds
// UnitID and AnalysisKey. It includes the parse and search admission limits
// because a unit produced under a lower limit is not the same result as one
// produced under a higher limit, and Section 20.2 forbids silently reusing a
// complete result produced under a different effective requirement.
//
// Operational settings -- worker counts, batch sizes, idle TTLs, logging -- are
// excluded: they change how the work is scheduled, never what it concludes.
func (c Config) AnalysisConfigHash() string {
	h := model.NewHasher(domainAnalysisConfig)
	h.AddString(quoteLimit(c.Workspace.MaxParseFileBytes))
	h.AddString(quoteLimit(c.Workspace.MaxSearchFileBytes))
	h.AddString(quoteBool(c.Providers.TreeSitter.Enabled))
	// The language list is a set: two files differing only in its order select
	// the same grammars and must not invalidate every unit.
	languages := append([]string(nil), c.Providers.TreeSitter.Languages...)
	sort.Strings(languages)
	h.AddString(quoteInt(int64(len(languages))))
	for _, lang := range languages {
		h.AddString(lang)
	}
	h.AddString(c.Providers.SCIP.Enabled.String())
	// The three SCIP bounds that change which facts are emitted, exactly as
	// workspace.max_parse_file_bytes above does: a user-set value leaves a
	// document's source unread, leaves a file out of the private
	// materialization, or leaves a C/C++ compilation database un-normalized,
	// which costs that unit its whole run. The other four providers.scip
	// bounds are reporting thresholds that cut nothing, so they are
	// deliberately absent: adjusting one must not invalidate an index.
	h.AddString(quoteLimit(c.Providers.SCIP.MaxSourceFileBytes))
	h.AddString(quoteLimit(c.Providers.SCIP.MaxMaterializeBytes))
	h.AddString(quoteLimit(c.Providers.SCIP.MaxManifestBytes))
	h.AddString(c.Providers.LSP.Enabled.String())
	h.AddString(c.Providers.Dependence.Enabled.String())
	return h.Sum()
}

// ContextPolicyHash is the context ranking and budget policy that feeds
// ManifestID. Changing it changes which entries a manifest contains and in what
// order, so a manifest compiled under a different policy is a different result.
func (c Config) ContextPolicyHash() string {
	return model.H(domainContextPolicy,
		c.Context.DefaultPhase,
		quoteInt(c.Context.DefaultEstimatedTokens),
		quoteInt(c.Context.DefaultMaxBytes),
		quoteInt(int64(c.Context.DefaultMaxFiles)),
		quoteInt(int64(c.Context.MaxSlices)),
		quoteLimit(c.Context.MaxGraphDepth),
		quoteLimit(c.Context.MaxVisitedNodes),
		quoteLimit(c.Context.MaxGraphEdges),
		quoteLimit(c.Context.MaxReasonPathsPerEntry),
		quoteLimit(c.Context.MaxManifestBytes),
		quoteLimit(c.Context.MaxCapsuleBytes),
		quoteBool(c.Context.StrictReadGate),
		quoteBool(c.Context.AllowExploratoryWaiverConsolidation),
		quoteLimit(c.Context.MaxSeeds),
	)
}
