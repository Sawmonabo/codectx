set -u
for spec in java:java java:deep-java python:py python:deep-py javascript:ts javascript:deep-ts; do
  lang=${spec%%:*}; fx=${spec#*:}
  extra=""; [ "$lang" = java ] && extra="--build-mode=none"
  echo "== $(date '+%I:%M:%S %p') create $fx ($lang)"
  nice -n 10 <probe>/third_party/codeql/codeql/codeql database create <probe>/out/codeql/db-$fx --language=$lang $extra --source-root=<probe>/fixtures/$fx --overwrite --threads=2 --ram=2000 --common-caches=<probe>/third_party/codeql-cache > <probe>/out/codeql/create-$fx.log 2>&1; echo "exit=$?"; tail -2 <probe>/out/codeql/create-$fx.log
done
