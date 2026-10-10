"""Read call-like and type edges with all properties back from Memgraph.

Usage: python -I query_memgraph.py <host> <port> <project-name> <out.json>
"""

import json
import sys

import mgclient

QUERY = """
MATCH (a)-[r]->(b)
WHERE type(r) IN ['CALLS', 'REFERENCES', 'INSTANTIATES', 'INHERITS',
                  'OVERRIDES', 'IMPLEMENTS', 'ACCEPTS']
  AND (a.qualified_name STARTS WITH $prefix OR a.name = $project)
RETURN labels(a) AS from_labels, coalesce(a.qualified_name, a.name) AS from_qn,
       type(r) AS rel, properties(r) AS props,
       labels(b) AS to_labels, coalesce(b.qualified_name, b.name) AS to_qn
ORDER BY rel, from_qn, to_qn, props.line, props.col
"""


def main() -> None:
    host, port, project, dst = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
    conn = mgclient.connect(host=host, port=port)
    cur = conn.cursor()
    cur.execute(QUERY, {"prefix": project + ".", "project": project})
    cols = [c.name for c in cur.description]
    rows = [dict(zip(cols, row)) for row in cur.fetchall()]
    cur.execute("MATCH ()-[r]->() RETURN type(r) AS t, count(r) AS n ORDER BY t")
    counts = {t: n for t, n in cur.fetchall()}
    with open(dst, "w", encoding="utf-8") as f:
        json.dump({"query": QUERY, "rows": rows, "all_relationship_counts": counts},
                  f, indent=2, sort_keys=True, default=str)
    print("relationship counts:", json.dumps(counts, sort_keys=True))
    for r in rows:
        print(f"{r['rel']:<13} {r['from_qn']} -> {r['to_qn']}  "
              f"{json.dumps(r['props'], sort_keys=True)}")


if __name__ == "__main__":
    main()
