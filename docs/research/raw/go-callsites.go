package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type site struct {
	File            string `json:"file"`
	Line, Col, End  int
	Kind            string `json:"kind"` // ident | selector | other
	Enclosing       string `json:"enclosing"`
}

func main() {
	root := os.Args[1]
	fset := token.NewFileSet()
	var out []site
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") { return nil }
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil { return nil }
		rel, _ := filepath.Rel(root, p)
		var encl []string
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				encl = []string{x.Name.Name}
			case *ast.CallExpr:
				var id *ast.Ident; kind := "other"
				switch fn := x.Fun.(type) {
				case *ast.Ident: id, kind = fn, "ident"
				case *ast.SelectorExpr: id, kind = fn.Sel, "selector"
				case *ast.IndexExpr: // generic instantiation f[T](...)
					if i, ok := fn.X.(*ast.Ident); ok { id, kind = i, "generic" }
				}
				e := ""; if len(encl) > 0 { e = encl[0] }
				if id != nil {
					pos := fset.Position(id.Pos()); end := fset.Position(id.End())
					out = append(out, site{rel, pos.Line - 1, pos.Column - 1, end.Column - 1, kind, e})
				} else {
					pos := fset.Position(x.Lparen)
					out = append(out, site{rel, pos.Line - 1, pos.Column - 1, 0, kind, e})
				}
			}
			return true
		})
		return nil
	})
	json.NewEncoder(os.Stdout).Encode(out)
	fmt.Fprintln(os.Stderr, "sites", len(out))
}
