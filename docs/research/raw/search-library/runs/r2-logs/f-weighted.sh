#!/bin/bash
# Run 3 (owner): weighted document length under product scoring.
LOG=<bench>/logs/r2/f-weighted.log
source <bench>/logs/r2/lib.sh
T3=$B/rust/target-next3/release; QD=$B/qual; I=$B/idx
ti collen $T3 "-collen" 16 large end --column-lengths
ti collen-code $T3 "-collen" 16 large end --column-lengths --tokenizer code
q8() { local name=$1; shift; local tmp=$B/tmp/res-$name; mkdir -p $tmp
  echo "=== $name $(date '+%H:%M:%S') $*" | clean >> $LOG
  xenv TB_RESULTS=$tmp systemd-run --user --scope -q -p MemoryMax=8G "$@" 2>&1 | clean >> $LOG
  echo "rc=${PIPESTATUS[0]} $(date '+%H:%M:%S')" >> $LOG; settle $tmp ""; }
for s in name doc subword; do for dl in total weighted; do
  q8 w-$s-$dl-product $T3/tquery $I/collen $QD/queries-$s.txt 1 product --dl $dl --phase q-$s-$dl
  q8 w-$s-$dl-code $T3/tquery $I/collen-code $QD/queries-$s.txt 1 product-code --dl $dl --phase q-$s-$dl
done; done
echo ALLDONE >> $LOG
