import java.util.ArrayList;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;

import soot.G;
import soot.PackManager;
import soot.Scene;
import soot.SootClass;
import soot.SootMethod;
import soot.jimple.Stmt;
import soot.jimple.toolkits.callgraph.CallGraph;
import soot.jimple.toolkits.callgraph.Edge;
import soot.options.Options;

/**
 * Builds a Soot call graph (cha or spark) over one tiny class directory and prints, for every
 * zoo.*.sound method, the edges into it, plus every edge out of zoo.Main.speak.
 *
 * usage: java -cp soot.jar:. SootProbe CLASS_DIR cha|spark [all-reachable]
 */
public class SootProbe {
  public static void main(String[] args) {
    String dir = args[0];
    String algo = args[1];
    boolean allReachable = args.length > 2 && args[2].equals("all-reachable");

    G.reset();
    Options o = Options.v();
    o.set_whole_program(true);
    o.set_prepend_classpath(true);
    o.set_soot_classpath(dir);
    o.set_process_dir(Collections.singletonList(dir));
    o.set_allow_phantom_refs(true);
    o.set_no_bodies_for_excluded(true);
    List<String> excl = new ArrayList<>();
    excl.add("java.*");
    excl.add("jdk.*");
    excl.add("sun.*");
    o.set_exclude(excl);
    o.set_output_format(Options.output_format_none);
    o.set_main_class("zoo.Main");
    o.setPhaseOption("cg", "all-reachable:" + allReachable);
    if (algo.equals("cha")) {
      o.setPhaseOption("cg.cha", "on");
    } else {
      o.setPhaseOption("cg.spark", "on");
    }
    Scene.v().loadNecessaryClasses();
    PackManager.v().getPack("cg").apply();
    CallGraph cg = Scene.v().getCallGraph();

    System.out.println("algo=" + algo + " all-reachable=" + allReachable + " dir=" + dir);
    for (String cls : new String[] {"zoo.Animal", "zoo.Dog", "zoo.Cat"}) {
      SootMethod m = Scene.v().getSootClass(cls).getMethodByName("sound");
      System.out.println("edgesInto(" + m.getSignature() + "):");
      for (Iterator<Edge> it = cg.edgesInto(m); it.hasNext(); ) {
        print(it.next());
      }
    }
    SootMethod speak = Scene.v().getSootClass("zoo.Main").getMethodByName("speak");
    System.out.println("edgesOutOf(" + speak.getSignature() + "):");
    for (Iterator<Edge> it = cg.edgesOutOf(speak); it.hasNext(); ) {
      print(it.next());
    }
  }

  private static void print(Edge e) {
    Stmt s = e.srcStmt();
    String declared = (s != null && s.containsInvokeExpr())
        ? s.getInvokeExpr().getMethodRef().getSignature() : "-";
    System.out.println("  src=" + e.src().getSignature() + " kind=" + e.kind() + " tgt="
        + e.tgt().getSignature() + " stmt=[" + s + "] declaredTargetFromStmt=" + declared);
  }
}
