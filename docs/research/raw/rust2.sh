#!/bin/bash
cd "$(dirname "$0")"; export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:$HOME/.cargo/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=rust2.txt; : > $LOG
git clone -q --depth 1 https://github.com/BurntSushi/ripgrep.git ripgrep 2>>$LOG
echo "### ripgrep LOC=$(find ripgrep -name '*.rs' | xargs cat | wc -l) crates=$(ls ripgrep/crates | tr '\n' ' ')" >> $LOG
for d in ripgrep ripgrep/crates/*/; do n=$(echo $d | tr '/' '_'); echo "## dir=$d" >> $LOG
  JAVA_OPTS=-Xmx2g /usr/bin/time -f "parse time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse $d --language rust --output rg-$n.bin > rg-$n.log 2> rg-$n.err
  grep -m1 -o "Process exited with code [0-9]*" rg-$n.err >> $LOG; grep -m1 "panicked at" rg-$n.err >> $LOG
  rm -rf rg-$n.exp; JAVA_OPTS=-Xmx1g $J/joern-export rg-$n.bin --repr=all --format=neo4jcsv --out rg-$n.exp >/dev/null 2>&1
  echo "cpg_kb=$(du -k rg-$n.bin | cut -f1) FILE=$(wc -l < rg-$n.exp/nodes_FILE_data.csv 2>/dev/null) METHOD=$(wc -l < rg-$n.exp/nodes_METHOD_data.csv 2>/dev/null) CALL=$(wc -l < rg-$n.exp/edges_CALL_data.csv 2>/dev/null) CDG=$(wc -l < rg-$n.exp/edges_CDG_data.csv 2>/dev/null) REACHING_DEF=$(wc -l < rg-$n.exp/edges_REACHING_DEF_data.csv 2>/dev/null)" >> $LOG; rm -rf rg-$n.exp rg-$n.bin
done
echo DONE >> $LOG
