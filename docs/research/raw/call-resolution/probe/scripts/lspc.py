#!/usr/bin/env python3
"""Minimal LSP client for call-hierarchy probes.

Usage: python3 -I lspc.py SPEC.json OUTDIR

Speaks JSON-RPC over stdio, saves every raw frame (both directions) to
OUTDIR/transcript.jsonl, and writes a readable OUTDIR/summary.json.
"""
import json
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.parse
import queue


def uri_of(path):
    return "file://" + urllib.parse.quote(os.path.abspath(path))


def path_of(uri):
    return urllib.parse.unquote(urllib.parse.urlparse(uri).path)


class Client:
    def __init__(self, spec, outdir):
        self.spec = spec
        self.outdir = outdir
        self.root = os.path.abspath(spec["root"])
        os.makedirs(outdir, exist_ok=True)
        self.tlog = open(os.path.join(outdir, "transcript.jsonl"), "w")
        self.errlog = open(os.path.join(outdir, "stderr.log"), "wb")
        env = dict(os.environ)
        env.update(spec.get("env", {}))
        self.proc = subprocess.Popen(
            spec["cmd"], cwd=spec.get("cwd", self.root), stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=self.errlog, env=env, start_new_session=True)
        self.lock = threading.Lock()
        self.next_id = 1
        self.pending = {}
        self.notes = queue.Queue()
        self.all_notes = []
        self.reader = threading.Thread(target=self._read_loop, daemon=True)
        self.reader.start()

    def _log(self, direction, msg):
        rec = {"ts": time.time(), "dir": direction, "msg": msg}
        with self.lock:
            self.tlog.write(json.dumps(rec) + "\n")
            self.tlog.flush()

    def _send(self, msg):
        self._log("client->server", msg)
        body = json.dumps(msg).encode()
        data = b"Content-Length: %d\r\n\r\n" % len(body) + body
        self.proc.stdin.write(data)
        self.proc.stdin.flush()

    def _read_loop(self):
        f = self.proc.stdout
        while True:
            headers = {}
            line = f.readline()
            if not line:
                return
            while line and line.strip():
                k, _, v = line.decode().partition(":")
                headers[k.strip().lower()] = v.strip()
                line = f.readline()
            n = int(headers.get("content-length", 0))
            body = f.read(n)
            try:
                msg = json.loads(body)
            except Exception:
                continue
            self._log("server->client", msg)
            if "id" in msg and "method" in msg:
                self._answer(msg)
            elif "id" in msg:
                slot = self.pending.get(msg["id"])
                if slot is not None:
                    slot.put(msg)
            else:
                self.all_notes.append(msg)
                self.notes.put(msg)

    def _lookup(self, section):
        cur = self.spec.get("settings", {})
        if not section:
            return cur
        if section in cur:
            return cur[section]
        for part in section.split("."):
            if isinstance(cur, dict) and part in cur:
                cur = cur[part]
            else:
                return None
        return cur

    def _answer(self, msg):
        m = msg["method"]
        result = None
        if m == "workspace/configuration":
            result = [self._lookup(it.get("section")) for it in msg["params"].get("items", [])]
        elif m == "workspace/workspaceFolders":
            result = [{"uri": uri_of(self.root), "name": os.path.basename(self.root)}]
        elif m == "workspace/applyEdit":
            result = {"applied": False}
        self._send({"jsonrpc": "2.0", "id": msg["id"], "result": result})

    def request(self, method, params, timeout=120):
        rid = self.next_id
        self.next_id += 1
        slot = queue.Queue()
        self.pending[rid] = slot
        self._send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
        try:
            resp = slot.get(timeout=timeout)
        except queue.Empty:
            return {"timeout": True}
        finally:
            self.pending.pop(rid, None)
        return resp

    def notify(self, method, params):
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def wait_note(self, pred, timeout):
        deadline = time.time() + timeout
        for n in list(self.all_notes):
            if pred(n):
                return n
        while time.time() < deadline:
            try:
                n = self.notes.get(timeout=max(0.1, deadline - time.time()))
            except queue.Empty:
                break
            if pred(n):
                return n
        return None

    def close(self):
        try:
            self.request("shutdown", None, timeout=15)
            self.notify("exit", None)
        except Exception:
            pass
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            pass
        for sig in (signal.SIGTERM, signal.SIGKILL):
            try:
                os.killpg(self.proc.pid, sig)
            except ProcessLookupError:
                break
            time.sleep(2)
        self.tlog.close()
        self.errlog.close()


def position(root, q):
    text = open(os.path.join(root, q["file"])).read()
    idx = -1
    for _ in range(q.get("nth", 1)):
        idx = text.index(q["find"], idx + 1)
    idx += q.get("offset", 0)
    line = text.count("\n", 0, idx)
    col = idx - (text.rfind("\n", 0, idx) + 1)
    return {"line": line, "character": col}


def loc_str(uri, rng, root):
    p = path_of(uri)
    rel = os.path.relpath(p, root) if p.startswith(root) else p
    s = rng["start"]
    try:
        src = open(p).read().split("\n")[s["line"]].strip()
    except Exception:
        src = "?"
    return "%s:%d:%d  %s" % (rel, s["line"] + 1, s["character"] + 1, src)


def item_str(it, root):
    return {"name": it.get("name"), "detail": it.get("detail"), "kind": it.get("kind"),
            "at": loc_str(it["uri"], it["selectionRange"], root), "keys": sorted(it.keys())}


