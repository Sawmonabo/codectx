#!/bin/bash
cd "$(dirname "$0")"; while ! grep -q "^DONE" big.txt 2>/dev/null; do sleep 20; done
export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:$HOME/.cargo/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=lang.txt; : > $LOG
pipe() { # name lang dir parsecap exportcap
  local n=$1 l=$2 d=$3 pc=$4 ec=$5; echo "### $n ($l) parse_cap=$pc export_cap=$ec" >> $LOG
  if [ "$pc" = default ]; then unset JAVA_OPTS; else export JAVA_OPTS=-Xmx$pc; fi
  /usr/bin/time -f "parse time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse "$d" --language $l --output $n.bin > $n.log 2> $n.err
  echo "skipped_methods=$(grep -c 'Skipping\.' $n.err) $(grep -m1 -io 'outofmemoryerror[^ ]*\|RuntimeException: [^\n]\{0,60\}' $n.err)" >> $LOG
  [ -f $n.bin ] || { echo "no cpg" >> $LOG; return; }; du -k $n.bin | awk '{print "cpg_kb="$1}' >> $LOG
  if [ "$ec" = default ]; then unset JAVA_OPTS; else export JAVA_OPTS=-Xmx$ec; fi; rm -rf $n.exp
  /usr/bin/time -f "export time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-export $n.bin --repr=all --format=neo4jcsv --out $n.exp > /dev/null 2> $n.exp.err
  grep -m1 -io 'outofmemoryerror' $n.exp.err >> $LOG; du -sk $n.exp | awk '{print "csv_kb="$1}' >> $LOG
  for e in CALL CDG REACHING_DEF; do echo "$e=$(wc -l < $n.exp/edges_${e}_data.csv 2>/dev/null)" >> $LOG; done; echo "METHOD=$(wc -l < $n.exp/nodes_METHOD_data.csv 2>/dev/null) FILE=$(wc -l < $n.exp/nodes_FILE_data.csv 2>/dev/null)" >> $LOG; rm -rf $n.exp
}
pipe git-default c ./git default default
pipe git-2g c ./git 2g 1g
pipe tokio-default rust ./tokio default default
pipe tokio-1g rust ./tokio 1g 512m
pipe both-default jssrc $HOME/repos/r3/app/both default default
pipe both-1g jssrc $HOME/repos/r3/app/both 1g 512m
pipe spring-default javasrc ./spring-framework default default
pipe spring-6g javasrc ./spring-framework 6g 2g
pipe postgres-default c ./postgres default default
pipe postgres-8g c ./postgres 8g 2g
echo DONE >> $LOG
