#!/bin/bash
# Area C: rule audit (delta drift), delta arms, doc-collapse diagnosis, then our engine's delta (last: it changes <bench>/store).
LOG=<bench>/logs/r2/c-audit.log
source <bench>/logs/r2/lib.sh
Q=$B/queries.txt; QD=$B/qual; I=$B/idx; I2=$I/t16-large-end
U=treesitter/file:vendor/golang.org/x/sys/windows/zerrors_windows.go
# the post-delta documents: U's first document gets " deltachange1" (one more unicode61 token), as tdelta does
if [ ! -e $B/tmp/docs-postdelta.jsonl ]; then
python3 -I - "$B/docs.jsonl" "$B/tmp/docs-postdelta.jsonl" "$U" >> $LOG 2>&1 <<'PY'
import sys, json
src, dst, u = sys.argv[1:4]; done = False
with open(src) as f, open(dst, 'w') as g:
    for l in f:
        if not done and u in l:
            r = json.loads(l)
            if r['unit'] == u:
                r['body'] += ' deltachange1'; r['token_count'] += 1; done = True
                l = json.dumps(r, ensure_ascii=False) + '\n'
        g.write(l)
print('postdelta written, edited:', done)
PY
fi
DOCS=$B/tmp/docs-postdelta.jsonl ti fresh-postdelta $TN "-fresh-postdelta" 16 large end
# copies of t16-large-end: A plain delta, B delta + forced merge, C id route, D query route, E soft, F log:del=0.2
for c in A B C D E F; do [ -e $I/delta-$c ] || { cp -a $I2 $I/delta-$c; echo "copied t16-large-end -> delta-$c" >> $LOG; }; done
memok; run tdelta-A "-copyA" $TN/tdelta $B/docs.jsonl $I/delta-A $U
memok; run tdelta-B "-copyB" $TN/tdelta $B/docs.jsonl $I/delta-B $U --merge
memok; run tdelta-C "-copyC" $TN/tdelta $B/docs.jsonl $I/delta-C $U --delete id
memok; run tdelta-D "-copyD" $TN/tdelta $B/docs.jsonl $I/delta-D $U --delete query
memok; run tdelta-E "-copyE" $TN/tdelta $B/docs.jsonl $I/delta-E $U --soft $B/tmp/soft-E.tsv
memok; run tdelta-F "-copyF" $TN/tdelta $B/docs.jsonl $I/delta-F $U --merge-policy log:del=0.2
# pages: post-delta (A), post-delta + merge (B), fresh build of the post-delta documents
for ix in delta-A delta-B fresh-postdelta; do
  for m in library library-all product; do tq dd-$ix-speed $TN $I/$ix $Q 5 $m; done
  for m in library library-all; do for s in name doc subword; do tq dd-$ix-qual-$s $TN $I/$ix $QD/queries-$s.txt 1 $m; done; done
  tq dd-$ix-live-speed $TN $I/$ix $Q 5 library-all --stats live
  for s in name doc subword; do tq dd-$ix-live-qual-$s $TN $I/$ix $QD/queries-$s.txt 1 library-all --stats live; done
done
# soft delta: hidden old addresses on E vs the hard delete on A
for m in library library-all product; do tq dd-delta-E-soft-speed $TN $I/delta-E $Q 5 $m --hide $B/tmp/soft-E.tsv; done
# doc collapse (harness 09) on kubernetes
head -5 $QD/queries-doc.txt > $B/tmp/queries-doc5.txt
tq doc-collapse $TN $I2 $B/tmp/queries-doc5.txt 1 library --explain 10 --explain-qrels $QD/qrels-doc.tsv
python3 -I $B/research-raw/harness/source/smoke/diag.py $B/results/tantivy-library-doc-collapse-explain.jsonl > $B/logs/r2/doc-collapse-diag.txt 2>&1; echo "diag rc=$?" >> $LOG
# our engine's delta, last (it writes generation 2 into <bench>/store)
memok
echo "=== tbdelta $(date '+%H:%M:%S') -unit $U <bench>/docs.jsonl <bench>/store" >> $LOG
[ -e $B/results/ours-delta.jsonl ] && { echo "ours-delta.jsonl exists, refusing" >> $LOG; exit 8; }
xenv XDG_CONFIG_HOME=$B/xdg /usr/bin/time -v -o $B/logs/r2/tbdelta.time systemd-run --user --scope -q -p MemoryMax=12G \
  $B/bin/tbdelta -unit $U $B/docs.jsonl $B/store 2>&1 | clean >> $LOG
echo "rc=${PIPESTATUS[0]} $(date '+%H:%M:%S')" >> $LOG
rg 'Elapsed|Maximum resident' $B/logs/r2/tbdelta.time >> $LOG
echo ALLDONE >> $LOG
