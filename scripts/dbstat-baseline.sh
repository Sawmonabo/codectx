#!/usr/bin/env bash
# dbstat-baseline.sh — the one storage measurement every §3 lane re-runs.
#
# Prints, for a codectx SQLite store:
#   1. per-b-tree bytes  (SUM(pgsize) GROUP BY name), table+index, descending
#   2. the two §3e numbers: total store bytes and BYTES PER INDEXED SYMBOL
#      (bytes / node_facts rows) — the corpus-independent regression metric
#   3. row counts for the identity tables, so a later run can tell a real
#      shrink from a smaller corpus
#
# dbstat is a read-only virtual table that attributes every page of the file to
# the b-tree that owns it (https://www.sqlite.org/dbstat.html). The script only
# reads, but SQLite may still want to recover a hot journal, so ALWAYS point it
# at a COPY of a fixture store, never at the fixture itself.
#
# usage: scripts/dbstat-baseline.sh <path/to/codectx.db> [label]
set -euo pipefail

db=${1:?usage: dbstat-baseline.sh <codectx.db> [label]}
label=${2:-$(basename "$(dirname "$db")")}
[[ -r $db ]] || { echo "dbstat-baseline: cannot read $db" >&2; exit 2; }

q() { sqlite3 -readonly -noheader -batch "file:$db?immutable=1" "$1"; }

if ! q "SELECT 1 FROM dbstat LIMIT 1;" >/dev/null 2>&1; then
    echo "dbstat-baseline: this sqlite3 was built without SQLITE_ENABLE_DBSTAT_VTAB" >&2
    exit 3
fi

db_bytes=$(q "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size();")
wal_bytes=0; [[ -f ${db}-wal ]] && wal_bytes=$(stat -c%s "${db}-wal")
symbols=$(q "SELECT count(*) FROM node_facts;")

echo "== dbstat baseline: ${label}"
echo "-- file: ${db}"
echo "-- page_size=$(q 'PRAGMA page_size;') page_count=$(q 'PRAGMA page_count;') freelist=$(q 'PRAGMA freelist_count;') auto_vacuum=$(q 'PRAGMA auto_vacuum;')"
echo "-- db_bytes=${db_bytes} wal_bytes=${wal_bytes}"
echo
echo "-- per-b-tree bytes (SUM(pgsize) GROUP BY name)"
sqlite3 -header -column -batch "file:$db?immutable=1" \
    "SELECT name, SUM(pgsize) AS bytes, count(*) AS pages
       FROM dbstat GROUP BY name HAVING bytes > 0 ORDER BY bytes DESC;"
echo
echo "-- identity row counts"
sqlite3 -header -column -batch "file:$db?immutable=1" \
    "SELECT 'node_ids' t, count(*) n FROM node_ids
      UNION ALL SELECT 'node_facts', count(*) FROM node_facts
      UNION ALL SELECT 'relation_ids', count(*) FROM relation_ids
      UNION ALL SELECT 'relation_facts', count(*) FROM relation_facts
      UNION ALL SELECT 'native_aliases', count(*) FROM native_aliases
      UNION ALL SELECT 'evidence', count(*) FROM evidence
      UNION ALL SELECT 'fact_keys', count(*) FROM fact_keys
      UNION ALL SELECT 'search_units', count(*) FROM search_units
      UNION ALL SELECT 'files', count(*) FROM files
      UNION ALL SELECT 'units', count(*) FROM units;"
echo
if [[ $symbols -gt 0 ]]; then
    awk -v b="$((db_bytes + wal_bytes))" -v s="$symbols" \
        'BEGIN{printf "-- BYTES PER INDEXED SYMBOL = %d / %d = %.1f B/symbol\n", b, s, b/s}'
else
    echo "-- BYTES PER INDEXED SYMBOL = n/a (node_facts is empty)"
fi
echo "-- (ratio needs the eligible source bytes; S-VERIFY supplies them:"
echo "--  ratio = (db_bytes + wal_bytes + content-store bytes) / source bytes)"
