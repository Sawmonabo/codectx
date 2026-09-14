# CodeContext (`codectx`) Design Specification and Implementation Plan

> **For agentic workers:** Implement the complete design and the task plan together. Use `superpowers:subagent-driven-development` or `superpowers:executing-plans` when the execution environment supports it. Every worker is bound by Section 5.1 and every task begins with the read/trace/reuse gate in Section 30.1. A worker report is not verification evidence.

**Status:** Revised greenfield design and implementation baseline; application behavior and performance require implementation evidence.  
**Revision date:** 2026-09-04  
**Supersedes:** The 2026-09-03 combined specification, in full.  
**Working module path:** `github.com/Sawmonabo/codectx`  
**Project license:** Apache License 2.0  
**Goal:** Build a deterministic codebase-intelligence and context-management system that indexes software repositories, exposes precise structural and semantic queries through a Go CLI and MCP server, and compiles task-specific, snapshot-pinned context for AI agents without requiring any paid service, AI API, embedding API, cloud database, or token purchase.

**Architecture:** A Go application with immutable source snapshots, reusable immutable analysis units, generation-qualified canonical facts, SQLite/FTS5, bounded local analyzer workers, a query engine, a deterministic context compiler, and an actor-scoped source-coverage and workflow gate. CLI and MCP share typed application services.

