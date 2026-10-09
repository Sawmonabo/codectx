# diag.py <explain.jsonl>: per query, per-field score contributions of the top 10 and of the relevant document.
import json, re, sys, collections
F = ['name', 'qualified_name', 'signature', 'path', 'body']
def contribs(e, out):
    d = e.get('description', '')
    if d.startswith('Boost x'):
        ctx = json.dumps(e, ensure_ascii=False)
        m = re.search(r'field=(\d+), type=Str, \\"([^\\"]*)', ctx)
        n = re.search(r'"n, number of docs containing this term", "value": ([0-9.]+)', ctx)
        dl = re.search(r'"dl, length of field", "value": ([0-9.]+)', ctx)
        av = re.search(r'"avgdl, average length of field", "value": ([0-9.]+)', ctx)
        if m:
            out.append((F[int(m.group(1))], m.group(2), e['value'], n and float(n.group(1)), dl and float(dl.group(1)), av and float(av.group(1))))
        return
    for c in e.get('details', []) or []:
        contribs(c, out)
rows = [json.loads(l) for l in open(sys.argv[1])]
by = collections.OrderedDict()
for r in rows:
    by.setdefault(r['query'], []).append(r)
for q, rs in by.items():
    top = [r for r in rs if not r.get('relevant')]
    rel = [r for r in rs if r.get('relevant')]
    print(f'## {q!r}')
    for label, r in [('top1', top[0]), ('top10', top[-1])] + [('target', x) for x in rel]:
        c = []
        contribs(r['explanation'], c)
        per = collections.defaultdict(float)
        for f, *_ in c:
            pass
        for f, t, v, n, dl, av in c:
            per[f] += v
        terms = '; '.join(f'{f}:{t} {v:.2f} (df {n:.0f}, dl {dl:.0f}/avg {av:.2f})' for f, t, v, n, dl, av in sorted(c, key=lambda x: -x[2])[:4])
        print(f'  {label:6s} rank {r["rank"]} score {r["score"]:.2f} | ' + ', '.join(f'{k} {v:.1f}' for k, v in sorted(per.items(), key=lambda x: -x[1])) + f' | top terms: {terms}')
