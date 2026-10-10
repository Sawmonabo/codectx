/**
 * @name Call targets of every call (new data-flow dispatch)
 * @kind table
 * @id probe/python-targets
 */
import python
import semmle.python.dataflow.new.internal.DataFlowDispatch as D

from CallNode call, string tgt, string ty
where
  call.getLocation().getFile().getShortName() = ["animals.py", "deep.py"] and
  (
    exists(Function f, D::CallType t | D::resolveCall(call, f, t) and tgt = f.getQualifiedName() and ty = t.toString())
    or
    not D::resolveCall(call, _, _) and tgt = "(no target)" and ty = "-"
  )
select call.getLocation().getFile().getShortName() as file, call.getScope().getName() as caller,
  call.getLocation().getStartLine() as line, call.toString() as site, tgt as target, ty as calltype
