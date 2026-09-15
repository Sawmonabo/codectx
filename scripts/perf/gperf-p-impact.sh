#!/usr/bin/env bash
# GPERF-P: profiling harness for the impact walk on the r3 certification store.
#
# Read-only. Runs `impact <hub> --depth 0 --edges 0 --visited N` against an
# ISOLATED copy of the store (never the certification store itself, never a
# re-index) with a CPU + heap profile per run, and records wall clock, the
# counts the answer reports, peak RSS and the bytes the run spooled.
#
# Host safety: every run is capped (ulimit -v, GOMEMLIMIT, GOMAXPROCS) because
# an uncapped walk on this store can take the whole VM down with it.
set -u

G=${GPERF_ROOT:-$HOME/.cache/codectx-gperf}
HUB=${HUB:-f45cf2a87b577955a189c1c4a4f252581ae66dd23a3635d3b9ff292fe692e33c}
REPO=${REPO:-/home/sabossedgh/repos/r3}
BIN=$G/codectx
N=$1

PROF=$G/prof/n$N
OUT=$G/out/n$N
rm -rf "$PROF"; mkdir -p "$PROF" "$G/out"

before=$(du -sb "$G/store/work" 2>/dev/null | cut -f1)
before=${before:-0}

# /usr/bin/time gives the peak RSS of the process tree; the shell builtin does not.
( ulimit -v 6000000
  GOMEMLIMIT=3GiB GOMAXPROCS=4 \
  XDG_CACHE_HOME=$G/xdg/cache XDG_CONFIG_HOME=$G/xdg/config \
  XDG_DATA_HOME=$G/xdg/data XDG_STATE_HOME=$G/xdg/state \
  TMPDIR=$G/tmp CODECTX_PPROF_DIR=$PROF \
  /usr/bin/time -v -o "$OUT.time" \
    "$BIN" impact "$HUB" --repo "$REPO" --depth 0 --edges 0 --visited "$N" \
      --limit "${LIMIT:-0}" --json > "$OUT.json" 2> "$OUT.err" )
rc=$?

after=$(du -sb "$G/store/work" 2>/dev/null | cut -f1)
after=${after:-0}

echo "=== N=$N rc=$rc ==="
grep -E "Elapsed \(wall|Maximum resident" "$OUT.time" 2>/dev/null
echo "work-dir delta bytes: $(( after - before ))"
python3 - "$OUT.json" <<'PY'
import json,sys
try: d=json.load(open(sys.argv[1]))
except Exception as e: print("json:",e); sys.exit()
def walk(o,p=""):
    if isinstance(o,dict):
        for k,v in o.items():
            if k in ("visited_count","edge_count","truncated","truncation_reason","next_cursor","depth_reached","item_count") or (isinstance(v,(int,str,bool)) and ("count" in k or "truncat" in k)):
                print(f"  {p}{k} = {v if not (isinstance(v,str) and len(v)>40) else v[:40]+'...'}")
            walk(v,p+k+".")
    elif isinstance(o,list):
        print(f"  {p}[] len = {len(o)}")
        if o: walk(o[0],p+"0.")
PY
ls -la "$PROF" 2>/dev/null | tail -n +2
