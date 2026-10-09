#!/bin/bash
# q <label> <index> <queries> <mode> [options]: one tquery run, 3 warm runs, results in results-v/q/<label>.
cd "$(dirname "$0")/.."
N=${TB_BIN:-../rust/target-next/release}
label=$1; idx=$2; qs=$3; mode=$4; shift 4
rm -rf results-v/q/$label
mkdir -p results-v/q
TB_RESULTS=$PWD/results-v/q/$label nice -n 19 systemd-run --user --scope -q -p MemoryMax=4G $N/tquery $idx $qs 3 $mode "$@" > results-v/q/$label.log 2>&1 || { mkdir -p results-v/q; echo "$label FAILED: $(grep -m1 -i error results-v/q/$label.log | cut -c1-200)"; }
