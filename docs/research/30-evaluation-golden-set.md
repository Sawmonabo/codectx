# Evaluation golden set: how other tools and papers build and score theirs, and keep them honest

Research behind [the evaluation design](../designs/2026-10-13-codectx-eval-design.md). Gathered on 2026-10-13 from
primary sources: papers, specifications, official documentation and the benchmark repositories themselves. A source
that could not be fetched is marked as such. This is the one document of the evaluation that names other tools,
papers, datasets and vendors; the design describes the same choices in codectx's own terms.

## 1. Call-graph benchmarks with annotated ground truth

**Judge / JCG** (Reif, Kübler, Eichberg, Helm, Mezini, *Judge: Identifying, Understanding, and Evaluating Sources
of Unsoundness in Call Graphs*, ISSTA 2019, DOI 10.1145/3293882.3330555; https://github.com/opalj/JCG).
- **What it holds.** 122 test cases in 23 categories (polymorphic calls, Java 8 default methods, lambdas and method
  references, reflection, serialization, static initializers, class loading and others), each a runnable program in
  a Markdown file.
- **How expectations are written.** As annotations in the source, for example
  `@DirectCall(name = "method", line = 15, resolvedTargets = {"Lvc/ClassImpl;"}, prohibitedTargets = {"Lvc/Class;"})`
  (https://raw.githubusercontent.com/opalj/JCG/master/jcg_testcases/src/main/resources/java/VirtualCalls.md). The
  JavaScript and Python cases use a separate JSON block of direct and indirect links instead.
- **Verdicts** (https://raw.githubusercontent.com/opalj/JCG/master/jcg_annotation_matcher/src/main/scala/Assessment.scala,
  `CGMatcher.scala`):
  - Sound;
  - Imprecise: a prohibited target appeared;
  - Unsound: an expected target is missing;
  - Error and Timeout.
- **Validation.** A built-in verifier checks every case's annotations, and two more authors cross-checked them.
- **The lesson the design takes.** Pair every expected edge with prohibited ones. The paper notes that some cases
  "are simply supported due to an algorithm's inherent imprecision", so without prohibited targets an
  over-approximation passes.

**Systematic Evaluation of the Unsoundness of Call Graph Construction Algorithms for Java** (Reif et al., SOAP
2018, DOI 10.1145/3236454.3236503;
https://2018.ecoop.org/details/SOAP-2018-papers/3/Systematic-Evaluation-of-the-Unsoundness-of-Call-Graph-Construction-Algorithms-for-Ja).
- The precursor suite. Each fixture tests one aspect.
- Missed edges are studied far less than precision.

**PyCG** (Salis, Sotiropoulos, Louridas, Spinellis, Mitropoulos, ICSE 2021; https://arxiv.org/abs/2103.00587;
https://github.com/vitsalis/PyCG/tree/master/micro-benchmark).
- **Micro-benchmark.** 112 minimal programs in 16 categories. Each has its source, a separate `callgraph.json`
  adjacency list keyed by qualified name, and a description. Each program has one execution path.
- **Terms.** "Complete" means no false positive, and "sound" means no false negative. Precision and recall are
  measured on the edges of a macro-benchmark of five real packages.
- **Macro ground truth.** The hand-built ground truth missed 90 of 2,506 edges until it was diffed against three
  tools' outputs.
- **Results.** PyCG reaches 99.2% precision and 69.9% recall.

**JARVIS** (Huang et al., arXiv 2305.05949; https://arxiv.org/html/2305.05949v5).
- Its macro ground truth is a dynamic trace (`python -m trace --listfuncs`) plus manual additions. Two authors work
  independently and a third adjudicates.
- The paper's own case counts disagree between its sections, which is an argument for pinning and hashing a set.

**HeaderGen** (Venkatesh et al., arXiv 2301.04419; https://arxiv.org/html/2301.04419v4).
- It maps call sites to line numbers.
- At least two annotators label each item, with Cohen's κ 0.89.

**TypeEvalPy** (Venkatesh et al., arXiv 2312.16882; https://github.com/secure-software-engineering/TypeEvalPy).
- Facts are keyed by location (`file`, `line_number`, `col_offset`, `function`, `parameter` or `variable`) and
  scored by exact match per fact kind.

**JavaScript call graphs.**
- **Antal et al.** (*Static JavaScript Call Graphs: a Comparative Study*, SCAM 2018;
  https://arxiv.org/html/2405.07206v1). The gold standard is the union of five tools' true edges, checked by hand.
  Recall against such a union is relative recall and overstates true recall.
- **Chakraborty, Olivares, Sridharan, Hassanshahi** (ECOOP 2022; https://par.nsf.gov/servlets/purl/10335186).
  Recall of call-site targets averaged over call sites is the metric for code navigation. Reachable-node and
  reachable-edge recall serve other clients.
- **Jelly** (https://github.com/cs-au-dk/jelly). It compares a dynamic call graph from NodeProf with its static one,
  reporting precision and recall.
- **Feldthaus et al.** (*Efficient Construction of Approximate Call Graphs for JavaScript IDE Services*, ICSE 2013;
  https://2013.icse-conferences.org/content/efficient-construction-approximate-call-graphs-javascript-ide-services.html,
  abstract only).

**Dynamic oracles and their limits.**
- **Sui, Dietrich, Tahir, Fourtounis** (*On the Recall of Static Call Graph Construction in Practice*, ICSE 2020;
  https://2020.icse-conferences.org/details/icse-2020-papers/47/On-the-Recall-of-Static-Call-Graph-Construction-in-Practice,
  abstract only). Executions are the recall oracle, and median recall is 0.884.
- **Helm et al.** (*Total Recall? How Good Are Static Call Graphs Really?*, ISSTA 2024;
  https://sse.cs.tu-dortmund.de/storages/sse-cs/r/Publications/Preprints/dyncg-issta-2024.pdf). Precision measured
  against a dynamic baseline is a lower bound on true precision (Theorem 3.3), and recall against it is bounded.
  The design reports its truth lens as bounds for this reason.

**Differential testing.** Klinger, Christakis, Wüstholz, *Differentially Testing Soundness and Precision of Program
Analyzers* (https://arxiv.org/abs/1812.05033).
- Analyzers can be "designed to perform well in such competitions by specifically tailoring their techniques to the
  given benchmarks".
- Fresh variants generated from seed programs defend against that. The design's renamed copies and held-out cases
  come from this.

## 2. Inline markers and golden files

- **gopls marker tests** (https://raw.githubusercontent.com/golang/tools/master/gopls/internal/test/marker/doc.go).
  - Each test is one txtar archive of files.
  - Markers are `//@` comments: `loc`, `implementation`, `incomingcalls`, `outgoingcalls`, `refs`, `subtypes` and
    `supertypes`. They compare sets of locations, not their order.
  - `-update` regenerates golden blobs but never the hand-written expectations.
  - This is the model for the design's `@tag` anchors.
- **txtar** (https://raw.githubusercontent.com/golang/tools/master/txtar/archive.go). Files are separated by
  `-- name --` lines, and "there are no possible syntax errors". The design's refresh edit steps use this shape.
- **Go golden files with `-update`** (Hashimoto, *Advanced Testing with Go*,
  https://speakerdeck.com/mitchellh/advanced-testing-with-go). The flag writes actual output into the golden file.
  The design rejects that, because it writes product output into expectations.
- **rust-analyzer fixtures**
  (https://raw.githubusercontent.com/rust-lang/rust-analyzer/master/crates/test-fixture/src/lib.rs;
  https://raw.githubusercontent.com/rust-lang/rust-analyzer/master/crates/ide/src/call_hierarchy.rs).
  - Multi-file fixtures use `//- /lib.rs crate:foo deps:bar`, and `$0` marks the cursor.
  - Call-hierarchy expectations are inline snapshots carrying the call-site ranges.
- **TypeScript fourslash**
  (https://raw.githubusercontent.com/microsoft/TypeScript/main/tsc/internal/fourslash/tests/callHierarchyClass_test.go).
  `/*marker*/` comments, with the expected output in separate baselines. A `new Baz()` appears as an outgoing call.
- **clangd call-hierarchy tests**
  (https://raw.githubusercontent.com/llvm/llvm-project/main/clang-tools-extra/clangd/unittests/CallHierarchyTests.cpp).
  `$Name[[...]]` ranges, with incoming calls through overrides included.
- **insta** (https://raw.githubusercontent.com/mitsuhiko/insta/master/insta/src/lib.rs) and **ApprovalTests**
  (https://github.com/approvals/ApprovalTests.Java).
  - Updating is off in CI.
  - New output waits for review.
  - Approved files are committed.
- **tree-sitter corpus tests** (https://tree-sitter.github.io/tree-sitter/creating-parsers/5-writing-tests.html)
  and **universal-ctags Units** (https://docs.ctags.io/en/latest/testing-parser.html). These are expected trees and
  expected tags per case. Both offer a mode that rewrites expectations from current output, which the design does
  not have.

## 3. Why other analyzers are not live oracles

Research 24 put one fixture to six language servers: two subclasses, three levels, a `super` call, a qualified call
and an interface case.
- **The servers disagree with each other.** tsserver and gopls add `direct(d: Dog)` to `Animal.sound`'s callers.
  pyright and rust-analyzer list no caller through the base. Only jdtls gets three levels right. No server marks a
  call through a base.
- **Reading them as truth would score their drift.** The design therefore uses analyzers only before the first run, to
  catch the authors' mistakes (section 4), and never as the expected answer.

## 4. The oracles used before the first run

Each language's static oracle is its reference compiler or type checker. A second static oracle and a runtime trace
are added beside it. None runs during an evaluation; each runs once, on an uncommitted copy of the golden projects,
before the first run.

| Language | Static oracle | Second static oracle | Runtime oracle |
|---|---|---|---|
| Go | `go/types` through `golang.org/x/tools/go/packages` (`Selection`, `Uses`, `types.Implements`); `go/callgraph/static` and `cha` as lower and upper bounds | gopls `callHierarchy` and `implementation` | a function-entry trace using `runtime.Callers`, inserted into the private copy |
| Java | javac with a Trees plugin (`MethodInvocationTree` to its element, `Elements.overrides`) | Eclipse JDT Language Server call and type hierarchy | a `StackWalker` entry trace |
| C, C++ | clang `-Xclang -ast-dump=json` (`CallExpr` to its referenced declaration, `overridden_methods`) | clangd 20 or later (incoming and outgoing calls) | `-finstrument-functions`, with addresses resolved by `addr2line` |
| Rust | rustc MIR (`-Zunpretty=mir`, nightly) and `rustdoc --output-format json` for impls | rust-analyzer call hierarchy and implementations | an entry trace with `#[track_caller]` and backtraces |
| TypeScript, TSX, JavaScript | the TypeScript compiler API (`getResolvedSignature`, `getSymbolAtLocation`; `allowJs` and `checkJs` for JavaScript) | the TypeScript language service's call hierarchy (the same engine; independence comes from the trace and the blind derivation) | a Node entry trace (`new Error().stack`, with `--enable-source-maps`) |
| Python | pyright or mypy (not the product's own overlay server) | jedi | `sys.monitoring` CALL events (Python 3.12 or later), which carry exact positions |

**Oracles for the other capabilities.**

| Capability | Oracle |
|---|---|
| Dependence | `go/ssa`; LLVM IR at `-O0` after `mem2reg`; Rust MIR. For Java, Python and JavaScript, two blind derivations by the textbook definitions (Ferrante, Ottenstein and Warren for control dependence) |
| Routes | each application's runtime router table, dumped in the private copy with the frameworks installed: Express's stack, Starlette and FastAPI `routes`, the Go routers' route lists, Spring's request mappings |
| Manifests and lock files | the format specifications, and each package manager's own reading of the lock where it works offline |
| Structure | the language servers' `documentSymbol`, and the compilers' ASTs for names, nesting and name ranges |

**Tool facts that matter.**
- **gopls** (https://raw.githubusercontent.com/golang/tools/master/gopls/doc/features/navigation.md) leaves dynamic
  calls out of its call hierarchy, so it is an oracle for static edges and `implements`, not for interface dispatch.
- **`go/callgraph`** (https://raw.githubusercontent.com/golang/tools/master/go/callgraph/cha/cha.go). CHA "may thus
  include spurious call edges", so it is the upper bound, and `static` is the lower bound.
- **clangd** has had outgoing calls only since version 20:
  https://raw.githubusercontent.com/llvm/llvm-project/llvmorg-20.1.0/clang-tools-extra/clangd/ClangdLSPServer.cpp
  and https://lists.llvm.org/pipermail/cfe-commits/Week-of-Mon-20241202/650615.html.
- **Eclipse JDT LS** implements `implementation`, both call-hierarchy directions and the type hierarchy
  (https://raw.githubusercontent.com/eclipse-jdtls/eclipse.jdt.ls/main/org.eclipse.jdt.ls.core/src/org/eclipse/jdt/ls/core/internal/handlers/JDTLanguageServer.java).
- **Pyright** registers call hierarchy but no implementation provider. basedpyright adds one
  (https://raw.githubusercontent.com/microsoft/pyright/main/packages/pyright-internal/src/languageServerBase.ts;
  https://raw.githubusercontent.com/DetachHead/basedpyright/main/packages/pyright-internal/src/languageServerBase.ts).
- **SCIP indexers** (https://raw.githubusercontent.com/sourcegraph/scip/main/scip.proto) give references,
  `is_implementation` relationships and enclosing ranges, but no dispatch resolution. codectx already imports their
  output as a provider, so they are no independent oracle for its `references`.
- **LSP 3.17**
  (https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#callHierarchy_incomingCalls,
  `#callHierarchy_outgoingCalls`, `#typeHierarchy_supertypes`, `#textDocument_implementation`). The shared shape
  of these answers: an item, plus `fromRanges` for the call sites.

**What is recorded here before the first run.** The exact version and argv of each tool, and the agreement statistics per language:
rows checked, disagreements, and how each class of disagreement was settled.

## 5. Other capabilities

**Search.**
- Buckley and Voorhees, *Retrieval Evaluation with Incomplete Information* (SIGIR 2004;
  https://tsapps.nist.gov/publication/get_pdf.cfm?pub_id=150469). An unjudged document is not a judged
  nonrelevant one, and bpref is robust to incomplete judgments.
- ir-measures (https://ir-measur.es/en/latest/measures.html): the definitions of Success@k, RR, nDCG and Judged@k.
- CodeSearchNet (https://arxiv.org/abs/1909.09436): graded 0–3 relevance over pooled candidates. CoSQA
  (https://arxiv.org/abs/2105.13239): three annotators per label.
- The design judges every document that holds a query token, so its judgments are complete and Judged@10 must be
  100%.

**Context and localization.**
- RepoBench (https://arxiv.org/abs/2306.03091).
- CrossCodeEval (https://arxiv.org/abs/2310.11248). The need for cross-file context is proven by compiler
  undefined-name errors.
- ContextBench (https://arxiv.org/html/2602.05892v1). Gold context is traced by hand at file, block and line
  granularity, and scored by recall, precision and F1. Its languages are the same eight families as codectx's.
- LocAgent / Loc-Bench (https://arxiv.org/abs/2503.09089). Acc@k counts a case only when every gold location is in
  the top k. This is the AND rule of the design's context cases.
- CodeRAG-Bench (https://arxiv.org/abs/2406.14497, abstract only).

**Slicing and data flow.**
- *ORBS and the Limits of Static Slicing* (SCAM 2015; https://discovery.ucl.ac.uk/1470412/1/scam15.pdf).
  Observation-based slices are a one-sided oracle for slicer soundness.
- The OWASP Benchmark keeps its expected results in a separate CSV
  (https://raw.githubusercontent.com/OWASP-Benchmark/BenchmarkJava/master/expectedresults-1.2.csv) and scores by the
  Youden index.
- The Juliet C/C++ suite encodes ground truth in good and bad function variants
  (https://samate.nist.gov/SRD/resources/Juliet_Test_Suite_v1.2_for_C_Cpp_-_User_Guide.pdf).
- DroidBench's ground truth is prose (https://github.com/secure-software-engineering/DroidBench), which is the
  weakness the design avoids.

**Manifests and lock files.**
- Yu, Song, Hu, *On the Correctness of Metadata-based SBOM Generation* (DSN 2024;
  https://cs.ucr.edu/~heng/pubs/sbom-dsn24.pdf). Generators disagree with each other and omit dependencies, and
  some that claim to read `package.json` extract nothing from it.
- *A Reality Check on SBOM-based Vulnerability Management* (https://arxiv.org/abs/2511.20313). Lock files give
  accurate and consistent results.

**Routes.**
- Respector (*Generating REST API Specifications through Static Analysis*, ICSE 2024;
  https://par.nsf.gov/servlets/purl/10537946). Several authors analyse each API independently, reconcile their
  findings, and measure precision and recall per entity kind. Developer-written specifications were incomplete, so
  they are no oracle on their own.
- Lercher et al. (https://arxiv.org/abs/2410.23873): a hand-built ground truth for Spring Boot endpoints.

**Incremental correctness.**
- Szabó, Erdweg, Bergmann (PLDI 2021;
  https://www.pl.informatik.uni-mainz.de/files/2021/04/inca-whole-program.pdf). An incremental analysis "must yield
  the exact same results as a non-incremental analysis would".
- The reproducible-builds definition (https://reproducible-builds.org/docs/definition/).
- *Detecting Call Graph Unsoundness without Ground Truth* (https://arxiv.org/abs/2604.00885): metamorphic partial
  orders.

## 6. Contamination

- **SWE-bench** (https://arxiv.org/abs/2310.06770) and its human-validated Verified subset
  (https://huggingface.co/datasets/princeton-nlp/SWE-bench_Verified). The announcement post returned 403 and was
  not fetched.
  - SWE-Bench+ (https://arxiv.org/abs/2410.06992) found solutions leaked in the issue text.
  - *The SWE-Bench Illusion* (https://arxiv.org/abs/2506.12286) found models naming buggy files from the issue text
    alone.
- **Contamination studies.** Riddell, Ni, Cohan (https://arxiv.org/abs/2403.04811) and LiveCodeBench
  (https://arxiv.org/abs/2403.07974) show that scores rise on material a model has seen. For a fixed set, the answer is held-out
  cases and renamed copies (section 10).

## 7. Metrics, agreement and abstention

- **Cohen's κ**: https://scikit-learn.org/stable/modules/generated/sklearn.metrics.cohen_kappa_score.html
- **Selective classification** (Geifman and El-Yaniv, https://arxiv.org/abs/1705.08500): trading coverage for risk.
- **SimpleQA** (https://arxiv.org/abs/2411.04368): correct, incorrect and not attempted, reported apart.
- **Kalai, Nachum, Vempala, Zhang**, *Why Language Models Hallucinate* (https://arxiv.org/abs/2509.04664): "I don't
  know" scores 0, and a wrong answer scores −t/(1−t). At t = ½ this is the design's abstention-aware score.
- **No prior call-graph benchmark credits an honest "unresolved".**
  - PyCG keeps an edge to an unavailable function as a leaf.
  - gopls leaves dynamic calls out.
  - Judge counts every missing edge as Unsound.

  Crediting a disclosed open site above a silent miss is this design's own addition.

## 8. Licensing of real-repository excerpts

These are the primary texts, not legal advice.
- **Apache-2.0 §4** (https://www.apache.org/licenses/LICENSE-2.0.txt). Copies must carry the licence, notices on
  modified files, the retained attribution notices, and the upstream NOTICE text.
- **MIT** (https://opensource.org/license/mit). The copyright and permission notice must go with every substantial
  portion. SWE-bench is MIT (https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/LICENSE), but its task text
  and patches come from upstream repositories and carry those licences too.
- **elasticsearch** (https://raw.githubusercontent.com/elastic/elasticsearch/main/LICENSE.txt) is triple-licensed
  AGPL-3.0, SSPL-1 and Elastic License 2.0, and x-pack is ELv2 only.
  - ELv2 (https://www.elastic.co/licensing/elastic-license) requires every recipient of a copy to get its terms.
  - The SSPL (https://www.mongodb.com/legal/licensing/server-side-public-license) attaches service source
    obligations.
  - The ASF lists AGPL and SSPL as Category X, not to be included in Apache products
    (https://www.apache.org/legal/resolved.html).
- **What the design concludes.** The golden set is original code. Real repositories stay out of the repository.

## 9. Language specifications cited by the design's cases

- **Go**: https://go.dev/ref/spec (`#Selectors`, `#Struct_types`, `#Method_sets`, `#Interface_types`,
  `#Implementing_an_interface`, `#Method_values`, `#Method_expressions`, `#Instantiations`, `#Type_switches`,
  `#Package_initialization`, `#Import_declarations`); https://pkg.go.dev/cmd/go#hdr-Build_constraints;
  https://go.dev/ref/mod#workspaces; https://go.dev/ref/mod#vendoring
- **Python**: https://docs.python.org/3/howto/mro.html; https://docs.python.org/3/library/functions.html#super;
  https://docs.python.org/3/reference/import.html; https://docs.python.org/3/reference/datamodel.html;
  https://peps.python.org/pep-0544/; https://peps.python.org/pep-0562/; https://peps.python.org/pep-0698/;
  https://docs.python.org/3/library/functools.html#functools.wraps;
  https://docs.python.org/3/library/importlib.html#importlib.import_module
- **Java SE 21**: https://docs.oracle.com/javase/specs/jls/se21/html/ (§7.5, §8.4.2, §8.4.8.1, §8.4.8.2, §8.9,
  §8.10, §9.4.1, §12.1.4, §15.9.5, §15.12 including §15.12.4.4, and §15.13)
- **C++**: https://eel.is/c++draft/ (`class.virtual`, `class.member.lookup`, `class.mi`, `class.cdtor`,
  `expr.call`, `temp.dep`, `basic.lookup.argdep`, `namespace.udecl`, `expr.mptr.oper`)
- **C**: ISO/IEC 9899 draft N3220, https://www.open-std.org/jtc1/sc22/wg14/www/docs/n3220.pdf. Its sections on
  linkage, function calls, generic selection and conditional inclusion are cited by number when the cases are
  written.
- **Rust**: https://doc.rust-lang.org/reference/expressions/method-call-expr.html;
  https://doc.rust-lang.org/reference/items/traits.html;
  https://doc.rust-lang.org/reference/items/implementations.html#trait-implementation-coherence;
  https://doc.rust-lang.org/reference/paths.html#qualified-paths;
  https://doc.rust-lang.org/reference/items/use-declarations.html;
  https://doc.rust-lang.org/reference/macros-by-example.html;
  https://doc.rust-lang.org/reference/conditional-compilation.html;
  https://doc.rust-lang.org/cargo/reference/specifying-dependencies.html#renaming-dependencies-in-cargotoml
- **ECMAScript**: https://tc39.es/ecma262/ (`#sec-resolveexport`, `#sec-exports`, `#sec-super-keyword`,
  `#sec-function.prototype.bind`)
- **Node.js**: https://nodejs.org/api/modules.html#exports-shortcut;
  https://nodejs.org/api/esm.html#interoperability-with-commonjs;
  https://nodejs.org/api/packages.html#conditional-exports; https://nodejs.org/api/packages.html#subpath-imports
- **TypeScript**: https://www.typescriptlang.org/docs/handbook/modules/reference.html;
  https://www.typescriptlang.org/docs/handbook/declaration-merging.html;
  https://www.typescriptlang.org/docs/handbook/mixins.html;
  https://www.typescriptlang.org/docs/handbook/2/classes.html;
  https://www.typescriptlang.org/docs/handbook/2/functions.html#function-overloads;
  https://www.typescriptlang.org/docs/handbook/jsx.html

## 10. Overfitting to a benchmark, and the guardrails against it

Fetched on 2026-10-13. Quotes are from the primary text unless marked.

**A measure that becomes a target.**
- Strathern, "'Improving ratings': audit in the British University system", *European Review* 5(3), 1997, p. 308
  (https://cambridge.org/core/journals/european-review/article/improving-ratings-audit-in-the-british-university-system/FC2EE640C0C44E3DB87C29FB666E9AAB;
  scan read at https://www.gwern.net/doc/statistics/decision/1997-strathern.pdf): "When a measure becomes a target,
  it ceases to be a good measure." This is the usual wording of Goodhart's law. Goodhart's own 1975 text was not
  found.
- Campbell, "Assessing the Impact of Planned Social Change", 1976
  (https://jmde.journals.publicknowledgeproject.org/index.php/jmde_1/article/download/297/292/988): "The more any
  quantitative social indicator is used for social decisionmaking, the more subject it will be to corruption
  pressures".
- Manheim and Garrabrant, "Categorizing Variants of Goodhart's Law" (https://arxiv.org/abs/1803.04585), and Thomas
  and Uminsky (https://arxiv.org/abs/2002.08512), who propose "using a slate of metrics to get a fuller and more
  nuanced picture". The design's report therefore has no single score.

**Held-out data and how often it can be looked at.**
- Dwork, Feldman, Hardt, Pitassi, Reingold, Roth, "Generalization in Adaptive Data Analysis and Holdout Reuse"
  (https://arxiv.org/abs/1506.02629): "Because the predictor is independent of the holdout dataset, such an estimate
  is a valid estimate of the true prediction accuracy ... However, in practice the holdout dataset is rarely used
  only once, and as a result the predictor may not be independent of the holdout set, resulting in overfitting to
  the holdout set". On data with no signal, reusing a standard holdout reported "accuracy of over 63% for k = 500".
- Blum and Hardt, "The Ladder" (https://arxiv.org/abs/1502.04585): participants "may begin to overfit to the holdout
  data that supports the leaderboard". Hardt's post (http://blog.mrtz.org/2015/03/09/competition.html): "the public
  score is in general no longer an unbiased estimate of the true score".
- Roelofs et al., "A Meta-Analysis of Overfitting in Machine Learning", NeurIPS 2019
  (https://papers.nips.cc/paper/9117-a-meta-analysis-of-overfitting-in-machine-learning): competitions rank on "a
  separate test set used only once", and found "little evidence of substantial overfitting" from the public
  leaderboard.
- Recht, Roelofs, Schmidt, Shankar, "Do ImageNet Classifiers Generalize to ImageNet?", ICML 2019
  (https://proceedings.mlr.press/v97/recht19a.html): accuracy drops of "11% - 14% on ImageNet" on a new test set,
  "not caused by adaptivity, but by the models' inability to generalize to slightly "harder" images".
- ARC Prize 2024 Technical Report (https://arxiv.org/html/2412.04604v2): public, semi-private and private evaluation
  sets; "We consider scores overfit if semi-private and public evaluation set scores exceed ±10% absolute
  difference"; and "on the order of 10,000 private evaluation set scores have been reported to participants so far
  ... This presents a significant risk of overfitting."

  **For the design:** run 2 is one look at held-out cases by a product built without them, the case the holdout
  papers call valid, so its open-against-held-out gap is the headline. Once the held-out cases are fixed against,
  they no longer measure generalisation, so run 3 is a regression run whose unseen part is a fresh renaming.

**Benchmarks people tune to.**
- SWE-Bench+ (https://arxiv.org/abs/2410.06992): "31.08% of the passed patches are suspicious patches due to weak
  test cases". Weak checks let wrong output pass, hence closed-world expectations with prohibited rows.
- *The SWE-Bench Illusion* (https://arxiv.org/abs/2506.12286): buggy-file identification "up to 76%" on the
  benchmark's repositories against "up to 53% on tasks from repositories not included". A drop on unseen material is
  the signature of memorisation.
- SWE-bench-Live (https://arxiv.org/abs/2505.23419), SWE-rebench (https://arxiv.org/abs/2505.20411) and LiveCodeBench
  (https://arxiv.org/abs/2403.07974) answer it with fresh tasks over time; the survey of Xu et al.
  (https://arxiv.org/html/2406.04244) lists private, dynamic and restructured benchmarks as the mitigations.
- NIST SATE V, SP 500-326 (https://nvlpubs.nist.gov/nistpubs/SpecialPublications/NIST.SP.500-326.pdf): "Synthetic
  test cases are much smaller and less complex than production software ... tool results ... on synthetic test cases
  may differ from results on production software", and "recall was higher for the Synthetic test cases". This is
  why the golden set never certifies scale, and the separate proofs on large real repositories stay the
  counterweight.
- SV-COMP 2021 (https://www.sosy-lab.org/research/pub/2021-TACAS.Software_Verification_10th_Comparative_Evaluation_SV-COMP_2021.pdf):
  "all verification tasks are known before the submission of the participating verifiers". An open set measures
  capability, not generalisation.

**Renamed and transformed copies.**
- Chen et al., "Metamorphic Testing: A Review of Challenges and Opportunities", ACM CSUR 2018
  (https://i.cs.hku.hk/~tse/Papers/2010s/hlmtCSUR.html): metamorphic relations "are necessary properties of the
  target function or algorithm".
- Le, Afshari, Su, "Compiler Validation via Equivalence Modulo Inputs", PLDI 2014
  (https://web.cs.ucdavis.edu/~su/publications/emi.pdf): "EMI can be adapted to validate program transformation and
  analysis systems in general."
- Statfier, ESEC/FSE 2023
  (https://2023.esec-fse.org/details/fse-2023-research-papers/31/Statfier-Automated-Testing-of-Static-Analyzers-via-Semantic-preserving-Program-Trans):
  static analyzers tested by comparing results on a program and on a semantics-preserving variant.
- Ramakrishnan et al., "Semantic Robustness of Models of Source Code" (https://arxiv.org/abs/2002.03043), with
  transforms including "RenameLocalVariables", "RenameParmeters" and "RenameFields"; ReCode
  (https://arxiv.org/abs/2212.10264) perturbs function and variable names. Both test learned models; applying
  renaming to a deterministic analyzer, whose answers must change only by the renaming, is the design's own step.

**Fixes that fit the tests.**
- Smith, Barr, Le Goues, Brun, "Is the Cure Worse Than the Disease? Overfitting in Automated Program Repair", FSE
  2015 (https://people.cs.umass.edu/~brun/pubs/pubs/Smith15fse.pdf): "Patches that are overly specific to the
  training tests and fail to generalize to the held-out tests overfit to the training tests", and "novice developers
  also overfit".
- Qi, Long, Achour, Rinard, ISSTA 2015 (https://www.cs.toronto.edu/~fanl/papers/kali-issta2015.pdf): of plausible
  patches, "GenProg produced a correct patch for only 2 of the 105 considered defects"; the authors "developed new
  test cases that exposed the defects". Hence a unit test on a different example for every fix.
- Meszaros, *xUnit Test Patterns*, "Test Logic in Production"
  (http://xunitpatterns.com/Test%20Logic%20in%20Production.html): "A system that behaves one way in the test lab and
  an entirely different way in production is a recipe for disaster!" Hence the check that product code never names
  the set.
- Google Engineering Practices, "What to look for in a code review"
  (https://google.github.io/eng-practices/review/reviewer/looking-for.html): "Will the tests actually fail when the
  code is broken?" Hence a fix's test must fail with the fix reverted.

## 11. What to return for a call with several possible targets

Fetched on 2026-10-13, from source code, specifications and papers. Behaviour read from code but not from a test is
marked as inferred.

**What systems return.**
- **CodeQL, JavaScript** (https://raw.githubusercontent.com/github/codeql/main/javascript/ql/lib/semmle/javascript/dataflow/Nodes.qll):
  `getACallee()` "Gets a potential callee of this call site", with an `imprecision` that "is a heuristic measure of
  how likely it is that `callee` is only suggested as a potential callee due to imprecise analysis of global
  variables"; `isIndefinite(cause)` names the causes `"await", "call", "eval", "global", "heap", "import",
  "namespace", "yield"` (DataFlow.qll). `fn.call(x, y)` "gets a call node with `fn` as the callee, `x` as the
  receiver"; `bind` is a partial call whose result refers to the underlying function; `Object.assign` is an extend
  step at imprecision 0 (Extend.qll, CallGraphs.qll).
- **CodeQL, C and C++** (https://raw.githubusercontent.com/github/codeql/main/cpp/ql/lib/semmle/code/cpp/exprs/Call.qll):
  `getTarget()` is "the statically resolved method"; `isOnlyFoundByADL()` "Holds if the target of this function call
  was found by argument-dependent lookup". `getAViableTarget` gives a virtual call's possible targets
  (dispatch/VirtualDispatchPrototype.qll). A macro invocation links to its macro (`MacroInvocation.getMacro()`) and
  the expanded call stays an ordinary call marked `isInMacroExpansion`
  (https://codeql.github.com/codeql-standard-libraries/cpp/semmle/code/cpp/Macro.qll/type.Macro$MacroInvocation.html).
  `C11GenericExpr` keeps every association and the selected one (exprs/Expr.qll).
- **CodeQL, Java and C#**: `exactVirtualMethod` returns one method only "if we can establish one", and
  `lowConfidenceDispatchTarget` flags "imprecise open-world type-based dispatch"
  (https://raw.githubusercontent.com/github/codeql/main/java/ql/lib/semmle/code/java/dispatch/VirtualDispatch.qll);
  C# keeps `getTarget()` (static) apart from `getARuntimeTarget()` (potential run-time targets)
  (https://raw.githubusercontent.com/github/codeql/main/csharp/ql/lib/semmle/code/csharp/exprs/Call.qll).
- **SCIP** (https://raw.githubusercontent.com/sourcegraph/scip/main/scip.proto): one symbol per occurrence and no
  certainty field; several targets go through symbol relationships (`is_reference`, `is_implementation`,
  `is_definition`, the last suggested for mixins).
- **Kythe** (https://kythe.io/docs/schema/): `ref/call`, `ref/call/direct` ("ignores indirect dispatch"),
  `ref/call/implicit`, and `ref/expands` / `ref/expands/transitive` for macros; a `lookup` node records "a structured
  name whose resolution cannot be completed without additional context", as for dependent names in C++ templates.
- **Glean** (https://raw.githubusercontent.com/facebookincubator/Glean/main/glean/schema/source/cxx.angle):
  `XRefIndirectTarget { via : XRefVia, target }`, "Cross-reference that goes through something (macro or using)".
- **Sourcegraph** (https://raw.githubusercontent.com/sourcegraph/sourcegraph-public-snapshot/main/cmd/frontend/graphqlbackend/codeintel.codenav.graphql):
  each usage carries `provenance` `PRECISE` ("doesn't have false positives"), `SYNTACTIC` or `SEARCH_BASED`, and
  "Results are first grouped by provenance in the order precise, syntactic, then search-based."
- **LSP 3.17** (https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/language/definition.md):
  definition returns "`Location` | `Location`[] | `LocationLink`[] | `null`", with no certainty field; `SymbolTag`
  has only `Deprecated`.
- **rust-analyzer** (https://raw.githubusercontent.com/rust-lang/rust-analyzer/master/crates/ide/src/goto_definition.rs):
  a method call on a concrete type goes to the impl, on a generic `T: Trait` to the trait declaration; a call written
  inside a macro resolves through the expansion. `Default::default()` with an inferred type resolves to the impl
  through `lookup_impl_method` (inferred from crates/hir/src/source_analyzer.rs).
- **clangd** (https://raw.githubusercontent.com/llvm/llvm-project/main/clang-tools-extra/clangd/XRefs.cpp): a macro
  goes to its `#define`; a pure virtual method jumps "to all overrides"; the textual fallback is ranked, and "If we
  have more than 5 results, don't return anything, as confidence is too low." Its tests treat a resolved ADL call as
  an exact reference and leave a template-dependent one unresolved (unittests/XRefsTests.cpp).
- **TypeScript language service** (https://raw.githubusercontent.com/microsoft/TypeScript/v5.9.3/src/services/goToDefinition.ts):
  an array of definitions, the resolved signature first; fallback results carry `unverified`. `strictBindCallApply`
  types `bind`, `call` and `apply` from the function
  (https://www.typescriptlang.org/docs/handbook/release-notes/typescript-3-2.html).
- **Python analyzers on `getattr`**: Jedi resolves a constant name and returns nothing for a computed one
  (https://raw.githubusercontent.com/davidhalter/jedi/master/jedi/plugins/stdlib.py); typeshed declares the result
  `Any` (https://raw.githubusercontent.com/python/typeshed/main/stdlib/builtins.pyi).

**What the literature says.**
- Møller and Schwartzbach, *Static Program Analysis* (https://cs.au.dk/~amoeller/spa/spa.pdf): a may analysis
  "computes an over-approximation"; for call graphs "we usually aim for an over-approximation, meaning that call
  graphs may contain too many call edges but never too few."
- Livshits et al., "In Defense of Soundiness: A Manifesto", CACM 2015 (https://yanniss.github.io/Soundiness-CACM.pdf):
  "We issue a call to the community to identify clearly the nature and extent of unsoundness in static analyses",
  and an analysis "may pretend that eval does nothing, unless it can precisely resolve its string argument at
  compile time."
- Feldthaus, Schäfer, Sridharan, Dolby, Tip, ICSE 2013 (https://www.franktip.org/pubs/icse2013approximate.pdf): "for
  Jump to Declaration, listing many spurious call targets is perhaps even worse than occasionally missing one"; a
  useful compromise indicates "call sites where some callees may be missing"; "reflective invocations using call and
  apply must also be accounted for."
- PyCG, ICSE 2021 (https://arxiv.org/pdf/2103.00587): "precision (∼99.2%) and adequate recall (∼69.9%)", and "the
  effects of functions such as getattr and setattr are ignored."
- Antal et al., SCAM 2018 (https://arxiv.org/pdf/2405.07206v1): static JavaScript call graphs miss "dynamic call
  edges coming from the non-trivial usages of eval(), bind(), or apply()".
- Nielsen, Torp, Møller, ISSTA 2021 (https://cs.au.dk/~amoeller/papers/jam/paper.pdf): precision as "the percentage
  of call sites that have a unique callee", and an explicit list of the sources of unsoundness.
- Chakraborty, Gnanakumar, Sridharan, Møller, ECOOP 2024
  (https://drops.dagstuhl.de/storage/00lipics/lipics-vol313-ecoop2024/LIPIcs.ECOOP.2024.10/LIPIcs.ECOOP.2024.10.pdf):
  per-call-site precision, "the percentage of true ... call targets among those predicted".

**What the consumers tolerate.**
- Sadowski et al., CACM 2018 (https://storage.googleapis.com/gweb-research2023-media/pubtools/4365.pdf): "Produce
  less than 10% effective false positives."
- Christakis and Bird, ASE 2016 (https://mariachris.github.io/Pubs/ASE-2016.pdf): "Developers care much more about
  too many false positives than about too many false negatives", a false-positive rate "no higher than 15–20%", and
  "55% of developers preferred to get false positives when they are easy to sift through."
- Liu et al., "Lost in the Middle" (https://arxiv.org/pdf/2307.03172), and Shi et al., ICML 2023
  (https://arxiv.org/pdf/2302.00093): "the model performance is dramatically decreased when irrelevant information is
  included."
- SWE-agent, NeurIPS 2024 (https://arxiv.org/pdf/2405.15793): "Environment feedback should be informative but
  concise"; with many search results agents "look through every match exhaustively", doing worse than without the
  search tool.
- Sun et al., "Tools Fail" (https://arxiv.org/pdf/2406.19228): models "overtrust tools"; naming the failure modes
  helps, while "giving the raw confidence scores is not as helpful". These are not code tools, so applying it here is
  an inference.

**For the design (7.8.1).** One target when the compiler would pick one; a closed set listed whole and marked
possible; a name match as an open site with a named reason and ranked candidates after every precise row; nothing
for a computed name. clangd drops a long heuristic list; codectx's exact total with a ranked first page keeps the
answer complete without flooding the reader. The proposed `dependent` reason follows Kythe's `lookup` node and
clangd's treatment of dependent names.
