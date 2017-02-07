// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"go/ast"
	"go/token"
)

// TODO: use x/tools/go/ssa?

// RULE: remove each one of the statements
func (r *reducer) removeStmt(list *[]ast.Stmt) {
	orig := *list
	for i, stmt := range orig {
		// discard those that will break compilation
		switch x := stmt.(type) {
		case *ast.DeclStmt, *ast.ReturnStmt:
			continue
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE { // :=
				continue
			}
		}
		l := make([]ast.Stmt, len(orig)-1)
		copy(l, orig[:i])
		copy(l[i:], orig[i+1:])
		*list = l
		if r.okChange() {
			return
		}
	}
	*list = orig
}

func (r *reducer) changeStmt(stmt ast.Stmt) bool {
	orig := *r.stmt
	if *r.stmt = stmt; r.okChange() {
		return true
	}
	*r.stmt = orig
	return false
}

// RULE: bypass to if or else branches
func (r *reducer) bypassIf(ifs *ast.IfStmt) {
	switch {
	case r.changeStmt(ifs.Body):
	case ifs.Else != nil && r.changeStmt(ifs.Else):
	}
}

// RULE: reduce basic lits to zero values
func (r *reducer) reduceLit(l *ast.BasicLit) {
	orig := l.Value
	changeValue := func(val string) {
		if l.Value == val {
			return
		}
		if l.Value = val; !r.okChange() {
			l.Value = orig
		}
	}
	switch l.Kind {
	case token.STRING:
		changeValue(`""`)
	case token.INT:
		changeValue(`0`)
	}
}

// RULE: remove slice expression parts
func (r *reducer) reduceSlice(sl *ast.SliceExpr) {
	if r.changeExpr(sl.X) {
		return
	}
	for i, expr := range [...]*ast.Expr{
		&sl.Max,
		&sl.High,
		&sl.Low,
	} {
		orig := *expr
		if orig == nil {
			continue
		}
		if i == 0 {
			sl.Slice3 = false
		}
		if *expr = nil; r.okChange() {
			return
		}
		if i == 0 {
			sl.Slice3 = true
		}
		*expr = orig
	}
}

func (r *reducer) changeExpr(expr ast.Expr) bool {
	orig := *r.expr
	if *r.expr = expr; r.okChange() {
		return true
	}
	*r.expr = orig
	return false
}

// RULE: reduce binary expressions
func (r *reducer) reduceBinary(bi *ast.BinaryExpr) {
	switch {
	case r.changeExpr(bi.X):
	case r.changeExpr(bi.Y):
	}
}
