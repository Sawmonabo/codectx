# r2.py: tables for round 2 from results/*.jsonl, space/*.json and the round-2 logs.
import json, os, re, statistics, sys, collections
B = os.path.expanduser('<bench>')
R = os.path.join(B, 'results')

def load(fn):
    p = os.path.join(R, fn)
    if not os.path.exists(p): return None
    d = collections.OrderedDict()
    for l in open(p):
        r = json.loads(l); q = r['query']
        e = d.setdefault(q, {'runs': {}, 'hits': r.get('hits'), 'page': None, 'hide': []})
        e['runs'][r.get('run', 0)] = r['wall_ms']
        if r.get('hide_build_ms') is not None: e['hide'].append(r['hide_build_ms'])
        if r.get('page') is not None and e['page'] is None: e['page'] = r['page']
    out = collections.OrderedDict()
    for q, e in d.items():
        runs = e['runs']; warm = [v for k, v in runs.items() if k > 0]
        out[q] = dict(cold=runs.get(0), warm=statistics.median(warm) if warm else runs.get(0), hits=e['hits'], page=e['page'] or [], hide=e['hide'])
    return out

def summ(a):
    w = [v['warm'] for v in a.values()]; c = [v['cold'] for v in a.values() if v['cold'] is not None]
    return dict(n=len(a), sumw=sum(w), medw=statistics.median(w), sumc=sum(c))

def cmp(a, base):
    """hits equal, page identical, page equal as a set, mean top-200 overlap, mean top-10 overlap"""
    he = pe = se = 0; ov = []; ov10 = []; n = 0
    for q, x in a.items():
        y = base.get(q)
        if y is None: continue
        n += 1
        he += x['hits'] == y['hits']; pe += x['page'] == y['page']; se += set(x['page']) == set(y['page'])
        if y['page']: ov.append(len(set(x['page']) & set(y['page'])) / len(y['page']))
        if y['page'][:10]: ov10.append(len(set(x['page'][:10]) & set(y['page'][:10])) / len(y['page'][:10]))
    m = lambda v: sum(v) / len(v) if v else float('nan')
    return dict(n=n, hits=he, page=pe, set=se, ov=m(ov), ov10=m(ov10))

OURS = load(os.environ.get('OURS', 'ours-query-speed.jsonl'))
OURS_SUM = summ(OURS)['sumw'] if OURS else None
def row(label, fn, basefn, extra=''):
    a = load(fn)
    if a is None: return f'| {label} | (no result: {fn}) |||||||'
    b = load(basefn) if basefn else None
    s = summ(a)
    bs = summ(b)['sumw'] if b else None
    c = cmp(a, b) if b else None
    vs_base = f"{s['sumw']:.1f} vs {bs:.1f} ({(s['sumw']/bs-1)*100:+.0f}%)" if b else f"{s['sumw']:.1f}"
    vs_ours = f"{OURS_SUM/s['sumw']:.0f}x" if (OURS_SUM and s['n'] == 30) else ''
    same = f"{c['hits']}/{c['n']} hits, {c['page']}/{c['n']} pages, {c['set']}/{c['n']} sets, top-10 overlap {c['ov10']:.2f}" if c else ''
    return f"| {label} | {vs_base} | {s['medw']:.2f} | {s['sumc']:.1f} | {vs_ours} | {same} | {extra} |"

HDR = '| variant | sum of warm medians, ms (vs baseline) | median warm ms | sum cold ms | ours / variant (warm sum) | same as baseline | note |\n|---|---|---|---|---|---|---|'

def space(name):
    p = os.path.join(B, 'space', name + '.json')
    if not os.path.exists(p): return None
    return json.load(open(p))

def idx_lines(logfile):
    """name -> dict from the tindex summary line and time -v"""
    out = {}; cur = None
    for l in open(logfile):
        m = re.match(r'=== (?:tindex )?(\S+) \d', l)
        if m: cur = m.group(1); out[cur] = {}; continue
        m = re.match(r'tindex (\S+): (\d+) docs, (\d+) units, (\d+) commits, (\d+) segments, add ([\d.]+) ms, final ([\d.]+) ms, total ([\d.]+) ms, cpu ([\d.]+) ms, peak rss (\d+) B, disk (\d+) B, token_count mismatches (\d+)', l)
        if m and cur:
            out[cur].update(label=m.group(1), commits=int(m.group(4)), segs=int(m.group(5)), add=float(m.group(6)), final=float(m.group(7)), total=float(m.group(8)), cpu=float(m.group(9)), rss=int(m.group(10)), disk=int(m.group(11)), mism=int(m.group(12)))
        m = re.match(r'rc=(\d+)', l)
        if m and cur: out[cur]['rc'] = int(m.group(1))
        m = re.search(r'persist', l)
    return out

