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
| C1-113 | `QA/SeleniumWebdriver/TestngExtentFramework/doc/script-dir/jquery-3.5.1.min.js`:2 | `t()` | `t` | in-file | absent | b-lib | internal helper inside the vendored jQuery bundle |
| C1-114 | `QA/SeleniumWebdriver/TestngExtentFramework/doc/script-dir/jquery-ui.min.js`:6 | `this._delay(function(){var i=!t.contains(this.element[0],t.ui.safeActiveElement(this.do…` | `_delay` | unresolved | absent | b-lib | jQuery UI widget method inside the vendored bundle |
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
| C1-128 | `app/client/components/slickGrid/slickGrid.js`:2068 | `grid.getCanvasNode()` | `getCanvasNode` | unresolved | absent | b-lib | SlickGrid grid API method |
| C1-129 | `app/client/components/slickGrid/slickGrid.js`:4376 | `$(grid.getCanvasNode())` | `$` | unresolved | absent | b-lib | jQuery factory call |
| C1-130 | `app/client/components/workflow/simpleWorkflow.js`:38 | `$(document).on("click", function(evt) { popoverOffClick(evt); })` | `on` | unresolved | absent | b-lib | jQuery on() on a wrapped document |
| C1-131 | `app/client/components/workflow/workflow.js`:134 | `Template.instance()` | `instance` | unresolved | absent | b-lib | Blaze Template.instance |
| C1-132 | `app/client/imports/amcharts/amcharts.js`:188 | `n.click(function(a){h.handleGraphEvent(a,"clickGraph")})` | `click` | unresolved | absent | b-lib | internal call inside the vendored amcharts bundle |
| C1-133 | `app/client/imports/amcharts/amcharts.js`:190 | `d.setCN(f,n,this.bcn+"stroke")` | `setCN` | unresolved | absent | b-lib | AmCharts namespace helper inside the vendored bundle |
| C1-134 | `app/client/imports/amcharts/ammap.js`:21 | `d.formatNumber(a,g)` | `formatNumber` | unresolved | absent | b-lib | AmCharts namespace helper inside the vendored ammap bundle |
| C1-135 | `app/client/imports/amcharts/ammap.js`:70 | `Math.round(d.toCoordinate(this.height,e))` | `round` | unresolved | absent | c-platform | Math.round built-in |
| C1-136 | `app/client/imports/amcharts/gauge.js`:6 | `isNaN(K)` | `isNaN` | unresolved | absent | c-platform | global isNaN built-in |
| C1-137 | `app/client/imports/amcharts/plugins/export/libs/fabric.js/fabric.min.js`:1 | `this._objects.filter(function(o){return o.type===type})` | `filter` | unresolved | absent | c-platform | Array.prototype.filter on an object array |
| C1-138 | `app/client/imports/amcharts/plugins/export/libs/jszip/jszip.min.js`:12 | `a._data.getCompressedContent()` | `getCompressedContent` | unresolved | absent | b-lib | JSZip internal data-object method |
| C1-139 | `app/client/imports/amcharts/plugins/export/libs/jszip/jszip.min.js`:12 | `s(this.crc32(p),4)` | `s` | ambiguous | absent | b-lib | internal helper inside the vendored JSZip bundle |
| C1-140 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:7 | `Math.pow(2,-a)` | `pow` | unresolved | absent | c-platform | Math.pow built-in |
| C1-141 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:8 | `pn(e,n,3)` | `pn` | in-file | absent | b-lib | internal helper inside the vendored pdfmake bundle |
| C1-142 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:8 | `r(t,e)` | `r` | ambiguous | absent | b-lib | internal helper inside the vendored pdfmake bundle |
| C1-143 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:9 | `new h(e,n,r,this.imageMeasure,this.tableLayouts,u)` | `h` | ambiguous | absent | b-lib | internal constructor inside the vendored pdfmake bundle |
| C1-144 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:13 | `n(71)` | `n` | ambiguous | absent | b-lib | webpack module require inside the vendored bundle |
| C1-145 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `t.readString(4)` | `readString` | unresolved | absent | b-lib | pdfkit stream reader method in the vendored bundle |
| C1-146 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `T.writeUInt16(i)` | `writeUInt16` | unresolved | absent | b-lib | pdfkit buffer writer method in the vendored bundle |
| C1-147 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:15 | `t.charCodeAt(r)` | `charCodeAt` | unresolved | absent | c-platform | String.prototype.charCodeAt |
| C1-148 | `app/client/imports/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:17 | `n()` | `n` | ambiguous | absent | b-lib | internal helper inside the vendored pdfmake bundle |
| C1-149 | `app/client/imports/amcharts/plugins/export/libs/xlsx/xlsx.min.js`:2 | `path.toUpperCase().replace(chr0,"").replace(chr1,"!")` | `replace` | unresolved | absent | c-platform | String.prototype.replace chained on toUpperCase |
| C1-150 | `app/client/imports/amcharts/serial.js`:80 | `e.resetDateToMin(new Date(this.data[f].time),g,w,p)` | `resetDateToMin` | unresolved | absent | b-lib | AmCharts namespace helper inside the vendored bundle |
| C1-151 | `app/client/imports/amcharts/xy.js`:5 | `this.getAxisBounds(a,f,m,k,b)` | `getAxisBounds` | unresolved | absent | b-lib | AmCharts chart method inside the vendored bundle |
| C1-152 | `app/client/lib/SlickGrid - a grid/slick.grid.js`:240 | `$("<div class='slick-header-columns' style='left:-1000px' />").appendTo($headerScroller)` | `appendTo` | unresolved | absent | b-lib | jQuery appendTo on a wrapped element |
| C1-153 | `app/client/lib/SlickGrid - a grid/slick.grid.js`:660 | `getEditorLock()` | `getEditorLock` | in-file | absent | b-lib | internal function inside the vendored SlickGrid source |
| C1-154 | `app/client/lib/bootstrap-daterangepicker-custom/daterangepicker.js`:458 | `moment(startDate, this.locale.format)` | `moment` | unresolved | absent | b-lib | moment factory call |
| C1-155 | `app/client/lib/bootstrap-editable/js/bootstrap-editable.js`:254 | `this.showForm(false)` | `showForm` | unresolved | absent | b-lib | internal method inside vendored bootstrap-editable |
| C1-156 | `app/client/lib/bootstrap-editable/js/bootstrap-editable.js`:2653 | `$.proxy(function () { this.sourceData = cache.sourceData; this.doPrepend(); success.cal…` | `proxy` | unresolved | absent | b-lib | jQuery.proxy |
| C1-157 | `app/client/lib/gojs/go.js`:57 | `this.Fa.reset()` | `reset` | unresolved | absent | b-lib | internal method inside the vendored GoJS bundle |
| C1-158 | `app/client/lib/gojs/go.js`:103 | `Object.isFrozen(this)` | `isFrozen` | unresolved | absent | c-platform | Object.isFrozen built-in |
| C1-159 | `app/client/lib/gojs/go.js`:337 | `d.Df()` | `Df` | unresolved | absent | b-lib | internal method inside the vendored GoJS bundle |
| C1-160 | `app/client/lib/gojs/go.js`:882 | `d.gt(f.gj)` | `gt` | unresolved | absent | b-lib | internal method inside the vendored GoJS bundle |
| C1-161 | `app/client/lib/gojs/go.js`:1162 | `c.lb(a)` | `lb` | unresolved | absent | b-lib | internal method inside the vendored GoJS bundle |
| C1-162 | `app/client/lib/gojs/go.js`:1225 | `a.rect(l,r,Math.max(m,.1),Math.max(k,.1))` | `rect` | unresolved | absent | c-platform | canvas 2D context rect, beside beginPath and moveTo |
| C1-163 | `app/client/lib/gojs/go.js`:1366 | `Ho(b,!1)` | `Ho` | in-file | absent | b-lib | internal function inside the vendored GoJS bundle |
| C1-164 | `app/client/lib/gojs/go.js`:1718 | `l.na()` | `na` | unresolved | absent | b-lib | internal method inside the vendored GoJS bundle |
| C1-165 | `app/client/lib/gojs/go.js`:1739 | `Wq(a.width)` | `Wq` | in-file | absent | b-lib | internal function inside the vendored GoJS bundle |
| C1-166 | `app/client/lib/gojs/go.js`:2048 | `Ma.S.h(K,Ga)` | `h` | unresolved | absent | b-lib | internal method inside the vendored GoJS bundle |
| C1-167 | `app/client/lib/jquery-ui-1.12.0.custom/jquery-ui.js`:3668 | `parentInstance._over.call( parentInstance, event )` | `call` | unresolved | absent | c-platform | Function.prototype.call |
| C1-168 | `app/client/lib/jquery-ui-1.12.0.custom/jquery-ui.js`:4005 | `[ "padding", /ne\|nw\|n/.test( i ) ? "Top" : /se\|sw\|s/.test( i ) ? "Bottom" : /^e$/.t…` | `join` | unresolved | absent | c-platform | Array.prototype.join on an array literal |
| C1-169 | `app/client/plugins/d3/d3.min.js`:2 | `o.unshift(l)` | `unshift` | unresolved | absent | c-platform | Array.prototype.unshift |
| C1-170 | `app/client/styles/framework/bootstrap3-plugins/bootstrap-tagsinput/bootstrap-tagsinput.js`:349 | `$(event.target)` | `$` | unresolved | absent | b-lib | jQuery factory call |
| C1-171 | `app/client/views/boImports/boImports.js`:246 | `setFileData({ file, headers, tpl })` | `setFileData` | in-file | absent | a-repo | T-none: syntax tier resolves it in-file |
| C1-172 | `app/client/views/boSimplifiedForm/detectedVulnerabilitySimplified/dvRehashModal.js`:68 | `BusinessObjects.find({ "md.type" : "assets" }).fetch()` | `fetch` | unresolved | absent | b-lib | Mongo cursor fetch on a collection find |
| C1-173 | `app/client/views/campaigns/campaignStats/campaignStats.js`:60 | `_.isEmpty(existing)` | `isEmpty` | unresolved | absent | b-lib | lodash isEmpty() |
| C1-174 | `app/client/views/common/iboxTools/ibox-tools.js`:74 | `$(e.target)` | `$` | unresolved | absent | b-lib | jQuery factory call |
| C1-175 | `app/client/views/internalSecOps/boQuickEntry/quickEntryField.js`:566 | `relatedSet.add(key)` | `add` | ambiguous | absent | c-platform | Set.prototype.add; sibling binding is a Map |
| C1-176 | `app/client/views/internalSecOps/incidents/incidentsList.js`:49 | `incidentsGrid.grid.render()` | `render` | unresolved | absent | b-lib | SlickGrid grid render() |
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
| C1-193 | `app/client/views/system/manageKeywordMatchRules/manageKeywordMatchRules.js`:46 | `SlickGrid.keywordMatchRulesGrid.grid.getSelectionModel()` | `getSelectionModel` | unresolved | absent | b-lib | SlickGrid grid getSelectionModel() |
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
| C1-213 | `app/packages/meteor-amcharts/lib/amcharts.js`:39 | `a.getLabel()` | `getLabel` | unresolved | stub | b-lib | AmCharts axis method inside the vendored bundle |
| C1-214 | `app/packages/meteor-amcharts/lib/amcharts.js`:301 | `b.set()` | `set` | unresolved | stub | b-lib | AmCharts container set inside the vendored bundle |
| C1-215 | `app/packages/meteor-amcharts/lib/plugins/animate/animate.js`:351 | `getKeysGraphs( chart.graphs, keys, seen, getKeysGraph )` | `getKeysGraphs` | in-file | in-repo | b-lib | internal function inside the vendored amcharts plugin |
| C1-216 | `app/packages/meteor-amcharts/lib/plugins/export/export.js`:1351 | `_this.gatherClassName( group.parent, _this.setup.chart.classNamePrefix + "-legend-div",…` | `gatherClassName` | unresolved | stub | b-lib | internal method inside the vendored amcharts plugin |
| C1-217 | `app/packages/meteor-amcharts/lib/plugins/export/libs/fabric.js/fabric.js`:329 | `this.getObjects()` | `getObjects` | unresolved | in-repo | b-lib | internal method inside the vendored fabric.js source |
| C1-218 | `app/packages/meteor-amcharts/lib/plugins/export/libs/fabric.js/fabric.min.js`:6 | `toFixed(this.scaleX,NUM_FRACTION_DIGITS)` | `toFixed` | unresolved | absent | b-lib | fabric.util.toFixed, not Number.prototype.toFixed |
| C1-219 | `app/packages/meteor-amcharts/lib/plugins/export/libs/fabric.js/fabric.min.js`:11 | `floor(j*ratioH)` | `floor` | unresolved | absent | c-platform | Math.floor via a local alias in the vendored bundle |
| C1-220 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.js`:2003 | `__webpack_require__(7)` | `__webpack_require__` | in-file | absent | b-lib | webpack module runtime require in the vendored bundle |
| C1-221 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.js`:17016 | `result.push({ x: self.rowSpanData[self.rowSpanData.length - 1].left, index: self.rowSpa…` | `push` | unresolved | absent | c-platform | Array.prototype.push on a result array |
| C1-222 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:1 | `r.fs.bindFS(this.vfs)` | `bindFS` | unresolved | absent | b-lib | pdfkit virtual filesystem bindFS in the vendored bundle |
| C1-223 | `app/packages/meteor-amcharts/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:13 | `s.split("\n").map(function(t){return" "+t})` | `map` | unresolved | absent | c-platform | Array.prototype.map on a split() result |
| C1-224 | `app/packages/meteor-amcharts/lib/plugins/export/libs/xlsx/xlsx.js`:420 | `fmt.match(dec1)` | `match` | unresolved | stub | c-platform | String.prototype.match on a format string |
| C1-225 | `app/packages/meteor-amcharts/lib/plugins/export/libs/xlsx/xlsx.js`:8532 | `unescapexml(Rn[3])` | `unescapexml` | unresolved | in-repo | b-lib | internal function inside the vendored xlsx source |
| C1-226 | `app/packages/meteor-amcharts/lib/plugins/export/libs/xlsx/xlsx.min.js`:3 | `__utf16le(this,this.l,this.l+size)` | `__utf16le` | unresolved | absent | b-lib | internal helper inside the vendored xlsx bundle |
| C1-227 | `app/packages/meteor-apm-agent/tests/hijack/subscriptions.js`:17 | `h2.stop()` | `stop` | unresolved | absent | b-lib | DDP subscription handle stop, from client.subscribe |
| C1-228 | `app/packages/meteor-apm-agent/tests/models/base_error.js`:49 | `new BaseErrorModel()` | `BaseErrorModel` | unresolved | absent | a-repo | T-import: Meteor package-scope global from a repo file |
| C1-229 | `app/packages/meteor-stylus/plugin/compile-stylus.js`:213 | `absoluteImportPath(parsed)` | `absoluteImportPath` | in-file | in-repo | a-repo | T-none: syntax in-file and engine in-repo agree |
| C1-230 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:116 | `setTimeout(function(){b.destroy.call(b)},1E3*a)` | `setTimeout` | unresolved | absent | c-platform | setTimeout host API |
| C1-231 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:143 | `d.setCN(c,r,"graph-bullet")` | `setCN` | unresolved | absent | b-lib | AmCharts namespace helper inside the vendored bundle |
| C1-232 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:259 | `n.translate(t,p)` | `translate` | unresolved | absent | b-lib | AmCharts object method inside the vendored bundle |
| C1-233 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:369 | `c.fire({type:"selectedObjectChanged",chart:c})` | `fire` | unresolved | absent | b-lib | AmCharts event fire inside the vendored bundle |
| C1-234 | `app/packages/mikemccrickard-ammap/web.browser-legacy/packages/mikemccrickard_ammap.js`:482 | `this.chart.coordinatesToXY(this.longitudes[b],this.latitudes[b])` | `coordinatesToXY` | unresolved | absent | b-lib | AmCharts chart method inside the vendored bundle |
| C1-235 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:15 | `h.slice(g)` | `slice` | unresolved | stub | c-platform | String.prototype.slice inside a wordwrap loop |
| C1-236 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:77 | `a[b].remove()` | `remove` | unresolved | stub | b-lib | AmCharts label object remove in the vendored bundle |
| C1-237 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:95 | `d.Class({construct:function(){}})` | `Class` | unresolved | stub | b-lib | AmCharts.Class factory inside the vendored bundle |
| C1-238 | `app/packages/mikemccrickard-ammap/web.browser/lib/ammap.js`:126 | `d.applyTheme(this,a,this.cname)` | `applyTheme` | unresolved | stub | b-lib | AmCharts applyTheme inside the vendored bundle |
| C1-239 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/fabric.js/fabric.js`:7920 | `ctx.setLineDash(this.strokeDashArray)` | `setLineDash` | unresolved | stub | c-platform | canvas 2D context setLineDash |
| C1-240 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/fabric.js/fabric.js`:19503 | `_this.setAngle(value)` | `setAngle` | unresolved | stub | b-lib | fabric object setAngle inside the vendored source |

### B. Reference repository — JavaScript, second half

| id | file:line | call expression | callee | syntax | engine | class | reason |
|---|---|---|---|---|---|---|---|
| C1-241 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/fabric.js/fabric.min.js`:7 | `this.getCurrentCharStyle(s,u)` | `getCurrentCharStyle` | unresolved | absent | ? | ? |
| C1-242 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:16132 | `baseAssignValue(result, iteratee(value, key, object), value)` | `baseAssignValue` | in-file | absent | ? | ? |
| C1-243 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:42362 | `Array.isArray(_iterator4)` | `isArray` | unresolved | absent | ? | ? |
| C1-244 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:43843 | `feature('stylisticAlternatives', 'stylisticAltEleven')` | `feature` | in-file | absent | ? | ? |
| C1-245 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.js`:66619 | `__webpack_require__(142)` | `__webpack_require__` | in-file | absent | ? | ? |
| C1-246 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:2 | `Math.pow(2,8*n-1)` | `pow` | unresolved | absent | ? | ? |
| C1-247 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:5 | `t.replace("\t"," ")` | `replace` | unresolved | absent | ? | ? |
| C1-248 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `new Error("TODO: cmap format 14")` | `Error` | unresolved | absent | ? | ? |
| C1-249 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/pdfmake/pdfmake.min.js`:26 | `t.slice(c,f+1)` | `slice` | unresolved | absent | ? | ? |
| C1-250 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.js`:10474 | `f.replace(/COM\.MICROSOFT\./g, "")` | `replace` | unresolved | stub | ? | ? |
| C1-251 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.min.js`:2 | `data.charCodeAt(0)` | `charCodeAt` | unresolved | absent | ? | ? |
| C1-252 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.min.js`:2 | `fmt.substr(i,5)` | `substr` | unresolved | absent | ? | ? |
| C1-253 | `app/packages/mikemccrickard-ammap/web.browser/lib/plugins/export/libs/xlsx/xlsx.min.js`:10 | `px2pt(row.hpx)` | `px2pt` | in-file | absent | ? | ? |
| C1-254 | `app/packages/sylido-meteor-selectize-bootstrap/selectize/dist/js/selectize.min.js`:1 | `e(g[d],c)` | `e` | in-file | absent | ? | ? |
| C1-255 | `app/server/actionButtons/actionButtons.js`:584 | `_.union(_.get(relatedAggsByType, k, []), val)` | `union` | unresolved | stub | ? | ? |
| C1-256 | `app/server/campaigns/campaigns.js`:247 | `_.get(auth, "sub._id")` | `get` | unresolved | stub | ? | ? |
| C1-257 | `app/server/centralServicesAPI/requestProductUtils.test.js`:176 | `new Error("not-allowed")` | `Error` | unresolved | absent | ? | ? |
| C1-258 | `app/server/fortressAPI/fortressAPI.js`:58 | `_.mapValues(_.get(requestOptions, "headers", {}), (v) => String(v))` | `mapValues` | unresolved | in-repo | ? | ? |
| C1-259 | `app/server/jsreports/print.js`:174 | `_.get(user, "information.email", "")` | `get` | unresolved | stub | ? | ? |
| C1-260 | `app/server/lib/publish/detectedVulnerabilitiesGrid.js`:222 | `__cb(err)` | `__cb` | ambiguous | in-repo | ? | ? |
| C1-261 | `app/server/lib/publish/myWatchedItems.js`:122 | `_.get(Meteor, "settings.flags.debug", false)` | `get` | unresolved | stub | ? | ? |
| C1-262 | `app/server/lib/publish/surveys.js`:813 | `_.escapeRegExp(query)` | `escapeRegExp` | unresolved | stub | ? | ? |
| C1-263 | `app/server/lib/publish/widgetAggs/vulnByBUoverTimeAgg.js`:37 | `dateMomentUtils.modifiedDate("", "subtract", 6, "months")` | `modifiedDate` | import | stub | ? | ? |
| C1-264 | `app/server/manageScoring/manageScoring.js`:416 | `_.get(oldDoc, "md.id")` | `get` | unresolved | stub | ? | ? |
| C1-265 | `app/server/manageUsers/manUsers.js`:430 | `Meteor.users.find({ _id : { $in : tenantUserIds }, "information.vendorPortal" : true, i…` | `find` | unresolved | stub | ? | ? |
| C1-266 | `app/server/methods/getBoDescriptions.test.js`:143 | `handler.call({}, ["a", "b"])` | `call` | unresolved | absent | ? | ? |
| C1-267 | `app/server/navigation/navigation.test.js`:333 | `expect(boFindOne.mock.calls[0][0])` | `expect` | unresolved | absent | ? | ? |
| C1-268 | `app/server/rolesNotifications/rolesNotifications.js`:88 | `EmailTemplates.find($match, $project)` | `find` | unresolved | in-repo | ? | ? |
| C1-269 | `app/server/serverRouteApi/serverRouteApi.js`:157 | `logger.error("(/fp-api/runAggsMulti) Missing header parameters.", { nonce, hashAuth })` | `error` | unresolved | in-repo | ? | ? |
| C1-270 | `app/server/surveys/importExport.js`:286 | `_.get(auth, "user._id")` | `get` | unresolved | stub | ? | ? |
| C1-271 | `app/server/taxonomy/taxonomy.js`:97 | `taxonomySchema.validate(modifier, { modifier : true })` | `validate` | import | stub | ? | ? |
| C1-272 | `app/server/unitTests/boAutomationRules/boAutomationRules.app-test.js`:96 | `sinon.createSandbox()` | `createSandbox` | import | absent | ? | ? |
| C1-273 | `app/server/unitTests/cmDashboard/cmDashboardFindingsGrid.app-test.js`:419 | `chai.expect(runChart({ vendorID : "" }))` | `expect` | import | absent | ? | ? |
| C1-274 | `app/server/utils/emailUtils.js`:572 | `_.get(val, "email", "")` | `get` | unresolved | stub | ? | ? |
| C1-275 | `app/server/wfStatsBasic/wfStatsBasic.js`:54 | `getCustomLabel("info.date.due", "Date Due", "assessmentFindings")` | `getCustomLabel` | unresolved | stub | ? | ? |
| C1-276 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/bootstrap.js`:190 | `$(element)` | `$` | unresolved | stub | ? | ? |
| C1-277 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/inspinia.js`:255 | `$('body')` | `$` | unresolved | stub | ? | ? |
| C1-278 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-2.1.1.js`:3 | `n.now()` | `now` | unresolved | absent | ? | ? |
| C1-279 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:6 | `e("<a>")` | `e` | ambiguous | absent | ? | ? |
| C1-280 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:7 | `t.effects.animateClass.call(this,n?{add:s}:{remove:s},a,o,r)` | `call` | unresolved | absent | ? | ? |
| C1-281 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:7 | `s.outerHeight()` | `outerHeight` | unresolved | absent | ? | ? |
| C1-282 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/jquery-ui-1.10.4.min.js`:7 | `this.helper.offset()` | `offset` | unresolved | absent | ? | ? |
| C1-283 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/chartist/chartist.min.js`:7 | `c.serialize(o.meta)` | `serialize` | unresolved | absent | ? | ? |
| C1-284 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/codemirror.js`:7629 | `getOrder(line)` | `getOrder` | in-file | in-repo | ? | ? |
| C1-285 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/mode/rst/rst.js`:170 | `stream.match(rx_role_pre, false)` | `match` | unresolved | in-repo | ? | ? |
| C1-286 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/mode/sparql/sparql.js`:121 | `pushContext(state, "}", stream.column())` | `pushContext` | in-file | in-repo | ? | ? |
| C1-287 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/codemirror/mode/xml/xml.js`:133 | `stream.match(/^[^\s\u00a0=<>\"\']*[^\s\u00a0=<>\"\'\/]/)` | `match` | unresolved | in-repo | ? | ? |
| C1-288 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/cropper/cropper.min.js`:9 | `this.renderImage("zoom")` | `renderImage` | unresolved | absent | ? | ? |
| C1-289 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/dataTables/jquery.dataTables.js`:500 | `_fnCompatMap( init, 'orderDataType', 'sortDataType' )` | `_fnCompatMap` | in-file | in-repo | ? | ? |
| C1-290 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/dataTables/jquery.dataTables.js`:4217 | `tmpTable.css( 'width', 'auto' )` | `css` | unresolved | in-repo | ? | ? |
| C1-291 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/dataTables/jquery.dataTables.js`:14632 | `a.toString()` | `toString` | unresolved | stub | ? | ? |
| C1-292 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/footable/footable.all.min.js`:14 | `t(a.table).unbind(".sorting").bind({"footable_initialized.sorting":function(){var i,o,n…` | `data` | unresolved | absent | ? | ? |
| C1-293 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/fullcalendar/fullcalendar.min.js`:6 | `ye(r,t[s],r.forwardSegs)` | `ye` | in-file | absent | ? | ? |
| C1-294 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/fullcalendar/moment.min.js`:6 | `r(C(a)%60,2)` | `r` | in-file | absent | ? | ? |
| C1-295 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:117 | `b(y,a).closest("table.ui-jqgrid-btable").attr("id")` | `attr` | unresolved | absent | ? | ? |
| C1-296 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:221 | `a(c)` | `a` | ambiguous | absent | ? | ? |
| C1-297 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:332 | `a("#"+v.themodal)` | `a` | ambiguous | absent | ? | ? |
| C1-298 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:411 | `a.isFunction(b.p.beforeSelectRow)` | `isFunction` | unresolved | absent | ? | ? |
| C1-299 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:437 | `s("unbind")` | `s` | ambiguous | absent | ? | ? |
| C1-300 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:462 | `f.hasClass(a)` | `hasClass` | unresolved | absent | ? | ? |
| C1-301 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jqGrid/jquery.jqGrid.min.js`:517 | `this.toObj(g)` | `toObj` | unresolved | absent | ? | ? |
| C1-302 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:5372 | `rplusequals.exec( value )` | `exec` | unresolved | stub | ? | ? |
| C1-303 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:6413 | `$( this )` | `$` | unresolved | stub | ? | ? |
| C1-304 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:6605 | `toShow .hide()` | `hide` | unresolved | absent | ? | ? |
| C1-305 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.js`:8129 | `this._updateAlternate(inst)` | `_updateAlternate` | unresolved | in-repo | ? | ? |
| C1-306 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.min.js`:5 | `t(n.containment)` | `t` | in-file | absent | ? | ? |
| C1-307 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.min.js`:8 | `s.removeClass("ui-accordion-header-active ui-state-active")` | `removeClass` | unresolved | absent | ? | ? |
| C1-308 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jquery-ui/jquery-ui.min.js`:8 | `this._updateDatepicker(e)` | `_updateDatepicker` | unresolved | absent | ? | ? |
| C1-309 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/jsTree/jstree.min.js`:4 | `c.element.find("ul:visible").addBack()` | `addBack` | unresolved | absent | ? | ? |
| C1-310 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/morris/raphael-2.1.0.min.js`:10 | `bJ(b)` | `bJ` | in-file | absent | ? | ? |
| C1-311 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/morris/raphael-2.1.0.min.js`:10 | `b.defs.removeChild(this.gradient)` | `removeChild` | unresolved | absent | ? | ? |
| C1-312 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/rickshaw/rickshaw.min.js`:2 | `d3.svg.line().x(function(d){return graph.x(d.x)}).y(function(d){return graph.y(d.y)}).i…` | `tension` | unresolved | absent | ? | ? |
| C1-313 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/rickshaw/vendor/d3.v3.js`:1 | `u()` | `u` | ambiguous | absent | ? | ? |
| C1-314 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/select2/select2.full.min.js`:3 | `d.join(c._valueSeparator)` | `join` | unresolved | absent | ? | ? |
| C1-315 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/sparkline/jquery.sparkline.min.js`:4 | `e.get("colorMap")` | `get` | unresolved | absent | ? | ? |
| C1-316 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/staps/jquery.steps.min.js`:6 | `h.eq(e)` | `eq` | unresolved | absent | ? | ? |
| C1-317 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/sweetalert/sweetalert.min.js`:1 | `l.addClass(o,"visible")` | `addClass` | unresolved | absent | ? | ? |
| C1-318 | `mockup/INSPINIA-bootstrap-template/Static_Full_Version/js/plugins/validate/jquery.validate.min.js`:4 | `a(b).rules()` | `rules` | unresolved | absent | ? | ? |
| C1-319 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/bootstrap.js`:1812 | `$tip.find('.popover-content')` | `find` | unresolved | stub | ? | ? |
| C1-320 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/bootstrap.min.js`:6 | `c.isInStateTrue()` | `isInStateTrue` | unresolved | absent | ? | ? |
| C1-321 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/jquery-ui-1.10.4.min.js`:6 | `this.headers.removeClass("ui-accordion-header ui-accordion-header-active ui-helper-rese…` | `removeAttr` | unresolved | absent | ? | ? |
| C1-322 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/jquery-ui-1.10.4.min.js`:7 | `Math.max(0,a.maxHeight-e)` | `max` | unresolved | absent | ? | ? |
| C1-323 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/plugins/pace/pace.min.js`:2 | `t(p)` | `t` | unresolved | absent | ? | ? |
| C1-324 | `mockup/INSPINIA-bootstrap-template/Static_Seed_Project/js/plugins/slimscroll/jquery.slimscroll.min.js`:11 | `b.scrollTop()` | `scrollTop` | unresolved | absent | ? | ? |
| C1-325 | `mockup/www/js/bootstrap.min.js`:6 | `a(this)` | `a` | unresolved | absent | ? | ? |
| C1-326 | `mockup/www/js/bootstrap.min.js`:7 | `a(document).on("click.bs.tab.data-api",'[data-toggle="tab"]',e).on("click.bs.tab.data-a…` | `on` | unresolved | absent | ? | ? |
| C1-327 | `mockup/www/js/jquery-ui-1.10.4.min.js`:6 | `this.focusable.add(e)` | `add` | unresolved | absent | ? | ? |
| C1-328 | `mockup/www/js/jquery-ui-1.10.4.min.js`:6 | `this._hideDatepicker()` | `_hideDatepicker` | unresolved | absent | ? | ? |
| C1-329 | `mockup/www/js/jquery-ui-1.10.4.min.js`:7 | `this._trigger("beforeActivate",e,c)` | `_trigger` | unresolved | absent | ? | ? |
| C1-330 | `mockup/www/js/plugins/amcharts/amcharts.js`:359 | `d.applyTheme(this,a,this.cname)` | `applyTheme` | unresolved | absent | ? | ? |
| C1-331 | `mockup/www/js/plugins/amcharts/funnel.js`:24 | `q.getBBox()` | `getBBox` | unresolved | absent | ? | ? |
| C1-332 | `mockup/www/js/plugins/amcharts/plugins/animate/animate.js`:288 | `getKeysSliced( chart, keys, seen )` | `getKeysSliced` | in-file | absent | ? | ? |
| C1-333 | `mockup/www/js/plugins/amcharts/plugins/export/export.min.js`:1 | `c.isTainted(a)` | `isTainted` | unresolved | absent | ? | ? |
| C1-334 | `mockup/www/js/plugins/amcharts/plugins/export/libs/fabric.js/fabric.js`:13929 | `Math.sin(this.endAngle)` | `sin` | unresolved | absent | ? | ? |
| C1-335 | `mockup/www/js/plugins/amcharts/plugins/export/libs/fabric.js/fabric.js`:21986 | `this.fire('editing:exited')` | `fire` | unresolved | absent | ? | ? |
| C1-336 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.js`:779 | `hexWrite(this, string, offset, length)` | `hexWrite` | in-file | absent | ? | ? |
| C1-337 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.js`:12141 | `string.slice(0, trimmedRightIndex(string) + 1)` | `slice` | unresolved | absent | ? | ? |
| C1-338 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.js`:15481 | `pack(ALPHANUMERIC_MAP[data.charAt(i-1)], 6)` | `pack` | ambiguous | absent | ? | ? |
| C1-339 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:8 | `_(e,ai.placeholder)` | `_` | ambiguous | absent | ? | ? |
| C1-340 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:14 | `t.readInt()` | `readInt` | unresolved | absent | ? | ? |
| C1-341 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:15 | `this.glyphIDs.push(l.readShort())` | `push` | unresolved | absent | ? | ? |
| C1-342 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:15 | `n.match(/^End(\w+)/)` | `match` | unresolved | absent | ? | ? |
| C1-343 | `mockup/www/js/plugins/amcharts/plugins/export/libs/pdfmake/pdfmake.min.js`:16 | `(function(t){/*! @source http://purl.eligrey.com/github/FileSaver.js/blob/master/FileSa…` | `call` | unresolved | absent | ? | ? |
| C1-344 | `mockup/www/js/plugins/amcharts/plugins/export/libs/xlsx/xlsx.js`:1189 | `sector_list[minifat_store].data.slice(o.start*MSSZ,o.start*MSSZ+o.size)` | `slice` | unresolved | absent | ? | ? |
| C1-345 | `mockup/www/js/plugins/amcharts/plugins/export/libs/xlsx/xlsx.min.js`:2 | `write_num("n",r[1],ff[1])` | `write_num` | unresolved | absent | ? | ? |
| C1-346 | `mockup/www/js/plugins/amcharts/plugins/export/libs/xlsx/xlsx.min.js`:6 | `stack.pop()` | `pop` | unresolved | absent | ? | ? |
| C1-347 | `mockup/www/js/plugins/fullcalendar/moment.min.js`:6 | `vb(this)` | `vb` | unresolved | absent | ? | ? |
| C1-348 | `mockup/www/js/plugins/ionRangeSlider/ion.rangeSlider.js`:989 | `this.checkDiapason(this.coords.p_single_real, this.options.from_min, this.options.from_…` | `checkDiapason` | unresolved | absent | ? | ? |
| C1-349 | `mockup/www/js/plugins/ionRangeSlider/ion.rangeSlider.min.js`:50 | `this.callOnChange()` | `callOnChange` | unresolved | absent | ? | ? |
| C1-350 | `mockup/www/js/plugins/morris/morris.js`:1649 | `$.extend({}, this.defaults, options)` | `extend` | unresolved | absent | ? | ? |
| C1-351 | `mockup/www/js/plugins/query-builder/query-builder.standalone.min.js`:2747 | `cbRule.call(context, this.rules[i])` | `call` | unresolved | absent | ? | ? |
| C1-352 | `mockup/www/js/plugins/slimscroll/jquery.slimscroll.min.js`:12 | `e("<div></div>").addClass(a.wrapperClass).css({position:"relative",overflow:"hidden",wi…` | `css` | unresolved | absent | ? | ? |
| C1-353 | `mockup/www/js/plugins/switchery/switchery.js`:1 | `adv.call(layer,type,callback.hijacked\|\|(callback.hijacked=function(event){if(!event.p…` | `call` | unresolved | absent | ? | ? |
| C1-354 | `mockup/www/js/plugins/switchery/switchery.js`:1 | `this.needsClick(this.targetElement)` | `needsClick` | unresolved | absent | ? | ? |
| C1-355 | `mockup/www/js/plugins/typeahead/bloodhound.js`:61 | `$.each(obj, function(key, val) { if (result = test.call(null, val, key, obj)) { return …` | `each` | unresolved | absent | ? | ? |
| C1-356 | `mockup/www/js/plugins/typeahead/bloodhound.js`:701 | `this.index.reset()` | `reset` | unresolved | absent | ? | ? |
| C1-357 | `mongo scripts/aiMonitoringScripts/aim-1056-activity-stamp-backfill.js`:165 | `JSON.stringify(batcher.result)` | `stringify` | unresolved | stub | ? | ? |
| C1-358 | `mongo scripts/aiMonitoringScripts/aim-165-set-custom-fields-on-selectize-options.js`:147 | `db.getCollection(targetCollection).updateOne( { _id: doc._id }, { $set: doc }, { upsert…` | `updateOne` | unresolved | stub | ? | ? |
| C1-359 | `mongo scripts/aiMonitoringScripts/aim-728-osd12-scrm-category-remap.js`:29 | `print("Migration ID: " + migrationID)` | `print` | unresolved | stub | ? | ? |
| C1-360 | `mongo scripts/disa/fix_object_ids.js`:44 | `bulk.find({_id : obj_id})` | `find` | unresolved | stub | ? | ? |
| C1-361 | `mongo scripts/internalTab-migration-initial.js`:749 | `_.each(_.get(selOption, "options", []), (option) => { newStatuses[_.toLower(_.get(optio…` | `each` | unresolved | absent | ? | ? |
| C1-362 | `mongo scripts/vrm_servicenow_sys_id_fix.js`:278 | `combineResult(bulk[targetCollection].execute(), targetCollection)` | `combineResult` | in-file | in-repo | ? | ? |
| C1-363 | `playwright-validation/capture-settings-baseline.js`:218 | `Object.values(snap.vocabularies)` | `values` | unresolved | stub | ? | ? |
| C1-364 | `playwright-validation/deep-grid.js`:29 | `buf.toString("utf8").split("\n") .filter((l) => /(^\|\s)error:\|Exception while\|TypeEr…` | `filter` | unresolved | absent | ? | ? |
| C1-365 | `playwright-validation/sweep-session.js`:121 | `page.locator(".slick-row").first()` | `first` | unresolved | in-repo | ? | ? |
| C1-366 | `vendorPortal/client/accounts/accountsTemplates.app-test.js`:76 | `chai.assert.isFalse(routerGo.calledWith("login"), "must not fall back to login on succe…` | `isFalse` | unresolved | absent | ? | ? |
| C1-367 | `vendorPortal/client/components/boCards/boAttachFileDialog.js`:132 | `_.get(tpl, "data.bo")` | `get` | unresolved | in-repo | ? | ? |
| C1-368 | `vendorPortal/client/lib/bootstrap-editable/js/bootstrap-editable.js`:1257 | `this.hide()` | `hide` | unresolved | in-repo | ? | ? |
| C1-369 | `vendorPortal/client/lib/bootstrap-editable/js/bootstrap-editable.js`:5203 | `d.getTimezoneOffset()` | `getTimezoneOffset` | unresolved | stub | ? | ? |
| C1-370 | `vendorPortal/client/lib/form_utils.js`:170 | `errorCallback(errMsg)` | `errorCallback` | unresolved | stub | ? | ? |
| C1-371 | `vendorPortal/client/lib/jquery-stickytableheaders/jquery.stickytableheaders.min.js`:1 | `f.css("padding-right")` | `css` | unresolved | absent | ? | ? |
| C1-372 | `vendorPortal/client/lib/jquery-ui-1.12.0.custom/jquery-ui.js`:2543 | `c.css( "paddingLeft" )` | `css` | unresolved | stub | ? | ? |
| C1-373 | `vendorPortal/client/pages/findings/findings.js`:185 | `Meteor.call("boUpdateSeen", _id)` | `call` | unresolved | stub | ? | ? |
| C1-374 | `vendorPortal/client/plugins/blueimp/jquery.blueimp-gallery.min.js`:1 | `define(["./blueimp-helper","./blueimp-gallery"],a)` | `define` | unresolved | absent | ? | ? |
| C1-375 | `vendorPortal/client/plugins/d3/d3.min.js`:1 | `Math.cos(w)` | `cos` | unresolved | absent | ? | ? |
| C1-376 | `vendorPortal/client/plugins/d3/d3.min.js`:1 | `n.point(p[0],p[1])` | `point` | unresolved | absent | ? | ? |
| C1-377 | `vendorPortal/client/plugins/d3/d3.min.js`:2 | `Su(r=l,u)` | `Su` | in-file | absent | ? | ? |
| C1-378 | `vendorPortal/client/plugins/d3/d3.min.js`:2 | `o.push(r[1])` | `push` | unresolved | absent | ? | ? |
| C1-379 | `vendorPortal/client/plugins/d3/d3.min.js`:3 | `t.push(n[e])` | `push` | unresolved | absent | ? | ? |
| C1-380 | `vendorPortal/client/plugins/d3/d3.min.js`:5 | `q.on("mousemove.brush",null).on("mouseup.brush",null)` | `on` | unresolved | absent | ? | ? |
| C1-381 | `vendorPortal/client/plugins/slimscroll/jquery.slimscroll.min.js`:719 | `target.addEventListener('wheel', _onWheel, false )` | `addEventListener` | unresolved | absent | ? | ? |
| C1-382 | `vendorPortal/lib/logger.js`:79 | `transports.push(new winston.transports.File({ level : logLevel, levels : customLevels.l…` | `push` | unresolved | absent | ? | ? |
| C1-383 | `vendorPortal/packages/keithcoach-bootstrap3-datepicker/lib/js/bootstrap-datepicker.js`:500 | `this._detachEvents()` | `_detachEvents` | unresolved | in-repo | ? | ? |
| C1-384 | `vendorPortal/packages/matomo-custom/client/matomo.js`:7 | `FlowRouter.current()` | `current` | unresolved | stub | ? | ? |
| C1-385 | `vendorPortal/packages/meteor-template-extension/lib/template-inherits-hooks-from.js`:16 | `self.onCreated(hook)` | `onCreated` | unresolved | stub | ? | ? |
| C1-386 | `vendorPortal/public/okta-auth-js.min.js`:8 | `Object.defineProperties(e,Object.getOwnPropertyDescriptors(n))` | `defineProperties` | unresolved | absent | ? | ? |
| C1-387 | `vendorPortal/public/okta-auth-js.min.js`:8 | `n.n(i)` | `n` | unresolved | absent | ? | ? |
| C1-388 | `vendorPortal/public/okta-auth-js.min.js`:8 | `Object.getOwnPropertyDescriptor(e,t)` | `getOwnPropertyDescriptor` | unresolved | absent | ? | ? |
| C1-389 | `vendorPortal/public/okta-auth-js.min.js`:8 | `n(9231)` | `n` | ambiguous | absent | ? | ? |
| C1-390 | `vendorPortal/server/lib/publish/surveys.js`:456 | `_.get(v, "invited.o.rescindedUser", [])` | `get` | unresolved | in-repo | ? | ? |

### C. Reference repository — Java, Python, TypeScript

| id | file:line | call expression | callee | syntax | engine | class | reason |
|---|---|---|---|---|---|---|---|
| C1-001 | `QA/SeleniumWebdriver/TestngExtentFramework/src/main/java/com/selenium/test/webtestsbase/BasePage.java`:586 | `browserName.equals("chrome")` | `equals` | unresolved | absent | ? | ? |
| C1-002 | `QA/SeleniumWebdriver/TestngExtentFramework/src/main/java/com/selenium/test/webtestsbase/Listeners.java`:67 | `context.getAttribute("WebDriver4Method")` | `getAttribute` | unresolved | absent | ? | ? |
| C1-003 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/restassured/test/tests/RestAssuredAPITestsDemo.java`:239 | `RestAssured.given(). when(). get("http://ergast.com/api/f1/2017/circuits.json"). then()…` | `assertThat` | unresolved | absent | ? | ? |
| C1-004 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/dataEntities/threatsFactors.java`:21 | `data.getRandomDigit("0123")` | `getRandomDigit` | unresolved | absent | ? | ? |
| C1-005 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/end2endTests/EndToEnd_Campaigns_Tests.java`:103 | `logger.info("Going to Create Vendor page")` | `info` | unresolved | absent | ? | ? |
| C1-006 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/end2endTests/EndToEnd_Campaigns_Tests.java`:202 | `relatedContactsGrid.returnCell(1,GridColumnNames.columnNameEmail)` | `returnCell` | unresolved | absent | ? | ? |
| C1-007 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/BOFormTemplatesTests.java`:22 | `new LogUtils()` | `LogUtils` | unresolved | absent | ? | ? |
| C1-008 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/BOFormTemplatesTests.java`:174 | `boFormTemplatesPage.removeMessage(driver)` | `removeMessage` | unresolved | absent | ? | ? |
| C1-009 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/CampaignsValidationTests.java`:47 | `TestsConfig.getConfig().isLocalRun()` | `isLocalRun` | unresolved | absent | ? | ? |
| C1-010 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/CampaignsValidationTests.java`:52 | `new LoginPage(driver)` | `LoginPage` | unresolved | absent | ? | ? |
| C1-011 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/CreateDeleteBOs_HelpMethods.java`:193 | `new GridsPage(driver, "systemsGrid", usedColumns)` | `GridsPage` | unresolved | absent | ? | ? |
| C1-012 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/EmailTemplatesValidationsTests.java`:106 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement(driver.findElement(By…` | `until` | unresolved | absent | ? | ? |
| C1-013 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/EnableVRMFeaturesAndRiskOutcomes.java`:217 | `createEditOFRAPage.getCreateSaveButton().click()` | `click` | unresolved | absent | ? | ? |
| C1-014 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/EnableVRMFeaturesAndRiskOutcomes.java`:251 | `sidebarMenuPage.getMyApprovals()` | `getMyApprovals` | unresolved | absent | ? | ? |
| C1-015 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/KnowledgeBaseCUDTests.java`:94 | `knowledgeBasePage.getKnowledgeTitle(1).getAttribute("value")` | `getAttribute` | unresolved | absent | ? | ? |
| C1-016 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/KnowledgeBaseCreateValidationTests.java`:91 | `knowledgeBasePage.getSuccessErrorMessage().getText().equals("Failed to update Knowledge…` | `equals` | unresolved | absent | ? | ? |
| C1-017 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/KnowledgeBaseUpdateValidationTests.java`:78 | `knowledgeBasePage.getUpdateBtn().click()` | `click` | unresolved | absent | ? | ? |
| C1-018 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageBOSubTypesTests.java`:79 | `getWait(driver)` | `getWait` | unresolved | absent | ? | ? |
| C1-019 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageBOSubTypesTests.java`:335 | `navigateToListPage(boName)` | `navigateToListPage` | unresolved | absent | ? | ? |
| C1-020 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesBulkEditTests.java`:111 | `TimeUtils.waitForSeconds(1)` | `waitForSeconds` | import | absent | ? | ? |
| C1-021 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesBulkEditTests.java`:330 | `manageFilesPage.getChangeRelationshipBtn()` | `getChangeRelationshipBtn` | unresolved | absent | ? | ? |
| C1-022 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesBulkEditTests.java`:476 | `TimeUtils.waitForSeconds(1, driver)` | `waitForSeconds` | import | absent | ? | ? |
| C1-023 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesTests.java`:383 | `manageFilesPage.getSuccessErrorMessage()` | `getSuccessErrorMessage` | unresolved | absent | ? | ? |
| C1-024 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageFilesTests.java`:561 | `Assert.assertTrue(manageFilesPage.getSuccessErrorMessage().getText().contains("was uplo…` | `assertTrue` | import | absent | ? | ? |
| C1-025 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageNotificationRulesBOTypeTests.java`:177 | `logger.info("The 2nd test b_updateRuleWithRandomBOType() finished")` | `info` | unresolved | absent | ? | ? |
| C1-026 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageNotificationRulesBOTypeTests.java`:239 | `newEditNotificationRulePage.getEmailTemplateSelectbox().click()` | `click` | unresolved | absent | ? | ? |
| C1-027 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageNotificationRulesIntervalTests.java`:80 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement( notificationRulesPag…` | `until` | unresolved | absent | ? | ? |
| C1-028 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManagePhaseCUDTests.java`:156 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement(managePhasePage.getSu…` | `until` | unresolved | absent | ? | ? |
| C1-029 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageUsersBusinessRoleTests.java`:55 | `sidebarMenuPage.getUserName()` | `getUserName` | unresolved | absent | ? | ? |
| C1-030 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageUsersCUDTests.java`:164 | `TimeUtils.waitForSeconds(2)` | `waitForSeconds` | import | absent | ? | ? |
| C1-031 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/ManageUsersValidationsTests.java`:62 | `logger.info("ManageUsersValidationsTests completed on " + browser.toUpperCase())` | `info` | unresolved | absent | ? | ? |
| C1-032 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/MyWatchedItemsTests.java`:137 | `gridsPage.getRemoveAllFiltersBtn().click()` | `click` | unresolved | absent | ? | ? |
| C1-033 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/NotificationRulesAppliedToBOTests.java`:52 | `TestsConfig.getConfig().getBrowser(browser)` | `getBrowser` | unresolved | absent | ? | ? |
| C1-034 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/NotificationRulesAppliedToWorkflowsTests.java`:301 | `gridsPage.waitForLoadingIndicator()` | `waitForLoadingIndicator` | unresolved | absent | ? | ? |
| C1-035 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/QuestionsLibraryTests.java`:350 | `getWait(driver).until(ExpectedConditions.visibilityOf(listQuestionsPage.getEditIDField(…` | `until` | unresolved | absent | ? | ? |
| C1-036 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/QuestionsLibraryTests.java`:447 | `newEditSurveyPage.getQuestionSelectBox().sendKeys(randomTag2)` | `sendKeys` | unresolved | absent | ? | ? |
| C1-037 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RelationshipInAFTests.java`:274 | `assessmentFindingsGrid.waitForLoadingIndicator()` | `waitForLoadingIndicator` | unresolved | absent | ? | ? |
| C1-038 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:640 | `riskRankQuickEntryFormPage.getResponseForQuestion(2).clear()` | `clear` | unresolved | absent | ? | ? |
| C1-039 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:703 | `boConfigsPage.getSuccessErrorMessage().getText().contains("Successfully")` | `contains` | unresolved | absent | ? | ? |
| C1-040 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:820 | `sidebarMenuPage.getNavigationLink()` | `getNavigationLink` | unresolved | absent | ? | ? |
| C1-041 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/RiskRankQuickEntryPopUpsTests.java`:964 | `boConfigsPage.getCustomLabelField().clear()` | `clear` | unresolved | absent | ? | ? |
| C1-042 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SortIgnoreLowerCaseSurveyTests.java`:71 | `logger.info("SortIgnoreLowerCaseSurveyTests completed on " + browser.toUpperCase())` | `info` | unresolved | absent | ? | ? |
| C1-043 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SortIgnoreLowerCaseSurveyTests.java`:160 | `new GridsPage(driver, "servicesGrid", usedColumns)` | `GridsPage` | unresolved | absent | ? | ? |
| C1-044 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SortIgnoreLowerCase_HelpMethods.java`:199 | `gridsPage.getWait(driver)` | `getWait` | unresolved | absent | ? | ? |
| C1-045 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SubscriptionTests.java`:228 | `manageSubscriptionPage.getAllowLinkingFiles().click()` | `click` | unresolved | absent | ? | ? |
| C1-046 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:113 | `gridsPageSurveys.waitForLoadingIndicator()` | `waitForLoadingIndicator` | unresolved | absent | ? | ? |
| C1-047 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:129 | `newEditSurveyPage.getSwitchers(1).click()` | `click` | unresolved | absent | ? | ? |
| C1-048 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:174 | `getWait(driver).until(ExpectedConditions.textToBePresentInElement( newEditSurveyPage.ge…` | `until` | unresolved | absent | ? | ? |
| C1-049 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:210 | `getSurveyName()` | `getSurveyName` | unresolved | absent | ? | ? |
| C1-050 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryRuleTests.java`:319 | `ExpectedConditions.visibilityOf(newEditSurveyPage.getSurveyRuleBtns(2))` | `visibilityOf` | import | absent | ? | ? |
| C1-051 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryTests.java`:223 | `TimeUtils.waitForSeconds(2)` | `waitForSeconds` | import | absent | ? | ? |
| C1-052 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryTests.java`:387 | `createEditOFRAPage.getNameField()` | `getNameField` | unresolved | absent | ? | ? |
| C1-053 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyLibraryTests.java`:471 | `surveyProgressPage.getAFNameField().sendKeys(randomTag)` | `sendKeys` | unresolved | absent | ? | ? |
| C1-054 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAFSystemsTests.java`:291 | `getWait(driver)` | `getWait` | unresolved | absent | ? | ? |
| C1-055 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:48 | `TestsConfig.getConfig()` | `getConfig` | import | absent | ? | ? |
| C1-056 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:137 | `surveysMiniGrid.getWait(180, driver)` | `getWait` | unresolved | absent | ? | ? |
| C1-057 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:157 | `createEditOFRAPage.getPartOfGridName("vendors")` | `getPartOfGridName` | unresolved | absent | ? | ? |
| C1-058 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/SurveyMiniGridsAssetsVendorsTest.java`:297 | `gridsPage.getOkButton().click()` | `click` | unresolved | absent | ? | ? |
| C1-059 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:156 | `workflowsPage.getLabelField().sendKeys("Name")` | `sendKeys` | unresolved | absent | ? | ? |
| C1-060 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:246 | `TimeUtils.waitForSeconds(3)` | `waitForSeconds` | import | absent | ? | ? |
| C1-061 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:524 | `createEditOFRAPage.getOptions()` | `getOptions` | unresolved | absent | ? | ? |
| C1-062 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/newTests/WorkflowsTests.java`:634 | `systemPage.removeMessage()` | `removeMessage` | unresolved | absent | ? | ? |
| C1-063 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BOConfigsTests.java`:189 | `boConfigsPage.removeMessage()` | `removeMessage` | unresolved | absent | ? | ? |
| C1-064 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BOConfigsTests.java`:338 | `excludedBOType.substring(0,5)` | `substring` | unresolved | absent | ? | ? |
| C1-065 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BOConfigsTests.java`:422 | `boConfigsPage.getLobMultiSelectMaxItemsInput().clear()` | `clear` | unresolved | absent | ? | ? |
| C1-066 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/BaseTest.java`:95 | `getWait(driver)` | `getWait` | unresolved | absent | ? | ? |
| C1-067 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DDBOTests.java`:105 | `inputData.get()` | `get` | unresolved | absent | ? | ? |
| C1-068 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DDBOTests.java`:199 | `inputData.get()` | `get` | unresolved | absent | ? | ? |
| C1-069 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DataDrivenCUDBOsTests.java`:135 | `createEditOFRAPage.getNameField().sendKeys(createdBOName)` | `sendKeys` | unresolved | absent | ? | ? |
| C1-070 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DataDrivenCUDBOsTests.java`:140 | `createEditOFRAPage.getPartOfGridName("assets")` | `getPartOfGridName` | unresolved | absent | ? | ? |
| C1-071 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/junit/tests/DataDrivenManageBOSubTypesGeneralTests.java`:181 | `ScrollToElement(boSubTypesPage.getDeleteSubType(), driver)` | `ScrollToElement` | unresolved | absent | ? | ? |
| C1-072 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/ApplicationInfoPage.java`:219 | `applicationInfoData.getApplicationFlag()` | `getApplicationFlag` | unresolved | absent | ? | ? |
| C1-073 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/ApplicationInfoPage.java`:241 | `getSoxFlagNoYes().get(0)` | `get` | unresolved | absent | ? | ? |
| C1-074 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/ApplicationInfoPage.java`:312 | `clearAndSendKeys(getSourceURLField(),applicationInfoData.getSourceURL(), driver)` | `clearAndSendKeys` | unresolved | absent | ? | ? |
| C1-075 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditContractPage.java`:349 | `getInsuranceSelectBox()` | `getInsuranceSelectBox` | unresolved | absent | ? | ? |
| C1-076 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditContractPage.java`:365 | `getServicesOnlyRadioBtns()` | `getServicesOnlyRadioBtns` | unresolved | absent | ? | ? |
| C1-077 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditContractPage.java`:373 | `cnt.getContactName()` | `getContactName` | unresolved | absent | ? | ? |
| C1-078 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditOFRAPage.java`:1145 | `ofraData.setClassName(selectClass())` | `setClassName` | unresolved | absent | ? | ? |
| C1-079 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditOFRAPage.java`:1522 | `visibilityOf(getDatePicker())` | `visibilityOf` | unresolved | absent | ? | ? |
| C1-080 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/CreateEditOFRAPage.java`:1581 | `getResolutionReviewField()` | `getResolutionReviewField` | unresolved | absent | ? | ? |
| C1-081 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/DDBOPage.java`:537 | `navigateToListPage(inputData)` | `navigateToListPage` | unresolved | absent | ? | ? |
| C1-082 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/GridsPage.java`:517 | `certifyBtn.isDisplayed()` | `isDisplayed` | unresolved | absent | ? | ? |
| C1-083 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/GridsPage.java`:1946 | `displaySingleGridRow(ofraData.getBoName())` | `displaySingleGridRow` | unresolved | absent | ? | ? |
| C1-084 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/KnownVulnsInfoPage.java`:275 | `kvF.getEaseOfExploit()` | `getEaseOfExploit` | unresolved | absent | ? | ? |
| C1-085 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/LoginPage.java`:38 | `driver.manage()` | `manage` | unresolved | absent | ? | ? |
| C1-086 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/MyWatchedItemsPage.java`:23 | `getSearchFieldForMyWatchedItems().isDisplayed()` | `isDisplayed` | unresolved | absent | ? | ? |
| C1-087 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/RiskRankInfoPage.java`:214 | `getDunsNumber().sendKeys(data.getDunsNumber())` | `sendKeys` | unresolved | absent | ? | ? |
| C1-088 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/UserSessionsPage.java`:70 | `ls.get(i).getText().equals(name)` | `equals` | unresolved | absent | ? | ? |
| C1-089 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/selenium/test/pages/VendorInfoPage.java`:350 | `clearAndSendKeys(countryField, vendorInfoData.getCountry(), driver)` | `clearAndSendKeys` | unresolved | absent | ? | ? |
| C1-090 | `QA/SeleniumWebdriver/TestngExtentFramework/src/test/java/com/sikulix/test/BasicSikulixDemo.java`:43 | `new LoginPage(driver)` | `LoginPage` | unresolved | absent | ? | ? |
| C1-391 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/ab-compare.py`:14 | `l.strip()` | `strip` | unresolved | stub | ? | ? |
| C1-392 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-check.py`:110 | `len({len(l) for l in body})` | `len` | unresolved | stub | ? | ? |
| C1-393 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-check.py`:238 | `float(span)` | `float` | unresolved | stub | ? | ? |
| C1-394 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-check.py`:238 | `probes(span, step)` | `probes` | in-file | in-repo | ? | ? |
| C1-395 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:53 | `len(_plain(str(title)))` | `len` | unresolved | stub | ? | ? |
| C1-396 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:72 | `type(r)` | `type` | unresolved | stub | ? | ? |
| C1-397 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:148 | `lanes.get(srv, 0)` | `get` | unresolved | stub | ? | ? |
| C1-398 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/chart-template.py`:259 | `round(float(sp), 1)` | `round` | unresolved | stub | ? | ? |
| C1-399 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/ledger-append.py`:67 | `os.path.join(r, "chosen-fields.tsv")` | `join` | unresolved | stub | ? | ? |
| C1-400 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/ledger-append.py`:93 | `lines.append("")` | `append` | unresolved | stub | ? | ? |
| C1-401 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/rebaseline.py`:29 | `open(p)` | `open` | unresolved | stub | ? | ? |
| C1-402 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/rebaseline.py`:41 | `os.path.exists(f)` | `exists` | unresolved | stub | ? | ? |
| C1-403 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:122 | `bar[:width].ljust(width)` | `ljust` | unresolved | stub | ? | ? |
| C1-404 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:175 | `r.get("project")` | `get` | unresolved | stub | ? | ? |
| C1-405 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:268 | `int(now.get(k, 0) or 0)` | `int` | unresolved | stub | ? | ? |
| C1-406 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:344 | `exp.append(key)` | `append` | unresolved | stub | ? | ? |
| C1-407 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:382 | `f(8)` | `f` | unresolved | in-repo | ? | ? |
| C1-408 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:435 | `os.path.isdir(tdir)` | `isdir` | unresolved | stub | ? | ? |
| C1-409 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:459 | `os.path.basename(compare_run.rstrip('/'))` | `basename` | unresolved | stub | ? | ? |
| C1-410 | `Meteor3preUpgradeScripts/meteor-async-migration/tests/FP/lib/w4-report.py`:500 | `", ".join(cmp_[1])` | `join` | unresolved | stub | ? | ? |
| C1-411 | `QA/RobotTests/libraries/RiskConfigsDB.py`:229 | `by_sub.setdefault(d.get("subID"), [])` | `setdefault` | unresolved | stub | ? | ? |
| C1-412 | `QA/RobotTests/libraries/RiskConfigsDB.py`:257 | `any((d.get("version") or 0) != 1 for d in startups)` | `any` | unresolved | stub | ? | ? |
| C1-413 | `QA/RobotTests/libraries/RiskConfigsDB.py`:296 | `range(num_levels)` | `range` | unresolved | stub | ? | ? |
| C1-414 | `QA/RobotTests/libraries/RiskConfigsDB.py`:306 | `doc.get("scoreBands", [])` | `get` | unresolved | stub | ? | ? |
| C1-415 | `QA/RobotTests/libraries/RiskConfigsDB.py`:308 | `self.expected_even_bands(len(bands))` | `expected_even_bands` | in-file | in-repo | ? | ? |
| C1-416 | `QA/RobotTests/libraries/RiskConfigsDB.py`:318 | `bool(actual)` | `bool` | unresolved | stub | ? | ? |
| C1-417 | `QA/RobotTests/libraries/RiskConfigsDB.py`:368 | `d.get("subID")` | `get` | unresolved | stub | ? | ? |
| C1-418 | `QA/RobotTests/libraries/RiskConfigsDB.py`:371 | `int(expected_version)` | `int` | unresolved | stub | ? | ? |
| C1-419 | `QA/RobotTests/resources/libraries/RegulatoryFixtures.py`:92 | `os.environ.get("RF_FIXTURE_MONGO_URL", "").strip()` | `strip` | unresolved | stub | ? | ? |
| C1-420 | `QA/RobotTests/resources/libraries/SubDailyNRBackend.py`:74 | `self._sub_id(sub_id)` | `_sub_id` | in-file | in-repo | ? | ? |
| C1-421 | `QA/RobotTests/resources/libraries/env_loader.py`:47 | `os.path.abspath(os.path.join(here, "..", ".."))` | `abspath` | unresolved | stub | ? | ? |
| C1-422 | `QA/RobotTests/resources/libraries/env_loader.py`:57 | `print( "[env_loader] WARNING: could not read %s (%s: %s); " "continuing with the existi…` | `print` | unresolved | stub | ? | ? |
| C1-423 | `QA/RobotTests/resources/libraries/evidence.py`:61 | `cleaned.strip("_")` | `strip` | unresolved | stub | ? | ? |
| C1-424 | `QA/RobotTests/resources/libraries/fpst332_scenario_runner.py`:257 | `type(err)` | `type` | unresolved | stub | ? | ? |
| C1-425 | `QA/RobotTests/resources/libraries/regulatory_timer_core_diagnostics.py`:36 | `_driver()` | `_driver` | in-file | in-repo | ? | ? |
| C1-426 | `QA/RobotTests/resources/libraries/regulatory_timer_core_evidence.py`:63 | `cleaned.replace("__", "_")` | `replace` | unresolved | stub | ? | ? |
| C1-427 | `QA/RobotTests/resources/libraries/regulatory_timer_core_evidence.py`:108 | `str(_variable("${TEST NAME}", ""))` | `str` | unresolved | stub | ? | ? |
| C1-428 | `cijobs/scripts/build-publish-payload.py`:30 | `read_package_json(archive)` | `read_package_json` | unresolved | in-repo | ? | ? |
| C1-429 | `cijobs/scripts/build-publish-payload.py`:51 | `base64.b64encode(hashlib.sha512(data).digest()).decode("ascii")` | `decode` | unresolved | stub | ? | ? |
| C1-430 | `cijobs/scripts/registry-cleanup.py`:81 | `urllib.parse.urlencode(params)` | `urlencode` | unresolved | stub | ? | ? |
| C1-431 | `playwright-validation/async-predicate-sweep-A.py`:88 | `max(0, m.start() - 90)` | `max` | unresolved | stub | ? | ? |
| C1-432 | `playwright-validation/inventory-clickA.py`:27 | `sorted(glob.glob(os.path.join(RD, "*.js")))` | `sorted` | unresolved | stub | ? | ? |
| C1-433 | `playwright-validation/inventory-clickA.py`:27 | `glob.glob(os.path.join(RD, "*.js"))` | `glob` | unresolved | stub | ? | ? |
| C1-434 | `playwright-validation/schema-sweep-clickA.py`:76 | `len(src)` | `len` | unresolved | stub | ? | ? |
| C1-435 | `playwright-validation/schema-sweep-clickA.py`:87 | `re.compile(r'^(\s*)(?:"([\w.$\[\]]+)"\|\'([\w.$\[\]]+)\'\|([\w$]+))\s*(?::\s*)?(async\s…` | `compile` | import | stub | ? | ? |
| C1-436 | `Meteor3preUpgradeScripts/meteor-async-migration/samples/meteor-2.9.ts`:35 | `Accounts.createUserVerifyingEmail()` | `createUserVerifyingEmail` | import | stub | ? | ? |
| C1-437 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props-withTracker.ts`:24 | `debug( `\n************************************************** *** ${fileInfo.path} *****…` | `debug` | unresolved | stub | ? | ? |
| C1-438 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props-withTracker.ts`:64 | `j(p.value.arguments) .find(j.ReturnStatement)` | `find` | unresolved | stub | ? | ? |
| C1-439 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props-withTracker.ts`:118 | `v.value.init.properties.map((sp) => { props[sp.key.name] = sp.value; })` | `map` | unresolved | stub | ? | ? |
| C1-440 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-component-props.ts`:74 | `findImportNodeByVariableName( componentName, rootCollection, j )` | `findImportNodeByVariableName` | unresolved | in-repo | ? | ? |
| C1-441 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-find-await-without-async.ts`:24 | `j(p).toSource()` | `toSource` | unresolved | stub | ? | ? |
| C1-442 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-find-promise-all-foreach.ts`:18 | `j(fileInfo.source)` | `j` | unresolved | stub | ? | ? |
| C1-443 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-find-promise-all-foreach.ts`:22 | `rootCollection.find(j.CallExpression)` | `find` | unresolved | stub | ? | ? |
| C1-444 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-meteor-call.ts`:61 | `setFunctionAsync(parentFunction, j)` | `setFunctionAsync` | unresolved | in-repo | ? | ? |
| C1-445 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-meteor-call.ts`:71 | `debug("**************************************************")` | `debug` | unresolved | stub | ? | ? |
| C1-446 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-rename-functions.ts`:110 | `debug( "\n+++replace this", j(replaceThisPath).toSource(), p.value.loc?.start, p.value.…` | `debug` | unresolved | stub | ? | ? |
| C1-447 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-rename-functions.ts`:116 | `j(byNode.value).toSource()` | `toSource` | unresolved | stub | ? | ? |
| C1-448 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-rename-functions.ts`:195 | `rootCollection.find(j.MemberExpression).map((p) => { if ( p.value.object.type === "Iden…` | `map` | unresolved | stub | ? | ? |
| C1-449 | `Meteor3preUpgradeScripts/meteor-async-migration/transform-use-async-function.ts`:139 | `findParentObject(p.parentPath)` | `findParentObject` | unresolved | in-repo | ? | ? |
| C1-450 | `Meteor3preUpgradeScripts/meteor-async-migration/transform.ts`:47 | `debug(`***************************************************** ${fileInfo.path} *********…` | `debug` | unresolved | stub | ? | ? |
| C1-451 | `Meteor3preUpgradeScripts/meteor-async-migration/transform.ts`:137 | `debug("variable declaration:", node.declaration.declarations)` | `debug` | unresolved | stub | ? | ? |
| C1-452 | `Meteor3preUpgradeScripts/meteor-async-migration/transform.ts`:183 | `j(xp)` | `j` | unresolved | stub | ? | ? |
| C1-453 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:21 | `require("debug")` | `require` | unresolved | stub | ? | ? |
| C1-454 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:85 | `debug( "____found in parent component props:", parentComponentProps[expression.name] )` | `debug` | unresolved | stub | ? | ? |
| C1-455 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:378 | `debug("[handleComponent] _context usage parent path:", p.parentPath)` | `debug` | unresolved | stub | ? | ? |
| C1-456 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:479 | `decl.id.properties.map((idProp) => { if ( idProp.type == "ObjectProperty" && idProp.key…` | `map` | unresolved | stub | ? | ? |
| C1-457 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:532 | `j(componentPath).find(j.JSXElement)` | `find` | unresolved | stub | ? | ? |
| C1-458 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:532 | `j(componentPath).find(j.JSXElement).paths()` | `paths` | unresolved | stub | ? | ? |
| C1-459 | `Meteor3preUpgradeScripts/meteor-async-migration/utils-component.ts`:557 | `debug( "[handleComponent] _child component at:", theComponent?.value.loc.start )` | `debug` | unresolved | stub | ? | ? |
| C1-460 | `Meteor3preUpgradeScripts/meteor-async-migration/utils.ts`:363 | `debug( `convert all functions use the async function which has the name is ${name} to a…` | `debug` | unresolved | stub | ? | ? |

### D. Second corpus — Python, TSX/TypeScript, Go/Java/JavaScript/C

| id | file:line | call expression | callee | syntax | precise | class | reason |
|---|---|---|---|---|---|---|---|
| C2-001 | `apps/runner/redglass_runner/fuzz_canary/Fuzz_Honggfuzz/harness.c`:17 | `memcpy(buf, data, size)` | `memcpy` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-002 | `archive/dashboard/console/public/mockServiceWorker.js`:227 | `values.filter( (value) => value !== 'msw/passthrough', )` | `filter` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-003 | `archive/demo/probe/fixtures/vulnerable-package/src/index.js`:1 | `require("lodash")` | `require` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-004 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/onboard.go`:164 | `huh.NewGroup( huh.NewInput(). Title("LangSmith API Key"). Placeholder("lsv2_..."). Echo…` | `NewGroup` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-005 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/onboard.go`:173 | `huh.ThemeFunc(ui.RedglassTheme)` | `ThemeFunc` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-006 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/onboard_test.go`:39 | `providerCredentialEnv(provider)` | `providerCredentialEnv` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-007 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/remove.go`:105 | `os.RemoveAll(home)` | `RemoveAll` | import | unjoined-file-not-indexed | ? | ? |
| C2-008 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/start.go`:150 | `ui.DimText("CLI exited. Services kept running — run 'redglass stop' to shut down.")` | `DimText` | import | unjoined-file-not-indexed | ? | ? |
| C2-009 | `archive/docs/reference/source-corpus/files/clients/launcher/cmd/update.go`:61 | `c.Pull(targetVersion)` | `Pull` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-010 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/config/config.go`:55 | `os.Open(path)` | `Open` | import | unjoined-file-not-indexed | ? | ? |
| C2-011 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/engagement/picker_test.go`:49 | `mkBareDir(t, home, "charlie")` | `mkBareDir` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-012 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/engagement/picker_test.go`:170 | `validateSlug(home, "acme-2026")` | `validateSlug` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-013 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/health/health.go`:65 | `bytes.NewReader(body)` | `NewReader` | import | unjoined-file-not-indexed | ? | ? |
| C2-014 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/health/health.go`:91 | `time.Now()` | `Now` | import | unjoined-file-not-indexed | ? | ? |
| C2-015 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/ui/theme.go`:135 | `lipgloss.Color("#FF0000")` | `Color` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-016 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/updater/updater.go`:122 | `io.ReadAll(resp.Body)` | `ReadAll` | import | unjoined-file-not-indexed | ? | ? |
| C2-017 | `archive/docs/reference/source-corpus/files/clients/launcher/internal/updater/updater.go`:160 | `fmt.Errorf("get executable path: %w", err)` | `Errorf` | import | unjoined-file-not-indexed | ? | ? |
| C2-018 | `packages/infra/redglass_infra/analysis/intercept_profiles/jvm/RedglassInterceptAgent.java`:277 | `"#profile:".length()` | `length` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-019 | `packages/infra/redglass_infra/analysis/intercept_profiles/jvm/RedglassInterceptAgent.java`:289 | `byType.computeIfAbsent(sink.type, t -> new ArrayList<>()).add(sink)` | `add` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-020 | `packages/infra/redglass_infra/analysis/intercept_profiles/node/preload.js`:123 | `Array.prototype.slice.call(args)` | `call` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-021 | `apps/runner/tests/conftest.py`:288 | `FakeGraph()` | `FakeGraph` | in-file | joined-defined | ? | ? |
| C2-022 | `apps/runner/tests/test_run_loop.py`:124 | `run_loop( run_id="run-1", drive=_drive_two_steps, fold=_fold, event_log=log, clock=Froz…` | `run_loop` | unresolved | joined-defined | ? | ? |
| C2-023 | `archive/docs/reference/source-corpus/files/tests/unit/research/test_graph.py`:36 | `g.upsert_node(Node.make(NodeKind.HOST, "10.0.0.1", os="linux"))` | `upsert_node` | unresolved | joined-defined | ? | ? |
| C2-024 | `archive/src/redglass/adapters/sandbox/local_process.py`:578 | `handle.output.finish( timeout_seconds=( timeout_seconds if timeout_seconds is not None …` | `finish` | unresolved | joined-defined | ? | ? |
| C2-025 | `packages/infra/redglass_infra/enrichment/mirror/sync.py`:327 | `ArchiveFetchError("sync cursor JSON is not an object")` | `ArchiveFetchError` | unresolved | joined-defined | ? | ? |
| C2-026 | `packages/infra/redglass_infra/sandbox/lifecycle_failures.py`:331 | `error.retain_cleanup_diagnostic( f"capture finalization: {capture_error}" )` | `retain_cleanup_diagnostic` | unresolved | joined-defined | ? | ? |
| C2-027 | `packages/infra/tests/test_binary_recon_parsers.py`:700 | `parse_sinkxrefs(repeated, tool=AnalysisTool.BINARY_SINKXREFS)` | `parse_sinkxrefs` | unresolved | joined-defined | ? | ? |
| C2-028 | `packages/infra/tests/test_recording_source.py`:240 | `used.finalize_misses( { "pkg:deb/debian/libssl1.1@1.1": _INSUFF, "pkg:deb/debian/libssl…` | `finalize_misses` | unresolved | joined-defined | ? | ? |
| C2-029 | `packages/pipeline/tests/test_analysis_services.py`:152 | `_StubPort( _source_run(exit_code=1, timed_out=False, findings=[_FINDING]) )` | `_StubPort` | unresolved | joined-defined | ? | ? |
| C2-030 | `packages/pipeline/tests/test_usage_summary_projection.py`:164 | `UsageSummaryProjection().rebuild(log, run_id="run-1")` | `rebuild` | unresolved | joined-defined | ? | ? |
| C2-031 | `scripts/substrate-vm/guest-delegate.py`:989 | `_frame(digest, _read_root_file(path))` | `_frame` | in-file | joined-defined | ? | ? |
| C2-032 | `tests/architecture/test_taint_image_pins.py`:168 | `_string_constant(_PROVISIONER_SRC, "_WHEEL_SHA256")` | `_string_constant` | in-file | joined-defined | ? | ? |
| C2-033 | `apps/api/redglass_api/api/v1/routes/evidence_category.py`:49 | `tuple( EvidenceCategoryFacet(category=category, count=count) for category, count in ind…` | `tuple` | unresolved | joined-external | ? | ? |
| C2-034 | `apps/runner/redglass_runner/health/report.py`:181 | `isinstance(status, _Unreadable)` | `isinstance` | unresolved | joined-external | ? | ? |
| C2-035 | `archive/docs/reference/source-corpus/files/redglass/tools/defense/tools.py`:452 | `section_match.group(1)` | `group` | unresolved | joined-external | ? | ? |
| C2-036 | `packages/infra/redglass_infra/provisioning/bundler.py`:341 | `extracted.mkdir(mode=0o700)` | `mkdir` | unresolved | joined-external | ? | ? |
| C2-037 | `packages/infra/redglass_infra/provisioning/cargo_policy.py`:108 | `isinstance(source, str)` | `isinstance` | unresolved | joined-external | ? | ? |
| C2-038 | `packages/infra/redglass_infra/research_egress/capture.py`:157 | `document.get("outcomes")` | `get` | ambiguous | joined-external | ? | ? |
| C2-039 | `packages/infra/tests/test_codeql_extraction_completeness.py`:455 | `(_FIXTURES / "codeql_diagnostics_cpp_buildless.json").read_bytes()` | `read_bytes` | unresolved | joined-external | ? | ? |
| C2-040 | `packages/pipeline/redglass_pipeline/analysis/observed_effects.py`:221 | `ValueError("a NETWORK_ATTEMPT observation must record blocked")` | `ValueError` | unresolved | joined-external | ? | ? |
| C2-041 | `packages/pipeline/tests/test_analysis_services.py`:399 | `pytest.raises(ValueError, match="non-binary")` | `raises` | import | joined-external | ? | ? |
| C2-042 | `packages/tools/tests/test_joern_aderyn_cpg_tools.py`:136 | `len(out)` | `len` | unresolved | joined-external | ? | ? |
| C2-043 | `tests/architecture/test_subprocess_calls_are_bounded.py`:252 | `bool(node.args)` | `bool` | unresolved | joined-external | ? | ? |
| C2-044 | `tests/contract/test_target_intake_contract.py`:38 | `len(body)` | `len` | unresolved | joined-external | ? | ? |
| C2-045 | `apps/api/redglass_api/api/v1/routes/assurance.py`:31 | `router.get("/runs/{run_id}/assurance", response_model=RunAssuranceView)` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-046 | `apps/api/tests/test_create_run_route.py`:210 | `client.post("/v1/runs", json=_request_body(sandbox="local"))` | `post` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-047 | `apps/api/tests/test_fuzz_health_route.py`:96 | `self.inner.read_fuzz_prefix(run_id=run_id)` | `read_fuzz_prefix` | in-file | unjoined-in-indexed-file | ? | ? |
| C2-048 | `apps/api/tests/test_fuzz_health_route.py`:122 | `payload.model_dump(mode="json")` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-049 | `apps/api/tests/test_read_routes.py`:219 | `KgProjector(log, kg).project()` | `project` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-050 | `apps/api/tests/test_read_routes.py`:449 | `stale.json()` | `json` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-051 | `apps/api/tests/test_usage_cost_route.py`:233 | `_client(_UsageStore(view), _SelectivePricebook(), tmp_path).get( "/v1/runs/run-1/usage/…` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-052 | `apps/cli/redglass_cli/composition/supervisor.py`:191 | `supervisor.admission_loop()` | `admission_loop` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-053 | `apps/cli/tests/test_replay_command.py`:168 | `runner.invoke( app, ["replay", "--run-id", "run-reject"] )` | `invoke` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-054 | `apps/runner/redglass_runner/fuzz_canary_preflight.py`:837 | `evidence_store.get(Sha256Digest(digest))` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-055 | `apps/runner/redglass_runner/run_risk.py`:128 | `reachability.rank_subgraph(subgraph, run_id=run_id)` | `rank_subgraph` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-056 | `apps/runner/redglass_runner/run_start.py`:876 | `bundle.path(ref)` | `path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-057 | `apps/runner/redglass_runner/streaming/bus_publishing_event_log.py`:70 | `self._inner.append_many(events)` | `append_many` | in-file | unjoined-in-indexed-file | ? | ? |
| C2-058 | `apps/runner/redglass_runner/supervisor/reconcile.py`:181 | `RunBundle(run_root / run_id).path(STATE_DB_NAME)` | `path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-059 | `apps/runner/redglass_runner/supervisor/supervisor.py`:754 | `pid_path.read_text(encoding="utf-8")` | `read_text` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-060 | `apps/runner/tests/supervisor/test_creation.py`:282 | `bundle.path(STATE_DB_NAME)` | `path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-061 | `apps/runner/tests/test_bootstrap_catalog.py`:355 | `web_fetch.invoke( {"source": "package_meta", "resource_id": _UNKNOWN_PURL} )` | `invoke` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-062 | `apps/runner/tests/test_bootstrap_pattern_registry.py`:175 | `CrossRunValidatedPatternRegistry(root=root).register( ValidatedPatternEntry( signature=…` | `register` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-063 | `apps/runner/tests/test_coverage_disclosure_persistence.py`:132 | `bundle.path(COVERAGE_DISCLOSURES_REF)` | `path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-064 | `apps/runner/tests/test_coverage_disclosures_reach_reporting_role.py`:449 | `AnalysisExecutedPayload.model_validate(log.read()[-1].payload)` | `model_validate` | import | unjoined-in-indexed-file | ? | ? |
| C2-065 | `apps/runner/tests/test_projection_fold.py`:287 | `store.get_agent_transcript("run-1")` | `get_agent_transcript` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-066 | `apps/runner/tests/test_run_replay.py`:563 | `report.model_copy( update={ "validated_findings": ( original.model_copy(update={"severi…` | `model_copy` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-067 | `apps/runner/tests/test_run_replay.py`:607 | `view.model_copy(update={"rejected_total": view.rejected_total + 99})` | `model_copy` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-068 | `apps/runner/tests/test_run_replay.py`:837 | `bundle.path(RUN_MANIFEST_REF).unlink()` | `unlink` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-069 | `apps/runner/tests/test_supervised_run.py`:305 | `bundle.path(DRIVE_FAILURE_REF)` | `path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-070 | `apps/runner/tests/test_supervised_run.py`:327 | `bundle.path(SUPERVISED_LOCK_REF)` | `path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-071 | `archive/containers/manage.py`:289 | `typer.echo(f"ollama api: reachable ({OLLAMA_API_BASE})")` | `echo` | import | unjoined-in-indexed-file | ? | ? |
| C2-072 | `archive/demo/probe/src/redglass/control_plane/run_state.py`:155 | `run_state.model_copy( update={ "status": _run_status_from_lifecycle_state(snapshot.stat…` | `model_copy` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-073 | `archive/demo/probe/tests/test_agent_provider_runtime.py`:148 | `KnowledgeGraphBuilder(json_store=store).build(bundle)` | `build` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-074 | `archive/demo/probe/tests/test_intake_inventory.py`:251 | `Clock.fixed(FIXED_TIME)` | `fixed` | import | unjoined-in-indexed-file | ? | ? |
| C2-075 | `archive/demo/probe/tests/test_intake_inventory.py`:453 | `Clock.fixed(FIXED_TIME)` | `fixed` | import | unjoined-in-indexed-file | ? | ? |
| C2-076 | `archive/demo/probe/tests/test_langgraph_workflow.py`:20 | `ComponentAnalysisGraph.default(allow_noop_handlers=True)` | `default` | import | unjoined-in-indexed-file | ? | ? |
| C2-077 | `archive/demo/probe/tests/test_langgraph_workflow.py`:60 | `ComponentAnalysisGraph.default( checkpoint_store=store, allow_noop_handlers=True, )` | `default` | import | unjoined-in-indexed-file | ? | ? |
| C2-078 | `archive/demo/probe/tests/test_provider_workspace_bridge.py`:365 | `json_store.write( bundle.path("artifacts/generated-sbom.cdx.json"), {"bomFormat": "Cycl…` | `write` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-079 | `archive/demo/probe/tests/test_report_claims.py`:174 | `JsonStore().append( bundle.path("sandbox/run-results.jsonl"), _valid_sandbox_result(), )` | `append` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-080 | `archive/demo/probe/tests/test_run_bundle.py`:245 | `hash_service.sha256_bytes(copied)` | `sha256_bytes` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-081 | `archive/docs/reference/source-corpus/files/config/perplexity_handler.py`:206 | `resp.json()` | `json` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-082 | `archive/docs/reference/source-corpus/files/redglass/tools/research/tools.py`:221 | `pkg_path.startswith("node_modules/")` | `startswith` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-083 | `archive/docs/reference/source-corpus/files/redglass/tools/research/tools.py`:1017 | `row.get("status-code")` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-084 | `archive/docs/reference/source-corpus/files/redglass/tools/research/tools.py`:1890 | `row.get("cname")` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-085 | `archive/docs/reference/source-corpus/files/tests/unit/llm/test_factory.py`:114 | `monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-real-12345")` | `setenv` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-086 | `archive/docs/reference/source-corpus/files/tests/unit/observability/test_observability.py`:100 | `s.set_attribute("key", "value")` | `set_attribute` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-087 | `archive/src/redglass/adapters/llm/handlers/codex_chatgpt.py`:152 | `fn.get( "parameters", {"type": "object", "properties": {}}, )` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-088 | `archive/src/redglass/entrypoints/cli/analyze.py`:582 | `typer.Option( "none", "--component-limit", help="Component admission limit: none or an …` | `Option` | import | unjoined-in-indexed-file | ? | ? |
| C2-089 | `archive/src/redglass/entrypoints/cli/app.py`:14 | `typer.Typer( name="redglass", help="Run local Redglass component analysis.", no_args_is…` | `Typer` | import | unjoined-in-indexed-file | ? | ? |
| C2-090 | `archive/src/redglass/services/cloud/analyses.py`:112 | `e.model_dump(mode="json")` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-091 | `deploy/analysis/rust_recall_runner.py`:186 | `store.assert_ready_for_launch()` | `assert_ready_for_launch` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-092 | `deploy/analysis/source_provenance_runner.py`:519 | `driver.get("rules", [])` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-093 | `deploy/analysis/triton_harness.py`:556 | `inst.isTainted()` | `isTainted` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-094 | `packages/agent_runtime/redglass_agent_runtime/orchestration/pause_state.py`:324 | `snapshot.config.get("configurable", {})` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-095 | `packages/agent_runtime/redglass_agent_runtime/roles/smart_contract/definition.py`:29 | `catalog.advertised_for(Role.SMART_CONTRACT)` | `advertised_for` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-096 | `packages/agent_runtime/redglass_agent_runtime/roles/source_vuln/definition.py`:29 | `catalog.advertised_for(Role.SOURCE_VULN)` | `advertised_for` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-097 | `packages/agent_runtime/tests/test_dispatch.py`:577 | `tool.ainvoke( { "role": "source_vuln", "task": "scan", "component_id": DEFAULT_TEST_PUR…` | `ainvoke` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-098 | `packages/agent_runtime/tests/test_escalate_to_validation.py`:144 | `log.append( EventEnvelope( schema_version="1", event_id=ids.new_id(), sequence=0, times…` | `append` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-099 | `packages/agent_runtime/tests/test_run_pause_barrier.py`:264 | `log.read()` | `read` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-100 | `packages/contracts/tests/test_read_model_contracts.py`:97 | `FuzzResourceEvidence.model_validate(values)` | `model_validate` | import | unjoined-in-indexed-file | ? | ? |
| C2-101 | `packages/infra/redglass_infra/analysis/runtime_overhead_certificates.py`:72 | `certificate.model_dump( mode="json", exclude={"certificate_identity"} )` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-102 | `packages/infra/redglass_infra/analysis/taint_adapter.py`:395 | `self._sandbox.wait(handle)` | `wait` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-103 | `packages/infra/redglass_infra/artifacts/package.py`:78 | `value.startswith("pkg:pypi/")` | `startswith` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-104 | `packages/infra/redglass_infra/enrichment/mirror/manager.py`:483 | `self._clock.now().isoformat()` | `isoformat` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-105 | `packages/infra/redglass_infra/http/client.py`:218 | `httpx.HTTPTransport(retries=self._CONNECT_RETRIES)` | `HTTPTransport` | import | unjoined-in-indexed-file | ? | ? |
| C2-106 | `packages/infra/redglass_infra/llm/catalog.py`:147 | `LeadRoleBands.model_validate(bands)` | `model_validate` | import | unjoined-in-indexed-file | ? | ? |
| C2-107 | `packages/infra/redglass_infra/persistence/coverage_disclosure_store.py`:767 | `event.payload.get("locator_object_digest")` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-108 | `packages/infra/redglass_infra/research_egress/assembly.py`:130 | `self._capture.put(coordinate, cached)` | `put` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-109 | `packages/infra/tests/test_baseline_firing.py`:71 | `result.matched_advisory("pkg:github/madler/zlib@1.2.8")` | `matched_advisory` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-110 | `packages/infra/tests/test_mirror_manager.py`:568 | `snapshot.try_link_version(exdev_bundle)` | `try_link_version` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-111 | `packages/infra/tests/test_taint_emit_artifact_states.py`:763 | `_emit.read_published_report(out)` | `read_published_report` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-112 | `packages/pipeline/redglass_pipeline/knowledge/projection_readers.py`:108 | `event.payload.get("advisory_osv_id")` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-113 | `packages/pipeline/redglass_pipeline/validation/taint_evidence.py`:333 | `payload.model_dump(include=fields)` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-114 | `packages/pipeline/tests/test_pending_gate_rules.py`:536 | `denials[0].payload.get("fingerprint")` | `get` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-115 | `packages/pipeline/tests/test_report_projection.py`:261 | `synthetic_analyzer_result_recorder( log, FrozenClock(), ids ).record_execution( str(_RU…` | `record_execution` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-116 | `packages/pipeline/tests/test_reversing_recon_floor.py`:215 | `executed[0].model_dump( include=set(ToolExecutionEvidenceWire.model_fields) )` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-117 | `packages/pipeline/tests/test_toolchain_provision.py`:181 | `AnalysisProvenanceWire.model_validate( { "mode": AnalysisExtractionMode.BUILDLESS.value…` | `model_validate` | import | unjoined-in-indexed-file | ? | ? |
| C2-118 | `packages/pipeline/tests/test_usage_summary_projection_determinism.py`:74 | `payload.model_dump()` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-119 | `packages/pipeline/tests/test_work_state_determinism.py`:52 | `AgentArtifactRef( class_tag=ArtifactClassTag.ASSIGNMENT, digest=digest, schema_version=…` | `model_dump` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-120 | `packages/testing/redglass_testing/contracts/event_log.py`:152 | `log.append(harness.make_event(EventKind.SUPPRESSED))` | `append` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-121 | `packages/tools/redglass_tools/primitives/filesystem.py`:222 | `StructuredTool.from_function( func=_ls, name=ToolName.LS.value, description=( "Scan and…` | `from_function` | import | unjoined-in-indexed-file | ? | ? |
| C2-122 | `packages/tools/redglass_tools/research/advisory_lookup.py`:29 | `service.advisories(ecosystem, name)` | `advisories` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-123 | `packages/tools/redglass_tools/research/advisory_lookup.py`:77 | `StructuredTool.from_function( func=_lookup, name=ToolName.EPSS_LOOKUP.value, descriptio…` | `from_function` | import | unjoined-in-indexed-file | ? | ? |
| C2-124 | `packages/tools/redglass_tools/tracing/tracing.py`:98 | `StructuredTool.from_function( func=_run, name=ToolName.TRACE_SYSCALLS.value, descriptio…` | `from_function` | import | unjoined-in-indexed-file | ? | ? |
| C2-125 | `packages/tools/tests/test_analysis_delegates.py`:311 | `tool.invoke({"target": "Vault.sol"})` | `invoke` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-126 | `packages/tools/tests/test_matrix_delegates.py`:551 | `tools[ToolName.FORGE_TEST].invoke( { "harness": "contract Harness { function invariant_…` | `invoke` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-127 | `scripts/smokes/smoke_frida_intercept_firing.py`:274 | `bundle.staged_target_path()` | `staged_target_path` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-128 | `scripts/smokes/smoke_real_size_analysis.py`:358 | `AnalyzerImageResolver.default()` | `default` | import | unjoined-in-indexed-file | ? | ? |
| C2-129 | `scripts/smokes/smoke_source_language_certificates.py`:417 | `output.write_text(_SOLIDITY_CLEAN, encoding="utf-8")` | `write_text` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-130 | `tests/architecture/test_engine_canary_smoke.py`:189 | `smoke_engine_canary._certify_resource_exceptions( image="localhost/redglass-sandbox@sha…` | `_certify_resource_exceptions` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-131 | `tests/architecture/test_recipe_driver_registry.py`:59 | `default_recipe_drivers(emit=_discard).values()` | `values` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-132 | `tests/architecture/test_role_tool_grants.py`:140 | `GRANTS.values()` | `values` | import | unjoined-in-indexed-file | ? | ? |
| C2-133 | `tests/architecture/test_source_language_certificate.py`:74 | `source.is_file()` | `is_file` | unresolved | unjoined-in-indexed-file | ? | ? |
| C2-134 | `tests/architecture/test_tool_execution_evidence_totality.py`:655 | `CoverageDisclosureManifest.model_json_schema()` | `model_json_schema` | import | unjoined-in-indexed-file | ? | ? |
| C2-135 | `.claude/skills/claude-md-audit/evals/scripts/eval_harness.py`:297 | `banner( "setup", iteration=current_iteration, tests=f"{len(tests)} tests x 2 variants =…` | `banner` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-136 | `.claude/skills/claude-md-audit/evals/scripts/eval_harness.py`:537 | `task.stderr_handle.close()` | `close` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-137 | `.claude/skills/claude-md-audit/evals/scripts/eval_harness.py`:880 | `sum(result["total"] for result in results)` | `sum` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-138 | `.claude/skills/claude-md-audit/scripts/discover.py`:357 | `stripped[2:].strip().strip("\"'")` | `strip` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-139 | `.claude/skills/claude-md-audit/scripts/discover.py`:723 | `_write_phase_summary(phase_data)` | `_write_phase_summary` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-140 | `.claude/skills/llm-redteam/harness/config_generator.py`:51 | `plugin.startswith(REMOTE_ONLY_PREFIXES)` | `startswith` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-141 | `.claude/skills/llm-redteam/harness/config_generator.py`:94 | `path.startswith("$")` | `startswith` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-142 | `.claude/skills/llm-redteam/harness/config_generator.py`:121 | `list(dict.fromkeys(p for p in plugins if not _is_remote_only(p)))` | `list` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-143 | `.claude/skills/llm-redteam/harness/engine.py`:139 | `native_grader.grade(probe.output, asserts)` | `grade` | import | unjoined-file-not-indexed | ? | ? |
| C2-144 | `.claude/skills/llm-redteam/harness/recon.py`:142 | `path.read_text(encoding="utf-8", errors="ignore")` | `read_text` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-145 | `.claude/skills/llm-redteam/harness/recon.py`:149 | `any(p.search(line) for p in patterns)` | `any` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-146 | `.claude/skills/llm-redteam/harness/recon.py`:302 | `any(p.search(line) for p in patterns)` | `any` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-147 | `.claude/skills/llm-redteam/harness/recon.py`:322 | `vectors.items()` | `items` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-148 | `.claude/skills/llm-redteam/harness/runner.py`:88 | `isinstance(status, int)` | `isinstance` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-149 | `.claude/skills/llm-redteam/harness/runner.py`:180 | `RunnerError("npx not found on PATH — run the Node prereq check first")` | `RunnerError` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-150 | `.claude/skills/llm-redteam/harness/runner.py`:223 | `subprocess.run( # nosec B603 # fixed argv, abs npx, no shell cmd, cwd=str(gc.config_pat…` | `run` | import | unjoined-file-not-indexed | ? | ? |
| C2-151 | `.claude/skills/llm-redteam/harness/target_client.py`:134 | `TargetError(f"refusing non-http(s) target URL: {url}")` | `TargetError` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-152 | `.claude/skills/llm-redteam/harness/target_client.py`:140 | `resp.read().decode("utf-8", "ignore")` | `decode` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-153 | `.claude/skills/llm-redteam/tests/_poison_server.py`:66 | `"\n".join(out)` | `join` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-154 | `.claude/skills/llm-redteam/tests/_poison_server.py`:180 | `self._json(200, {"reply": f"Answer.\n{rendered}".rstrip()})` | `_json` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-155 | `.claude/skills/llm-redteam/tests/test_cli.py`:202 | `str(fixtures_dir / "sample_source")` | `str` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-156 | `.claude/skills/llm-redteam/tests/test_descriptor.py`:61 | `descriptor.load(p)` | `load` | import | unjoined-file-not-indexed | ? | ? |
| C2-157 | `.claude/skills/llm-redteam/tests/test_poison_cli.py`:96 | `(out_dir / "poison-report.md").read_text()` | `read_text` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-158 | `.claude/skills/llm-redteam/tests/test_poison_executor.py`:219 | `_descriptor(srv.server_address[1])` | `_descriptor` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-159 | `.claude/skills/llm-redteam/tests/test_poison_executor.py`:556 | `_chunk(b"IDAT", idat)` | `_chunk` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-160 | `.claude/skills/llm-redteam/tests/test_poisoning.py`:115 | `load_workflow(_write(tmp_path, cmd))` | `load_workflow` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-161 | `.claude/skills/llm-redteam/tests/test_report.py`:90 | `md.splitlines()` | `splitlines` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-162 | `.claude/skills/llm-redteam/tests/test_target_client.py`:52 | `self.send_response(200)` | `send_response` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-163 | `.claude/skills/llm-redteam/tests/test_target_client.py`:161 | `_descriptor(9)` | `_descriptor` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-164 | `.claude/skills/llm-redteam/tests/test_target_client.py`:179 | `models.AuthSpec(type="none", tenant_header="X-Tenant-Id")` | `AuthSpec` | import | unjoined-file-not-indexed | ? | ? |
| C2-165 | `apps/console/src/api/events.test.ts`:21 | `StreamEventsV1RunsRunIdEventsGetResponse.safeParse(FRAME)` | `safeParse` | import | unjoined-file-not-indexed | ? | ? |
| C2-166 | `apps/console/src/api/liveSync.ts`:176 | `useRef(0)` | `useRef` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-167 | `apps/console/src/app/routeComponents.tsx`:92 | `lazy(() => import("../views/graph/AttackGraphView") .then((m) => ({ default: m.AttackGr…` | `lazy` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-168 | `apps/console/src/views/advisory/AdvisoryLedgerView.test.tsx`:172 | `expect(screen.queryByText("no recorded deviation"))` | `expect` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-169 | `apps/console/src/views/agents/OrchestrationOverlay.test.tsx`:69 | `refused.getByTitle(`declined near ${SECRET}`)` | `getByTitle` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-170 | `apps/console/src/views/agents/OrchestrationOverlay.test.tsx`:75 | `view(null)` | `view` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-171 | `apps/console/src/views/agents/SpecialistArtifactPanel.test.tsx`:64 | `expect(label.textContent)` | `expect` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-172 | `apps/console/src/views/analysis/BinaryReconPanel.test.tsx`:178 | `expect(screen.getByText(/partial: yes/)).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-173 | `apps/console/src/views/analysis/FuzzHealthPanel.test.tsx`:211 | `screen.getByText("artifacts")` | `getByText` | import | unjoined-file-not-indexed | ? | ? |
| C2-174 | `apps/console/src/views/analysis/RunAssurancePanel.test.tsx`:270 | `expect(screen.getByText("closure not clean")).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-175 | `apps/console/src/views/analysis/ToolExecutionReceipt.tsx`:188 | `recorded(artifact?.bundle_path)` | `recorded` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-176 | `apps/console/src/views/authz/AuthzCoverageView.test.tsx`:77 | `expect(new Set(classes).size).toBeGreaterThan(1)` | `toBeGreaterThan` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-177 | `apps/console/src/views/dynamic-confirmation/DynamicConfirmationView.test.tsx`:115 | `render(<DynamicConfirmationView runId="run-1" />)` | `render` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-178 | `apps/console/src/views/graph/ForceGraph.test.tsx`:18 | `vi.fn()` | `fn` | import | unjoined-file-not-indexed | ? | ? |
| C2-179 | `apps/console/src/views/graph/danger.test.ts`:134 | `expect(reachabilityTier(undefined)).toBe("unknown")` | `toBe` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-180 | `apps/console/src/views/graph/danger.test.ts`:143 | `nodeShape({ kind: "Entrypoint", is_entrypoint: true })` | `nodeShape` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-181 | `apps/console/src/views/graph/feeder.test.ts`:35 | `feeder.merge(view([{ id: "a", is_dangerous_sink: true }]))` | `merge` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-182 | `apps/console/src/views/history/compare.test.ts`:39 | `expect(t.latest.run_id)` | `expect` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-183 | `apps/console/src/views/home/HomeView.tsx`:13 | `useRunCatalog()` | `useRunCatalog` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-184 | `apps/console/src/views/overview/ElapsedTime.tsx`:66 | `formatDuration(seconds)` | `formatDuration` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-185 | `apps/console/src/views/overview/RunControlButtons.test.tsx`:48 | `screen.getByRole("button", { name: /pause/i })` | `getByRole` | import | unjoined-file-not-indexed | ? | ? |
| C2-186 | `apps/console/src/views/runtime-trace/RuntimeTraceView.test.tsx`:489 | `expect(screen.getByRole("alert")).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-187 | `apps/console/src/views/taint/TaintCoverage.test.tsx`:153 | `expect( await screen.findByRole("heading", { name: "Taint coverage" }), )` | `expect` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-188 | `archive/dashboard/console/src/api/liveSync.ts`:56 | `keysFor(runId, ev.kind)` | `keysFor` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-189 | `archive/dashboard/console/src/contract/schema.test.ts`:117 | `expect(r.attack_paths[0].crown_jewel_id).toBe("n-crown")` | `toBe` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-190 | `archive/dashboard/console/src/contract/schema.test.ts`:179 | `runRejectedSchema.parse({ reason: "concurrency_cap", detail: "max 3", max_concurrent: 3…` | `parse` | import | unjoined-file-not-indexed | ? | ? |
| C2-191 | `archive/dashboard/console/src/contract/schema.test.ts`:222 | `traceStepSchema.parse({ ...base, type: "reasoning", thinking_detected: true, redacted: …` | `parse` | import | unjoined-file-not-indexed | ? | ? |
| C2-192 | `archive/dashboard/console/src/contract/schema.test.ts`:249 | `expect(() => componentInventorySchema.parse({ run_id: "r", components: [withoutSourceRe…` | `expect` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-193 | `archive/dashboard/console/src/contract/schema.ts`:171 | `z.object({ type: z.literal("message"), agent_id: z.string(), seq: z.number(), ts: iso, …` | `object` | import | unjoined-file-not-indexed | ? | ? |
| C2-194 | `archive/dashboard/console/src/design/elapsed.test.ts`:36 | `expect(elapsedSeconds(from, Date.parse("")))` | `expect` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-195 | `archive/dashboard/console/src/mock/fixtures/findings.ts`:63 | `finding({ finding_id: "FIND-003", title: "Suspected UAF in reset path", severity: "high…` | `finding` | in-file | unjoined-file-not-indexed | ? | ? |
| C2-196 | `archive/dashboard/console/src/views/findings/TierChips.tsx`:14 | `onToggle(tier)` | `onToggle` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-197 | `archive/dashboard/console/src/views/findings/TierChips.tsx`:17 | `active.has(tier)` | `has` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-198 | `archive/dashboard/console/src/views/home/useChangeHighlight.ts`:34 | `new Set()` | `Set` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-199 | `archive/dashboard/console/src/views/overview/OverviewView.tsx`:85 | `useLiveSync(runId, events, liveness)` | `useLiveSync` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-200 | `archive/dashboard/console/src/views/usage/UsageView.test.tsx`:32 | `expect(screen.getByText(/USD not computed/)).toBeInTheDocument()` | `toBeInTheDocument` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-201 | `archive/docs/reference/source-corpus/files/clients/cli/src/components/messages/DelegateMessage.tsx`:19 | `React.memo(function DelegateMessage({ agent, content, }: Props) { const screen = useScr…` | `memo` | import | unjoined-file-not-indexed | ? | ? |
| C2-202 | `archive/docs/reference/source-corpus/files/clients/cli/src/hooks/useAgent.ts`:193 | `setRunState("idle")` | `setRunState` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-203 | `archive/docs/reference/source-corpus/files/clients/cli/src/index.tsx`:7 | `args.includes("--resume")` | `includes` | unresolved | unjoined-file-not-indexed | ? | ? |
| C2-204 | `archive/docs/reference/source-corpus/files/clients/web/server/terminal-server.ts`:31 | `(process.env.TERMINAL_ALLOWED_ORIGINS ?? `http://localhost:${WEB_PORT},http://127.0.0.1…` | `filter` | unresolved | unjoined-file-not-indexed | ? | ? |

## 6. Results

_TBD — filled once every row above carries a class._
