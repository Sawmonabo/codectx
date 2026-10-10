/**
 * @name Callees of every invocation with imprecision
 * @kind table
 * @id probe/js-targets
 */
import javascript

string quality(DataFlow::InvokeNode c) {
  if c.isImprecise() then result = "imprecise" else
  if c.isIncomplete() then result = "incomplete" else result = "-"
}

from DataFlow::InvokeNode call, string tgt, int imp
where
  call.getFile().getBaseName() = ["animals.ts", "deep.ts"] and
  (
    exists(Function f | f = call.getACallee(imp) and tgt = f.getName() + "@" + f.getLocation().getStartLine())
    or
    not exists(call.getACallee(_)) and tgt = "(no callee)" and imp = -1
  )
select call.getFile().getBaseName() as file, call.getContainer().(Function).getName() as caller,
  call.getStartLine() as line, call.toString() as site, tgt as callee, imp as imprecision, quality(call) as quality
