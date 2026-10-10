#!/bin/bash
# Round-2 measurement helpers. Every run: clean env, capped scope, its own results dir,
# then the files move into results/ (refusing to overwrite), phases optionally suffixed.
B=<bench>; cd $B || exit 9
TN=$B/rust/target-next/release; TS=$B/rust/target-sstable/release
LOG=${LOG:-$B/logs/r2/round2.log}
clean() { sed "s#$B/#<bench>/#g; s#$B#<bench>#g; s#$HOME#~#g"; }
xenv() { env -i HOME=$HOME PATH=/usr/local/bin:/usr/bin:/bin USER=$USER XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR \
  DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS TMPDIR=$B/tmp "$@"; }
memok() { local a; a=$(free -g | awk '/Mem:/{print $7}'); echo "free -g available ${a}G $(date '+%H:%M:%S')" >> $LOG
  if [ "$a" -lt 8 ]; then echo "STOP: available memory ${a}G < 8G" | tee -a $LOG; exit 7; fi; }
# settle <tmpdir> <suffix>: move result files into results/, appending suffix to file and phase.
settle() { python3 -I - "$1" "$2" "$B/results" >> $LOG 2>&1 <<'PY'
import sys, os, json
src, suf, dst = sys.argv[1], sys.argv[2], sys.argv[3]
for fn in sorted(os.listdir(src)):
    if not fn.endswith('.jsonl'): continue
    base = fn[:-6]; out = os.path.join(dst, base + suf + '.jsonl')
    if os.path.exists(out): print('SETTLE-COLLISION', out); sys.exit(3)
    with open(os.path.join(src, fn)) as f, open(out, 'w') as g:
        for l in f:
            if suf:
                r = json.loads(l)
                if 'phase' in r: r['phase'] += suf
                l = json.dumps(r) + '\n'
            g.write(l)
    print('settled', os.path.basename(out))
PY
}
# run <name> <suffix> <cmd...>: one capped measurement with its own results dir.
run() { local name=$1 suf=$2; shift 2; local tmp=$B/tmp/res-$name
  if [ -e "$tmp" ]; then echo "SKIP-EXISTS $name" | tee -a $LOG; return 0; fi
  mkdir -p $tmp
  echo "=== $name $(date '+%H:%M:%S') $*" | clean >> $LOG
  xenv TB_RESULTS=$tmp systemd-run --user --scope -q -p MemoryMax=12G "$@" 2>&1 | clean >> $LOG
  local rc=${PIPESTATUS[0]}
  echo "rc=$rc $(date '+%H:%M:%S')" >> $LOG
  settle $tmp "$suf"
  return 0
}
# tq <name/phase> <bin dir> <index> <queries> <runs> <mode> [opts]
tq() { local ph=$1 bd=$2; shift 2; run q-$ph-$4 "" $bd/tquery "$@" --phase $ph; }
# ti <idxname> <bin dir> <suffix> <threads> <budget> <cadence> [opts]: index build, time -v, tspace
ti() { local name=$1 bd=$2 suf=$3; shift 3
  if [ -e $B/idx/$name ]; then echo "SKIP-IDX-EXISTS $name" | tee -a $LOG; return 0; fi
  memok
  local tmp=$B/tmp/res-i-$name; mkdir -p $tmp $B/idx
  echo "=== tindex $name $(date '+%H:%M:%S') args: $*" | clean >> $LOG
  xenv TB_RESULTS=$tmp /usr/bin/time -v -o $B/logs/r2/tindex-$name.time systemd-run --user --scope -q -p MemoryMax=12G \
     $bd/tindex ${DOCS:-$B/docs.jsonl} $B/idx/$name "$@" 2>&1 | clean >> $LOG
  local rc=${PIPESTATUS[0]}
  echo "rc=$rc $(date '+%H:%M:%S') du=$(du -sb $B/idx/$name 2>/dev/null | cut -f1)" >> $LOG
  rg 'Elapsed|Maximum resident' $B/logs/r2/tindex-$name.time >> $LOG
  settle $tmp "$suf"
  xenv systemd-run --user --scope -q -p MemoryMax=12G $bd/tspace $B/idx/$name > $B/space/$name.json 2>>$LOG; echo "tspace $name rc=$?" >> $LOG
}
