/**
 * @name Call targets of each sound/talk call
 * @kind table
 * @id probe/java-targets
 */
import java
import semmle.code.java.dispatch.VirtualDispatch

string q(Callable c) { result = c.getDeclaringType().getName() + "." + c.getName() }

from MethodCall c, string view, Callable t
where
  c.getMethod().getName() = ["sound", "talk"] and
  (
    view = "1 getMethod (static target)" and t = c.getMethod()
    or
    view = "2 viableCallable (dispatch targets)" and t = viableCallable(c)
    or
    view = "3 exactVirtualMethod (single target)" and t = exactVirtualMethod(c)
  )
select q(c.getEnclosingCallable()) as caller, c.getLocation().getStartLine() as line, c.toString() as site, view, q(t) as target
