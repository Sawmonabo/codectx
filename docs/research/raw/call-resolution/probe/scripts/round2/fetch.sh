#!/usr/bin/env bash
# usage: fetch.sh OWNER/REPO SHA PATH  -> writes to third_party/r2-sources/OWNER__REPO/SHA-short/PATH
set -euo pipefail
repo=$1; sha=$2; path=$3
base=<probe>/third_party/r2-sources
out="$base/${repo//\//__}/${sha:0:10}/$path"
mkdir -p "$(dirname "$out")"
curl -fsSL "https://raw.githubusercontent.com/$repo/$sha/$path" -o "$out"
echo "$out $(wc -l < "$out") lines"
