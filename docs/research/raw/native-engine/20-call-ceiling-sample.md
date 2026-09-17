# Native-engine research, raw evidence — the honest denominator for `calls`, and the resolution ceiling per repository class

Two evidence stores were read with `sqlite3 -readonly` **only**; the product was never run against
either repository, and both source clones were read without being modified. Every figure is scoped
through the active generation and its unit set.

## 0. Why this file exists

The published call-resolution figures use **every call site** as the denominator, including calls
whose target is a library, the platform or the language runtime — for which "no in-repo definition"
is the correct answer and not a miss. This file measures the denominator nobody had measured: the
share of call sites whose callee **is defined in the repository**. Everything downstream — what the
producers resolve today, what the reachable ceiling is, and what the plan may promise — is restated
against that denominator, **per repository class**, never per repository.

## 1. The two corpora, and the class each one instantiates

| | reference repository | second corpus |
|---|---|---|
| class | **(b)** dynamic/unconfigured: no project configuration for its majority language | **(a)** configured typed: a project configuration present and a precise indexer run over the whole repository |
| files in snapshot | 13,222 (6,663 parsed) | 6,270 (3,812 parsed) |
| majority language by call sites | JavaScript 526,393 (94.8%) | Python 123,029 (90.7%) |
| other languages by call sites | java 26,416 · python 2,291 · typescript 488 | tsx 7,300 · typescript 3,634 · go 1,101 · java 362 · javascript 233 · c 54 · rust 1 |
| tree-sitter `calls` sites / edges | 555,588 / 363,750 | 135,714 / 108,153 |
| syntax tier resolved | 75,751 (13.6%) | 39,526 (29.1%) |
| engine (`dependence`) `calls` sites | 217,881, of which 40,743 (18.7%) in-repo | 168,376, of which 33,990 (20.2%) in-repo |
| precise units in the active generation | 1 (17 files of one language) | 1, **whole-repository**, 2,990 documents |

The second corpus is what a *configured* repository looks like in the store, and it is the only
place the Section 11.3 call-site join can be measured at scale. The reference repository is what an
*unconfigured* repository looks like. Neither is the definition of a target; each is an instance of
a class.

## 2. The instruments

**Instrument 1 — the compiler index, where one exists (second corpus).** The Section 11.3 join keys
a tree-sitter call site and a precise occurrence on the identical `callsite:<path>:<start>-<end>`
native alias. Both providers publish that alias, so `native_aliases` answers, per call site and
without a human, three questions: is there a compiler-precision occurrence at the callee identifier
(the join), and does the symbol that occurrence names have a **definition** occurrence inside a
repository document (in-repo) or none (external). All 2,990 documents and all 143,369 definition
occurrences of the second corpus resolve to files present in the snapshot, so "has a definition
occurrence" is exactly "defined in this repository".

**Instrument 2 — the hand sample.** Where no compiler index covers the site, the callee is
classified by reading the source at the call site in a read-only clone of the corpus at the exact
commit the snapshot records (`snapshots.head_object_id`, working tree clean in both clones).

**Instrument 3 — the engine verdict (reference repository).** A tree-sitter call site is matched to
an engine `calls` site on exact `(file, start_byte, end_byte)` equality, falling back to an
overlapping range with the same callee name. 129,576 of the engine's 203,896 distinct call-site
ranges match a tree-sitter range exactly (63.6%); a further 26,577 tree-sitter sites overlap an
engine range without equality. **Method limitation:** a sampled site whose engine verdict is
`absent` may be a site the engine lowered to a different range rather than one it did not resolve,
so the engine share measured this way is a lower bound.

## 3. The draw

Deterministic and reproducible: inside each stratum the rows are sorted by `(path, start, end)` and
`random.Random(seed).sample` picks the indices.

| corpus | seed | strata and allocation |
|---|---|---|
| reference repository | **20260916** | javascript 300 of 526,393 · java 90 of 26,416 · python 45 of 2,291 · typescript 25 of 488 = **460 rows** |
| second corpus | **20260917** | unjoined-in-an-indexed-file 90 of 8,091 · unjoined-file-not-indexed 30 of 2,384 · joined-defined 12 of 48,302 · joined-external 12 of 64,252 · TSX/TypeScript 40 of 10,934 · Go/Java/JavaScript/C/Rust 20 of 1,751 = **204 rows** |

Allocation is **disproportionate on purpose**: the overall share is recovered with the stratified
estimator (each stratum weighted by its population share), which is what lets the small strata carry
a per-language statement without distorting the total. All 664 drawn rows passed the offset check —
the callee name recorded in the store appears inside the bytes the range names in the clone — so the
byte positions and the clone agree.

## 4. Classification

| code | meaning |
|---|---|
| `a-repo` | the callee is **defined in this repository** — a function, method or class in a tracked source file |
| `b-lib` | the callee belongs to a **third-party library or framework** the repository depends on |
| `c-platform` | the callee is **platform or runtime**: a built-in, a standard-library function, a DOM or host API |
| `d-unknown` | **undeterminable without executing**: a callee whose identity depends on runtime values, dynamic dispatch on data, or configuration not present in the tree |

`a-repo` is the honest denominator. `b-lib` and `c-platform` are calls for which "no in-repo
definition" is the **correct** answer; counting them in the denominator is what produced the
published 18.7%.

## 5. The sample rows

Columns: `syntax` is the tree-sitter tier's own verdict (`in-file`, `import`, `ambiguous`,
`unresolved`); `engine` is instrument 3; `precise` is instrument 1 (`joined-defined`,
`joined-external`, `unjoined-in-indexed-file`, `unjoined-file-not-indexed`).

### A. Reference repository — JavaScript, first half

