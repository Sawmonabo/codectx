import sys,os,re,statistics
sys.path.insert(0,os.path.expanduser('<bench>/analysis'))
import r2
sk={}; cur=None
for l in open(r2.B+'/logs/r2/e-owner.log'):
    m=re.match(r'=== (\S+) ',l)
    if m: cur=m.group(1)
    m=re.search(r'product-topk "(.*)": blocks scored (\d+) of (\d+), documents scored (\d+)',l)
    if m: sk.setdefault(cur,{}).setdefault(m.group(1),(int(m.group(2)),int(m.group(3)),int(m.group(4))))
print('| set | queries | pages identical to product | product warm: median / sum ms | product-topk warm: median / sum ms | median per-query ratio | blocks skipped (all queries) | median per-query skip |')
print('|---|---|---|---|---|---|---|---|')
rows=[('speed 30 (5 warm runs)','tantivy-product-o2-prod-speed.jsonl','tantivy-product-topk-o2-topk-speed.jsonl','q-o2-topk-speed-product-topk')]
rows+=[(f'{s} 1000 (1 warm run)',f'tantivy-product-qual-{s}.jsonl',f'tantivy-product-topk-o2-topk-qual-{s}.jsonl',f'q-o2-topk-qual-{s}-product-topk') for s in ['name','doc','subword']]
for lab,pf,tf,key in rows:
    p=r2.load(pf); t=r2.load(tf)
    if t is None: print(f'| {lab} | not run |'); continue
    c=r2.cmp(t,p); qs=[q for q in t if q in p]
    rat=[t[q]['warm']/p[q]['warm'] for q in qs if p[q]['warm']>0]
    s=sk.get(key,{}); sc=sum(v[0] for v in s.values()); tot=sum(v[1] for v in s.values())
    per=[1-v[0]/v[1] for v in s.values() if v[1]]
    print(f"| {lab} | {c['n']} | {c['page']} | {statistics.median(p[q]['warm'] for q in qs):.2f} / {sum(p[q]['warm'] for q in qs):.0f} | {statistics.median(t[q]['warm'] for q in qs):.2f} / {sum(t[q]['warm'] for q in qs):.0f} | {statistics.median(rat):.2f}x | {1-sc/tot:.1%} of {tot} | {statistics.median(per):.1%} |")
for lab,a,b in [('product, 4 threads','tantivy-product-o2-prod-thr4-speed.jsonl','tantivy-product-o2-prod-speed.jsonl'),('product-topk, 4 threads','tantivy-product-topk-o2-topk-thr4-speed.jsonl','tantivy-product-topk-o2-topk-speed.jsonl')]:
    x=r2.load(a); y=r2.load(b)
    if x is None: print(lab,'not run'); continue
    sx=r2.summ(x); sy=r2.summ(y); c=r2.cmp(x,y)
    print(f"| {lab} | warm sum {sx['sumw']:.0f} ms vs {sy['sumw']:.0f} (1 thread) | median {sx['medw']:.2f} vs {sy['medw']:.2f} | pages {c['page']}/30 |")
