"""Decode a code-graph-rag protobuf index (index.bin) into JSON.

Usage: python -I decode_index.py <index.bin> <out.json>

Prints every relationship of the edge types of interest with all of its
properties, and writes every node and every relationship to <out.json>.
"""

import json
import sys

import codec.schema_pb2 as pb
from google.protobuf.json_format import MessageToDict

INTERESTING = {"CALLS", "REFERENCES", "INSTANTIATES", "INHERITS", "OVERRIDES",
               "IMPLEMENTS", "DEFINES_METHOD"}


def main() -> None:
    src, dst = sys.argv[1], sys.argv[2]
    index = pb.GraphCodeIndex()
    with open(src, "rb") as f:
        index.ParseFromString(f.read())

    nodes = []
    for node in index.nodes:
        kind = node.WhichOneof("payload")
        nodes.append({"kind": kind,
                      "fields": MessageToDict(getattr(node, kind),
                                              preserving_proto_field_name=True)})
    rels = []
    for rel in index.relationships:
        rels.append({
            "type": pb.Relationship.RelationshipType.Name(rel.type),
            "source_label": rel.source_label,
            "source_id": rel.source_id,
            "target_label": rel.target_label,
            "target_id": rel.target_id,
            "properties": MessageToDict(rel.properties),
        })
    with open(dst, "w", encoding="utf-8") as f:
        json.dump({"nodes": nodes, "relationships": rels}, f, indent=2,
                  sort_keys=True)

    print(f"nodes={len(nodes)} relationships={len(rels)}")
    for n in nodes:
        qn = n["fields"].get("qualified_name") or n["fields"].get("path") \
            or n["fields"].get("name")
        print(f"NODE {n['kind']:<10} {qn}")
    counts: dict[str, int] = {}
    for r in rels:
        counts[r["type"]] = counts.get(r["type"], 0) + 1
    print("relationship counts:", json.dumps(counts, sort_keys=True))
    for r in rels:
        if r["type"] in INTERESTING:
            props = json.dumps(r["properties"], sort_keys=True)
            print(f"{r['type']:<13} {r['source_label']}:{r['source_id']} -> "
                  f"{r['target_label']}:{r['target_id']}  {props}")


if __name__ == "__main__":
    main()
