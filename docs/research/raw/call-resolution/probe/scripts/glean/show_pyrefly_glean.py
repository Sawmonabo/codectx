"""Print python.* xref and call facts from pyrefly --report-glean JSON files.
Usage: python3 -I show_pyrefly_glean.py SRC_ROOT JSON...
"""
import json, sys, os

root = sys.argv[1]
for path in sys.argv[2:]:
    data = json.load(open(path))
    preds = {e.get("predicate"): e.get("facts", []) for e in data if "predicate" in e}
    file_key = None
    for f in preds.get("src.FileLanguage.1", []):
        file_key = f["key"]["file"]["key"]
    src = open(os.path.join(root, file_key), "rb").read() if file_key and os.path.exists(os.path.join(root, file_key)) else b""
    print("=== file", file_key, "(", os.path.basename(path), ")")
    def text(sp):
        return src[sp["start"]:sp["start"] + sp["length"]].decode()
    def line(sp):
        return src[:sp["start"]].count(b"\n") + 1
    for f in preds.get("python.XRefsViaNameByFile.4", []):
        for x in f["key"]["xrefs"]:
            sp = x["source"]
            print(f"  XRefViaName  line {line(sp)} {text(sp)!r:12} -> {x['target']['key']}")
    for f in preds.get("python.xrefs.XRefsByFile.1", []):
        for x in f["key"]["xrefs"]:
            sp = x["source"]; t = x["target"]
            fl = t.get("file")
            fl = fl["key"] if isinstance(fl, dict) else fl
            if text(sp) in ("sound",):
                print(f"  xrefs.XRef   line {line(sp)} {text(sp)!r:12} -> name={t['name']['key']} file={fl}")
    for f in preds.get("python.CalleeToCaller.4", []):
        print(f"  CalleeToCaller callee={f['key']['callee']['key']} caller={f['key']['caller']['key']}")
    for f in preds.get("python.FileCall.4", []):
        sp = f["key"]["callee_span"]
        print(f"  FileCall     line {line(sp)} {text(sp)!r}")
    print("  predicates:", sorted(k for k, v in preds.items() if v))
