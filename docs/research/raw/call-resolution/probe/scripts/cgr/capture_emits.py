"""Run the real `cgr index` command while recording every relationship emit.

Usage: python -I capture_emits.py <repo-dir> <proto-out-dir> <emits.jsonl>
       [--trace-visits]

With --trace-visits, each CallProcessor._ingest_named_call visit is also
logged (caller, call name, site, receiver types, template parameters).

Wraps ProtobufFileIngestor.ensure_relationship_batch so each call is
appended, in emission order, to <emits.jsonl> with its properties and the
names of the code-graph-rag functions on the stack that emitted it, then
delegates to the real method so the stored index is the same artifact the
plain CLI writes.
"""

import json
import sys
import traceback

from codebase_rag.services import protobuf_service

_original = protobuf_service.ProtobufFileIngestor.ensure_relationship_batch
_seq = 0
_out = None


def _callers() -> list[str]:
    frames = traceback.extract_stack()[:-2]
    names = [f"{f.name}@{f.filename.rsplit('/', 1)[-1]}:{f.lineno}"
             for f in frames if "codebase_rag" in f.filename]
    return names


def _recording(self, from_spec, rel_type, to_spec, properties=None):
    global _seq
    _seq += 1
    _out.write(json.dumps({
        "seq": _seq,
        "type": str(rel_type),
        "from": [str(x) for x in from_spec],
        "to": [str(x) for x in to_spec],
        "properties": {k: (v if isinstance(v, (int, float, bool, str, type(None)))
                           else repr(v)) for k, v in (properties or {}).items()},
        "stack": _callers(),
    }, sort_keys=True) + "\n")
    return _original(self, from_spec, rel_type, to_spec, properties)


def _trace_call_visits() -> None:
    # Interleave one "visit" record per call-node visit with the emit
    # records, so the log shows which visit produced which emit.
    from codebase_rag.parsers import call_processor as cp

    original_visit = cp.CallProcessor._ingest_named_call

    def visit(self, ctx, call_node, call_name):
        global _seq
        _seq += 1
        _out.write(json.dumps({
            "seq": _seq,
            "visit": "_ingest_named_call",
            "caller_qn": ctx.caller_qn,
            "call_name": call_name,
            "start_byte": call_node.start_byte,
            "line": call_node.start_point[0] + 1,
            "col": call_node.start_point[1],
            "local_var_types": dict(ctx.local_var_types or {}),
            "cpp_template_params": sorted(ctx.cpp_template_params),
            "stack": _callers(),
        }, sort_keys=True) + "\n")
        return original_visit(self, ctx, call_node, call_name)

    cp.CallProcessor._ingest_named_call = visit

    original_func = cp.CallProcessor._ingest_func_node_calls

    def func_visit(self, func_node, func_name, *args, **kwargs):
        global _seq
        _seq += 1
        _out.write(json.dumps({
            "seq": _seq,
            "visit": "_ingest_func_node_calls",
            "func_name": func_name,
            "func_node_type": func_node.type,
            "func_node_line": func_node.start_point[0] + 1,
            "func_node_col": func_node.start_point[1],
            "func_node_parent_type": func_node.parent.type if func_node.parent else None,
        }, sort_keys=True) + "\n")
        return original_func(self, func_node, func_name, *args, **kwargs)

    cp.CallProcessor._ingest_func_node_calls = func_visit


def main() -> None:
    global _out
    repo, proto_dir, emits = sys.argv[1], sys.argv[2], sys.argv[3]
    protobuf_service.ProtobufFileIngestor.ensure_relationship_batch = _recording
    if "--trace-visits" in sys.argv[4:]:
        _trace_call_visits()
    from codebase_rag.cli import app

    with open(emits, "w", encoding="utf-8") as _out_file:
        _out = _out_file
        try:
            app(["index", "--repo-path", repo, "-o", proto_dir],
                standalone_mode=False)
        finally:
            _out_file.flush()
    print(f"recorded {_seq} relationship emits to {emits}")


if __name__ == "__main__":
    main()
