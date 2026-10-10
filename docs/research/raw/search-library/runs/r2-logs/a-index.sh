#!/bin/bash
# Area A: every index variant not yet built, each followed by tspace.
LOG=<bench>/logs/r2/a-index.log
source <bench>/logs/r2/lib.sh
# threads and budgets (I1, I2), against t16-large-end / t1-default-end
ti t2-large-end   $TN "" 2 large end
ti t4-large-end   $TN "" 4 large end
ti t8-large-end   $TN "" 8 large end
ti t16-256-end    $TN "" 16 256 end
ti t16-1024-end   $TN "" 16 1024 end
# merge policies, merge threads, batched ops, cadence (I3-I6, I8), against t16-default-every1000
ti e1000-log-min4   $TN "" 16 default every:1000 --merge-policy log:min=4
ti e1000-log-min16  $TN "" 16 default every:1000 --merge-policy log:min=16
ti e1000-none       $TN "" 16 default every:1000 --merge-policy none
ti e1000-nonefinal  $TN "" 16 default every:1000 --merge-policy none+final
ti e1000-mt1        $TN "" 16 default every:1000 --merge-threads 1
ti e1000-mt8        $TN "" 16 default every:1000 --merge-threads 8
ti e1000-batchunit  $TN "" 16 default every:1000 --batch unit
ti t16-default-every100 $TN "" 16 default every:100
# single-segment writer, RAM then persist (I11, I12)
ti t1-large-end-single $TN "" 1 large end --single-segment
ti t16-large-end-ram   $TN "" 16 large end --ram-then-persist
# docstore and stored fields (F1, F2, F4, lean)
ti ds-none          $TN "" 16 large end --docstore none
ti ds-zstd3         $TN "" 16 large end --docstore zstd:3
ti ds-zstd9         $TN "" 16 large end --docstore zstd:9
ti ds-zstd3-bs64k   $TN "" 16 large end --docstore zstd:3 --blocksize 65536
ti ds-zstd3-bs256k  $TN "" 16 large end --docstore zstd:3 --blocksize 262144
ti store-none       $TN "" 16 large end --store none
ti lean             $TN "" 16 large end --schema lean
# fieldnorms, record options, fast-only kind, filter side fields (F5-F7, C5, C8)
ti fn-textonly      $TN "" 16 large end --fieldnorms text-only
ti fn-none          $TN "" 16 large end --fieldnorms none
ti body-freqs       $TN "" 16 large end --body-record freqs
ti kind-fast        $TN "" 16 large end --kind-fast-only
ti kindcode-facet   $TN "" 16 large end --kind-code --path-facet
# sstable term dictionary (F10): its own build, phase suffixed
ti sstable          $TS "-sstable" 16 large end
# tokenizers (T2, T7, T10) and side fields (QD, T4, S2, combined)
ti tok-default      $TN "" 16 large end --tokenizer default
ti tok-simple       $TN "" 16 large end --tokenizer simple
ti tok-enstem       $TN "" 16 large end --tokenizer en_stem
ti tok-pretok       $TN "" 16 large end --tokenizer pretokenized
ti parts            $TN "" 16 large end --parts-fields
ti ngram            $TN "" 16 large end --name-ngram
ti counts           $TN "" 16 large end --field-counts
ti combined         $TN "" 16 large end --combined
ti code-combined    $TN "" 16 large end --tokenizer code --combined
# tspace on the indexes built earlier
for n in t16-large-end t1-default-end t16-default-end t16-default-every1000 t16-large-end-code t1-default-unit; do
  [ -e $B/space/$n.json ] || { xenv systemd-run --user --scope -q -p MemoryMax=12G $TN/tspace $B/idx/$n > $B/space/$n.json 2>>$LOG; echo "tspace $n rc=$?" >> $LOG; }
done
echo ALLDONE >> $LOG
