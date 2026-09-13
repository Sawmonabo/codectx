#!/bin/bash
# usage: treemem.sh <outfile> -- <command...>
# Runs the command; every 0.5 s sums RSS (kB) over the command's whole descendant tree; records peak sum, peak single, and process count at peak.
out=$1; shift; [ "$1" = "--" ] && shift
"$@" & pid=$!
peak=0; peaksingle=0; peakn=0
while kill -0 $pid 2>/dev/null; do
  pids="$pid $(pgrep -P $pid | tr '\n' ' ')"; for _ in 1 2 3 4 5; do more=""; for p in $pids; do more="$more $(pgrep -P $p | tr '\n' ' ')"; done; pids="$(echo $pids $more | tr ' ' '\n' | sort -u | tr '\n' ' ')"; done
  sum=0; single=0; n=0
  for p in $pids; do r=$(awk '/VmRSS/{print $2}' /proc/$p/status 2>/dev/null); [ -n "$r" ] || continue; sum=$((sum+r)); n=$((n+1)); [ $r -gt $single ] && single=$r; done
  if [ $sum -gt $peak ]; then peak=$sum; peaksingle=$single; peakn=$n; fi
  sleep 0.5
done
wait $pid; rc=$?
echo "tree_peak_sum_kb=$peak tree_peak_single_kb=$peaksingle procs_at_peak=$peakn exit=$rc" >> "$out"
exit $rc
