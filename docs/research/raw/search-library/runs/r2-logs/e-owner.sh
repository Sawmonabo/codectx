#!/bin/bash
# Owner ruling (Oct 8): three runs only.
LOG=<bench>/logs/r2/e-owner.log
source <bench>/logs/r2/lib.sh
T2=$B/rust/target-next2/release
Q=$B/queries.txt; QD=$B/qual; I=$B/idx; I2=$I/t16-large-end
# 1. product-code: the earlier code index has no token_count_code (older build), so it is rebuilt with the same args
ti code-next2 $T2 "-next2" 16 large end --tokenizer code
for s in name doc subword; do tq o1-code-qual-$s $T2 $I/code-next2 $QD/queries-$s.txt 1 product-code; done
# 2. product-topk: block-max side files, then the 30 queries and the three quality sets
echo "=== tblock t16-large-end $(date '+%H:%M:%S')" >> $LOG
xenv systemd-run --user --scope -q -p MemoryMax=12G $T2/tblock $I2 2>&1 | clean >> $LOG; echo "rc=${PIPESTATUS[0]} $(date '+%H:%M:%S')" >> $LOG
tq o2-topk-speed $T2 $I2 $Q 5 product-topk
tq o2-prod-speed $T2 $I2 $Q 5 product
for s in name doc subword; do tq o2-topk-qual-$s $T2 $I2 $QD/queries-$s.txt 1 product-topk; done
tq o2-topk-thr4-speed $T2 $I2 $Q 5 product-topk --search-threads 4
tq o2-prod-thr4-speed $T2 $I2 $Q 5 product --search-threads 4
echo ALLDONE >> $LOG
