import json, sys
s = json.load(open(sys.argv[1]))
print({k: s[k] for k in s if k not in ('results', 'first_pass_results')})
for r in s['results']:
    print('==', r['label'], r['position'])
    for k, v in r.items():
        if k in ('label', 'position'):
            continue
        if k in ('incoming', 'outgoing') and isinstance(v, list):
            print('   ', k, ':', len(v))
            for c in v:
                if 'other' in c:
                    o = c['other']
                    print('       ', o['name'], '|', (o.get('detail') or '').replace('\n', ' '), '|', o['at'], '| sites:', c['ranges'], '| keys:', c['call_keys'], o['keys'])
                else:
                    print('       ', c)
        elif k == 'prepare':
            print('   ', k, ':', [(i['name'], i['detail'], i['kind'], i['at'], i['keys']) for i in v] if isinstance(v, list) else v)
        else:
            print('   ', k, ':', json.dumps(v))
