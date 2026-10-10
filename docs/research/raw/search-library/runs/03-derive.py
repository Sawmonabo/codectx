# Derives 03-pooled-latency.txt and 03-first-page.txt from the per-run result
# lines in <bench>/results/ (not published: each file is several MB of page
# hashes). Usage: python3 -I 03-derive.py <bench>/results <ours-query.log>
import json, statistics, sys, os
res, ourslog = sys.argv[1], sys.argv[2]

def lat():
    print("Warm query latency on kubernetes, first page of 200: 30 queries x 5 warm runs each")
    print("%-12s %-12s %7s %9s %14s %22s" % ("phase", "arm", "queries", "warm runs", "median, all runs", "sum of per-query medians"))
    for ph in ['speed', 'speed-kind', 'speed-path', 'speed-11seg', 'speed-t16']:
        for arm, f in [('ours', 'ours-query-%s.jsonl' % ph), ('library', 'tantivy-library-%s.jsonl' % ph),
                       ('library-all', 'tantivy-library-all-%s.jsonl' % ph), ('product', 'tantivy-product-%s.jsonl' % ph)]:
            p = os.path.join(res, f)
            if not os.path.exists(p):
                continue
            byq = {}
            for l in open(p):
                d = json.loads(l)
                if d['run'] > 0:
                    byq.setdefault(d['query'], []).append(d['wall_ms'])
            warm = [w for v in byq.values() for w in v]
            per = [statistics.median(v) for v in byq.values()]
            print("%-12s %-12s %7d %9d %11.2f ms %19.1f ms" % (ph, arm, len(byq), len(warm), statistics.median(warm), sum(per)))
    print("speed-kind: --kind function --kind method; speed-path: --path-prefix pkg; speed-11seg: the 11-segment index;")
    print("speed-t16: 16 search threads (library arms only). Our arm has no 11seg or t16 phase.")

def first_page():
    def load(f):
        r = {}
        for l in open(os.path.join(res, f)):
            d = json.loads(l)
            if d['run'] == 0:
                r[d['query']] = d
        return r
    o, p = load('ours-query-speed.jsonl'), load('tantivy-product-speed.jsonl')
    served = {}
    for line in open(ourslog):
        if line.startswith('==='):
            cur = line.split()[1]
            continue
        if line.startswith('{') and cur == 'speed':
            d = json.loads(line)
            served[d['query']] = d['served_hits']
    same = sum(1 for q in o if o[q]['page'] == p[q]['page'])
    folded = sum(1 for q in o if served[q] == p[q]['hits'])
    print("Our arm's document-level first page against the library arm's product mode (our rules, folded), 30 queries")
    print("identical first page: %d/30; our served (folded) hit count equal to product hits: %d/30" % (same, folded))
    print("%-24s %8s %9s %9s %9s %9s" % ("query", "1st diff", "ours docs", "served", "product", "page len"))
    for q in o:
        a, b = o[q]['page'], p[q]['page']
        if a == b:
            continue
        i = next((k for k in range(min(len(a), len(b))) if a[k] != b[k]), min(len(a), len(b)))
        print("%-24s %8d %9d %9d %9d %5d/%d" % (q, i + 1, o[q]['hits'], served[q], p[q]['hits'], len(a), len(b)))

{'lat': lat, 'page': first_page}[sys.argv[3]]()
