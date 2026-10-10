#!/bin/bash
B=B
until grep -q ALLDONE $B/logs/r2/a-index.log; do sleep 30; done
$B/logs/r2/b-query.sh
$B/logs/r2/c-audit.sh
echo CHAINDONE >> $B/logs/r2/chain.log
