#!/usr/bin/env bash
# usage: fetch.sh OWNER/REPO SHA PATH...
# Saves each file under third_party/r1-sources/OWNER__REPO/PATH
set -euo pipefail
repo="$1"; sha="$2"; shift 2
base=<probe>/third_party/r1-sources/${repo//\//__}
for p in "$@"; do
  out="$base/$p"
  mkdir -p "$(dirname "$out")"
  if curl -fsSL "https://raw.githubusercontent.com/$repo/$sha/$p" -o "$out"; then
    echo "ok $(wc -l < "$out") $out"
  else
    echo "FAIL $repo $p"
  fi
done
