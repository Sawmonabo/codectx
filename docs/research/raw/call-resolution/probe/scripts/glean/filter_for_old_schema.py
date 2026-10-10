"""Copy pyrefly Glean JSON, dropping the schema_id entry and the given predicate prefixes.
Usage: python3 -I filter_for_old_schema.py OUT_DIR PREFIXES(comma) JSON...
"""
import json, os, sys
out, prefixes = sys.argv[1], sys.argv[2].split(",")
os.makedirs(out, exist_ok=True)
for p in sys.argv[3:]:
    d = json.load(open(p))
    kept = [e for e in d if "predicate" in e and not any(e["predicate"].startswith(x) for x in prefixes)]
    dropped = sorted({e.get("predicate", "schema_id") for e in d} - {e["predicate"] for e in kept})
    json.dump(kept, open(os.path.join(out, os.path.basename(p)), "w"))
    print(os.path.basename(p), "dropped:", dropped)