**Tech stack:** Go 1.27 language baseline with Go 1.27.1 as the initial exact toolchain pin; official MCP Go SDK v1.7.0 with MCP 2026-07-28; `modernc.org/sqlite` v1.58.0 as the initial driver pin; SQLite FTS5; official Tree-sitter Go bindings and pinned grammars; SCIP protobuf; local LSP; optional local Joern; Git; `fsnotify`; Cobra; TOML; Go standard-library cryptography, logging, process, I/O, and testing packages. Go and MCP pins are supported by the published release records, not inferred from version numbers. [1](#ref-1) [2](#ref-2) [22](#ref-22)

**Reading this revision:** All 33 design sections and all 21 implementation tasks are included. The product scope remains intact. Changes remove contradictory migration requirements, avoid whole-graph duplication, specify measurable aggregate resource budgets, close source/provenance/workflow gaps, and repair implementation dependencies. Performance figures are engineering acceptance targets, not measured claims. The supplied material is a specification, not an application repository; no claim of a bug-free executable is made.

---

## Table of Contents

- [1. Executive Summary](#1-executive-summary)
- [2. Vision and Project Goal](#2-vision-and-project-goal)
   - [Vision](#vision)
   - [Project goal](#project-goal)
   - [Success statement](#success-statement)
- [3. Problem Statement](#3-problem-statement)
- [4. Scope](#4-scope)
   - [4.1 Required Outcomes](#41-required-outcomes)
   - [4.2 Core and Enhanced Local Capabilities](#42-core-and-enhanced-local-capabilities)
   - [4.3 Non-Goals](#43-non-goals)
- [5. Design Principles](#5-design-principles)
   - [5.1 Mandatory Engineering and Agent Policy](#51-mandatory-engineering-and-agent-policy)
   - [5.2 Product Design Principles](#52-product-design-principles)
- [6. Global Constraints and Invariants](#6-global-constraints-and-invariants)
- [7. Architecture Overview](#7-architecture-overview)
   - [7.1 Boundaries and Ownership](#71-boundaries-and-ownership)
   - [7.2 Process Model](#72-process-model)
- [8. Primary Runtime Flows](#8-primary-runtime-flows)
   - [8.1 Cold Index](#81-cold-index)
   - [8.2 Incremental Refresh](#82-incremental-refresh)
   - [8.3 Agent Context Request](#83-agent-context-request)
- [9. Canonical Domain Model](#9-canonical-domain-model)
   - [9.1 Identifiers and Semantic Bindings](#91-identifiers-and-semantic-bindings)
   - [9.2 Nodes, Relations, and Occurrences](#92-nodes-relations-and-occurrences)
   - [9.3 Evidence, Precision, and Source Coordinates](#93-evidence-precision-and-source-coordinates)
   - [9.4 Identity Reconciliation and Reuse Safety](#94-identity-reconciliation-and-reuse-safety)
- [10. Repository Snapshots and Content Retention](#10-repository-snapshots-and-content-retention)
   - [10.1 Capture Contract](#101-capture-contract)
   - [10.2 Consistent Capture and Safe Access](#102-consistent-capture-and-safe-access)
   - [10.3 CAS Integrity Without Quadratic Source Reads](#103-cas-integrity-without-quadratic-source-reads)
   - [10.4 Materialization, Repair, and Retention](#104-materialization-repair-and-retention)
- [11. Provider Architecture](#11-provider-architecture)
   - [11.1 Provider Contract and Unit Ownership](#111-provider-contract-and-unit-ownership)
   - [11.2 Filesystem, Documentation, and Manifest Providers](#112-filesystem-documentation-and-manifest-providers)
   - [11.3 Tree-sitter Structural Provider](#113-tree-sitter-structural-provider)
   - [11.4 SCIP Import and Managed Indexers](#114-scip-import-and-managed-indexers)
   - [11.5 LSP Snapshot-Qualified Working-Tree Overlay](#115-lsp-snapshot-qualified-working-tree-overlay)
   - [11.6 Dependence Provider](#116-dependence-provider)
   - [11.7 Managed Analyzer Toolchain](#117-managed-analyzer-toolchain)
- [12. Storage Architecture](#12-storage-architecture)
   - [12.1 SQLite Operating Model](#121-sqlite-operating-model)
   - [12.2 Current Schema and Reusable Units](#122-current-schema-and-reusable-units)
   - [12.3 Generation Activation and Failure Isolation](#123-generation-activation-and-failure-isolation)
   - [12.4 Search Isolation, Storage Growth, and Cleanup](#124-search-isolation-storage-growth-and-cleanup)
- [13. Indexing Coordinator and Freshness](#13-indexing-coordinator-and-freshness)
   - [13.1 Scheduling and Invalidation](#131-scheduling-and-invalidation)
   - [13.2 Watch Mode and Cross-Process Coordination](#132-watch-mode-and-cross-process-coordination)
   - [13.3 Capability Freshness](#133-capability-freshness)
- [14. Search and Query Engine](#14-search-and-query-engine)
   - [14.1 Typed Requests and Bounded Results](#141-typed-requests-and-bounded-results)
   - [14.2 Retrieval and Deterministic Ranking](#142-retrieval-and-deterministic-ranking)
   - [14.3 Graph Queries and Impact](#143-graph-queries-and-impact)
   - [14.4 Cursor and Cache Contracts](#144-cursor-and-cache-contracts)
- [15. Deterministic Context Compiler](#15-deterministic-context-compiler)
   - [15.1 Inputs, Output, and Canonical Identity](#151-inputs-output-and-canonical-identity)
   - [15.2 Seed Discovery and Required Scope](#152-seed-discovery-and-required-scope)
   - [15.3 Graph Expansion and Ranking](#153-graph-expansion-and-ranking)
   - [15.4 Budgeting and Context Slices](#154-budgeting-and-context-slices)
- [16. Source-Coverage Gate](#16-source-coverage-gate)
   - [16.1 Honest Actor-Scoped Coverage](#161-honest-actor-scoped-coverage)
   - [16.2 Source Read and Receipt Contracts](#162-source-read-and-receipt-contracts)
   - [16.3 Interval Merging, Empty Files, and Readiness](#163-interval-merging-empty-files-and-readiness)
- [17. Workflow State Machine](#17-workflow-state-machine)
   - [17.1 States and Guards](#171-states-and-guards)
   - [17.2 Observations and Source-Backed Review](#172-observations-and-source-backed-review)
   - [17.3 Waivers and Deterministic Capsules](#173-waivers-and-deterministic-capsules)
- [18. CLI Contract](#18-cli-contract)
   - [18.1 Commands and Shared Options](#181-commands-and-shared-options)
   - [18.2 Output and Exit Codes](#182-output-and-exit-codes)
- [19. MCP Contract](#19-mcp-contract)
   - [19.1 Official SDK and Protocol Baseline](#191-official-sdk-and-protocol-baseline)
   - [19.2 Required Tool Surface](#192-required-tool-surface)
   - [19.3 Transport and Safety Rules](#193-transport-and-safety-rules)
- [20. Configuration](#20-configuration)
   - [20.1 Defaults and Resource Controls](#201-defaults-and-resource-controls)
   - [20.2 Trust and Fingerprints](#202-trust-and-fingerprints)
- [21. Security and Privacy](#21-security-and-privacy)
- [22. Reliability, Errors, and Observability](#22-reliability-errors-and-observability)
- [23. Performance and Scalability Targets](#23-performance-and-scalability-targets)
   - [23.1 Measurement Contract and Capability Preservation](#231-measurement-contract-and-capability-preservation)
   - [23.2 Initial Release Budgets](#232-initial-release-budgets)
   - [23.3 Memory Admission and Allocation Discipline](#233-memory-admission-and-allocation-discipline)
   - [23.4 Required Hot-Path Design](#234-required-hot-path-design)
   - [23.5 Regression and Release Policy](#235-regression-and-release-policy)
- [24. Packaging and Deployment](#24-packaging-and-deployment)
- [25. Testing Strategy](#25-testing-strategy)
   - [25.1 Minimum Critical Test Set](#251-minimum-critical-test-set)
   - [25.2 Execution Discipline](#252-execution-discipline)
- [26. Acceptance Criteria](#26-acceptance-criteria)
- [27. Risks and Mitigations](#27-risks-and-mitigations)
- [28. Repository Structure](#28-repository-structure)
- [29. Build-versus-Adopt Decisions](#29-build-versus-adopt-decisions)
- [30. Implementation Plan](#30-implementation-plan)
   - [30.1 Execution Model, Read Gate, and Dependency DAG](#301-execution-model-read-gate-and-dependency-dag)
   - [30.2 Milestones and Shared Verification Assets](#302-milestones-and-shared-verification-assets)
   - [Task 1: Bootstrap the Go Module, CLI Shell, and Dependency Policy](#task-1-bootstrap-the-go-module-cli-shell-and-dependency-policy)
   - [Task 2: Implement Canonical Model Types and Deterministic Identities](#task-2-implement-canonical-model-types-and-deterministic-identities)
   - [Task 3: Implement Configuration, Workspace Discovery, Path Safety, and the Shared Process Runner](#task-3-implement-configuration-workspace-discovery-path-safety-and-the-shared-process-runner)
   - [Task 4: Build Exact Git/Worktree Snapshots and the Local Content-Addressed Store](#task-4-build-exact-gitworktree-snapshots-and-the-local-content-addressed-store)
   - [Task 5: Implement the Current SQLite Schema, Reusable Units, Leases, and Atomic Activation](#task-5-implement-the-current-sqlite-schema-reusable-units-leases-and-atomic-activation)
   - [Task 6: Implement Provider Contracts, Registry, Byte-Bounded Sink, and Deterministic Resolution](#task-6-implement-provider-contracts-registry-byte-bounded-sink-and-deterministic-resolution)
   - [Task 7: Index Files, Documentation, Build Metadata, and Package Manifests](#task-7-index-files-documentation-build-metadata-and-package-manifests)
   - [Task 8: Implement the Tree-sitter Structural Provider](#task-8-implement-the-tree-sitter-structural-provider)
   - [Task 9: Implement Streaming SCIP Import and Explicit Local Indexer Profiles](#task-9-implement-streaming-scip-import-and-explicit-local-indexer-profiles)
   - [Task 10: Implement the LSP Snapshot-Qualified Working-Tree Overlay](#task-10-implement-the-lsp-snapshot-qualified-working-tree-overlay)
   - [Task 11: Implement the Dependence Provider over the Managed Graph Engine](#task-11-implement-the-dependence-provider-over-the-managed-graph-engine)
   - [Task 12: Implement the Index Coordinator, Incremental Invalidation, Watch Mode, and Atomic Refresh](#task-12-implement-the-index-coordinator-incremental-invalidation-watch-mode-and-atomic-refresh)
   - [Task 13: Implement Exact, Symbol, Path, and Generation-Scoped FTS5 Search](#task-13-implement-exact-symbol-path-and-generation-scoped-fts5-search)
   - [Task 14: Implement Bounded Graph Traversal, Dependency Paths, and Impact Analysis](#task-14-implement-bounded-graph-traversal-dependency-paths-and-impact-analysis)
   - [Task 15: Implement the Deterministic Context Compiler, Required Scope, Budgeting, and Slicing](#task-15-implement-the-deterministic-context-compiler-required-scope-budgeting-and-slicing)
   - [Task 16: Implement Snapshot-Pinned Source Serving and Actor-Scoped Receipt Coverage](#task-16-implement-snapshot-pinned-source-serving-and-actor-scoped-receipt-coverage)
   - [Task 17: Implement Workflow Guards, Scope Review, Deterministic Capsules, and Shared Service Contracts](#task-17-implement-workflow-guards-scope-review-deterministic-capsules-and-shared-service-contracts)
   - [Task 18: Implement the Human-Readable and Machine-Stable CLI](#task-18-implement-the-human-readable-and-machine-stable-cli)
   - [Task 19: Implement the MCP Server and Typed Current-Protocol Tool Contracts](#task-19-implement-the-mcp-server-and-typed-current-protocol-tool-contracts)
   - [Task 20: Complete Diagnostics, Retention, Resource Accounting, and Integrated Fault Isolation](#task-20-complete-diagnostics-retention-resource-accounting-and-integrated-fault-isolation)
   - [Task 21: Complete End-to-End, Performance, Offline, Documentation, Packaging, and Release Gates](#task-21-complete-end-to-end-performance-offline-documentation-packaging-and-release-gates)
   - [Task 22: Implement the Managed Analyzer Toolchain, Lock, Store, and Full Language Matrix](#task-22-implement-the-managed-analyzer-toolchain-lock-store-and-full-language-matrix)
   - [Task 23: Implement Early Cutoff on Declaration-Only Signature Digests](#task-23-implement-early-cutoff-on-declaration-only-signature-digests)
- [31. Verification Matrix](#31-verification-matrix)
- [32. Definition of Done](#32-definition-of-done)
- [33. Authoritative References](#33-authoritative-references)

---
<a id="1-executive-summary"></a>
## 1. Executive Summary

`codectx` is not another chat client and not another vector-RAG wrapper. It is a deterministic compiler from:

```text
repository source
+ exact repository snapshot
+ analyzer facts and provenance
+ task or explicit query
+ workflow phase
+ context budget

               ↓

a bounded, explainable, snapshot-pinned context manifest
```

The system gives AI agents a reliable substrate for answering questions such as:

- Where is a symbol defined, referenced, implemented, or tested?
- Which functions call this function, and what does it call?
- What files, contracts, configurations, tests, and documentation are likely affected by a change?
- Which exact source files must be served in full before an implementation decision is considered context-complete?
- What context belongs in discovery, verification, or consolidation?

The entire core runs locally. It has no OpenAI, Anthropic, Gemini, Voyage, Cohere, Pinecone, hosted graph database, or cloud-vector-store dependency. No API key is accepted or required by the core configuration. An AI coding agent may consume `codectx` through MCP, but `codectx` itself performs deterministic indexing, retrieval, ranking, graph traversal, and context assembly.

The platform is written in Go. Mature compiler and analysis systems are integrated through narrow provider boundaries rather than rewritten:

- Tree-sitter supplies incremental syntax trees.
- SCIP supplies compiler-derived definitions, references, occurrences, and relationships.
- LSP supplies live working-tree semantics when a local language server is available.
- A managed dependence engine supplies on-demand control-dependence, data-dependence and fallback call facts for all nine languages.

All provider facts are normalized into a canonical, generation-qualified graph with source provenance. Sealed analysis units are reused without copying unchanged facts into each generation. Optional-provider failure degrades the affected capability; required-provider failure preserves the last usable generation. Native parser work is isolated from the serving process. These guarantees require the failure and resource checks specified below.

---

<a id="2-vision-and-project-goal"></a>
## 2. Vision and Project Goal

<a id="vision"></a>
### Vision

Give every software-development agent a precise, inspectable, local knowledge layer that understands the structure and relationships of an entire codebase without forcing the agent to rediscover the repository through repeated grep calls or to trust opaque LLM-generated summaries.

The long-term product should feel like a compiler-backed “memory and navigation system” for code:

```text
Index once.
Refresh incrementally.
Query semantically.
Inspect provenance.
Compile only the context the current task requires.
```

<a id="project-goal"></a>
### Project goal

Deliver a production-ready Go CLI and MCP server that can:

1. Index a local Git repository and its uncommitted working tree.
2. Preserve the exact content associated with an indexed snapshot.
3. Build a canonical graph of files, packages, modules, symbols, calls, references, tests, manifests, configuration, and documentation.
4. Combine facts from multiple local analyzers while retaining origin, precision, freshness, and source ranges.
5. Answer bounded symbol, graph, impact, and architecture queries.
6. Convert a task into a deterministic context manifest and dependency-coherent slices.
7. Track which required source ranges were served to an agent session.
8. Expose identical behavior through a human CLI, stable JSON CLI output, and MCP tools.
9. Work with no AI model, no AI token purchase, no paid third-party service, and no mandatory network access at runtime.

<a id="success-statement"></a>
### Success statement

A developer must be able to build or install the binary, enter a repository, and run:

```bash
codectx index .
codectx context plan --task "Add retry semantics to PaymentService.Authorize" --phase verify --actor lead-session-1
codectx mcp serve --repo .
```

without entering an API key or connecting to a hosted service. If only the bundled structural providers are available, the commands still work and report their precision. When the managed SCIP, LSP, or dependence engines are available, the same graph and query APIs become richer without changing the agent integration.

---

<a id="3-problem-statement"></a>
## 3. Problem Statement

Current coding-agent repository exploration commonly fails in five ways:

1. **Text search is mistaken for understanding.** Grep and ripgrep find character sequences, not symbol identity, type relationships, call edges, or data flow.
2. **Each subagent rediscovers the codebase.** Parallel agents spend tokens and time rebuilding overlapping, inconsistent mental models.
3. **Context selection is opaque.** Embedding similarity or agent intuition determines which snippets are loaded, often without explaining why adjacent contracts and tests were omitted.
4. **Repository state drifts.** A result can refer to a prior commit or stale index while the agent reads the current working tree.
5. **“Read the full file” is only a prompt request.** There is no machine-readable record of which exact snapshot ranges were served.

`codectx` solves these by separating discovery, evidence, storage, context compilation, and agent reasoning. The canonical graph is deterministic and inspectable. LLM reasoning occurs above the system, not inside its correctness path.

---

<a id="4-scope"></a>
## 4. Scope

<a id="41-required-outcomes"></a>
### 4.1 Required Outcomes

The first generally usable release shall provide:

- Local repository discovery and ignore processing.
- Git commit plus dirty/untracked overlay snapshots.
- Immutable source access for retained snapshots.
- Structural facts for Go, JavaScript, TypeScript/TSX, Python, Java, Rust, C, and C++ through pinned Tree-sitter grammars.
- Generic SCIP protobuf import plus configurable local SCIP indexer profiles.
- Optional LSP live-overlay support over local subprocesses.
- On-demand dependence analysis (control and data dependence) over a managed local engine.
- Common manifest extraction for `go.mod`, `package.json`, `pyproject.toml`, `Cargo.toml`, and `pom.xml`.
- Markdown and plain-text documentation indexing.
- A canonical graph with evidence and precision classes.
- SQLite persistence and FTS5 lexical retrieval.
- Exact-symbol, definition, reference, implementation, caller, callee, dependency, test, configuration, documentation, path, and impact queries.
- Deterministic context plans for `sweep`, `verify`, and `consolidate` phases.
- Context slicing when required context exceeds a budget.
- Served-range coverage tracking with snapshot hashes.
- Human CLI, stable `--json` CLI, and MCP stdio server.
- Watch mode plus periodic reconciliation.
- Diagnostics, health reporting, bounded worker pools, timeouts, and graceful degradation.

<a id="42-core-and-enhanced-local-capabilities"></a>
### 4.2 Core and Enhanced Local Capabilities

The installation is layered so absence of a heavyweight analyzer never makes the system unusable.

| Layer | Capability | Required for core runtime | Paid service or API key |
|---|---|---:|---:|
| Go core | snapshots, graph, storage, search, context compiler, coverage, CLI, MCP | Yes | No |
| Bundled Tree-sitter grammars | syntax, symbols, imports, structural calls | Yes | No |
| Manifest/docs providers | packages, dependencies, configs, docs | Yes | No |
| Local SCIP indexers | precise definitions/references/relationships | No; strongly recommended | No |
| Local language servers | snapshot-qualified dirty-worktree semantic overlay | No | No |
| Local dependence engine | control/data dependence and fallback call facts, on demand | No | No |
| Local embedding plugin | conceptual retrieval enhancement | Outside core | No, if self-hosted |
| Cloud AI or embedding provider | optional future plugin only | Never | Optional and non-core |

“Optional” means the provider may be absent while every core command remains operational. It does not mean the architecture defers the provider contract or integration path.

<a id="43-non-goals"></a>
### 4.3 Non-Goals

The initial product shall not:

- Generate, edit, or patch repository code on behalf of the agent, or act as a general build/execution tool. A managed semantic indexer may need compiler processing; that activity runs against a private snapshot materialization under the tool lock policy of Section 11.7, not silently treated as safe parsing.
- Include an LLM, chat UI, prompt engine, or autonomous coding loop.
- Require embeddings or a vector database.
- Claim that static analysis captures all dynamic dispatch, reflection, generated runtime behavior, or remote service behavior.
- Claim that a served source range proves an AI model cognitively read or understood it.
- Become a distributed, multi-tenant SaaS platform in the first release.
- Execute any analyzer that is not a lock-pinned, checksum-verified managed tool or an explicit user-level override, or fetch anything the embedded tool lock does not name.
- Treat provider-generated natural-language summaries as canonical truth.
- Follow symlinks outside the repository root by default.
- Index binaries, build outputs, package caches, vendored dependencies, or generated trees by default.

---

<a id="5-design-principles"></a>
## 5. Design Principles

<a id="51-mandatory-engineering-and-agent-policy"></a>
### 5.1 Mandatory Engineering and Agent Policy

> Use grep/ripgrep only for discovery and navigation; search results are not a substitute for understanding the code. Before making any architectural, behavioral, or implementation decision, you and every dispatched subagent must open and fully read the relevant files, then trace and understand their callers, contracts, state, dependencies, and integration boundaries before modifying anything. Do not over-engineer: this is a greenfield application, so no backward compatibility, migration layers, transitional shims, or legacy-preservation work is required. Fix and update the architecture and implementation in place, remove obsolete paths, and ensure no dead consumers or unreachable code remain. Keep tests to the absolute minimum, adding only those that protect genuinely critical behavior, and avoid AI-generated code slop, speculative infrastructure, needless indirection, and premature abstractions; use the least amount of clear, production-quality code necessary while following modern best practices. Always investigate whether an existing shared utility, helper, component, or abstraction already solves the problem before creating new code, reuse existing implementations whenever appropriate, and when duplicate behavior begins to emerge, consolidate it into a common reusable utility rather than allowing parallel implementations to drift.

This is a normative execution contract, not optional advice. It applies to the lead implementer, every dispatched subagent, reviewers, and repair work discovered during another task. Read whole relevant files in bounded consecutive segments when necessary, not just search hits, declarations, or the edited function. Trace both direct and indirect consumers until the affected contract boundary is understood. Existing shared helpers must be inspected in full before deciding whether to reuse or change them.

Before a design or implementation decision, record a short task-local evidence note: complete files read and their revision, caller/consumer path, contract and state ownership, dependency/integration boundaries, and existing implementation considered for reuse. A nonexistent greenfield file is not a file that can be read; read its already-existing producers, consumers, governing specification, and shared implementations instead. Do not invent evidence or a repository that was not supplied.

Subagent handoffs include the exact policy above, the task scope, the pinned revision, relevant files and known integration boundaries, resource limits, and existing helpers. The lead agent verifies the changed files and callers independently before integration. Search output, a subagent's assurance, a passing test, or an LLM summary alone cannot satisfy this gate. No inference that an agent cognitively understood source is made by the product.

<a id="52-product-design-principles"></a>
### 5.2 Product Design Principles

1. **Deterministic core, probabilistic consumer.** Identity, provenance, traversal, ranking, context assembly, and workflow rules are deterministic for identical semantic inputs.
2. **Evidence before assertion.** Node attributes as well as relationships retain producing unit, provider/run, source hash/range, and precision. Conflicts remain inspectable.
3. **Search discovers; full source verifies.** Search and graph tools locate relevant files. Strict verify requires complete selected source and a separately labeled actor review record.
4. **Snapshot pinning everywhere.** Every result binds to an immutable snapshot and generation. Stable entity keys never authorize use outside that binding.
5. **Fast by avoiding unnecessary work.** Reuse unchanged analysis units and source blobs; stream inputs; bound memory; use indexed queries; avoid duplicate parsing, whole-repository copies, and per-result database calls.
6. **Capability-preserving resource control.** Apply admission, backpressure, cache eviction, sequential execution, and bounded disk spooling before rejecting work. Never silently omit facts, languages, source, or evidence to meet a benchmark.
7. **No provider lock-in or hidden network requirement.** Canonical Go contracts do not expose analyzer-native types. The only network the product performs is fetching lock-pinned, checksum-verified analyzer payloads into its private tool store; `tools.offline = true` or the offline bundle removes even that.
8. **Read-only repository behavior.** Source writes, project hooks, and arbitrary commands are not core features. All state and private analyzer workspaces live under the private data directory.
9. **Small, explicit abstractions.** One existing owner per behavior. Interfaces exist at real substitution, process, persistence, or consumer boundaries, not around every function.
10. **Clean-room and maintainable implementation.** Adopt mature permissively licensed components. Do not copy restricted competitors. Keep tests focused on critical invariants; do not build a test framework or generate getter/mock boilerplate.

A product feature is not a justification for speculative infrastructure. No distributed queues, service mesh, plugin marketplace, universal dependency-injection framework, compatibility facade, migration engine, or generic agent framework is part of V1.

---

<a id="6-global-constraints-and-invariants"></a>
## 6. Global Constraints and Invariants

These constraints apply to every task and to all dispatched workers.

| Area | Mandatory invariant |
|---|---|
| Execution | Section 5.1 is enforced before decisions and edits; whole-file reading, caller/state/contract tracing, and reuse inspection are required for every actor. |
| Language and licensing | All product-owned production and automation logic is Go; module language baseline `go 1.27`, initial CI/release toolchain `go1.27.1`, Apache-2.0 project license. SQL, TOML, CI YAML, fixture languages, and embedded grammar/query data are declarative resources, not prohibited product executables. |
| Greenfield | Change architecture and code in place, update all consumers and docs, remove obsolete paths. No backward-compatibility work, migration layers, transitional shims, dual APIs, or legacy preservation. |
| External dependencies | Pin exact tested versions/checksums. No moving `latest`, unbounded minimum-version promises, or execution of an unpinned tool. Analyzer payloads and their runtimes are fetched only from the embedded tool lock and verified before use (Section 11.7). |
| No paid dependencies | No AI API keys, paid service, cloud database, runtime telemetry endpoint, or outbound core network operation other than lock-pinned tool fetches through the one toolchain owner. Analyzer-internal networking is declared per profile and reported. |
| Repository safety | No source writes; safe root-relative opens, not string validation alone. Analyzer materializations cannot share writable hard links with source or CAS. |
| Memory | No whole-repository file list, graph, protobuf index, token corpus, or result set retained in Go heap. Memory admission counts bytes, concurrency, parser processes, SQLite buffers, caches, and output serialization. |
| Resource limits | Every queue, batch, cache, record, request, traversal, process, temporary tree, and response has an explicit finite bound. Limits never masquerade as complete results. |
| Indexing | One cross-process indexing owner per workspace; completed immutable units may be reused only when their entire input/dependency fingerprint matches. Failed or unsealed units are invisible. |
| Publication | The active-generation pointer changes atomically only after validation. Optional failure cannot leak partial invalid facts or promote stale evidence as fresh. |
| Queries | Pin a generation and retention lease at request start; all reads, pages, evidence, and source references use that binding. Public lists are paginated and all graph work is bounded. |
| Determinism | Canonical hashes exclude operational IDs/timestamps where appropriate and include all semantic configuration, analyzer versions, unit inputs, ranking versions, and policy inputs. Another generation's FTS contents cannot change a pinned result. |
| Source | Retain every eligible nondeleted file in CAS. Serving never falls back to the live checkout. Byte positions are authoritative; returned encoding must preserve bytes exactly. |
| Coverage | Coverage belongs to one actor session and source hash. No receipt sharing across subagents, no search-snippet credit, no acknowledgment substituted for comprehension, and no strict readiness obtained through a waiver. |
| Freshness | Historical read completeness is distinct from permission to modify the current worktree. A superseded or changed scope must be revalidated before the orchestrator writes. |
| Storage | Current-schema initialization only; schema mismatch requires an explicit rebuild decision, never an automatic destructive reset. SQLite foreign keys, WAL, durable state writes, bounded connection pools, and short transactions are mandatory. |
| Privacy | No source bodies, inherited secrets, environment values, raw analyzer diagnostics, or access tokens in ordinary logs. |
| Verification | Use the smallest set of tests that protects critical behavior, shared fixtures, actual protocol tests, and measured resource gates. Do not replace evidence with a coverage-percentage target. |
| Completion | No known unaddressed defect within the implemented scope, dead consumer, unreachable production path, placeholder provider, unbounded operation, or undocumented capability loss is acceptable. |

Go toolchain release facts are documented in [1](#ref-1) and [32](#ref-32). SQLite and runtime controls are applied with the limitations described in Sections 12 and 23; a soft runtime limit is not an operating-system memory sandbox.

---

<a id="7-architecture-overview"></a>
## 7. Architecture Overview

```text
                       CLI / MCP stdio clients
                                 |
                      Typed Go application services
                                 |
       +-------------------------+-------------------------+
       |                         |                         |
 Query / graph             Context compiler          Index coordinator
       |                  Coverage / workflow              |
       +-------------------------+-------------------------+
                                 |
            Generation-qualified canonical fact store
                SQLite + FTS5 + immutable unit membership
                                 |
       +-------------------------+-------------------------+
       |                         |                         |
 Immutable source CAS     Normalization/resolution   Provider registry
       |                         |                         |
 Git/worktree capture      Provenance + aliases      Local bounded workers
                                                     /     |      \
                                               Tree-sitter SCIP   dependence
                                               child pool  local  local

 LSP: on-demand, private snapshot-qualified query overlay through the same
 process runner; never an untracked mutation of canonical generation facts.
```

<a id="71-boundaries-and-ownership"></a>
### 7.1 Boundaries and Ownership

`internal/model` contains data contracts and validation, with no dependency on services, providers, or storage. `internal/storage` owns SQL, unit publication, generation pinning, retention metadata, and transactions. Providers own their native formats and emit bounded canonical batches. `internal/process` owns child lifecycle for Git, parser workers, SCIP, LSP, and the dependence engine. `internal/source` owns byte/range/encoding conversion shared across source serving and adapters. CLI and MCP perform request translation and rendering only.

No package outside a provider's implementation imports its Tree-sitter handles, SCIP protobuf, LSP wire structs, or dependence-engine IDs. There is no storage-to-provider import cycle: neutral provider descriptors and run metadata live in the model package. Application composition is concrete; consumers receive only the small service interfaces they actually use. Do not introduce `Graph(...) (any, error)` or an undefined `query` package.

<a id="72-process-model"></a>
### 7.2 Process Model

A normal CLI command is short-lived. `version`, `help`, and config validation do not open the database or load grammars. MCP and watch mode are long-running. Native Tree-sitter parsing occurs in a small pool of private subcommands of the same Go binary; parser handles and file buffers are owned by one worker and are not sent across goroutines. This specific process boundary protects the serving process from native parser crashes and uninterruptible native work; it is not a general distributed-worker architecture.

The parent validates child messages before admitting facts and counts all base-worker RSS in its base memory report. Start workers lazily; stop idle workers. A required parser crash fails the affected staging work after one bounded retry, without replacing the last usable generation. Optional semantic analyzers have their own budgets. SQLite serializes writes across connections; a workspace lock prevents separate CLI/watch/MCP processes from building competing generations. Read commands remain available during indexing.

---

<a id="8-primary-runtime-flows"></a>
## 8. Primary Runtime Flows

<a id="81-cold-index"></a>
### 8.1 Cold Index

```text
Acquire workspace indexing lock and resource reservation
  -> discover eligible paths as a stream
  -> capture every eligible source into CAS; build immutable source manifest
  -> create staging generation and provider plan
  -> execute bounded base units; run managed optional providers
  -> validate/seal each unit and normalize its facts with provenance
  -> build lexical documents once per unit; reconcile deterministic identities
  -> validate generation membership, source bindings, capabilities and FTS
  -> atomically publish active-generation pointer
  -> release temporary files, reservations and lock
```

Clean tracked files and dirty/untracked files follow the same CAS retention path. Git object IDs are provenance and an explicit repair option, never a shortcut that changes the captured bytes. Base-provider failure preserves the previous active generation. Optional failures affect only the capabilities actually requested and attempted.

<a id="82-incremental-refresh"></a>
### 8.2 Incremental Refresh

```text
Watcher hint or periodic filesystem/Git reconciliation
  -> coalesce bounded path set; overflow requests reconciliation
  -> capture exact new source state and validate the capture
  -> compare unit input/dependency fingerprints
  -> reuse matching sealed units by flat generation membership
  -> invalidate affected file/package/workspace/derived units
  -> execute only invalid units, without cloning unchanged facts or FTS bodies
  -> validate complete staging membership and atomically publish
```

A no-change refresh returns the existing source/generation where all semantic inputs match. New events during a refresh are queued for the next pass, not mixed into the running snapshot. Generation membership is a flat indexed relation, not an unbounded chain of base generations. Copying small membership rows in SQLite is permitted; copying the entire graph or source corpus to make a new generation is not.

<a id="83-agent-context-request"></a>
### 8.3 Agent Context Request

```text
Task + explicit seeds + actor identity + phase + resource budget
  -> pin generation and acquire lease
  -> discover exact and lexical candidates
  -> trace bounded callers, contracts, state, dependencies and integrations
  -> compile immutable manifest, ordered slices and missing-scope diagnostics
  -> open an actor-specific session
  -> serve complete selected files as verified bounded chunks
  -> client echoes signed chunk receipts; record confirmed served intervals
  -> actor records source-backed scope review and unresolved items
  -> strict readiness check, including current-source revalidation
  -> orchestrator-controlled edits outside codectx
  -> guarded consolidation and deterministic capsule
```

The parent cannot fulfill a subagent's read requirement by sending a summary or sharing its receipts. Context slices are a delivery mechanism, not permission to decide or edit after reading only the first slice.

---

<a id="9-canonical-domain-model"></a>
## 9. Canonical Domain Model

<a id="91-identifiers-and-semantic-bindings"></a>
### 9.1 Identifiers and Semantic Bindings

Public content-derived IDs are lowercase SHA-256 hex strings; SQLite stores their decoded 32 bytes. Numeric SQLite row IDs never escape as entity identity. `GenerationID` is an installation-local integer used for lookup, not a reproducible analysis fingerprint. Session and run IDs use cryptographic randomness and are operational identifiers, not semantic hashes.

```go
type RepositoryID string
type SnapshotID string
type AnalysisKey string
type UnitID string
type FileID string
type NodeID string
type RelationID string
type EvidenceID string
type ManifestID string
type SessionID string
type ProviderRunID string
type GenerationID int64

type Binding struct {
    RepositoryID RepositoryID `json:"repository_id"`
    SnapshotID   SnapshotID   `json:"snapshot_id"`
    GenerationID GenerationID `json:"generation_id"`
    AnalysisKey  AnalysisKey  `json:"analysis_key"`
}
```

Identity definitions:

```text
RepositoryID = persisted random repository identity within this installation
FileID       = H("file-v1", repository_id, normalized_root_relative_path)
NodeID       = H("node-v1", repository_id, kind, canonical_entity_key)
RelationID   = H("relation-v1", repository_id, from_id, relation_kind, to_id)
SnapshotID   = H("snapshot-v1", repository_id, head_provenance,
                 source_policy_hash, canonical_sorted_source_manifest)
UnitID       = H("unit-v1", provider_id, exact_provider_version, scope_key,
                 analysis_config_hash, canonical_inputs, dependency_unit_keys)
AnalysisKey  = H("analysis-v1", snapshot_id, schema_fingerprint,
                 canonical_sorted_unit_membership, capability_completeness,
                 normalization_version, semantic_config_hash)
```

`H` prefixes each component with its unsigned 64-bit big-endian byte length. Hash a streamed canonical manifest, not a concatenated whole-repository string. File identity is path identity; a rename has a new FileID. Node keys identify declarations or provider-local unresolved entities, not an assertion of timeless semantic continuity. An edit may change a canonical key. A public NodeID is valid only within its returned binding; all queries validate membership in that generation. Reusing the logical key does not reuse stale attributes or evidence.

This separation permits unchanged facts to be shared safely between snapshots. No ID contains a newly assigned generation number merely to force every entity to be rewritten. Cross-snapshot lineage remains evidence-backed; preserve `renamed_from` and `moved_from`, and never infer a precise rename solely from a matching short name.

<a id="92-nodes-relations-and-occurrences"></a>
### 9.2 Nodes, Relations, and Occurrences

Required node vocabulary is unchanged:

```go
type NodeKind string

const (
    NodeRepository    NodeKind = "repository"
    NodeDirectory     NodeKind = "directory"
    NodeFile          NodeKind = "file"
    NodePackage       NodeKind = "package"
    NodeModule        NodeKind = "module"
    NodeNamespace     NodeKind = "namespace"
    NodeFunction      NodeKind = "function"
    NodeMethod        NodeKind = "method"
    NodeClass         NodeKind = "class"
    NodeInterface     NodeKind = "interface"
    NodeStruct        NodeKind = "struct"
    NodeEnum          NodeKind = "enum"
    NodeField         NodeKind = "field"
    NodeVariable      NodeKind = "variable"
    NodeConstant      NodeKind = "constant"
    NodeTest          NodeKind = "test"
    NodeBuildTarget   NodeKind = "build_target"
    NodeDependency    NodeKind = "dependency"
    NodeConfiguration NodeKind = "configuration"
    NodeDocument      NodeKind = "document"
    NodeEndpoint      NodeKind = "endpoint"
    NodeDatabaseEntity NodeKind = "database_entity"
)

```

Required relation vocabulary is unchanged:

```go
type RelationKind string

const (
    RelContains          RelationKind = "contains"
    RelDefines           RelationKind = "defines"
    RelReferences        RelationKind = "references"
    RelCalls             RelationKind = "calls"
    RelImports           RelationKind = "imports"
    RelExports           RelationKind = "exports"
    RelImplements        RelationKind = "implements"
    RelExtends           RelationKind = "extends"
    RelOverrides         RelationKind = "overrides"
    RelReads             RelationKind = "reads"
    RelWrites            RelationKind = "writes"
    RelDataFlowsTo       RelationKind = "data_flows_to"
    RelControlDependsOn  RelationKind = "control_depends_on"
    RelTests             RelationKind = "tests"
    RelDependsOn         RelationKind = "depends_on"
    RelBuilds            RelationKind = "builds"
    RelGenerates         RelationKind = "generates"
    RelDocuments         RelationKind = "documents"
    RelConfigures        RelationKind = "configures"
    RelOwns              RelationKind = "owns"
    RelRenamedFrom       RelationKind = "renamed_from"
    RelMovedFrom         RelationKind = "moved_from"
    RelMayReferTo        RelationKind = "may_refer_to"
)

```

Use a `NodeFunction`/`NodeMethod` provider-local target with explicit `resolution=unresolved` metadata when a call target is unknown, not a fabricated precise definition. Reverse relations use indexed target columns rather than duplicated `called_by` rows. One canonical relation can have several call/reference occurrences; each occurrence has distinct range-bearing evidence. APIs distinguish relation count from occurrence count.

```go
type Position struct {
    Byte   uint64 `json:"byte"`
    Line   uint32 `json:"line"`
    Column uint32 `json:"column"`
}
type SourceRange struct {
    Start Position `json:"start"`
    End Position `json:"end"`
}
type ByteRange struct {
    Start uint64 `json:"start"`
    End uint64 `json:"end"`
}

type Node struct {
    ID            NodeID          `json:"id"`
    Kind          NodeKind        `json:"kind"`
    Language      string          `json:"language,omitempty"`
    Name          string          `json:"name"`
    QualifiedName string          `json:"qualified_name,omitempty"`
    Signature     string          `json:"signature,omitempty"`
    FileID        FileID          `json:"file_id,omitempty"`
    ContentHash   string          `json:"content_hash,omitempty"`
    Range         *SourceRange    `json:"range,omitempty"`
    Metadata      json.RawMessage `json:"metadata,omitempty"`
}
type Relation struct {
    ID RelationID `json:"id"`
    From NodeID `json:"from"`
    Kind RelationKind `json:"kind"`
    To NodeID `json:"to"`
}
```

Every public result envelope carries `Binding`, completeness, and pagination. A stored fact is qualified by its producing UnitID; a public fact is qualified by its generation binding. Neither a provider candidate nor a mutable Node struct is a globally authoritative record.

<a id="93-evidence-precision-and-source-coordinates"></a>
### 9.3 Evidence, Precision, and Source Coordinates

Precision classes remain `compiler`, `language_server`, `static_analysis`, `syntax`, and `heuristic`. They describe origin, not calibrated confidence or guaranteed soundness. Deterministic ranking weights are not probabilities, and no numeric confidence is published anywhere: a name-resolved node carries factual attributes instead, `resolution` (`exact`, `import`, `same_module`, `unique_name`, `ambiguous`, `unresolved`) and `candidates` (the count considered), and ambiguity is a retained `may_refer_to` relation.

```go
type Evidence struct {
    ID EvidenceID `json:"id"`
    UnitID UnitID `json:"unit_id"`
    ProviderID string `json:"provider_id"`
    ProviderVersion string `json:"provider_version"`
    OriginRunID ProviderRunID `json:"origin_run_id"`
    NodeID NodeID `json:"node_id,omitempty"`
    RelationID RelationID `json:"relation_id,omitempty"`
    Precision string `json:"precision"`
    FileID FileID `json:"file_id,omitempty"`
    ContentHash string `json:"content_hash,omitempty"`
    Range *SourceRange `json:"range,omitempty"`
    NativeKey string `json:"native_key,omitempty"`
    Detail string `json:"detail,omitempty"`
}
```

Exactly one of NodeID or RelationID is present. Every published node attribute set and relation has evidence. Evidence identity includes semantic unit identity, subject, precision, native key, source hash/range, and bounded detail; it excludes run timestamps and random run IDs. A reused unit retains its original producing run. A new run records a reuse link rather than pretending it produced old facts.

Byte ranges are half-open `[start,end)`, zero-based. Public line numbers are one-based, columns are zero-based UTF-8 byte columns; an EOF position is valid. CRLF is two bytes and one logical line break. A source range is absent, not zero-filled, for entities without source locations. SQLite-bound values must fit signed 64-bit integers and configured file-size limits. One shared source-position implementation converts provider UTF-8, UTF-16, or UTF-32 coordinates using the exact source hash and validates boundaries; it never guesses an unspecified encoding. SCIP explicitly defines all three encodings. [23](#ref-23)

<a id="94-identity-reconciliation-and-reuse-safety"></a>
### 9.4 Identity Reconciliation and Reuse Safety

Resolve in this order: an unambiguous strong native identifier already associated with this source scope; exact language/path/range/kind/qualified-name match; exact package/qualified-name/signature/defining-file match; deterministic structural key; otherwise a provider-local unresolved entity. SCIP local identifiers are scoped to their document. Overloads, shadowed variables, anonymous declarations, and separate namespaces must not merge by short name.

Persist native aliases, their unit/scope, node facts, and node evidence. Providers resolve only against completed declared dependencies. For equal supported candidates, retain ambiguity with `may_refer_to` edges. Do not choose by worker completion order. Public attribute selection uses a documented precedence tuple: exact-source binding, match basis, precision rank, provider ID, and stable fact key. Conflicting assertions remain retrievable.

A unit may be reused only when every content, path, mode, configuration, analyzer/query version, and declared dependency fingerprint it read is unchanged. Cross-file resolution reads are dependencies. File-local Tree-sitter extraction does not cache a cross-file binding without such a dependency. Package/workspace analyzers invalidate on membership, manifest, build-tag, dependency, and resolution-catalog changes, including additions/deletions that could alter name resolution. A changed file invalidates derived relations that depend on it, not merely its own outgoing edges.

---

<a id="10-repository-snapshots-and-content-retention"></a>
## 10. Repository Snapshots and Content Retention

<a id="101-capture-contract"></a>
### 10.1 Capture Contract

A snapshot is an immutable manifest of the bytes actually captured, not simply `HEAD`. Clean, modified, added, deleted, untracked, and non-Git files are supported. Each eligible nondeleted file is retained in local CAS, deduplicated by SHA-256; deleted entries are tombstones. Git object IDs are recorded as provenance where available. Because Git attributes can transform checkout bytes, a clean Git blob is not automatically identical to the worktree file. [17](#ref-17) [24](#ref-24)

```go
type FileVersion struct {
    ID FileID `json:"id"`
    Path string `json:"path"`
    Status string `json:"status"`
    Size int64 `json:"size"`
    ContentHash string `json:"content_hash,omitempty"`
    GitObjectID string `json:"git_object_id,omitempty"`
    Language string `json:"language,omitempty"`
    Executable bool `json:"executable"`
}
type Snapshot struct {
    ID SnapshotID `json:"id"`
    RepositoryID RepositoryID `json:"repository_id"`
    HeadObjectID string `json:"head_object_id,omitempty"`
    SourcePolicyHash string `json:"source_policy_hash"`
    FileCount uint64 `json:"file_count"`
    SourceBytes uint64 `json:"source_bytes"`
    ManifestHash string `json:"manifest_hash"`
    CreatedAt time.Time `json:"created_at"`
}
type FileSelection struct {
    ChangedOnly bool
    Paths []string // bounded explicit filters, not a repository file list
}
type SnapshotView interface {
    Header() Snapshot
    EachFile(context.Context, FileSelection, func(FileVersion) error) error
    Open(context.Context, FileID) (io.ReadCloser, FileVersion, error)
    ReadRange(context.Context, FileID, ByteRange) ([]byte, FileVersion, error)
}
```

The builder streams paths into indexed on-disk staging, orders the manifest there, and hashes it incrementally. Do not store `Snapshot.Files []FileVersion`, a whole-manifest JSON BLOB, or an unbounded `Changed []FileID`. Public file listings use the same paginated source of truth. Source-policy fingerprint includes eligibility and byte-affecting settings; analyzer versions, ranking, worker counts, and output formatting do not change source identity. Analyzer configuration belongs to AnalysisKey/UnitID instead.

<a id="102-consistent-capture-and-safe-access"></a>
### 10.2 Consistent Capture and Safe Access

Use descriptor-relative root-confined opens, regular-file checks, bounded streaming, and pre/post-open metadata validation. Symlinks are excluded by default. Explicit in-root symlink support must still use a root-confined opener and cycle detection. Detect additions/deletions/renames during final reconciliation and retry a changing file or capture at most twice; then return `CTX_SNAPSHOT_UNSTABLE` and retain the last active generation.

Hash the actual captured bytes. A previous hash may be reused only with a validated unchanged-file mechanism; watcher events and coarse timestamps alone are not proof. Periodic full reconciliation must catch missed and timestamp-preserving changes. Git path enumeration follows the documented plumbing behavior [33](#ref-33) and is NUL-delimited and honors tracked files even when ignore patterns match them; apply ignore rules to untracked discovery. Exclude the actual cache root and `.git` internals unconditionally. Sparse checkouts, submodules, and Git LFS pointer files are reported explicitly: do not fetch absent content or recursively analyze another repository by surprise.

A regular filesystem does not supply a transactionally simultaneous read of every file while unrelated processes write. The guarantee is an exact immutable captured manifest plus detected-change validation, not an impossible claim of a global instant. A strict atomic capture requires a quiescent/locked source or an OS snapshot supplied by the operator. Expose `capture_consistency=validated_capture|operator_frozen` and never upgrade it by inference. [25](#ref-25)

<a id="103-cas-integrity-without-quadratic-source-reads"></a>
### 10.3 CAS Integrity Without Quadratic Source Reads

CAS insertion streams to a private temporary file while calculating the whole-file SHA-256, fixed 64-KiB block digests, and sparse line checkpoints. Flush file data, publish atomically without replacing different content, and persist metadata before a snapshot can become active. Fsync directory publication where the platform provides it; document platform durability behavior. CAS paths derive only from validated digests, not provider paths.

Block digests and line checkpoints are indexed metadata bound to the whole-file digest. Range serving verifies every block it exposes and obtains line/column information from a nearby checkpoint. A full-file pass verifies the whole-file hash; `doctor --deep` can perform full integrity auditing. Do not rehash the whole file or scan from byte zero for every 64-KiB chunk, especially across separate CLI invocations. This design validates served blocks; it does not claim an unread block was revalidated during that request.

Use raw immutable blob files initially; do not add compression, mmap, an AST cache, or a custom packed-file format without measurements and an actual need. Large files remain retained and source-readable even when a configured parse/search admission limit prevents analysis; that limitation is explicit capability metadata. No file is silently removed to improve benchmarks.

<a id="104-materialization-repair-and-retention"></a>
### 10.4 Materialization, Repair, and Retention

External analyzers receive a private materialization of the pinned inputs, including required manifests and dependencies. Use copies or copy-on-write clones with independent writes, never writable hard links to the repository or CAS. Bound materialized bytes and temporary disk use. Reject symlink escape in exports and outputs. Clean up on success, failure, cancellation, and startup recovery.

Normal source reads never fall back to Git or the current checkout. Explicit repair may reconstruct a missing CAS object from its recorded Git OID only if the complete SHA-256 matches the manifest. Repair cannot change the snapshot's identity. A mismatch remains a typed integrity error.

Retain the active generation, two prior generations by default, open unexpired context sessions, active query/cursor leases, and staging captures. GC acquires the workspace indexing lock before collection, so it cannot collect a capture in progress. It rechecks reachability under the storage/GC coordination lock before moving an object to a private trash area; final deletion follows a grace period and a second reachability check. A new pin/ingest and deletion cannot race. Expired or closed sessions are pruned under the documented audit-retention policy before their foreign-key references are removed. Disk pressure returns a typed error or pauses indexing; it does not silently evict open-session source.

---

<a id="11-provider-architecture"></a>
## 11. Provider Architecture

<a id="111-provider-contract-and-unit-ownership"></a>
### 11.1 Provider Contract and Unit Ownership

A provider emits small immutable units with explicit inputs, not one mutable global graph. The coordinator assigns scope, source view, memory/disk reservations, and dependencies. The storage writer stages batches, validates references and provenance, seals the unit, and only then makes it eligible for generation membership. Failed unsealed output is deleted or quarantined; it cannot be queried.

```go
// Neutral records live in internal/model, avoiding storage/provider cycles.
type ProviderDescriptor struct {
    ID string
    Version string
    Capabilities []string
    DependsOn []string
    InvalidationScope string // file, package, workspace
    Required bool
}
type UnitSpec struct {
    ID UnitID
    ProviderID string
    ProviderVersion string
    ScopeKey string
    InputHash string
    DependencyHash string
}
type ProviderResult struct {
    RunID ProviderRunID
    State string // succeeded, partial, skipped, timed_out, failed, canceled
    Capabilities []CapabilityState
    RecordsEmitted uint64
    BytesProcessed uint64
}
type CapabilityState struct {
    ProviderID string `json:"provider_id"`
    Capability string `json:"capability"`
    Scope string `json:"scope"`
    State string `json:"state"`
    DiagnosticCode string `json:"diagnostic_code,omitempty"`
}
```

```go
// internal/provider; concrete batch records are neutral model types.
type UnitRequest struct {
    Binding model.Binding
    Unit model.UnitSpec
    Content model.SnapshotView
    Resolver Resolver
}
type Resolver interface {
    Resolve(context.Context, model.NodeCandidate) (model.Resolution, error)
}
type Sink interface {
    PutNodes(context.Context, []model.NodeFact) error
    PutRelations(context.Context, []model.RelationFact) error
    PutAliases(context.Context, []model.NativeAlias) error
    PutSearchUnits(context.Context, []model.SearchUnit) error
}
type Provider interface {
    Descriptor() model.ProviderDescriptor
    Detect(context.Context, workspace.View) (Detection, error)
    IndexUnit(context.Context, UnitRequest, Sink) (model.ProviderResult, error)
}
```

`NodeCandidate` contains provider/scope/native key, optional strong key, node kind/language/name/qualified name/signature, and source file/hash/range. `Resolution` contains a canonical node, match basis, and bounded ambiguous candidate IDs. `NodeFact` contains one Node plus bounded Evidence; `RelationFact` contains one Relation plus bounded Evidence. `NativeAlias` contains scope/native key/NodeID. `SearchUnit` contains a stable ID, optional NodeID, FileID, path, kind, name/qualified name/signature, source byte bounds, and a bounded body. These records are fully defined and validated in Task 2; their ownership and nullability match Section 12.

Batches are capped by **both records and retained bytes**, including strings, evidence, and serialization buffers. The sink blocks before exceeding its reservation and returns promptly on cancellation. The provider must not retain a handed-off batch or mutate it while the sink owns it. A single oversized fact is a typed limit failure, not a bypass. Provider output ordering is normalized; scheduling order cannot change canonical identity.

| Provider | Completed dependencies required for resolution | Invalidation unit | Runtime mode |
|---|---|---|---|
| filesystem/docs | none | file plus deterministic ancestor identities | base index |
| manifest | filesystem | manifest/package and its dependency inputs | base index |
| tree-sitter | filesystem | file-local extraction | base parser worker |
| scip | filesystem, tree-sitter; manifest when profile needs it | declared package/workspace | managed optional index |
| dependence | filesystem, tree-sitter; SCIP aliases when present | frontend-native project per language; workspace for C/C++ | managed optional index, on demand |
| lsp | pinned files and available canonical structural facts | query workspace | optional query overlay, not `Provider.IndexUnit` |

The LSP manager is a separate query adapter sharing the process runner, source conversion, and capability vocabulary; it is not forced into a nonexistent on-demand indexing enum. Optional provider absence is distinct from failure: a disabled/unconfigured optional provider is `unavailable` without making a healthy base generation falsely fail. An enabled requested capability that fails makes overall health `degraded` and preserves the precise missing scope.

<a id="112-filesystem-documentation-and-manifest-providers"></a>
### 11.2 Filesystem, Documentation, and Manifest Providers

Required base facts include repository/directory/file/package/module/dependency/configuration/build-target/document nodes and `contains`, `defines`, `depends_on`, `builds`, `configures`, and `documents` relations. Source ranges and precision accompany extractable relationships.

| Input | V1 extraction |
|---|---|
| `go.mod`, `go.work` | `golang.org/x/mod/modfile`: modules, requires, replacements and workspace use; do not fetch modules. |
| `package.json` | Standard JSON decoder: identity, dependencies/dev/peer/optional dependencies; scripts are searchable metadata, never executed. |
| `Cargo.toml`, `pyproject.toml` | Pinned TOML parser: package/project identity, explicit dependency tables and relevant workspace inheritance; unresolved dynamic values stay unresolved. |
| `pom.xml` | Streaming XML: coordinates, modules, dependencies and property references; do not evaluate arbitrary build extensions. |
| Markdown and plain text | Headings, links, source-linked documentation and bounded searchable text. |
| AsciiDoc, reStructuredText, ADRs | Recognized document paths and lexical content, without claiming complete semantic parsing. |
| OpenAPI, GraphQL, protobuf, SQL, Docker, CI, Terraform, Kubernetes, Gradle, Bazel, CMake | Recognize configuration/build/interface documents and explicitly supported static relationships; dynamic template or build evaluation is not invented. |

Unknown manifests remain searchable documents. Binary/NUL policy is explicit and does not prevent raw snapshot retention. Search source chunks are at most 32 KiB, line-preferred, and overlap at most two bounded lines. A line longer than a chunk is split safely rather than producing an oversized chunk. Do not duplicate an entire function/file body for every symbol, provider, and generation. Store source chunks once in their owning filesystem unit; symbol search documents contain names/signatures and bounded linked comments only. [15](#ref-15) [16](#ref-16)

<a id="113-tree-sitter-structural-provider"></a>
### 11.3 Tree-sitter Structural Provider

The mandatory bundled grammars remain Go, JavaScript, TypeScript, TSX, Python, Java, Rust, C, and C++. Extract nested/top-level declarations, containing scopes, signatures, imports/exports, syntax references/call sites, test declarations/conventions, and attached comments/docstrings. All grammar/query-set versions are pinned and included in the unit fingerprint. Unknown configured languages fail validation; unavailable structural coverage is not silently advertised.

Each worker owns its parser/tree/query/cursor. Close native objects deterministically on success, error, and cancellation. Reuse a parser within its language worker only while its ownership and memory remain bounded. Do not keep every source buffer, syntax tree, or capture in a repository-wide cache. Parse unchanged files zero times. Incremental tree edits may be used only with exact old/new content, an actual edit map, bounded retained trees, and benchmark evidence; correct file-level incremental indexing does not require an AST cache.

The official binding requires explicit native lifecycle handling. A reported `ParseWithOptions` leak is a dependency-review risk, not proof that any arbitrary pinned release is fixed or affected. Inspect the chosen binding's complete relevant lifecycle/cancellation files, pin a verified revision, and cover the actual callback path in the shared repeated-parse resource check. Do not add a local compatibility shim or trust deprecated timeout APIs blindly. [4](#ref-4) [5](#ref-5) [26](#ref-26)

Call sites are syntax facts. Every call site publishes an alias `callsite:<path>:<start>-<end>` in scope `file:<path>` (1-based inclusive byte range of the callee identifier) attached to its callee node, and the callee node carries the `resolution` and `candidates` attributes of Section 9.3. The SCIP importer publishes the same alias for each reference occurrence whose range equals a call site in the same file version, so the reconciler merges the syntactic callee with the compiler-resolved symbol and the `calls` relation acquires a precise target while both evidence rows remain. SCIP alone cannot mark a call: no indexer distinguishes a call-site reference from a function-value reference, and none sets a write role, so call identification and read/write positions are syntax facts joined to resolved symbols.

<a id="114-scip-import-and-managed-indexers"></a>
### 11.4 SCIP Import and Managed Indexers

Support generic local `.scip` import and the six managed indexer profiles of Section 11.7: `scip-go`, `scip-typescript`, `scip-python`, `scip-java`, `rust-analyzer scip`, and `scip-clang`. A profile is product-owned code: the lock entry it runs, the fixed argument array with typed substitutions, the input scope and trigger files, the output path, the timeout and resource budgets, the environment allowlist, the declared network posture, and the evidence binding. The user configures nothing; a repository whose trigger files match runs the matching profile with the managed tool. No shell command string is inferred and no tool outside the lock is executed unless the user explicitly overrides that profile's executable in user-level configuration.

Decode top-level protobuf wire fields incrementally with a bounded reader; official generated messages are used only for bounded individual records. Do not `io.ReadAll` or unmarshal an entire index, and do not assume one document is small. Bound nested message length, field size, recursion, occurrences, and total spool bytes before allocation. Large document records are walked incrementally; a disk-backed native-symbol map or two-pass import resolves forward references and external symbols without an unbounded in-memory symbol table. Keep local symbols document-scoped; duplicate occurrences deduplicate by semantic evidence key without collapsing distinct source ranges.

**Delta import.** A managed indexer always re-indexes its whole unit (no indexer except `scip-java` has a partial mode, and narrowing `scip-typescript` or `scip-python` changes symbol names), but the importer never rewrites the whole unit. SCIP documents are independent and global symbol strings are position-free, so a refreshed unit is applied as a per-document delta against the stored unit: hash each canonical document, replace only documents whose hash changed (delete-then-insert by path; the format carries no recency marker), delete stored documents whose path is absent from the fresh index (a deleted file yields no document, not an empty one), and reject documents whose path escapes the project root (Go test mains in the build cache). The changed-document set is computed from the index, never from the git diff: adding a method to an interface changes the implementing file's document with no edit to it. Measured on five corpora, a one-file edit changes one document of 141, 954, 76, 1,316 and 2, so the import, FTS and reconcile cost falls to about 0.01–2% of a rewrite while the indexer's own wall time is unchanged. Index the unit's `external_symbols` separately; they are index-level, not per document. [6](#ref-6) [23](#ref-23)

Validate UTF-8/UTF-16/UTF-32 positions against the exact file. Project root, a matching path, timestamps, or an indexer version alone do not prove the index analyzed these bytes. A codectx-invoked profile records captured input hashes. A supplied index must have matching embedded source text or a verified input-hash manifest to qualify as exact-source compiler evidence. Without that association, import remains available but is marked `source_binding=unverified`, isolated from strict canonical compiler facts, and surfaced for discovery/diagnosis. Never silently upgrade stale externally supplied data to precise evidence.

Compiler-precise indexing needs the project's own dependencies to be resolvable inside the private materialization: `node_modules` present for TypeScript and Python packages installed for scip-python, a warm Go module cache, a Cargo registry cache, a `compile_commands.json` for scip-clang, and a build tool that can resolve Java dependencies. A profile runs with its declared network posture and fails typed (`dependencies_unresolved`) when they are not; the structural baseline still serves the repository and the status/doctor output names the missing prerequisite. This is the same boundary CodeQL's autobuild has and it is disclosed, not hidden. [37](#ref-37) [43](#ref-43) [44](#ref-44) [45](#ref-45)

<a id="115-lsp-snapshot-qualified-working-tree-overlay"></a>
### 11.5 LSP Snapshot-Qualified Working-Tree Overlay

LSP provides on-demand document/workspace symbols, definition, references, implementations, type definition, and call hierarchy when the managed server advertises them. Use private materialized bytes from the pinned dirty-worktree snapshot, not mutable live files mislabeled as historical truth. Source and dependency inputs are qualified together. Unsaved editor buffers remain outside V1 because CLI/MCP does not own the editor synchronization stream.

The LSP client owns Content-Length framing, initialize/initialized and shutdown/exit lifecycle, bounded outstanding requests, out-of-order responses, cancellation, document synchronization, and negotiated position encoding. LSP lifecycle is distinct from the MCP 2026 protocol. Treat server-initiated requests explicitly: return supported bounded read-only configuration, otherwise a protocol error; never perform `workspace/applyEdit`, run project commands, or follow external URIs. Validate returned ranges and locations against the materialization. [7](#ref-7)

The manager starts managed servers lazily, caps concurrent servers and overlay bytes, expires idle state, and shares one existing process runner. Profiles cover `gopls`, `rust-analyzer`, `pyright`, `typescript-language-server`, `clangd`, and `jdtls`; each is a lock entry of Section 11.7 and needs no user configuration. Overlay facts carry server/version/input hashes and `language_server` precision, remain separate from immutable canonical context planning, and disappear without corruption when the server stops. Persistence of LSP facts is not a second V1 indexing path; canonical enrichment already exists through SCIP and the dependence provider. The complete advertised live-query feature set remains available as a labeled overlay.

<a id="116-dependence-provider"></a>
### 11.6 Dependence Provider

The `dependence` provider supplies control dependence, intraprocedural-plus-capture data dependence, and fallback call facts for the nine supported languages through a managed code-property-graph engine. The engine is a Section 11.7 lock entry; its name appears in the repository, the lock, and the license inventory, never in a command, a config key, a provider id, an evidence detail, or a query result, so a future engine change is a backend swap and not a product change. Use the engine's built-in noninteractive tools and one exact tested argv per step, not product-owned analysis scripts or an exposed interpreter server:

```text
parse:  <engine>-parse --language <frontend> <private unit materialization> --output <private cpg>
export: <engine>-export <private cpg> --repr=all --format=neo4jcsv --out <private export>
```

One parse handles exactly one language, so the coordinator schedules one unit per (language, frontend-native project): a Go module, a Maven or Gradle module, a `package.json`/`tsconfig.json` project, a Python package with its environment, a Cargo package (which needs `cargo` on the engine's allowlisted PATH). C and C++ are one unit per repository with real include paths, because header resolution spans the whole tree. A call whose target lives in another unit keeps its edge and the callee's fully qualified name on an external stub; the reconciler aliases that stub to the real declaration from the other unit or from SCIP. The single `all` export already carries every edge family the provider imports (`CALL`, `CDG`, `REACHING_DEF`, `CONTAINS`); no second export exists, and `--repr=pdg|cdg|ddg` is not implemented for CSV or GraphML in the pinned engine.

The provider never delays base readiness. Nothing runs before the base generation is active, and nothing is installed when the workspace is opened: the backend construction resolves the analysis payload only if the store already holds it, keeps the payload's pinned identity otherwise, and the first unit that needs the engine fetches it at unit time. With `providers.dependence.enabled = "auto"` (the default), the coordinator then enqueues every dependence unit as low-priority background work under the resource governor; a query that asks for a dependence fact promotes its units to the front of that queue and answers with a typed `pending` capability (unit count, position, estimate) until they seal. A one-shot building command is not exempt from that queue: `codectx index` and `codectx refresh` alike print their own generation as soon as it activates, then drain the deferred queue to empty, printing one line per publication, and exit. An interrupted run keeps that generation and every generation already published and reports the pending unit count; `providers.dependence.enabled = false` is the opt-out, and there is no flag that skips the drain while still planning the work. `true` blocks the index on the units; `false` disables the provider. A sealed dependence unit never mutates the active generation: the coordinator builds a new generation from the active generation's units plus the sealed unit, validates it, and activates it atomically through the same path a refresh uses (Section 13.1); seals landing in one scheduler tick coalesce into one generation, and the `pending` answer becomes a real answer exactly at activation. The parsed graph is cached per unit under the private data directory and reused across generations, so unchanged code pays nothing on refresh. The cache key is the complete semantic closure: the unit's source file hashes, its manifest and lock files (`go.mod`/`go.sum`, `package.json`/lockfile/`tsconfig.json`, `pyproject.toml`/`requirements*.txt`, `Cargo.toml`/`Cargo.lock`, `compile_commands.json` and include paths), the frontend argv including excludes and the definition cap, the engine payload digest, and the runtime payload digest. Any of these changing invalidates the unit.

Memory is governed per process tree, not per Go heap, and never at the expense of results. The engine's frontend honors a heap cap through its environment and a cap is lossless: on every repository measured (239k-line Go, 443k- and 1.8M-line C, 81k-line TypeScript, 1.5M-line Java, 506k-line Python) a capped run produced the same facts as the default run or no graph at all. A heap cap is not a memory cap: resident memory above the cap is frontend-specific (Go/TypeScript/Java 0.1–0.5 GB, Python about 1.9 GB, C/C++ about 2.6 GB, plus a fixed ~0.8 GB helper for Rust), so the scheduler computes a reservation = heap cap + per-frontend allowance + helper allowance and uses it to order and co-schedule work, not to refuse it. There is no default memory ceiling: the heap cap is sized from the unit's byte count with headroom (a cap near the live set costs time instead of memory: 3.6× slower on the Java repository) up to the machine-derived allocation, which is free memory minus the base index's footprint minus a safety margin, further bounded only by an explicit user limit (`unit_memory_ceiling_bytes` set by the user; `0` means machine-derived). Only that explicit user limit may reject a unit before it runs. On an out-of-memory exit the provider retries exactly once at the machine-derived allocation, and only when that allocation exceeds the failed cap; if the first attempt already had the maximum, it skips the retry (a doomed retry cost 100 s on the 1.05M-line Python tree) and reports `failed: memory` with the observed figures and the unit's estimated requirement. Heavy analyzers run one at a time by default (`max_concurrent_heavy_analyzers = 1`); the scheduler admits a second only when the sum of reservations fits the machine-derived allocation. The export step gets its own reservation (it scales with the graph, not the source: 2 GB of CSV for the 1.8M-line C repository) and the CSV is deleted after import. Engine failures are classified, never guessed: an out-of-memory exit is `failed: memory`; a deterministic pass crash (reproduced on a 137k-line Go module and a 324k-line TypeScript tree) is `failed: engine` with the failing pass name from stderr; a helper crash that the orchestrator hides behind a zero exit and a near-empty graph (reproduced on a 183k-line Rust workspace) is detected by the `Process exited with code` stderr line or a zero file count for a non-empty unit and is also `failed: engine`. Every result is validated for non-emptiness before it is admitted. Subdivision exists only as the last-resort recovery from a reproducible engine crash, never for memory. The full frontend-native unit always runs first. On a crash the provider reruns the same unit once with the frontend's allowlist of semantics-neutral options (a fixed per-frontend list maintained in the backend and verified in Task 22; today that list is empty for all six frontends, because every known option such as the Rust helper's `--no-sysroot` changes results) to prove the crash is reproducible and not transient. Only then does it split the unit along the next frontend-native boundary. A subdivided unit is never presented as equivalent: every capability it publishes carries `partial` with reason `subdivided`, the failed unit id, the backend failure (pass name and exception class), and the affected capabilities. The honesty test is per capability, measured in Section 8 of the round-3 research: control and data dependence survive splitting almost intact (99.7–99.9% of edges on the TypeScript corpus), so they are published `partial: subdivided`; engine `calls` from a subdivided TypeScript unit keep under half of their resolved targets, so they are published `partial: subdivided` and consumers are pointed at the syntax-plus-SCIP `calls` path, which never depended on the engine. If a subdivided run cannot produce an honest useful result for any capability, the unit fails with `failed: engine` instead. The engine's per-method definition cap is raised in the pinned argv to `--max-num-def 40000` (measured uncapped on a 1.05M-line Python tree: +23% parse time, +3% memory, zero skipped methods, every other fact count identical; see `docs/research/10-round3-empirical.md` Section 9a) and is part of the cache key; there is no second parse at a higher limit. The provider captures stderr, parses any remaining skip warnings, and publishes `data_flows_to` as `partial` with the count and the skipped method names.

Reads and writes are published by this provider from the graph's assignment operators: the written operand with a resolved reference becomes `writes`, other resolved identifier uses become `reads`, both at `static_analysis` precision. No SCIP indexer sets a write role and syntax alone cannot resolve the target, so this is the only honest source. Provenance is never lost behind the neutral name: every evidence row's `provider_version` carries the adapter version and the engine payload digest, `Detection.ObservedVersion` reports the engine version and payload digest (never the engine name) to `status`, `doctor` and the ledger, and `docs/providers-dependence.md` maps digests to releases.

The engine has no incremental mode (maintainer-confirmed) and no merge, append or per-file export, so a refreshed unit is a whole-unit parse and export; the storage step is a delta, never a rewrite. Facts are keyed by an id-independent semantic key (method full name, file, ordered ranges, edge label, operator kind, and for relations the published identities of both endpoints, so an edge into an edited declaration is keyed as a changed fact), the fresh export is diffed against the stored unit on that key, and only changed rows are written; the Go frontend's nondeterministic package-initializer rows (`<clinit>`) are normalized before comparison so they do not appear as churn. Deriving the keys measured 1–5 s per unit against 17–65 s engine runs, and a one-line edit changes about one fact row in ten thousand. Stream CSV with field/record/depth caps enforced **before** a standard decoder can allocate an unbounded field. Use an on-disk native-ID map for arbitrary graph ordering; do not drop forward edges because a lookup window expired. Map only profile-owned labels; count and report unknown labels. External methods that carry a snapshot file location are declarations in another unit, not junk, and are kept. A dependence edge does not alone prove an end-to-end source-to-sink exploit or complete dataflow; data dependence through closures and globals crosses method boundaries and its evidence detail says so. Verify actual overlays and exported labels against the pinned engine release; do not assume all releases require the same commands. [8](#ref-8) [9](#ref-9) [10](#ref-10) [27](#ref-27)

A tool the lock does not provide for the current platform is unavailable with a typed platform reason; a timeout, malformed export, or unsupported unit is failed/partial with no unsealed facts admitted. The real pinned engine runs in the Task 22 CI matrix on every language before the docs call the provider supported. Remove materializations, graphs not selected for the cache, and exports on every termination path. Server mode is outside V1 and must not be introduced as a shortcut.
Live-query routing is explicit: symbol/reference/call-hierarchy requests accept `semantic_source=canonical|lsp` (default canonical), a managed profile selector, and the required pinned file/range or symbol input. The CLI exposes `--semantic-source` on those commands; MCP uses the same typed field. `find_symbol` supports document/workspace symbol operations, and references supports implementation/type-definition operations. Query metadata contains a labeled overlay provider/version/input digest when LSP is used. Unsupported LSP methods return unavailable capability, not a silently substituted canonical answer. Context compilation always requests canonical mode.


<a id="117-managed-analyzer-toolchain"></a>
### 11.7 Managed Analyzer Toolchain

A user installs codectx and every supported language works at its best available precision. The product owns every external analyzer and every runtime those analyzers need. Nothing is looked up on PATH, nothing is installed by the user, and nothing runs that the shipped binary did not pin. This is CodeQL's distribution model: one bundle with every extractor and its own runtime, plus on-demand fetch of pinned packs for the slim install. It is also mise's lockfile model: an exact version, a per-platform URL, and a SHA-256 per artifact, verified before anything executes. [37](#ref-37) [38](#ref-38) [39](#ref-39) [40](#ref-40)

**Tool lock.** The binary embeds `internal/toolchain/tools.lock.json`. One entry per tool: `name`, `version`, `kind` (`indexer`, `server`, `cpg`, `runtime`), `license`, `upstream`, `runtime` (empty, `node` or `jdk`), `entry` (payload-relative executable, script or jar), `entry_sha256`, and a `platforms` map from `<os>_<arch>` to `{url, sha256, size}`. A platform absent from the map means the tool is unavailable there, reported as such, never a failure. The lock is data; the profile argv, triggers, and budgets are product code that names a lock entry.

**Payloads.** Hosting is hybrid, and the rule is simple: a tool that ships a real upstream binary is fetched from upstream; a tool that does not is prebuilt and hosted by us. The engine, the JDK (Temurin), Node, `scip-go`, `scip-java`, `scip-clang`, `rust-analyzer`, `clangd` and `jdtls` are lock entries pointing at the upstream release asset URL (GitHub releases, `nodejs.org/dist`, Adoptium, `download.eclipse.org/jdtls`) with that file's SHA-256 and size, verified against the upstream-published digest where one exists and recorded as `upstream_digest`; the archive is used as upstream ships it (zip or tar.gz) and the entry path is the executable inside it. Only `gopls` (no upstream binaries; built per platform) and the four npm-based tools (`typescript-language-server` with `typescript`, `pyright`, `scip-typescript`, `scip-python`; prebuilt with `npm ci --omit=dev` so the user never needs npm) are published as `<name>-<version>-<os>-<arch>.tar.gz` assets of a `tools-v<n>` release of the codectx repository, about 50 MB in total. The release-time Go program `internal/tools/toollock` downloads every upstream distribution, verifies digests, builds the five hosted bundles, normalizes them, and writes the lock; `toollock -check` re-downloads and re-digests every URL in the lock, upstream and hosted. The runtime never runs `npm`, `pip`, `coursier`, `go install`, or any package manager, and the user needs neither Go nor Node installed: Node itself is a pinned upstream download. Digest pinning makes an upstream URL exactly as tamper-proof as a re-hosted one; re-hosting everything was rejected because it added ~15 GB of release assets and a fragile upload pipeline for no security gain. `tools.mirror` substitutes the host prefix per source so locked-down networks can serve every URL from an internal copy. The license of every redistributed bundle is recorded in the lock and in `THIRD_PARTY_LICENSES.md`. [41](#ref-41) [42](#ref-42) [46](#ref-46)

**Store.** Payloads live under `<data_dir>/tools/<name>/<version>/` with mode `0o700`. Install is atomic: fetch to `<data_dir>/tools/.staging/<random>/` with the byte cap, verify size and SHA-256 against the lock, extract with root confinement (reject absolute paths, `..`, symlinks whose target leaves the payload, hard links, devices, and more than the declared file count and bytes), fsync, then rename into place and write a `.complete` file carrying the payload digest. A cross-process file lock serializes installs of the same tool. A directory without `.complete` is invisible and swept. Versions not named by the current lock are removed by retention after a binary upgrade.

**Resolution.** `toolchain.Resolver.Resolve(ctx, name)` returns the runnable tool in this order: the user-level `[tools.override.<name>]` if present (absolute executable, exact version, SHA-256, verified on every run start); otherwise the store entry whose `.complete` digest matches the lock and whose `entry` hashes to `entry_sha256`; otherwise, unless `tools.offline = true`, a fetch of the lock payload, after which the store entry is used. A runtime-dependent tool resolves its runtime the same way and the result's `ArgvPrefix` carries the launcher, for example `[<store>/node/<v>/bin/node, <store>/scip-typescript/<v>/dist/main.js]` or `[<store>/jdk/<v>/bin/java, -jar, <store>/scip-java/<v>/scip-java.jar]`. Unavailability is typed: `CTX_TOOL_OFFLINE`, `CTX_TOOL_UNSUPPORTED_PLATFORM`, `CTX_TOOL_FETCH_FAILED`, `CTX_TOOL_DIGEST_MISMATCH`, `CTX_TOOL_CORRUPT` (a store entry without `.complete` or whose entry hash disagrees with the lock; repaired by a fresh install), `CTX_TOOL_OVERRIDE_INVALID`. Fetch failures are retried within one fixed attempt/deadline budget and then reported; a digest mismatch is never retried and never executed.

**Network.** `internal/toolchain` is the only package in the product that imports `net/http`, and CI enforces that with a build-time check. It fetches only URLs that appear in the embedded lock, or the same host-and-path under `tools.mirror` when set (the mirror keeps each URL's original host as its first path segment, so one mirror serves every upstream and hosted asset), follows at most three redirects to the same host set, honors the standard proxy environment, and bounds bytes and time. `tools.offline = true` makes every fetch a typed refusal without opening a socket. The release bundle is the slim binary with the store pre-populated for one platform and needs no network at all. Ordinary logging records tool name, version, digest, byte count, and elapsed time for each fetch, and nothing else. This is the Zed lesson applied: a silent runtime download is acceptable only when every byte is pinned and verified; Zed's unverified auto-download is the anti-pattern. [40](#ref-40)

**Inference.** Which managed tools a repository needs is inferred from evidence the snapshot already holds, as Sourcegraph auto-indexing infers jobs from repository files: the SCIP trigger files and the LSP root markers of Sections 11.4 and 11.5, together with the language table. Nothing is fetched for a language the repository does not contain. `codectx tools prefetch --all` and the bundle exist for CI and offline hosts, not as a required step. [39](#ref-39)

**Supported matrix.** Platform keys follow Section 24. Exact versions and digests are lock contents produced at Task 22 from the real upstream releases, not values asserted here.

| Tool | Kind | Languages | Runtime | Upstream distribution | Platforms |
|---|---|---|---|---|---|
| `node` | runtime | — | — | `nodejs.org/dist` current LTS, `SHASUMS256.txt` | all six |
| `jdk` | runtime | — | — | Adoptium Temurin 21 LTS JDK | all six |
| `scip-go` | indexer | go | — | `sourcegraph/scip-go` release binaries for linux amd64/arm64 and darwin arm64; codectx cross-builds and hosts darwin amd64, windows amd64 and windows arm64 (the gopls recipe) | all six |
| `scip-typescript` | indexer | typescript, tsx, javascript | node | npm `@sourcegraph/scip-typescript` | all six |
| `scip-python` | indexer | python | node | npm `@sourcegraph/scip-python` | all six |
| `scip-java` | indexer | java | jdk | `sourcegraph/scip-java` release; run as `java -jar` under the managed JDK so Windows is covered without the dropped upstream launcher | all six, verified per platform at Task 22 |
| `rust-analyzer` | indexer and server | rust | — | `rust-lang/rust-analyzer` release; `rust-analyzer scip <path> --output` for SCIP, `rust-analyzer` for LSP | all six |
| `scip-clang` | indexer | c, cpp | — | `sourcegraph/scip-clang` release binaries | linux and darwin only; Windows uses `clangd` for precise C/C++ |
| `gopls` | server | go | — | built at release time from `golang.org/x/tools/gopls` at the pinned version | all six |
| `typescript-language-server` | server | typescript, tsx, javascript | node | npm `typescript-language-server` with `typescript` | all six |
| `pyright` | server | python | node | npm `pyright` | all six |
| `clangd` | server | c, cpp | — | `clangd/clangd` release archives | as published upstream |
| `jdtls` | server | java | jdk | `download.eclipse.org/jdtls/milestones` tarball | all six |
| `joern` | cpg (backend of the `dependence` provider) | all nine languages; Rust requires `cargo` on the allowlisted PATH | jdk | `joernio/joern` per-platform `joern-cli` archives (astgen helpers bundled) | all six (upstream publishes linux amd64/arm64, macOS amd64/arm64, windows amd64/arm64) |

**Lifecycle commands.** `codectx tools status` lists every lock entry with installed/available/unsupported/override state and the languages it unlocks; `tools prefetch [--all | --for-repo PATH]` installs ahead of time; `tools verify` rehashes the store; `tools gc` removes versions the current lock does not name. `doctor` includes the same report. None of these is required for ordinary use.

**Verification.** Every profile is exercised against the real lock payload on Linux and macOS in the CI matrix of Task 21 before the docs may call it supported; Windows runs every entry the lock provides there. "Unverified against a real tool" is a CI failure, not a documentation label. [39](#ref-39) [41](#ref-41)

---

<a id="12-storage-architecture"></a>
## 12. Storage Architecture

<a id="121-sqlite-operating-model"></a>
### 12.1 SQLite Operating Model

Use one private SQLite database per workspace with `modernc.org/sqlite` v1.58.0 as the initial exact driver pin. Its published platform table reports SQLite 3.53.4; verify `sqlite_version()` and `sqlite_source_id()` in CI and doctor, and pin `modernc.org/libc` to the exact version required by that driver. Reject affected embedded engines older than SQLite 3.51.3 and the withdrawn 3.52.0 release; do not assume a Go module name proves the WAL-reset fix is present. [13](#ref-13) [34](#ref-34) [35](#ref-35) The driver avoids a SQLite CGo dependency; native Tree-sitter still exists in its parser workers. Initial connection policy:

```sql
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = FILE;
PRAGMA mmap_size = 0;
-- Writer connection, KiB budget:
PRAGMA cache_size = -8192;
-- Each read connection uses cache_size = -4096 instead.
```

One write connection and at most two read connections are the default **per workspace owner process**. Separate short-lived CLI readers have their own small budget; report them where observable and avoid a background service solely to centralize every command. Configure per-connection pragmas using a supported driver hook/connector or DSN mechanism and verify them on new pooled connections. Setting a pragma on one arbitrary pooled connection is insufficient. Foreign keys and database ownership are validated before serving. [11](#ref-11) [13](#ref-13) [28](#ref-28)

Use short transactions and prepared statements. No large transaction contains an entire analyzer run. `FULL` is the conservative default for acknowledged user-visible session/receipt state; batching amortizes write synchronization. A future less-durable index-only mode must not silently weaken receipt durability. `quick_check`, full FTS integrity checking, VACUUM, and whole-CAS verification run on initialization, explicit doctor/deep checks, recovery, or scheduled maintenance, not every short-lived query invocation.

A writer-owned passive checkpoint runs outside latency-sensitive queries. Monitor WAL size and checkpoint progress; long-lived read transactions must not pin WAL indefinitely. A WAL high-water threshold triggers checkpointing and indexing backpressure. `journal_size_limit` is not represented as a hard live-WAL cap. The operating disk quota includes DB, WAL, CAS, temporary files, and analyzer artifacts, with a reserved free-space margin.

<a id="122-current-schema-and-reusable-units"></a>
### 12.2 Current Schema and Reusable Units

Initialize one current schema from embedded `schema.sql`. Store its version and fingerprint in `schema_meta`; there is **no migration runner, migration directory, migration history, dual schema, or compatibility facade**. A different fingerprint fails with `CTX_SCHEMA_MISMATCH`. `index --rebuild` creates an explicitly requested new cache after warning about old sessions; it does not silently mutate or erase the incompatible database. Existing artifacts remain untouched until the user separately approves deletion. Greenfield schema changes are made in place in this file and all consumers.

The physical model stores source manifests separately from reusable analysis units. `generation_units` selects one immutable unit per provider/scope. Node/relation identity dictionaries are not themselves visible facts; queries join through visible unit membership. Reusing a unit preserves evidence, aliases, and search documents. Numeric unit row IDs are compact internal join keys; exported UnitID is its 32-byte key encoded as hex. Provider run provenance outlives the creating generation when a retained unit still references it.

The following is the complete DDL, kept byte-identical to `internal/storage/sqlite/schema.sql` (the embedded schema whose SHA-256 is the store fingerprint); it includes workflow/coverage state and the delta tables (`fact_keys`, `unit_delta_state`). Logical range, ownership, visibility, JSON shape, completeness, and state-transition checks that require joins are additionally enforced by the single store API at unit seal/publication or mutation. No provider gets a SQL handle.

```sql
CREATE TABLE schema_meta (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    version INTEGER NOT NULL CHECK(version = 1),
    fingerprint TEXT NOT NULL
);
CREATE TABLE repositories (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    root_path TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE blobs (
    hash BLOB PRIMARY KEY CHECK(length(hash) = 32),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    state TEXT NOT NULL CHECK(state IN ('ready','quarantined','trash')),
    created_at TEXT NOT NULL
);
CREATE TABLE blob_blocks (
    blob_hash BLOB NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    block_index INTEGER NOT NULL CHECK(block_index >= 0),
    digest BLOB NOT NULL CHECK(length(digest) = 32),
    PRIMARY KEY(blob_hash, block_index)
) WITHOUT ROWID;
CREATE TABLE line_checkpoints (
    blob_hash BLOB NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    byte_offset INTEGER NOT NULL CHECK(byte_offset >= 0),
    line_number INTEGER NOT NULL CHECK(line_number >= 1),
    line_start_byte INTEGER NOT NULL CHECK(line_start_byte >= 0 AND line_start_byte <= byte_offset),
    PRIMARY KEY(blob_hash, byte_offset)
) WITHOUT ROWID;
CREATE TABLE files (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    path TEXT NOT NULL,
    UNIQUE(repository_id, path)
);
CREATE TABLE snapshots (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    head_object_id TEXT NOT NULL DEFAULT '',
    source_policy_hash TEXT NOT NULL,
    manifest_hash TEXT NOT NULL,
    file_count INTEGER NOT NULL CHECK(file_count >= 0),
    source_bytes INTEGER NOT NULL CHECK(source_bytes >= 0),
    capture_consistency TEXT NOT NULL CHECK(capture_consistency IN ('validated_capture','operator_frozen')),
    created_at TEXT NOT NULL,
    UNIQUE(repository_id, id)
);
CREATE TABLE snapshot_files (
    snapshot_id BLOB NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    file_id BLOB NOT NULL REFERENCES files(id),
    status TEXT NOT NULL CHECK(status IN ('tracked','modified','added','deleted','untracked')),
    content_hash BLOB REFERENCES blobs(hash),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    git_object_id TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL DEFAULT '',
    executable INTEGER NOT NULL CHECK(executable IN (0,1)),
    PRIMARY KEY(snapshot_id, file_id),
    UNIQUE(snapshot_id, file_id, content_hash),
    CHECK((status = 'deleted' AND content_hash IS NULL AND size_bytes = 0)
       OR (status <> 'deleted' AND content_hash IS NOT NULL))
) WITHOUT ROWID;
-- generations.ref is the ref the generation was built from (Section 12.4):
-- retention keeps the last retain_refs DISTINCT refs the user actually
-- indexed, not the last N generations, which is what makes A -> B -> C -> A
-- find A's units still on disk. The value is the caller's: a branch name, the
-- HEAD object id when HEAD is detached, or the fixed sentinel '(none)' for a
-- workspace that is not a Git repository. Storage never interprets it, only
-- groups by it.
CREATE TABLE generations (
    id INTEGER PRIMARY KEY,
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    snapshot_id BLOB NOT NULL,
    ref TEXT NOT NULL CHECK(length(ref) > 0),
    analysis_key BLOB CHECK(analysis_key IS NULL OR length(analysis_key) = 32),
    semantic_config_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('staging','active','superseded','failed')),
    health TEXT NOT NULL CHECK(health IN ('fresh','degraded','failed')),
    created_at TEXT NOT NULL,
    activated_at TEXT,
    UNIQUE(repository_id, id),
    UNIQUE(id, snapshot_id),
    FOREIGN KEY(repository_id, snapshot_id) REFERENCES snapshots(repository_id, id)
);
CREATE TABLE active_generations (
    repository_id BLOB PRIMARY KEY REFERENCES repositories(id),
    generation_id INTEGER NOT NULL,
    FOREIGN KEY(repository_id, generation_id) REFERENCES generations(repository_id, id)
);
CREATE TABLE provider_runs (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    generation_id INTEGER REFERENCES generations(id) ON DELETE SET NULL,
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('running','succeeded','partial','skipped','timed_out','failed','canceled')),
    counters_json TEXT NOT NULL DEFAULT '{}',
    diagnostic_code TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE TABLE units (
    id INTEGER PRIMARY KEY,
    unit_key BLOB NOT NULL UNIQUE CHECK(length(unit_key) = 32),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    dependency_hash TEXT NOT NULL,
    origin_run_id BLOB NOT NULL REFERENCES provider_runs(id),
    state TEXT NOT NULL CHECK(state IN ('building','sealed','failed','quarantined')),
    source_binding TEXT NOT NULL CHECK(source_binding IN ('verified','unverified')),
    UNIQUE(id, provider_id, scope_key)
);
CREATE TABLE unit_inputs (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    file_id BLOB NOT NULL REFERENCES files(id),
    content_hash BLOB NOT NULL REFERENCES blobs(hash),
    executable INTEGER NOT NULL CHECK(executable IN (0,1)),
    PRIMARY KEY(unit_id, file_id)
) WITHOUT ROWID;
CREATE TABLE unit_dependencies (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    dependency_id INTEGER NOT NULL REFERENCES units(id),
    PRIMARY KEY(unit_id, dependency_id),
    CHECK(unit_id <> dependency_id)
) WITHOUT ROWID;
-- carried marks the Section 13.3 stale-with-distance member: the scope's
-- previous sealed unit, kept in the generation while its fresh rebuild is
-- still running. Such a unit's inputs differ from the generation's snapshot by
-- definition, so AttachCarried admits it where AttachUnit refuses, and the two
-- distance columns record how far behind it is (generations, and changed files
-- between its snapshot and this one). All three columns are folded into the
-- membership digest, and therefore into the AnalysisKey: without them a
-- generation carrying a stale unit would be byte-identical to one that rebuilt
-- it, and a stale answer would be indistinguishable from a fresh one.
CREATE TABLE generation_units (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    unit_id INTEGER NOT NULL,
    carried INTEGER NOT NULL CHECK(carried IN (0,1)),
    distance_generations INTEGER NOT NULL CHECK(distance_generations >= 0),
    distance_files INTEGER NOT NULL CHECK(distance_files >= 0),
    PRIMARY KEY(generation_id, provider_id, scope_key),
    UNIQUE(generation_id, unit_id),
    FOREIGN KEY(unit_id, provider_id, scope_key) REFERENCES units(id, provider_id, scope_key),
    CHECK(carried = 1 OR (distance_generations = 0 AND distance_files = 0))
) WITHOUT ROWID;
CREATE TABLE generation_capabilities (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    capability TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('fresh','partial','stale','unavailable','failed')),
    diagnostic_code TEXT NOT NULL DEFAULT '',
    details_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(generation_id, provider_id, capability, scope_key)
) WITHOUT ROWID;
CREATE TABLE node_ids (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    kind TEXT NOT NULL,
    canonical_key TEXT NOT NULL,
    UNIQUE(repository_id, kind, canonical_key)
);
CREATE TABLE node_facts (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id BLOB NOT NULL REFERENCES node_ids(id),
    language TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id),
    CHECK((start_byte IS NULL AND end_byte IS NULL)
       OR (file_id IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL
           AND start_byte >= 0 AND end_byte >= start_byte))
) WITHOUT ROWID;
CREATE TABLE relation_ids (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    from_node_id BLOB NOT NULL REFERENCES node_ids(id),
    kind TEXT NOT NULL,
    to_node_id BLOB NOT NULL REFERENCES node_ids(id),
    UNIQUE(repository_id, from_node_id, kind, to_node_id)
);
CREATE TABLE relation_facts (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    relation_id BLOB NOT NULL REFERENCES relation_ids(id),
    PRIMARY KEY(unit_id, relation_id)
) WITHOUT ROWID;
-- fact_keys carries the producer's own id-independent delta keys for one fact
-- (Section 11.4). A fact is backed by every key that produced it, not by one:
-- a canonical edge is published once but is derived from N occurrences, each
-- with its own key, and a refresh re-emits the whole fact when any of them
-- changes. A carry-over therefore keeps a fact unless ANY of its keys is
-- replaced, which is the same condition the producer re-emits on, so the fresh
-- and carried sets partition exactly. A fact with no row here carries no key
-- and can only be replaced by its retention bucket.
CREATE TABLE fact_keys (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id BLOB,
    relation_id BLOB,
    fact_key TEXT NOT NULL,
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, relation_id) REFERENCES relation_facts(unit_id, relation_id),
    CHECK((node_id IS NOT NULL AND relation_id IS NULL) OR (node_id IS NULL AND relation_id IS NOT NULL))
);
CREATE TABLE evidence (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id BLOB,
    relation_id BLOB,
    precision TEXT NOT NULL CHECK(precision IN ('compiler','language_server','static_analysis','syntax','heuristic')),
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    native_key TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    content_hash_bound INTEGER NOT NULL DEFAULT 0 CHECK(content_hash_bound IN (0,1)),
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, relation_id) REFERENCES relation_facts(unit_id, relation_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id),
    CHECK((node_id IS NOT NULL AND relation_id IS NULL) OR (node_id IS NULL AND relation_id IS NOT NULL)),
    CHECK(content_hash_bound = 0 OR file_id IS NOT NULL),
    CHECK((start_byte IS NULL AND end_byte IS NULL)
       OR (file_id IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL
           AND start_byte >= 0 AND end_byte >= start_byte))
);
CREATE TABLE unit_delta_state (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    payload BLOB NOT NULL,
    PRIMARY KEY(unit_id, kind)
) WITHOUT ROWID;
CREATE TABLE native_aliases (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    scope_key TEXT NOT NULL,
    native_key TEXT NOT NULL,
    node_id BLOB NOT NULL,
    PRIMARY KEY(unit_id, scope_key, native_key, node_id),
    FOREIGN KEY(node_id) REFERENCES node_ids(id)
) WITHOUT ROWID;
CREATE TABLE search_units (
    rowid INTEGER PRIMARY KEY,
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    search_key BLOB NOT NULL CHECK(length(search_key) = 32),
    node_id BLOB,
    file_id BLOB NOT NULL,
    path TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    qualified_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte >= start_byte),
    body TEXT NOT NULL,
    token_count INTEGER NOT NULL CHECK(token_count >= 0),
    UNIQUE(unit_id, search_key),
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id)
);
CREATE VIRTUAL TABLE search_fts USING fts5(
    name, qualified_name, signature, path, body,
    content='search_units', content_rowid='rowid', tokenize='unicode61', detail='full'
);
CREATE VIRTUAL TABLE search_vocab USING fts5vocab(search_fts, 'instance');
CREATE TABLE context_manifests (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    generation_id INTEGER NOT NULL,
    snapshot_id BLOB NOT NULL,
    phase TEXT NOT NULL CHECK(phase IN ('sweep','verify','consolidate')),
    request_hash TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    request_json TEXT NOT NULL,
    completeness_json TEXT NOT NULL,
    canonical_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(id, generation_id, snapshot_id),
    FOREIGN KEY(generation_id, snapshot_id) REFERENCES generations(id, snapshot_id)
);
CREATE TABLE context_entries (
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    node_id BLOB REFERENCES node_ids(id),
    file_id BLOB REFERENCES files(id),
    requirement TEXT NOT NULL CHECK(requirement IN ('required_full','required_symbol','recommended','optional')),
    score_micros INTEGER NOT NULL,
    estimated_bytes INTEGER NOT NULL CHECK(estimated_bytes >= 0),
    estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens >= 0),
    reasons_json TEXT NOT NULL,
    evidence_paths_json TEXT NOT NULL,
    PRIMARY KEY(manifest_id, ordinal),
    CHECK(node_id IS NOT NULL OR file_id IS NOT NULL)
) WITHOUT ROWID;
CREATE TABLE context_slices (
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
    slice_index INTEGER NOT NULL CHECK(slice_index >= 0),
    estimated_bytes INTEGER NOT NULL CHECK(estimated_bytes >= 0),
    estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens >= 0),
    PRIMARY KEY(manifest_id, slice_index)
) WITHOUT ROWID;
CREATE TABLE context_slice_entries (
    manifest_id BLOB NOT NULL,
    slice_index INTEGER NOT NULL,
    position INTEGER NOT NULL CHECK(position >= 0),
    entry_ordinal INTEGER NOT NULL,
    PRIMARY KEY(manifest_id, slice_index, position),
    FOREIGN KEY(manifest_id, slice_index) REFERENCES context_slices(manifest_id, slice_index) ON DELETE CASCADE,
    FOREIGN KEY(manifest_id, entry_ordinal) REFERENCES context_entries(manifest_id, ordinal) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE excluded_context_entries (
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    reference_json TEXT NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY(manifest_id, ordinal)
) WITHOUT ROWID;
CREATE TABLE read_sessions (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    actor_id TEXT NOT NULL CHECK(length(trim(actor_id)) > 0),
    idempotency_key TEXT CHECK(idempotency_key IS NULL OR length(idempotency_key) > 0),
    open_request_hash TEXT NOT NULL,
    manifest_id BLOB NOT NULL,
    generation_id INTEGER NOT NULL,
    snapshot_id BLOB NOT NULL,
    workflow_state TEXT NOT NULL CHECK(workflow_state IN ('sweep_open','verify_open','consolidate_open','complete','closed')),
    state_version INTEGER NOT NULL CHECK(state_version >= 1),
    scope_version INTEGER NOT NULL CHECK(scope_version >= 1),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    closed_at TEXT,
    UNIQUE(id, snapshot_id),
    UNIQUE(actor_id, idempotency_key),
    FOREIGN KEY(manifest_id, generation_id, snapshot_id)
        REFERENCES context_manifests(id, generation_id, snapshot_id)
);
CREATE TABLE session_manifests (
    session_id BLOB NOT NULL REFERENCES read_sessions(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id),
    phase TEXT NOT NULL CHECK(phase IN ('sweep','verify','consolidate')),
    activated_at TEXT NOT NULL,
    PRIMARY KEY(session_id, ordinal)
) WITHOUT ROWID;
CREATE TABLE session_files (
    session_id BLOB NOT NULL,
    snapshot_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    requirement TEXT NOT NULL CHECK(requirement IN ('required_full','required_symbol','recommended','optional')),
    PRIMARY KEY(session_id, file_id, content_hash),
    FOREIGN KEY(session_id, snapshot_id) REFERENCES read_sessions(id, snapshot_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, file_id, content_hash) REFERENCES snapshot_files(snapshot_id, file_id, content_hash)
) WITHOUT ROWID;
CREATE TABLE issued_chunks (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte >= start_byte),
    expires_at TEXT NOT NULL,
    confirmed_at TEXT,
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
);
CREATE TABLE served_ranges (
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte > start_byte),
    PRIMARY KEY(session_id, file_id, content_hash, start_byte, end_byte),
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE source_acknowledgements (
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    acknowledged_at TEXT NOT NULL,
    PRIMARY KEY(session_id, file_id, content_hash),
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE coverage_waivers (
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    reason TEXT NOT NULL CHECK(length(trim(reason)) > 0),
    created_at TEXT NOT NULL,
    PRIMARY KEY(session_id, file_id, content_hash),
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE session_observations (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    session_id BLOB NOT NULL REFERENCES read_sessions(id) ON DELETE CASCADE,
    scope_version INTEGER NOT NULL CHECK(scope_version >= 1),
    kind TEXT NOT NULL CHECK(kind IN ('accept_fact','reject_fact','contradiction','unresolved','scope_review')),
    references_json TEXT NOT NULL,
    note TEXT NOT NULL CHECK(length(trim(note)) > 0),
    created_at TEXT NOT NULL
);
CREATE TABLE context_capsules (
    session_id BLOB PRIMARY KEY REFERENCES read_sessions(id) ON DELETE CASCADE,
    canonical_hash TEXT NOT NULL,
    capsule_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE retention_leases (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    generation_id INTEGER REFERENCES generations(id),
    snapshot_id BLOB REFERENCES snapshots(id),
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('query','cursor','session','staging')),
    expires_at TEXT NOT NULL,
    CHECK(generation_id IS NOT NULL OR snapshot_id IS NOT NULL)
);
CREATE INDEX idx_generation_snapshot ON generations(snapshot_id);
-- Retention ranks a repository's refs by the most recent generation each was
-- activated for, then sweeps everything below the retain_refs cut.
CREATE INDEX idx_generation_ref ON generations(repository_id, ref, activated_at);
CREATE INDEX idx_generation_units_unit ON generation_units(unit_id, generation_id);
CREATE INDEX idx_unit_input_file ON unit_inputs(file_id, unit_id);
CREATE INDEX idx_unit_dependencies_reverse ON unit_dependencies(dependency_id, unit_id);
CREATE INDEX idx_nodes_name ON node_facts(name, unit_id, node_id);
CREATE INDEX idx_nodes_qname ON node_facts(qualified_name, unit_id, node_id);
CREATE INDEX idx_nodes_file ON node_facts(file_id, start_byte, unit_id);
CREATE INDEX idx_node_facts_id ON node_facts(node_id, unit_id);
CREATE INDEX idx_relations_from ON relation_ids(from_node_id, kind, to_node_id);
CREATE INDEX idx_relations_to ON relation_ids(to_node_id, kind, from_node_id);
CREATE INDEX idx_relation_facts_id ON relation_facts(relation_id, unit_id);
CREATE UNIQUE INDEX idx_fact_keys ON fact_keys(unit_id, fact_key, coalesce(node_id, x''), coalesce(relation_id, x''));
CREATE INDEX idx_fact_keys_node ON fact_keys(unit_id, node_id);
CREATE INDEX idx_fact_keys_relation ON fact_keys(unit_id, relation_id);
CREATE INDEX idx_evidence_unit ON evidence(unit_id, id);
CREATE INDEX idx_evidence_node ON evidence(node_id, unit_id);
CREATE INDEX idx_evidence_relation ON evidence(relation_id, unit_id);
CREATE INDEX idx_alias_lookup ON native_aliases(scope_key, native_key, unit_id);
-- The identity sweep in unit deletion asks, per candidate node, whether any
-- alias still points at it. Without this index that question is a full scan of
-- native_aliases per node, which is quadratic in the size of the unit being
-- deleted; the same holds for a manifest entry's node.
CREATE INDEX idx_alias_node ON native_aliases(node_id, unit_id);
CREATE INDEX idx_context_entries_node ON context_entries(node_id);
CREATE INDEX idx_search_unit ON search_units(unit_id, rowid);
CREATE INDEX idx_session_expiry ON read_sessions(expires_at, workflow_state);
CREATE INDEX idx_issued_session ON issued_chunks(session_id, confirmed_at, expires_at);
CREATE INDEX idx_observations_session ON session_observations(session_id, scope_version, kind);
CREATE INDEX idx_lease_expiry ON retention_leases(expires_at);
```

Storage invariants beyond DDL:

- File/repository ownership, snapshot size/blob size, executable bits, source hashes, node/evidence byte bounds, visible relation endpoints, and all unit dependency inputs match the selected snapshot before activation.
- Node kinds and relation kinds use the exhaustive model enums. JSON columns are strict, size-bounded typed payloads, not arbitrary extension bags. Repeated evidence, references, and diagnostics have byte/count limits.
- A node identity with no visible node fact is not a query result. A relation identity with no visible relation fact is not a query result. Deleting an optional unit cannot remove a base unit's independent facts.
- `node_facts` and `relation_facts` have supporting evidence before seal. Native aliases remain scoped. `unit_dependencies` is acyclic and references sealed dependencies.
- Session history, context entries, observation references, leases, and capsule scope must belong to the session/manifest's generation and snapshot. Exact actor identity is checked by the application on every session operation.
- Immutable unit tables are never updated after seal. Source manifests, sealed manifests, and capsules are also immutable. Current session pointers and versioned workflow state are explicitly mutable.
- Insert/delete FTS content and index rows in the same write transaction. Before deleting `search_units`, issue the FTS delete with its old indexed values. Cascading content deletion is not a substitute for maintaining the FTS index. Unit/GC deletion uses the store's deletion procedure, not raw `DELETE units`.
- FTS row counts alone do not prove consistency. Use supported FTS integrity checking against external content on initial/release/deep validation; normal unit sealing validates that its transaction emitted the exact expected document keys. Do not perform a global FTS rebuild for a tiny refresh. [12](#ref-12)

<a id="123-generation-activation-and-failure-isolation"></a>
### 12.3 Generation Activation and Failure Isolation

The indexing lock is held from snapshot capture through publication. Seal each bounded unit only after its source and references are validated. Required provider failure aborts the staging generation. An optional provider can contribute completed independent units only when it explicitly reports their coverage; a malformed unit is not partially admitted. A failure must not keep old facts whose input hashes no longer match.

Before publication, calculate the AnalysisKey from the sorted membership rows — each selected unit key together with its `carried` marker and provenance distance (`distance_generations`, `distance_files`), so a generation carrying a stale unit never shares a key with an all-fresh generation of the same units (Section 13.3) — the source snapshot, complete capability report, normalization/schema/config fingerprints, and ranking-relevant inputs. Validate foreign keys and domain invariants, visible endpoints, mandatory provider coverage, source availability, and all required input bindings. Record a validation digest tied to that exact membership; the membership is frozen before the pointer transaction.

```sql
BEGIN IMMEDIATE;
-- Caller holds the indexing lock, and validates expected old pointer/version.
UPDATE generations SET status='active', activated_at=:now
WHERE id=:new_id AND repository_id=:repo AND status='staging'
  AND analysis_key IS NOT NULL;
-- Go must require exactly one affected row or roll back.
UPDATE generations SET status='superseded'
WHERE id=(SELECT generation_id FROM active_generations WHERE repository_id=:repo)
  AND id<>:new_id;
INSERT INTO active_generations(repository_id,generation_id) VALUES(:repo,:new_id)
ON CONFLICT(repository_id) DO UPDATE SET generation_id=excluded.generation_id;
COMMIT;
```

An active-generation lookup and its retention lease are acquired consistently under a short transaction. Cursor continuation renews or validates its lease and never silently switches to the current active generation. A failed publication rolls back all pointer/status changes. Startup recovery marks abandoned staging work failed, reclaims only unreferenced temporary state, and keeps the last valid active pointer.

<a id="124-search-isolation-storage-growth-and-cleanup"></a>
### 12.4 Search Isolation, Storage Growth, and Cleanup

A shared FTS corpus contains documents from several immutable units and retained generations. SQLite's built-in BM25 uses corpus-wide document statistics; filtering rows afterward does not make its statistics generation-local. Therefore canonical lexical ranking uses **generation-scoped BM25 statistics**, not the shared table's raw `bm25()` result. This is an architectural consequence of immutable snapshot queries, not a claim that SQLite implements generation filtering itself. [12](#ref-12)

Obtain matching terms and occurrences from FTS/vocabulary using the same tokenizer, restrict documents through `generation_units`, and compute document frequency/length statistics for that membership. Use a tiny temporary FTS query-token table on a connection when tokenizer access requires it; do not reimplement Unicode tokenization. Stream per-query aggregates and rank with a bounded heap or disk-backed result spool. Cache only small scalar/term statistics keyed by AnalysisKey and the exact query with a byte cap. No second full corpus token index or graph-sized Go map is needed. Section 14 defines scoring and pagination.

Retention keeps the results of the last `retain_refs` distinct refs (branches or commits) the user actually indexed, not the last N index snapshots: switching A → B → C → A must find A's SCIP and dependence units still on disk and reuse them without a run. Each activated generation records the ref it was built from; a ref's most recent generation is retained while the ref is among the last `retain_refs` worked on, and every unit any retained generation references is retained with it. Sealed but not yet activated units (a dependence unit still refreshing when the user switched away) are retained by the same rule through the generation they were produced for. There is no default size limit on this store; `[index] max_retained_bytes` is user-set only, `0` by default, and when set it evicts least-recently-used refs first and never the active one, exactly the user-set-only posture memory has. Generation removal deletes only membership and metadata after leases/session references are resolved. Units referenced by another retained generation or unit dependency remain. Remove unreachable units in reverse dependency order through the FTS-aware store procedure, then orphan identities and origin runs, then unreferenced source blobs using the GC grace protocol. Report disk categories separately; generation pruning does not justify unbounded retained completed sessions or query spools.
An alias may identify a node defined by a completed dependency rather than copying its attributes into the alias-owning unit; the seal validator requires that target to be visible through the unit/dependency closure. An idempotency-key collision with a different open-request hash fails rather than reusing a session. Snapshot header counts are computed/validated at seal, not rescanned on every status call. Query spools are bounded private files whose signed cursor header and durable lease bind their generation, expiry and immutable query; no second database catalog is introduced.


---

<a id="13-indexing-coordinator-and-freshness"></a>
## 13. Indexing Coordinator and Freshness

<a id="131-scheduling-and-invalidation"></a>
### 13.1 Scheduling and Invalidation

The coordinator owns tool and trigger detection, the provider DAG, resource reservations, unit scheduling, reuse, capability reporting, and activation. Reuse must be demonstrated by equal UnitID and validated input/dependency membership, not assumed because a file's path is unchanged. File-local providers reprocess changed files only; semantic providers use their declared package/workspace unit. A source addition, deletion, rename, changed manifest, build flag, query/grammar version, or dependency catalog can invalidate downstream units.

Stage data on disk; compute dependency closure with bounded queues and indexed reverse dependency lookups. If a semantic provider cannot safely invalidate locally, rerun its declared larger unit and report it. Do not secretly rerun every base provider over the full repository on each small edit. A no-op refresh emits reuse counts and performs no parse or FTS rewrite. Early cutoff: a semantic unit's inputs are keyed on a declaration-only signature digest of each member file (exports, types, function signatures, imports; not bodies) in addition to the content hash of the unit's own files, so an edit confined to function bodies in file F re-runs only F's own unit and never the SCIP or dependence units of modules that merely depend on F's module. The digest is per language, conservative (anything the language can observe across files is in it), and proven by differential tests before a language may use it (Task 23); until proven, that language falls back to content-hash invalidation. Late-sealing optional units (Section 11.6) publish the same way: a new generation reusing every active unit plus the sealed one, then one atomic activation; the active generation is never edited in place.

Independent ready units run only when global byte/worker/disk admission permits. Acquire reservations in a fixed order to avoid deadlock, and release exactly once on all paths. Batches are written serially but providers can prepare bounded work concurrently. A blocked sink must propagate cancellation back to the producer. Repeated failures have a bounded retry policy; no busy loop or retry storm.

<a id="132-watch-mode-and-cross-process-coordination"></a>
### 13.2 Watch Mode and Cross-Process Coordination

Default debounce is 250 ms and reconciliation interval is 30 seconds. Coalesce paths; cap pending path count and bytes; overflow collapses to one full-reconciliation flag. During a run, new changes accumulate for the next pass. A cross-process workspace lock prevents concurrent `index`, `refresh`, and watch/MCP builds; a second explicit caller receives `busy` or joins a bounded wait, not another writer coordinator.

`fsnotify` does not recursively watch an entire tree automatically. Add permitted directories, handle new/deleted directories, prefer directory watches for atomic editor replacement, and fall back to bounded polling/reconciliation when OS watch limits or filesystem support prevent coverage. Git status alone is not the source of truth for non-Git/untracked or timestamp-preserving changes. Report watch coverage, last successful reconciliation, and pending state. [14](#ref-14)

<a id="133-capability-freshness"></a>
### 13.3 Capability Freshness

Use `fresh`, `partial`, `stale`, `unavailable`, and `failed` per capability/scope, and `fresh`, `degraded`, or `failed` for generation health. Store precise reason codes separately from user-readable remediation. An optional disabled tool can be unavailable while the base generation is fresh. While a semantic unit is refreshing after an edit, its previous sealed unit is carried into the new generation and its capabilities answer as `stale` with a provenance distance (the number of generations, and the changed files, between the unit's snapshot and the active one) rather than `pending`; the carried unit is replaced by the fresh one at the next activation. A carried unit is never reported `fresh`, and a unit whose inputs no longer exist (deleted files) is not carried. An enabled requested provider that failed makes that capability failed and the active generation degraded if required base coverage still passes.

An active generation is coherent with its own snapshot even when the worktree is newer. Status distinguishes `snapshot_coherent`, `worktree_changed`, `refresh_pending`, `superseded`, and provider completeness. A valid historical query is not an assertion of current worktree freshness. No `fresh` status is inferred merely because an executable was found or a run emitted some rows.

---

<a id="14-search-and-query-engine"></a>
## 14. Search and Query Engine

<a id="141-typed-requests-and-bounded-results"></a>
### 14.1 Typed Requests and Bounded Results

Every result has a `Binding`, per-capability completeness, `truncated`, a reason when truncated, and a continuation cursor when safe continuation exists. Query deadlines and hard work budgets are distinct from page size. Explicitly resolve ambiguous names; never select the first candidate silently.

```go
type PageRequest struct {
    Limit int `json:"limit"`
    Cursor string `json:"cursor,omitempty"`
}
type SearchRequest struct {
    GenerationID GenerationID `json:"generation_id,omitempty"`
    Query string `json:"query"`
    Kinds []NodeKind `json:"kinds,omitempty"`
    Languages []string `json:"languages,omitempty"`
    Paths []string `json:"paths,omitempty"`
    Page PageRequest `json:"page"`
}
type GraphRequest struct {
    GenerationID GenerationID `json:"generation_id,omitempty"`
    Start []NodeID `json:"start"`
    Relations []RelationKind `json:"relations,omitempty"`
    Direction string `json:"direction"`
    MaxDepth int `json:"max_depth"`
    MaxVisited int `json:"max_visited"`
    MaxEdges int `json:"max_edges"`
    Page PageRequest `json:"page"`
}
type QueryMeta struct {
    Binding Binding `json:"binding"`
    Completeness []CapabilityState `json:"completeness"`
    Truncated bool `json:"truncated"`
    TruncationReason string `json:"truncation_reason,omitempty"`
    NextCursor string `json:"next_cursor,omitempty"`
}
```

All arrays, query text, filters, offsets, and limits are validated before work begins. GenerationID zero selects the active generation once at request start. A supplied cursor already selects its pinned generation; conflicting request fields fail. The same rules apply to symbol resolution, reference occurrences, repository maps, evidence lists, context entries, exclusions, slices, and coverage files.

<a id="142-retrieval-and-deterministic-ranking"></a>
### 14.2 Retrieval and Deterministic Ranking

Retrieval tiers are exact normalized path, exact qualified name, qualified-name prefix, exact short name, and lexical FTS. Case-sensitive identities are not indiscriminately lowercased. Normalize query whitespace and path separators carefully; SQL parameters and an explicit literal-text FTS encoder prevent SQL/FTS syntax injection. Prefix queries use indexed ranges and escaped semantics, not an unbounded leading-wildcard scan.

Lexical retrieval retains BM25 relevance, with membership-scoped statistics as specified in Section 12.4. Use the same versioned formula and column weights for a pinned analysis: name 5, qualified name 5, signature 2, path 2, body 1; `k1=1.2`, `b=0.75`. Compute membership-specific document count, average token length, phrase/term frequency, and document frequency. Do not use a score from the shared global corpus and call it generation-local. Quoted multi-token phrases use actual phrase occurrences, not the sum of unrelated individual tokens. [12](#ref-12)

Map the score to a fixed integer ranking component with documented rounding. Validate identical ranking on all supported release targets; any platform-sensitive numerical edge must be resolved in the shared scorer before claiming cross-platform canonical equivalence. Integer graph contributions and deterministic final tie-breakers do not alone fix an unstable floating input. Canonical regression fixtures include near-ties, duplicate occurrences, punctuation, and unrelated generation activation/pruning.

Tie-break by retrieval tier, descending score, normalized path, start byte, NodeID, then evidence/search key. Deduplicate hits by canonical entity or file chunk as the endpoint specifies, preserving bounded reasons and occurrence counts. Search documents carry signatures and linked source positions; generic results never return source bodies. Raw source is obtained through a context session only.

<a id="143-graph-queries-and-impact"></a>
### 14.3 Graph Queries and Impact

Required operations remain definitions, references, implementations, callers, callees, incoming/outgoing neighbors, tests, documentation, configuration, shortest dependency paths, impact expansion, and package/module dependency rollups. BFS serves unweighted neighborhoods; Dijkstra uses nonnegative deterministic integer costs for weighted paths. Include direction, visited count, returned-edge count, and truncation reason.

Fetch adjacency in bounded indexed batches across each frontier rather than one SQL query per node. Use the relation's target index for reverse queries. Bound seeds, depth, nodes, edges, frontier bytes, reason paths per result, total explanation bytes, and duration. Preserve visited/frontier state in a bounded disk-backed cursor spool when continuation needs it; never serialize a 50,000-node visited set into a token. A hit limit produces a continuation; exhausting a hard traversal budget produces an explicit incomplete result or a resumable bounded work state, never a claim of exhaustive impact.

Impact expands an explicit relation allowlist, prioritizing implementations/overrides, calls, reads/writes, tests, configuration, documents, dependencies, and imports. Direction matters: callers can be impacted by a callee contract change; downstream dependencies may need reading without necessarily being modified. Every affected entry includes an evidence-backed reason. Package rollups aggregate distinct pairs and evidence counts without fabricating precise symbol calls.

<a id="144-cursor-and-cache-contracts"></a>
### 14.4 Cursor and Cache Contracts

One shared internal cursor codec signs bounded versioned payloads with an installation-local private random key. Payload includes endpoint, generation/AnalysisKey, normalized query/filter/ordering hash, last sort tuple or spool ID, lease ID, and expiry. Reject tampering, expired state, another endpoint/query/filter, and unknown versions. A cursor key is not a user authentication system.

Use keyset pagination or bounded spools, not growing SQL offsets. Deduplication precedes paging so duplicates cannot hide later hits. Temporary query spools count against disk quotas, have TTLs, and retain the generation they reference. Replaying a cursor is deterministic and non-destructive. Small cache entries are byte-accounted and generation-qualified; no persistent whole-graph cache is justified. Cancellation closes rows, rolls back transactions, cancels subprocess requests, and releases leases/reservations.
Generation membership filtering occurs before scoring, deduplication and top-K/page limits. Timeouts/cancellation return an explicit noncanonical incomplete response and never persist a supposedly reproducible completed manifest. Deterministic work-limit prefixes use stable ordering; time-dependent cutoffs are not canonical compiler inputs.


---

<a id="15-deterministic-context-compiler"></a>
## 15. Deterministic Context Compiler

<a id="151-inputs-output-and-canonical-identity"></a>
### 15.1 Inputs, Output, and Canonical Identity

```go
type Phase string // sweep, verify, consolidate
type Budget struct {
    MaxEstimatedTokens int64 `json:"max_estimated_tokens"`
    MaxBytes int64 `json:"max_bytes"`
    MaxFiles int `json:"max_files"`
    MaxSlices int `json:"max_slices"`
}
type ContextRequest struct {
    Task string `json:"task"`
    Seeds []string `json:"seeds,omitempty"`
    Phase Phase `json:"phase"`
    Budget Budget `json:"budget"`
    GenerationID GenerationID `json:"generation_id,omitempty"`
}
type PlanRequest struct {
    Context ContextRequest `json:"context"`
    ActorID string `json:"actor_id"`
    IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type ContextEntry struct {
    Ordinal int `json:"ordinal"`
    NodeID NodeID `json:"node_id,omitempty"`
    FileID FileID `json:"file_id,omitempty"`
    Requirement string `json:"requirement"`
    ScoreMicros int64 `json:"score_micros"`
    EstimatedBytes int64 `json:"estimated_bytes"`
    EstimatedTokens int64 `json:"estimated_tokens"`
    Reasons []string `json:"reasons"`
    EvidencePaths [][]RelationID `json:"evidence_paths,omitempty"`
}
type ContextSlice struct {
    Index int `json:"index"`
    EntryOrdinals []int `json:"entry_ordinals"`
    EstimatedBytes int64 `json:"estimated_bytes"`
    EstimatedTokens int64 `json:"estimated_tokens"`
}
type ContextManifest struct {
    ID ManifestID `json:"id"`
    Binding Binding `json:"binding"`
    Phase Phase `json:"phase"`
    RequestHash string `json:"request_hash"`
    PolicyVersion string `json:"policy_version"`
    CanonicalHash string `json:"canonical_hash"`
    Budget Budget `json:"budget"`
    EntryCount int `json:"entry_count"`
    SliceCount int `json:"slice_count"`
    Completeness []CapabilityState `json:"completeness"`
    ScopeComplete bool `json:"scope_complete"`
    EstimateMethod string `json:"estimate_method"`
    CreatedAt time.Time `json:"created_at"`
}
type PlanResult struct {
    Manifest ContextManifest `json:"manifest"`
    SessionID SessionID `json:"session_id"`
    ActorID string `json:"actor_id"`
}
```

Large manifests are persisted as a header plus normalized ordered entries/slices/exclusions and read by bounded pages. Slice entries reference ordinals rather than duplicating complete entry structs. A complete export streams canonical JSON; do not collect an entire source bundle or duplicate serialized payloads in RAM.

Manifest identity hashes AnalysisKey, normalized request, ranking and budget policy versions, and other semantic compiler inputs. Its canonical projection excludes CreatedAt, local GenerationID, random session/run IDs, cursor tokens, and timings; it includes SnapshotID, unit/evidence semantic keys, selected scope and exclusions. Runtime results retain all operational binding/audit fields. Repeated compile requests reuse the immutable manifest. Repeated plan requests reuse a session only for an explicit idempotency key bound to the same actor/request; identical tasks from different actors always get separate sessions. No universal session reuse by manifest ID.

Only sweep and verify can open a new session. Consolidate is compiled by the workflow service for an existing session using its current observation/scope digest. Observations and coverage are not omitted from a consolidation cache key.

<a id="152-seed-discovery-and-required-scope"></a>
### 15.2 Seed Discovery and Required Scope

Resolve explicit seeds first; then backtick-delimited identifiers/paths, path-like tokens, qualified identifiers, exact resolution, FTS terms, and low-priority changed files. Preserve ambiguity; no LLM rewrites task intent. An empty or ambiguous scope produces a discovery result, not an implementation-ready plan.

For verify, every selected implementation file and its relevant callers, callees, contracts/types, state ownership, dependencies, configuration, tests, documentation, and integration/registration boundaries must be assigned a full-file requirement when it informs the decision. Follow reachability through actual wiring, exports, registries, reflection/config conventions, and shared helpers, not just textual references. The graph is a starting point, not proof that all dynamic or external dependencies were found.

Record missing semantic coverage and unresolved boundary questions. When a bounded traversal truncates or a dependency is unresolved, `scope_complete=false`; rank-based omission cannot convert it to true. The actor can add discovered seeds/files through `context include` within the pinned snapshot, producing a new immutable manifest and scope version. This is necessary when full reading reveals a consumer that the graph missed. Scope review must be repeated for the new scope; same-actor receipts for identical retained bytes remain valid.

<a id="153-graph-expansion-and-ranking"></a>
### 15.3 Graph Expansion and Ranking

Use scaled integer weights for graph expansion. Defaults preserve the original intent:

| Contribution | Weight |
|---|---:|
| Explicit seed | 1.00 |
| Exact symbol/path | 0.98 |
| Implements/overrides | 0.94 |
| Tests | 0.92 |
| Calls | 0.90 |
| Reads/writes; data/control dependence | 0.88 |
| References | 0.78 |
| Depends on/configures | 0.72 |
| Imports | 0.60 |
| Documents | 0.55 |
| Same package/module | 0.35 |

Precision multipliers are compiler 1.00, language server 0.95 for labeled overlay results, static analysis 0.90, syntax 0.72, heuristic 0.45. Canonical plans do not silently incorporate ephemeral overlay facts. Depth decay is 0.65 per hop after the first. Bounded boosts are exact task identifier +0.20, active captured change +0.12, associated test/contract +0.10, and package centrality up to +0.05. Compute only persisted or bounded centrality inputs, not whole-graph centrality during each plan.

Specify integer scale `1_000_000`, multiplication/division rounding, overflow checks, deterministic path aggregation, and boost caps in the shared scorer. Default aggregation is the maximum supported path contribution plus unique bounded boosts, avoiding cycle/path-count inflation. Tie-break by requirement, descending score, normalized path, start byte, then entity ID. Bound stored explanation paths; report additional-path counts rather than growing an exponential path list.

<a id="154-budgeting-and-context-slices"></a>
### 15.4 Budgeting and Context Slices

The default token estimate is a heuristic, **not a conservative guarantee for every tokenizer**:

```go
func EstimateTokensUTF8Bytes(n int64) (int64, error) {
    if n < 0 { return 0, errors.New("negative source byte count") }
    // Equivalent to ceil(n/3), without n+2 overflow or loading file bytes.
    tokens := n / 3
    if n%3 != 0 { tokens++ }
    return tokens, nil
}
```

The exported method label is `utf8_bytes_div_3_heuristic`. Exact local tokenizers can be adopted when a real consumer needs them, but no tokenizer API/service is mandatory. Enforce exact byte limits independently. Count task/provenance headers, metadata, repeated contracts, delimiters, escaping, and encoded source in the actual output budget; source-file length alone is not the serialized message length.

MaxBytes and MaxEstimatedTokens apply per slice; MaxFiles applies to distinct selected files across the entire plan; MaxSlices caps total slices. The compiler also enforces global candidate, stored-manifest, explanation, and total-export byte caps. An impossible requested budget returns a typed minimum-budget/scope-limit error with the necessary floor and missing requirements. It does not drop required files, mark them optional, split a file into misleading independently complete slices, or fabricate an exhaustive plan.

Mandatory metadata and primary full files are selected first, followed by required direct contracts/tests/callers and then recommended/optional entries. Group required entries by strong dependency components and pack deterministically. Split an oversized component at file boundaries with repeated bounded task/contracts, preserving full-file requirements. An individual required file larger than the per-slice ceiling requires a larger explicit slice budget, while transport may still deliver it in many chunks. Reading one chunk or one slice never grants readiness for the entire task.

| Phase | Default representation |
|---|---|
| Sweep | Repository map, symbols, signatures, manifests and ranked candidates. No implementation readiness. |
| Verify | Complete relevant files, callers/callees, contracts, state, tests, configuration, dependencies and integration evidence. |
| Consolidate | Recorded accepted/rejected facts, contradictions, unresolved items, actor review, coverage/waivers and a deterministic capsule. |
For base-only operation, an absent optional compiler provider does not by itself prevent verification. The actor may finish manually tracing the declared local boundaries using fully served source and record those source-backed results; provider completeness stays honestly lower. Scope completeness describes the declared task boundary, not proof of every possible dynamic/external dependency. A required source file excluded from the pinned snapshot cannot be added by inventing a FileID: capture a new explicitly expanded snapshot and open a new session.


---

<a id="16-source-coverage-gate"></a>
## 16. Source-Coverage Gate

<a id="161-honest-actor-scoped-coverage"></a>
### 16.1 Honest Actor-Scoped Coverage

Coverage proves the source byte ranges issued by the service and confirmed by the client for one actor's pinned session. It does **not** prove attention, retention, comprehension, or correctness. The server cannot stop edits through unrelated tools. Strict enforcement belongs to the orchestrator's write gate and must apply separately to every dispatched agent.

Every session belongs to a stable actor ID plus a unique session ID. The actor is an orchestration identity, not authentication against a malicious local user. Require it on reads, receipts, acknowledgments, observations, waivers, includes, transitions, close, and status. Do not let one subagent's completed session satisfy another's requirement. Shared immutable manifests and source blobs are safe; shared read credit is not.

<a id="162-source-read-and-receipt-contracts"></a>
### 16.2 Source Read and Receipt Contracts

```go
type ReadChunkRequest struct {
    SessionID SessionID `json:"session_id"`
    ActorID string `json:"actor_id"`
    FileID FileID `json:"file_id"`
    Offset uint64 `json:"offset"`
    MaxBytes uint32 `json:"max_bytes"`
    ConfirmReceipts []string `json:"confirm_receipts,omitempty"`
}
type ReadChunkResponse struct {
    Binding Binding `json:"binding"`
    FileID FileID `json:"file_id"`
    ContentHash string `json:"content_hash"`
    ByteRange ByteRange `json:"byte_range"`
    LineRange SourceRange `json:"line_range"`
    Encoding string `json:"encoding"` // utf8 or base64
    Content string `json:"content"`
    PartialLine bool `json:"partial_line"`
    NextOffset *uint64 `json:"next_offset,omitempty"`
    Receipt string `json:"receipt"`
    Coverage string `json:"coverage"` // unserved, partial_served, full_served
}
type AcknowledgeRequest struct {
    SessionID SessionID `json:"session_id"`
    ActorID string `json:"actor_id"`
    Kind string `json:"kind"` // receipt or file
    Receipts []string `json:"receipts,omitempty"`
    FileID FileID `json:"file_id,omitempty"`
}
```

`Kind=receipt` confirms one or more returned signed chunk tokens; `Kind=file` records a separately labeled client statement for a fully served file. These are distinct operations behind the existing acknowledgment surface, not interchangeable semantics. Cap receipt batches at 16. The next read may echo prior tokens to avoid an extra round trip; the final chunk still needs explicit confirmation. Acknowledging a file without full confirmed coverage fails.

Read validates actor/session/TTL, manifest membership, offset/bounds and content hash, verifies the accessed CAS blocks, and persists an **issued**, not yet confirmed, chunk record. Its token binds session, actor, snapshot, file, hash, byte interval, issuance ID and expiry with the shared signing utility. Echoing that token transactionally marks it confirmed and merges served intervals. A broken output pipe, serialization failure, or disconnected MCP call cannot by itself add full-served credit. Duplicate confirmations are idempotent. Receipt possession is evidence of delivery to that client, not evidence of reading.

The default raw source chunk is 64 KiB; configured raw maximum is 1 MiB. Source responses have a separately accounted wire budget allowing encoding/envelope expansion, with a 7-MiB hard ceiling. Generic tools remain under the much smaller metadata response limit. For a lower requested wire budget, reduce raw chunk size before emitting and never silently exceed it.

Prefer complete UTF-8 sequences and complete line endings when they fit. A line larger than the chunk limit is split at a valid boundary with `partial_line=true`; never return zero progress waiting for a newline. Reject an offset into a UTF-8 continuation byte. Preserve CRLF and a final line without newline. Invalid UTF-8 is returned losslessly as base64 with byte-based ranges, not coerced into replacement characters. Enforce a nonempty request size sufficient for a UTF-8 code point, except valid EOF/empty-file responses.

<a id="163-interval-merging-empty-files-and-readiness"></a>
### 16.3 Interval Merging, Empty Files, and Readiness

Merge confirmed half-open intervals under a short transaction, including overlap and adjacency. `full_served` requires the union `[0,size)` for that exact session/file/hash. An empty file requires an explicitly confirmed zero-length EOF receipt; it is not automatically considered opened simply because it contains no bytes. A nonempty-file EOF receipt does not cover missing earlier bytes. Served range ends cannot exceed stored source size.

A session can remain fully served for its historical snapshot after a newer generation activates. Status therefore separates `read_complete_for_snapshot` from `ready_for_implementation`. `superseded` is that generation fact and is answered from the repository's active generation, not from per-file hashes: a republished generation supersedes a session even when every pinned file is byte-identical. Status also carries `guarantee_limit`, which is never empty while `ready_for_implementation` is true: it states in words that the answer is point-in-time, so a caller cannot receive a bare `true` and treat it as a standing permission. Strict readiness additionally requires the verify phase, resolved and complete scope, a current-scope actor review, no blocking unresolved dependency, no required-file waiver, and successful current-source validation. Historical reads continue to work even when readiness becomes false.

Waivers remain available for audit and explicitly permitted exploratory consolidation but never fabricate coverage or strict implementation readiness. An acknowledgment never replaces a receipt or source-backed review. A strict orchestrator must check the same gate immediately before enabling its write phase and revalidate affected hashes after changes; a stale Boolean cached earlier is not authorization.
An integrating orchestrator must also pair the readiness check with expected-content-hash validation at each actual write, or hold an appropriate coordinated source lock. Intervening source changes invalidate the gate. Codectx does not claim an atomic multi-file write transaction it does not control. Signed token payloads include a purpose discriminator; cursor tokens and source receipts are never accepted interchangeably.


---

<a id="17-workflow-state-machine"></a>
## 17. Workflow State Machine

<a id="171-states-and-guards"></a>
### 17.1 States and Guards

```text
SWEEP_OPEN -> VERIFY_OPEN -> CONSOLIDATE_OPEN -> COMPLETE
                  |
                  +-- context include -> new manifest + new scope version
Any open state -> CLOSED (explicit close or expiry handling)
```

Direct verify planning is allowed only with at least one resolved seed. Direct consolidation planning is not. Every transition uses a compare-and-swap state version in one transaction; concurrent clients cannot both advance from the same version. Session manifests are immutable, the current-manifest pointer is versioned, and history is append-only. Manifest generation/snapshot binding cannot change within a session.

| Operation | Required guard and effect |
|---|---|
| Direct verify / Sweep to Verify | Resolved seed, valid verify manifest, complete declared requirements; populate session files and history. Incomplete discovery may enter verify for further reading but cannot grant readiness. |
| Include more scope | Open sweep/verify session; canonical files/seeds belong to the pinned generation. Recompile immutable manifest, increment scope/state version, retain same-actor same-hash read coverage, invalidate old scope-review readiness. |
| Verify to Consolidate | All required source fully served and current-scope review recorded, or explicit trusted exploratory policy with audited waivers. Waivers set `strict_gate_satisfied=false`; unresolved items remain visible. |
| Consolidate to Complete | Persist a deterministic capsule for the exact manifest, scope, observation and coverage digest; verify its hash, then change state atomically. |
| Close/expire | Stop new mutations, release active-session lease according to retention policy; preserve closed audit artifacts for the configured finite period. |

Historical consolidation does not require that the current worktree still match, but the resulting capsule must report supersession and cannot be presented as an approval to modify the current code. Completion is a workflow state, not a claim that implementation is correct.

<a id="172-observations-and-source-backed-review"></a>
### 17.2 Observations and Source-Backed Review

Observation kinds are `accept_fact`, `reject_fact`, `contradiction`, `unresolved`, and `scope_review`. Every request contains actor/session, expected scope version, bounded canonical relation/node references, optional source file/hash/ranges, and a nonempty note. References must be visible in the pinned generation; cited source intervals must already be confirmed served by the same actor. Accept/reject requires at least one relation; contradiction requires at least two distinct claim references; unresolved requires a specific node/relation/source and reason. The product records a client's contradiction assertion without pretending to solve general semantic inconsistency.

A `scope_review` is a structured, explicitly labeled actor attestation with these required categories: complete files read; callers/consumers; contracts/types; state/lifecycle; dependencies; integration/registration boundaries; existing shared utilities considered; remaining uncertainty. Each category contains relevant source references or an explicit source-backed explanation of non-applicability. It is bound to the current manifest hash and scope version. Review cannot mark a file read without confirmed full coverage, and a nonempty blocking uncertainty prevents strict readiness.

Observation IDs hash session/actor/scope/kind and canonically sorted semantic references plus note; duplicate submission is idempotent. Reject an attempt to mutate an existing observation in place. A new include/review produces new records rather than rewriting historical assertions. No natural-language summaries or accepted facts are generated by codectx itself.

<a id="173-waivers-and-deterministic-capsules"></a>
### 17.3 Waivers and Deterministic Capsules

A waiver records session/actor, required file, pinned content hash, nonempty reason and UTC timestamp. It neither updates coverage nor makes `ready_for_implementation=true`. The default execution policy does not use waivers to bypass the user's full-file requirement. An exploratory override must be explicit and visible in every downstream capsule/status.

```go
type Capsule struct {
    SessionID SessionID `json:"session_id"`
    ActorID string `json:"actor_id"`
    Binding Binding `json:"binding"`
    ManifestHash string `json:"manifest_hash"`
    ScopeVersion int `json:"scope_version"`
    Scope []NodeID `json:"scope"`
    AcceptedFacts []FactReference `json:"accepted_facts"`
    RejectedFacts []FactReference `json:"rejected_facts"`
    Contradictions []ObservationReference `json:"contradictions"`
    Unresolved []ObservationReference `json:"unresolved"`
    ScopeReviewIDs []string `json:"scope_review_ids"`
    Coverage []FileCoverage `json:"coverage"`
    Waivers []WaiverRecord `json:"waivers"`
    Completeness []CapabilityState `json:"completeness"`
    StrictGateSatisfied bool `json:"strict_gate_satisfied"`
    CanonicalHash string `json:"canonical_hash"`
    CreatedAt time.Time `json:"created_at"`
}
```

`FactReference` carries RelationID, sorted EvidenceIDs and ObservationID. `ObservationReference` carries ObservationID, canonical node/relation/source references and note. `FileCoverage` carries FileID, content hash, size, confirmed served bytes, coverage state and waiver flag. `WaiverRecord` carries FileID/hash/actor/reason/timestamp. All are bounded model records defined in Task 2, with source coordinates from the shared model. Capsule data is assembled only from stored facts and explicit observations.

Canonical capsule hashing excludes operational timestamps, local generation/run IDs and transport tokens, and includes manifest hash, semantic analysis binding, current scope/review, observation semantics, coverage and waiver semantics. Preserve audit timestamps in the stored public record. Repeated completion/retrieval returns the first stored capsule, not a new timestamp. Use paginated/streamed export if the capsule exceeds a generic response budget; exceeding the configured total persisted capsule budget is explicit, never silent omission.

---

<a id="18-cli-contract"></a>
## 18. CLI Contract

<a id="181-commands-and-shared-options"></a>
### 18.1 Commands and Shared Options

```text
codectx init [path] [--force]
codectx doctor [path] [--offline] [--deep] [--json]
codectx index [--repo PATH] [--full] [--rebuild] [--watch] [--json]
codectx refresh [paths...] [--repo PATH] [--json]
codectx status [--repo PATH] [--json]
codectx watch [--repo PATH] [--json]
codectx repo-map [path] [--depth N] [--limit N] [--cursor TOKEN] [--json]
codectx search <query> [--kind K] [--language L] [--limit N] [--cursor TOKEN] [--json]
codectx symbol <name-or-id> [--limit N] [--cursor TOKEN] [--json]
codectx refs <name-or-id> [--kind references|implements|type-definition] [--limit N] [--cursor TOKEN] [--json]
codectx callers <name-or-id> [--depth N] [--limit N] [--cursor TOKEN] [--json]
codectx callees <name-or-id> [--depth N] [--limit N] [--cursor TOKEN] [--json]
codectx path <from> <to> [--relations CSV] [--depth N] [--json]
codectx impact <name-or-id> [--depth N] [--limit N] [--cursor TOKEN] [--json]
codectx context plan --task TEXT --phase sweep|verify --actor ID [--seed SEED] [--budget N] [--json]
codectx context status SESSION --actor ID [--limit N] [--cursor TOKEN] [--json]
codectx context next SESSION --actor ID [--json]
codectx context entries SESSION --actor ID [--view entries|slices|excluded] [--limit N] [--cursor TOKEN] [--json]
codectx context include SESSION --actor ID --seed SEED --expected-version N [--json]
codectx context read SESSION FILE --actor ID [--offset N] [--max-bytes N] [--confirm-receipt TOKEN] [--json]
codectx context acknowledge SESSION --actor ID --receipt TOKEN [--json]
codectx context acknowledge SESSION FILE --actor ID --file-review [--json]
codectx context waive SESSION FILE --actor ID --reason TEXT [--json]
codectx context record SESSION --actor ID --kind KIND --expected-scope N --input FILE [--json]
codectx context advance SESSION verify|consolidate|complete --actor ID --expected-version N [--json]
codectx context capsule SESSION --actor ID [--limit N] [--cursor TOKEN] [--json]
codectx context close SESSION --actor ID [--json]
codectx context export SESSION --actor ID --output FILE
codectx tools status [--json]
codectx tools prefetch [--all | --for-repo PATH] [--json]
codectx tools verify [--json]
codectx tools gc [--json]
codectx mcp serve --repo PATH [--watch]
codectx version [--json]
```

`record --input` reads bounded typed observation JSON, including source references and structured scope review; simple `--relation`, `--node`, `--source file:start:end`, and `--note` flags remain supported for ordinary observations, mutually exclusive with `--input`. A source flag resolves its hash from the pinned session, never the live file. All list commands support the same cursor policy. Applicable commands also accept `--repo`, `--generation`, and bounded `--timeout` options; graph options include visited/edge caps. Context byte/file/slice budgets have explicit flags in addition to the token-estimate `--budget` shorthand.

`index --scip-index PATH --scip-inputs PATH` imports a supplied index with its optional input-hash manifest. Managed indexer profiles need no configuration; `tools` commands are conveniences for CI and offline hosts, never a prerequisite (Section 11.7). `init` is the only ordinary command writing project configuration; it refuses overwrite unless explicitly forced. `context export` writes only the explicitly selected output and refuses to overwrite source or existing files without a specific user request.

<a id="182-output-and-exit-codes"></a>
### 18.2 Output and Exit Codes

| Code | Meaning |
|---:|---|
| 0 | Successful complete operation; a normally paginated page is success. |
| 2 | Invalid command, arguments, input shape or incompatible flags. |
| 3 | Workspace/configuration/trust/schema error. |
| 4 | No usable active generation. |
| 5 | Required provider/index/integrity failure. |
| 6 | Policy/read/scope/freshness gate not ready. |
| 7 | Hard query/resource/deadline limit, or explicit incomplete work. |
| 8 | Version conflict, workspace busy, expired session/cursor or stale scope. |
| 10 | Internal error. |

Every `--json` request emits one bounded envelope with `schema_version`, `command`, `ok`, `data`, `warnings`, and `error`, including domain/argument errors after JSON mode is selected. JSON error data goes to stdout in that envelope, while logs go to stderr; do not also print a conflicting human error or call `os.Exit` inside services. Partial results on a hard limit use the same envelope with their explicit incompleteness and code 7. A broken stdout pipe fails the command and does not confirm source receipt delivery.

Human output may truncate display while showing omitted counts and continuation. The service result is not silently truncated by a renderer. Install signal cancellation at the process boundary; all rows, processes, leases and reservations are released on cancellation. Help/version remain cheap and usable without Git, a database, or optional analyzers.
Symbol, reference and call-hierarchy commands expose `--semantic-source canonical|lsp` and a `--profile` selector naming a managed server; canonical is the default. `symbol --operation resolve|document-symbols|workspace-symbols|definition` selects the typed symbol operation. These flags are forwarded by the shared facade, keeping every supported LSP operation reachable rather than leaving an unwired manager.

Exploratory consolidation with recorded waivers additionally requires the explicit user-level `allow_exploratory_waiver_consolidation` setting; it is disabled by default, forbidden in project config, and never changes strict readiness.


---

<a id="19-mcp-contract"></a>
## 19. MCP Contract

<a id="191-official-sdk-and-protocol-baseline"></a>
### 19.1 Official SDK and Protocol Baseline

Use the official MCP Go SDK pinned to v1.7.0 initially, protocol `2026-07-28`, and stdio transport. The July 2026 protocol removes the old MCP initialize/initialized handshake and supports `server/discover` with request metadata. Do not copy an old initialization test and call it current-protocol coverage. Let the pinned SDK implement its wire protocol; no product-owned legacy adapter is required. Product context sessions are application data, independent of whether the MCP transport is stateless. [2](#ref-2) [3](#ref-3) [22](#ref-22) [36](#ref-36)

Existing SDK compatibility behavior is not a reason to implement or preserve parallel application APIs. Test the selected current protocol explicitly. New SDK versions require an intentional exact pin and schema/protocol verification, not an open-ended `v1.7.0 or newer` promise.

<a id="192-required-tool-surface"></a>
### 19.2 Required Tool Surface

| Tool | Contract |
|---|---|
| `codectx_index_status` | Active snapshot/generation, provider/capability coverage, resource/freshness warnings. |
| `codectx_refresh_index` | Refresh private cache with trust and resource policy; no source edits. Takes no arguments: a full build and a rebuild into a new cache are `codectx index --full`/`--rebuild`, not tool inputs, and no watcher starts from a tool. |
| `codectx_repo_overview` | Bounded repository/package/module/language map. |
| `codectx_search` | Paginated lexical/path/symbol discovery without source bodies. |
| `codectx_find_symbol` | Resolve canonical IDs or names with explicit ambiguity. |
| `codectx_symbol_info` | Definition metadata, signature, scope and paginated evidence. |
| `codectx_references` | Reference occurrences and implementations, distinguish relations from occurrences. |
| `codectx_callers` | Bounded incoming graph with reasons. |
| `codectx_callees` | Bounded outgoing graph with reasons. |
| `codectx_dependency_path` | Bounded shortest path with evidence. |
| `codectx_impact` | Affected scope and required boundaries, with completeness. |
| `codectx_context_plan` | Compile/reuse manifest and open an actor-specific sweep/verify session. |
| `codectx_context_status` | Read completeness, phase, strict gate, scope version and supersession. |
| `codectx_context_next` | Next required file/offset or manifest action; **metadata only**. |
| `codectx_context_entries` | Paginated entries, slices or exclusions using a typed view selector. |
| `codectx_context_include` | Add discovered scope to a pinned session; invalidate old scope review. |
| `codectx_read_source` | Exact encoded source chunk plus issued receipt, session/actor required. |
| `codectx_context_acknowledge` | Confirm signed receipts or separately record full-file client acknowledgment. |
| `codectx_context_waive` | Auditable waiver; cannot grant strict implementation readiness. |
| `codectx_context_record` | Validated explicit observations and source-backed scope review. |
| `codectx_context_advance` | Version-checked guarded transition. |
| `codectx_context_capsule` | Bounded capsule page or canonical export metadata. |
| `codectx_context_close` | Explicit lifecycle close and lease release. |

The few added lifecycle/scope/page tools close concrete gaps in the original contract; do not add overlapping per-relation tools or speculative tool namespaces. Expose labels and descriptions compactly so tool schemas do not consume unnecessary client context.

<a id="193-transport-and-safety-rules"></a>
### 19.3 Transport and Safety Rules

Use typed input/output structs and SDK-generated schemas. Request/response structs have explicit lowercase JSON tags, required fields, bounded arrays, enum validation, and no untyped `any` domain payload. Register tools through the pinned SDK and call the same application services as CLI. Enforce input bytes before decoding and output bytes before transport serialization; bound concurrent tool calls independently of parser concurrency.

Only `codectx_read_source` returns source bodies. Search, symbol, graph, context-next, and diagnostics never sneak in complete source. Return metadata previews only where the contract explicitly permits them, with no coverage credit. Stdio stdout is reserved for SDK framing; logs/startup diagnostics go to stderr. Never expose raw SQL, secrets, stack traces, or private absolute roots in tool errors.

Protocol tests use a real SDK client/transport, exercise current discovery/request metadata, tools/list, typed dispatch, limits, cancellation, receipt confirmation and shutdown, and distinguish protocol errors from domain tool errors. No HTTP listener, authentication server, sampling loop, or remote transport is added to V1 merely because the SDK supports it.
The symbol/reference/caller/callee tools expose the same typed semantic-source/profile selector and operation enums as CLI. LSP results are distinctly labeled overlay results with exact input binding, not persisted canonical facts. The registration and parity fixture includes this route whenever a fake or approved local server is configured.


---

<a id="20-configuration"></a>
## 20. Configuration

<a id="201-defaults-and-resource-controls"></a>
### 20.1 Defaults and Resource Controls

Project configuration is optional `.codectx.toml`. User configuration lives in the OS configuration directory; all cache/state defaults to a user-private data directory outside the repository. Merge explicit fields only, reject unknown keys, and validate the complete resolved configuration. There is no unimplemented extension namespace.

```toml
version = 1

[workspace]
follow_symlinks = false
include_untracked = true
index_generated = false
index_vendor = false
max_files = 250000
max_parse_file_bytes = 5242880
max_search_file_bytes = 26214400

[index]
# 0 = choose from available CPUs and memory reservations, never "unlimited".
workers = 0
max_parser_workers = 2
batch_records = 1000
batch_bytes = 4194304
queue_bytes = 16777216
watch_pending_paths = 10000
watch_pending_bytes = 2097152
watch_debounce = "250ms"
reconcile_interval = "30s"
# Results of the last N distinct refs (branches/commits) you indexed stay on disk; A -> B -> C -> A reuses A.
retain_refs = 8
# 0 = no size limit. User-set only; when set, evicts least-recently-used refs first, never the active one.
max_retained_bytes = 0

[resources]
base_memory_budget_bytes = 805306368
query_memory_bytes = 33554432
cache_bytes = 33554432
max_concurrent_queries = 4
max_concurrent_graph_queries = 2
max_concurrent_heavy_analyzers = 1
max_temp_bytes = 4294967296
min_free_disk_bytes = 1073741824
max_metadata_response_bytes = 262144
max_source_response_bytes = 7340032
query_timeout = "10s"
max_query_text_bytes = 8192
max_query_terms = 32
max_page_items = 200
max_provider_record_bytes = 4194304

[storage]
data_dir = ""
busy_timeout = "5s"
read_connections = 2
writer_cache_kib = 8192
reader_cache_kib = 4096
wal_high_water_bytes = 67108864
closed_session_retention = "7d"
query_cursor_ttl = "15m"

[tools]
# Managed analyzer toolchain (Section 11.7). Nothing here is required.
offline = false
cache_dir = ""
mirror = ""
max_fetch_bytes = 2147483648
fetch_timeout = "10m"

[providers.tree_sitter]
enabled = true
languages = ["go", "javascript", "typescript", "tsx", "python", "java", "rust", "c", "cpp"]
worker_idle_ttl = "60s"

[providers.scip]
enabled = "auto"
timeout = "20m"

[providers.lsp]
enabled = "auto"
request_timeout = "15s"
max_servers = 1
max_outstanding_requests = 8
idle_ttl = "60s"

[providers.dependence]
# auto = low-priority background units after the base index activates, queried units first;
# true = block the index on them; false = off.
enabled = "auto"
timeout = "45m"
cache_bytes = 4294967296
unit_memory_floor_bytes = 805306368
# 0 = machine-derived (free memory minus base footprint minus safety margin).
# Only a non-zero user value may reject a unit before it runs.
unit_memory_ceiling_bytes = 0

[context]
default_phase = "sweep"
default_estimated_tokens = 80000
default_max_bytes = 524288
default_max_files = 200
max_slices = 16
max_graph_depth = 3
max_visited_nodes = 50000
max_graph_edges = 100000
max_reason_paths_per_entry = 3
max_manifest_bytes = 8388608
max_capsule_bytes = 8388608
strict_read_gate = true
allow_exploratory_waiver_consolidation = false

[coverage]
chunk_bytes = 65536
max_chunk_bytes = 1048576
session_ttl = "24h"
max_receipts_per_confirmation = 16
max_unconfirmed_chunks_per_session = 64

[mcp]
transport = "stdio"
watch = true
```

Parse/search file thresholds are explicit analysis admission limits, not snapshot retention limits. Report a skipped analysis with file/scope/capability reason; users may raise the limit with sufficient reservations. A resource budget is neither permission to omit required context nor proof that a native process cannot temporarily exceed it. Low-memory operation reduces concurrency and caches before it rejects work. All mandatory and optional capabilities remain implemented.

Validate related values together: source chunk plus worst-case wire encoding must fit the source response budget; batches must fit queue/memory reservations; baseline concurrency must fit aggregate memory; file and result arithmetic must not overflow; disk budgets must leave the free-space reserve. No zero/negative setting means unlimited. Log only effective numeric policy, not credentials or source paths.

<a id="202-trust-and-fingerprints"></a>
### 20.2 Trust and Fingerprints

Trust is the embedded tool lock (Section 11.7). A managed tool is runnable because its payload digest and entry digest match the lock the shipped binary carries; there is no approval step and nothing is looked up on PATH. Project config may select source inclusion, language and safe query preferences, but cannot set any `[tools]` key, authorize executables, arbitrary argv/env, shell interpreters, network access, broader path roots, weakened strict gates, or larger security ceilings. Those controls require explicit user-level configuration/CLI policy. Exploratory waiver consolidation is user-only and never weakens strict read readiness. `enabled="auto"` means run every managed profile whose trigger evidence the snapshot holds.

`[tools.override.<name>]` is the only way to run a tool the lock did not ship: an absolute executable, an exact version and a required SHA-256, verified on every run start. Profile argv, environment allowlists, budgets, work directories, and network posture are product code, not configuration; an override changes the binary, never the invocation. Detection via `--version` or `--help` is still execution and runs only against a lock-verified or override-verified binary. No cloud/AI credential fields exist in core.

Separate fingerprints: source eligibility/byte policy for SnapshotID; analyzer/query/grammar/build inputs, including the tool name, version and payload digest, for UnitID and AnalysisKey; context ranking/budget policy for ManifestID; operational worker counts, logging and UI formatting are not semantic source inputs. Changing a safety limit affects completeness/result metadata when work is limited, but must not silently reuse a complete result produced under a different effective requirement.

---

<a id="21-security-and-privacy"></a>
## 21. Security and Privacy

Threats include malicious repository files/configuration, hostile filenames, symlink races, oversized or malformed analyzer output, process escape, poisoned external indexes, stale evidence, accidental source/log disclosure, and resource exhaustion. This is a local-user tool, not a hostile multi-tenant sandbox. A same-user process that can modify the database and signing keys is outside the cryptographic receipt trust boundary.

| Boundary | Required control |
|---|---|
| Filesystem | Root-confined opens; reject absolute/unmapped paths, `..` escapes, NULs and unsupported file types; handle case sensitivity and Unicode without aliasing distinct valid paths. Use OS-specific safe-opening semantics. |
| Git | One shared argv-based runner; NUL-delimited paths; avoid hooks/filter execution and optional index writes; set safe process environment and treat configured Git helpers as untrusted execution. |
| Analyzer execution | Lock-verified managed tool or digest-verified user override; product-owned argv; no shell interpolation; private snapshot materialization; allowlisted environment; bounded stdin/stdout/stderr, temporary bytes and runtime. |
| Project code execution | Compilers/indexers may load plugins, build macros or project configuration. Explicitly disclose this in status/doctor output, run only lock-verified tools against private materializations, and isolate accordingly; argv alone is not a sandbox. |
| Process trees | Unix process groups and Windows Job Objects, graceful stop then bounded forced termination, wait/drain and cleanup. Prevent Windows child escape during process/job assignment using supported atomic/suspended-launch mechanics. |
| Native parser | Same-binary worker process; bounded input/IPC; validate every result; native crashes cannot kill MCP/CLI serving. |
| Network | The only outbound path is `internal/toolchain` fetching lock-named payloads, build-checked as the sole `net/http` importer and disabled by `tools.offline` or the bundle. Strong analyzer denial uses an OS sandbox/container/namespace or equivalent deployment control; proxy clearing/environment flags are not a security boundary. |
| Provider data | Validate protocol lengths before allocation, nested depth, record fields, canonical IDs, source hashes, range encoding, capability claims, and visible endpoints. |
| API | Typed bounded inputs, session actor checks, signed cursors/receipts, schema validation and cancellation. No source bodies in generic tools. |
| Private storage | User-private directories/files where supported, no writable hard links, no secrets/source in routine logs, quotas and explicit retention. |

The source-position/path/hash/cursor/process utilities are shared concrete implementations, not independently reimplemented by each adapter. A current-source safety check must use the same safe opener as snapshot capture. Diagnostic remediation is generated from typed codes; raw analyzer output is retained only in explicitly requested private debug artifacts with byte/TTL caps, never blindly string-redacted into ordinary logs.

`doctor --offline` reports core policy and whether an actual OS-level analyzer restriction is active. A release test runs index/query/context/MCP with networking disabled and audits connection attempts where supported. Passing a network-disabled test demonstrates that tested workflow; it is not a universal proof about every installed third-party tool. Release artifacts include an SBOM, grammar/native-code license inventory and dependency checksums. [25](#ref-25) [29](#ref-29)

---

<a id="22-reliability-errors-and-observability"></a>
## 22. Reliability, Errors, and Observability

Use `log/slog` with component, repository/snapshot/generation/analysis IDs, provider/unit/run, operation, duration, status and error code. Source bodies, task text, secrets, inherited environment, and raw child stdout/stderr are excluded by default. Sanitize filenames/control characters before human display; JSON escaping is not terminal sanitization.

Run states are consistently lowercase: `running`, `succeeded`, `partial`, `skipped`, `timed_out`, `failed`, `canceled`. Capability states are `fresh`, `partial`, `stale`, `unavailable`, `failed`. Generation health is `fresh`, `degraded`, `failed`. Unknown enum values fail validation; do not maintain competing uppercase wire vocabularies.

Stable error families include:

```text
CTX_WORKSPACE_NOT_FOUND       CTX_CONFIG_INVALID
CTX_TRUST_REQUIRED           CTX_PATH_ESCAPE
CTX_SCHEMA_MISMATCH           CTX_SNAPSHOT_UNSTABLE
CTX_SNAPSHOT_CHANGED          CTX_SOURCE_INTEGRITY
CTX_NO_ACTIVE_GENERATION      CTX_PROVIDER_UNAVAILABLE
CTX_PROVIDER_OUTPUT_INVALID   CTX_PROVIDER_TIMEOUT
CTX_SOURCE_BINDING_UNVERIFIED CTX_QUERY_TRUNCATED
CTX_QUERY_DEADLINE            CTX_RESOURCE_LIMIT
CTX_MINIMUM_BUDGET            CTX_COVERAGE_INCOMPLETE
CTX_SCOPE_INCOMPLETE          CTX_SCOPE_CHANGED
CTX_SESSION_SUPERSEDED        CTX_SESSION_EXPIRED
CTX_ACTOR_MISMATCH            CTX_CURSOR_INVALID
CTX_VERSION_CONFLICT          CTX_WORKSPACE_BUSY
CTX_DISK_FULL                 CTX_STORAGE_CORRUPT
CTX_TOOL_OFFLINE              CTX_TOOL_UNSUPPORTED_PLATFORM
CTX_TOOL_FETCH_FAILED         CTX_TOOL_DIGEST_MISMATCH
CTX_TOOL_CORRUPT              CTX_TOOL_OVERRIDE_INVALID
```

Errors have a typed code, safe message, bounded structured details, retryability and remediation. Cancellation is not logged as an unexpected crash. Retry only transient operations with a fixed attempt/deadline budget; never retry a malformed input, bad source hash or denied trust decision as if it were a temporary network fault.

`doctor` checks build/toolchain pins, workspace/data-directory permissions, free space, SQLite schema/FTS/WAL, active pointer, recent capture/freshness, sample CAS blocks, bundled grammar availability, managed toolchain status per lock entry, orphan temporary state and session/lease retention. `--deep` performs expensive integrity and parser smoke checks explicitly. No full database scan occurs on every version/search command.

Status exposes current and peak parent RSS, aggregate base-worker RSS, Go-managed memory, native-worker memory, query/cache/queue reservations, live subprocesses, pending events, DB/WAL/temp/CAS bytes, unit reuse and parse counts, capability limits, and per-stage timing. Record unavailable metrics as unavailable, not zero. Peak process-tree memory is sampled concurrently, not calculated by adding unrelated per-process historical peaks. There is no remote metrics or telemetry endpoint in V1.

---

<a id="23-performance-and-scalability-targets"></a>
## 23. Performance and Scalability Targets

<a id="231-measurement-contract-and-capability-preservation"></a>
### 23.1 Measurement Contract and Capability Preservation

These are **proposed engineering acceptance budgets**, not observed results. The implementer must measure them on a reproducible fixture and report misses honestly. “Blazing fast” is not an executable acceptance criterion; the tables below are. No performance target authorizes deleting a capability, removing evidence, shrinking the indexed corpus, omitting a grammar, lowering required context coverage, or changing output semantics.

Reference workload: 1 million eligible source lines, 10,000 files, approximately 80 MiB source, all required languages represented, fixed generated source/graph seeds, representative manifests/tests/configuration/docs and a high-fanout case. Reference machine: 8 logical cores, 8 GiB RAM, local SSD, pinned OS/toolchain/build. Publish exact hardware, filesystem, cache state, grammar/provider versions, configuration, line/file/byte/node/edge counts and fixture hashes. Test real repositories as an additional signal, not a moving substitute for the fixed fixture.

Base means parent process **plus all mandatory parser workers**, not only Go heap. Report managed SCIP/LSP/dependence engines individually and as a concurrently sampled full process-tree total when enabled. Optional analyzer memory is not hidden, but it is not dishonestly promised to fit the base parser budget.

<a id="232-initial-release-budgets"></a>
### 23.2 Initial Release Budgets

| Workload or resource | Target on the reference fixture |
|---|---:|
| `version --json` / help, no DB or analyzer startup | p95 at most 50 ms process wall time |
| Exact symbol/path query, warm store, end-to-end CLI | p95 at most 50 ms |
| FTS lexical search, warm store, bounded page | p95 at most 150 ms |
| One-hop caller/callee query, ordinary fanout | p95 at most 100 ms |
| Context plan with up to 50,000 visited nodes | p95 at most 1 s |
| Ten changed medium files, base refresh | p95 at most 1.5 s after debounce, excluding a scheduled full reconciliation |
| No-change base refresh | No parser work and no FTS body rewrite; p95 at most 250 ms for ordinary metadata reconciliation |
| Cold base index, full reference fixture | At most 3 min |
| Idle MCP process after caches settle and parser workers expire | RSS at most 128 MiB |
| Interactive base process-tree peak | At most 256 MiB for the specified query concurrency |
| Base indexing process-tree peak | At most 768 MiB, including native parser workers |
| Source chunk, default | 64 KiB raw; explicit 1-MiB raw configurable ceiling |
| Base storage, one active snapshot | At most 3.5 times eligible source bytes; report graph-heavy fixture misses separately |
| Retained unchanged generations | No duplicate source blobs, graph facts or FTS bodies; only bounded membership/audit metadata growth |

The no-change fast path does not replace scheduled content verification. Measure full reconciliation cost separately and expose its staleness interval. High-fanout or adversarial requests have correctness/bounded-resource targets rather than an unconditional sub-100-ms promise. Cold and warm runs, concurrent indexing/query load, low-memory mode and long-lived watch/MCP soak are all reported.

<a id="233-memory-admission-and-allocation-discipline"></a>
### 23.3 Memory Admission and Allocation Discipline

Go's memory limit is a soft runtime control and does not cover arbitrary native allocations or subprocess memory. It also must not be confused with RSS. Use it only as one component after accounting for baseline runtime, native parser workers, buffers and process limits; do not blindly set an aggressive global limit that causes GC thrashing. [30](#ref-30) [31](#ref-31)

Concrete defaults are in Section 20. Reserve bytes before reading/decoding a file, allocating provider batches, starting parser work, storing graph frontiers, serializing responses or materializing analyzer input. The scheduler uses the smaller of effective CPU capacity and memory-admissible worker count, with a default two-parser-worker ceiling. Leave headroom for transient allocations and RSS variation. Degrade concurrency before coverage.

The aggregate resource report must include queue payloads, retained slice capacities, duplicated strings/JSON, native trees, pending LSP responses, SQLite connection caches, vocabulary/query spools, source buffers and child IPC. Caches have byte and entry limits with one owner; oversized buffers are not returned to a pool that retains them forever. Avoid `sync.Pool` as a correctness or memory-bound mechanism. Copy only at explicit ownership boundaries, release source/AST references after extraction, and stream large outputs to bounded destinations. No forced GC after every file or unmeasured allocator trick is accepted as a speed improvement.

Hard OS memory enforcement is platform-dependent. Use supported cgroup/job/process controls where available, report when only admission/monitoring is available, and stop a worker approaching its budget without admitting incomplete output. An OOM-killed optional provider yields explicit degradation, not application-wide silent data loss. A 2-GiB test environment must still run the full base feature set with fewer workers, subject to documented slower throughput and the same semantic results.

<a id="234-required-hot-path-design"></a>
### 23.4 Required Hot-Path Design

| Hot path | Required design |
|---|---|
| Snapshot | Streaming capture and sorted on-disk manifest; no repository-sized slices. |
| Incremental index | Immutable unit reuse; only changed/dependent units parsed and normalized. |
| Parser | Lazy bounded worker pool; explicit native closure; no all-repository AST cache. |
| Provider import | Length bounds before allocation; disk-backed mappings for global identifiers. |
| Storage | Prepared bounded batches, short transactions, byte-limited connection pools, no full integrity scan on query startup. |
| Search | Indexed exact lookup, generation-scoped ranking, bounded top-K/spool, no global corpus clone. |
| Graph | Indexed batched adjacency, bounded compact frontier/visited state, no N+1 hydration. |
| Context | Metadata-only planning, bounded reasons, ordinal slice references, no source bundle in heap. |
| Source | Indexed line checkpoints plus per-block integrity; no full-file rehash for each chunk. |
| Watch | Coalesced changes, bounded pending set, full-reconciliation fallback, no overlapping refreshes. |
| MCP | Bounded concurrent requests/serialization; compact schemas; no eager optional server startup. |

<a id="235-regression-and-release-policy"></a>
### 23.5 Regression and Release Policy

Benchmark CPU time, allocations, bytes allocated, peak heap, parent/native/process-tree RSS, disk read/write and temporary bytes, startup time, p50/p95/p99 latency, throughput, reuse/parse counts, and peak DB/WAL/CAS sizes. Use uninstrumented release builds for latency/RSS; race/coverage builds have separate correctness purposes. Profile before and after changing architecture for performance.

A change exceeding the absolute budget fails the release gate. A measured regression above 10% relative to a stable baseline requires investigation, repeated measurements and an explicit explanation; noise tolerance is calibrated on the runner, not used to excuse arbitrary degradation. A requested budget change needs reviewed evidence and user approval where it weakens this requirement. Optimizations must pass the same capability/fact/query/context/coverage fingerprint checks as the reference implementation. No target may be “met” by excluding the failing feature or rewriting the benchmark after seeing results.

---

<a id="24-packaging-and-deployment"></a>
## 24. Packaging and Deployment

Supported base targets remain Linux amd64/arm64, macOS amd64/arm64, Windows amd64/arm64, and WSL through the Linux build. Every claimed target has a native or compatible tested toolchain and actual parser smoke run. Do not advertise Windows arm64 while omitting it from the release task.

```text
codectx_<version>_linux_amd64.tar.gz
codectx_<version>_linux_arm64.tar.gz
codectx_<version>_darwin_amd64.tar.gz
codectx_<version>_darwin_arm64.tar.gz
codectx_<version>_windows_amd64.zip
codectx_<version>_windows_arm64.zip
codectx-bundle_<version>_<os>_<arch>.tar.gz   (slim binary plus pre-populated tool store, one per target)
tools-v<n>/<tool>-<version>-<os>-<arch>.tar.gz  (only gopls and the four npm bundles; every other tool is fetched from its upstream release, digest-pinned)
checksums.txt
sbom.spdx.json
THIRD_PARTY_LICENSES.md
```

Ship the binary, bundled pinned grammars, LICENSE/NOTICE, documented default config, shell completions, checksums and SBOM. The private Tree-sitter worker is a mode of that same binary, not another installed service. Official Tree-sitter bindings use native code, so release builds must not claim `CGO_ENABLED=0` portability; SQLite's CGo-free driver does not remove Tree-sitter's native requirements. Build with `-trimpath`, pinned toolchain and deterministic version metadata; a wall-clock build timestamp must not defeat reproducibility. [4](#ref-4) [13](#ref-13)

Profiles remain local base (bundled grammars), local precise (managed SCIP/LSP), and local deep (managed dependence engine). The slim archive fetches lock payloads on first use; the bundle archive carries the store pre-populated for its platform and never touches the network. Every redistributed tool and runtime has a recorded license and version in the lock and in `THIRD_PARTY_LICENSES.md` (Section 11.7). A future team server is outside V1. No supported profile requires a paid service or AI API key.

The data directory is workspace/installation-specific. Rebuild and pruning commands make destructive consequences explicit. An incompatible schema is not automatically migrated, and an existing user's session artifacts are not silently deleted by a new binary. Version mismatches fail clearly; greenfield development changes are in place, not a reason to add a compatibility system.
Release SBOM generation uses the pinned selected SPDX or CycloneDX tooling, with native grammar assets included. [19](#ref-19) [20](#ref-20)


---

<a id="25-testing-strategy"></a>
## 25. Testing Strategy

<a id="251-minimum-critical-test-set"></a>
### 25.1 Minimum Critical Test Set

Tests exist to protect critical product behavior, not to maximize file count, mock count or coverage percentages. Extend a relevant existing fixture/table/property test before creating another suite. Use ordinary Go testing; add a dependency only for a demonstrated need. Avoid tests for trivial getters, enum spelling repeated at every layer, private implementation details, boilerplate mocks, and identical golden outputs through multiple adapters.

| Critical family | Minimum evidence and why it is needed |
|---|---|
| Source identity and safety | One table/property fixture covering stable hashes, changing files, Git/worktree transformations, path escape, source ranges/encoding, CAS corruption and full retained reads. Prevents reading/writing the wrong bytes. |
| Storage and incremental publication | One integration fixture covering reusable units, changed dependency invalidation, optional partial-output failure, atomic activation, FKs, query/session leases and GC races. Prevents stale/mixed facts or lost source. |
| Provider boundary | Shared conformance fixture plus small native-format cases for each required grammar/SCIP/LSP/dependence profile. Covers malformed lengths, cancellation, source binding and process cleanup. Prevents false facts, OOM and hangs. |
| Search, graph and context | One deterministic fixture covering exact/lexical order, unrelated-generation ranking isolation, ambiguity, cycles/limits, required full files, scope gaps and budget/slicing boundaries. Prevents incomplete context presented as complete. |
| Actor coverage and workflow | One state-machine fixture covering delivery receipts, duplicates, wrong actor/hash, empty/long/invalid-UTF8 files, scope changes, waiver behavior and guarded transitions. Prevents false full-read readiness. |
| Product boundary | One end-to-end flow exercised by CLI and real MCP transport, sharing expected domain data. Covers offline/no-source-write behavior, JSON/protocol channels, version conflicts and cleanup. Prevents disconnected or incorrectly wired features. |
| Performance/resource matrix | One reusable benchmark/soak harness across fixture sizes and process modes, with capability fingerprint comparison. Prevents leaks, regressions and capability reduction disguised as optimization. |

The relevant task adds only missing cases in these families. A new test names the failure mode it protects. Critical parser/framing/path/range fuzzing can share a few focused targets; do not fuzz every parser wrapper separately without a distinct risk. A real pinned external-provider smoke is required before its profile is called supported; normal CI uses offline fixtures and does not download tools during tests.

<a id="252-execution-discipline"></a>
### 25.2 Execution Discipline

For a bug, reproduce it first in the smallest existing critical test when possible; verify the test fails for the intended reason, fix production code, and rerun it plus affected integration checks. For new critical behavior, a focused failing contract test is appropriate. Do not manufacture missing-package compilation failures for every tiny scaffolding step or generate unrelated tests just to satisfy a TDD ritual.

Run `go test ./...`, `go vet ./...`, formatting checks and a supported `go test -race ./...` release gate. Race instrumentation does not detect all native memory bugs; native parser lifecycle and subprocess resource checks remain necessary. Unsupported race targets use a documented supported host plus target-native functional checks, not a falsely reported pass.

The same tests can satisfy multiple verification-matrix rows. Full source reading and integration review remain required even when all tests pass. Do not remove a meaningful critical test merely to reduce test count; consolidate overlapping coverage while retaining the critical assertion.

---

<a id="26-acceptance-criteria"></a>
## 26. Acceptance Criteria

The release is acceptable only when demonstrated evidence supports every item:

- Base indexing succeeds offline without optional analyzers and includes all required languages, files, symbols, scopes, imports/exports, structural calls/references, tests, manifests, dependencies, configuration, documents and source ranges.
- SCIP enrichment preserves exact-source provenance and symbol identity; LSP offers all advertised supported live queries as a labeled snapshot-qualified overlay; the dependence provider contributes only export-supported deep facts.
- Every retained source remains readable after worktree edits and Git history changes; unsafe paths, wrong hashes, invalid coordinates and unverified imports cannot become trusted facts.
- Unit reuse avoids reprocessing unchanged source and correctly invalidates changed dependencies, including new/deleted files and provider/config changes. Failed units do not leak and generations publish atomically.
- All queries remain pinned, paginated, explainable and deterministic; unrelated staging/retained generations do not change canonical lexical scores. Hard bounds and incomplete capability are always visible.
- Verify context includes complete relevant files and boundary tracing. Oversized scope produces coherent full-file slices or a clear budget error, never silently missing required context.
- Each actor confirms its own source receipts and current-scope review. Waivers, another actor's coverage, a sweep-only plan, unresolved/truncated required scope, or stale current-source validation cannot grant strict implementation readiness.
- CLI and MCP call the same typed services, preserve error/channel semantics, and exercise the current supported protocol rather than an obsolete handshake assumption.
- Cross-platform release artifacts, SBOM/licenses, exact dependency pins, offline behavior, safe process termination and resource cleanup are demonstrated.
- Section 23 targets are measured on the declared fixture and hardware, including aggregate native/base RSS and capability parity. Any miss remains a reported release blocker unless explicitly approved with evidence; a plan cannot declare its own benchmark result.
- Section 5.1 is applied to the lead and every subagent; shared code is reused, obsolete consumers are removed, and no compatibility/migration scaffolding or speculative infrastructure is introduced.

This is a falsifiable definition of acceptance, not a claim that static analysis or finite testing can prove the absence of every possible application bug.

---

<a id="27-risks-and-mitigations"></a>
## 27. Risks and Mitigations

| Risk | Consequence | Concrete mitigation / gate |
|---|---|---|
| Snapshot-bound IDs force every fact to change | Slow refresh and duplicate storage | Stable logical keys qualified by generation, immutable unit membership, reuse counters and no-change benchmark. |
| Reused unit misses an indirect dependency | Stale caller/type/impact facts | Full input/dependency fingerprint, reverse invalidation, add/delete/rename fixture. |
| Weak canonical merging | False calls or lost overloads | Scoped strong aliases, exact ranges/signatures, ambiguous candidates retained with provenance. |
| Partial provider output becomes visible | Corrupt or overstated capability | Seal/admit independent complete units only; failed unit invisible; explicit per-scope completeness. |
| Shared FTS corpus changes pinned ranking | Nondeterministic context | Generation-scoped statistics and unrelated-generation activation/pruning regression. |
| Native parser allocates outside Go heap or crashes | OOM or MCP crash | Small parser subprocess pool, native closure, aggregate RSS and callback-path soak. |
| A single protobuf/CSV/XML record is huge | OOM despite top-level streaming | Length/depth/field caps before allocation and on-disk mappings. |
| Current worktree is confused with Git blobs or LSP state | Wrong source with apparently precise evidence | Actual-byte CAS capture, private materialization, exact hash/encoding checks. |
| Source chunks rehash or rescan entire files | Quadratic read cost | Block verification and sparse line checkpoints. |
| Token estimator is treated as a hard bound | Oversized client context | Explicit heuristic label plus exact wire-byte budgets and minimum-budget errors. |
| Issued output counted as delivered | False full-read gate | Signed receipt echo, transactional interval merge and broken-pipe scenario. |
| Parent/subagent reuse one session | One actor edits unread code | Actor-specific sessions and receipts, current-scope review and orchestrator check. |
| A waiver or incomplete graph grants readiness | User's full-read rule bypassed | Strict gate never accepts required-file waivers or missing/truncated required scope. |
| Watch notifications are incomplete | Stale source | Recursive directory management and periodic content reconciliation, not Git hints alone. |
| Separate processes index concurrently | Lost publication or excessive memory | Workspace indexing lock and finite contention policy. |
| Long reads pin WAL / GC races with new leases | Disk growth or missing source | Short transactions, WAL backpressure, coordinated leases/GC grace checks. |
| Analyzer runs hooks or follows unsafe config | Repository writes or disclosure | Lock-verified tool, product-owned argv, private inputs, process/OS controls and explicit limitations. |
| Upstream tool asset removed, renamed or changed | Managed install fails typed (`CTX_TOOL_FETCH_FAILED`); never runs unexpected bytes | Lock digest and size checked before extraction, so a changed asset fails closed; a removed asset is repaired by a lock bump in the next release or served from `tools.mirror`; only gopls and the npm bundles are hosted by codectx. |
| Lock argv drifts from the real tool | Profile silently produces nothing | Task 22 CI matrix runs every lock entry per release on each platform; unverified means failing. |
| Fetch path becomes a general network capability | Hidden outbound traffic | Single `net/http` importer enforced at build; only lock URLs; `tools.offline` and bundle. |
| Schema mismatch destroys session evidence | Lost audit/source | Fail closed, explicit new-cache rebuild; no automatic migration or deletion. |
| Tests become another large framework | Slow development and maintenance | Few shared critical suites, table cases and one resource harness; remove redundant tests. |
| Performance claimed without execution | False assurance | Published reproducible measurements; mark targets unverified until run. |
| Restricted dependency/competitor code copied | Licensing exposure | Complete native/module license inventory and clean-room contribution policy. |

---

<a id="28-repository-structure"></a>
## 28. Repository Structure

This is a responsibility map, not an instruction to create empty directories, one-file interfaces, or dozens of unused abstractions. Inspect the actual repository first; reuse existing owners and split a file only when a coherent responsibility or measured need justifies it. Keep related implementation, focused tests and fixtures together. Query/service records belong to `internal/model`; no undefined `internal/query` dependency is introduced.

```text
cmd/codectx/main.go                 process boundary, signals, private worker entry
internal/app/                      concrete composition and typed shared services
internal/model/                    IDs, facts, requests, pages, workflow records
internal/config/                   strict layered config, trust and fingerprints
internal/workspace/                discovery, safe paths, ignore-aware streaming walk
internal/process/                  one runner, Unix groups, Windows jobs, IPC limits
internal/source/                   hash/range/encoding/line-index utilities
internal/snapshot/                 capture, CAS, manifest, private materialization
internal/vcs/git/                  Git argv and NUL-delimited result interpretation
internal/storage/sqlite/           open.go, schema.go, schema.sql, units, query, state
internal/provider/                 provider contracts, registry and bounded sink
internal/provider/filesystem/      file/docs/search chunk extraction
internal/provider/manifest/        static supported manifests/configuration
internal/provider/treesitter/      worker lifecycle, extraction, language registrations
internal/provider/treesitter/languages/  pinned grammar and query packs
internal/provider/scip/            wire import, aliases, source binding, profiles
internal/provider/lsp/             bounded client, manager, snapshot-qualified overlay
internal/provider/dependence/      frontend-native units, graph cache, streaming CSV import and normalization
internal/provider/dependence/joern/  pinned engine argv, stderr parsing, label map
internal/toolchain/                embedded tool lock, verified fetch, atomic store, resolver, runtime launchers
internal/tools/toollock/           release-time lock and payload generator
internal/reconcile/                deterministic canonical resolution and lineage
internal/index/                    unit planning, scheduling, invalidation, watch/status
internal/pagination/               shared cursor signing, leases and bounded spools
internal/search/                   exact/lexical retrieval and shared lexical scoring
internal/graph/                    adjacency traversal, paths, impact and rollups
internal/context/                  seeds, scope, rank, budget, manifests and capsules
internal/coverage/                 reads, receipt confirmation, intervals and status
internal/workflow/                 state/version guards, observations and waivers
internal/cli/                      command parsing and human/JSON rendering
internal/mcpserver/                official SDK registration and thin handlers
internal/diagnostics/              typed health/resource/doctor reporting
internal/retention/                coordinated unreachable-object collection
internal/e2e/                      shared critical workflow fixture
internal/bench/                    one fixture generator and resource benchmark harness
internal/tools/                    only thin Go automation actually needed by CI and release

testdata/                          small polyglot, SCIP, LSP and dependence-export fixtures
docs/                              architecture, providers, MCP, operations,
                                   configuration, performance, threat model
.github/workflows/                 CI, offline and native-target release jobs
.codectx.toml.example
go.mod / go.sum
LICENSE / NOTICE / THIRD_PARTY_LICENSES.md
README.md / SECURITY.md / CONTRIBUTING.md
```

There is no `migrations/`, legacy provider path, duplicate graph/search cursor implementation, per-adapter byte converter, generic `utils` dumping ground, or speculative extension loader. The shared utilities listed above solve identified cross-cutting behavior, not hypothetical future consumers. Business logic remains in its owner; avoid turning `internal/app` into a second implementation of every service.

---

<a id="29-build-versus-adopt-decisions"></a>
## 29. Build-versus-Adopt Decisions

| Concern | Decision | Reason and boundary |
|---|---|---|
| MCP | Official Go SDK | Use pinned current protocol/schema support, not a custom transport or compatibility facade. |
| Syntax parsing | Official Tree-sitter Go binding and grammars | Mature syntax engine; own extraction queries and bounded worker lifecycle. |
| Precise interchange | Official SCIP schema/bindings and protobuf primitives | Stream validated records, do not invent a competing symbol protocol. |
| Live semantics | Small bounded LSP client/manager | Only required read-only methods and profiles, not an editor or generic command executor. |
| Deep analysis | Managed Joern behind the neutral `dependence` provider | Integrate built-in exports; no copied CPG engine or product Scala code; engine swappable without a product-surface change. |
| Analyzer distribution | Product-owned tool lock with re-hosted payloads and a stdlib fetch/verify/extract path | No package manager, npm, coursier or `go install` at runtime; runtimes (Node, JDK) are lock entries too. |
| Database/FTS | SQLite through `modernc.org/sqlite`, FTS5 | Embedded indexed store; own generation membership/scoped ranking where required. |
| Watch | `fsnotify` with reconciliation | Notifications are hints; avoid a bespoke filesystem watcher. |
| CLI/config | Cobra and pinned TOML parser | Own strict policy validation, not a global dynamic configuration framework. |
| Source/CAS/graph/context/coverage | Small Go domain implementations | These exact snapshot/evidence/actor semantics are the product differentiator. |
| Hashing/serialization/process basics | Standard library first | Shared domain validation around mature primitives. |
| License/SBOM/build automation | Adopt pinned existing Go tooling where adequate; thin policy glue only | Do not build a custom license detector, package manager or release framework. Include native grammar files and bundled assets, not only module manifests. |
| Embedding/vector services | No core dependency | All required retrieval/context functions work deterministically and locally. |
| Redis/Kafka/Postgres/Neo4j/distributed platform | Not included | No V1 need; unnecessary operational and memory overhead. |
| CKB/Code Knowledge Backend | Do not adopt or copy implementation | Its source-available license imposes commercial-use conditions; retain the clean-room decision. This is not a judgment about its feature quality. |

Do not add a generic plugin or retriever abstraction merely to leave a theoretical extension point. Existing provider interfaces are justified by concrete Tree-sitter/SCIP/dependence implementations, and the LSP query boundary has actual consumers. Recheck precise dependency and asset licenses when pinning them. OSI/copyleft/custom restrictions require explicit review rather than an invented assertion that all transitive components are permissive. [2](#ref-2) [4](#ref-4) [6](#ref-6) [13](#ref-13) [21](#ref-21)
The original graph-ranked repository-map precedent is retained as background only; no third-party implementation is copied. [18](#ref-18)


---

<a id="30-implementation-plan"></a>
## 30. Implementation Plan

<a id="301-execution-model-read-gate-and-dependency-dag"></a>
### 30.1 Execution Model, Read Gate, and Dependency DAG

The sections above are normative and travel with this plan. Tasks are coherent reviewable changes, not permission to skip prerequisites or defer a V1 capability. Create only files actually required by the current implementation. Before making any task decision, the lead and each subagent must satisfy the exact Section 5.1 policy.

**Entry gate for every task:** identify the real repository revision; read the entire relevant files (in consecutive bounded segments if necessary); trace callers/consumers, contracts/types, state/lifecycle, dependencies and integration boundaries; inspect existing shared implementations; choose the smallest clear implementation that preserves scope; record the planned resource ownership and the critical behavior at risk. If those files were not supplied or cannot be read, say so rather than claiming this gate passed. Grep/ripgrep is discovery only. This document review is not a substitute for reading the future implementation repository.

**Dispatch contract:** include the verbatim Section 5.1 paragraph, exact task/owner scope, revision, relevant files, dependency producers/consumers, existing helpers, memory/latency budgets, and required evidence. Each worker independently reads its relevant files. The lead must inspect the complete changed implementation and real consumer wiring, not accept a worker summary as proof. Parallel tasks have disjoint file ownership; a single integration owner updates shared app composition, go.mod/go.sum and shared schemas/contracts.

**Completion gate:** inspect the full diff and affected whole files; rerun relevant critical tests and measured checks; verify no dead callers, registrations, config keys, obsolete files, unused production dependencies or accidental parallel implementations remain; update documentation/TOC/source citations and commands/contracts together; record actual evidence. Fix discovered in-scope gaps immediately, in place. Do not add compatibility/migration work or speculative infrastructure. Do not add tests that duplicate a critical assertion already exercised by the shared fixture. Before committing, use `git diff --check` and review explicitly staged files rather than indiscriminately staging unrelated changes.

```text
1 -> 2 -> 3 (includes shared process/source helpers) -> 4 -> 5 -> 6
6 -> 7 and 8                            base providers
6 -> 9, 10 and 11                       optional adapter development
7 + 8 -> 12                            usable base coordinator
9 + 10 + 11 -> 22                       managed toolchain rewrites the three profile boundaries; runs parallel to 12
12 + 22 -> app composition              integration owner wires toolchain.Resolver into provider composition
5 + 12 -> 13 and 14                     independent search and graph
13 + 14 -> 15 -> 16 -> 17               context, coverage, workflow + facade
17 -> 18 and 19                        independent CLI / MCP adapters
12 -> 23                                early cutoff on declaration-only digests, one language at a time behind a proof gate
1–19, 22, 23 -> 20 -> 21               integrated hardening / measured release (21 runs the real-tool matrix)
```

Runtime provider dependency order is still enforced when source tasks are developed in parallel. Task 5 consumes neutral model metadata, not future provider implementations. Task 4 already has the Task 3 runner. Graph accepts resolved IDs and does not depend on Search. Both product adapters consume the facade delivered by Task 17. These constraints remove the original task-order/import-cycle ambiguity.

<a id="302-milestones-and-shared-verification-assets"></a>
### 30.2 Milestones and Shared Verification Assets

| Milestone | Tasks | Demonstrable result |
|---|---|---|
| M0 Foundation | 1–6 | Pinned Go project, safe process/source boundaries, CAS, current SQLite schema and provider/unit runtime. |
| M1 Base intelligence | 7–8, 12–14 | Offline full-language base index, genuinely incremental refresh, search/graph/impact. |
| M2 Precise/deep capability | 9–11, 22 | Managed six-indexer SCIP, full supported LSP query overlay and version-verified dependence provider, every profile proven against its lock payload in CI. |
| M3 Context/workflow | 15–17 | Deterministic complete scope, bounded slices, actor receipts/review, strict gates and typed facade. |
| M4 Product interfaces | 18–20 | CLI/MCP parity, diagnostic/retention/resource integration. |
| M5 Release | 21 | Measured performance/capability parity, offline and six-target slim and bundle artifacts. |

Introduce the shared fixture and resource harness when first needed, not only at Task 21. Task 8 creates `TestParserResourcePlateau`; Task 12 extends the harness with `TestIncrementalReuse`; Task 21 implements `TestResourceBudgets` and published benchmarks. These are named implementation deliverables, not tests claimed to exist in the supplied attachment. Package-focused checks run per task; full suite/race/protocol/resource checks run at integration milestones and final release. Exact external tool version and digest pins are a Task 22 lock deliverable and its CI matrix, not an invented pin in this specification.

<a id="task-1-bootstrap-the-go-module-cli-shell-and-dependency-policy"></a>
### Task 1: Bootstrap the Go Module, CLI Shell, and Dependency Policy

**Deliverable:** A compilable pinned Go project, cheap version/help command, stable JSON envelope and a lean license/build policy.

**Files and ownership:** Create or update `go.mod`, `go.sum`, `cmd/codectx/main.go`, `internal/model/build.go`, `internal/cli/root.go`, `internal/cli/output.go`, `.github/workflows/ci.yml`, `.gitignore`, `LICENSE`, `NOTICE`, `README.md`, and `THIRD_PARTY_LICENSES.md`. Do not precreate unused services or tool directories.

**Dependencies / consumes:** Sections 5–7, 18, 24 and 29; no prior project-local service.

**Produces / contract:** `model.BuildInfo{Version, Commit, Toolchain, SchemaVersion string}`, typed `Envelope[T]`, `cli.NewRoot(build model.BuildInfo, stdout, stderr io.Writer) *cobra.Command`, and the signal/error process boundary.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One CLI version-envelope case proves exact channels/fields and operation without a workspace. Reuse it later rather than adding separate equivalent golden files. Verify formatting/license policy through tools, not redundant Go tests.
- [ ] **Step 2: Implement the complete production path.** Use `module github.com/Sawmonabo/codectx`, `go 1.27`, and `toolchain go1.27.1`; pin CI to that toolchain and disable runtime toolchain auto-download behavior in shipped operation. Add Cobra when consumed and pin resolved versions/checksums. Version/help must not open SQLite, detect analyzers, spawn parser workers or import an eagerly initialized service graph. Output errors through the single envelope policy. Use standard error propagation; only main chooses exit codes.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Adopt existing pinned Go-native dependency/license/SBOM tooling where it covers the requirement, with only small policy glue if needed. Inventory generated grammar/native assets when they arrive, not just module license labels. CI starts with formatting, `go vet`, tests/build and `go mod verify`. A review exception must be explicit and evidence-backed, not an unknown-license pass.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/cli/... -count=1
go vet ./...
go build -trimpath -o ./bin/codectx ./cmd/codectx
./bin/codectx version --json
go mod verify
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-2-implement-canonical-model-types-and-deterministic-identities"></a>
### Task 2: Implement Canonical Model Types and Deterministic Identities

**Deliverable:** One neutral, complete set of source/fact/query/context/workflow contracts with generation-qualified identity and no provider/storage import cycle.

**Files and ownership:** `internal/model/` for build/IDs/source/facts/provider metadata/query/context/workflow records. Keep cohesive types together; do not split every enum into a file.

**Dependencies / consumes:** Task 1 build/envelope policy; all domain contracts in Sections 9–19.

**Produces / contract:** All named model records in this document, validators, canonical length-framed hashing, and generic `Page[T]{Meta QueryMeta, Items []T}`. `Result` records embed QueryMeta where appropriate; source/session results carry their binding explicitly.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend one model/source contract fixture for delimiter-safe IDs, directional relations, distinct same-edge occurrences, scope-bound local symbols and canonical JSON. Include failure for mixed/partial ranges and unknown wire enums only where public validation depends on it; do not test each trivial field accessor.
- [ ] **Step 2: Implement the complete production path.** Define IDs and neutral provider descriptors before storage consumes them. Implement source coordinates, FileVersion/Snapshot, node/relation/evidence candidates and facts, NativeAlias, SearchUnit, Binding/QueryMeta, budgets/entries/slices/manifests, receipt and actor requests, observation/review/waiver/capsule records. Give every public field an explicit JSON tag; Go sketches showing grouped fields do not authorize default capitalized wire keys. Validate maximum lengths, signed-SQL integer bounds, enum values, required XOR references and half-open ranges at boundaries. Semantic hashes exclude run/session randomness and timestamps; API binding retains them as required.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Define typed endpoint-specific requests/results used by the facade: IndexRequest/IndexResult, IndexStatus, OverviewRequest/OverviewItem, SymbolRequest/Node, ReferenceRequest/ReferenceOccurrence, GraphResult, PathRequest/PathResult, ImpactEntry, SessionRequest/SessionStatus, ContextPageRequest, IncludeRequest, NextContextItem, ObservationRequest/Observation, AdvanceRequest/WorkflowStatus, WaiverRequest, CapsulePage and DoctorRequest/DoctorReport. Each consists of the relevant binding, fields already specified in Sections 14–19, page metadata and bounded domain records; no undefined `query` package or `any` domain result is allowed. Document source-less external entities and ambiguity.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/model -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-3-implement-configuration-workspace-discovery-path-safety-and-the-shared-process-runner"></a>
### Task 3: Implement Configuration, Workspace Discovery, Path Safety, and the Shared Process Runner

**Deliverable:** Strict trusted configuration, bounded ignore-aware traversal, shared source helpers and one safe process runner ready before Git capture.

**Files and ownership:** `internal/config/`, `internal/workspace/`, `internal/source/`, `internal/process/runner.go` and platform-specific process files; `.codectx.toml.example` and `docs/configuration.md`.

**Dependencies / consumes:** Task 2 neutral types and source conventions. This task owns the shared runner needed by Task 4; Task 6 must not create a second runner.

**Produces / contract:** `config.Load(root string) (Config,error)`, separate source/analysis/policy fingerprints, `workspace.Discover(start string) (Root,error)`, `workspace.Walk(ctx, root, policy, visit func(File) error) error`, root-confined file access, shared source-position conversion and `process.Runner.Run(ctx, Spec) (Result,error)`.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Use the shared safety fixture for unknown config keys, project-config executable escalation, root escape, a symlink race, cancellation and child/grandchild termination. A small argument-echo helper verifies literal argv without creating another test framework.
- [ ] **Step 2: Implement the complete production path.** Load defaults then user then permitted project fields, merge explicit values, and validate cross-field budgets. Implement streaming deterministic discovery using disk-backed ordering when needed; no `[]File` repository collection. Preserve tracked Git paths regardless of ignore matches, reject unsupported encodings explicitly, handle case-preserving paths and platform aliases safely, and exclude the actual data directory. Runner Spec contains an absolute executable, argv, private directory, allowlisted environment, bounded stdin/stdout/stderr streams, timeout/grace and memory/disk reservation. No shell interpolation. A single Unix process-group / Windows Job Object implementation handles all children, including Git and native workers.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Add cancellation-safe pipe draining, output-limit termination, wait cleanup, temp cleanup and safe typed diagnostics. Create shared byte-range/UTF encoding/line-checkpoint helpers used by providers and serving. Trust version/help execution just like any executable. Do not claim environment clearing prevents network access. Document regular-filesystem capture limitations and active OS controls.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/config ./internal/workspace ./internal/source ./internal/process -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-4-build-exact-gitworktree-snapshots-and-the-local-content-addressed-store"></a>
### Task 4: Build Exact Git/Worktree Snapshots and the Local Content-Addressed Store

**Deliverable:** Immutable captured clean/dirty/untracked/deleted/non-Git source with streamed manifests, bounded integrity metadata and safe private materialization.

**Files and ownership:** `internal/vcs/git/`, `internal/snapshot/` and source/CAS cases in the shared fixture. Reuse Task 3 process/path/encoding helpers.

**Dependencies / consumes:** Tasks 2–3; the shared runner and root-confined opener already exist.

**Produces / contract:** `snapshot.Builder.Build(ctx) (model.Snapshot,error)`, an implementation of `model.SnapshotView`, `CAS.Put/Open/ReadRange`, and `Materialize(ctx, view, selection) (Materialization,error)` where Materialization exposes `Root() string` and idempotent `Close() error`.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One retained-source fixture checks dirty bytes after edits, clean Git CRLF/filter differences, deleted/untracked/non-Git cases, unstable capture, deduplication, wrong/corrupt blocks and retained reads after Git history removal. Check an empty file and one very long line using the same fixture.
- [ ] **Step 2: Implement the complete production path.** Use NUL-delimited Git plumbing through the shared runner. Stream actual worktree bytes into CAS with whole/block SHA-256 and sparse line checkpoints, sync and atomically publish; Git OIDs remain provenance. Stage/sort/hash manifests on disk and derive stable FileIDs before source snapshot hashing without circular identity. Reconcile capture membership and bound retries. Snapshot.Open streams authenticated blocks; ReadRange validates only bounded accessed blocks and metadata. Do not rehash the entire file per chunk. The index/GC coordination lock protects in-progress captures until their durable snapshot references exist.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Private copies/clones must not share writable hard links with originals/CAS. Bound materialization bytes and cleanup on all exits. Explicit repair must verify the recorded whole hash before replacing a missing blob. Document sparse checkout/submodule/LFS behavior, binary/oversized retention versus analysis coverage, and the distinction between validated capture and operator-frozen capture. Capture may temporarily use private staging files before Task 5 imports the immutable metadata; no second durable catalog is introduced.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/snapshot ./internal/vcs/git ./internal/source -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-5-implement-the-current-sqlite-schema-reusable-units-leases-and-atomic-activation"></a>
### Task 5: Implement the Current SQLite Schema, Reusable Units, Leases, and Atomic Activation

**Deliverable:** One initialized schema, reusable immutable unit storage, pinned readers, FTS ownership, actor/workflow persistence and crash-safe publication, without migrations.

**Files and ownership:** `internal/storage/sqlite/open.go`, `schema.go`, `schema.sql`, `units.go`, `query.go`, `state.go`; `internal/pagination/` for the one signed cursor/lease/spool utility. Extend existing focused storage tests.

**Dependencies / consumes:** Tasks 2–4 neutral model and source catalog. Storage imports no provider implementation.

**Produces / contract:** `Store.BeginGeneration`, `BeginUnit`, `SealUnit`, `AttachUnit`, `Activate`, `Abort`, `PinGeneration`, bounded query methods and transactional state methods. `PinnedReader` exposes Binding, bounded reads and `Close() error`. Cursor signing and lease APIs are shared by search/graph/context, with receipt signing reusing the same cryptographic framing primitives.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One storage integration scenario proves staging invisibility, unchanged unit reuse across two snapshots, changed-input rejection, failed activation rollback, retained original provider provenance, cross-snapshot FK rejection, FTS-aware deletion and a lease/GC race. Exercise schema initialization/mismatch, not migration history.
- [ ] **Step 2: Implement the complete production path.** Pin the Section 12 driver/libc versions, verify the embedded SQLite WAL-reset fix/version, and implement the complete Section 12 DDL, per-connection pragmas, bounded read/write pools, prepared byte-limited batches and strict typed JSON columns. Store source manifest rows instead of a whole JSON manifest. Facts join via generation_units; orphan identities never imply visible facts. Validate whole-unit evidence/aliases/endpoints/source inputs before seal. Preserve provider run provenance across generation deletion. Configure durable session/receipt writes, short read transactions and scheduled passive checkpoint/backpressure. Store/schema fingerprint mismatch fails closed; rebuild is explicit and creates a separate cache.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Implement signed bounded cursors, keyset/spool storage and TTL leases before parallel query tasks. Do not maintain FTS only through cascades or row counts: explicit index/content updates share a transaction. Pin acquisition and GC publication use one coordination protocol. Schema SQL and Go constraints must agree on partial-null ranges, current manifest binding and invalid state changes. Update storage/operations docs and prove every public read excludes unsealed units.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/storage/... ./internal/pagination -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-6-implement-provider-contracts-registry-byte-bounded-sink-and-deterministic-resolution"></a>
### Task 6: Implement Provider Contracts, Registry, Byte-Bounded Sink, and Deterministic Resolution

**Deliverable:** A complete provider runtime with explicit dependencies, source-bound unit reuse, bounded ownership transfer and deterministic canonical reconciliation.

**Files and ownership:** `internal/provider/provider.go`, `registry.go`, `sink.go`, `internal/reconcile/`; reuse `internal/process/` rather than adding a provider-specific process package.

**Dependencies / consumes:** Task 5 unit writer and pinned lookup; Tasks 2–4 neutral types, source view and runner.

**Produces / contract:** The Section 11 interfaces, registry detection/selection with trust, shared provider conformance helper, deterministic Resolver and byte-accounted sink.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend the shared provider fixture with duplicate IDs/DAG cycles, a blocked sink, byte-cap overflow before count-cap, cancellation and ambiguous symbol resolution. Verify a source-equivalent unit produces identical identities regardless of scheduling order.
- [ ] **Step 2: Implement the complete production path.** Registry validates duplicate/missing dependencies and cycles, returns stable ordering, and separates unavailable/disabled from failed enabled providers. Resolution uses persisted unit-scoped aliases and only completed dependencies. Sink owns bounded batches until persisted; reserve bytes before decoding/queuing, flush at the smaller record/byte limit, and cancel all producers when a required write fails. Single records cannot bypass limits. The unit is sealed only after validation; failed partial output is not attached to a generation.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** The same semantic ownership holds for node attributes, relations and search documents. Record source binding, consumed dependency keys and origin runs. Do not add a universal plugin framework or a second process abstraction. Maintain a narrow deterministic contract for provider implementations that can be developed separately but merged through a single composition owner.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/provider ./internal/reconcile ./internal/process ./internal/storage/... -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-7-index-files-documentation-build-metadata-and-package-manifests"></a>
### Task 7: Index Files, Documentation, Build Metadata, and Package Manifests

**Deliverable:** All baseline file/document/package/configuration/build-target and dependency facts, plus bounded reusable lexical documents.

**Files and ownership:** `internal/provider/filesystem/`, `internal/provider/manifest/`, shared polyglot manifest/doc fixtures and provider documentation.

**Dependencies / consumes:** Task 6 provider/sink/resolver; Task 4 snapshots. Runtime manifest units depend on filesystem facts.

**Produces / contract:** Filesystem/docs and manifest providers with the complete Section 11.2 format behavior and source/evidence precision.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend the polyglot fixture to assert Go/npm/Cargo/Python/Maven dependency semantics, workspace inheritance, unresolved dynamic properties, malformed input handling and long-line chunks. Use a single canonical fact comparison rather than one golden per parser method.
- [ ] **Step 2: Implement the complete production path.** Emit repository/ancestor/file identities with exact unit-owned attributes; derived counts come from the selected generation, not stale mutable global node records. Parse explicit static manifests without invoking project code. Preserve docs/config/build formats and searchable unknown manifests. Build source chunks once per file unit, capped at 32 KiB with bounded overlap; symbol metadata is indexed separately without copying full function bodies. Large/binary admission limits affect declared analysis coverage, not CAS retention.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Ensure all fact kinds have evidence and ranges when present, error states distinguish malformed format from provider process failure, and deduplication retains distinct dependency kinds. Implement actual capability reporting and register both providers through the shared composition owner. Reuse existing range/path/TOML/JSON helpers, update native/module licenses only when dependencies change.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/provider/filesystem ./internal/provider/manifest -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-8-implement-the-tree-sitter-structural-provider"></a>
### Task 8: Implement the Tree-sitter Structural Provider

**Deliverable:** Full required-language structural intelligence with lazy isolated parser workers and bounded native resources.

**Files and ownership:** `internal/provider/treesitter/`, language registrations/query packs, private worker entry in `cmd/codectx/main.go`, and the shared structural/resource fixture.

**Dependencies / consumes:** Task 6 contracts/runner; Task 7 filesystem runtime dependency. Source development may proceed alongside Task 7 after contracts are stable.

**Produces / contract:** Base tree-sitter provider; all nine language/grammar registrations; definition/scope/import/export/reference/call/test/comment extraction and syntax precision.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One parameterized language fixture covers the critical extraction contract for each required grammar, with nested declarations and a non-ASCII range. The shared parser soak covers the selected binding callback path, canceled parse, worker crash and native memory plateau; avoid 1,000 duplicate unit tests.
- [ ] **Step 2: Implement the complete production path.** Pin grammar and binding versions with a reviewed ABI/query match. Fully read the selected binding lifecycle/cancellation implementation and grammar query assumptions before choosing methods. Launch at most the admitted parser workers through the shared runner, stream bounded source, validate child framing/facts, close every owned native resource and release capture buffers. Keep source/native handles inside a worker. File-local unit reuse performs no parse; cross-file target resolution declares its dependencies or stays explicitly unresolved.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Enforce aggregate parent-plus-worker accounting and reset an unhealthy worker with one bounded retry. A required extraction failure blocks publication while preserving the prior generation. Do not mark missing/unsupported grammar coverage fresh, drop language packs to meet memory targets, or add an unbounded AST cache. Document native worker behavior and license inventory.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/provider/treesitter ./internal/process -count=1
go test ./internal/bench -run TestParserResourcePlateau -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-9-implement-streaming-scip-import-and-explicit-local-indexer-profiles"></a>
### Task 9: Implement Streaming SCIP Import and Explicit Local Indexer Profiles

**Deliverable:** Generic precise-index import and managed indexer execution with validated exact-source binding and bounded memory.

**Files and ownership:** `internal/provider/scip/`, small SCIP fixtures and provider docs; only the composition owner changes shared registry wiring.

**Dependencies / consumes:** Task 6 provider/runner/resolver; Task 4 materialization; runtime filesystem/Tree-sitter dependencies from Tasks 7–8.

**Produces / contract:** SCIP provider, source-binding validation, scoped native aliases, typed indexer profiles (tool resolution moves to Task 22) and explicit import CLI/service request fields.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend one small canonical protobuf fixture with a forward/external reference, repeated same-edge occurrences, document-local symbols, UTF-8/16/32 ranges and source-hash mismatch. A generated streamed input/resource case proves memory is bounded even for a large document field.
- [ ] **Step 2: Implement the complete production path.** Implement the per-document delta import of Section 11.4 (document hash, delete-then-insert by path, set-based deletion of absent paths, project-root rejection, index-level `external_symbols`). Walk top-level and nested wire fields incrementally; generated protobuf records must be size-limited before decode. Use an on-disk identifier mapping/two passes for unresolved ordering; do not retain a whole index or symbol graph. Map definitions/references/implementations and supported relationships with exact evidence. Duplicate relation IDs do not erase separate occurrence ranges. Missing encoding or source proof remains explicit rather than guessed.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Profiles execute managed tools against private snapshots with input-hash manifests. Supplied unverified indexes remain importable for discovery/quarantine but cannot silently enter strict compiler evidence. Validate executable trust, output path, output length, profile scope and version. Attach only completed validated units; preserve unrelated base capability on failure. Document exact profile commands after real-tool verification.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/provider/scip ./internal/reconcile -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-10-implement-the-lsp-snapshot-qualified-working-tree-overlay"></a>
### Task 10: Implement the LSP Snapshot-Qualified Working-Tree Overlay

**Deliverable:** The full optional read-only live semantic query set with bounded local servers and no mutation of canonical generations.

**Files and ownership:** `internal/provider/lsp/` client/framing/manager/profile/normalization code and one fake-server protocol fixture.

**Dependencies / consumes:** Task 6 runner and canonical lookup; Task 4 materialized pinned bytes; shared source encoding from Task 3.

**Produces / contract:** `lsp.Manager.Open(ctx, view, profile) (Overlay,error)` and typed Definition, References, Implementations, TypeDefinition, DocumentSymbols, WorkspaceSymbols, CallHierarchy and Close operations, all with bounded pages/results.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One fake-server lifecycle scenario exercises initialization, out-of-order replies, negotiated UTF positions, cancellation, hostile URI, unsupported method/server request and shutdown; add cases to that fixture rather than mock every method separately.
- [ ] **Step 2: Implement the complete production path.** Implement Content-Length framing with length/depth/outstanding-request bounds and context deadlines, then the required LSP initialize/initialized and shutdown/exit exchange. Start managed profiles lazily against private snapshot materializations; synchronize owned documents and validate all returned coordinates. Keep results labeled with source/dependency hashes and server/version. Reject writes, arbitrary commands and external URI reads. LSP is not the MCP protocol and does not use MCP discovery.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Bound one default server, overlay bytes, outstanding calls and idle TTL. Expose all advertised supported methods through the typed query boundary while keeping ephemeral enrichment separate from deterministic canonical plans. Profile detection does not auto-authorize execution. On server failure, release pending calls/processes/temp input and report unavailable/failed overlay without erasing base facts. No unsaved-editor claim or future-persistence placeholder remains.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/provider/lsp ./internal/source ./internal/process -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-11-implement-the-dependence-provider-over-the-managed-graph-engine"></a>
### Task 11: Implement the Dependence Provider over the Managed Graph Engine

**Deliverable:** A real version-pinned parse/export/import path providing evidence-supported control-dependence, data-dependence and fallback call facts per frontend-native unit, extraction-lazy and cached, with independent resource/failure and partial-analysis reporting, and no engine name on any product surface.

**Files and ownership:** `internal/provider/dependence/` (provider, units, cache, CSV import, emit) with the engine adapter in `internal/provider/dependence/joern/`, small Neo4j CSV fixtures, `docs/providers-dependence.md`.

**Dependencies / consumes:** Task 6 runner/resolver; Task 4 private materialization; completed structural runtime dependencies from Tasks 7–8; Task 22 `toolchain.Resolver` for the engine and its JDK.

**Produces / contract:** `dependence` Provider with `Descriptor().ID == "dependence"`, `ProviderVersion` = adapter version plus engine payload digest, `Detection.ObservedVersion` = engine version and payload digest; unit scope `pkg:<frontend-native project key>` per language (workspace scope only for C/C++); `Detection.Capabilities` naming `control_depends_on`, `data_flows_to`, `reads`, `writes`, `calls`; `partial` state with `skipped_methods` count; `failed: memory` with observed peak bytes; evidence details `cdg`, `reaching_def`, `reaching_def capture`, `assignment`, `call`.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Install the pinned engine release on the development machine, run every argv below against the polyglot fixture in each of the nine languages, and record observed version, payload SHA-256 and raw output before deciding anything. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Use one export fixture with calls/control/data dependence, unknown labels, forward IDs, an external method carrying a snapshot filename, malformed/oversized records and mismatched source paths. One process-fault case proves a failed export does not admit partial invalid facts. One stderr case proves a skipped-method warning yields `partial` with the count rather than a sealed complete unit.
- [ ] **Step 2: Implement the complete production path.** Resolve engine and JDK through the toolchain resolver; always pass `--language <frontend>` (`c`, `golang`, `pythonsrc`, `javasrc`, `jssrc`, `rust`) and `--max-num-def 40000`; one unit per frontend-native project (walk every `go.mod` yourself because the Go frontend ignores `go.work`; a Cargo workspace root is one unit; a `tsconfig`/`package.json` project is never split, because splitting loses more than half of resolved calls; a Python unit is a package), C/C++ whole-repository with include paths; subdivision only after a reproducible engine crash confirmed by one rerun with the frontend's (currently empty) semantics-neutral option allowlist, never for memory, published per capability as `partial: subdivided` with failed unit, backend failure and affected capabilities; single `--repr=all --format=neo4jcsv` export, no GraphML or DOT path; cache the parsed graph under the data directory keyed on the full semantic closure of Section 11.6 with retention; size the frontend heap cap from unit bytes up to the machine-derived allocation (free memory minus base footprint minus safety margin, bounded only by an explicit user limit), retry once on out-of-memory only when more memory is actually available, fail closed otherwise; classify out-of-memory, deterministic pass crash, and zero-exit helper crash (near-empty graph or `Process exited with code` on stderr) as distinct typed failures; derive `reads`/`writes` from assignment operators with resolved references; capture stderr through the runner's bounded buffer and parse the skip warnings; cap CSV fields before decoder allocation, stream results, and use a disk mapping for native IDs; apply the export as a keyed delta against the stored unit (Section 11.6), normalizing Go `<clinit>` rows, and write only changed facts. Normalize only supported labels; keep external methods that carry a snapshot location and alias them by full name; publish data dependence through captures and globals with its own detail rather than the intraprocedural label.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** `providers.dependence.enabled = auto|true|false` with `auto` default and the `pending` capability contract of Section 11.6; count unknown labels and missing capabilities; fail unsupported units rather than guess compatibility. No product-owned analysis script or interpreter server. Account the entire analyzer tree memory/temp/output as scheduler input (serialize heavy units by default; co-schedule only when the summed reservations fit), close processes, delete private inputs/exports and leave the base generation intact on optional failure. Record full process-tree metrics separately from base metrics. Docs name the engine only in `docs/providers-dependence.md` and `THIRD_PARTY_LICENSES.md`.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/provider/dependence/... ./internal/process -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, engine name on a product surface, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-12-implement-the-index-coordinator-incremental-invalidation-watch-mode-and-atomic-refresh"></a>
### Task 12: Implement the Index Coordinator, Incremental Invalidation, Watch Mode, and Atomic Refresh

**Deliverable:** A complete base cold/refresh/watch workflow with safe optional-provider attachment, unit reuse and per-scope freshness.

**Files and ownership:** `internal/index/` coordinator/scheduler/invalidation/watch/status and concrete provider composition under `internal/app/`.

**Dependencies / consumes:** Tasks 4–8 are sufficient for base completion. Tasks 9–11 integrate via the same registry as they finish, and Task 22's `toolchain.Resolver` is what the composition hands them; absence does not block a usable base milestone.

**Produces / contract:** `Coordinator.Index`, Refresh, Watch and Status; explicit reuse/invalidation/capture/publication metrics and one cross-process workspace owner.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One incremental integration scenario covers no-op reuse, ten changed files, a new symbol/dependency, delete/rename, optional partial-output failure, required failure, a concurrent refresh request and watcher overflow. Assert unchanged units and lexical bodies were not rewritten.
- [ ] **Step 2: Implement the complete production path.** Acquire the workspace lock and reservations, capture a snapshot, compute unit/dependency keys, attach verified reusable units, carry the previous sealed unit of any refreshing semantic scope into the new generation as `stale` with its provenance distance (Section 13.3), retain by distinct ref per Section 12.4 (`retain_refs`, user-set `max_retained_bytes`), execute admitted invalid units and seal/publish only after complete validation. Keep temporary graph work on disk. File-level syntax refresh must not trigger repository-wide base reparse. Full semantic scopes are legitimate only when declared and required by the provider. Events arriving mid-run form the next bounded set.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Manage directory watches recursively within policy and newly created directories; report unavailable watcher coverage and fall back to periodic filesystem/Git reconciliation. Preserve source/hash truth over notification hints. Optional provider failure yields explicit requested-capability degradation; disabled absence does not falsely mark the base broken. Cancellation releases locks/reservations and retains the prior generation. Update status and operations docs.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/index ./internal/storage/... -count=1
go test ./internal/bench -run TestIncrementalReuse -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-13-implement-exact-symbol-path-and-generation-scoped-fts5-search"></a>
### Task 13: Implement Exact, Symbol, Path, and Generation-Scoped FTS5 Search

**Deliverable:** Paginated exact/lexical discovery whose canonical ranking is stable while other generations are built or pruned.

**Files and ownership:** `internal/search/` exact/lexical/scoring code; reuse `internal/pagination/` and bounded storage query methods.

**Dependencies / consumes:** Task 5 store/pagination and Task 12 active generations. Task 14 may run in parallel and does not import this search implementation.

**Produces / contract:** `Search(ctx, model.SearchRequest) (model.Page[model.SearchHit],error)` and paginated `Resolve(ctx, model.SymbolRequest) (model.Page[model.Node],error)`; SearchHit has Node/File reference, ScoreMicros and bounded reasons.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One search fixture proves exact-before-lexical ranking, prefix/Unicode/literal punctuation handling, occurrence deduplication, tampered/stale cursors and unchanged scores after unrelated staging/activation/GC. Compare canonical domain data rather than CLI/MCP duplicate goldens.
- [ ] **Step 2: Implement the complete production path.** Use indexed normalized exact lookup and FTS candidate matching. Derive generation-local BM25 document/term/phrase statistics using visible units and the actual FTS tokenizer; no raw shared-corpus ranking. Stream aggregates/candidates with bounded heap or disk spool, quantize scores consistently and apply complete stable tie-breaks. Keep source text out of generic result payloads. Do not use an unbounded token dictionary or scan all source into memory.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Query plans and representative high-frequency terms must be measured; batching/caching is keyed by AnalysisKey and byte-limited. Use shared signed keyset/spool cursors and leases, enforce deadlines/work/page limits, close rows on cancellation and report when a hard bound prevents completeness. Verify supported-platform near-tie determinism before claiming cross-build canonical parity.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/search ./internal/storage/... ./internal/pagination -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-14-implement-bounded-graph-traversal-dependency-paths-and-impact-analysis"></a>
### Task 14: Implement Bounded Graph Traversal, Dependency Paths, and Impact Analysis

**Deliverable:** Incoming/outgoing graph, reference/implementation/test/config/doc queries, bounded shortest paths and explained impact with efficient adjacency access.

**Files and ownership:** `internal/graph/` traversal/path/impact/rollup and bounded storage adjacency methods; reuse the shared pagination/signing code.

**Dependencies / consumes:** Task 5 pinned store and Task 12 generations; inputs are resolved canonical NodeIDs. Application composition may use search resolution, but graph implementation does not depend on Task 13.

**Produces / contract:** Typed Neighbors, Callers, Callees, ShortestPath, Impact, References and PackageDependencies operations using model Graph/Path/Page contracts.

- [x] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [x] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend one cyclic/high-fanout fixture for incoming/outgoing direction, multiple seeds, equal-cost paths, max depth/visited/edges, partial capability and continuation. Every returned impact/path must have an evidence-backed reason; truncated closure must remain explicit.
- [x] **Step 2: Implement the complete production path.** Implement BFS and nonnegative integer-cost Dijkstra with stable frontier ordering. Read adjacency for a bounded batch of frontier IDs, using indexed from/to columns and unit membership; no N+1 query/hydration loop. Keep compact visited state under reservation or use the existing bounded spool. Deduplicate canonical relations while preserving occurrence/evidence retrieval. Compute package rollups from selected units and distinct pairs.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Explain caller-versus-callee impact and dependency-read versus change intent. Cursor state retains visited/frontier position server-side when necessary; tokens remain small, signed, expiring and generation-bound. Hard work limits cannot reset with each page to create an unbounded operation. Reuse exact same graph methods in context compilation and public tools.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/graph ./internal/storage/... ./internal/pagination -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-15-implement-the-deterministic-context-compiler-required-scope-budgeting-and-slicing"></a>
### Task 15: Implement the Deterministic Context Compiler, Required Scope, Budgeting, and Slicing

**Deliverable:** Complete persisted task context with explicit boundary requirements, deterministic ranking, bounded explanations and full-file slices without hidden omissions.

**Files and ownership:** `internal/context/` seeds/scope/rank/budget/manifest/slice and existing store manifest methods; extend one shared context fixture.

**Dependencies / consumes:** Tasks 13–14 query services, Task 4 source metadata and Task 5 manifest persistence.

**Produces / contract:** `Compiler.Compile(ctx, model.ContextRequest) (model.ContextManifest,error)`, bounded manifest entry/slice/exclusion pages and the byte-length token heuristic.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** One deterministic context fixture proves required source/test/contract/caller selection, an unresolved boundary, scoped FTS stability, exact byte/slice floors, an overlarge single file and a multi-slice complete scope. Recompile after reopen and compare canonical hashes, not volatile session metadata.
- [ ] **Step 2: Implement the complete production path.** Extract seeds exactly in Section 15 order and record all unresolved ambiguity. Build bounded required scope over the graph and caller/state/dependency/integration boundaries; no score-based silent removal of a required consumer. Use integer graph scoring and bounded path reasons. Deduplicate selected files globally, count actual serialized overhead, estimate tokens from metadata without loading source, and keep the estimator explicitly heuristic.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Persist immutable headers/normalized entries plus ordinal slice references, canonical request/analysis/policy hash and incomplete-scope diagnostics. A required single file too large returns its minimum budget, not snippets labeled full. Inclusion of newly discovered files creates a new manifest for workflow use. Generic plan output is metadata/paged content, not an unbounded source bundle. Update scope/budget docs and shared type consumers together.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/context ./internal/search ./internal/graph ./internal/storage/... -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-16-implement-snapshot-pinned-source-serving-and-actor-scoped-receipt-coverage"></a>
### Task 16: Implement Snapshot-Pinned Source Serving and Actor-Scoped Receipt Coverage

**Deliverable:** Lossless bounded source reads with efficient integrity checks, signed delivery confirmation and per-actor full-file coverage.

**Files and ownership:** `internal/coverage/` source/read/receipt/range/status code; reuse source helpers, shared signing and store session/issued-chunk/range methods.

**Dependencies / consumes:** Task 15 manifests, Task 4 CAS/range reads and Task 5 transactional session/lease storage.

**Produces / contract:** OpenSession, Read, Acknowledge, Status, Next and Close operations with Section 16 types; per-actor read completeness distinct from strict readiness.

- [x] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [x] **Step 1: Protect the critical behavior with the minimum existing test extension.** One receipt/range fixture covers sequential/out-of-order/duplicate reads, output failure before echo, another actor, wrong hash/session, partial UTF-8/CRLF, a giant line, invalid UTF-8 base64 and empty-file confirmation. Use a bounded byte-set oracle for interval merging rather than separate redundant examples.
- [x] **Step 2: Implement the complete production path.** Open a new actor session unless an exact same-actor idempotency retry is proven. Read only pinned membership, validate offsets and verify accessed CAS blocks, calculate lines via checkpoints and return bounded exact bytes/encoding. Persist an issued chunk and sign its receipt; do not merge served coverage until a matching receipt is echoed. Confirm prior receipts on a subsequent read when supplied, with the same transaction/validation as explicit acknowledgment. Cap unconfirmed chunks and token batches.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Merge confirmed intervals transactionally; full means exact byte union, with explicit EOF confirmation for empty files. A file-review acknowledgment remains a separate client assertion. Expired sessions/cursors cannot resurrect deleted source; session close/lease retention are wired. Source response serialization includes worst-case encoding bounds. Next returns only metadata and never bypasses the read endpoint.
- [x] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/coverage ./internal/source ./internal/storage/... -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-17-implement-workflow-guards-scope-review-deterministic-capsules-and-shared-service-contracts"></a>
### Task 17: Implement Workflow Guards, Scope Review, Deterministic Capsules, and Shared Service Contracts

**Deliverable:** Transactional actor workflow with scope expansion, non-bypassing waivers, historical audit and a typed facade ready for parallel CLI/MCP wiring.

**Files and ownership:** `internal/workflow/`, context observation/capsule code and `internal/app/services.go`; update model/store contracts already defined rather than introducing another query model.

**Dependencies / consumes:** Tasks 15–16 manifests/coverage, Task 12 freshness and Task 5 state persistence. This task creates the facade before Tasks 18 and 19.

**Produces / contract:** Advance/Include/Record/Waive/Close with version guards; capsule builder; concrete app composition and narrow typed consumer interfaces for all Section 18/19 operations.

- [x] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [x] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend the single workflow fixture for direct verify, scope-version review invalidation, conflicting transitions, unserved files, a required waiver that never grants strict readiness, blocking unresolved boundaries, wrong-actor mutations, supersession and idempotent capsule retrieval.
- [x] **Step 2: Implement the complete production path.** Implement state/current-manifest/history changes atomically. Source-backed scope review covers every category in Section 17 and requires confirmed same-actor full files. Include adds discovered pinned scope and invalidates old review. Readiness requires complete scope, required reads, no bypass and current-source validation; completed historical reads alone are not write permission. Validate observation IDs/reference membership and use semantic hashes for duplicate idempotency.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Build capsules from stored explicit observations/evidence/coverage/waivers only and preserve all timestamps as audit metadata outside the canonical hash. Add per-request or immediately-before-write source revalidation for strict readiness and expose the guarantee limits. Define typed app methods for overview, symbol/reference/graph, context pages/next/include/close, diagnostics and export as well as the original operations. Wire the explicit canonical/LSP semantic-source selector and symbol/reference operation enums so the full LSP query manager has actual CLI/MCP consumers. Interface declarations depend only on model contracts; app must not import CLI/MCP, preventing cycles. *(Left unchecked deliberately: capsules, per-request revalidation, the exposed guarantee limit, the semantic-source selector and the operation enums all landed, but `Overview` and `Doctor` have no producer in this repository and stand as typed purpose-named refusals ledgered for Task 20, and `Close` still does not release the session lease -- `pagination.Leases` offers no lookup from a session to the lease `OpenSession` minted, so releasing it needs the Task 20 retention work. See T17-INT-report.md.)*
- [x] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/workflow ./internal/context ./internal/coverage ./internal/app -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-18-implement-the-human-readable-and-machine-stable-cli"></a>
### Task 18: Implement the Human-Readable and Machine-Stable CLI

**Deliverable:** Every listed CLI command wired to the shared services, with bounded typed output, complete lifecycle and exact error/channel behavior.

**Files and ownership:** `internal/cli/` command/rendering code and `cmd/codectx/main.go`; reuse Task 17 facade and model.

**Dependencies / consumes:** Task 17 complete service contracts; all corresponding service implementations. May run in parallel with Task 19 with separate file ownership.

**Produces / contract:** The full Section 18 command/options/exit contract, including actor identity, receipt confirmation, scope include, page/export and close.

- [x] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [x] **Step 1: Protect the critical behavior with the minimum existing test extension.** Use one table-driven adapter test to assert representative command-to-service translation, mandatory flags, bounds and JSON error channels, then the shared E2E flow for real behavior. Do not repeat service algorithm tests behind CLI mocks. *(Two cases in the existing `internal/cli/root_test.go` -- `init` refusing to overwrite project configuration without `--force`, and a broken stdout pipe failing the command without confirming a receipt. Both are top-level funcs rather than `TestCommandEnvelope` rows because that table asserts exactly one envelope on a buffer, which a broken pipe makes impossible by construction. The broken-pipe row pins the exit class only: its write end is not fd 1, so `signal.Ignore(SIGPIPE)` is not exercised there. The shared E2E flow was the wave VERIFY lane's row and has now run against the real store -- wave-f-verify-T18.md: golden `--json` stability over 28 runs of every read command, every documented exit code driven for real except 10 (including `status --json | head -c 0` -> 7, the 141 -> 7 signal path), the managed-LSP route against real gopls v0.23.0, the metadata bound and the database-free `version`/`--help`.)*
- [x] **Step 2: Implement the complete production path.** Parse/validate flags and bounded observation input, call services and render results. Add all pagination/generation/timeout controls to applicable commands. Keep source only in the explicit read/export path; export output is an explicit destination, not permission to overwrite source. Enforce mutually exclusive receipt/file acknowledgment and JSON/file observation modes. Match minimum-budget, trust, conflict, partial-result and provider errors to typed exit codes. *(Every Section 18.1 spelling except `repo-map` and `doctor`, which are not registered: `ExploreService.Overview` and `DiagnoseService.Doctor` are typed refusals whose producers Task 20 owns, so a registered command would always refuse. Ledgered here by name rather than shipped as a placeholder -- see Task 20. Two more entries on the same ledger. `init --json` reports the absolute repository path it wrote to, the same Section 5 exemption `status --json`'s tool-store path already carries: both name the operator's own location, which is the fact the field exists to report. And `index --scip-index <typo>` currently exits 0 with completeness rows byte-identical to a run with no flag, so a mistyped path is indistinguishable from supplying none -- honest absence at the provider, but nothing records the supplied index path in the generation, which is Task 20's item alongside its own "record the supplied-index path" entry.)*
- [x] **Step 3: Wire consumers, failure handling and documentation.** Wire signal context, one-envelope JSON errors, stderr logs, broken-pipe handling and stable human omission indicators. Init is explicit and safe; rebuild creates an explicit separate cache, not migration. Help/version remain independent of database/provider startup. Update examples to confirm each final source receipt and provide a scope review before claiming readiness. *(Signal cancellation, `signal.Ignore(SIGPIPE)` so a broken stdout pipe reaches the typed EPIPE path and exits 7 instead of being killed with 141, one-envelope JSON errors, stderr logs, `init --force` and `index --rebuild`'s separate cache all landed. Examples confirming each final source receipt and the pre-readiness scope review were exercised by the wave VERIFY lane against the real store (wave-f-verify-T18.md): `context read` issued a chunk with its receipt, `context status` reported `fully_served_files: 0` for receipts that were never confirmed, `context advance <sid> consolidate` refused with `CTX_COVERAGE_INCOMPLETE "0 of 17 required files"` (exit 6) and `--expected-version 99` with `CTX_VERSION_CONFLICT` (exit 8) -- the scope review before readiness is the gate, not a claim in this document. One row remains unproven and is scheduled by the controller rather than claimed here: rendering a SEALED capsule through L5's rewritten `context.go` renderers needs a fully-read session, which that lane's budget could not reach; the exit-2 "session has no capsule" refusal did exercise the new `runService` path.)*
- [x] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/cli/... ./internal/app -count=1
go build -trimpath -o ./bin/codectx ./cmd/codectx
./bin/codectx version --json
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-19-implement-the-mcp-server-and-typed-current-protocol-tool-contracts"></a>
### Task 19: Implement the MCP Server and Typed Current-Protocol Tool Contracts

**Deliverable:** The entire compact MCP tool surface over official SDK stdio, exercising protocol 2026-07-28 and shared services.

**Files and ownership:** `internal/mcpserver/` registration/handlers/errors, the MCP serve entrypoint, and one schema/protocol fixture.

**Dependencies / consumes:** Task 17 typed facade, not Task 18 implementation. Pin official SDK v1.7.0 and use its supported test transport/client.

**Produces / contract:** Every tool in Section 19 with concrete schemas, typed results, resource/actor gates, source isolation and protocol-correct errors.

- [x] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [x] **Step 1: Protect the critical behavior with the minimum existing test extension.** One actual SDK transport scenario checks current discovery/request metadata, tools/list and representative typed calls, receipt echo, wrong actor/source denial, oversized input, canceled call and clean shutdown. One schema snapshot checks names/required fields; do not write a mock-only handler test for every trivial forwarding line. *(The in-package scenario table and snapshot cover typed dispatch, receipt echo, wrong-actor denial, oversized input and cancellation; `server/discover`, the negotiated protocol version and clean shutdown over real stdio are the wave VERIFY lane's rows.)*
- [x] **Step 2: Implement the complete production path.** Register through the SDK using concrete Go structs and explicit descriptions/tags. Use the current protocol rather than an old initialize-handshake assertion. Let SDK code handle framing and its own compatibility behavior; add no legacy facade. Separate app sessions from MCP wire state. Tool inputs, outstanding calls and serialization buffers obey global byte/concurrency reservations.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Only read_source returns source content; context_next is metadata and context_entries/capsule are bounded pages. Shared app methods perform receipt/scope/version validation. Route all logs to stderr, do not print startup text to stdout, and convert typed errors without raw paths/SQL/secrets. Verify CLI/MCP canonical result parity through the shared fixture once both adapters land. *(Left unchecked deliberately: everything but the last clause landed. CLI/MCP canonical result parity needs the shared `internal/e2e` fixture and the CLI adapter Task 18 is still building, so it cannot be verified from this task. `codectx_repo_overview` additionally surfaces `Overview`'s typed refusal as a tool error rather than an empty page -- its producer is Task 20's, the same ledger Task 17 Step 3 carries. See T19-INT-report.md.)*
- [x] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/mcpserver ./internal/app -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-20-complete-diagnostics-retention-resource-accounting-and-integrated-fault-isolation"></a>
### Task 20: Complete Diagnostics, Retention, Resource Accounting, and Integrated Fault Isolation

**Deliverable:** Actionable resource/health reports, coordinated cleanup and verified integrated safety, not a late retrofit of deferred critical protections.

**Files and ownership:** `internal/diagnostics/`, `internal/retention/`, existing process/store/source boundaries, doctor adapter and `docs/threat-model.md`/`docs/operations.md`.

**Dependencies / consumes:** Tasks 3–19 already implement their own safety/resource controls. This task integrates and validates them across process and persistence boundaries.

**Produces / contract:** Typed Doctor report including the toolchain status of every lock entry, live/peak resource report, retention collector (which also sweeps tool-store staging and versions the lock no longer names) and recovery procedure with explicit unsupported-metric/platform limitations.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Extend the existing E2E/storage/process fault fixture for disk full, corrupt active pointer/blob, long reader/WAL pressure, canceled writes, child process tree, stale lease/GC and structured diagnostic privacy. Add only uncovered critical cases, not another parallel adversarial suite.
- [ ] **Step 2: Implement the complete production path.** Implement metadata-only diagnostics, optional expensive deep checks and typed safe remediation. Track parent/base-worker/optional/full-tree memory distinctly with simultaneous sampling. Count queues, cached capacity, native resources, temporaries and DB/WAL. GC acquires the indexing/GC coordination lock so capture and collection cannot race; recheck leases/reachability before trash/final deletion. Prune finite closed-session audit data in dependency order. Recover abandoned staging and unreferenced temp artifacts without deleting retained source.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Ordinary logs use known typed fields, not raw analyzer text passed through guess-based redaction. Report whether real OS network/memory controls are active. Verify no provider can escape source/materialization boundaries, no platform silently skips process-tree cleanup, and no metric is zero merely because unsupported. Profile actual hot paths and reuse existing helper implementations for fixes discovered here.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/diagnostics ./internal/retention ./internal/process ./internal/storage/... -count=1
go test ./internal/e2e -count=1
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.


<a id="task-21-complete-end-to-end-performance-offline-documentation-packaging-and-release-gates"></a>
### Task 21: Complete End-to-End, Performance, Offline, Documentation, Packaging, and Release Gates

**Deliverable:** A measured, reproducible release candidate with all preserved capabilities, six target archives and transparent remaining limitations.

**Files and ownership:** `internal/e2e/`, `internal/bench/`, minimal adopted-tool automation, CI/offline/release workflows, all operations/user/provider/performance docs, SECURITY/CONTRIBUTING and license/SBOM outputs.

**Dependencies / consumes:** All Tasks 1–20 and 22, including all optional integration contracts and the real-tool matrix. No new speculative core subsystem.

**Produces / contract:** Release archives/checksums/SBOM, benchmark and capability-parity report, complete updated specification/docs and verification evidence.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** Use the one complete index/search/impact/plan/read/confirm/review/consolidate/capsule scenario through CLI and real MCP. Extend it for incremental mutation and old-session retained reads. The shared performance matrix checks memory/latency/reuse, and the offline run uses the compiled binary with all build dependencies prepared before isolation.
- [ ] **Step 2: Implement the complete production path.** Measure Section 23 reference and low-memory workloads using release builds, including parser worker/native memory and full optional process tree; the benchmark corpora are cloned at pinned commits recorded with the results so they serve as the differential oracle for any future engine change. Validate exact capability/fact/query fingerprints before accepting an optimization. Use network-disabled Linux execution plus attempted-socket auditing where supported; no test downloads modules/grammars during isolated runtime. Build/test all six OS/arch combinations with compatible native toolchains; build the six bundle archives by running `codectx tools prefetch --all` into a fresh store per target and verify a bundle indexes the polyglot fixture with networking disabled. Run the Task 22 tools matrix as a release gate. Generate SBOM and native/module license inventory using pinned existing tools; do not write a new release framework.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Complete quick start, source/provenance limitations, strict actor workflow with receipts/review, managed toolchain behavior and offline/bundle use, configuration, retention/rebuild, diagnosis and performance evidence. Check all document anchors/cross-references and command/model parity. Report any unmet budget or unverified platform/profile as a blocker rather than declaring success. Stage/commit reviewed generated files before checking clean-tree status; do not require an empty diff before the release changes have been committed.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go mod tidy
go mod verify
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go build -trimpath -o ./bin/codectx ./cmd/codectx
go test ./internal/bench -run TestResourceBudgets -count=1
go test ./internal/bench -run '^$' -bench . -benchmem -count=5
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.

<a id="task-22-implement-the-managed-analyzer-toolchain-lock-store-and-full-language-matrix"></a>
### Task 22: Implement the Managed Analyzer Toolchain, Lock, Store, and Full Language Matrix

**Deliverable:** Every SCIP, LSP, and dependence profile runs from a lock-pinned, checksum-verified tool the product installs itself, with the six-indexer and six-server matrix of Section 11.7 and no user configuration. `config.Analyzer` and `[analyzers.<name>]` are deleted in place; the release-time lock generator and the CI real-tool matrix inputs exist.

**Files and ownership:** `internal/toolchain/` (lock schema and embedded `tools.lock.json`, store, fetcher, extractor, resolver, runtime launchers, typed errors), `internal/tools/toollock/` (release-time generator), rewritten profile files `internal/provider/scip/profile.go`, `internal/provider/lsp/profile.go`, `internal/provider/dependence/joern/profile.go`, new SCIP profiles for `scip-python`, `rust-analyzer scip`, and `scip-clang`, `internal/config/` removal of `Analyzer` and addition of `Tools`, `internal/cli/tools.go`, `.github/workflows/tools-matrix.yml`, `docs/toolchain.md`, provider docs, `THIRD_PARTY_LICENSES.md`. The integration owner wires `toolchain.Resolver` into `internal/app` composition alongside Task 12.

**Dependencies / consumes:** Task 3 `config.Config`, `process.Runner`, and the confined opener; Task 6 `provider.Detection` (`Available`, `DiagnosticCode`, `ObservedVersion`); Tasks 9–11 profile call sites (`scip.runProfile`, `lsp.Trusted`/`Manager.Open`, `dependence/joern` tool resolution); `model.Error` codes; `lang.Of`.

**Produces / contract:**

```go
package toolchain

type Platform struct{ OS, Arch string }          // "linux","darwin","windows" × "amd64","arm64"
func Current() Platform

type Payload struct{ URL string; SHA256 string; Size int64 }
type Entry struct {
    Name, Version, Kind, License, Upstream string
    Runtime     string            // "", "node", "jdk"
    Entry       string            // payload-relative executable/script/jar
    EntrySHA256 string
    Platforms   map[string]Payload // "linux_amd64" ...
}
type Lock struct{ LockVersion int; Tools map[string]Entry }
func Embedded() Lock                               // parsed once from tools.lock.json; panics only at init on a malformed lock

type Source string // "managed" | "override"
type Tool struct {
    Name, Version string
    Root         string   // store or override directory, absolute
    Executable   string   // absolute; for runtime tools this is the runtime binary
    ArgvPrefix   []string // full launcher argv, e.g. [node, main.js]; Executable == ArgvPrefix[0]
    Env          []string // runtime variables the child needs (JAVA_HOME), nothing inherited
    Checksum     string   // lowercase hex SHA-256 of Executable at resolution time
    Source       Source
}

type Options struct {
    DataDir       string        // <data_dir>/tools is created 0o700
    Offline       bool
    Mirror        string        // optional URL prefix; the lock URL's host is replaced and its full path kept, so one mirror serves upstream and hosted assets
    MaxFetchBytes int64
    FetchTimeout  time.Duration
    Overrides     map[string]config.ToolOverride
    Log           *slog.Logger
}
type Resolver struct{ /* unexported */ }
func New(opts Options) (*Resolver, error)
func (r *Resolver) Resolve(ctx context.Context, name string) (Tool, error)   // typed CTX_TOOL_* errors
func (r *Resolver) Status(ctx context.Context) []Status                      // every lock entry, name order
func (r *Resolver) Prefetch(ctx context.Context, names []string) error
func (r *Resolver) Verify(ctx context.Context) ([]Status, error)
func (r *Resolver) GC(ctx context.Context) (removed int, err error)

type State string // "installed" | "available" | "unsupported_platform" | "override" | "corrupt"
type Status struct{ Name, Version string; State State; Languages []string; Detail string }
```

```go
package config

type Tools struct {
    Offline       bool               `toml:"offline"`
    CacheDir      string             `toml:"cache_dir"`
    Mirror        string             `toml:"mirror"`
    MaxFetchBytes int64              `toml:"max_fetch_bytes"`
    FetchTimeout  Duration           `toml:"fetch_timeout"`
    Override      map[string]ToolOverride `toml:"override"` // user configuration only
}
type ToolOverride struct {
    Executable string `toml:"executable"` // absolute
    Version    string `toml:"version"`    // exact
    Checksum   string `toml:"checksum"`   // required lowercase hex SHA-256
}
```

Profiles become product code: `scip.Profile{Kind profileKind; Tool toolchain.Tool}` with `profileKinds` extended to `scip-python` (triggers `pyproject.toml`, `setup.py`, `requirements.txt`, `setup.cfg`), `rust-analyzer` (trigger `Cargo.toml`, argv `scip ${input_dir} --output ${output_file}`), and `scip-clang` (trigger `compile_commands.json`, argv `--compdb-path ${input_dir}/compile_commands.json --index-output-path ${output_file}`); each profile carries its fixed argv, env allowlist, budgets, timeout, and network posture as Go values, verified against the real tool in this task. `lsp.Trusted(cfg, name)` becomes `lsp.Resolve(ctx, resolver, cfg, name)`; `dependence/joern` resolves its `Parse`/`Export` tools through `resolver.Resolve(ctx, "joern")` with `Parse`/`Export` tools from the `joern` entry under the `jdk` runtime. `provider.Detection.DiagnosticCode` carries the `CTX_TOOL_*` reason when a tool cannot be resolved, so `status`/`doctor` name the exact cause.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current files and callers before deciding or editing. Read all three profile files, their run/open paths, `config.Analyzer` and every consumer, `process.Runner`, the confined opener, and `snapshot` materialization in full. Record missing/unread context rather than guessing. Every subagent performs its own read.
- [ ] **Step 1: Protect the critical behavior with the minimum test set.** Four critical invariants, one table test each: a payload whose SHA-256 or size disagrees with the lock is neither extracted nor executed and leaves no store directory; an archive containing an absolute path, `..`, a symlink escaping the payload, a hard link, or more files/bytes than declared is rejected before any byte lands outside staging; `Offline: true` resolves a missing tool to `CTX_TOOL_OFFLINE` without a dial (use an `http.Transport` whose `DialContext` fails the test); a store directory without `.complete`, or whose `entry` hash disagrees with `entry_sha256`, is invisible and repaired by a fresh install. Serve fixtures from `httptest` with a two-file tarball built in the test; no real download in unit tests.
- [ ] **Step 2: Implement the complete production path.** Embed the lock with `embed`; validate it at init (unique names, lowercase hex digests, positive sizes, runtime references that exist, entry paths that are relative and confined). Implement the fetcher as the single `net/http` owner with byte cap, timeout, same-host redirect limit, mirror substitution, and `HTTP_PROXY` support; stream to staging while hashing, never buffer a payload. Extract with the confined opener semantics of Section 21, preserving executable bits and applying `0o700` directories. Rename into place after fsync; write `.complete`. Serialize per-tool installs with a lock file. Resolve runtimes first and compose `ArgvPrefix`; hash the entry executable at every resolution. Delete `config.Analyzer`, `SortedAnalyzers`, `validateAnalyzers`, the `analyzers` fingerprint contribution and every consumer; add `config.Tools` with validation (absolute override paths, exact versions, required checksums, project configuration cannot set `[tools]`). Fold tool name, version and payload digest into `UnitSpec.ProviderVersion` and the LSP input digest so a tool change invalidates units. Rewrite the three profile files and add the three new SCIP profiles. Write `internal/tools/toollock` (download upstream, verify upstream digest, build-time `npm ci --omit=dev` and `go build` for `gopls`, normalize, tar.gz, compute digests, emit the lock and a license table). Publish the five hosted bundles (gopls and the four npm tools) as `tools-v1` release assets, point every other entry at its upstream asset with the upstream digest, and commit the generated lock.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Add `codectx tools status|prefetch|verify|gc [--json]` on the shared facade; extend `doctor` and `status` with the toolchain report; the `CTX_TOOL_*` codes map to exit 5 (provider unavailable) except `CTX_TOOL_OVERRIDE_INVALID`, which is exit 3. Add the `net/http` import check to CI (`go list -deps -f` over `./cmd/codectx` fails on any importer outside `internal/toolchain`). Add `.github/workflows/tools-matrix.yml`: for each lock entry on `ubuntu-latest`, `macos-latest`, and `windows-latest`, `codectx tools prefetch --all` then the provider smoke run against the polyglot fixture, which must exercise every advertised language through its real path (tree-sitter grammar, SCIP indexer where applicable, dependence frontend, normalization, storage, query): Go, TypeScript, TSX, JavaScript, Python, Java, C, C++ and Rust, with C++ and TSX included explicitly because the research rounds exercised only C and TypeScript; the callsite-join check includes non-ASCII TypeScript and Java fixtures so UTF-16 column conversion is tested; a profile whose smoke fails cannot be documented as supported. Update `docs/toolchain.md`, `docs/providers-scip.md`, `docs/providers-lsp.md`, `docs/providers-dependence.md`, `docs/configuration.md`, README quick start, `THIRD_PARTY_LICENSES.md` (Node, Temurin, jdtls EPL-2.0, clangd Apache-2.0 with LLVM exception, rust-analyzer MIT/Apache-2.0, scip-python MIT (vendors pyright), other scip-* Apache-2.0, pyright MIT, typescript-language-server Apache-2.0, TypeScript Apache-2.0, Joern Apache-2.0) and remove every "unverified against a real tool" label that the matrix now proves.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review. Diagnose failures, rerun after fixes, and record the actual result and resource evidence.

```bash
go test ./internal/toolchain ./internal/config ./internal/provider/scip ./internal/provider/lsp ./internal/provider/dependence/... -count=1
go run ./internal/tools/toollock -check   # lock digests match published tools-v1 assets
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate. No known in-scope defect, dead consumer, duplicate implementation, silent capability reduction, or missing critical evidence may be carried forward as complete. Update task/architecture/source documentation in the same change.

---

<a id="task-23-implement-early-cutoff-on-declaration-only-signature-digests"></a>
### Task 23: Implement Early Cutoff on Declaration-Only Signature Digests

**Deliverable:** An edit confined to function bodies in one file re-runs only that file's own unit; the SCIP and dependence units of every module that merely depends on it are reused, proven per language by differential tests before that language is enabled.

**Files and ownership:** `internal/provider/treesitter/signature/` (per-language declaration digest over the existing tree-sitter AST: exports, type declarations, function and method signatures, imports, constants that other files can observe; bodies excluded), `internal/model` (`unit_inputs.signature_hash` beside `content_hash`), `internal/storage/sqlite` (schema and reuse validation reading `signature_hash` for dependency inputs), `internal/index` (coordinator keys semantic units on own-file content hashes plus dependency-file signature hashes; a language without a proven digest keeps content-hash invalidation), docs.

**Dependencies / consumes:** Task 8 tree-sitter AST and queries, Task 12 coordinator and reverse-dependency closure, Task 9/11 unit input records.

**Produces / contract:** `signature.Digest(lang, ast) (hash, ok)`; a per-language `proven` flag that is `true` only when the differential gate below passes and is recorded in the plan and the language docs; coordinator reuse decisions logged with the reason (`content`, `signature`, `dependency-signature`).

**Rulings applied:** research/13 R4 and research/14 §3 item 4. The digest is conservative: anything the language lets another file observe (Python module-level assignments, Go exported identifiers and struct fields, TypeScript exports and ambient declarations, Java public/protected members and annotations, Rust `pub` items and macros, C/C++ everything in headers) is in it. If a language's cross-file semantics cannot be bounded this way (C/C++ macros), that language is never enabled and the plan says so. Depends on Section 13.3's carried-over units so a rebinding of inputs is honest.

- [ ] **Step 0: Complete the read, trace, reuse and resource gate.** Apply Section 30.1 to the actual current tree-sitter, coordinator and storage files before editing.
- [ ] **Step 1: Protect the critical behavior with the minimum existing test extension.** The differential gate: for each language, a corpus of body-only edits and a corpus of declaration edits; a body-only edit must leave every dependent unit's SCIP and dependence facts identical to a full re-run (compared by the Task 9/11 delta keys), and every declaration edit must change the digest. One failure disables the language.
- [ ] **Step 2: Implement the complete production path.** Digest, storage column, coordinator keying, reuse-reason logging, and the per-language enable flags; no language enabled without a passing gate recorded in the report.
- [ ] **Step 3: Wire consumers, failure handling and documentation.** Status and doctor show which languages use signature cutoff; docs/indexing.md explains the two invalidation modes; a digest error falls back to content-hash invalidation for that unit and is counted.
- [ ] **Step 4: Run the focused verification below.** These are commands to run during implementation, not claimed results of this document review.

```bash
go test ./internal/provider/treesitter/signature ./internal/index ./internal/storage/... -count=1
go test ./internal/e2e -run 'TestEarlyCutoff' -count=1   # body-only edit: dependent units reused; declaration edit: dependents re-run
go vet ./...
git diff --check
```

- [ ] **Step 5: Review the complete changed files and integration boundary, then commit.** Follow Section 30.1's completion gate.

---

<a id="31-verification-matrix"></a>
## 31. Verification Matrix

The same focused test or review artifact may satisfy multiple rows. A requirement is not complete merely because its code or document exists. Column “Evidence” names what implementation must produce, not a result already measured in this specification review.

| Requirement | Sections | Tasks | Required evidence |
|---|---|---|---|
| Full-file read, caller/contract/state/dependency/integration tracing by every worker | 5.1, 6, 30.1 | All | Task read/trace/reuse note tied to real revision; lead review of complete changed files and consumers. |
| Greenfield in-place changes; no legacy/migration paths | 5, 6, 12, 28 | All | Current schema only; no obsolete config/API/consumer/registration paths; focused architecture review. |
| Reuse before new code; consolidate emerging duplication | 5, 7, 28–30 | All | Existing shared implementations inspected; one actual owner for process, source conversion, signing, storage and domain logic. |
| Go-only product logic and exact dependencies | 6, 24, 29 | 1–11, 19, 21 | Module/toolchain/checksum inventory; native assets separately licensed; reproducible build metadata. |
| Zero paid/AI/cloud core dependency | 2, 4, 6, 20–21 | 1, 3, 18–21 | Strict config and offline full workflow; no core outbound attempts. |
| Exact retained source and safe materialization | 9–10, 21 | 3–5, 16, 20 | Worktree/Git transformation and history-change fixture; safe opener, block/hash checks, no writable hard links. |
| Streaming bounded memory | 6, 10–14, 20, 23 | 3–16, 20–21 | Byte reservations and measurements; large nested record case; no whole-source/graph/index collections. |
| Genuine incremental unit reuse | 9, 12–13, 23 | 5–9, 11–14 | No-change parse/FTS counters; changed dependency/add/delete/rename invalidation; retained provenance. |
| Atomic publication and optional failure isolation | 11–13 | 5–12, 20 | Unsealed invisibility, failed-unit cleanup, rollback, concurrent reader and workspace-lock checks. |
| Complete bundled structural baseline | 4, 11.3 | 7–8, 21 | All nine grammars in shared polyglot fixture and native-target smoke; no optional tool required. |
| Exact-source SCIP | 11.4 | 9, 22 | Source-binding/position/local-symbol/occurrence fixtures, streamed memory check and real-tool smoke for all six indexers. |
| Managed analyzer toolchain | 11.7, 20, 21, 24 | 22, 20, 21 | Digest/size mismatch never extracted or executed; confined extraction; offline never dials; atomic store; single `net/http` importer; per-platform CI matrix runs every lock entry. |
| Full supported LSP overlay | 11.5 | 10, 22 | Actual protocol fake-server lifecycle and required-method table; private pinned input and cleanup. |
| Dependence integration | 11.6 | 11, 22 | CSV fixture, real pinned engine run per language, partial reporting from skipped methods, no engine name on a product surface. |
| Canonical node and relation provenance | 9, 11–12 | 2, 5–11 | Node facts, aliases and range-bearing relation evidence with scope/source identity; ambiguity retained. |
| Stable generation-scoped lexical ranking | 12.4, 14 | 5, 13, 15 | Scores/hash unchanged by unrelated unit/generation insert/activation/pruning; near-tie platform checks. |
| Bounded graph and pagination | 14 | 5, 13–14 | Cycle/fanout, edge/node/depth/work limits, signed cursor, lease and deterministic continuation. |
| Complete required context with honest budgets | 15 | 15, 17 | Full selected files, boundary gaps visible, exact wire budgets, single-file floor and coherent slices. |
| Actor-specific delivery and full-file coverage | 16 | 16–19 | Wrong-actor/receipt/hash rejection, broken delivery, interval union, empty/long/invalid-UTF8 cases. |
| Scope review, waiver and freshness enforcement | 16–17 | 17–19 | Current-scope review, include invalidation, waiver cannot grant strict readiness, immediate source check. |
| Deterministic observations/capsules | 17 | 2, 5, 17 | Typed reference validation, idempotency, canonical hash and repeat retrieval without new timestamp. |
| CLI/MCP parity and correct protocol | 18–19 | 17–19, 21 | One shared E2E domain fixture through both adapters, current SDK protocol, typed schema/channels. |
| Leases, bounded retention and recovery | 10, 12, 20, 22 | 5, 16, 20 | Concurrent pin/GC, open-session retention, expired audit cleanup, abandoned staging and disk-full cases. |
| Aggregate RAM/native/process performance | 20, 23 | 3, 5, 8–16, 20–21 | Reference benchmark/soak report, capability fingerprints, process-tree RSS and real latency distributions. |
| Minimal critical tests | 25, 30 | All | Small shared suites, explicit risk for added tests, no repeated trivial/golden/mock coverage. |
| Cross-platform release completeness | 24 | 1, 8, 19–22 | All six slim and bundle artifacts and native smoke evidence, including Windows arm64. |
| Full updated documentation and citations | All, 33 | All | TOC/link/code/schema validation, interfaces/commands reflected consistently and current source notes. |

---

<a id="32-definition-of-done"></a>
## 32. Definition of Done

- [ ] All 22 tasks have their actual required producer/consumer dependencies implemented, reviewed and wired; optional integrations are not replaced by stubs or deferred contracts.
- [ ] Every worker and reviewer applied the full-file read/trace/reuse policy to the actual code. The lead verified real changes, callers and wiring rather than accepting summaries.
- [ ] No known unresolved in-scope correctness/resource defect, dead consumer, unreachable production path, duplicate shared behavior, compatibility/migration scaffolding, speculative infrastructure or undocumented mandatory dependency remains.
- [ ] All required base and optional capabilities in Section 4 are implemented. Every managed tool of Section 11.7 installs itself from the lock with no user configuration, and unavailability is reported with its typed platform/offline/fetch reason.
- [ ] Source snapshots retain exact captured bytes; unit reuse is input-safe; generations are atomic; pinned queries and open sessions survive later activation and Git/worktree changes.
- [ ] All public responses are bounded, generation/snapshot-qualified and explicit about completeness. Canonical search/context does not change because unrelated generations change.
- [ ] Strict readiness requires each actor's own complete confirmed source, current-scope review, complete declared boundaries and fresh write-time validation. Search hits, acknowledgments, another actor's coverage and waivers cannot bypass it.
- [ ] CLI and MCP invoke the same typed services; current protocol, error envelopes, output channels, cancellation and source isolation are verified.
- [ ] All resource limits have real owners and release paths. Measured process-tree/native memory, latency, disk, reuse and soak behavior meet the declared budgets without reduced capabilities.
- [ ] The minimal critical test families, supported race checks, vet/build, source safety, offline/no-outbound checks, real provider-profile smoke and release-target checks have actual recorded passing results.
- [ ] Every supported target has an archive, checksum, SBOM, license/notice inventory and reproducible build metadata. Versions/embedded SQLite floor are checked at runtime/CI, not assumed from a dependency name.
- [ ] Documentation is complete and consistent with the shipped app, including source/coverage limitations, trust, rebuild/retention, current commands, measurement methodology, citations and working TOC.
- [ ] Final changes and generated artifacts are reviewed and committed before the clean-working-tree check; unrelated files are never staged accidentally.

**Evidence boundary:** This revised document defines what must be implemented and verified. A document audit, schema smoke check, or successful plan regeneration does not establish that the application compiles, that its tests pass, that the performance targets have been reached, or that every possible bug has been eliminated. Those claims require the executable and the evidence above.

---

<a id="33-authoritative-references"></a>
## 33. Authoritative References

References 1–21 retain the original architecture's source set; 22–36 support the corrections and more specific contracts in this revision; 37–46 support the managed analyzer toolchain. Current version and protocol facts were checked on 2026-09-04 and the toolchain sources on 2026-09-12. These sources support dependency behavior and design rationale; the new resource budgets, actor policy, reusable-unit model and acceptance gates are explicit product design decisions, not externally certified performance results.

Pin exact dependency/grammar/profile revisions and checksums during implementation. A link to a moving default branch is a source citation, not a reproducible dependency pin. The Tree-sitter issue is a reported risk, not evidence that the selected future build has or has fixed the defect. External profile commands must be verified against the installed pinned distribution. No source is presented as proof that codectx itself has been built or benchmarked.

<a id="ref-1"></a>
1. **Go release history and support policy**  
   <https://go.dev/doc/devel/release>  
   Go 1.27 / initial 1.27.1 toolchain pin.

<a id="ref-2"></a>
2. **Official Model Context Protocol Go SDK**  
   <https://github.com/modelcontextprotocol/go-sdk>  
   Adopted Go transport and typed tool implementation.

<a id="ref-3"></a>
3. **MCP specification 2026-07-28**  
   <https://modelcontextprotocol.io/specification/2026-07-28>  
   Selected current wire-protocol baseline.

<a id="ref-4"></a>
4. **Official Tree-sitter Go bindings**  
   <https://github.com/tree-sitter/go-tree-sitter>  
   Native parser ownership and versioned Go API.

<a id="ref-5"></a>
5. **Tree-sitter documentation**  
   <https://tree-sitter.github.io/tree-sitter/>  
   Parsing/query concepts and grammar contracts.

<a id="ref-6"></a>
6. **SCIP protocol and Go bindings**  
   <https://github.com/scip-code/scip>  
   Adopted precise-index interchange.

<a id="ref-7"></a>
7. **Language Server Protocol specification 3.18**  
   <https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/>  
   Capability-negotiated local LSP interface; pin the exact supported feature/profile revision during implementation.

<a id="ref-8"></a>
8. **Joern Code Property Graph documentation**  
   <https://docs.joern.io/code-property-graph/>  
   CPG concepts and explicit evidence mapping.

<a id="ref-9"></a>
9. **Joern complex query steps**  
   <https://docs.joern.io/cpgql/complex-steps/>  
   Call/dataflow interpretation, not a substitute for verified exported facts.

<a id="ref-10"></a>
10. **Joern graph export documentation**  
   <https://docs.joern.io/export/>  
   Built-in formats and version-sensitive noninteractive export.

<a id="ref-11"></a>
11. **SQLite Write-Ahead Logging**  
   <https://www.sqlite.org/wal.html>  
   Reader/writer/checkpoint behavior and documented WAL-reset fix.

<a id="ref-12"></a>
12. **SQLite FTS5**  
   <https://www.sqlite.org/fts5.html>  
   Corpus-scoped BM25, vocabulary data and external-content consistency.

<a id="ref-13"></a>
13. **modernc.org/sqlite documentation**  
   <https://pkg.go.dev/modernc.org/sqlite>  
   Driver/connection hooks, supported platforms and exact libc dependency requirement.

<a id="ref-14"></a>
14. **fsnotify official repository and FAQ**  
   <https://github.com/fsnotify/fsnotify>  
   Directory watches, nonrecursive behavior and filesystem limits.

<a id="ref-15"></a>
15. **Cobra official repository**  
   <https://github.com/spf13/cobra>  
   CLI parsing and help behavior.

<a id="ref-16"></a>
16. **BurntSushi TOML parser**  
   <https://github.com/BurntSushi/toml>  
   Strict typed TOML parsing/undecoded-key handling.

<a id="ref-17"></a>
17. **Git plumbing documentation**  
   <https://git-scm.com/docs>  
   Git object/status/path commands.

<a id="ref-18"></a>
18. **Aider repository-map design**  
   <https://aider.chat/docs/repomap.html>  
   Retained design precedent for graph-ranked context only, not implementation code or a required dependency.

<a id="ref-19"></a>
19. **CycloneDX specification**  
   <https://cyclonedx.org/specification/overview/>  
   Alternative SBOM representation supported by selected release tooling.

<a id="ref-20"></a>
20. **SPDX specifications**  
   <https://spdx.dev/specifications/>  
   Default release SBOM/license representation.

<a id="ref-21"></a>
21. **CKB license**  
   <https://github.com/SimplyLiz/ckb/blob/main/LICENSE>  
   Retained explicit non-adoption/clean-room constraint; review actual license text at any adoption decision.

<a id="ref-22"></a>
22. **MCP 2026-07-28 server discovery**  
   <https://modelcontextprotocol.io/specification/2026-07-28/server/discover>  
   Current discovery instead of the obsolete MCP initialize-handshake assumption.

<a id="ref-23"></a>
23. **SCIP schema source**  
   <https://github.com/scip-code/scip/blob/main/scip.proto>  
   Document text, local symbols and UTF-8/16/32 position encodings.

<a id="ref-24"></a>
24. **Git attributes: checkout/clean transformations**  
   <https://git-scm.com/docs/gitattributes>  
   Why a Git blob and worktree bytes can differ.

<a id="ref-25"></a>
25. **Go traversal-resistant file APIs**  
   <https://go.dev/blog/osroot>  
   Root-confined filesystem opening and TOCTOU considerations.

<a id="ref-26"></a>
26. **Tree-sitter Go ParseWithOptions leak report, issue 55**  
   <https://github.com/tree-sitter/go-tree-sitter/issues/55>  
   Dependency risk report; inspect the selected pinned implementation and verify, do not claim an unverified fix.

<a id="ref-27"></a>
27. **Joern common issues**  
   <https://docs.joern.io/common-issues/>  
   Version-dependent dataflow overlay behavior.

<a id="ref-28"></a>
28. **SQLite PRAGMA reference**  
   <https://www.sqlite.org/pragma.html>  
   Connection caches, durability, temp storage, busy timeout and checkpoint controls.

<a id="ref-29"></a>
29. **Windows Job Objects**  
   <https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects>  
   Child process grouping, limits and termination semantics.

<a id="ref-30"></a>
30. **Go garbage collector guide**  
   <https://go.dev/doc/gc-guide>  
   Memory accounting, soft limits, native exclusions and GC tradeoffs.

<a id="ref-31"></a>
31. **Go runtime/debug SetMemoryLimit**  
   <https://pkg.go.dev/runtime/debug#SetMemoryLimit>  
   Runtime memory-limit API and scope; use with measured admission, not as an RSS guarantee.

<a id="ref-32"></a>
32. **Go 1.27 release notes**  
   <https://go.dev/doc/go1.27>  
   Language/runtime/toolchain baseline.

<a id="ref-33"></a>
33. **Git ls-files documentation**  
   <https://git-scm.com/docs/git-ls-files>  
   NUL-delimited tracked/untracked discovery and ignore handling.

<a id="ref-34"></a>
34. **SQLite 3.51.3 release notes**  
   <https://sqlite.org/releaselog/3_51_3.html>  
   WAL-reset corruption fix; reject older affected embedded engines and the withdrawn 3.52.0 release.

<a id="ref-35"></a>
35. **modernc.org/sqlite v1.58.0 documentation**  
   <https://pkg.go.dev/modernc.org/sqlite@v1.58.0>  
   Initial exact driver candidate; published documentation reports SQLite 3.53.4 on the required platforms.

<a id="ref-36"></a>
36. **MCP Go SDK v1.7.0 release**  
   <https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0>  
   Exact initial SDK release supporting the selected protocol.

---

**End of complete revised design specification and implementation plan.**

<a id="ref-37"></a>
37. **CodeQL CLI bundle and pack download**  
   <https://docs.github.com/en/code-security/codeql-cli/using-the-advanced-functionality-of-the-codeql-cli/codeql-cli-reference> and <https://docs.github.com/en/code-security/codeql-cli/codeql-cli-manual/pack-download>  
   Single bundle with every extractor and its own runtime; on-demand pinned pack download for the slim CLI.

<a id="ref-38"></a>
38. **mise lockfile**  
   <https://mise.jdx.dev/dev-tools/mise-lock.html>  
   Exact version, per-platform URL and checksum per tool, verified on install.

<a id="ref-39"></a>
39. **Sourcegraph auto-indexing inference and scip-io indexer orchestration**  
   <https://sourcegraph.com/docs/code-search/code-navigation/auto_indexing> and <https://github.com/GlitterKill/scip-io>  
   Evidence-driven inference of which indexers a repository needs; pinned hash-verified indexer installs.

<a id="ref-40"></a>
40. **Zed language-server auto-download discussion and checksum defect**  
   <https://news.ycombinator.com/item?id=40902826> and <https://github.com/bug-ops/deps-zed/issues/14>  
   Silent runtime download without verification is the anti-pattern the lock model avoids.

<a id="ref-41"></a>
41. **Mason registry and Trunk hermetic tool cache**  
   <https://github.com/mason-org/mason.nvim> and <https://docs.trunk.io/code-quality/linters/configure-linters>  
   Declarative tool registry with platform targets and a private bin directory; pinned hermetic tool cache.

<a id="ref-42"></a>
42. **Node.js distribution index and checksums**  
   <https://nodejs.org/dist/>  
   `SHASUMS256.txt` per release used by the lock generator.

<a id="ref-43"></a>
43. **SCIP indexers: scip-python, scip-clang, scip-java**  
   <https://github.com/sourcegraph/scip-python>, <https://github.com/sourcegraph/scip-clang>, <https://github.com/sourcegraph/scip-java>  
   Upstream distributions, platform coverage (no Windows scip-clang) and dependency-resolution requirements.

<a id="ref-44"></a>
44. **rust-analyzer `scip` subcommand**  
   <https://rust-lang.github.io/rust-analyzer/src/rust_analyzer/cli/scip.rs.html>  
   `rust-analyzer scip <path> --output <file>`; one binary serves both SCIP and LSP profiles.

<a id="ref-45"></a>
45. **Eclipse JDT Language Server**  
   <https://github.com/eclipse-jdtls/eclipse.jdt.ls>  
   Milestone tarball distribution, Java 21 minimum, equinox launcher argv and `-data` requirement.

<a id="ref-46"></a>
46. **Adoptium Temurin releases**  
   <https://adoptium.net/temurin/releases/>  
   JDK 21 LTS per-platform archives with published SHA-256 digests for the managed `jdk` runtime.