def mb(x): return f'{x/1e6:.1f}'
def index_table(names, base, logs=('logs/r2/a-index.log', 'logs/r2/c-audit.log')):
    info = {}
    for lf in logs:
        p = os.path.join(B, lf)
        if os.path.exists(p): info.update(idx_lines(p))
    old = idx_lines(os.path.join(B, 'logs/tindex.log'))
    for k, v in old.items(): info.setdefault(k, v)
    print('| index | args | wall s | CPU s | peak RSS GB | commits | segments | disk MB | termdict | postings | positions | fast | fieldnorms | store | vs baseline |')
    print('|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|')
    b = info.get(base, {})
    for n in names:
        i = info.get(n, {}); s = space(n)
        if not i or 'total' not in i:
            print(f'| {n} | rc={i.get("rc")} | failed |||||||||||||'); continue
        c = s['components'] if s else {}
        d = f"wall {(i['total']/b['total']-1)*100:+.0f}%, disk {(i['disk']/b['disk']-1)*100:+.1f}%" if b and n != base else 'baseline'
        print(f"| {n} | {i['label']} | {i['total']/1e3:.1f} | {i['cpu']/1e3:.1f} | {i['rss']/1e9:.2f} | {i['commits']} | {i['segs']} | {mb(i['disk'])} | "
              + ' | '.join(mb(c.get(k, 0)) for k in ['termdict', 'postings', 'positions', 'fast_fields', 'fieldnorms', 'store']) + f' | {d} |')

if __name__ == '__main__' and len(sys.argv) > 1 and sys.argv[1] == 'idx':
    index_table(sys.argv[3:], sys.argv[2])

if __name__ == '__main__' and len(sys.argv) > 1 and sys.argv[1] == 'speed':
    # speed <basefile> label=file ...
    print(HDR)
    for a in sys.argv[3:]:
        label, fn = a.split('=', 1)
        print(row(label, fn, sys.argv[2] if sys.argv[2] != '-' else None))

import subprocess
TEVAL = os.path.join(B, 'rust/target-next/release/teval')
def teval(s, files):
    files = [f if os.path.isabs(f) else os.path.join(R, f) for f in files]
    files = [f for f in files if os.path.exists(f)]
    out = subprocess.run([TEVAL, f'{B}/qual/qrels-{s}.tsv', f'{B}/qual/queries-{s}.txt'] + files, capture_output=True, text=True).stdout
    res = {}
    for l in out.splitlines():
        p = l.split()
        if len(p) == 8 and '/' in p[0]:
            try: res[p[0]] = dict(mrr=float(p[1]), s1=float(p[2]), s10=float(p[3]), r200=float(p[4]), hits=float(p[5]), zero=int(p[6]), missing=int(p[7]))
            except ValueError: pass
    return res

SETS = ['name', 'doc', 'subword', 'ident']
def qual_table(rows, ref_arm='tantivy-library', ref_fmt='{s}'):
    """rows: list of (label, arm, phase_fmt) where phase_fmt has {s}; prints MRR@10 / R@200 per set with deltas vs ours and vs the library baseline."""
    cache = {}
    def get(arm, phase, s):
        if (s) not in cache:
            fs = [f for f in os.listdir(R) if f.startswith('tantivy-') and f'qual-{s}' in f and not f.endswith(('-explain.jsonl', '-open.jsonl', '-cache.jsonl'))]
            cache[s] = teval(s, fs + [os.path.join(B, f'analysis/norm/ours-qual-{s}.jsonl')])
        return cache[s].get(f'{arm}/{phase}')
    print('| variant | ' + ' | '.join(f'{s} MRR@10 / R@200' for s in SETS) + ' | zero-hit queries (name/doc/subword/ident) |')
    print('|---|' + '---|' * (len(SETS) + 1))
    for label, arm, pf in rows:
        cells = []; zeros = []
        for s in SETS:
            r = get(arm, pf.format(s=s), s)
            o = get('ours', 'cold', s)
            b = get(ref_arm, ref_fmt.format(s=f'qual-{s}'), s)
            if r is None: cells.append('—'); zeros.append('—'); continue
            d = ''
            if o and arm != 'ours': d += f" (ours {r['mrr']-o['mrr']:+.3f})"
            if b and arm != 'ours' and not (arm == ref_arm and pf.format(s=s) == f'qual-{s}'): d += f" (lib {r['mrr']-b['mrr']:+.3f})"
            cells.append(f"{r['mrr']:.4f} / {r['r200']:.4f}{d}")
            zeros.append(str(r['zero']))
        print(f'| {label} | ' + ' | '.join(cells) + ' | ' + '/'.join(zeros) + ' |')

if __name__ == '__main__' and len(sys.argv) > 1 and sys.argv[1] == 'qual':
    rows = [tuple(a.split('=', 2)) for a in sys.argv[2:]]
    qual_table(rows)