def run_queries(c, spec):
    root = c.root
    out = []
    for q in spec["queries"]:
        uri = uri_of(os.path.join(root, q["file"]))
        pos = position(root, q)
        rec = {"label": q["label"], "position": "%s:%d:%d" % (q["file"], pos["line"] + 1, pos["character"] + 1)}
        tdp = {"textDocument": {"uri": uri}, "position": pos}
        ops = q.get("ops", ["incoming", "outgoing", "references", "implementation"])
        if "incoming" in ops or "outgoing" in ops:
            r = c.request("textDocument/prepareCallHierarchy", tdp)
            items = r.get("result") or []
            rec["prepare"] = [item_str(i, root) for i in items] if "result" in r else r
            for op, method in (("incoming", "callHierarchy/incomingCalls"), ("outgoing", "callHierarchy/outgoingCalls")):
                if op not in ops:
                    continue
                res = []
                for it in items:
                    rr = c.request(method, {"item": it})
                    if "result" not in rr:
                        res.append({"error": rr.get("error", rr)})
                        continue
                    for call in rr["result"] or []:
                        other = call.get("from") or call.get("to")
                        # incoming: fromRanges lie in the caller (`from`); outgoing: in the queried item
                        site_uri = other["uri"] if op == "incoming" else it["uri"]
                        res.append({"other": item_str(other, root),
                                    "ranges": [loc_str(site_uri, fr, root) for fr in call.get("fromRanges", [])],
                                    "call_keys": sorted(call.keys())})
                rec[op] = res
        for op, method, extra in (("references", "textDocument/references", {"context": {"includeDeclaration": False}}),
                                  ("references_with_decl", "textDocument/references", {"context": {"includeDeclaration": True}}),
                                  ("implementation", "textDocument/implementation", {}),
                                  ("definition", "textDocument/definition", {})):
            if op not in ops and not (op == "references_with_decl" and "references" in ops):
                continue
            params = dict(tdp)
            params.update(extra)
            rr = c.request(method, params)
            if "result" not in rr:
                rec[op] = {"error": rr.get("error", rr)}
                continue
            locs = rr["result"] or []
            if isinstance(locs, dict):
                locs = [locs]
            norm = []
            for l in locs:
                if "targetUri" in l:
                    norm.append(loc_str(l["targetUri"], l["targetSelectionRange"], root))
                else:
                    norm.append(loc_str(l["uri"], l["range"], root))
            rec[op] = sorted(norm)
        out.append(rec)
    return out


def main():
    spec = json.load(open(sys.argv[1]))
    outdir = sys.argv[2]
    c = Client(spec, outdir)
    summary = {"server": spec["name"], "cmd": spec["cmd"], "root": c.root, "started": time.strftime("%Y-%m-%d %I:%M:%S %p %Z")}
    try:
        caps = {
            "textDocument": {
                "callHierarchy": {"dynamicRegistration": False},
                "references": {"dynamicRegistration": False},
                "implementation": {"dynamicRegistration": False, "linkSupport": True},
                "definition": {"linkSupport": True},
                "synchronization": {"didSave": True},
                "publishDiagnostics": {},
            },
            "workspace": {"configuration": True, "workspaceFolders": True},
            "window": {"workDoneProgress": True},
            "experimental": spec.get("experimental_caps", {}),
        }
        init = c.request("initialize", {
            "processId": os.getpid(), "rootUri": uri_of(c.root), "rootPath": c.root,
            "workspaceFolders": [{"uri": uri_of(c.root), "name": os.path.basename(c.root)}],
            "capabilities": caps, "initializationOptions": spec.get("init_options"),
            "clientInfo": {"name": "dispatch-probe", "version": "0.1"}}, timeout=spec.get("init_timeout", 180))
        summary["server_info"] = (init.get("result") or {}).get("serverInfo")
        summary["call_hierarchy_provider"] = (init.get("result") or {}).get("capabilities", {}).get("callHierarchyProvider")
        c.notify("initialized", {})
        if spec.get("settings"):
            c.notify("workspace/didChangeConfiguration", {"settings": spec["settings"]})
        for o in spec.get("open", []):
            p = os.path.join(c.root, o["path"])
            c.notify("textDocument/didOpen", {"textDocument": {
                "uri": uri_of(p), "languageId": o["languageId"], "version": 1, "text": open(p).read()}})
        ready = spec.get("ready", {})
        t0 = time.time()
        if ready.get("kind") == "ra-quiescent":
            n = c.wait_note(lambda n: n.get("method") == "experimental/serverStatus" and n["params"].get("quiescent"),
                            ready.get("timeout", 180))
            summary["ready"] = bool(n)
        elif ready.get("kind") == "jdtls":
            n = c.wait_note(lambda n: n.get("method") == "language/status" and n["params"].get("type") in ("ServiceReady", "Started"),
                            ready.get("timeout", 300))
            summary["ready"] = n["params"] if n else None
        summary["ready_wait_s"] = round(time.time() - t0, 1)
        time.sleep(ready.get("sleep", 2))
        passes = []
        prev = None
        for i in range(spec.get("max_passes", 5)):
            res = run_queries(c, spec)
            passes.append(res)
            if prev is not None and json.dumps(res, sort_keys=True) == json.dumps(prev, sort_keys=True):
                break
            prev = res
            time.sleep(spec.get("pass_sleep", 4))
        summary["passes_run"] = len(passes)
        summary["stable"] = len(passes) >= 2 and json.dumps(passes[-1], sort_keys=True) == json.dumps(passes[-2], sort_keys=True)
        summary["results"] = passes[-1]
        if len(passes) > 1 and json.dumps(passes[0], sort_keys=True) != json.dumps(passes[-1], sort_keys=True):
            summary["first_pass_results"] = passes[0]
    finally:
        c.close()
        summary["finished"] = time.strftime("%Y-%m-%d %I:%M:%S %p %Z")
        json.dump(summary, open(os.path.join(outdir, "summary.json"), "w"), indent=1)
        print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    main()
