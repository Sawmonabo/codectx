/**
 * @name Callers of each sound/talk method under calls and polyCalls
 * @kind table
 * @id probe/java-callers
 */
import java

string q(Callable c) { result = c.getDeclaringType().getName() + "." + c.getName() }

from Method m, Callable caller, string view
where
  m.fromSource() and
  m.getName() = ["sound", "talk"] and
  caller.fromSource() and
  (
    view = "calls" and caller.calls(m)
    or
    view = "polyCalls" and caller.polyCalls(m)
  )
select q(m) as method, view, q(caller) as callerName
