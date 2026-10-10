import java.util.Collections;
import java.util.List;

import sootup.callgraph.CallGraph;
import sootup.callgraph.ClassHierarchyAnalysisAlgorithm;
import sootup.callgraph.RapidTypeAnalysisAlgorithm;
import sootup.core.signatures.MethodSignature;
import sootup.java.bytecode.frontend.inputlocation.JavaClassPathAnalysisInputLocation;
import sootup.java.core.JavaIdentifierFactory;
import sootup.java.core.views.JavaView;

/**
 * Builds a SootUp call graph (cha or rta) over one tiny class directory from one entry method and
 * prints callsTo(...) for every zoo.*.sound method and callsFrom(speak), with the declared target
 * read back from each Call's invokable statement.
 *
 * usage: java -cp sootup-cp:. SootUpProbe CLASS_DIR cha|rta main|speak
 */
public class SootUpProbe {
  public static void main(String[] args) {
    String dir = args[0];
    String algo = args[1];
    String entry = args[2];

    JavaView view = new JavaView(new JavaClassPathAnalysisInputLocation(dir));
    JavaIdentifierFactory f = view.getIdentifierFactory();
    MethodSignature mainSig =
        f.getMethodSignature("zoo.Main", "main", "void", List.of("java.lang.String[]"));
    MethodSignature speakSig =
        f.getMethodSignature("zoo.Main", "speak", "void", List.of("zoo.Animal"));
    List<MethodSignature> entries =
        Collections.singletonList(entry.equals("main") ? mainSig : speakSig);

    CallGraph cg = algo.equals("cha")
        ? new ClassHierarchyAnalysisAlgorithm(view).initialize(entries)
        : new RapidTypeAnalysisAlgorithm(view).initialize(entries);

    System.out.println("algo=" + algo + " entry=" + entries + " dir=" + dir);
    for (String cls : new String[] {"zoo.Animal", "zoo.Dog", "zoo.Cat"}) {
      MethodSignature m = f.getMethodSignature(cls, "sound", "void", List.of());
      if (!cg.containsMethod(m)) {
        System.out.println("containsMethod(" + m + ") = false (not a node in the call graph)");
        continue;
      }
      System.out.println("callsTo(" + m + "):");
      for (CallGraph.Call c : cg.callsTo(m)) {
        print(c);
      }
      System.out.println("callSourcesTo(" + m + ") = " + cg.callSourcesTo(m));
    }
    if (!cg.containsMethod(speakSig)) {
      System.out.println("containsMethod(" + speakSig + ") = false (not a node in the call graph)");
      return;
    }
    System.out.println("callsFrom(" + speakSig + "):");
    for (CallGraph.Call c : cg.callsFrom(speakSig)) {
      print(c);
    }
  }

  private static void print(CallGraph.Call c) {
    String declared = c.invokableStmt().getInvokeExpr()
        .map(ie -> ie.getClass().getSimpleName() + " " + ie.getMethodSignature())
        .orElse("-");
    System.out.println("  " + c + " declaredFromStmt=" + declared);
  }
}
