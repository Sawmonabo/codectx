#!/bin/bash
cd "$(dirname "$0")"
export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:$HOME/.cargo/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=extra.txt; : > $LOG
# 1. tokio: workspace root vs member crate
echo "### tokio-crate (rust) dir=tokio/tokio Xmx=2g" >> $LOG; head -3 tokio/Cargo.toml >> $LOG
JAVA_OPTS=-Xmx2g /usr/bin/time -f "parse time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse tokio/tokio --language rust --output tokio-crate.bin > tokio-crate.log 2> tokio-crate.err
du -k tokio-crate.bin | awk '{print "cpg_kb="$1}' >> $LOG
JAVA_OPTS=-Xmx1g /usr/bin/time -f "export time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-export tokio-crate.bin --repr=all --format=neo4jcsv --out tokio-crate.exp > /dev/null 2> tokio-crate.exp.err
du -sk tokio-crate.exp | awk '{print "csv_kb="$1}' >> $LOG
for e in CALL CDG REACHING_DEF; do echo "$e=$(wc -l < tokio-crate.exp/edges_${e}_data.csv 2>/dev/null)" >> $LOG; done; echo "METHOD=$(wc -l < tokio-crate.exp/nodes_METHOD_data.csv) FILE=$(wc -l < tokio-crate.exp/nodes_FILE_data.csv)" >> $LOG; rm -rf tokio-crate.exp
# 2. kubectl crash: deterministic? default heap
echo "### kubectl-retry (golang) Xmx=default" >> $LOG
unset JAVA_OPTS; /usr/bin/time -f "parse time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG $J/joern-parse k8s/staging/src/k8s.io/kubectl --language golang --output u-kubectl2.bin > /dev/null 2> u-kubectl2.err
grep -m1 -o "Pass [a-zA-Z.]* failed" u-kubectl2.err >> $LOG; grep -m1 "Caused by" u-kubectl2.err >> $LOG
# 3. scip-go on k8s in workspace mode
echo "### scip-go k8s (workspace mode)" >> $LOG
( cd k8s && /usr/bin/time -f "scip-go wall=%e s rss=%M KB exit=%x" -a -o ../$LOG ../../scip/scip-go --output ../k8s.scip > /dev/null 2> ../k8s.scip.err ); ls -la k8s.scip 2>/dev/null | awk '{print "scip_bytes="$5}' >> $LOG; tail -2 k8s.scip.err >> $LOG
echo DONE >> $LOG
