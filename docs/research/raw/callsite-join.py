import json,sys,subprocess,re
from tree_sitter import Language, Parser, Query, QueryCursor
import tree_sitter_go, tree_sitter_python, tree_sitter_typescript, tree_sitter_java, tree_sitter_rust, tree_sitter_c
Q='/home/sabossedgh/dev/codectx/internal/provider/treesitter/lang/queries/'
SCIP='/tmp/claude-1000/-home-sabossedgh-dev-codectx/b0d7dd67-07aa-4b1e-90f3-b5373301a26b/scratchpad/scip'
langs={
 'go':      (Language(tree_sitter_go.language()), 'go.scm', f'{SCIP}/mod/a.go', f'{SCIP}/mod/index.scip','a.go'),
 'python':  (Language(tree_sitter_python.language()), 'python.scm', f'{SCIP}/pymod/a.py', f'{SCIP}/pymod/index.scip','a.py'),
 'typescript':(Language(tree_sitter_typescript.language_typescript()), 'ecmascript.scm', f'{SCIP}/tsmod/a.ts', f'{SCIP}/tsmod/index.scip','a.ts'),
 'java':    (Language(tree_sitter_java.language()), 'java.scm', f'{SCIP}/jmod/src/main/java/m/A.java', f'{SCIP}/jmod/index.scip','A.java'),
 'rust':    (Language(tree_sitter_rust.language()), 'rust.scm', f'{SCIP}/rsmod/src/lib.rs', f'{SCIP}/rsmod/index.scip','lib.rs'),
 'c':       (Language(tree_sitter_c.language()), 'c.scm', f'{SCIP}/cmod/a.c', f'{SCIP}/cmod/index.scip','a.c'),
}
def scm_calls(path):
    # keep only the @call patterns from our production query files (they may include other captures)
    txt=open(path).read(); pats=[l for l in txt.split('\n') if '@call' in l]
    return '\n'.join(pats)
def scip_occ(index, doc_suffix):
    d=json.loads(subprocess.run([f'{SCIP}/scip','print','--json',index],capture_output=True,text=True).stdout)
    out={}
    for doc in d['documents']:
        if not doc['relative_path'].endswith(doc_suffix): continue
        for o in doc['occurrences']:
            r=o.get('range')
            if r is None and o.get('TypedRange'):
                s=o['TypedRange'].get('SingleLineRange'); r=[s['line'],s['start_character'],s['end_character']] if s else None
            if not r: continue
            if len(r)==4: r=[r[0],r[1],r[3]]
            out[(r[0],r[1])]=(o['symbol'],o.get('symbol_roles',0),r[2])
    return out
for name,(lang,scm,src,index,suffix) in langs.items():
    parser=Parser(lang); code=open(src,'rb').read(); tree=parser.parse(code)
    q=Query(lang, scm_calls(Q+scm)); caps=QueryCursor(q).captures(tree.root_node)
    names=caps.get('call.name',[])
    occ=scip_occ(index,suffix)
    hits=[];miss=[]
    for n in names:
        key=(n.start_point[0], n.start_point[1]); txt=code[n.start_byte:n.end_byte].decode()
        if key in occ: hits.append((txt,occ[key][0].split('/')[-1][:30],occ[key][1]))
        else: miss.append((txt,key))
    print(f"{name}: call sites={len(names)} resolved={len(hits)} missed={miss}")
    for h in hits: print("   ",h)
