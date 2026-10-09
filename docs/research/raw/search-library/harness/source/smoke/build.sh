#!/bin/bash
# Builds every index variant of the fixture into v/idx-<name>, with its tindex line and tspace totals.
set -e
cd "$(dirname "$0")/.."
N=../rust/target-next/release
R="nice -n 19 systemd-run --user --scope -q -p MemoryMax=4G"
export TB_RESULTS=$PWD/results-v/build
b() { name=$1; shift; rm -rf v/idx-$name; echo "== $name: $*"; $R $N/tindex docs.jsonl v/idx-$name "$@" 2>&1 | tail -1; $R $N/tspace v/idx-$name > v/space-$name.json; }
b base-unit 1 default unit
b base 1 default end
for t in code default simple en_stem pretokenized; do b tok-$t 1 default end --tokenizer $t; done
b nostore 1 default end --store none
b fn-textonly 1 default end --fieldnorms text-only
b fn-none 1 default end --fieldnorms none
b bodyfreqs 1 default end --body-record freqs
b lean 1 default end --schema lean
b kindfast 1 default end --kind-fast-only
b facet 1 default end --path-facet
b kindcode 1 default end --kind-code
b parts 1 default end --parts-fields
b ngram 1 default end --name-ngram
b counts 1 default end --field-counts
b ds-none 1 default end --docstore none
b ds-zstd3 1 default end --docstore zstd:3
b ds-zstd9 1 default end --docstore zstd:9
b ds-zstd3-64k 1 default end --docstore zstd:3 --blocksize 65536
b ds-zstd3-256k 1 default end --docstore zstd:3 --blocksize 262144
b mp-nonefinal 1 default unit --merge-policy none+final
b mp-min4 1 default unit --merge-policy log:min=4
b mp-min16 1 default unit --merge-policy log:min=16
b mt1 1 default unit --merge-threads 1
b batchunit 1 default unit --batch unit
b every100 1 default every:100
b single 1 default end --single-segment
b ram 1 default end --ram-then-persist
for t in 2 4 8 16; do b t$t $t default end; done
b budget15 1 15 end
b budget256 1 256 end