| id | file:line | call expression | callee | syntax | engine | class | reason |
|---|---|---|---|---|---|---|---|
| C1-091 | `Meteor3preUpgradeScripts/meteor-async-migration/11-propagate-async-imports.js`:518 | `root.find(j.ImportDeclaration) .forEach((imp) => { let source = imp.value.source && imp…` | `forEach` | unresolved | stub | b-lib | jscodeshift Collection.forEach on a find() result |
| C1-092 | `Meteor3preUpgradeScripts/meteor-async-migration/15-numeral-to-numbro.js`:99 | `j(p).remove()` | `remove` | unresolved | stub | b-lib | jscodeshift Collection.remove on j(p) |
| C1-093 | `Meteor3preUpgradeScripts/meteor-async-migration/4-baseline-app-fixes.anchorcheck.js`:10 | `require("path")` | `require` | unresolved | stub | c-platform | Node module loader require() |
| C1-094 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/agent-rank1-forms.spec.js`:1157 | `(q(`print(db.businessObjects.find({"md.type":"${QE_TYPE}","info.owner.subID":"${F.SUB}"…` | `split` | unresolved | absent | c-platform | String.prototype.split on a string expression |
| C1-095 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/agent-rank2-settings.spec.js`:627 | `JSON.stringify(getPath(repaired, "ofraSubTypes"))` | `stringify` | unresolved | absent | c-platform | JSON.stringify built-in |
| C1-096 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/agent-settings-editors.spec.js`:1257 | `expect(rows.filter((r) => !r.ok).map((r) => r.k), "every editor test reached its end as…` | `toEqual` | unresolved | absent | b-lib | Playwright expect() matcher |
| C1-097 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/agent-surveys-controls.spec.js`:664 | `page.locator("#nextSection").first().click({ timeout : 10000 })` | `click` | unresolved | absent | b-lib | Playwright Locator.click |
| C1-098 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/bo-type.par.spec.js`:581 | `Object.keys(window.SlickGrid \|\| {}).some((k) => { const g = window.SlickGrid[k] && wi…` | `some` | unresolved | absent | c-platform | Array.prototype.some on an Object.keys result |
| C1-099 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/file-surface.spec.js`:348 | `B.mongo(`print(db.qExports.countDocuments({subID:"${sub}"}))`)` | `mongo` | unresolved | absent | a-repo | T-import: B required from a repo test lib module |
| C1-100 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/boForm.js`:2909 | `shadow.getAttribute("aria-required")` | `getAttribute` | unresolved | absent | c-platform | DOM Element.getAttribute |
| C1-101 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/parallel.js`:181 | `use(await ctx.page())` | `use` | unresolved | absent | d-unknown | Playwright fixture callback parameter invoked |
| C1-102 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/timing.js`:14 | `new Date().toTimeString()` | `toTimeString` | unresolved | absent | c-platform | Date.prototype.toTimeString |
| C1-103 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/manage-scoring.spec.js`:96 | `test.skip(!(await btn.count()), "this tenant's scoring page offers no Score Tuning card…` | `skip` | unresolved | absent | b-lib | Playwright test.skip |
| C1-104 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/manage-subscriptions.spec.js`:219 | `/no activity/i.test((document.querySelector(".panel-body.tabContent") \|\| {}).innerTex…` | `test` | unresolved | absent | c-platform | RegExp.prototype.test on a regex literal |
| C1-105 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/pages-clean.spec.js`:67 | `push("Meteor._debug", a.map(String).join(" "))` | `push` | in-file | absent | a-repo | T-none: const push arrow defined in this file |
| C1-106 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/probe-run4-defects.js`:101 | `h.className.slice(0, 60)` | `slice` | unresolved | absent | c-platform | String.prototype.slice on className |
| C1-107 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/probes/fp-grid-columns-probe.js`:22 | `page.fill("#password", SWEEPSEC.loginPassword(), { timeout : 8000 })` | `fill` | unresolved | absent | b-lib | Playwright Page.fill |
| C1-108 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/roles-crud.spec.js`:109 | `new Event("change", { bubbles : true })` | `Event` | unresolved | absent | c-platform | DOM Event constructor |
| C1-109 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/roles-crud.spec.js`:477 | `String(out)` | `String` | unresolved | absent | c-platform | String() built-in conversion |
| C1-110 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/setup/clone-test-users.js`:35 | `SEC.passwordBcrypt()` | `passwordBcrypt` | unresolved | absent | a-repo | T-import: SEC required from a repo test lib module |
| C1-111 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/system-surface.spec.js`:98 | `c.close()` | `close` | unresolved | absent | b-lib | Playwright BrowserContext.close |
| C1-112 | `Meteor3preUpgradeScripts/meteor-async-migration/vp-codemods/6-password-method-hooks.js`:156 | `out.replace(`${a}\n`, "")` | `replace` | unresolved | stub | c-platform | String.prototype.replace on a source string |
| C1-113 | `QA/SeleniumWebdriver/TestngExtentFramework/doc/script-dir/jquery-3.5.1.min.js`:2 | `t()` | `t` | in-file | absent | a-repo | T-none: vendored jQuery bundle-internal helper |
| C1-114 | `QA/SeleniumWebdriver/TestngExtentFramework/doc/script-dir/jquery-ui.min.js`:6 | `this._delay(function(){var i=!t.contains(this.element[0],t.ui.safeActiveElement(this.do…` | `_delay` | unresolved | absent | a-repo | T-hier: vendored jQuery UI widget prototype method |
| C1-115 | `QA/SeleniumWebdriver/TestngExtentFramework/doc/script.js`:87 | `selected.previousSibling.click()` | `click` | unresolved | stub | c-platform | DOM HTMLElement.click via previousSibling |
| C1-116 | `app/both/definitions/sbomTypes.test.js`:12 | `expect(sbomTypeOptions).toHaveLength(2)` | `toHaveLength` | unresolved | absent | b-lib | vitest expect() matcher |
| C1-117 | `app/both/schemas/gridViews.js`:138 | `autoVal(this)` | `autoVal` | unresolved | stub | a-repo | T-field: Meteor global assigned as this.autoVal elsewhere |
| C1-118 | `app/both/schemas/manageFlowEditSchema.js`:81 | `_.chain(fortress)` | `chain` | unresolved | stub | b-lib | lodash chain() |
| C1-119 | `app/both/utils/dynamicOfraSchema.js`:419 | `Meteor.user()` | `user` | unresolved | absent | b-lib | Meteor.user global API |
| C1-120 | `app/both/utils/markdownUtils.test.js`:30 | `expect(result).toContain("&lt;")` | `toContain` | unresolved | absent | b-lib | vitest expect() matcher |
| C1-121 | `app/both/utils/quickEntryUtils.js`:1755 | `tpl.get("data")` | `get` | in-file | in-repo | a-repo | T-none: engine resolves it in-repo; syntax says in-file |
| C1-122 | `app/both/utils/recalcAIMScore.test.js`:107 | `shouldAIMRescore(modifier, oldRecord, SUB_ID)` | `shouldAIMRescore` | in-file | absent | a-repo | T-none: syntax tier resolves it in-file |
| C1-123 | `app/client/components/actionButtons/bulkActions/bulkActions.js`:492 | `_.result(self, "totalCount.get")` | `result` | unresolved | absent | b-lib | lodash result() |
| C1-124 | `app/client/components/activityStream/activityMergeHsitory.js`:19 | `new Switchery(elem)` | `Switchery` | unresolved | absent | b-lib | Switchery third-party toggle plugin constructor |
| C1-125 | `app/client/components/customAutoFormInputs/text-expandable.js`:42 | `autosize($(".expandableTextarea textarea"))` | `autosize` | unresolved | absent | b-lib | autosize third-party package |
| C1-126 | `app/client/components/slickGrid/boColumnDefinitions.js`:131 | `_.find(value, { label : item })` | `find` | unresolved | absent | b-lib | lodash find() |
| C1-127 | `app/client/components/slickGrid/slickGrid.js`:792 | `filters.entries()` | `entries` | unresolved | absent | c-platform | Array.prototype.entries, guarded by Array.isArray |
| C1-128 | `app/client/components/slickGrid/slickGrid.js`:2068 | `grid.getCanvasNode()` | `getCanvasNode` | unresolved | absent | a-repo | T-flow: vendored SlickGrid grid method |
| C1-129 | `app/client/components/slickGrid/slickGrid.js`:4376 | `$(grid.getCanvasNode())` | `$` | unresolved | absent | b-lib | jQuery factory call |
| C1-130 | `app/client/components/workflow/simpleWorkflow.js`:38 | `$(document).on("click", function(evt) { popoverOffClick(evt); })` | `on` | unresolved | absent | b-lib | jQuery on() on a wrapped document |
| C1-131 | `app/client/components/workflow/workflow.js`:134 | `Template.instance()` | `instance` | unresolved | absent | b-lib | Blaze Template.instance |
| C1-132 | `app/client/imports/amcharts/amcharts.js`:188 | `n.click(function(a){h.handleGraphEvent(a,"clickGraph")})` | `click` | unresolved | absent | a-repo | T-flow: vendored AmCharts set object method |
| C1-133 | `app/client/imports/amcharts/amcharts.js`:190 | `d.setCN(f,n,this.bcn+"stroke")` | `setCN` | unresolved | absent | a-repo | T-field: vendored AmCharts namespace helper |
| C1-134 | `app/client/imports/amcharts/ammap.js`:21 | `d.formatNumber(a,g)` | `formatNumber` | unresolved | absent | a-repo | T-field: vendored AmCharts namespace helper |
| C1-135 | `app/client/imports/amcharts/ammap.js`:70 | `Math.round(d.toCoordinate(this.height,e))` | `round` | unresolved | absent | c-platform | Math.round built-in |
| C1-136 | `app/client/imports/amcharts/gauge.js`:6 | `isNaN(K)` | `isNaN` | unresolved | absent | c-platform | global isNaN built-in |
| C1-137 | `app/client/imports/amcharts/plugins/export/libs/fabric.js/fabric.min.js`:1 | `this._objects.filter(function(o){return o.type===type})` | `filter` | unresolved | absent | c-platform | Array.prototype.filter on an object array |
| C1-138 | `app/client/imports/amcharts/plugins/export/libs/jszip/jszip.min.js`:12 | `a._data.getCompressedContent()` | `getCompressedContent` | unresolved | absent | a-repo | T-flow: vendored JSZip compressed-data object method |
| C1-139 | `app/client/imports/amcharts/plugins/export/libs/jszip/jszip.min.js`:12 | `s(this.crc32(p),4)` | `s` | ambiguous | absent | a-repo | T-flow: vendored JSZip bundle-internal local helper |
| C1-140 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:7 | `Math.pow(2,-a)` | `pow` | unresolved | absent | c-platform | Math.pow built-in |
| C1-141 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:8 | `pn(e,n,3)` | `pn` | in-file | absent | a-repo | T-none: vendored pdfmake bundle-internal helper |
| C1-142 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:8 | `r(t,e)` | `r` | ambiguous | absent | a-repo | T-flow: vendored pdfmake bundle-internal local helper |
| C1-143 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:9 | `new h(e,n,r,this.imageMeasure,this.tableLayouts,u)` | `h` | ambiguous | absent | a-repo | T-flow: vendored pdfmake bundle-internal constructor |
| C1-144 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:13 | `n(71)` | `n` | ambiguous | absent | a-repo | T-flow: vendored bundle module require, a bound parameter |
| C1-145 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `t.readString(4)` | `readString` | unresolved | absent | a-repo | T-flow: vendored pdfmake stream reader method |
| C1-146 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `T.writeUInt16(i)` | `writeUInt16` | unresolved | absent | a-repo | T-flow: vendored pdfmake buffer writer method |
| C1-147 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:15 | `t.charCodeAt(r)` | `charCodeAt` | unresolved | absent | c-platform | String.prototype.charCodeAt |
| C1-148 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:17 | `n()` | `n` | ambiguous | absent | a-repo | T-flow: vendored pdfmake bundle-internal local helper |
| C1-149 | `app/client/imports/amcharts/plugins/export/libs/xlsx/xlsx.min.js`:2 | `path.toUpperCase().replace(chr0,"").replace(chr1,"!")` | `replace` | unresolved | absent | c-platform | String.prototype.replace chained on toUpperCase |
| C1-150 | `app/client/imports/amcharts/serial.js`:80 | `e.resetDateToMin(new Date(this.data[f].time),g,w,p)` | `resetDateToMin` | unresolved | absent | a-repo | T-field: vendored AmCharts namespace helper |
| C1-151 | `app/client/imports/amcharts/xy.js`:5 | `this.getAxisBounds(a,f,m,k,b)` | `getAxisBounds` | unresolved | absent | a-repo | T-hier: vendored AmCharts chart prototype method |
| C1-152 | `app/client/lib/SlickGrid - a grid/slick.grid.js`:240 | `$("<div class='slick-header-columns' style='left:-1000px' />").appendTo($headerScroller)` | `appendTo` | unresolved | absent | b-lib | jQuery appendTo on a wrapped element |
| C1-153 | `app/client/lib/SlickGrid - a grid/slick.grid.js`:660 | `getEditorLock()` | `getEditorLock` | in-file | absent | a-repo | T-none: vendored SlickGrid internal function |
| C1-154 | `app/client/lib/bootstrap-daterangepicker-custom/daterangepicker.js`:458 | `moment(startDate, this.locale.format)` | `moment` | unresolved | absent | b-lib | moment factory call |
| C1-155 | `app/client/lib/bootstrap-editable/js/bootstrap-editable.js`:254 | `this.showForm(false)` | `showForm` | unresolved | absent | a-repo | T-hier: vendored bootstrap-editable prototype method |
| C1-156 | `app/client/lib/bootstrap-editable/js/bootstrap-editable.js`:2653 | `$.proxy(function () { this.sourceData = cache.sourceData; this.doPrepend(); success.cal…` | `proxy` | unresolved | absent | b-lib | jQuery.proxy |
| C1-157 | `app/client/lib/gojs/go.js`:57 | `this.Fa.reset()` | `reset` | unresolved | absent | a-repo | T-flow: vendored GoJS method on an obfuscated field |
| C1-158 | `app/client/lib/gojs/go.js`:103 | `Object.isFrozen(this)` | `isFrozen` | unresolved | absent | c-platform | Object.isFrozen built-in |
| C1-159 | `app/client/lib/gojs/go.js`:337 | `d.Df()` | `Df` | unresolved | absent | a-repo | T-flow: vendored GoJS method on an obfuscated local |
| C1-160 | `app/client/lib/gojs/go.js`:882 | `d.gt(f.gj)` | `gt` | unresolved | absent | a-repo | T-flow: vendored GoJS method on an obfuscated local |
| C1-161 | `app/client/lib/gojs/go.js`:1162 | `c.lb(a)` | `lb` | unresolved | absent | a-repo | T-flow: vendored GoJS method on an obfuscated local |
| C1-162 | `app/client/lib/gojs/go.js`:1225 | `a.rect(l,r,Math.max(m,.1),Math.max(k,.1))` | `rect` | unresolved | absent | a-repo | T-hier: vendored GoJS context wrapper rect, not canvas |
| C1-163 | `app/client/lib/gojs/go.js`:1366 | `Ho(b,!1)` | `Ho` | in-file | absent | a-repo | T-none: vendored GoJS bundle-internal function |
| C1-164 | `app/client/lib/gojs/go.js`:1718 | `l.na()` | `na` | unresolved | absent | a-repo | T-flow: vendored GoJS method on an obfuscated local |
| C1-165 | `app/client/lib/gojs/go.js`:1739 | `Wq(a.width)` | `Wq` | in-file | absent | a-repo | T-none: vendored GoJS bundle-internal function |
| C1-166 | `app/client/lib/gojs/go.js`:2048 | `Ma.S.h(K,Ga)` | `h` | unresolved | absent | a-repo | T-flow: vendored GoJS method on an obfuscated local |
| C1-167 | `app/client/lib/jquery-ui-1.12.0.custom/jquery-ui.js`:3668 | `parentInstance._over.call( parentInstance, event )` | `call` | unresolved | absent | c-platform | Function.prototype.call |
| C1-168 | `app/client/lib/jquery-ui-1.12.0.custom/jquery-ui.js`:4005 | `[ "padding", /ne\|nw\|n/.test( i ) ? "Top" : /se\|sw\|s/.test( i ) ? "Bottom" : /^e$/.t…` | `join` | unresolved | absent | c-platform | Array.prototype.join on an array literal |
| C1-169 | `app/client/plugins/d3/d3.min.js`:2 | `o.unshift(l)` | `unshift` | unresolved | absent | c-platform | Array.prototype.unshift |
| C1-170 | `app/client/styles/framework/bootstrap3-plugins/bootstrap-tagsinput/bootstrap-tagsinput.js`:349 | `$(event.target)` | `$` | unresolved | absent | b-lib | jQuery factory call |
| C1-171 | `app/client/views/boImports/boImports.js`:246 | `setFileData({ file, headers, tpl })` | `setFileData` | in-file | absent | a-repo | T-none: syntax tier resolves it in-file |
| C1-172 | `app/client/views/boSimplifiedForm/detectedVulnerabilitySimplified/dvRehashModal.js`:68 | `BusinessObjects.find({ "md.type" : "assets" }).fetch()` | `fetch` | unresolved | absent | b-lib | Mongo cursor fetch on a collection find |
| C1-173 | `app/client/views/campaigns/campaignStats/campaignStats.js`:60 | `_.isEmpty(existing)` | `isEmpty` | unresolved | absent | b-lib | lodash isEmpty() |
| C1-174 | `app/client/views/common/iboxTools/ibox-tools.js`:74 | `$(e.target)` | `$` | unresolved | absent | b-lib | jQuery factory call |
| C1-175 | `app/client/views/internalSecOps/boQuickEntry/quickEntryField.js`:566 | `relatedSet.add(key)` | `add` | ambiguous | absent | c-platform | Set.prototype.add; sibling binding is a Map |
| C1-176 | `app/client/views/internalSecOps/incidents/incidentsList.js`:49 | `incidentsGrid.grid.render()` | `render` | unresolved | absent | a-repo | T-flow: vendored SlickGrid grid method |
| C1-177 | `app/client/views/internalSecOps/ofra/ofraTracking.js`:240 | `mapUsers([Meteor.user()])` | `mapUsers` | unresolved | absent | a-repo | T-import: named import from a repo module in this file |
| C1-178 | `app/client/views/internalSecOps/risks/risksCreateEdit.js`:117 | `_.result(fortress, "customOfraLinks.get.risks")` | `result` | unresolved | absent | b-lib | lodash result() |
| C1-179 | `app/client/views/internalSecOps/rootIssues/edit/editRootIssues.js`:49 | `Template.editRootIssues.onRendered(function() { $('#panelCompanies').removeClass('hidde…` | `onRendered` | unresolved | absent | b-lib | Blaze Template.onRendered |
| C1-180 | `app/client/views/internalSecOps/rootIssues/rootIssuesGrid.js`:25 | `selectizeUtils.listSync("rootIssues-category")` | `listSync` | import | absent | a-repo | T-none: syntax tier resolves the import |
| C1-181 | `app/client/views/internalSecOps/services/services.js`:200 | `tpl.get("visibleIfFields")` | `get` | unresolved | absent | a-repo | T-hier: Blaze TemplateInstance prototype method, repo package |
| C1-182 | `app/client/views/internalSecOps/vendors/editVendorDetails/tabContent/activityShared/activityHooks.test.js`:257 | `expect(out[0].action).toBe( `updated [${STOCK_OFRA.services.label}: Engagement A]`, )` | `toBe` | unresolved | absent | b-lib | vitest expect() matcher |
| C1-183 | `app/client/views/internalSecOps/vulnSummaryPage/vulnSummaryPage.js`:78 | `_.result(tpl, "buInfo.get.noBU", false)` | `result` | unresolved | absent | b-lib | lodash result() |
| C1-184 | `app/client/views/internalSecOps/vulnerabilities/knownVulnerabilities/knownVulnsSpecificTabs/knownVulnsSpecificTabs.js`:150 | `self.disable()` | `disable` | unresolved | absent | b-lib | selectize instance method in an onInitialize hook |
| C1-185 | `app/client/views/manageFiles/uploadedFilesGrid.js`:183 | `modal.find(".modal-body")` | `find` | unresolved | absent | b-lib | jQuery find() on a wrapped modal element |
| C1-186 | `app/client/views/manageUsers/manUsers.js`:727 | `_.get(v.roles, [_.keys(v.roles)[0]])` | `get` | unresolved | absent | b-lib | lodash get() |
| C1-187 | `app/client/views/manage_subscription/catalogConfigs.js`:55 | `_.get(err, "message")` | `get` | unresolved | absent | b-lib | lodash get() |
| C1-188 | `app/client/views/recertification/createNewVendorModal/createNewVendorModal.js`:37 | `Tracker.afterFlush(() => { $(`#${MODAL_ID}`).modal("show"); $(`#${MODAL_ID}`).one("hidd…` | `afterFlush` | unresolved | absent | b-lib | Meteor Tracker.afterFlush |
| C1-189 | `app/client/views/recertification/useContactCardPopover.test.jsx`:73 | `expect(valueOrEmpty("hello"))` | `expect` | unresolved | absent | b-lib | vitest expect() matcher |
| C1-190 | `app/client/views/rolesNotifications/notificationRules/recipientFields.js`:313 | `_.castArray(notificationRecipientUtils.getRecipientFieldValue(_.get(doc, "recipients"),…` | `castArray` | unresolved | absent | b-lib | lodash castArray() |
| C1-191 | `app/client/views/surveys/surveyBuilder/newSurvey.js`:611 | `validateSectionRanges(scale, path, vc, section)` | `validateSectionRanges` | in-file | absent | a-repo | T-none: syntax tier resolves it in-file |
| C1-192 | `app/client/views/surveys/surveyBuilder/newSurvey.js`:914 | `Session.get("readOnly")` | `get` | unresolved | absent | b-lib | Meteor Session.get |
| C1-193 | `app/client/views/system/manageKeywordMatchRules/manageKeywordMatchRules.js`:46 | `SlickGrid.keywordMatchRulesGrid.grid.getSelectionModel()` | `getSelectionModel` | unresolved | absent | a-repo | T-flow: vendored SlickGrid grid method |
| C1-194 | `app/client/views/system/manage_boConfigs/quickEntryFormBuilder/quickEntryFormBuilder.js`:825 | `_.get(o, "label")` | `get` | unresolved | absent | b-lib | lodash get() |
| C1-195 | `app/client/views/system/navigation/systemNavigation.js`:759 | `_.get(existing, "parent")` | `get` | unresolved | absent | b-lib | lodash get() |
| C1-196 | `app/client/views/threatAlerts/icsAlerts/IcsAlertsPage.test.jsx`:128 | `expect( extractCves({ cves : [ { code : "CVE-2024-1234", boID : "bo1" }, { code : "CVE-…` | `expect` | unresolved | absent | b-lib | vitest expect() matcher |
| C1-197 | `app/client/views/threatAlerts/icsAlerts/IcsAlertsPage.test.jsx`:221 | `advisoryDateOf({ ts_posted : new Date("2024-07-03T00:30:00.000Z"), ts_updated : new Dat…` | `toISOString` | unresolved | absent | c-platform | Date.prototype.toISOString |
| C1-198 | `app/client/views/workflowStats/workflowStats.js`:494 | `_.chain(_.cloneDeep(headerCols)) .map((p) => _.chain(p) .map("wfProcesses") .flatten() …` | `value` | unresolved | absent | b-lib | lodash chain terminator value() |
| C1-199 | `app/client/views/workflowStats/workflowStats.js`:713 | `_.chain(p).get("wfProcesses", []).map(function(pr) { let steps = _.get(pr, "procSteps",…` | `map` | unresolved | absent | b-lib | lodash chain map() |
| C1-200 | `app/imports/ui/cm-dashboard/components/charts/findings-chart/index.test.jsx`:602 | `renderAndSettle([ { _id: "Sev-1", count: 4, sevID: "s1", toolMappings: [critical.mappin…` | `renderAndSettle` | in-file | absent | a-repo | T-none: syntax tier resolves it in-file |
| C1-201 | `app/imports/ui/cm-dashboard/components/charts/vendors-location/create-map.test.js`:233 | `expect(getCountryFallbackCoords("VN"))` | `expect` | unresolved | absent | b-lib | vitest expect() matcher |
| C1-202 | `app/imports/ui/cm-dashboard/components/overall-filter-menu/SavedViewsPopover.jsx`:816 | `savedViews.system.map((viewItem) => { const isOwner = viewItem?.userID === Meteor.userI…` | `map` | unresolved | stub | c-platform | Array.prototype.map; savedViews.system defaults to [] |
| C1-203 | `app/imports/ui/cm-dashboard/components/overall-filters/index.jsx`:1721 | `recalcBannerHeight()` | `recalcBannerHeight` | in-file | in-repo | a-repo | T-none: syntax in-file and engine in-repo agree |
| C1-204 | `app/imports/ui/cm-dashboard/components/tabs/360-view/risk-profile/initial-layout.jsx`:43 | `markdownUtils.md2html(description \|\| "")` | `md2html` | import | stub | a-repo | T-none: syntax tier resolves the import |
| C1-205 | `app/imports/ui/cm-dashboard/microComponents/drag-and-drop-file-upload.jsx`:724 | `prev?.some((existing) => existing?.id === file?.id)` | `some` | unresolved | stub | c-platform | Array.prototype.some on a state array |
| C1-206 | `app/imports/ui/cm-dashboard/providers/VendorProvider.test.jsx`:229 | `new Error("Invalid vendor id")` | `Error` | unresolved | absent | c-platform | Error constructor built-in |
| C1-207 | `app/lib/flowRouterCompat.js`:1682 | `globalWaitOn().concat( _.isFunction(controller.waitOn) ? controller.waitOn(ironParams, …` | `concat` | unresolved | stub | c-platform | Array.prototype.concat; globalWaitOn returns an array |
| C1-208 | `app/lib/flowRouterCompat.serverPaths.test.js`:595 | `dispatch(request("GET", "/api/thing"), res)` | `dispatch` | in-file | absent | a-repo | T-none: syntax tier resolves it in-file |
| C1-209 | `app/lib/object_utils.js`:107 | `_.each(data, function(doc) { _.each(exportFields, function(field, i) { if (i > 0) { str…` | `each` | unresolved | stub | b-lib | lodash each() |
| C1-210 | `app/lib/routeControllers/campaignStatsController.js`:17 | `createController({ template : "campaignStats", waitOn(params) { return [ Meteor.subscri…` | `createController` | unresolved | stub | a-repo | T-import: named import from a repo module in this file |
| C1-211 | `app/lib/routeDefinitions/samlRoutes.js`:333 | `Subscriptions.findOneAsync({ "saml.identifier" : _.get(fGroup, "sub", "") })` | `findOneAsync` | unresolved | stub | b-lib | Mongo collection findOneAsync |
| C1-212 | `app/packages/jade-compiler/package.js`:42 | `api.addFiles(["tests/tests.js"], "server")` | `addFiles` | unresolved | absent | b-lib | Meteor package build API api.addFiles |
| C1-213 | `app/packages/meteor-amcharts/lib/amcharts.js`:39 | `a.getLabel()` | `getLabel` | unresolved | stub | a-repo | T-flow: vendored AmCharts axis object method |
| C1-214 | `app/packages/meteor-amcharts/lib/amcharts.js`:301 | `b.set()` | `set` | unresolved | stub | a-repo | T-flow: vendored AmCharts container set method |
| C1-215 | `app/packages/meteor-amcharts/lib/plugins/animate/animate.js`:351 | `getKeysGraphs( chart.graphs, keys, seen, getKeysGraph )` | `getKeysGraphs` | in-file | in-repo | a-repo | T-none: vendored amcharts plugin internal function |
| C1-216 | `app/packages/meteor-amcharts/lib/plugins/export/export.js`:1351 | `_this.gatherClassName( group.parent, _this.setup.chart.classNamePrefix + "-legend-div",…` | `gatherClassName` | unresolved | stub | a-repo | T-hier: vendored amcharts export plugin method |
| C1-217 | `app/packages/meteor-amcharts/lib/plugins/export/libs/fabric.js/fabric.js`:329 | `this.getObjects()` | `getObjects` | unresolved | in-repo | a-repo | T-none: vendored fabric.js method, engine says in-repo |
| C1-218 | `app/packages/meteor-amcharts/lib/plugins/export/libs/fabric.js/fabric.min.js`:6 | `toFixed(this.scaleX,NUM_FRACTION_DIGITS)` | `toFixed` | unresolved | absent | a-repo | T-field: vendored fabric.util.toFixed, not the built-in |
| C1-219 | `app/packages/meteor-amcharts/lib/plugins/export/libs/fabric.js/fabric.min.js`:11 | `floor(j*ratioH)` | `floor` | unresolved | absent | c-platform | Math.floor via a local alias in the vendored bundle |
| C1-220 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.js`:2003 | `__webpack_require__(7)` | `__webpack_require__` | in-file | absent | a-repo | T-none: vendored bundle webpack module loader |
| C1-221 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.js`:17016 | `result.push({ x: self.rowSpanData[self.rowSpanData.length - 1].left, index: self.rowSpa…` | `push` | unresolved | absent | c-platform | Array.prototype.push on a result array |
| C1-222 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:1 | `r.fs.bindFS(this.vfs)` | `bindFS` | unresolved | absent | a-repo | T-flow: vendored pdfkit virtual filesystem method |
| C1-223 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:13 | `s.split("\n").map(function(t){return" "+t})` | `map` | unresolved | absent | c-platform | Array.prototype.map on a split() result |
| C1-224 | `app/packages/meteor-amcharts/lib/plugins/export/libs/xlsx/xlsx.js`:420 | `fmt.match(dec1)` | `match` | unresolved | stub | c-platform | String.prototype.match on a format string |
| C1-225 | `app/packages/meteor-amcharts/lib/plugins/export/libs/xlsx/xlsx.js`:8532 | `unescapexml(Rn[3])` | `unescapexml` | unresolved | in-repo | a-repo | T-none: vendored xlsx function, engine says in-repo |
| C1-226 | `app/packages/meteor-amcharts/lib/plugins/export/libs/xlsx/xlsx.min.js`:3 | `__utf16le(this,this.l,this.l+size)` | `__utf16le` | unresolved | absent | a-repo | T-flow: vendored xlsx bundle-internal helper |
| C1-227 | `app/packages/meteor-apm-agent/tests/hijack/subscriptions.js`:17 | `h2.stop()` | `stop` | unresolved | absent | b-lib | DDP subscription handle stop, from client.subscribe |
| C1-228 | `app/packages/meteor-apm-agent/tests/models/base_error.js`:49 | `new BaseErrorModel()` | `BaseErrorModel` | unresolved | absent | a-repo | T-import: Meteor package-scope global from a repo file |
| C1-229 | `app/packages/meteor-stylus/plugin/compile-stylus.js`:213 | `absoluteImportPath(parsed)` | `absoluteImportPath` | in-file | in-repo | a-repo | T-none: syntax in-file and engine in-repo agree |
| C1-230 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:116 | `setTimeout(function(){b.destroy.call(b)},1E3*a)` | `setTimeout` | unresolved | absent | c-platform | setTimeout host API |
| C1-231 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:143 | `d.setCN(c,r,"graph-bullet")` | `setCN` | unresolved | absent | a-repo | T-field: vendored AmCharts namespace helper |
| C1-232 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:259 | `n.translate(t,p)` | `translate` | unresolved | absent | a-repo | T-flow: vendored AmCharts object translate method |
| C1-233 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:369 | `c.fire({type:"selectedObjectChanged",chart:c})` | `fire` | unresolved | absent | a-repo | T-flow: vendored AmCharts event fire method |
| C1-234 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:482 | `this.chart.coordinatesToXY(this.longitudes[b],this.latitudes[b])` | `coordinatesToXY` | unresolved | absent | a-repo | T-flow: vendored AmCharts chart method via this.chart |
| C1-235 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:15 | `h.slice(g)` | `slice` | unresolved | stub | c-platform | String.prototype.slice inside a wordwrap loop |
| C1-236 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:77 | `a[b].remove()` | `remove` | unresolved | stub | a-repo | T-flow: vendored AmCharts label object remove |
| C1-237 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:95 | `d.Class({construct:function(){}})` | `Class` | unresolved | stub | a-repo | T-field: vendored AmCharts.Class factory |
| C1-238 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:126 | `d.applyTheme(this,a,this.cname)` | `applyTheme` | unresolved | stub | a-repo | T-field: vendored AmCharts namespace helper |
| C1-239 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/fabric.js/fabric.js`:7920 | `ctx.setLineDash(this.strokeDashArray)` | `setLineDash` | unresolved | stub | c-platform | canvas 2D context setLineDash |
| C1-240 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/fabric.js/fabric.js`:19503 | `_this.setAngle(value)` | `setAngle` | unresolved | stub | a-repo | T-hier: vendored fabric.js object prototype method |

### B. Reference repository — JavaScript, second half

| id | file:line | call expression | callee | syntax | engine | class | reason |
|---|---|---|---|---|---|---|---|
| C1-241 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/fabric.js/fabric.min.js`:7 | `this.getCurrentCharStyle(s,u)` | `getCurrentCharStyle` | unresolved | absent | a-repo | T-field: fabric.js prototype method in a vendored bundle |
| C1-242 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:16132 | `baseAssignValue(result, iteratee(value, key, object), value)` | `baseAssignValue` | in-file | absent | a-repo | T-none: bundled lodash helper in a vendored pdfmake bundle |
| C1-243 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:42362 | `Array.isArray(_iterator4)` | `isArray` | unresolved | absent | c-platform | Array.isArray built-in |
| C1-244 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:43843 | `feature('stylisticAlternatives', 'stylisticAltEleven')` | `feature` | in-file | absent | a-repo | T-none: in-file helper of a vendored pdfmake bundle |
| C1-245 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:66619 | `__webpack_require__(142)` | `__webpack_require__` | in-file | absent | a-repo | T-none: webpack runtime require in a vendored bundle |
| C1-246 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:2 | `Math.pow(2,8*n-1)` | `pow` | unresolved | absent | c-platform | Math.pow built-in |
| C1-247 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:5 | `t.replace("\t"," ")` | `replace` | unresolved | absent | c-platform | String.prototype.replace on a string |
| C1-248 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `new Error("TODO: cmap format 14")` | `Error` | unresolved | absent | c-platform | Error constructor built-in |
| C1-249 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:26 | `t.slice(c,f+1)` | `slice` | unresolved | absent | c-platform | slice on a built-in array/string value |
| C1-250 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.js`:10474 | `f.replace(/COM\.MICROSOFT\./g, "")` | `replace` | unresolved | stub | c-platform | String.prototype.replace; receiver used with charCodeAt |
| C1-251 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.min.js`:2 | `data.charCodeAt(0)` | `charCodeAt` | unresolved | absent | c-platform | String.prototype.charCodeAt on a string |
| C1-252 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.min.js`:2 | `fmt.substr(i,5)` | `substr` | unresolved | absent | c-platform | String.prototype.substr on a format string |
| C1-253 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.min.js`:10 | `px2pt(row.hpx)` | `px2pt` | in-file | absent | a-repo | T-none: in-file helper of a vendored xlsx bundle |
| C1-254 | `app/packages/sylido-meteor-selectize-bootstrap/selectize/dist/js/selectize.min.js`:1 | `e(g[d],c)` | `e` | in-file | absent | a-repo | T-none: minified local of a vendored selectize bundle |
| C1-255 | `app/server/actionButtons/actionButtons.js`:584 | `_.union(_.get(relatedAggsByType, k, []), val)` | `union` | unresolved | stub | b-lib | lodash union; no lodash source tracked in the app tree |
| C1-256 | `app/server/campaigns/campaigns.js`:247 | `_.get(auth, "sub._id")` | `get` | unresolved | stub | b-lib | lodash get; no lodash source tracked in the app tree |
| C1-257 | `app/server/centralServicesAPI/requestProductUtils.test.js`:176 | `new Error("not-allowed")` | `Error` | unresolved | absent | c-platform | Error constructor built-in |
| C1-258 | `app/server/fortressAPI/fortressAPI.js`:58 | `_.mapValues(_.get(requestOptions, "headers", {}), (v) => String(v))` | `mapValues` | unresolved | in-repo | b-lib | lodash mapValues; no lodash source tracked in the app tree |
| C1-259 | `app/server/jsreports/print.js`:174 | `_.get(user, "information.email", "")` | `get` | unresolved | stub | b-lib | lodash get; no lodash source tracked in the app tree |
| C1-260 | `app/server/lib/publish/detectedVulnerabilitiesGrid.js`:222 | `__cb(err)` | `__cb` | ambiguous | in-repo | a-repo | T-none: local const callback function in the same block |
| C1-261 | `app/server/lib/publish/myWatchedItems.js`:122 | `_.get(Meteor, "settings.flags.debug", false)` | `get` | unresolved | stub | b-lib | lodash get; no lodash source tracked in the app tree |
| C1-262 | `app/server/lib/publish/surveys.js`:813 | `_.escapeRegExp(query)` | `escapeRegExp` | unresolved | stub | b-lib | lodash escapeRegExp; no lodash source tracked in-tree |
| C1-263 | `app/server/lib/publish/widgetAggs/vulnByBUoverTimeAgg.js`:37 | `dateMomentUtils.modifiedDate("", "subtract", 6, "months")` | `modifiedDate` | import | stub | a-repo | T-none: default import of a repo date utils module |
| C1-264 | `app/server/manageScoring/manageScoring.js`:416 | `_.get(oldDoc, "md.id")` | `get` | unresolved | stub | b-lib | lodash get; no lodash source tracked in the app tree |
| C1-265 | `app/server/manageUsers/manUsers.js`:430 | `Meteor.users.find({ _id : { $in : tenantUserIds }, "information.vendorPortal" : true, i…` | `find` | unresolved | stub | b-lib | Mongo collection find; Meteor packages are not tracked |
| C1-266 | `app/server/methods/getBoDescriptions.test.js`:143 | `handler.call({}, ["a", "b"])` | `call` | unresolved | absent | c-platform | Function.prototype.call on a method handler |
| C1-267 | `app/server/navigation/navigation.test.js`:333 | `expect(boFindOne.mock.calls[0][0])` | `expect` | unresolved | absent | b-lib | jest global expect; jest is not tracked in-tree |
| C1-268 | `app/server/rolesNotifications/rolesNotifications.js`:88 | `EmailTemplates.find($match, $project)` | `find` | unresolved | in-repo | b-lib | Mongo.Collection find; Meteor packages are not tracked |
| C1-269 | `app/server/serverRouteApi/serverRouteApi.js`:157 | `logger.error("(/fp-api/runAggsMulti) Missing header parameters.", { nonce, hashAuth })` | `error` | unresolved | in-repo | b-lib | winston logger method; winston is not tracked in-tree |
| C1-270 | `app/server/surveys/importExport.js`:286 | `_.get(auth, "user._id")` | `get` | unresolved | stub | b-lib | lodash get; no lodash source tracked in the app tree |
| C1-271 | `app/server/taxonomy/taxonomy.js`:97 | `taxonomySchema.validate(modifier, { modifier : true })` | `validate` | import | stub | b-lib | SimpleSchema validate; aldeed package not tracked in-tree |
| C1-272 | `app/server/unitTests/boAutomationRules/boAutomationRules.app-test.js`:96 | `sinon.createSandbox()` | `createSandbox` | import | absent | b-lib | sinon createSandbox; no sinon source or definition in-tree |
| C1-273 | `app/server/unitTests/cmDashboard/cmDashboardFindingsGrid.app-test.js`:419 | `chai.expect(runChart({ vendorID : "" }))` | `expect` | import | absent | b-lib | chai expect; chai is not tracked in-tree |
| C1-274 | `app/server/utils/emailUtils.js`:572 | `_.get(val, "email", "")` | `get` | unresolved | stub | b-lib | lodash get; no lodash source tracked in the app tree |
| C1-275 | `app/server/wfStatsBasic/wfStatsBasic.js`:54 | `getCustomLabel("info.date.due", "Date Due", "assessmentFindings")` | `getCustomLabel` | unresolved | stub | a-repo | T-import: named import from a repo schema module |
| C1-276 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/bootstrap.js`:190 | `$(element)` | `$` | unresolved | stub | a-repo | T-flow: global $ assigned in the tracked jquery-2.1.1.js |
| C1-277 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/inspinia.js`:255 | `$('body')` | `$` | unresolved | stub | a-repo | T-flow: global $ assigned in the tracked jquery-2.1.1.js |
| C1-278 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-2.1.1.js`:3 | `n.now()` | `now` | unresolved | absent | c-platform | jQuery now is bound to the built-in Date.now |
| C1-279 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:6 | `e("<a>")` | `e` | ambiguous | absent | a-repo | T-flow: minified jQuery param bound to the tracked jQuery |
| C1-280 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:7 | `t.effects.animateClass.call(this,n?{add:s}:{remove:s},a,o,r)` | `call` | unresolved | absent | c-platform | Function.prototype.call on a plugin method |
| C1-281 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:7 | `s.outerHeight()` | `outerHeight` | unresolved | absent | a-repo | T-demand: jQuery outerHeight generated in the tracked jQuery |
| C1-282 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:7 | `this.helper.offset()` | `offset` | unresolved | absent | a-repo | T-field: jQuery offset in the tracked jquery-2.1.1.js |
| C1-283 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/chartist/chartist.min.js`:7 | `c.serialize(o.meta)` | `serialize` | unresolved | absent | a-repo | T-field: Chartist core serialize in a vendored bundle |
| C1-284 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/codemirror.js`:7629 | `getOrder(line)` | `getOrder` | in-file | in-repo | a-repo | T-none: in-file helper of the vendored CodeMirror core |
| C1-285 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/mode/rst/rst.js`:170 | `stream.match(rx_role_pre, false)` | `match` | unresolved | in-repo | a-repo | T-none: CodeMirror StringStream match in the tracked core |
| C1-286 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/mode/sparql/sparql.js`:121 | `pushContext(state, "}", stream.column())` | `pushContext` | in-file | in-repo | a-repo | T-none: in-file helper of a vendored CodeMirror mode |
| C1-287 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/mode/xml/xml.js`:133 | `stream.match(/^[^\s\u00a0=<>\"\']*[^\s\u00a0=<>\"\'\/]/)` | `match` | unresolved | in-repo | a-repo | T-none: CodeMirror StringStream match in the tracked core |
| C1-288 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/cropper/cropper.min.js`:9 | `this.renderImage("zoom")` | `renderImage` | unresolved | absent | a-repo | T-field: cropper plugin method in a vendored bundle |
| C1-289 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/dataTables/jquery.dataTables.js`:500 | `_fnCompatMap( init, 'orderDataType', 'sortDataType' )` | `_fnCompatMap` | in-file | in-repo | a-repo | T-none: in-file helper of vendored DataTables |
| C1-290 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/dataTables/jquery.dataTables.js`:4217 | `tmpTable.css( 'width', 'auto' )` | `css` | unresolved | in-repo | a-repo | T-none: jQuery css in the tracked jquery-2.1.1.js |
| C1-291 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/dataTables/jquery.dataTables.js`:14632 | `a.toString()` | `toString` | unresolved | stub | c-platform | Object.prototype.toString on an arbitrary value |
| C1-292 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/footable/footable.all.min.js`:14 | `t(a.table).unbind(".sorting").bind({"footable_initialized.sorting":function(){var i,o,n…` | `data` | unresolved | absent | a-repo | T-field: jQuery data in the tracked jquery-2.1.1.js |
| C1-293 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/fullcalendar/fullcalendar.min.js`:6 | `ye(r,t[s],r.forwardSegs)` | `ye` | in-file | absent | a-repo | T-none: minified in-file helper of vendored FullCalendar |
| C1-294 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/fullcalendar/moment.min.js`:6 | `r(C(a)%60,2)` | `r` | in-file | absent | a-repo | T-none: minified in-file helper of vendored moment |
| C1-295 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:117 | `b(y,a).closest("table.ui-jqgrid-btable").attr("id")` | `attr` | unresolved | absent | a-repo | T-field: jQuery attr in the tracked jquery-2.1.1.js |
| C1-296 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:221 | `a(c)` | `a` | ambiguous | absent | a-repo | T-flow: minified jQuery param bound to the tracked jQuery |
| C1-297 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:332 | `a("#"+v.themodal)` | `a` | ambiguous | absent | a-repo | T-flow: minified jQuery param bound to the tracked jQuery |
| C1-298 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:411 | `a.isFunction(b.p.beforeSelectRow)` | `isFunction` | unresolved | absent | a-repo | T-field: jQuery isFunction in the tracked jquery-2.1.1.js |
| C1-299 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:437 | `s("unbind")` | `s` | ambiguous | absent | a-repo | T-flow: minified local of the vendored jqModal plugin |
| C1-300 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:462 | `f.hasClass(a)` | `hasClass` | unresolved | absent | a-repo | T-field: jQuery hasClass in the tracked jquery-2.1.1.js |
| C1-301 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:517 | `this.toObj(g)` | `toObj` | unresolved | absent | a-repo | T-field: xmlJsonClass method in the vendored jqGrid bundle |
| C1-302 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:5372 | `rplusequals.exec( value )` | `exec` | unresolved | stub | c-platform | RegExp.prototype.exec on a regex literal |
| C1-303 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:6413 | `$( this )` | `$` | unresolved | stub | a-repo | T-flow: global $ assigned in the tracked jquery-2.1.1.js |
| C1-304 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:6605 | `toShow .hide()` | `hide` | unresolved | absent | a-repo | T-field: jQuery hide in the tracked jquery-2.1.1.js |
| C1-305 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:8129 | `this._updateAlternate(inst)` | `_updateAlternate` | unresolved | in-repo | a-repo | T-none: jQuery UI datepicker method in the same tracked file |
| C1-306 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.min.js`:5 | `t(n.containment)` | `t` | in-file | absent | a-repo | T-none: minified jQuery alias in vendored jQuery UI |
| C1-307 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.min.js`:8 | `s.removeClass("ui-accordion-header-active ui-state-active")` | `removeClass` | unresolved | absent | a-repo | T-field: jQuery removeClass in the tracked jquery-2.1.1.js |
| C1-308 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.min.js`:8 | `this._updateDatepicker(e)` | `_updateDatepicker` | unresolved | absent | a-repo | T-field: jQuery UI datepicker method in the same tracked file |
| C1-309 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jsTree/jstree.min.js`:4 | `c.element.find("ul:visible").addBack()` | `addBack` | unresolved | absent | a-repo | T-field: jQuery addBack in the tracked jquery-2.1.1.js |
| C1-310 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/morris/raphael-2.1.0.min.js`:10 | `bJ(b)` | `bJ` | in-file | absent | a-repo | T-none: minified in-file helper of vendored Raphael |
| C1-311 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/morris/raphael-2.1.0.min.js`:10 | `b.defs.removeChild(this.gradient)` | `removeChild` | unresolved | absent | c-platform | DOM Node.removeChild on an SVG defs node |
| C1-312 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/rickshaw/rickshaw.min.js`:2 | `d3.svg.line().x(function(d){return graph.x(d.x)}).y(function(d){return graph.y(d.y)}).i…` | `tension` | unresolved | absent | a-repo | T-flow: d3 line generator tension; d3.v3.js is tracked |
| C1-313 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/rickshaw/vendor/d3.v3.js`:1 | `u()` | `u` | ambiguous | absent | a-repo | T-flow: minified in-file helper of vendored d3 v3 |
| C1-314 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/select2/select2.full.min.js`:3 | `d.join(c._valueSeparator)` | `join` | unresolved | absent | c-platform | Array.prototype.join on a values array |
| C1-315 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/sparkline/jquery.sparkline.min.js`:4 | `e.get("colorMap")` | `get` | unresolved | absent | a-repo | T-field: sparkline internal options get in a vendored bundle |
| C1-316 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/staps/jquery.steps.min.js`:6 | `h.eq(e)` | `eq` | unresolved | absent | a-repo | T-field: jQuery eq in the tracked jquery-2.1.1.js |
| C1-317 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/sweetalert/sweetalert.min.js`:1 | `l.addClass(o,"visible")` | `addClass` | unresolved | absent | a-repo | T-field: sweetalert dom utils addClass in a vendored bundle |
| C1-318 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/validate/jquery.validate.min.js`:4 | `a(b).rules()` | `rules` | unresolved | absent | a-repo | T-field: jQuery Validate rules method in a vendored bundle |
| C1-319 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/bootstrap.js`:1812 | `$tip.find('.popover-content')` | `find` | unresolved | stub | a-repo | T-field: jQuery find in the tracked jquery-2.1.1.js |
| C1-320 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/bootstrap.min.js`:6 | `c.isInStateTrue()` | `isInStateTrue` | unresolved | absent | a-repo | T-field: bootstrap button plugin method in a vendored bundle |
| C1-321 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/jquery-ui-1.10.4.min.js`:6 | `this.headers.removeClass("ui-accordion-header ui-accordion-header-active ui-helper-rese…` | `removeAttr` | unresolved | absent | a-repo | T-field: jQuery removeAttr in the tracked jquery-2.1.1.js |
| C1-322 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/jquery-ui-1.10.4.min.js`:7 | `Math.max(0,a.maxHeight-e)` | `max` | unresolved | absent | c-platform | Math.max built-in |
| C1-323 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/plugins/pace/pace.min.js`:2 | `t(p)` | `t` | unresolved | absent | a-repo | T-flow: minified in-file helper of vendored pace |
| C1-324 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/plugins/slimscroll/jquery.slimscroll.min.js`:11 | `b.scrollTop()` | `scrollTop` | unresolved | absent | a-repo | T-demand: jQuery scrollTop generated in the tracked jQuery |
| C1-325 | `mockup/www/js/bootstrap.min.js`:6 | `a(this)` | `a` | unresolved | absent | a-repo | T-flow: minified jQuery param bound to the tracked jQuery |
| C1-326 | `mockup/www/js/bootstrap.min.js`:7 | `a(document).on("click.bs.tab.data-api",'[data-toggle="tab"]',e).on("click.bs.tab.data-a…` | `on` | unresolved | absent | a-repo | T-field: jQuery on in the tracked jquery-2.1.1.js |
| C1-327 | `mockup/www/js/jquery-ui-1.10.4.min.js`:6 | `this.focusable.add(e)` | `add` | unresolved | absent | a-repo | T-field: jQuery add in the tracked jquery-2.1.1.js |
| C1-328 | `mockup/www/js/jquery-ui-1.10.4.min.js`:6 | `this._hideDatepicker()` | `_hideDatepicker` | unresolved | absent | a-repo | T-field: jQuery UI datepicker method in the same tracked file |
| C1-329 | `mockup/www/js/jquery-ui-1.10.4.min.js`:7 | `this._trigger("beforeActivate",e,c)` | `_trigger` | unresolved | absent | a-repo | T-field: jQuery UI widget _trigger in the same tracked file |
| C1-330 | `mockup/www/js/plugins/amcharts/amcharts.js`:359 | `d.applyTheme(this,a,this.cname)` | `applyTheme` | unresolved | absent | a-repo | T-field: AmCharts applyTheme in the same tracked file |
| C1-331 | `mockup/www/js/plugins/amcharts/funnel.js`:24 | `q.getBBox()` | `getBBox` | unresolved | absent | a-repo | T-field: AmCharts SVG wrapper getBBox in a tracked file |
| C1-332 | `mockup/www/js/plugins/amcharts/plugins/animate/animate.js`:288 | `getKeysSliced( chart, keys, seen )` | `getKeysSliced` | in-file | absent | a-repo | T-none: in-file helper of the vendored amcharts animate plugin |
| C1-333 | `mockup/www/js/plugins/amcharts/plugins/export/export.min.js`:1 | `c.isTainted(a)` | `isTainted` | unresolved | absent | a-repo | T-field: amcharts export plugin method in a vendored bundle |
| C1-334 | `mockup/www/js/plugins/amcharts/plugins/export/libs/fabric.js/fabric.js`:13929 | `Math.sin(this.endAngle)` | `sin` | unresolved | absent | c-platform | Math.sin built-in |
| C1-335 | `mockup/www/js/plugins/amcharts/plugins/export/libs/fabric.js/fabric.js`:21986 | `this.fire('editing:exited')` | `fire` | unresolved | absent | a-repo | T-field: fabric Observable fire in a vendored bundle |
| C1-336 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.js`:779 | `hexWrite(this, string, offset, length)` | `hexWrite` | in-file | absent | a-repo | T-none: in-file buffer shim helper in vendored pdfmake |
| C1-337 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.js`:12141 | `string.slice(0, trimmedRightIndex(string) + 1)` | `slice` | unresolved | absent | c-platform | String.prototype.slice on a string |
| C1-338 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.js`:15481 | `pack(ALPHANUMERIC_MAP[data.charAt(i-1)], 6)` | `pack` | ambiguous | absent | a-repo | T-flow: in-file qrcode helper of a vendored pdfmake bundle |
| C1-339 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:8 | `_(e,ai.placeholder)` | `_` | ambiguous | absent | a-repo | T-flow: bundled lodash in a vendored pdfmake bundle |
| C1-340 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `t.readInt()` | `readInt` | unresolved | absent | a-repo | T-field: internal stream reader of vendored pdfmake |
| C1-341 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:15 | `this.glyphIDs.push(l.readShort())` | `push` | unresolved | absent | c-platform | Array.prototype.push on a glyph id array |
| C1-342 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:15 | `n.match(/^End(\w+)/)` | `match` | unresolved | absent | c-platform | String.prototype.match on a string |
| C1-343 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:16 | `(function(t){/*! @source http://purl.eligrey.com/github/FileSaver.js/blob/master/FileSa…` | `call` | unresolved | absent | c-platform | Function.prototype.call on an IIFE |
| C1-344 | `mockup/www/js/plugins/amcharts/plugins/export/libs/xlsx/xlsx.js`:1189 | `sector_list[minifat_store].data.slice(o.start*MSSZ,o.start*MSSZ+o.size)` | `slice` | unresolved | absent | c-platform | slice on a sector data buffer/array value |
| C1-345 | `mockup/www/js/plugins/amcharts/plugins/export/libs/xlsx/xlsx.min.js`:2 | `write_num("n",r[1],ff[1])` | `write_num` | unresolved | absent | a-repo | T-flow: internal writer of the vendored xlsx bundle |
| C1-346 | `mockup/www/js/plugins/amcharts/plugins/export/libs/xlsx/xlsx.min.js`:6 | `stack.pop()` | `pop` | unresolved | absent | c-platform | Array.prototype.pop on a stack array |
| C1-347 | `mockup/www/js/plugins/fullcalendar/moment.min.js`:6 | `vb(this)` | `vb` | unresolved | absent | a-repo | T-flow: minified in-file helper of vendored moment |
| C1-348 | `mockup/www/js/plugins/ionRangeSlider/ion.rangeSlider.js`:989 | `this.checkDiapason(this.coords.p_single_real, this.options.from_min, this.options.from_…` | `checkDiapason` | unresolved | absent | a-repo | T-field: ion.rangeSlider method in a vendored bundle |
| C1-349 | `mockup/www/js/plugins/ionRangeSlider/ion.rangeSlider.min.js`:50 | `this.callOnChange()` | `callOnChange` | unresolved | absent | a-repo | T-field: ion.rangeSlider method in a vendored bundle |
| C1-350 | `mockup/www/js/plugins/morris/morris.js`:1649 | `$.extend({}, this.defaults, options)` | `extend` | unresolved | absent | a-repo | T-field: jQuery extend in the tracked jquery-2.1.1.js |
| C1-351 | `mockup/www/js/plugins/query-builder/query-builder.standalone.min.js`:2747 | `cbRule.call(context, this.rules[i])` | `call` | unresolved | absent | c-platform | Function.prototype.call on a callback |
| C1-352 | `mockup/www/js/plugins/slimscroll/jquery.slimscroll.min.js`:12 | `e("<div></div>").addClass(a.wrapperClass).css({position:"relative",overflow:"hidden",wi…` | `css` | unresolved | absent | a-repo | T-field: jQuery css in the tracked jquery-2.1.1.js |
| C1-353 | `mockup/www/js/plugins/switchery/switchery.js`:1 | `adv.call(layer,type,callback.hijacked\|\|(callback.hijacked=function(event){if(!event.p…` | `call` | unresolved | absent | c-platform | Function.prototype.call on addEventListener |
| C1-354 | `mockup/www/js/plugins/switchery/switchery.js`:1 | `this.needsClick(this.targetElement)` | `needsClick` | unresolved | absent | a-repo | T-field: bundled fastclick method in vendored switchery |
| C1-355 | `mockup/www/js/plugins/typeahead/bloodhound.js`:61 | `$.each(obj, function(key, val) { if (result = test.call(null, val, key, obj)) { return …` | `each` | unresolved | absent | a-repo | T-field: jQuery each in the tracked jquery-2.1.1.js |
| C1-356 | `mockup/www/js/plugins/typeahead/bloodhound.js`:701 | `this.index.reset()` | `reset` | unresolved | absent | a-repo | T-field: bloodhound SearchIndex reset in a vendored bundle |
| C1-357 | `mongo scripts/aiMonitoringScripts/aim-1056-activity-stamp-backfill.js`:165 | `JSON.stringify(batcher.result)` | `stringify` | unresolved | stub | c-platform | JSON.stringify built-in |
| C1-358 | `mongo scripts/aiMonitoringScripts/aim-165-set-custom-fields-on-selectize-options.js`:147 | `db.getCollection(targetCollection).updateOne( { _id: doc._id }, { $set: doc }, { upsert…` | `updateOne` | unresolved | stub | c-platform | mongo shell collection updateOne host API |
| C1-359 | `mongo scripts/aiMonitoringScripts/aim-728-osd12-scrm-category-remap.js`:29 | `print("Migration ID: " + migrationID)` | `print` | unresolved | stub | c-platform | mongo shell print host global |
| C1-360 | `mongo scripts/disa/fix_object_ids.js`:44 | `bulk.find({_id : obj_id})` | `find` | unresolved | stub | c-platform | mongo shell bulk op find host API |
| C1-361 | `mongo scripts/internalTab-migration-initial.js`:749 | `_.each(_.get(selOption, "options", []), (option) => { newStatuses[_.toLower(_.get(optio…` | `each` | unresolved | absent | a-repo | T-field: lodash each; the loaded lodash.min.js is tracked |
| C1-362 | `mongo scripts/vrm_servicenow_sys_id_fix.js`:278 | `combineResult(bulk[targetCollection].execute(), targetCollection)` | `combineResult` | in-file | in-repo | a-repo | T-none: top-level function in the same script file |
| C1-363 | `playwright-validation/capture-settings-baseline.js`:218 | `Object.values(snap.vocabularies)` | `values` | unresolved | stub | c-platform | Object.values built-in |
| C1-364 | `playwright-validation/deep-grid.js`:29 | `buf.toString("utf8").split("\n") .filter((l) => /(^\|\s)error:\|Exception while\|TypeEr…` | `filter` | unresolved | absent | c-platform | Array.prototype.filter on a split result |
| C1-365 | `playwright-validation/sweep-session.js`:121 | `page.locator(".slick-row").first()` | `first` | unresolved | in-repo | b-lib | Playwright Locator first; playwright is not tracked in-tree |
| C1-366 | `vendorPortal/client/accounts/accountsTemplates.app-test.js`:76 | `chai.assert.isFalse(routerGo.calledWith("login"), "must not fall back to login on succe…` | `isFalse` | unresolved | absent | b-lib | chai assert isFalse; chai is not tracked in-tree |
| C1-367 | `vendorPortal/client/components/boCards/boAttachFileDialog.js`:132 | `_.get(tpl, "data.bo")` | `get` | unresolved | in-repo | b-lib | lodash get; no lodash source tracked in the client tree |
| C1-368 | `vendorPortal/client/lib/bootstrap-editable/js/bootstrap-editable.js`:1257 | `this.hide()` | `hide` | unresolved | in-repo | a-repo | T-none: bootstrap-editable method in the same tracked file |
| C1-369 | `vendorPortal/client/lib/bootstrap-editable/js/bootstrap-editable.js`:5203 | `d.getTimezoneOffset()` | `getTimezoneOffset` | unresolved | stub | c-platform | Date.prototype.getTimezoneOffset on a Date |
| C1-370 | `vendorPortal/client/lib/form_utils.js`:170 | `errorCallback(errMsg)` | `errorCallback` | unresolved | stub | d-unknown | callback parameter of validateForm invoked |
| C1-371 | `vendorPortal/client/lib/jquery-stickytableheaders/jquery.stickytableheaders.min.js`:1 | `f.css("padding-right")` | `css` | unresolved | absent | b-lib | jQuery css; no jQuery core tracked under this tree |
| C1-372 | `vendorPortal/client/lib/jquery-ui-1.12.0.custom/jquery-ui.js`:2543 | `c.css( "paddingLeft" )` | `css` | unresolved | stub | b-lib | jQuery css; no jQuery core tracked under this tree |
| C1-373 | `vendorPortal/client/pages/findings/findings.js`:185 | `Meteor.call("boUpdateSeen", _id)` | `call` | unresolved | stub | b-lib | Meteor.call; Meteor packages are not tracked in-tree |
| C1-374 | `vendorPortal/client/plugins/blueimp/jquery.blueimp-gallery.min.js`:1 | `define(["./blueimp-helper","./blueimp-gallery"],a)` | `define` | unresolved | absent | b-lib | AMD define; no requirejs or almond tracked in-tree |
| C1-375 | `vendorPortal/client/plugins/d3/d3.min.js`:1 | `Math.cos(w)` | `cos` | unresolved | absent | c-platform | Math.cos built-in |
| C1-376 | `vendorPortal/client/plugins/d3/d3.min.js`:1 | `n.point(p[0],p[1])` | `point` | unresolved | absent | a-repo | T-field: d3 stream listener point in a vendored bundle |
| C1-377 | `vendorPortal/client/plugins/d3/d3.min.js`:2 | `Su(r=l,u)` | `Su` | in-file | absent | a-repo | T-none: minified in-file helper of vendored d3 |
| C1-378 | `vendorPortal/client/plugins/d3/d3.min.js`:2 | `o.push(r[1])` | `push` | unresolved | absent | c-platform | Array.prototype.push on a local array |
| C1-379 | `vendorPortal/client/plugins/d3/d3.min.js`:3 | `t.push(n[e])` | `push` | unresolved | absent | c-platform | Array.prototype.push on a local array |
| C1-380 | `vendorPortal/client/plugins/d3/d3.min.js`:5 | `q.on("mousemove.brush",null).on("mouseup.brush",null)` | `on` | unresolved | absent | a-repo | T-field: d3 selection on in a vendored bundle |
| C1-381 | `vendorPortal/client/plugins/slimscroll/jquery.slimscroll.min.js`:719 | `target.addEventListener('wheel', _onWheel, false )` | `addEventListener` | unresolved | absent | c-platform | DOM addEventListener on a target element |
| C1-382 | `vendorPortal/lib/logger.js`:79 | `transports.push(new winston.transports.File({ level : logLevel, levels : customLevels.l…` | `push` | unresolved | absent | c-platform | Array.prototype.push on a transports array |
| C1-383 | `vendorPortal/packages/keithcoach-bootstrap3-datepicker/lib/js/bootstrap-datepicker.js`:500 | `this._detachEvents()` | `_detachEvents` | unresolved | in-repo | a-repo | T-none: bootstrap-datepicker method in the same tracked file |
| C1-384 | `vendorPortal/packages/matomo-custom/client/matomo.js`:7 | `FlowRouter.current()` | `current` | unresolved | stub | b-lib | FlowRouter; the ostrio package is not tracked in-tree |
| C1-385 | `vendorPortal/packages/meteor-template-extension/lib/template-inherits-hooks-from.js`:16 | `self.onCreated(hook)` | `onCreated` | unresolved | stub | b-lib | Blaze Template onCreated; Meteor packages not tracked |
| C1-386 | `vendorPortal/public/okta-auth-js.min.js`:8 | `Object.defineProperties(e,Object.getOwnPropertyDescriptors(n))` | `defineProperties` | unresolved | absent | c-platform | Object.defineProperties built-in |
| C1-387 | `vendorPortal/public/okta-auth-js.min.js`:8 | `n.n(i)` | `n` | unresolved | absent | a-repo | T-flow: webpack runtime helper in a vendored okta bundle |
| C1-388 | `vendorPortal/public/okta-auth-js.min.js`:8 | `Object.getOwnPropertyDescriptor(e,t)` | `getOwnPropertyDescriptor` | unresolved | absent | c-platform | Object.getOwnPropertyDescriptor built-in |
| C1-389 | `vendorPortal/public/okta-auth-js.min.js`:8 | `n(9231)` | `n` | ambiguous | absent | a-repo | T-flow: webpack module require in a vendored okta bundle |
| C1-390 | `vendorPortal/server/lib/publish/surveys.js`:456 | `_.get(v, "invited.o.rescindedUser", [])` | `get` | unresolved | in-repo | b-lib | lodash get; no lodash source tracked in the app tree |

### C. Reference repository — Java, Python, TypeScript

| id | file:line | call expression | callee | syntax | engine | class | reason |
|---|---|---|---|---|---|---|---|
| C1-001 | `QA/SeleniumWebdriver/TestngExtentFramework/src/main/java/com/selenium/test/webtestsbase/BasePage.java`:586 | `browserName.equals("chrome")` | `equals` | unresolved | absent | c-platform | JDK String.equals |
| C1-002 | `QA/SeleniumWebdriver/TestngExtentFramework/src/main/java/com/selenium/test/webtestsbase/Listeners.java`:67 | `context.getAttribute("WebDriver4Method")` | `getAttribute` | unresolved | absent | b-lib | TestNG ITestContext.getAttribute |
| C1-003 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/restassured/test/tests/RestAssuredAPITestsDemo.java`:239 | `RestAssured.given(). when(). get("http://ergast.com/api/f1/2017/circuits.json"). then()…` | `assertThat` | unresolved | absent | b-lib | REST Assured fluent validation API |
| C1-004 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/dataEntities/threatsFactors.java`:21 | `data.getRandomDigit("0123")` | `getRandomDigit` | unresolved | absent | a-repo | T-hier: DataUtils field, a repo utility class |
| C1-005 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/end2endTests/EndToEnd_Campaigns_Tests.java`:103 | `logger.info("Going to Create Vendor page")` | `info` | unresolved | absent | a-repo | T-hier: logger field typed as the repo LogUtils class |
| C1-006 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/end2endTests/EndToEnd_Campaigns_Tests.java`:202 | `relatedContactsGrid.returnCell(1,GridColumnNames.columnNameEmail)` | `returnCell` | unresolved | absent | a-repo | T-hier: local typed as the repo GridsPage page-object |
| C1-007 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/BOFormTemplatesTests.java`:22 | `new LogUtils()` | `LogUtils` | unresolved | absent | a-repo | T-import: constructor of a repo utility class |
| C1-008 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/BOFormTemplatesTests.java`:174 | `boFormTemplatesPage.removeMessage(driver)` | `removeMessage` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-009 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/CampaignsValidationTests.java`:47 | `TestsConfig.getConfig().isLocalRun()` | `isLocalRun` | unresolved | absent | a-repo | T-flow: receiver is the return of a repo static factory |
| C1-010 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/CampaignsValidationTests.java`:52 | `new LoginPage(driver)` | `LoginPage` | unresolved | absent | a-repo | T-import: constructor of a repo page-object |
| C1-011 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/CreateDeleteBOs_HelpMethods.java`:193 | `new GridsPage(driver, "systemsGrid", usedColumns)` | `GridsPage` | unresolved | absent | a-repo | T-import: constructor of a repo page-object |
| C1-012 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/EmailTemplatesValidationsTests.java`:106 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement(driver.findElement(By…` | `until` | unresolved | absent | b-lib | Selenium WebDriverWait.until |
| C1-013 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/EnableVRMFeaturesAndRiskOutcomes.java`:217 | `createEditOFRAPage.getCreateSaveButton().click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-014 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/EnableVRMFeaturesAndRiskOutcomes.java`:251 | `sidebarMenuPage.getMyApprovals()` | `getMyApprovals` | unresolved | absent | a-repo | T-hier: method on the repo SidebarMenuPage page-object |
| C1-015 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/KnowledgeBaseCUDTests.java`:94 | `knowledgeBasePage.getKnowledgeTitle(1).getAttribute("value")` | `getAttribute` | unresolved | absent | b-lib | Selenium WebElement.getAttribute |
| C1-016 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/KnowledgeBaseCreateValidationTests.java`:91 | `knowledgeBasePage.getSuccessErrorMessage().getText().equals("Failed to update Knowledge…` | `equals` | unresolved | absent | c-platform | JDK String.equals |
| C1-017 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/KnowledgeBaseUpdateValidationTests.java`:78 | `knowledgeBasePage.getUpdateBtn().click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-018 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageBOSubTypesTests.java`:79 | `getWait(driver)` | `getWait` | unresolved | absent | a-repo | T-hier: inherited from the repo BaseTest base class |
| C1-019 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageBOSubTypesTests.java`:335 | `navigateToListPage(boName)` | `navigateToListPage` | unresolved | absent | a-repo | T-hier: private helper in the same repo test class |
| C1-020 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesBulkEditTests.java`:111 | `TimeUtils.waitForSeconds(1)` | `waitForSeconds` | import | absent | a-repo | T-none: import tier already names the repo TimeUtils |
| C1-021 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesBulkEditTests.java`:330 | `manageFilesPage.getChangeRelationshipBtn()` | `getChangeRelationshipBtn` | unresolved | absent | a-repo | T-hier: method on the repo ManageFilesPage page-object |
| C1-022 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesBulkEditTests.java`:476 | `TimeUtils.waitForSeconds(1, driver)` | `waitForSeconds` | import | absent | a-repo | T-none: import tier already names the repo TimeUtils |
| C1-023 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesTests.java`:383 | `manageFilesPage.getSuccessErrorMessage()` | `getSuccessErrorMessage` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-024 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesTests.java`:561 | `Assert.assertTrue(manageFilesPage.getSuccessErrorMessage().getText().contains("was uplo…` | `assertTrue` | import | absent | b-lib | TestNG assertion API; no assertTrue defined in a tracked file |
| C1-025 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageNotificationRulesBOTypeTests.java`:177 | `logger.info("The 2nd test b_updateRuleWithRandomBOType() finished")` | `info` | unresolved | absent | a-repo | T-hier: logger field typed as the repo LogUtils class |
| C1-026 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageNotificationRulesBOTypeTests.java`:239 | `newEditNotificationRulePage.getEmailTemplateSelectbox().click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-027 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageNotificationRulesIntervalTests.java`:80 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement( notificationRulesPag…` | `until` | unresolved | absent | b-lib | Selenium WebDriverWait.until |
| C1-028 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManagePhaseCUDTests.java`:156 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement(managePhasePage.getSu…` | `until` | unresolved | absent | b-lib | Selenium WebDriverWait.until |
| C1-029 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageUsersBusinessRoleTests.java`:55 | `sidebarMenuPage.getUserName()` | `getUserName` | unresolved | absent | a-repo | T-hier: method on the repo SidebarMenuPage page-object |
| C1-030 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageUsersCUDTests.java`:164 | `TimeUtils.waitForSeconds(2)` | `waitForSeconds` | import | absent | a-repo | T-none: import tier already names the repo TimeUtils |
| C1-031 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageUsersValidationsTests.java`:62 | `logger.info("ManageUsersValidationsTests completed on " + browser.toUpperCase())` | `info` | unresolved | absent | a-repo | T-hier: logger field typed as the repo LogUtils class |
| C1-032 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/MyWatchedItemsTests.java`:137 | `gridsPage.getRemoveAllFiltersBtn().click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-033 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/NotificationRulesAppliedToBOTests.java`:52 | `TestsConfig.getConfig().getBrowser(browser)` | `getBrowser` | unresolved | absent | a-repo | T-flow: receiver is the return of a repo static factory |
| C1-034 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/NotificationRulesAppliedToWorkflowsTests.java`:301 | `gridsPage.waitForLoadingIndicator()` | `waitForLoadingIndicator` | unresolved | absent | a-repo | T-hier: method on the repo GridsPage page-object |
| C1-035 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/QuestionsLibraryTests.java`:350 | `getWait(driver).until(ExpectedConditions.visibilityOf(listQuestionsPage.getEditIDField(…` | `until` | unresolved | absent | b-lib | Selenium WebDriverWait.until |
| C1-036 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/QuestionsLibraryTests.java`:447 | `newEditSurveyPage.getQuestionSelectBox().sendKeys(randomTag2)` | `sendKeys` | unresolved | absent | b-lib | Selenium WebElement.sendKeys |
| C1-037 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RelationshipInAFTests.java`:274 | `assessmentFindingsGrid.waitForLoadingIndicator()` | `waitForLoadingIndicator` | unresolved | absent | a-repo | T-hier: local typed as the repo GridsPage page-object |
| C1-038 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:640 | `riskRankQuickEntryFormPage.getResponseForQuestion(2).clear()` | `clear` | unresolved | absent | b-lib | Selenium WebElement.clear |
| C1-039 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:703 | `boConfigsPage.getSuccessErrorMessage().getText().contains("Successfully")` | `contains` | unresolved | absent | c-platform | JDK String.contains |
| C1-040 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:820 | `sidebarMenuPage.getNavigationLink()` | `getNavigationLink` | unresolved | absent | a-repo | T-hier: method on the repo SidebarMenuPage page-object |
| C1-041 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:964 | `boConfigsPage.getCustomLabelField().clear()` | `clear` | unresolved | absent | b-lib | Selenium WebElement.clear |
| C1-042 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SortIgnoreLowerCaseSurveyTests.java`:71 | `logger.info("SortIgnoreLowerCaseSurveyTests completed on " + browser.toUpperCase())` | `info` | unresolved | absent | a-repo | T-hier: logger field typed as the repo LogUtils class |
| C1-043 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SortIgnoreLowerCaseSurveyTests.java`:160 | `new GridsPage(driver, "servicesGrid", usedColumns)` | `GridsPage` | unresolved | absent | a-repo | T-import: constructor of a repo page-object |
| C1-044 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SortIgnoreLowerCase_HelpMethods.java`:199 | `gridsPage.getWait(driver)` | `getWait` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-045 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SubscriptionTests.java`:228 | `manageSubscriptionPage.getAllowLinkingFiles().click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-046 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:113 | `gridsPageSurveys.waitForLoadingIndicator()` | `waitForLoadingIndicator` | unresolved | absent | a-repo | T-hier: field typed as the repo GridsPage page-object |
| C1-047 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:129 | `newEditSurveyPage.getSwitchers(1).click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-048 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:174 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement( newEditSurveyPage.ge…` | `until` | unresolved | absent | b-lib | Selenium WebDriverWait.until |
| C1-049 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:210 | `getSurveyName()` | `getSurveyName` | unresolved | absent | a-repo | T-hier: static helper inherited from repo BaseTest |
| C1-050 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:319 | `ExpectedConditions.visibilityOf(newEditSurveyPage.getSurveyRuleBtns(2))` | `visibilityOf` | import | absent | b-lib | Selenium ExpectedConditions; no visibilityOf in a tracked file |
| C1-051 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryTests.java`:223 | `TimeUtils.waitForSeconds(2)` | `waitForSeconds` | import | absent | a-repo | T-none: import tier already names the repo TimeUtils |
| C1-052 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryTests.java`:387 | `createEditOFRAPage.getNameField()` | `getNameField` | unresolved | absent | a-repo | T-hier: method on the repo CreateEditOFRAPage page-object |
| C1-053 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryTests.java`:471 | `surveyProgressPage.getAFNameField().sendKeys(randomTag)` | `sendKeys` | unresolved | absent | b-lib | Selenium WebElement.sendKeys |
| C1-054 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAFSystemsTests.java`:291 | `getWait(driver)` | `getWait` | unresolved | absent | a-repo | T-hier: inherited from the repo BaseTest base class |
| C1-055 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:48 | `TestsConfig.getConfig()` | `getConfig` | import | absent | a-repo | T-none: import tier already names the repo TestsConfig |
| C1-056 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:137 | `surveysMiniGrid.getWait(180, driver)` | `getWait` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-057 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:157 | `createEditOFRAPage.getPartOfGridName("vendors")` | `getPartOfGridName` | unresolved | absent | a-repo | T-hier: method on the repo CreateEditOFRAPage page-object |
| C1-058 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:297 | `gridsPage.getOkButton().click()` | `click` | unresolved | absent | b-lib | Selenium WebElement.click |
| C1-059 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:156 | `workflowsPage.getLabelField().sendKeys("Name")` | `sendKeys` | unresolved | absent | b-lib | Selenium WebElement.sendKeys |
| C1-060 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:246 | `TimeUtils.waitForSeconds(3)` | `waitForSeconds` | import | absent | a-repo | T-none: import tier already names the repo TimeUtils |
| C1-061 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:524 | `createEditOFRAPage.getOptions()` | `getOptions` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-062 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:634 | `systemPage.removeMessage()` | `removeMessage` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-063 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BOConfigsTests.java`:189 | `boConfigsPage.removeMessage()` | `removeMessage` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-064 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BOConfigsTests.java`:338 | `excludedBOType.substring(0,5)` | `substring` | unresolved | absent | c-platform | JDK String.substring |
| C1-065 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BOConfigsTests.java`:422 | `boConfigsPage.getLobMultiSelectMaxItemsInput().clear()` | `clear` | unresolved | absent | b-lib | Selenium WebElement.clear |
| C1-066 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BaseTest.java`:95 | `getWait(driver)` | `getWait` | unresolved | absent | a-repo | T-hier: same-class method on the repo BaseTest class |
| C1-067 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DDBOTests.java`:105 | `inputData.get()` | `get` | unresolved | absent | c-platform | JDK ThreadLocal.get |
| C1-068 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DDBOTests.java`:199 | `inputData.get()` | `get` | unresolved | absent | c-platform | JDK ThreadLocal.get |
| C1-069 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DataDrivenCUDBOsTests.java`:135 | `createEditOFRAPage.getNameField().sendKeys(createdBOName)` | `sendKeys` | unresolved | absent | b-lib | Selenium WebElement.sendKeys |
| C1-070 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DataDrivenCUDBOsTests.java`:140 | `createEditOFRAPage.getPartOfGridName("assets")` | `getPartOfGridName` | unresolved | absent | a-repo | T-hier: method on the repo CreateEditOFRAPage page-object |
| C1-071 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DataDrivenManageBOSubTypesGeneralTests.java`:181 | `ScrollToElement(boSubTypesPage.getDeleteSubType(), driver)` | `ScrollToElement` | unresolved | absent | a-repo | T-hier: static helper inherited from repo BaseTest |
| C1-072 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/ApplicationInfoPage.java`:219 | `applicationInfoData.getApplicationFlag()` | `getApplicationFlag` | unresolved | absent | a-repo | T-hier: getter on a repo dataEntities class |
| C1-073 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/ApplicationInfoPage.java`:241 | `getSoxFlagNoYes().get(0)` | `get` | unresolved | absent | c-platform | JDK List.get on a returned List |
| C1-074 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/ApplicationInfoPage.java`:312 | `clearAndSendKeys(getSourceURLField(),applicationInfoData.getSourceURL(), driver)` | `clearAndSendKeys` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-075 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditContractPage.java`:349 | `getInsuranceSelectBox()` | `getInsuranceSelectBox` | unresolved | absent | a-repo | T-hier: same-class getter on a repo page-object |
| C1-076 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditContractPage.java`:365 | `getServicesOnlyRadioBtns()` | `getServicesOnlyRadioBtns` | unresolved | absent | a-repo | T-hier: same-class getter on a repo page-object |
| C1-077 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditContractPage.java`:373 | `cnt.getContactName()` | `getContactName` | unresolved | absent | a-repo | T-hier: getter on a repo dataEntities class |
| C1-078 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditOFRAPage.java`:1145 | `ofraData.setClassName(selectClass())` | `setClassName` | unresolved | absent | a-repo | T-hier: setter on a repo dataEntities class |
| C1-079 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditOFRAPage.java`:1522 | `visibilityOf(getDatePicker())` | `visibilityOf` | unresolved | absent | b-lib | Selenium ExpectedConditions via static import |
| C1-080 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditOFRAPage.java`:1581 | `getResolutionReviewField()` | `getResolutionReviewField` | unresolved | absent | a-repo | T-hier: same-class getter on a repo page-object |
| C1-081 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/DDBOPage.java`:537 | `navigateToListPage(inputData)` | `navigateToListPage` | unresolved | absent | a-repo | T-hier: same-class helper on a repo page-object |
| C1-082 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/GridsPage.java`:517 | `certifyBtn.isDisplayed()` | `isDisplayed` | unresolved | absent | b-lib | Selenium WebElement.isDisplayed |
| C1-083 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/GridsPage.java`:1946 | `displaySingleGridRow(ofraData.getBoName())` | `displaySingleGridRow` | unresolved | absent | a-repo | T-hier: same-class helper on the repo GridsPage |
| C1-084 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/KnownVulnsInfoPage.java`:275 | `kvF.getEaseOfExploit()` | `getEaseOfExploit` | unresolved | absent | a-repo | T-hier: getter on a repo dataEntities class |
| C1-085 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/LoginPage.java`:38 | `driver.manage()` | `manage` | unresolved | absent | b-lib | Selenium WebDriver.manage |
| C1-086 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/MyWatchedItemsPage.java`:23 | `getSearchFieldForMyWatchedItems().isDisplayed()` | `isDisplayed` | unresolved | absent | b-lib | Selenium WebElement.isDisplayed |
| C1-087 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/RiskRankInfoPage.java`:214 | `getDunsNumber().sendKeys(data.getDunsNumber())` | `sendKeys` | unresolved | absent | b-lib | Selenium WebElement.sendKeys |
| C1-088 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/UserSessionsPage.java`:70 | `ls.get(i).getText().equals(name)` | `equals` | unresolved | absent | c-platform | JDK String.equals |
| C1-089 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/VendorInfoPage.java`:350 | `clearAndSendKeys(countryField, vendorInfoData.getCountry(), driver)` | `clearAndSendKeys` | unresolved | absent | a-repo | T-hier: inherited from the repo BasePage base class |
| C1-090 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/sikulix/test/BasicSikulixDemo.java`:43 | `new LoginPage(driver)` | `LoginPage` | unresolved | absent | a-repo | T-import: constructor of a repo page-object |
| C1-391 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/ab-compare.py`:14 | `l.strip()` | `strip` | unresolved | stub | c-platform | Python str.strip |
| C1-392 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-check.py`:110 | `len({len(l) for l in body})` | `len` | unresolved | stub | c-platform | Python builtin len |
| C1-393 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-check.py`:238 | `float(span)` | `float` | unresolved | stub | c-platform | Python builtin float |
| C1-394 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-check.py`:238 | `probes(span, step)` | `probes` | in-file | in-repo | a-repo | T-none: in-file tier already resolves this function |
| C1-395 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:53 | `len(_plain(str(title)))` | `len` | unresolved | stub | c-platform | Python builtin len |
| C1-396 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:72 | `type(r)` | `type` | unresolved | stub | c-platform | Python builtin type |
| C1-397 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:148 | `lanes.get(srv, 0)` | `get` | unresolved | stub | c-platform | Python dict.get |
| C1-398 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:259 | `round(float(sp), 1)` | `round` | unresolved | stub | c-platform | Python builtin round |
| C1-399 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/ledger-append.py`:67 | `os.path.join(r, "chosen-fields.tsv")` | `join` | unresolved | stub | c-platform | stdlib os.path.join |
| C1-400 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/ledger-append.py`:93 | `lines.append("")` | `append` | unresolved | stub | c-platform | Python list.append |
| C1-401 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/rebaseline.py`:29 | `open(p)` | `open` | unresolved | stub | c-platform | Python builtin open |
| C1-402 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/rebaseline.py`:41 | `os.path.exists(f)` | `exists` | unresolved | stub | c-platform | stdlib os.path.exists |
| C1-403 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:122 | `bar[:width].ljust(width)` | `ljust` | unresolved | stub | c-platform | Python str.ljust |
| C1-404 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:175 | `r.get("project")` | `get` | unresolved | stub | c-platform | Python dict.get on a parsed jsonl record |
| C1-405 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:268 | `int(now.get(k, 0) or 0)` | `int` | unresolved | stub | c-platform | Python builtin int |
| C1-406 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:344 | `exp.append(key)` | `append` | unresolved | stub | c-platform | Python list.append |
| C1-407 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:382 | `f(8)` | `f` | unresolved | in-repo | a-repo | T-none: engine in-repo is a name join to a local lambda |
| C1-408 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:435 | `os.path.isdir(tdir)` | `isdir` | unresolved | stub | c-platform | stdlib os.path.isdir |
| C1-409 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:459 | `os.path.basename(compare_run.rstrip('/'))` | `basename` | unresolved | stub | c-platform | stdlib os.path.basename |
| C1-410 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:500 | `", ".join(cmp_[1])` | `join` | unresolved | stub | c-platform | Python str.join |
| C1-411 | `QA/RobotTests/libraries/RiskConfigsDB.py`:229 | `by_sub.setdefault(d.get("subID"), [])` | `setdefault` | unresolved | stub | c-platform | Python dict.setdefault |
| C1-412 | `QA/RobotTests/libraries/RiskConfigsDB.py`:257 | `any((d.get("version") or 0) != 1 for d in startups)` | `any` | unresolved | stub | c-platform | Python builtin any |
| C1-413 | `QA/RobotTests/libraries/RiskConfigsDB.py`:296 | `range(num_levels)` | `range` | unresolved | stub | c-platform | Python builtin range |
| C1-414 | `QA/RobotTests/libraries/RiskConfigsDB.py`:306 | `doc.get("scoreBands", [])` | `get` | unresolved | stub | c-platform | Python dict.get |
| C1-415 | `QA/RobotTests/libraries/RiskConfigsDB.py`:308 | `self.expected_even_bands(len(bands))` | `expected_even_bands` | in-file | in-repo | a-repo | T-none: in-file tier already resolves this method |
| C1-416 | `QA/RobotTests/libraries/RiskConfigsDB.py`:318 | `bool(actual)` | `bool` | unresolved | stub | c-platform | Python builtin bool |
| C1-417 | `QA/RobotTests/libraries/RiskConfigsDB.py`:368 | `d.get("subID")` | `get` | unresolved | stub | c-platform | Python dict.get |
| C1-418 | `QA/RobotTests/libraries/RiskConfigsDB.py`:371 | `int(expected_version)` | `int` | unresolved | stub | c-platform | Python builtin int |
| C1-419 | `QA/RobotTests/resources/libraries/RegulatoryFixtures.py`:92 | `os.environ.get("RF_FIXTURE_MONGO_URL", "").strip()` | `strip` | unresolved | stub | c-platform | Python str.strip on an os.environ lookup |
| C1-420 | `QA/RobotTests/resources/libraries/SubDailyNRBackend.py`:74 | `self._sub_id(sub_id)` | `_sub_id` | in-file | in-repo | a-repo | T-none: in-file tier already resolves this method |
| C1-421 | `QA/RobotTests/resources/libraries/env_loader.py`:47 | `os.path.abspath(os.path.join(here, "..", ".."))` | `abspath` | unresolved | stub | c-platform | stdlib os.path.abspath |
| C1-422 | `QA/RobotTests/resources/libraries/env_loader.py`:57 | `print( "[env_loader] WARNING: could not read %s (%s: %s); " "continuing with the existi…` | `print` | unresolved | stub | c-platform | Python builtin print |
| C1-423 | `QA/RobotTests/resources/libraries/evidence.py`:61 | `cleaned.strip("_")` | `strip` | unresolved | stub | c-platform | Python str.strip |
| C1-424 | `QA/RobotTests/resources/libraries/fpst332_scenario_runner.py`:257 | `type(err)` | `type` | unresolved | stub | c-platform | Python builtin type |
| C1-425 | `QA/RobotTests/resources/libraries/regulatory_timer_core_diagnostics.py`:36 | `_driver()` | `_driver` | in-file | in-repo | a-repo | T-none: in-file tier already resolves this function |
| C1-426 | `QA/RobotTests/resources/libraries/regulatory_timer_core_evidence.py`:63 | `cleaned.replace("__", "_")` | `replace` | unresolved | stub | c-platform | Python str.replace |
| C1-427 | `QA/RobotTests/resources/libraries/regulatory_timer_core_evidence.py`:108 | `str(_variable("${TEST NAME}", ""))` | `str` | unresolved | stub | c-platform | Python builtin str |
| C1-428 | `cijobs/scripts/build-publish-payload.py`:30 | `read_package_json(archive)` | `read_package_json` | unresolved | in-repo | a-repo | T-none: engine already resolves this module-level function |
| C1-429 | `cijobs/scripts/build-publish-payload.py`:51 | `base64.b64encode(hashlib.sha512(data).digest()).decode("ascii")` | `decode` | unresolved | stub | c-platform | Python bytes.decode |
| C1-430 | `cijobs/scripts/registry-cleanup.py`:81 | `urllib.parse.urlencode(params)` | `urlencode` | unresolved | stub | c-platform | stdlib urllib.parse.urlencode |
| C1-431 | `playwright-validation/async-predicate-sweep-A.py`:88 | `max(0, m.start() - 90)` | `max` | unresolved | stub | c-platform | Python builtin max |
| C1-432 | `playwright-validation/inventory-clickA.py`:27 | `sorted(glob.glob(os.path.join(RD, "*.js")))` | `sorted` | unresolved | stub | c-platform | Python builtin sorted |
| C1-433 | `playwright-validation/inventory-clickA.py`:27 | `glob.glob(os.path.join(RD, "*.js"))` | `glob` | unresolved | stub | c-platform | stdlib glob.glob |
| C1-434 | `playwright-validation/schema-sweep-clickA.py`:76 | `len(src)` | `len` | unresolved | stub | c-platform | Python builtin len |
| C1-435 | `playwright-validation/schema-sweep-clickA.py`:87 | `re.compile(r'^(\s*)(?:"([\w.$\[\]]+)"\|\'([\w.$\[\]]+)\'\|([\w$]+))\s*(?::\s*)?(async\s…` | `compile` | import | stub | c-platform | stdlib re.compile; import tier names the module, not a file |
| C1-436 | `Meteor3preUpgradeScripts/meteor-async-migration/samples/meteor-2.9.ts`:35 | `Accounts.createUserVerifyingEmail()` | `createUserVerifyingEmail` | import | stub | b-lib | Meteor accounts-password; package source not tracked in-tree |
| C1-437 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props-withTracker.ts`:24 | `debug( `\n************************************************** *** ${fileInfo.path} *****…` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-438 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props-withTracker.ts`:64 | `j(p.value.arguments) .find(j.ReturnStatement)` | `find` | unresolved | stub | b-lib | jscodeshift Collection.find |
| C1-439 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props-withTracker.ts`:118 | `v.value.init.properties.map((sp) => { props[sp.key.name] = sp.value; })` | `map` | unresolved | stub | c-platform | Array.prototype.map on an AST property list |
| C1-440 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props.ts`:74 | `findImportNodeByVariableName( componentName, rootCollection, j )` | `findImportNodeByVariableName` | unresolved | in-repo | a-repo | T-none: engine already resolves this repo util function |
| C1-441 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-find-await-without-async.ts`:24 | `j(p).toSource()` | `toSource` | unresolved | stub | b-lib | jscodeshift Collection.toSource |
| C1-442 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-find-promise-all-foreach.ts`:18 | `j(fileInfo.source)` | `j` | unresolved | stub | b-lib | jscodeshift API handed to the transform |
| C1-443 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-find-promise-all-foreach.ts`:22 | `rootCollection.find(j.CallExpression)` | `find` | unresolved | stub | b-lib | jscodeshift Collection.find |
| C1-444 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-meteor-call.ts`:61 | `setFunctionAsync(parentFunction, j)` | `setFunctionAsync` | unresolved | in-repo | a-repo | T-none: engine already resolves this repo util function |
| C1-445 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-meteor-call.ts`:71 | `debug("**************************************************")` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-446 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-rename-functions.ts`:110 | `debug( "\n+++replace this", j(replaceThisPath).toSource(), p.value.loc?.start, p.value.…` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-447 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-rename-functions.ts`:116 | `j(byNode.value).toSource()` | `toSource` | unresolved | stub | b-lib | jscodeshift Collection.toSource |
| C1-448 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-rename-functions.ts`:195 | `rootCollection.find(j.MemberExpression).map((p) => { if ( p.value.object.type === "Iden…` | `map` | unresolved | stub | b-lib | jscodeshift Collection.map |
| C1-449 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-use-async-function.ts`:139 | `findParentObject(p.parentPath)` | `findParentObject` | unresolved | in-repo | a-repo | T-none: engine already resolves this repo util function |
| C1-450 | `Meteor3preUpgradeScripts/meteor-async-migration/transform.ts`:47 | `debug(`***************************************************** ${fileInfo.path} *********…` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-451 | `Meteor3preUpgradeScripts/meteor-async-migration/transform.ts`:137 | `debug("variable declaration:", node.declaration.declarations)` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-452 | `Meteor3preUpgradeScripts/meteor-async-migration/transform.ts`:183 | `j(xp)` | `j` | unresolved | stub | b-lib | jscodeshift API handed to the transform |
| C1-453 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:21 | `require("debug")` | `require` | unresolved | stub | c-platform | Node CommonJS require |
| C1-454 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:85 | `debug( "____found in parent component props:", parentComponentProps[expression.name] )` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-455 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:378 | `debug("[handleComponent] _context usage parent path:", p.parentPath)` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-456 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:479 | `decl.id.properties.map((idProp) => { if ( idProp.type == "ObjectProperty" && idProp.key…` | `map` | unresolved | stub | c-platform | Array.prototype.map on an AST property list |
| C1-457 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:532 | `j(componentPath).find(j.JSXElement)` | `find` | unresolved | stub | b-lib | jscodeshift Collection.find |
| C1-458 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:532 | `j(componentPath).find(j.JSXElement).paths()` | `paths` | unresolved | stub | b-lib | jscodeshift Collection.paths |
| C1-459 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:557 | `debug( "[handleComponent] _child component at:", theComponent?.value.loc.start )` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |
| C1-460 | `Meteor3preUpgradeScripts/meteor-async-migration/utils.ts`:363 | `debug( `convert all functions use the async function which has the name is ${name} to a…` | `debug` | unresolved | stub | b-lib | npm debug package logger instance |

### D. Second corpus — Python, TSX/TypeScript, Go/Java/JavaScript/C

| id | file:line | call expression | callee | syntax | precise | class | reason |
|---|---|---|---|---|---|---|---|
| C2-001 | `apps/runner/redglass_runner/fuzz_canary/Fuzz_Honggfuzz/harness.c`:17 | `memcpy(buf, data, size)` | `memcpy` | unresolved | unjoined-file-not-indexed | c-platform | C string.h memcpy in a fuzz harness |
| C2-002 | `archive/dashboard/console/public/mockServiceWorker.js`:227 | `values.filter( (value) => value !== 'msw/passthrough', )` | `filter` | unresolved | unjoined-file-not-indexed | c-platform | Array.prototype.filter on a split/map result |
| C2-003 | `archive/demo/probe/fixtures/vulnerable-package/src/index.js`:1 | `require("lodash")` | `require` | unresolved | unjoined-file-not-indexed | c-platform | Node CommonJS require, not the repo function |
| C2-004 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/onboard.go`:164 | `huh.NewGroup( huh.NewInput(). Title("LangSmith API Key"). Placeholder("lsv2_..."). Echo…` | `NewGroup` | unresolved | unjoined-file-not-indexed | b-lib | charmbracelet/huh form builder |
| C2-005 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/onboard.go`:173 | `huh.ThemeFunc(ui.RedglassTheme)` | `ThemeFunc` | unresolved | unjoined-file-not-indexed | b-lib | charmbracelet/huh theme constructor |
| C2-006 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/onboard_test.go`:39 | `providerCredentialEnv(provider)` | `providerCredentialEnv` | unresolved | unjoined-file-not-indexed | d-unknown | no definition in any tracked file; archived tree is partial |
| C2-007 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/remove.go`:105 | `os.RemoveAll(home)` | `RemoveAll` | import | unjoined-file-not-indexed | c-platform | Go stdlib os.RemoveAll |
| C2-008 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/start.go`:150 | `ui.DimText("CLI exited. Services kept running — run 'redglass stop' to shut down.")` | `DimText` | import | unjoined-file-not-indexed | a-repo | T-import: ui package function, in-tree archived corpus |
| C2-009 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/update.go`:61 | `c.Pull(targetVersion)` | `Pull` | unresolved | unjoined-file-not-indexed | a-repo | T-hier: c is a local of repo type Compose, archived tree |
| C2-010 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/config/config.go`:55 | `os.Open(path)` | `Open` | import | unjoined-file-not-indexed | c-platform | Go stdlib os.Open |
| C2-011 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/engagement/picker_test.go`:49 | `mkBareDir(t, home, "charlie")` | `mkBareDir` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier binds a helper in the archived tree |
| C2-012 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/engagement/picker_test.go`:170 | `validateSlug(home, "acme-2026")` | `validateSlug` | unresolved | unjoined-file-not-indexed | a-repo | T-import: same Go package sibling file, archived tree |
| C2-013 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/health/health.go`:65 | `bytes.NewReader(body)` | `NewReader` | import | unjoined-file-not-indexed | c-platform | Go stdlib bytes.NewReader |
| C2-014 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/health/health.go`:91 | `time.Now()` | `Now` | import | unjoined-file-not-indexed | c-platform | Go stdlib time.Now |
| C2-015 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/ui/theme.go`:135 | `lipgloss.Color("#FF0000")` | `Color` | unresolved | unjoined-file-not-indexed | b-lib | charmbracelet/lipgloss Color |
| C2-016 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/updater/updater.go`:122 | `io.ReadAll(resp.Body)` | `ReadAll` | import | unjoined-file-not-indexed | c-platform | Go stdlib io.ReadAll |
| C2-017 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/updater/updater.go`:160 | `fmt.Errorf("get executable path: %w", err)` | `Errorf` | import | unjoined-file-not-indexed | c-platform | Go stdlib fmt.Errorf |
| C2-018 | `packages/infra/redglass_infra/analysis/intercept_profiles/jvm/RedglassInterceptAgent.java`:277 | `"#profile:".length()` | `length` | unresolved | unjoined-file-not-indexed | c-platform | java.lang.String.length on a literal receiver |
| C2-019 | `packages/infra/redglass_infra/analysis/intercept_profiles/jvm/RedglassInterceptAgent.java`:289 | `byType.computeIfAbsent(sink.type, t -> new ArrayList<>()).add(sink)` | `add` | unresolved | unjoined-file-not-indexed | c-platform | java.util.List.add on computeIfAbsent's ArrayList |
| C2-020 | `packages/infra/redglass_infra/analysis/intercept_profiles/node/preload.js`:123 | `Array.prototype.slice.call(args)` | `call` | unresolved | unjoined-file-not-indexed | c-platform | Function.prototype.call via Array.prototype.slice |
| C2-021 | `apps/runner/tests/conftest.py`:288 | `FakeGraph()` | `FakeGraph` | in-file | joined-defined | a-repo | T-none: join resolves; in-file FakeGraph class |
| C2-022 | `apps/runner/tests/test_run_loop.py`:124 | `run_loop( run_id="run-1", drive=_drive_two_steps, fold=_fold, event_log=log, clock=Froz…` | `run_loop` | unresolved | joined-defined | a-repo | T-none: join resolves; tier missed the from-import |
| C2-023 | `archive/docs/reference/source-corpus/files/tests/unit/research/test_graph.py`:36 | `g.upsert_node(Node.make(NodeKind.HOST, "10.0.0.1", os="linux"))` | `upsert_node` | unresolved | joined-defined | a-repo | T-none: join resolves; g = KnowledgeGraph() receiver |
| C2-024 | `archive/src/redglass/adapters/sandbox/local_process.py`:578 | `handle.output.finish( timeout_seconds=( timeout_seconds if timeout_seconds is not None …` | `finish` | unresolved | joined-defined | a-repo | T-none: join resolves; handle.output capture object |
| C2-025 | `packages/infra/redglass_infra/enrichment/mirror/sync.py`:327 | `ArchiveFetchError("sync cursor JSON is not an object")` | `ArchiveFetchError` | unresolved | joined-defined | a-repo | T-none: join resolves; error class imported in-repo |
| C2-026 | `packages/infra/redglass_infra/sandbox/lifecycle_failures.py`:331 | `error.retain_cleanup_diagnostic( f"capture finalization: {capture_error}" )` | `retain_cleanup_diagnostic` | unresolved | joined-defined | a-repo | T-none: join resolves; method on a repo failure object |
| C2-027 | `packages/infra/tests/test_binary_recon_parsers.py`:700 | `parse_sinkxrefs(repeated, tool=AnalysisTool.BINARY_SINKXREFS)` | `parse_sinkxrefs` | unresolved | joined-defined | a-repo | T-none: join resolves; imported repo parser function |
| C2-028 | `packages/infra/tests/test_recording_source.py`:240 | `used.finalize_misses( { "pkg:deb/debian/libssl1.1@1.1": _INSUFF, "pkg:deb/debian/libssl…` | `finalize_misses` | unresolved | joined-defined | a-repo | T-none: join resolves; method on a repo miss recorder |
| C2-029 | `packages/pipeline/tests/test_analysis_services.py`:152 | `_StubPort( _source_run(exit_code=1, timed_out=False, findings=[_FINDING]) )` | `_StubPort` | unresolved | joined-defined | a-repo | T-none: join resolves; _StubPort imported from a helper |
| C2-030 | `packages/pipeline/tests/test_usage_summary_projection.py`:164 | `UsageSummaryProjection().rebuild(log, run_id="run-1")` | `rebuild` | unresolved | joined-defined | a-repo | T-none: join resolves; UsageSummaryProjection() receiver |
| C2-031 | `scripts/substrate-vm/guest-delegate.py`:989 | `_frame(digest, _read_root_file(path))` | `_frame` | in-file | joined-defined | a-repo | T-none: join resolves; in-file _frame helper |
| C2-032 | `tests/architecture/test_taint_image_pins.py`:168 | `_string_constant(_PROVISIONER_SRC, "_WHEEL_SHA256")` | `_string_constant` | in-file | joined-defined | a-repo | T-none: join resolves; in-file _string_constant helper |
| C2-033 | `apps/api/redglass_api/api/v1/routes/evidence_category.py`:49 | `tuple( EvidenceCategoryFacet(category=category, count=count) for category, count in ind…` | `tuple` | unresolved | joined-external | c-platform | builtin tuple |
| C2-034 | `apps/runner/redglass_runner/health/report.py`:181 | `isinstance(status, _Unreadable)` | `isinstance` | unresolved | joined-external | c-platform | builtin isinstance |
| C2-035 | `archive/docs/reference/source-corpus/files/redglass/tools/defense/tools.py`:452 | `section_match.group(1)` | `group` | unresolved | joined-external | c-platform | re.Match.group from stdlib re |
| C2-036 | `packages/infra/redglass_infra/provisioning/bundler.py`:341 | `extracted.mkdir(mode=0o700)` | `mkdir` | unresolved | joined-external | c-platform | pathlib.Path.mkdir |
| C2-037 | `packages/infra/redglass_infra/provisioning/cargo_policy.py`:108 | `isinstance(source, str)` | `isinstance` | unresolved | joined-external | c-platform | builtin isinstance |
| C2-038 | `packages/infra/redglass_infra/research_egress/capture.py`:157 | `document.get("outcomes")` | `get` | ambiguous | joined-external | c-platform | dict.get on a dict comprehension result |
| C2-039 | `packages/infra/tests/test_codeql_extraction_completeness.py`:455 | `(_FIXTURES / "codeql_diagnostics_cpp_buildless.json").read_bytes()` | `read_bytes` | unresolved | joined-external | c-platform | pathlib.Path.read_bytes |
| C2-040 | `packages/pipeline/redglass_pipeline/analysis/observed_effects.py`:221 | `ValueError("a NETWORK_ATTEMPT observation must record blocked")` | `ValueError` | unresolved | joined-external | c-platform | builtin ValueError |
| C2-041 | `packages/pipeline/tests/test_analysis_services.py`:399 | `pytest.raises(ValueError, match="non-binary")` | `raises` | import | joined-external | b-lib | pytest.raises context manager |
| C2-042 | `packages/tools/tests/test_joern_aderyn_cpg_tools.py`:136 | `len(out)` | `len` | unresolved | joined-external | c-platform | builtin len |
| C2-043 | `tests/architecture/test_subprocess_calls_are_bounded.py`:252 | `bool(node.args)` | `bool` | unresolved | joined-external | c-platform | builtin bool |
| C2-044 | `tests/contract/test_target_intake_contract.py`:38 | `len(body)` | `len` | unresolved | joined-external | c-platform | builtin len |
| C2-045 | `apps/api/redglass_api/api/v1/routes/assurance.py`:31 | `router.get("/runs/{run_id}/assurance", response_model=RunAssuranceView)` | `get` | unresolved | unjoined-in-indexed-file | b-lib | FastAPI APIRouter.get route decorator |
| C2-046 | `apps/api/tests/test_create_run_route.py`:210 | `client.post("/v1/runs", json=_request_body(sandbox="local"))` | `post` | unresolved | unjoined-in-indexed-file | b-lib | starlette TestClient.post; _client returns TestClient |
| C2-047 | `apps/api/tests/test_fuzz_health_route.py`:96 | `self.inner.read_fuzz_prefix(run_id=run_id)` | `read_fuzz_prefix` | in-file | unjoined-in-indexed-file | a-repo | T-hier: self.inner is a declared InMemoryEventLog field |
| C2-048 | `apps/api/tests/test_fuzz_health_route.py`:122 | `payload.model_dump(mode="json")` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump on a repo model |
| C2-049 | `apps/api/tests/test_read_routes.py`:219 | `KgProjector(log, kg).project()` | `project` | unresolved | unjoined-in-indexed-file | a-repo | T-flow: KgProjector() chained return receiver |
| C2-050 | `apps/api/tests/test_read_routes.py`:449 | `stale.json()` | `json` | unresolved | unjoined-in-indexed-file | b-lib | httpx Response.json from the test client |
| C2-051 | `apps/api/tests/test_usage_cost_route.py`:233 | `_client(_UsageStore(view), _SelectivePricebook(), tmp_path).get( "/v1/runs/run-1/usage/…` | `get` | unresolved | unjoined-in-indexed-file | b-lib | starlette TestClient.get; _client returns TestClient |
| C2-052 | `apps/cli/redglass_cli/composition/supervisor.py`:191 | `supervisor.admission_loop()` | `admission_loop` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: supervisor is a declared repo Supervisor |
| C2-053 | `apps/cli/tests/test_replay_command.py`:168 | `runner.invoke( app, ["replay", "--run-id", "run-reject"] )` | `invoke` | unresolved | unjoined-in-indexed-file | b-lib | typer/click CliRunner.invoke |
| C2-054 | `apps/runner/redglass_runner/fuzz_canary_preflight.py`:837 | `evidence_store.get(Sha256Digest(digest))` | `get` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: evidence_store is a Protocol EvidenceStorePort |
| C2-055 | `apps/runner/redglass_runner/run_risk.py`:128 | `reachability.rank_subgraph(subgraph, run_id=run_id)` | `rank_subgraph` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: reachability param is ReachabilityRankingService |
| C2-056 | `apps/runner/redglass_runner/run_start.py`:876 | `bundle.path(ref)` | `path` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: bundle is a declared repo RunBundle |
| C2-057 | `apps/runner/redglass_runner/streaming/bus_publishing_event_log.py`:70 | `self._inner.append_many(events)` | `append_many` | in-file | unjoined-in-indexed-file | a-repo | T-hier: self._inner is a declared repo event log field |
| C2-058 | `apps/runner/redglass_runner/supervisor/reconcile.py`:181 | `RunBundle(run_root / run_id).path(STATE_DB_NAME)` | `path` | unresolved | unjoined-in-indexed-file | a-repo | T-flow: RunBundle() chained return receiver |
| C2-059 | `apps/runner/redglass_runner/supervisor/supervisor.py`:754 | `pid_path.read_text(encoding="utf-8")` | `read_text` | unresolved | unjoined-in-indexed-file | c-platform | pathlib.Path.read_text on the bundle path |
| C2-060 | `apps/runner/tests/supervisor/test_creation.py`:282 | `bundle.path(STATE_DB_NAME)` | `path` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: bundle is a declared repo RunBundle |
| C2-061 | `apps/runner/tests/test_bootstrap_catalog.py`:355 | `web_fetch.invoke( {"source": "package_meta", "resource_id": _UNKNOWN_PURL} )` | `invoke` | unresolved | unjoined-in-indexed-file | b-lib | LangChain StructuredTool.invoke on a catalog tool |
| C2-062 | `apps/runner/tests/test_bootstrap_pattern_registry.py`:175 | `CrossRunValidatedPatternRegistry(root=root).register( ValidatedPatternEntry( signature=…` | `register` | unresolved | unjoined-in-indexed-file | a-repo | T-flow: registry constructor chained return receiver |
| C2-063 | `apps/runner/tests/test_coverage_disclosure_persistence.py`:132 | `bundle.path(COVERAGE_DISCLOSURES_REF)` | `path` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: bundle is a declared repo RunBundle |
| C2-064 | `apps/runner/tests/test_coverage_disclosures_reach_reporting_role.py`:449 | `AnalysisExecutedPayload.model_validate(log.read()[-1].payload)` | `model_validate` | import | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_validate classmethod |
| C2-065 | `apps/runner/tests/test_projection_fold.py`:287 | `store.get_agent_transcript("run-1")` | `get_agent_transcript` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: store is a repo SqliteReadModelStore local |
| C2-066 | `apps/runner/tests/test_run_replay.py`:563 | `report.model_copy( update={ "validated_findings": ( original.model_copy(update={"severi…` | `model_copy` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_copy |
| C2-067 | `apps/runner/tests/test_run_replay.py`:607 | `view.model_copy(update={"rejected_total": view.rejected_total + 99})` | `model_copy` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_copy |
| C2-068 | `apps/runner/tests/test_run_replay.py`:837 | `bundle.path(RUN_MANIFEST_REF).unlink()` | `unlink` | unresolved | unjoined-in-indexed-file | c-platform | pathlib.Path.unlink on the bundle path |
| C2-069 | `apps/runner/tests/test_supervised_run.py`:305 | `bundle.path(DRIVE_FAILURE_REF)` | `path` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: bundle is a declared repo RunBundle |
| C2-070 | `apps/runner/tests/test_supervised_run.py`:327 | `bundle.path(SUPERVISED_LOCK_REF)` | `path` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: bundle is a declared repo RunBundle |
| C2-071 | `archive/containers/manage.py`:289 | `typer.echo(f"ollama api: reachable ({OLLAMA_API_BASE})")` | `echo` | import | unjoined-in-indexed-file | b-lib | typer.echo |
| C2-072 | `archive/demo/probe/src/redglass/control_plane/run_state.py`:155 | `run_state.model_copy( update={ "status": _run_status_from_lifecycle_state(snapshot.stat…` | `model_copy` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_copy |
| C2-073 | `archive/demo/probe/tests/test_agent_provider_runtime.py`:148 | `KnowledgeGraphBuilder(json_store=store).build(bundle)` | `build` | unresolved | unjoined-in-indexed-file | a-repo | T-flow: KnowledgeGraphBuilder() chained return receiver |
| C2-074 | `archive/demo/probe/tests/test_intake_inventory.py`:251 | `Clock.fixed(FIXED_TIME)` | `fixed` | import | unjoined-in-indexed-file | a-repo | T-none: tier resolves Clock via the import edge |
| C2-075 | `archive/demo/probe/tests/test_intake_inventory.py`:453 | `Clock.fixed(FIXED_TIME)` | `fixed` | import | unjoined-in-indexed-file | a-repo | T-none: tier resolves Clock via the import edge |
| C2-076 | `archive/demo/probe/tests/test_langgraph_workflow.py`:20 | `ComponentAnalysisGraph.default(allow_noop_handlers=True)` | `default` | import | unjoined-in-indexed-file | a-repo | T-none: tier resolves the graph class via the import |
| C2-077 | `archive/demo/probe/tests/test_langgraph_workflow.py`:60 | `ComponentAnalysisGraph.default( checkpoint_store=store, allow_noop_handlers=True, )` | `default` | import | unjoined-in-indexed-file | a-repo | T-none: tier resolves the graph class via the import |
| C2-078 | `archive/demo/probe/tests/test_provider_workspace_bridge.py`:365 | `json_store.write( bundle.path("artifacts/generated-sbom.cdx.json"), {"bomFormat": "Cycl…` | `write` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: json_store is a repo JsonStore local |
| C2-079 | `archive/demo/probe/tests/test_report_claims.py`:174 | `JsonStore().append( bundle.path("sandbox/run-results.jsonl"), _valid_sandbox_result(), )` | `append` | unresolved | unjoined-in-indexed-file | a-repo | T-flow: JsonStore() chained return receiver |
| C2-080 | `archive/demo/probe/tests/test_run_bundle.py`:245 | `hash_service.sha256_bytes(copied)` | `sha256_bytes` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: hash_service is a declared repo hashing service |
| C2-081 | `archive/docs/reference/source-corpus/files/config/perplexity_handler.py`:206 | `resp.json()` | `json` | unresolved | unjoined-in-indexed-file | b-lib | httpx/requests Response.json |
| C2-082 | `archive/docs/reference/source-corpus/files/redglass/tools/research/tools.py`:221 | `pkg_path.startswith("node_modules/")` | `startswith` | unresolved | unjoined-in-indexed-file | c-platform | str.startswith on a dict key |
| C2-083 | `archive/docs/reference/source-corpus/files/redglass/tools/research/tools.py`:1017 | `row.get("status-code")` | `get` | unresolved | unjoined-in-indexed-file | c-platform | dict.get on an untyped JSON row |
| C2-084 | `archive/docs/reference/source-corpus/files/redglass/tools/research/tools.py`:1890 | `row.get("cname")` | `get` | unresolved | unjoined-in-indexed-file | c-platform | dict.get on an untyped JSON row |
| C2-085 | `archive/docs/reference/source-corpus/files/tests/unit/llm/test_factory.py`:114 | `monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-real-12345")` | `setenv` | unresolved | unjoined-in-indexed-file | b-lib | pytest MonkeyPatch.setenv |
| C2-086 | `archive/docs/reference/source-corpus/files/tests/unit/observability/test_observability.py`:100 | `s.set_attribute("key", "value")` | `set_attribute` | unresolved | unjoined-in-indexed-file | d-unknown | span() yields an OTel span or the repo _NoopSpan |
| C2-087 | `archive/src/redglass/adapters/llm/handlers/codex_chatgpt.py`:152 | `fn.get( "parameters", {"type": "object", "properties": {}}, )` | `get` | unresolved | unjoined-in-indexed-file | c-platform | dict.get on an untyped tool-schema dict |
| C2-088 | `archive/src/redglass/entrypoints/cli/analyze.py`:582 | `typer.Option( "none", "--component-limit", help="Component admission limit: none or an …` | `Option` | import | unjoined-in-indexed-file | b-lib | typer.Option |
| C2-089 | `archive/src/redglass/entrypoints/cli/app.py`:14 | `typer.Typer( name="redglass", help="Run local Redglass component analysis.", no_args_is…` | `Typer` | import | unjoined-in-indexed-file | b-lib | typer.Typer |
| C2-090 | `archive/src/redglass/services/cloud/analyses.py`:112 | `e.model_dump(mode="json")` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump |
| C2-091 | `deploy/analysis/rust_recall_runner.py`:186 | `store.assert_ready_for_launch()` | `assert_ready_for_launch` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: store is a declared repo provisioning store |
| C2-092 | `deploy/analysis/source_provenance_runner.py`:519 | `driver.get("rules", [])` | `get` | unresolved | unjoined-in-indexed-file | c-platform | dict.get on an untyped SARIF driver dict |
| C2-093 | `deploy/analysis/triton_harness.py`:556 | `inst.isTainted()` | `isTainted` | unresolved | unjoined-in-indexed-file | b-lib | triton symbolic-execution Instruction.isTainted |
| C2-094 | `packages/agent_runtime/redglass_agent_runtime/orchestration/pause_state.py`:324 | `snapshot.config.get("configurable", {})` | `get` | unresolved | unjoined-in-indexed-file | c-platform | dict.get on a LangGraph snapshot config mapping |
| C2-095 | `packages/agent_runtime/redglass_agent_runtime/roles/smart_contract/definition.py`:29 | `catalog.advertised_for(Role.SMART_CONTRACT)` | `advertised_for` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: catalog is a declared repo tool catalog |
| C2-096 | `packages/agent_runtime/redglass_agent_runtime/roles/source_vuln/definition.py`:29 | `catalog.advertised_for(Role.SOURCE_VULN)` | `advertised_for` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: catalog is a declared repo tool catalog |
| C2-097 | `packages/agent_runtime/tests/test_dispatch.py`:577 | `tool.ainvoke( { "role": "source_vuln", "task": "scan", "component_id": DEFAULT_TEST_PUR…` | `ainvoke` | unresolved | unjoined-in-indexed-file | b-lib | LangChain tool.ainvoke |
| C2-098 | `packages/agent_runtime/tests/test_escalate_to_validation.py`:144 | `log.append( EventEnvelope( schema_version="1", event_id=ids.new_id(), sequence=0, times…` | `append` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: log is a repo InMemoryEventLog local |
| C2-099 | `packages/agent_runtime/tests/test_run_pause_barrier.py`:264 | `log.read()` | `read` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: log is a repo InMemoryEventLog local |
| C2-100 | `packages/contracts/tests/test_read_model_contracts.py`:97 | `FuzzResourceEvidence.model_validate(values)` | `model_validate` | import | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_validate classmethod |
| C2-101 | `packages/infra/redglass_infra/analysis/runtime_overhead_certificates.py`:72 | `certificate.model_dump( mode="json", exclude={"certificate_identity"} )` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump |
| C2-102 | `packages/infra/redglass_infra/analysis/taint_adapter.py`:395 | `self._sandbox.wait(handle)` | `wait` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: self._sandbox is a Protocol SandboxBackendPort |
| C2-103 | `packages/infra/redglass_infra/artifacts/package.py`:78 | `value.startswith("pkg:pypi/")` | `startswith` | unresolved | unjoined-in-indexed-file | c-platform | str.startswith on a purl value |
| C2-104 | `packages/infra/redglass_infra/enrichment/mirror/manager.py`:483 | `self._clock.now().isoformat()` | `isoformat` | unresolved | unjoined-in-indexed-file | c-platform | datetime.isoformat on the clock's instant |
| C2-105 | `packages/infra/redglass_infra/http/client.py`:218 | `httpx.HTTPTransport(retries=self._CONNECT_RETRIES)` | `HTTPTransport` | import | unjoined-in-indexed-file | b-lib | httpx.HTTPTransport |
| C2-106 | `packages/infra/redglass_infra/llm/catalog.py`:147 | `LeadRoleBands.model_validate(bands)` | `model_validate` | import | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_validate classmethod |
| C2-107 | `packages/infra/redglass_infra/persistence/coverage_disclosure_store.py`:767 | `event.payload.get("locator_object_digest")` | `get` | unresolved | unjoined-in-indexed-file | c-platform | Mapping.get on an event payload |
| C2-108 | `packages/infra/redglass_infra/research_egress/assembly.py`:130 | `self._capture.put(coordinate, cached)` | `put` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: self._capture is a Protocol capture port field |
| C2-109 | `packages/infra/tests/test_baseline_firing.py`:71 | `result.matched_advisory("pkg:github/madler/zlib@1.2.8")` | `matched_advisory` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: result is a declared repo baseline result |
| C2-110 | `packages/infra/tests/test_mirror_manager.py`:568 | `snapshot.try_link_version(exdev_bundle)` | `try_link_version` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: snapshot is a declared repo mirror snapshot |
| C2-111 | `packages/infra/tests/test_taint_emit_artifact_states.py`:763 | `_emit.read_published_report(out)` | `read_published_report` | unresolved | unjoined-in-indexed-file | a-repo | T-import: _emit is an imported repo module alias |
| C2-112 | `packages/pipeline/redglass_pipeline/knowledge/projection_readers.py`:108 | `event.payload.get("advisory_osv_id")` | `get` | unresolved | unjoined-in-indexed-file | c-platform | Mapping.get on an event payload |
| C2-113 | `packages/pipeline/redglass_pipeline/validation/taint_evidence.py`:333 | `payload.model_dump(include=fields)` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump |
| C2-114 | `packages/pipeline/tests/test_pending_gate_rules.py`:536 | `denials[0].payload.get("fingerprint")` | `get` | unresolved | unjoined-in-indexed-file | c-platform | Mapping.get on an event payload |
| C2-115 | `packages/pipeline/tests/test_report_projection.py`:261 | `synthetic_analyzer_result_recorder( log, FrozenClock(), ids ).record_execution( str(_RU…` | `record_execution` | unresolved | unjoined-in-indexed-file | a-repo | T-flow: recorder factory chained return receiver |
| C2-116 | `packages/pipeline/tests/test_reversing_recon_floor.py`:215 | `executed[0].model_dump( include=set(ToolExecutionEvidenceWire.model_fields) )` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump |
| C2-117 | `packages/pipeline/tests/test_toolchain_provision.py`:181 | `AnalysisProvenanceWire.model_validate( { "mode": AnalysisExtractionMode.BUILDLESS.value…` | `model_validate` | import | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_validate classmethod |
| C2-118 | `packages/pipeline/tests/test_usage_summary_projection_determinism.py`:74 | `payload.model_dump()` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump |
| C2-119 | `packages/pipeline/tests/test_work_state_determinism.py`:52 | `AgentArtifactRef( class_tag=ArtifactClassTag.ASSIGNMENT, digest=digest, schema_version=…` | `model_dump` | unresolved | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_dump on a constructed model |
| C2-120 | `packages/testing/redglass_testing/contracts/event_log.py`:152 | `log.append(harness.make_event(EventKind.SUPPRESSED))` | `append` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: log is a Protocol EventLogPort parameter |
| C2-121 | `packages/tools/redglass_tools/primitives/filesystem.py`:222 | `StructuredTool.from_function( func=_ls, name=ToolName.LS.value, description=( "Scan and…` | `from_function` | import | unjoined-in-indexed-file | b-lib | LangChain StructuredTool.from_function |
| C2-122 | `packages/tools/redglass_tools/research/advisory_lookup.py`:29 | `service.advisories(ecosystem, name)` | `advisories` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: service is a declared AdvisoryLookupService |
| C2-123 | `packages/tools/redglass_tools/research/advisory_lookup.py`:77 | `StructuredTool.from_function( func=_lookup, name=ToolName.EPSS_LOOKUP.value, descriptio…` | `from_function` | import | unjoined-in-indexed-file | b-lib | LangChain StructuredTool.from_function |
| C2-124 | `packages/tools/redglass_tools/tracing/tracing.py`:98 | `StructuredTool.from_function( func=_run, name=ToolName.TRACE_SYSCALLS.value, descriptio…` | `from_function` | import | unjoined-in-indexed-file | b-lib | LangChain StructuredTool.from_function |
| C2-125 | `packages/tools/tests/test_analysis_delegates.py`:311 | `tool.invoke({"target": "Vault.sol"})` | `invoke` | unresolved | unjoined-in-indexed-file | b-lib | LangChain tool.invoke |
| C2-126 | `packages/tools/tests/test_matrix_delegates.py`:551 | `tools[ToolName.FORGE_TEST].invoke( { "harness": "contract Harness { function invariant_…` | `invoke` | unresolved | unjoined-in-indexed-file | b-lib | LangChain tool.invoke via a registry lookup |
| C2-127 | `scripts/smokes/smoke_frida_intercept_firing.py`:274 | `bundle.staged_target_path()` | `staged_target_path` | unresolved | unjoined-in-indexed-file | a-repo | T-hier: bundle is a declared repo RunBundle |
| C2-128 | `scripts/smokes/smoke_real_size_analysis.py`:358 | `AnalyzerImageResolver.default()` | `default` | import | unjoined-in-indexed-file | a-repo | T-none: tier resolves the resolver via the import edge |
| C2-129 | `scripts/smokes/smoke_source_language_certificates.py`:417 | `output.write_text(_SOLIDITY_CLEAN, encoding="utf-8")` | `write_text` | unresolved | unjoined-in-indexed-file | c-platform | pathlib.Path.write_text |
| C2-130 | `tests/architecture/test_engine_canary_smoke.py`:189 | `smoke_engine_canary._certify_resource_exceptions( image="localhost/redglass-sandbox@sha…` | `_certify_resource_exceptions` | unresolved | unjoined-in-indexed-file | a-repo | T-import: smoke module alias imported in-repo |
| C2-131 | `tests/architecture/test_recipe_driver_registry.py`:59 | `default_recipe_drivers(emit=_discard).values()` | `values` | unresolved | unjoined-in-indexed-file | c-platform | Mapping.values; the factory returns a Mapping |
| C2-132 | `tests/architecture/test_role_tool_grants.py`:140 | `GRANTS.values()` | `values` | import | unjoined-in-indexed-file | c-platform | dict.values on the imported GRANTS mapping |
| C2-133 | `tests/architecture/test_source_language_certificate.py`:74 | `source.is_file()` | `is_file` | unresolved | unjoined-in-indexed-file | c-platform | pathlib.Path.is_file |
| C2-134 | `tests/architecture/test_tool_execution_evidence_totality.py`:655 | `CoverageDisclosureManifest.model_json_schema()` | `model_json_schema` | import | unjoined-in-indexed-file | b-lib | pydantic BaseModel.model_json_schema classmethod |
| C2-135 | `.claude/skills/claude-md-audit/evals/scripts/eval_harness.py`:297 | `banner( "setup", iteration=current_iteration, tests=f"{len(tests)} tests x 2 variants =…` | `banner` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the banner helper |
| C2-136 | `.claude/skills/claude-md-audit/evals/scripts/eval_harness.py`:537 | `task.stderr_handle.close()` | `close` | unresolved | unjoined-file-not-indexed | c-platform | io file object close on a stored handle |
| C2-137 | `.claude/skills/claude-md-audit/evals/scripts/eval_harness.py`:880 | `sum(result["total"] for result in results)` | `sum` | unresolved | unjoined-file-not-indexed | c-platform | builtin sum |
| C2-138 | `.claude/skills/claude-md-audit/scripts/discover.py`:357 | `stripped[2:].strip().strip("\"'")` | `strip` | unresolved | unjoined-file-not-indexed | c-platform | str.strip |
| C2-139 | `.claude/skills/claude-md-audit/scripts/discover.py`:723 | `_write_phase_summary(phase_data)` | `_write_phase_summary` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the helper |
| C2-140 | `.claude/skills/llm-redteam/harness/config_generator.py`:51 | `plugin.startswith(REMOTE_ONLY_PREFIXES)` | `startswith` | unresolved | unjoined-file-not-indexed | c-platform | str.startswith |
| C2-141 | `.claude/skills/llm-redteam/harness/config_generator.py`:94 | `path.startswith("$")` | `startswith` | unresolved | unjoined-file-not-indexed | c-platform | str.startswith |
| C2-142 | `.claude/skills/llm-redteam/harness/config_generator.py`:121 | `list(dict.fromkeys(p for p in plugins if not _is_remote_only(p)))` | `list` | unresolved | unjoined-file-not-indexed | c-platform | builtin list over dict.fromkeys |
| C2-143 | `.claude/skills/llm-redteam/harness/engine.py`:139 | `native_grader.grade(probe.output, asserts)` | `grade` | import | unjoined-file-not-indexed | a-repo | T-none: tier resolves the grader module via the import |
| C2-144 | `.claude/skills/llm-redteam/harness/recon.py`:142 | `path.read_text(encoding="utf-8", errors="ignore")` | `read_text` | unresolved | unjoined-file-not-indexed | c-platform | pathlib.Path.read_text |
| C2-145 | `.claude/skills/llm-redteam/harness/recon.py`:149 | `any(p.search(line) for p in patterns)` | `any` | unresolved | unjoined-file-not-indexed | c-platform | builtin any |
| C2-146 | `.claude/skills/llm-redteam/harness/recon.py`:302 | `any(p.search(line) for p in patterns)` | `any` | unresolved | unjoined-file-not-indexed | c-platform | builtin any |
| C2-147 | `.claude/skills/llm-redteam/harness/recon.py`:322 | `vectors.items()` | `items` | unresolved | unjoined-file-not-indexed | c-platform | dict.items |
| C2-148 | `.claude/skills/llm-redteam/harness/runner.py`:88 | `isinstance(status, int)` | `isinstance` | unresolved | unjoined-file-not-indexed | c-platform | builtin isinstance |
| C2-149 | `.claude/skills/llm-redteam/harness/runner.py`:180 | `RunnerError("npx not found on PATH — run the Node prereq check first")` | `RunnerError` | unresolved | unjoined-file-not-indexed | a-repo | T-import: from .errors import RunnerError, tier missed it |
| C2-150 | `.claude/skills/llm-redteam/harness/runner.py`:223 | `subprocess.run( # nosec B603 # fixed argv, abs npx, no shell cmd, cwd=str(gc.config_pat…` | `run` | import | unjoined-file-not-indexed | c-platform | subprocess.run from stdlib |
| C2-151 | `.claude/skills/llm-redteam/harness/target_client.py`:134 | `TargetError(f"refusing non-http(s) target URL: {url}")` | `TargetError` | unresolved | unjoined-file-not-indexed | a-repo | T-import: from .errors import TargetError, tier missed it |
| C2-152 | `.claude/skills/llm-redteam/harness/target_client.py`:140 | `resp.read().decode("utf-8", "ignore")` | `decode` | unresolved | unjoined-file-not-indexed | c-platform | bytes.decode on the HTTP response body |
| C2-153 | `.claude/skills/llm-redteam/tests/_poison_server.py`:66 | `"\n".join(out)` | `join` | unresolved | unjoined-file-not-indexed | c-platform | str.join |
| C2-154 | `.claude/skills/llm-redteam/tests/_poison_server.py`:180 | `self._json(200, {"reply": f"Answer.\n{rendered}".rstrip()})` | `_json` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier binds the handler's own _json |
| C2-155 | `.claude/skills/llm-redteam/tests/test_cli.py`:202 | `str(fixtures_dir / "sample_source")` | `str` | unresolved | unjoined-file-not-indexed | c-platform | builtin str on a Path |
| C2-156 | `.claude/skills/llm-redteam/tests/test_descriptor.py`:61 | `descriptor.load(p)` | `load` | import | unjoined-file-not-indexed | a-repo | T-none: tier resolves the descriptor module via import |
| C2-157 | `.claude/skills/llm-redteam/tests/test_poison_cli.py`:96 | `(out_dir / "…").read_text()` | `read_text` | unresolved | unjoined-file-not-indexed | c-platform | pathlib.Path.read_text |
| C2-158 | `.claude/skills/llm-redteam/tests/test_poison_executor.py`:219 | `_descriptor(srv.server_address[1])` | `_descriptor` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the fixture helper |
| C2-159 | `.claude/skills/llm-redteam/tests/test_poison_executor.py`:556 | `_chunk(b"IDAT", idat)` | `_chunk` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the chunk helper |
| C2-160 | `.claude/skills/llm-redteam/tests/test_poisoning.py`:115 | `load_workflow(_write(tmp_path, cmd))` | `load_workflow` | unresolved | unjoined-file-not-indexed | a-repo | T-import: imported from the repo harness package |
| C2-161 | `.claude/skills/llm-redteam/tests/test_report.py`:90 | `md.splitlines()` | `splitlines` | unresolved | unjoined-file-not-indexed | c-platform | str.splitlines |
| C2-162 | `.claude/skills/llm-redteam/tests/test_target_client.py`:52 | `self.send_response(200)` | `send_response` | unresolved | unjoined-file-not-indexed | c-platform | http.server BaseHTTPRequestHandler.send_response |
| C2-163 | `.claude/skills/llm-redteam/tests/test_target_client.py`:161 | `_descriptor(9)` | `_descriptor` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the fixture helper |
| C2-164 | `.claude/skills/llm-redteam/tests/test_target_client.py`:179 | `models.AuthSpec(type="none", tenant_header="X-Tenant-Id")` | `AuthSpec` | import | unjoined-file-not-indexed | a-repo | T-none: tier resolves the models module via the import |
| C2-165 | `apps/console/src/api/events.test.ts`:21 | `StreamEventsV1RunsRunIdEventsGetResponse.safeParse(FRAME)` | `safeParse` | import | unjoined-file-not-indexed | b-lib | zod ZodType.safeParse on a generated schema |
| C2-166 | `apps/console/src/api/liveSync.ts`:176 | `useRef(0)` | `useRef` | unresolved | unjoined-file-not-indexed | b-lib | React useRef hook |
| C2-167 | `apps/console/src/app/routeComponents.tsx`:92 | `lazy(() => import("../views/graph/AttackGraphView") .then((m) => ({ default: m.AttackGr…` | `lazy` | unresolved | unjoined-file-not-indexed | b-lib | React.lazy |
| C2-168 | `apps/console/src/views/advisory/AdvisoryLedgerView.test.tsx`:172 | `expect(screen.queryByText("no recorded deviation"))` | `expect` | unresolved | unjoined-file-not-indexed | b-lib | vitest expect |
| C2-169 | `apps/console/src/views/agents/OrchestrationOverlay.test.tsx`:69 | `refused.getByTitle(`declined near ${SECRET}`)` | `getByTitle` | unresolved | unjoined-file-not-indexed | b-lib | testing-library getByTitle on a render result |
| C2-170 | `apps/console/src/views/agents/OrchestrationOverlay.test.tsx`:75 | `view(null)` | `view` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the view helper |
| C2-171 | `apps/console/src/views/agents/SpecialistArtifactPanel.test.tsx`:64 | `expect(label.textContent)` | `expect` | unresolved | unjoined-file-not-indexed | b-lib | vitest expect |
| C2-172 | `apps/console/src/views/analysis/BinaryReconPanel.test.tsx`:178 | `expect(screen.getByText(/partial: yes/)).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | b-lib | jest-dom toBeInTheDocument matcher |
| C2-173 | `apps/console/src/views/analysis/FuzzHealthPanel.test.tsx`:211 | `screen.getByText("artifacts")` | `getByText` | import | unjoined-file-not-indexed | b-lib | testing-library screen.getByText |
| C2-174 | `apps/console/src/views/analysis/RunAssurancePanel.test.tsx`:270 | `expect(screen.getByText("closure not clean")).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | b-lib | jest-dom toBeInTheDocument matcher |
| C2-175 | `apps/console/src/views/analysis/ToolExecutionReceipt.tsx`:188 | `recorded(artifact?.bundle_path)` | `recorded` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the recorded helper |
| C2-176 | `apps/console/src/views/authz/AuthzCoverageView.test.tsx`:77 | `expect(new Set(classes).size).toBeGreaterThan(1)` | `toBeGreaterThan` | unresolved | unjoined-file-not-indexed | b-lib | vitest toBeGreaterThan matcher |
| C2-177 | `apps/console/src/views/dynamic-confirmation/DynamicConfirmationView.test.tsx`:115 | `render(<DynamicConfirmationView runId="run-1" />)` | `render` | unresolved | unjoined-file-not-indexed | b-lib | render from the testing-library react package |
| C2-178 | `apps/console/src/views/graph/ForceGraph.test.tsx`:18 | `vi.fn()` | `fn` | import | unjoined-file-not-indexed | b-lib | vitest vi.fn |
| C2-179 | `apps/console/src/views/graph/danger.test.ts`:134 | `expect(reachabilityTier(undefined)).toBe("unknown")` | `toBe` | unresolved | unjoined-file-not-indexed | b-lib | vitest toBe matcher |
| C2-180 | `apps/console/src/views/graph/danger.test.ts`:143 | `nodeShape({ kind: "Entrypoint", is_entrypoint: true })` | `nodeShape` | unresolved | unjoined-file-not-indexed | a-repo | T-import: nodeShape imported from the repo danger module |
| C2-181 | `apps/console/src/views/graph/feeder.test.ts`:35 | `feeder.merge(view([{ id: "a", is_dangerous_sink: true }]))` | `merge` | unresolved | unjoined-file-not-indexed | a-repo | T-hier: feeder is a local of repo type GraphFeeder |
| C2-182 | `apps/console/src/views/history/compare.test.ts`:39 | `expect(t.latest.run_id)` | `expect` | unresolved | unjoined-file-not-indexed | b-lib | vitest expect |
| C2-183 | `apps/console/src/views/home/HomeView.tsx`:13 | `useRunCatalog()` | `useRunCatalog` | unresolved | unjoined-file-not-indexed | a-repo | T-import: useRunCatalog imported from api/catalog |
| C2-184 | `apps/console/src/views/overview/ElapsedTime.tsx`:66 | `formatDuration(seconds)` | `formatDuration` | unresolved | unjoined-file-not-indexed | a-repo | T-import: formatDuration imported from the design module |
| C2-185 | `apps/console/src/views/overview/RunControlButtons.test.tsx`:48 | `screen.getByRole("button", { name: /pause/i })` | `getByRole` | import | unjoined-file-not-indexed | b-lib | testing-library screen.getByRole |
| C2-186 | `apps/console/src/views/runtime-trace/RuntimeTraceView.test.tsx`:489 | `expect(screen.getByRole("alert")).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | b-lib | jest-dom toBeInTheDocument matcher |
| C2-187 | `apps/console/src/views/taint/TaintCoverage.test.tsx`:153 | `expect( await screen.findByRole("heading", { name: "Taint coverage" }), )` | `expect` | unresolved | unjoined-file-not-indexed | b-lib | vitest expect |
| C2-188 | `archive/dashboard/console/src/api/liveSync.ts`:56 | `keysFor(runId, ev.kind)` | `keysFor` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds keysFor |
| C2-189 | `archive/dashboard/console/src/contract/schema.test.ts`:117 | `expect(r.attack_paths[0].crown_jewel_id).toBe("n-crown")` | `toBe` | unresolved | unjoined-file-not-indexed | b-lib | vitest toBe matcher |
| C2-190 | `archive/dashboard/console/src/contract/schema.test.ts`:179 | `runRejectedSchema.parse({ reason: "concurrency_cap", detail: "max 3", max_concurrent: 3…` | `parse` | import | unjoined-file-not-indexed | b-lib | zod schema.parse |
| C2-191 | `archive/dashboard/console/src/contract/schema.test.ts`:222 | `traceStepSchema.parse({ ...base, type: "reasoning", thinking_detected: true, redacted: …` | `parse` | import | unjoined-file-not-indexed | b-lib | zod schema.parse |
| C2-192 | `archive/dashboard/console/src/contract/schema.test.ts`:249 | `expect(() => componentInventorySchema.parse({ run_id: "r", components: [withoutSourceRe…` | `expect` | unresolved | unjoined-file-not-indexed | b-lib | vitest expect |
| C2-193 | `archive/dashboard/console/src/contract/schema.ts`:171 | `z.object({ type: z.literal("message"), agent_id: z.string(), seq: z.number(), ts: iso, …` | `object` | import | unjoined-file-not-indexed | b-lib | zod z.object |
| C2-194 | `archive/dashboard/console/src/design/elapsed.test.ts`:36 | `expect(elapsedSeconds(from, Date.parse("")))` | `expect` | unresolved | unjoined-file-not-indexed | b-lib | vitest expect |
| C2-195 | `archive/dashboard/console/src/mock/fixtures/findings.ts`:63 | `finding({ finding_id: "FIND-003", title: "Suspected UAF in reset path", severity: "high…` | `finding` | in-file | unjoined-file-not-indexed | a-repo | T-none: in-file tier already binds the finding factory |
| C2-196 | `archive/dashboard/console/src/views/findings/TierChips.tsx`:14 | `onToggle(tier)` | `onToggle` | unresolved | unjoined-file-not-indexed | d-unknown | onToggle is a prop callback supplied by the caller |
| C2-197 | `archive/dashboard/console/src/views/findings/TierChips.tsx`:17 | `active.has(tier)` | `has` | unresolved | unjoined-file-not-indexed | c-platform | Set.prototype.has; active is typed Set of tiers |
| C2-198 | `archive/dashboard/console/src/views/home/useChangeHighlight.ts`:34 | `new Set()` | `Set` | unresolved | unjoined-file-not-indexed | c-platform | Set constructor from the JS runtime |
| C2-199 | `archive/dashboard/console/src/views/overview/OverviewView.tsx`:85 | `useLiveSync(runId, events, liveness)` | `useLiveSync` | unresolved | unjoined-file-not-indexed | a-repo | T-import: useLiveSync imported from api/liveSync |
| C2-200 | `archive/dashboard/console/src/views/usage/UsageView.test.tsx`:32 | `expect(screen.getByText(/USD not computed/)).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | b-lib | jest-dom toBeInTheDocument matcher |
| C2-201 | `archive/docs/reference/source-corpus/files/clients/cli/src/components/messages/DelegateMessage.tsx`:19 | `React.memo(function DelegateMessage({ agent, content, }: Props) { const screen = useScr…` | `memo` | import | unjoined-file-not-indexed | b-lib | React.memo |
| C2-202 | `archive/docs/reference/source-corpus/files/clients/cli/src/hooks/useAgent.ts`:193 | `setRunState("idle")` | `setRunState` | unresolved | unjoined-file-not-indexed | b-lib | React useState setter from a destructured hook result |
| C2-203 | `archive/docs/reference/source-corpus/files/clients/cli/src/index.tsx`:7 | `args.includes("--resume")` | `includes` | unresolved | unjoined-file-not-indexed | c-platform | Array.prototype.includes |
| C2-204 | `archive/docs/reference/source-corpus/files/clients/web/server/terminal-server.ts`:31 | `(process.env.TERMINAL_ALLOWED_ORIGINS ?? `http://localhost:${WEB_PORT},http://127.0.0.1…` | `filter` | unresolved | unjoined-file-not-indexed | c-platform | Array.prototype.filter on a split/map chain |

## 6. The precise join, measured end to end on the nine-language fixture

The Section 11.3 join is not a projection here: the product was run on the polyglot tools-matrix
fixture — permitted, because a fixture is not a repository — with every pinned analyzer installed,
each indexer invoked through the product's own argv, and the resulting store read with
`sqlite3 -readonly`. Pinned versions at the time of the run: `scip-go 0.2.7`, `scip-typescript 0.4.0`,
`scip-python 0.6.6`, `scip-java 0.13.1`, `scip-clang 0.4.0`, `rust-analyzer 2026-09-14`, with
`clangd 22.1.6`, `gopls 0.23.0`, `jdtls 1.61.0`, `ty 0.0.81`,
`typescript-language-server 6.0.0`, `node 22.23.2` and `jdk 21.0.12.1+1` in the same store —
`tools status` reported **14 entries: 14 installed** and `tools verify` rehashed them against the lock.
The dependence provider was disabled for the run, so nothing below is the engine's.

| fixture project | languages | tree-sitter call sites | joined at compiler precision | joined **and** callee defined in the fixture |
|---|---|---|---|---|
| `c` (a compilation database) | c, cpp | 9 | 8 (88.9%) | 4 |
| `go` (a module) | go | 6 | 5 (83.3%) | 3 |
| `java` (a Maven layout) | java | 6 | **6 (100%)** | 1 |
| `js` (**package.json only, no compiler configuration**) | javascript | 2 | 2 (100%) | 2 |
| `py` (a project file) | python | 4 | 4 (100%) | 3 |
| `rust` (a crate) | rust | 6 | 6 (100%) | 2 |
| `ts` (a tsconfig) | typescript, tsx, javascript | 12 | 10 (83.3%) | 8 |
| **all nine advertised languages** | | **45** | **41 (91.1%)** | **23** |

**The four unjoined sites, named, because four is small enough to name:** `static_cast<int>(…)` in the
C++ file (a keyword operator the grammar records as a call site — it is not a call), `len(…)` in the Go
file (a language built-in), and `require("path")` and `path.join(…)` in a JavaScript file (the host
module loader and a standard-library method). **None of them targets a definition in the fixture.**

**So on this fixture, 23 of 23 call sites whose callee is defined in the repository are resolved at
compiler precision — 100%.** That is the measurement behind the class-(a) target, and it covers every
one of the nine advertised languages rather than one language family.

**Two method notes, recorded rather than smoothed over.**

1. The Java leg needed a `pom.xml` and the C/C++ leg a compilation database, because those are the
   profile triggers; with neither present the two profiles are not planned at all and their languages
   fall back to the syntax tier. This is a **configuration** boundary, not a capability one, and it is
   exactly what separates class (a) from class (b).
2. The C/C++ leg additionally required the compilation database's `directory` fields to be
   **absolute**. The private materialization relocates an absolute `directory` into the copy and
   deliberately leaves a relative one alone, so a hand-written database spelled `"directory": "."`
   reaches the indexer unchanged and it exits 1 in under 20 ms. Both behaviours are as documented;
   the interaction is worth knowing before a corpus is prepared.

## 7. What the producers resolve today, measured on the whole population

The sample bounds the denominator; where a compiler index exists the denominator needs no sample at
all, and the producer shares can be counted over every site rather than estimated. Both tables below
are population counts, not estimates.

**Class (b), the reference repository — every one of its 555,588 tree-sitter call sites.** A site
counts as engine-resolved when an engine `calls` site at the same byte range (or an overlapping range
with the same callee name) names a callee that has a file of its own.

| producer | sites resolved to an in-repo definition | share of all call sites |
|---|---|---|
| syntax tier (in-file or via import) | 75,751 | 13.63% |
| engine, matched onto a tree-sitter site | 38,779 | 6.98% |
| precise tier | 458 | 0.08% |
| **union of all three** | **98,081** | **17.65%** |
| (both syntax and engine) | 16,449 | 2.96% |

Per language: javascript 94,180 of 526,393 (17.89%), java 3,432 of 26,416 (12.99% — the engine's Java
unit failed, so the syntax tier is alone there), python 347 of 2,291 (15.15%), typescript 122 of 488
(25.00%).

**Class (a), the second corpus — its 48,302 compiler-confirmed in-repo-targeted Python call sites.**
This is the honest denominator counted rather than sampled: each of these sites has a compiler-precision
occurrence at the callee identifier whose symbol has a definition occurrence inside a repository file.

| producer | resolves | share of the in-repo-targeted denominator |
|---|---|---|
| syntax tier | 25,226 | **52.23%** |
| engine | 21,179 | **43.85%** |
| syntax ∪ engine | 27,544 | **57.02%** |
| precise join | 48,302 | **100%** |

Read the two together and the shape of the answer is already visible: **without a precise index the
two non-precise producers together reach about three in five of the calls that actually have an
in-repo target, and with one they reach all of them.** The unconfigured corpus is worse than that
because its majority language has no profile at all, not because its calls are harder.

**The name join is not the missing technique.** On the reference repository **70.59%** of all call
sites carry a callee name that some in-repo definition also carries, and among the 82.8% the syntax
tier leaves unresolved the figure is **65.68%**. A resolver keyed on the name alone would therefore
claim two sites in three and be wrong on most of them — which is precisely why the engine's exact
full-name equality join lands at 18.7% of all sites rather than at 70%. Every technique named in §9
is a way of **constraining** that name join with a receiver type, a field table or a hierarchy; none
of them is a way of relaxing it.


## 8. The result: the honest denominator, and what is resolved against it

Stratified estimator throughout; each stratum weighted by its population share, intervals from a
4,000-draw stratified bootstrap. `a-repo` is **the callee's definition is in a file this repository
tracks** — a vendored or bundled dependency's own source counts, because the producers resolve into
those files and a denominator has to admit what the numerator counts.

### 8.1 The honest denominator

| corpus / class | in-repo-targeted (`a-repo`) | 95% interval | in sites |
|---|---|---|---|
| reference repository — class (b), unconfigured | **49.92%** | 44.42 – 55.33 | ≈ 277,300 of 555,588 |
| second corpus, all languages | **40.88%** | 39.62 – 42.22 | ≈ 55,500 of 135,714 |
| …its configured language — class (a) | **42.81%** | 42.04 – 43.57 | ≈ 52,700 of 123,029 |
| …its unconfigured languages — class (b) | **22.15%** | 11.38 – 34.39 | ≈ 2,800 of 12,685 |

The other half of each corpus is `b-lib` (24.65% / 11.63%), `c-platform` (24.79% / 47.16%) and
`d-unknown` (0.63% / 0.33%) — calls for which **"no in-repo definition" is the correct answer**, not a
miss. Per language on the reference repository: javascript 49.67% [44.05, 55.29], java 58.89%
[48.56, 68.49], python 13.33% [6.26, 26.18], typescript 12.00% [4.17, 29.96].

**So the published 18.7% divides by a denominator roughly twice the honest one.** Against call sites
that actually have an in-repo target, the same producers reach the shares below.

### 8.2 The four shares

| resolved by | class (b) — reference repository | class (a) — second corpus's configured language |
|---|---|---|
| syntax tier | **22.85%** [16.70, 29.56] | 25.48% [3.15, 48.83] |
| engine | **10.89%** [6.31, 15.88] | 23.83% [1.21, 46.93] |
| precise join | **0.00%** (one profile ran, over 17 files) | **91.70%** [90.16, 93.40] |
| **union of all three** | **28.60%** [21.98, 35.48] | **94.26%** [92.75, 95.75] |

The class-(b) union is also available as a population count rather than an estimate — §7 gives 98,081
sites, 17.65% of all call sites, which against the 49.92% denominator is 35.4%; the sample's 28.60% is
lower because the sample **withdraws the numerator's own false positives**, which §8.3 measures.

### 8.3 A correction the sample forces on the numerator: `import` is not a resolution

The syntax tier publishes two resolved states. Checked against the hand class:

| tier verdict | reference repository | second corpus |
|---|---|---|
| `in-file` | **36 of 36** are `a-repo` | **16 of 16** are `a-repo` |
| `import` | **9 of 16** | **9 of 38** |

`in-file` is sound in every one of the 52 rows that carry it: the tier found the definition in the same
file. **`import` is not a resolution to a definition at all** — it matches the callee's base name
against an import statement, so `import re`, `import org.testng.Assert`, `from httpx import Response`,
`import typer` and `React.memo` all publish `resolution: import` while **no definition of that name
exists in any tracked file**. Corrected, the syntax tier resolves about **12.4%** of the reference
repository's call sites rather than 13.63%, and about **21.8%** of the second corpus's rather than
29.12%. Every figure of the form "the tree-sitter tier resolves X%" in the program's documents carries
this over-claim and is corrected in doc 14 and doc 20 alongside this file.

### 8.4 Why the reference repository is not a hard case — it is an unconfigured, half-vendored one

Two properties, both measured, and neither a property of its calls:

1. **Unconfigured.** Its majority language, 94.8% of its call sites, has no `tsconfig.json` anywhere in
   the tree, so no precise profile is planned for it and the precise column above is 0.00% by
   configuration rather than by capability.
2. **Half-vendored.** Its dependencies are checked in. 128,205 of its call sites are in one bundled
   front-end template alone, and in the sample **78 of block B's 83 `a-repo` rows and most of block A's
   66 are bundle-internal calls**. That is why its honest denominator is as high as 49.92% while only
   about a twentieth of its calls are its own team's code calling its own team's code.

A reader must not carry 49.92% to a repository that installs its dependencies instead of committing
them. The denominator is a property of the repository; the **shares against it** are the transferable
quantity, and those are what the targets are set on.

## 9. What would resolve the rest, technique by technique, language family by language family

Every `a-repo` row carries the technique that would resolve it. The shares are of the in-repo-targeted
population, so they sum to 100% and the cumulative column is the reachable ceiling after that
technique is added.

| technique | class (b) — reference repository | cumulative | class (a) — configured language | cumulative |
|---|---|---|---|---|
| `T-none` — resolved today | 28.60% [21.55, 35.71] | 28.60% | 93.91% [92.37, 95.42] | 93.91% |
| `T-import` — follow the module edge to a definition (not the name match of §8.3) | 4.32% [1.62, 7.71] | 32.93% | 0.79% | 94.71% |
| `T-field` — field-based resolution: property name → the repository definitions bound to it | **29.73%** [23.05, 37.15] | 62.66% | 0.00% | 94.71% |
| `T-flow` — flow-based type inference through assignments, parameters, returns | **27.41%** [20.56, 34.13] | **90.07%** | 1.02% | 95.73% |
| `T-hier` — class-hierarchy analysis through a declared or inferred repository type | 8.66% [5.54, 12.43] | 98.73% | **4.27%** [2.92, 5.59] | **100%** |
| `T-demand` — demand-driven resolution at read time | 1.27% [0.00, 3.22] | 100% | 0.00% | 100% |

**Each technique is a rule per language family, not per repository.**

| technique | language families it is stated for | the nine advertised languages it applies to |
|---|---|---|
| `T-import` | every family with a module or package system | all nine — ES modules and CommonJS (javascript, typescript, tsx), `import`/`from … import` (python), package imports (go, java), `use` (rust), `#include` (c, cpp) |
| `T-field` | families where a callable is stored in a named property of an object, prototype or class body | javascript, typescript, tsx, python primarily; degenerate but sound for java, go, rust, c, cpp, where a member name is already a declared member |
| `T-flow` | every family — it is the intraprocedural forward propagation over the SSA def-use chains decision 2 already builds, assigning each value a candidate set | all nine. It is the **only** technique that supplies a receiver type in the dynamic families, and it is what makes `T-field` and `T-hier` applicable at all |
| `T-hier` | families with a subtype relation | java, typescript, tsx (classes and interfaces), python (classes and `Protocol`), c++ (virtual dispatch), rust (traits), go (interfaces); inapplicable to c |
| `T-demand` | every family — it refines only the site a query names, so it carries no index-time budget | all nine |

**The name join is not among them, and §7 says why:** 70.59% of the reference repository's call sites
carry a callee name that some tracked definition also carries, so a resolver keyed on the name alone
claims two sites in three and is wrong on most. Every technique above **constrains** that join with a
receiver type, a field table or a hierarchy.

## 10. The targets these measurements support

**Class (a) — a configured typed repository** (a project configuration present and the precise
indexer run): **≥ 95% of in-repo-targeted call sites resolved at compiler precision through the
existing call-site join.** Supported, and the qualification is named rather than buried:

- On the nine-language fixture, where every project is configured and every dependency is resolvable,
  the join resolves **23 of 23** in-repo-targeted call sites — **100%**, across all nine languages.
- On the second corpus's configured language the join alone reaches **91.70%** and the union of the
  three producers **94.26% [92.75, 95.75]** — at the edge of the target, not past it. The gap is
  **10,475 sites the precise unit did not answer for**, and the sample says what they are: 2,384 in
  files the indexer never indexed, and 8,091 in files it did index where **all 90 sampled rows are
  attribute calls whose receiver the indexer could not type** — a third of them members of packages
  absent from the index environment, a third `Any`-typed receivers, a third repository objects reached
  through an un-inferred binding.
- The residue's own composition closes it: **4.27% of in-repo-targeted sites need class-hierarchy
  analysis through a declared repository type** and 1.02% flow inference, which takes the cumulative to
  **100%**. And the 94.26% union decomposes as **91.70 pp from the join**, 0.91 pp from the sound
  `in-file` state and **1.31 pp from the `import` state** that §8.3 measures at 9 of 38 — so the
  defensible floor for what is resolved today on this class is **92.61%**, which is below the target
  by more than the inference is worth guessing about. So ≥ 95% is honest **for the join plus the index-time inference of this plan**, and is
  **not** honest for the join alone on a repository whose precise unit skips files or runs without the
  dependency environment. The target is stated on the pair.

**Class (b) — a typed repository without configuration, or a dynamic-language repository:** **≥ 85% of
in-repo-targeted call sites resolved by language-general inference.** Supported, and the arithmetic is
the table in §9: 28.60% is resolved today, field-based resolution adds **29.73 pp** and flow-based
inference **27.41 pp**, which reaches **90.07%** — above the target with the interval's lower end at
roughly 80% and class-hierarchy analysis a further 8.66 pp in reserve. **The two techniques that carry
the target are named and neither is optional.** The last **1.27%** is `d-unknown` plus the sites whose
callee identity depends on a call-site-specific value; those are the demand-driven residue and are the
reason the target is 85% and not 100%.

## 11. Method limitations, stated rather than smoothed over

1. **The engine verdict is a lower bound.** 129,576 of the engine's 203,896 distinct call-site ranges
   match a tree-sitter range exactly (63.6%); a sampled site whose engine verdict is `absent` may be a
   site the engine lowered to a different range.
2. **The `T-hier`/`T-flow` boundary is a convention, and it is the largest single lever on §9's
   histogram.** Fixed across all four blocks as: a receiver with a **declared** repository type on a
   field, local or parameter is `T-hier`; a receiver that is a **chained return value** is `T-flow`.
   A reviewer using "any receiver type that must be inferred is `T-flow`" would move on the order of
   40 rows between the two. It does not move the cumulative ceiling, only the split.
3. **`T-none` is mechanical**, read off the instruments rather than re-judged, so a row the engine
   resolved by a name join that happens to be right still counts as resolved today. Two rows in the
   reference sample carry that caveat in their own cell.
4. **The small strata are indicative.** typescript on the reference repository is 25 rows of a
   488-site stratum weighing 0.09%; its per-language interval is wide and it moves the overall
   estimate by hundredths of a point.
5. **One snippet was elided.** `C2-157`'s call expression contains a file name that the repository's
   own tracked-reference check matches; the literal is replaced with `…` and nothing else about the
   row changed. The row was **not** dropped, because dropping it would break the seeded draw.
6. **Two corpora, one host, one engine version, one set of pinned indexer versions.** Re-measure when
   any of them changes.

## 12. Corrections this file makes to the program's earlier records

1. **`14-store-counts-r3.md` Q5.2 — "scip-java produced no occurrence at all at the call site" is
   wrong.** Run through the product's own argv on a Maven-layout fixture, scip-java 0.13.1 joins
   **6 of 6** Java call sites at compiler precision. The earlier finding came from a different fixture.
2. **`14-store-counts-r3.md` Q5.4 — "SCIP cannot help" with call-through-a-value is wrong at scale.**
   What remains true is that a call *edge* is not derivable from occurrence roles; the call **site**
   must come from the grammar. But the *join* works precisely on those sites: on the second corpus
   **79,154 of 95,780 syntax-unresolved call sites (82.6%) carry a compiler-precision occurrence at
   the callee identifier**.
3. **"A perfect precise run over this repository reaches roughly one call site in twenty" is a
   property of that repository's configuration, not of the precise tier.** The same tier covers
   **112,554 of 135,714 call sites (82.9%)** on a corpus whose majority language carries a project
   file, **91.5%** of that language's own sites, and **41 of 45** on the nine-language fixture.
4. **The syntax tier's `import` state is not a resolution** — §8.3.
5. **The per-scope observability gap recorded as Correction 3 in doc 14 reproduces on the second
   store.** Its `scip` capabilities are `fresh` with empty details while only one of its several
   eligible profiles produced a unit; the profiles that did not run leave no `units` row, no
   `provider_runs` row and no per-scope capability row. The gap is a property of the product, not of
   either corpus.
