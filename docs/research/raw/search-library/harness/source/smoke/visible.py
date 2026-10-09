# visible.py <docs.jsonl> <pct> <seed> <out>: the documents whose unit library.rs unit_hidden keeps visible.
import json, sys
M = (1 << 64) - 1
def hidden(unit, pct, seed):
    h = 0xcbf29ce484222325 ^ ((seed * 0x9E3779B97F4A7C15) & M)
    for b in unit.encode():
        h ^= b
        h = (h * 0x100000001b3) & M
    h ^= h >> 33
    h = (h * 0xff51afd7ed558ccd) & M
    h ^= h >> 33
    return (h % 1000000) < pct * 10000.0
src, pct, seed, out = sys.argv[1], float(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
n = k = 0
with open(out, 'w') as o:
    for l in open(src):
        n += 1
        if not hidden(json.loads(l)['unit'], pct, seed):
            o.write(l); k += 1
print(f'{k} of {n} documents visible')
