set -u
run() { db=$1; ql=$2; name=$3
  echo "== $(date '+%I:%M:%S %p') $name"
  nice -n 10 <probe>/third_party/codeql/codeql/codeql query run --database=<probe>/out/codeql/$db --common-caches=<probe>/third_party/codeql-cache --threads=2 --ram=2000 --output=<probe>/out/codeql/$name.bqrs <probe>/scripts/codeql/$ql > <probe>/out/codeql/$name.log 2>&1; echo "exit=$?"
  <probe>/third_party/codeql/codeql/codeql bqrs decode --format=csv --output=<probe>/out/codeql/$name.csv <probe>/out/codeql/$name.bqrs >> <probe>/out/codeql/$name.log 2>&1
}
run db-java java/targets.ql java-targets
run db-java java/callers.ql java-callers
run db-deep-java java/targets.ql deep-java-targets
run db-deep-java java/callers.ql deep-java-callers
run db-py python/targets.ql py-targets
run db-deep-py python/targets.ql deep-py-targets
run db-ts js/targets.ql ts-targets
run db-deep-ts js/targets.ql deep-ts-targets
