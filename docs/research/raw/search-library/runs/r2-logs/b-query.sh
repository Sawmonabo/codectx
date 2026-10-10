#!/bin/bash
# Area B: query variants. Speed set (5 warm runs) for every variant; the four quality
# sets (1 run) where the variant changes ranking or tokenization.
LOG=<bench>/logs/r2/b-query.log
source <bench>/logs/r2/lib.sh
Q=$B/queries.txt; QD=$B/qual; I=$B/idx; I2=$I/t16-large-end; I11=$I/t16-default-end
KF="--kind function --kind method"; PF="--path-prefix pkg"
qual() { # qual <prefix> <bin> <index> <mode> [opts]: the four quality sets
  local p=$1 bd=$2 ix=$3 m=$4; shift 4
  for s in name doc subword ident; do tq $p-qual-$s $bd $ix $QD/queries-$s.txt 1 $m "$@"; done; }
# baseline on the ident set (name/doc/subword exist)
for m in library library-all product; do tq qual-ident $TN $I2 $QD/queries-ident.txt 1 $m; done
for m in library library-all; do tq qual-ident-code $TN $I/t16-large-end-code $QD/queries-ident.txt 1 $m; done
# --- index variants: library speed on each; extra modes where they matter
for n in t2-large-end t4-large-end t8-large-end t16-256-end t16-1024-end e1000-log-min4 e1000-log-min16 e1000-none \
         e1000-nonefinal e1000-mt1 e1000-mt8 e1000-batchunit t16-default-every100 t1-large-end-single t16-large-end-ram \
         ds-none ds-zstd3 ds-zstd9 ds-zstd3-bs64k ds-zstd3-bs256k store-none lean fn-textonly fn-none body-freqs \
         tok-default tok-simple tok-enstem tok-pretok parts ngram counts; do
  tq v-$n-speed $TN $I/$n $Q 5 library
done
tq v-t16-default-every1000-speed $TN $I/t16-default-every1000 $Q 5 library
tq v-t1-default-end-speed $TN $I/t1-default-end $Q 5 library
for n in lean body-freqs tok-pretok fn-none; do tq v-$n-speed $TN $I/$n $Q 5 product; done
for n in fn-textonly fn-none; do tq v-$n-speed $TN $I/$n $Q 5 library-all; done
tq v-kind-fast-speed-kind $TN $I/kind-fast $Q 5 library $KF
tq v-kind-fast-speed-kind $TN $I/kind-fast $Q 5 product $KF
tq v-sstable-speed $TS $I/sstable $Q 5 library
tq v-sstable-speed $TS $I/sstable $Q 5 library-all
tq v-sstable-speed $TS $I/sstable $Q 5 product
# ranking/tokenization index variants: quality
for n in fn-textonly fn-none body-freqs tok-default tok-simple tok-enstem tok-pretok parts ngram; do qual v-$n $TN $I/$n library; done
for n in fn-textonly fn-none; do qual v-$n $TN $I/$n library-all; done
qual v-counts-live $TN $I/counts library --stats live
tq v-counts-live-speed $TN $I/counts $Q 5 library --stats live
# product-code and library-combined (quality comparison)
tq v-code-speed $TN $I/t16-large-end-code $Q 5 product-code
qual v-code $TN $I/t16-large-end-code product-code
for ix in combined code-combined; do
  tq v-$ix-comb-speed $TN $I/$ix $Q 5 library-combined --shape combined
  tq v-$ix-combf-speed $TN $I/$ix $Q 5 library-combined --shape combined+fields
  qual v-$ix-comb $TN $I/$ix library-combined --shape combined
  qual v-$ix-combf $TN $I/$ix library-combined --shape combined+fields
done
# --- query variants on t16-large-end
for s in flat dismax:0.1 dismax:0.3 msm fuzzy phrase-prefix:50 parser; do
  t=${s//:/}; tq s-$t-speed $TN $I2 $Q 5 library --shape $s; qual s-$t $TN $I2 library --shape $s
done
for n in 1 2; do tq slop$n-speed $TN $I2 $Q 5 library --slop $n; tq slop$n-qual-ident $TN $I2 $QD/queries-ident.txt 1 library --slop $n; done
tq w11111-speed $TN $I2 $Q 5 library --weights 1/1/1/1/1; qual w11111 $TN $I2 library --weights 1/1/1/1/1
for st in live live-union; do
  for m in library library-all; do tq stats-$st-speed $TN $I2 $Q 5 $m --stats $st; qual stats-$st $TN $I2 $m --stats $st; done
done
# filter routes on the kind-code + path-facet index (baselines terms / regex on the same index)
IF=$I/kindcode-facet
for k in terms termset bytes collector code; do tq kf-$k $TN $IF $Q 5 library $KF --kind-filter $k; done
for p in regex range ffrange invrange facet collector; do tq pf-$p $TN $IF $Q 5 library $PF --path-filter $p; done
tq kf-terms-all $TN $IF $Q 5 library-all $KF
tq kf-product $TN $IF $Q 5 product $KF
# collectors, id source, store cache
for c in tuple-count tuple-id tweak; do tq col-$c $TN $I2 $Q 5 library --collect $c; done
tq idfast $TN $I2 $Q 5 library --id-source fast
for n in 0 100 1000; do tq cache$n $TN $I2 $Q 5 library --doc-cache $n; done
# search threads on the 11-segment index
for t in 2 4 8; do for m in library library-all; do tq thr$t-11seg $TN $I11 $Q 5 $m --search-threads $t; done; done
# open time, madvise, warmer
tq open $TN $I2 $Q 5 library --time-open
for a in normal random sequential willneed; do tq madv-$a $TN $I2 $Q 5 library --madvise $a --time-open; done
tq open $TN $I2 $Q 5 product --time-open
tq warmstats $TN $I2 $Q 5 product --warm-stats --time-open
# count, docset, aggregations, facets
for v in query collector; do tq count-$v $TN $I2 $Q 5 count --count-via $v; done
for v in docset vec; do tq docset-$v $TN $I2 $Q 5 docset --docset-via $v; done
for v in tuple separate custom; do tq agg-$v $TN $I2 $Q 5 agg-kind --agg $v; done
for v in tuple separate; do tq facets-$v $TN $IF $Q 5 facets --facets $v; done
# product-block
tq pblock-speed $TN $I2 $Q 5 product-block
tq pblock-speed-kind $TN $I2 $Q 5 product-block $KF
# visibility: 0/10/50 % of units via weight, collector, alive-set
for pct in 0 10 50; do for via in weight collector alive-set; do
  for m in library library-all; do tq hide$pct-$via $TN $I2 $Q 5 $m --hide-units $pct --hide-seed 7 --hide-via $via; done
done; done
for pct in 0 10 50; do for via in collector alive-set; do tq hide$pct-$via $TN $I2 $Q 5 product --hide-units $pct --hide-seed 7 --hide-via $via; done; done
# f32 / tie stability: the quality sets on the 11-segment index
for m in library library-all; do for s in name doc subword; do tq qual-$s-11seg $TN $I11 $QD/queries-$s.txt 1 $m; done; done
# ttopk per shape, 2 and 11 segments
for s in flat dismax:0.0 dismax:0.1 msm fuzzy parser; do t=${s//:/}
  run topk-$t-2seg "" $TN/ttopk $I2 $Q --shape $s
  run topk-$t-11seg "" $TN/ttopk $I11 $Q --shape $s
done
run topk-flat-11seg-name "" $TN/ttopk $I11 $QD/queries-name.txt --shape flat
echo ALLDONE >> $LOG
