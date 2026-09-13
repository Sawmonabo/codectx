#!/bin/bash
cd "$(dirname "$0")"; export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:/usr/local/go/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=big.txt; : > $LOG
run() { local n=$1 l=$2 d=$3 x=$4; echo "### $n ($l) Xmx=${x:-default}" >> $LOG
  if [ -n "$x" ]; then export JAVA_OPTS="-Xmx$x"; else unset JAVA_OPTS; fi
  /usr/bin/time -f "parse wall=%e s rss=%M KB exit=%x" $J/joern-parse "$d" --language $l --output $n.bin > $n.log 2> $n.err
  tail -1 $n.err >> $LOG; echo "skipped_methods=$(grep -c 'Skipping\.' $n.err)" >> $LOG; grep -m1 -i "outofmemory\|RuntimeException" $n.err | cut -c1-120 >> $LOG
  [ -f $n.bin ] || { echo "no cpg" >> $LOG; return; }; du -k $n.bin | awk '{print "cpg_kb="$1}' >> $LOG
  rm -rf $n.exp; /usr/bin/time -f "export wall=%e s rss=%M KB exit=%x" $J/joern-export $n.bin --repr=all --format=neo4jcsv --out $n.exp > /dev/null 2> $n.exp.err; tail -1 $n.exp.err >> $LOG
  du -sk $n.exp | awk '{print "csv_kb="$1}' >> $LOG; for e in CALL CDG REACHING_DEF; do echo "$e=$(wc -l < $n.exp/edges_${e}_data.csv 2>/dev/null)" >> $LOG; done; echo "METHOD=$(wc -l < $n.exp/nodes_METHOD_data.csv 2>/dev/null) FILE=$(wc -l < $n.exp/nodes_FILE_data.csv 2>/dev/null)" >> $LOG; rm -rf $n.exp
}
run m32-default pythonsrc ./m32/src ""
run m32-4g pythonsrc ./m32/src 4g
# kubernetes: whole module (vendor excluded via frontend? gosrc2cpg has no exclude flag documented; run whole repo minus vendor by removing vendor dir)
rm -rf k8s/vendor
run k8s-default golang ./k8s ""
run k8s-8g golang ./k8s 8g
# per-unit: k8s staging modules are separate go.mod modules; time the largest few individually under a 4g cap
for m in $(ls -d k8s/staging/src/k8s.io/* | head -40); do n=$(basename $m); loc=$(find $m -name '*.go' | xargs cat 2>/dev/null | wc -l); [ "$loc" -lt 100000 ] && continue; echo "### unit $n LOC=$loc Xmx=4g" >> $LOG; JAVA_OPTS=-Xmx4g /usr/bin/time -f "parse wall=%e s rss=%M KB exit=%x" $J/joern-parse $m --language golang --output u-$n.bin >/dev/null 2> u-$n.err; tail -1 u-$n.err >> $LOG; rm -f u-$n.bin; done
echo "### scip-go k8s" >> $LOG; cd k8s && GOFLAGS=-mod=mod /usr/bin/time -f "scip-go wall=%e s rss=%M KB exit=%x" ../../scip/scip-go --output ../k8s.scip > /dev/null 2> ../k8s.scip.err; tail -1 ../k8s.scip.err >> ../$LOG; ls -la ../k8s.scip 2>/dev/null | awk '{print "scip_bytes="$5}' >> ../$LOG; cd ..
echo DONE >> $LOG
