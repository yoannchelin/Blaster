// Package complexity computes the cyclomatic complexity of Go functions by
// parsing source files with go/ast. It is intentionally lightweight: one
// public function, no state, no cgo, no DB.
//
// The metric counts decision points (if, for, range, switch cases, select
// cases, short-circuit operators) and adds 1. A score ≥ 10 is a signal that
// the touched function carries meaningful branching risk.
package complexity

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// OfFunction parses the Go source file at filePath and returns the cyclomatic
// complexity of the function body that contains lineStart. Returns 1 (the
// minimum) when the file cannot be parsed or no matching function is found.
func OfFunction(filePath string, lineStart int) int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		return 1
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		if lineStart >= start && lineStart <= end {
			return countBranches(fn.Body) + 1
		}
	}
	return 1
}

// countBranches walks a function body and counts decision points.
func countBranches(body *ast.BlockStmt) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.IfStmt:
			n++
		case *ast.ForStmt, *ast.RangeStmt:
			n++
		case *ast.CaseClause:
			if v.List != nil { // non-default case
				n++
			}
		case *ast.CommClause:
			if v.Comm != nil { // non-default select case
				n++
			}
		case *ast.BinaryExpr:
			if v.Op.String() == "&&" || v.Op.String() == "||" {
				n++
			}
		}
		return true
	})
	return n
}
