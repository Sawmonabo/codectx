#!/bin/bash
cd "$(dirname "$0")"; while ! grep -q "^DONE" lang.txt 2>/dev/null; do sleep 20; done
export JAVA_HOME=$HOME/.local/opt/bompedia-jdk-21 PATH=$HOME/.local/opt/bompedia-jdk-21/bin:$PATH; J=$HOME/.local/opt/joern-cli; LOG=parity.txt; : > $LOG
stats() { # exportdir label
  python3 - "$1" "$2" <<'PY' >> parity.txt
import csv,sys,glob
d,label=sys.argv[1],sys.argv[2]
def load(k):
    hdr=open(f'{d}/{k}_header.csv').read().strip().split(','); ix={h.split(':')[0]:i for i,h in enumerate(hdr)}; return ix,list(csv.reader(open(f'{d}/{k}_data.csv')))
mix,methods=load('nodes_METHOD'); cix,calls=load('edges_CALL')
ext={r[0] for r in methods if r[mix['IS_EXTERNAL']]=='true'}; internal={r[0] for r in methods if r[mix['IS_EXTERNAL']]=='false'}
name={r[0]:r[mix['FULL_NAME']] for r in methods}
to_ext=sum(1 for e in calls if e[1] in ext); to_int=sum(1 for e in calls if e[1] in internal)
ops=sum(1 for e in calls if name.get(e[1],'').startswith('<operator>'))
cdg=sum(1 for _ in open(f'{d}/edges_CDG_data.csv')); rd=sum(1 for _ in open(f'{d}/edges_REACHING_DEF_data.csv'))
print(f"{label}: methods_internal={len(internal)} methods_external={len(ext)} CALL_to_internal={to_int} CALL_to_external={to_ext} (operators={ops}) CDG={cdg} REACHING_DEF={rd}")
PY
}
# TS: r3/app/both whole vs per subdir
echo "### TS parity r3/app/both" >> $LOG
JAVA_OPTS=-Xmx3g $J/joern-parse $HOME/repos/r3/app/both --language jssrc --output pb-whole.bin >/dev/null 2>&1; rm -rf pb-whole.exp; $J/joern-export pb-whole.bin --repr=all --format=neo4jcsv --out pb-whole.exp >/dev/null 2>&1; stats pb-whole.exp "whole"
for d in $HOME/repos/r3/app/both/*/; do n=$(basename $d); JAVA_OPTS=-Xmx3g $J/joern-parse $d --language jssrc --output pb-$n.bin >/dev/null 2>&1 || continue; rm -rf pb-$n.exp; $J/joern-export pb-$n.bin --repr=all --format=neo4jcsv --out pb-$n.exp >/dev/null 2>&1; stats pb-$n.exp "unit:$n"; rm -rf pb-$n.exp pb-$n.bin; done
rm -rf pb-whole.exp
# Python: redglass/packages whole vs per package
echo "### Python parity redglass/packages" >> $LOG
JAVA_OPTS=-Xmx4g $J/joern-parse $HOME/repos/redglass/packages --language pythonsrc --output pp-whole.bin >/dev/null 2>&1; rm -rf pp-whole.exp; JAVA_OPTS=-Xmx2g $J/joern-export pp-whole.bin --repr=all --format=neo4jcsv --out pp-whole.exp >/dev/null 2>&1; stats pp-whole.exp "whole"
for d in $HOME/repos/redglass/packages/*/; do n=$(basename $d); [ $(find $d -name '*.py' | wc -l) -gt 0 ] || continue; JAVA_OPTS=-Xmx3g $J/joern-parse $d --language pythonsrc --output pp-$n.bin >/dev/null 2>&1 || continue; rm -rf pp-$n.exp; JAVA_OPTS=-Xmx2g $J/joern-export pp-$n.bin --repr=all --format=neo4jcsv --out pp-$n.exp >/dev/null 2>&1; stats pp-$n.exp "unit:$n"; rm -rf pp-$n.exp pp-$n.bin; done
rm -rf pp-whole.exp
# max-num-def retry on m32 (2 skipped methods at default 4000)
echo "### max-num-def retry m32 (skipped=2 at 4000)" >> $LOG
JAVA_OPTS=-Xmx6g /usr/bin/time -f "retry time_wall=%e s largest_rss_kb=%M exit=%x" -a -o $LOG ./treemem.sh $LOG -- $J/joern-parse ./m32/src --language pythonsrc --output m32-retry.bin --max-num-def 40000 >/dev/null 2> m32-retry.err
echo "skipped_methods_after=$(grep -c 'Skipping\.' m32-retry.err)" >> $LOG; grep -B1 "Skipping" m32-4g.err | grep -o "[^ ]* has more than [0-9]* definitions" | head -3 >> $LOG
rm -rf m32-retry.exp; JAVA_OPTS=-Xmx3g $J/joern-export m32-retry.bin --repr=all --format=neo4jcsv --out m32-retry.exp >/dev/null 2>&1; echo "REACHING_DEF_after=$(wc -l < m32-retry.exp/edges_REACHING_DEF_data.csv)" >> $LOG; rm -rf m32-retry.exp m32-retry.bin
echo DONE >> $LOG
