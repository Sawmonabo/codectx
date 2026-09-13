#!/bin/bash
cd "$(dirname "$0")"; while ! grep -q "^DONE" lang.txt 2>/dev/null; do sleep 15; done
export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=pgcap.txt; : > $LOG
for cap in 4g 2g; do echo "### postgres-$cap (c) parse_cap=$cap" >> $LOG
  JAVA_OPTS=-Xmx$cap /usr/bin/time -f "parse time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse ./postgres --language c --output pg-$cap.bin > pg-$cap.log 2> pg-$cap.err
  echo "skipped_methods=$(grep -c 'Skipping\.' pg-$cap.err) $(grep -m1 -o 'OutOfMemoryError[^ ]*' pg-$cap.err)" >> $LOG
  [ -f pg-$cap.bin ] && echo "cpg_kb=$(du -k pg-$cap.bin | cut -f1)" >> $LOG || echo "no cpg" >> $LOG
  if [ -f pg-$cap.bin ]; then rm -rf pg-$cap.exp; JAVA_OPTS=-Xmx1g /usr/bin/time -f "export(1g) time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-export pg-$cap.bin --repr=all --format=neo4jcsv --out pg-$cap.exp >/dev/null 2> pg-$cap.exp.err; grep -m1 -o 'OutOfMemoryError' pg-$cap.exp.err >> $LOG; echo "CALL=$(wc -l < pg-$cap.exp/edges_CALL_data.csv 2>/dev/null) CDG=$(wc -l < pg-$cap.exp/edges_CDG_data.csv 2>/dev/null) METHOD=$(wc -l < pg-$cap.exp/nodes_METHOD_data.csv 2>/dev/null)" >> $LOG; rm -rf pg-$cap.exp; fi
  rm -f pg-$cap.bin
done
echo DONE >> $LOG
