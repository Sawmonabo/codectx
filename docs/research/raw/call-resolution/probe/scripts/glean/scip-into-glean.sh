set -u
G="glean --db-root /tmp/db --schema dir:/glean-demo/schema/source"
f() { grep -v -E '^[IW][0-9]{4} ' ; }
mkdir -p /tmp/db
echo "## index scip"; $G index --db scipfix/0 scip /src --input /out/scip/index.scip 2>&1 | f | tail -15
q() { echo; echo "## query: $1"; $G query --repo scipfix/0 --expand "$1" 2>&1 | f | cut -c1-400 | head -${2:-60}; }
echo "## predicates in image scip.angle"; grep -E '^ *predicate ' /glean-demo/schema/source/scip.angle | sed -E 's/^ *predicate ([A-Za-z]+).*/\1/' | tr '\n' ' '; echo
q 'scip.Reference _' 80
q 'scip.IsImplementation _'
q 'scip.ReferenceLocation _' 80
