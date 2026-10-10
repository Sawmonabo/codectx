"""Print raw request/response pairs from a transcript: raw.py TRANSCRIPT METHOD [ITEM_NAME] [ITEM_LINE0] [MAX]"""
import json, sys
recs = [json.loads(l) for l in open(sys.argv[1])]
method = sys.argv[2]
name = sys.argv[3] if len(sys.argv) > 3 else None
line0 = int(sys.argv[4]) if len(sys.argv) > 4 else None
mx = int(sys.argv[5]) if len(sys.argv) > 5 else 1
req = {}
for r in recs:
    m = r['msg']
    if r['dir'] == 'client->server' and m.get('method') == method:
        req[m['id']] = m
n = 0
for r in recs:
    m = r['msg']
    if r['dir'] == 'server->client' and 'method' not in m and m.get('id') in req:
        q = req[m['id']]['params']
        it = q.get('item') or {}
        pos = it.get('selectionRange', {}).get('start') or q.get('position')
        if name and it and it.get('name') != name:
            continue
        if line0 is not None and pos and pos['line'] != line0:
            continue
        print('REQUEST :', json.dumps(req[m['id']]))
        print('RESPONSE:', json.dumps(m))
        n += 1
        if n >= mx:
            break
