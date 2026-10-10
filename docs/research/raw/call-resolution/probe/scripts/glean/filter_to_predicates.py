"""Copy pyrefly Glean JSON keeping only predicates listed in ALLOWED_FILE (one name.version per line).
Usage: python3 -I filter_to_predicates.py OUT_DIR ALLOWED_FILE JSON...
"""
import json, os, sys
out, allowed = sys.argv[1], set(open(sys.argv[2]).read().split())
os.makedirs(out, exist_ok=True)
for p in sys.argv[3:]:
    d = json.load(open(p))
    kept = [e for e in d if e.get("predicate") in allowed]
    dropped = sorted(e.get("predicate", "schema_id") for e in d if e.get("predicate") not in allowed)
    json.dump(kept, open(os.path.join(out, os.path.basename(p)), "w"))
    print(os.path.basename(p), "dropped:", dropped)
