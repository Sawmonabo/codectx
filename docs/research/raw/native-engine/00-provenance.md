# Native-engine research, raw evidence — provenance

Date: 2026-09-16T16:28:53-04:00.  Host: Linux 6.18.33.2-microsoft-standard-WSL2 x86_64, 16 cores, 47 GiB RAM.

## The one download this research made
```
$ git clone --depth 1 --branch v4.0.627 https://github.com/joernio/joern.git <engine clone>
```
checkout:   <engine clone> (a shallow clone outside the product tree)
tag:        v4.0.627
HEAD sha:   4bb889d96ce972e2ded50d0d5765c514c1a032cf
HEAD subj:  [rust2cpg] bump astgen to 0.21.6. (#6285)
HEAD date:  2026-09-11T14:09:54+01:00
on-disk:    103M

## Stores read (sqlite3 -readonly only; never the codectx binary)
r3-1: <proof store of the first r3 run>/codectx.db  (4.2G, mtime 2026-09-16 15:58:03)
r3-3: <proof store of the third r3 run>/codectx.db  (4.3G, mtime 2026-09-16 16:07:31)

## Product tree (read-only)
product tree: the integration branch checkout
branch:    the integration branch
HEAD sha:  62976df61e027f2206c45952a7239142751e4625

## Counting discipline
Aggregates use  find ... -exec cat {} + | wc -l   (NOT  xargs wc -l | tail -1 , which
reports only the last xargs chunk total).  Itemized tables use explicit  wc -l <paths> .
