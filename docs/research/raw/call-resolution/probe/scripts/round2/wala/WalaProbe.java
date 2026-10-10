import java.io.File;
import java.util.Iterator;
import java.util.Set;

import com.ibm.wala.classLoader.CallSiteReference;
import com.ibm.wala.classLoader.IClass;
import com.ibm.wala.classLoader.IMethod;
import com.ibm.wala.classLoader.Language;
import com.ibm.wala.core.util.config.AnalysisScopeReader;
import com.ibm.wala.ipa.callgraph.AnalysisCacheImpl;
import com.ibm.wala.ipa.callgraph.AnalysisOptions;
import com.ibm.wala.ipa.callgraph.AnalysisScope;
import com.ibm.wala.ipa.callgraph.CGNode;
import com.ibm.wala.ipa.callgraph.CallGraph;
import com.ibm.wala.ipa.callgraph.Entrypoint;
import com.ibm.wala.ipa.callgraph.cha.CHACallGraph;
import com.ibm.wala.ipa.callgraph.impl.AllApplicationEntrypoints;
import com.ibm.wala.ipa.callgraph.impl.Util;
import com.ibm.wala.ipa.cha.ClassHierarchyFactory;
import com.ibm.wala.ipa.cha.IClassHierarchy;
import com.ibm.wala.types.ClassLoaderReference;
import com.ibm.wala.types.TypeReference;

/**
 * Builds a WALA call graph (cha or 0cfa) over one tiny class directory and prints, per call site
 * in zoo.Main.speak, the declared target, the invocation code and the targets; then, for every
 * zoo.*.sound node, its callers and the caller sites that reach it.
 *
 * usage: java -cp wala-cp:. WalaProbe CLASS_DIR EXCLUSIONS_FILE cha|0cfa main|all
 */
public class WalaProbe {
  public static void main(String[] args) throws Exception {
    String dir = args[0];
    File exclusions = new File(args[1]);
    String algo = args[2];
    String entry = args[3];

    AnalysisScope scope = AnalysisScopeReader.instance.makeJavaBinaryAnalysisScope(dir, exclusions);
    IClassHierarchy cha = ClassHierarchyFactory.make(scope);
    Iterable<Entrypoint> eps = entry.equals("main")
        ? Util.makeMainEntrypoints(cha, "Lzoo/Main")
        : new AllApplicationEntrypoints(scope, cha);

    CallGraph cg;
    if (algo.equals("cha")) {
      CHACallGraph c = new CHACallGraph(cha);
      c.init(eps);
      cg = c;
    } else {
      AnalysisOptions options = new AnalysisOptions(scope, eps);
      cg = Util.makeZeroCFABuilder(Language.JAVA, options, new AnalysisCacheImpl(), cha)
          .makeCallGraph(options, null);
    }
    System.out.println("algo=" + algo + " entrypoints=" + entry + " dir=" + dir);

    IClass mainCls = cha.lookupClass(
        TypeReference.findOrCreate(ClassLoaderReference.Application, "Lzoo/Main"));
    for (IMethod m : mainCls.getDeclaredMethods()) {
      if (!m.getName().toString().equals("speak")) continue;
      for (CGNode n : cg.getNodes(m.getReference())) {
        System.out.println("node " + n.getMethod().getSignature() + " ctx=" + n.getContext());
        for (Iterator<CallSiteReference> it = n.iterateCallSites(); it.hasNext(); ) {
          CallSiteReference site = it.next();
          Set<CGNode> targets = cg.getPossibleTargets(n, site);
          System.out.println("  site pc=" + site.getProgramCounter()
              + " invocationCode=" + site.getInvocationCode()
              + " declaredTarget=" + site.getDeclaredTarget().getSignature()
              + " getNumberOfTargets=" + cg.getNumberOfTargets(n, site));
          for (CGNode t : targets) {
            System.out.println("    target " + t.getMethod().getSignature());
          }
        }
      }
    }
    for (String cls : new String[] {"Lzoo/Animal", "Lzoo/Dog", "Lzoo/Cat"}) {
      IClass c = cha.lookupClass(TypeReference.findOrCreate(ClassLoaderReference.Application, cls));
      for (IMethod m : c.getDeclaredMethods()) {
        if (!m.getName().toString().equals("sound")) continue;
        Set<CGNode> nodes = cg.getNodes(m.getReference());
        System.out.println("callers of " + m.getSignature() + " (nodes=" + nodes.size() + "):");
        for (CGNode n : nodes) {
          for (Iterator<CGNode> preds = cg.getPredNodes(n); preds.hasNext(); ) {
            CGNode p = preds.next();
            for (Iterator<CallSiteReference> sites = cg.getPossibleSites(p, n); sites.hasNext(); ) {
              CallSiteReference s = sites.next();
              System.out.println("  caller " + p.getMethod().getSignature()
                  + " site pc=" + s.getProgramCounter()
                  + " invocationCode=" + s.getInvocationCode()
                  + " declaredTarget=" + s.getDeclaredTarget().getSignature());
            }
          }
        }
      }
    }
  }
}
