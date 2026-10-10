#!/bin/bash
# Area D: the product-code arm needs token_count_code, which the earlier code index (older build) lacks: rebuild it.
LOG=<bench>/logs/r2/d-code.log
source <bench>/logs/r2/lib.sh
Q=$B/queries.txt; QD=$B/qual; I=$B/idx
ti code-next $TN "-next" 16 large end --tokenizer code
tq v-codenext-speed $TN $I/code-next $Q 5 product-code
for s in name doc subword ident; do tq v-codenext-qual-$s $TN $I/code-next $QD/queries-$s.txt 1 product-code; done
for s in name doc subword ident; do tq v-codenext-qual-$s $TN $I/code-next $QD/queries-$s.txt 1 library; done
for m in library library-all product; do tq base-r2-speed $TN $I/t16-large-end $Q 5 $m; done
echo ALLDONE >> $LOG
