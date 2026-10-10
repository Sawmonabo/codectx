#!/bin/bash
# same.sh: today's product (target-next2) vs target-next3 with and without --dl total, plain and filtered.
cd "$(dirname "$0")/.."
O=../rust/target-next2/release/tquery; N=../rust/target-next3/release/tquery
rm -rf results-v3/old results-v3/new results-v3/newd
one() { TB_RESULTS=$PWD/results-v3/$1 nice -n 19 systemd-run --user --scope -q -p MemoryMax=4G "$2" "$3" q-all.txt 2 "$4" "${@:5}" > results-v3/last.log 2>&1 || { echo "FAILED $*"; tail -1 results-v3/last.log; }; }
for pair in "idx-t1-default-unit product" "v/idx-tok-code-unit product-code" "idx-delta product" "v3/cl product" "v3/cl-code product-code"; do
  set -- $pair
  for f in "" "--kind function --path-prefix internal"; do  # $f unquoted on purpose
    one old $O $1 $2 $f; one new $N $1 $2 $f; one newd $N $1 $2 $f --dl total
  done
done
