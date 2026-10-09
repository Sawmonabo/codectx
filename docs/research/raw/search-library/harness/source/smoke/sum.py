# sum.py <ref label> <label>...: per label, the median over queries of the warm median and of the cold run,
# and how many queries answer exactly as the reference (same hits and page).
import json, glob, os, statistics, sys
base = os.path.join(os.path.dirname(__file__), '..', 'results-v', 'q')
def load(label):
    fs = [f for f in glob.glob(os.path.join(base, label, '*.jsonl')) if not f.endswith(('-open.jsonl', '-cache.jsonl', '-explain.jsonl'))]
    if not fs:
        return None
    by = {}
    for l in open(fs[0]):
        j = json.loads(l)
        by.setdefault(j['query'], []).append(j)
    return by
ref = load(sys.argv[1])
for label in sys.argv[2:]:
    d = load(label)
    if d is None:
        print(f'{label:34s} no result'); continue
    warm = [statistics.median([r['wall_ms'] for r in v if r['run'] > 0]) for v in d.values()]
    cold = [r['wall_ms'] for v in d.values() for r in v if r['run'] == 0]
    same = sum(1 for q, v in d.items() if ref and q in ref and v[0]['hits'] == ref[q][0]['hits'] and v[0]['page'] == ref[q][0]['page'])
    sameset = sum(1 for q, v in d.items() if ref and q in ref and set(v[0]['page']) == set(ref[q][0]['page']))
    jac = [len(set(v[0]['page']) & set(ref[q][0]['page'])) / max(1, len(set(v[0]['page']) | set(ref[q][0]['page']))) for q, v in d.items() if ref and q in ref]
    hits = sum(v[0]['hits'] for v in d.values())
    extra = d[next(iter(d))][0]
    hb = extra.get('hide_build_ms')
    print(f'{label:34s} warm p50 {statistics.median(warm):8.3f} ms  cold p50 {statistics.median(cold):8.3f} ms  same {same}/{len(d)} set {sameset}/{len(d)} jaccard {statistics.mean(jac) if jac else 0:.3f}  hits {hits}' + (f'  hide_build {hb:.3f} ms hidden {extra.get("hidden_pct")} stats_incl_hidden {extra.get("stats_include_hidden")}' if hb is not None else ''))
