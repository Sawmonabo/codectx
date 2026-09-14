#!/bin/bash
cd "$(dirname "$0")"; export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=retry.txt; : > $LOG
SRC=./m32/src
echo "### m32rimm uncapped, --max-num-def 4000 (engine default) LOC=$(find $SRC -name '*.py' | xargs cat | wc -l)" >> $LOG
unset JAVA_OPTS; /usr/bin/time -f "parse time_wall=%e s user=%U s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse $SRC --language pythonsrc --output m32-base.bin >/dev/null 2> m32-base.err
echo "skipped_methods=$(grep -c 'Skipping\.' m32-base.err) cpg_kb=$(du -k m32-base.bin | cut -f1)" >> $LOG
echo "### m32rimm uncapped, --max-num-def 40000" >> $LOG
/usr/bin/time -f "parse time_wall=%e s user=%U s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse $SRC --language pythonsrc --output m32-40k.bin --max-num-def 40000 >/dev/null 2> m32-40k.err
echo "skipped_methods=$(grep -c 'Skipping\.' m32-40k.err) $(grep -m1 -o 'OutOfMemoryError[^ ]*' m32-40k.err) cpg_kb=$(du -k m32-40k.bin 2>/dev/null | cut -f1)" >> $LOG
for n in m32-base m32-40k; do [ -f $n.bin ] || continue; rm -rf $n.exp; /usr/bin/time -f "export $n time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-export $n.bin --repr=all --format=neo4jcsv --out $n.exp >/dev/null 2>&1; echo "$n REACHING_DEF=$(wc -l < $n.exp/edges_REACHING_DEF_data.csv) CDG=$(wc -l < $n.exp/edges_CDG_data.csv) CALL=$(wc -l < $n.exp/edges_CALL_data.csv) METHOD=$(wc -l < $n.exp/nodes_METHOD_data.csv) csv_kb=$(du -sk $n.exp | cut -f1)" >> $LOG; done
grep -B1 "Skipping" m32-base.err | grep -o "[^ ]* has more than [0-9]* definitions" | sort -u >> $LOG
echo DONE >> $LOG
