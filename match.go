// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
)

func (m *matcher) matches(cmds []exprCmd, nodes []ast.Node) []ast.Node {
	if len(cmds) == 0 {
		return nodes
	}
	cmd := cmds[0]
	var fn func(exprCmd, []ast.Node) []ast.Node
	switch cmd.name {
	case "x":
		fn = m.cmdRange
	case "g":
		fn = m.cmdFilter(true)
	case "v":
		fn = m.cmdFilter(false)
	}
	return m.matches(cmds[1:], fn(cmd, nodes))
}

func (m *matcher) cmdRange(cmd exprCmd, nodes []ast.Node) []ast.Node {
	var matches []ast.Node
	seen := map[[2]token.Pos]bool{}
	match := func(exprNode, node ast.Node) {
		if node == nil {
			return
		}
		m.values = map[string]ast.Node{}
		found := m.topNode(exprNode, node)
		if found == nil {
			return
		}
		posRange := [2]token.Pos{found.Pos(), found.End()}
		if !seen[posRange] {
			matches = append(matches, found)
			seen[posRange] = true
		}
	}
	for _, node := range nodes {
		walkWithLists(cmd.node, node, match)
	}
	return matches
}

func (m *matcher) cmdFilter(wantAny bool) func(exprCmd, []ast.Node) []ast.Node {
	return func(cmd exprCmd, nodes []ast.Node) []ast.Node {
		var matches []ast.Node
		any := false
		match := func(exprNode, node ast.Node) {
			if node == nil {
				return
			}
			m.values = map[string]ast.Node{}
			found := m.topNode(exprNode, node)
			if found != nil {
				any = true
			}
		}
		for _, node := range nodes {
			any = false
			walkWithLists(cmd.node, node, match)
			if any == wantAny {
				matches = append(matches, node)
			}
		}
		return matches
	}
}

func walkWithLists(exprNode, node ast.Node, fn func(exprNode, node ast.Node)) {
	visit := func(node ast.Node) bool {
		fn(exprNode, node)
		for _, list := range nodeLists(node) {
			fn(exprNode, list)
		}
		return true
	}
	// ast.Walk barfs on ast.Node types it doesn't know, so
	// do the first level manually here
	if list, ok := node.(nodeList); ok {
		if e, ok := exprNode.(ast.Expr); ok {
			// so that "$*a" will match "a, b"
			fn(exprList([]ast.Expr{e}), list)
			// so that "$*a" will match "a; b"
			fn(stmtList([]ast.Stmt{&ast.ExprStmt{X: e}}), list)
		}
		fn(exprNode, list)
		for i := 0; i < list.len(); i++ {
			ast.Inspect(list.at(i), visit)
		}
	} else {
		ast.Inspect(node, visit)
	}
}

func (m *matcher) topNode(exprNode, node ast.Node) ast.Node {
	sts1, ok1 := exprNode.(stmtList)
	sts2, ok2 := node.(stmtList)
	if ok1 && ok2 {
		return m.nodes(sts1, sts2, true)
	}
	if m.node(exprNode, node) {
		return node
	}
	return nil
}

func (m *matcher) node(expr, node ast.Node) bool {
	switch node.(type) {
	case *ast.File, *ast.FuncType, *ast.BlockStmt, *ast.IfStmt,
		*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.CaseClause,
		*ast.CommClause, *ast.ForStmt, *ast.RangeStmt:
		m.scope = m.Info.Scopes[node]
	}
	if !m.aggressive {
		if expr == nil || node == nil {
			return expr == node
		}
	} else {
		if expr == nil && node == nil {
			return true
		}
		if node == nil {
			expr, node = node, expr
		}
	}
	switch x := expr.(type) {
	case nil: // only in aggressive mode
		y, ok := node.(*ast.Ident)
		return ok && y.Name == "_"

	case *ast.File:
		y, ok := node.(*ast.File)
		if !ok || !m.node(x.Name, y.Name) || len(x.Decls) != len(y.Decls) ||
			len(x.Imports) != len(y.Imports) {
			return false
		}
		for i, decl := range x.Decls {
			if !m.node(decl, y.Decls[i]) {
				return false
			}
		}
		for i, imp := range x.Imports {
			if !m.node(imp, y.Imports[i]) {
				return false
			}
		}
		return true

	case *ast.Ident:
		y, yok := node.(*ast.Ident)
		if !isWildName(x.Name) {
			// not a wildcard
			return yok && x.Name == y.Name
		}
		if _, ok := node.(ast.Node); !ok {
			return false // to not include our extra node types
		}
		id := fromWildName(x.Name)
		info := m.info(id)
		if info.any {
			return false
		}
		for _, rx := range info.nameRxs {
			if !yok || !rx.MatchString(y.Name) {
				return false
			}
		}
		if info.needExpr {
			expr, _ := node.(ast.Expr)
			if expr == nil {
				return false // only exprs have types
			}
			t := m.Info.TypeOf(expr)
			if t == nil {
				return false // an expr, but no type?
			}
			if info.comp && !types.Comparable(t) {
				return false
			}
			for _, tc := range info.types {
				want := resolveType(m.scope, tc.expr)
				switch {
				case tc.op == "type" && !types.Identical(t, want):
					return false
				case tc.op == "asgn" && !types.AssignableTo(t, want):
					return false
				case tc.op == "conv" && !types.ConvertibleTo(t, want):
					return false
				}
			}
		}
		if info.name == "_" {
			// values are discarded, matches anything
			return true
		}
		prev, ok := m.values[info.name]
		if !ok {
			// first occurrence, record value
			m.values[info.name] = node
			return true
		}
		// multiple uses must match
		return m.node(prev, node)

	// lists (ys are generated by us while walking)
	case exprList:
		y, ok := node.(exprList)
		return ok && m.exprs(x, y)
	case stmtList:
		y, ok := node.(stmtList)
		return ok && m.stmts(x, y)

	// lits
	case *ast.BasicLit:
		y, ok := node.(*ast.BasicLit)
		return ok && x.Kind == y.Kind && x.Value == y.Value
	case *ast.CompositeLit:
		y, ok := node.(*ast.CompositeLit)
		return ok && m.node(x.Type, y.Type) && m.exprs(x.Elts, y.Elts)
	case *ast.FuncLit:
		y, ok := node.(*ast.FuncLit)
		return ok && m.node(x.Type, y.Type) && m.node(x.Body, y.Body)

	// types
	case *ast.ArrayType:
		y, ok := node.(*ast.ArrayType)
		return ok && m.node(x.Len, y.Len) && m.node(x.Elt, y.Elt)
	case *ast.MapType:
		y, ok := node.(*ast.MapType)
		return ok && m.node(x.Key, y.Key) && m.node(x.Value, y.Value)
	case *ast.StructType:
		y, ok := node.(*ast.StructType)
		return ok && m.fields(x.Fields, y.Fields)
	case *ast.Field:
		// TODO: tags?
		y, ok := node.(*ast.Field)
		return ok && m.idents(x.Names, y.Names) && m.node(x.Type, y.Type)
	case *ast.FuncType:
		y, ok := node.(*ast.FuncType)
		return ok && m.fields(x.Params, y.Params) &&
			m.fields(x.Results, y.Results)
	case *ast.InterfaceType:
		y, ok := node.(*ast.InterfaceType)
		return ok && m.fields(x.Methods, y.Methods)
	case *ast.ChanType:
		y, ok := node.(*ast.ChanType)
		return ok && x.Dir == y.Dir && m.node(x.Value, y.Value)

	// other exprs
	case *ast.Ellipsis:
		y, ok := node.(*ast.Ellipsis)
		return ok && m.node(x.Elt, y.Elt)
	case *ast.ParenExpr:
		y, ok := node.(*ast.ParenExpr)
		return ok && m.node(x.X, y.X)
	case *ast.UnaryExpr:
		y, ok := node.(*ast.UnaryExpr)
		return ok && x.Op == y.Op && m.node(x.X, y.X)
	case *ast.BinaryExpr:
		y, ok := node.(*ast.BinaryExpr)
		return ok && x.Op == y.Op && m.node(x.X, y.X) && m.node(x.Y, y.Y)
	case *ast.CallExpr:
		y, ok := node.(*ast.CallExpr)
		return ok && m.node(x.Fun, y.Fun) && m.exprs(x.Args, y.Args) &&
			bothValid(x.Ellipsis, y.Ellipsis)
	case *ast.KeyValueExpr:
		y, ok := node.(*ast.KeyValueExpr)
		return ok && m.node(x.Key, y.Key) && m.node(x.Value, y.Value)
	case *ast.StarExpr:
		y, ok := node.(*ast.StarExpr)
		return ok && m.node(x.X, y.X)
	case *ast.SelectorExpr:
		y, ok := node.(*ast.SelectorExpr)
		return ok && m.node(x.X, y.X) && m.node(x.Sel, y.Sel)
	case *ast.IndexExpr:
		y, ok := node.(*ast.IndexExpr)
		return ok && m.node(x.X, y.X) && m.node(x.Index, y.Index)
	case *ast.SliceExpr:
		y, ok := node.(*ast.SliceExpr)
		return ok && m.node(x.X, y.X) && m.node(x.Low, y.Low) &&
			m.node(x.High, y.High) && m.node(x.Max, y.Max)
	case *ast.TypeAssertExpr:
		y, ok := node.(*ast.TypeAssertExpr)
		return ok && m.node(x.X, y.X) && m.node(x.Type, y.Type)

	// decls
	case *ast.GenDecl:
		if m.aggressive && len(x.Specs) == 1 && m.node(x.Specs[0], node) {
			return true
		}
		y, ok := node.(*ast.GenDecl)
		return ok && x.Tok == y.Tok && m.specs(x.Specs, y.Specs)
	case *ast.FuncDecl:
		y, ok := node.(*ast.FuncDecl)
		return ok && m.fields(x.Recv, y.Recv) && m.node(x.Name, y.Name) &&
			m.node(x.Type, y.Type) && m.node(x.Body, y.Body)

	// specs
	case *ast.ValueSpec:
		y, ok := node.(*ast.ValueSpec)
		if !ok || !m.node(x.Type, y.Type) {
			return false
		}
		if m.aggressive && len(x.Names) == 1 {
			for i := range y.Names {
				if m.node(x.Names[i], y.Names[i]) &&
					(x.Values == nil || m.node(x.Values[i], y.Values[i])) {
					return true
				}
			}
		}
		return m.idents(x.Names, y.Names) && m.exprs(x.Values, y.Values)

	// stmt bridge nodes
	case *ast.ExprStmt:
		if id, ok := x.X.(*ast.Ident); ok && isWildName(id.Name) {
			// prefer matching $x as a statement, as it's
			// the parent
			return m.node(id, node)
		}
		y, ok := node.(*ast.ExprStmt)
		return ok && m.node(x.X, y.X)
	case *ast.DeclStmt:
		y, ok := node.(*ast.DeclStmt)
		return ok && m.node(x.Decl, y.Decl)

	// stmts
	case *ast.EmptyStmt:
		_, ok := node.(*ast.EmptyStmt)
		return ok
	case *ast.LabeledStmt:
		y, ok := node.(*ast.LabeledStmt)
		return ok && m.node(x.Label, y.Label) && m.node(x.Stmt, y.Stmt)
	case *ast.SendStmt:
		y, ok := node.(*ast.SendStmt)
		return ok && m.node(x.Chan, y.Chan) && m.node(x.Value, y.Value)
	case *ast.IncDecStmt:
		y, ok := node.(*ast.IncDecStmt)
		return ok && x.Tok == y.Tok && m.node(x.X, y.X)
	case *ast.AssignStmt:
		y, ok := node.(*ast.AssignStmt)
		return ok && x.Tok == y.Tok && m.exprs(x.Lhs, y.Lhs) &&
			m.exprs(x.Rhs, y.Rhs)
	case *ast.GoStmt:
		y, ok := node.(*ast.GoStmt)
		return ok && m.node(x.Call, y.Call)
	case *ast.DeferStmt:
		y, ok := node.(*ast.DeferStmt)
		return ok && m.node(x.Call, y.Call)
	case *ast.ReturnStmt:
		y, ok := node.(*ast.ReturnStmt)
		return ok && m.exprs(x.Results, y.Results)
	case *ast.BranchStmt:
		y, ok := node.(*ast.BranchStmt)
		return ok && x.Tok == y.Tok && m.node(maybeNilIdent(x.Label), maybeNilIdent(y.Label))
	case *ast.BlockStmt:
		if m.aggressive && m.node(stmtList(x.List), node) {
			return true
		}
		y, ok := node.(*ast.BlockStmt)
		return ok && (m.cases(x.List, y.List) || m.stmts(x.List, y.List))
	case *ast.IfStmt:
		y, ok := node.(*ast.IfStmt)
		if !ok {
			return false
		}
		ident, ok := x.Cond.(*ast.Ident)
		switch {
		case x.Init != nil:
		case !ok, !isWildName(ident.Name):
		case !m.info(fromWildName(ident.Name)).any:
		default:
			// for $*x { ... } on the left
			left := stmtList([]ast.Stmt{&ast.ExprStmt{X: ident}})
			return m.node(left, initExprList(y.Init, y.Cond, nil)) &&
				m.node(x.Body, y.Body)
		}
		return m.node(x.Init, y.Init) && m.node(x.Cond, y.Cond) &&
			m.node(x.Body, y.Body) && m.node(x.Else, y.Else)
	case *ast.CaseClause:
		y, ok := node.(*ast.CaseClause)
		return ok && m.exprs(x.List, y.List) && m.stmts(x.Body, y.Body)
	case *ast.SwitchStmt:
		y, ok := node.(*ast.SwitchStmt)
		if !ok {
			return false
		}
		ident, ok := x.Tag.(*ast.Ident)
		switch {
		case x.Init != nil:
		case !ok, !isWildName(ident.Name):
		case !m.info(fromWildName(ident.Name)).any:
		default:
			// for $*x { ... } on the left
			left := stmtList([]ast.Stmt{&ast.ExprStmt{X: ident}})
			return m.node(left, initExprList(y.Init, y.Tag, nil)) &&
				m.node(x.Body, y.Body)
		}
		return m.node(x.Init, y.Init) && m.node(x.Tag, y.Tag) && m.node(x.Body, y.Body)
	case *ast.TypeSwitchStmt:
		y, ok := node.(*ast.TypeSwitchStmt)
		return ok && m.node(x.Init, y.Init) && m.node(x.Assign, y.Assign) && m.node(x.Body, y.Body)
	case *ast.CommClause:
		y, ok := node.(*ast.CommClause)
		return ok && m.node(x.Comm, y.Comm) && m.stmts(x.Body, y.Body)
	case *ast.SelectStmt:
		y, ok := node.(*ast.SelectStmt)
		return ok && m.node(x.Body, y.Body)
	case *ast.ForStmt:
		y, ok := node.(*ast.ForStmt)
		if !ok {
			return false
		}
		ident, ok := x.Cond.(*ast.Ident)
		switch {
		case x.Init != nil, x.Post != nil:
		case !ok, !isWildName(ident.Name):
		case !m.info(fromWildName(ident.Name)).any:
		default:
			// for $*x { ... } on the left
			left := stmtList([]ast.Stmt{&ast.ExprStmt{X: ident}})
			return m.node(left, initExprList(y.Init, y.Cond, y.Post)) &&
				m.node(x.Body, y.Body)
		}
		return m.node(x.Init, y.Init) && m.node(x.Cond, y.Cond) &&
			m.node(x.Post, y.Post) && m.node(x.Body, y.Body)
	case *ast.RangeStmt:
		y, ok := node.(*ast.RangeStmt)
		return ok && m.node(x.Key, y.Key) && m.node(x.Value, y.Value) &&
			m.node(x.X, y.X) && m.node(x.Body, y.Body)
	default:
		panic(fmt.Sprintf("unexpected node: %T", x))
	}
	panic(fmt.Sprintf("unfinished node: %T", expr))
}

func resolveType(scope *types.Scope, expr ast.Expr) types.Type {
	switch x := expr.(type) {
	case *ast.Ident:
		_, obj := scope.LookupParent(x.Name, token.NoPos)
		return obj.Type()
	case *ast.ArrayType:
		elt := resolveType(scope, x.Elt)
		if x.Len == nil {
			return types.NewSlice(elt)
		}
		bl, ok := x.Len.(*ast.BasicLit)
		if !ok || bl.Kind != token.INT {
			panic(fmt.Sprintf("TODO: %T", x))
		}
		len, _ := strconv.ParseInt(bl.Value, 0, 0)
		return types.NewArray(elt, len)
	case *ast.StarExpr:
		return types.NewPointer(resolveType(scope, x.X))
	case *ast.SelectorExpr:
		scope = findScope(scope, x.X)
		return resolveType(scope, x.Sel)
	default:
		panic(fmt.Sprintf("resolveType TODO: %T", x))
	}
}

func findScope(scope *types.Scope, expr ast.Expr) *types.Scope {
	switch x := expr.(type) {
	case *ast.Ident:
		_, obj := scope.LookupParent(x.Name, token.NoPos)
		return obj.(*types.PkgName).Imported().Scope()
	default:
		panic(fmt.Sprintf("findScope TODO: %T", x))
	}
}

func maybeNilIdent(x *ast.Ident) ast.Node {
	if x == nil {
		return nil
	}
	return x
}

func bothValid(p1, p2 token.Pos) bool {
	return p1.IsValid() == p2.IsValid()
}

type nodeList interface {
	at(i int) ast.Node
	len() int
	slice(from, to int) nodeList
	ast.Node
}

// nodes matches two lists of nodes. It uses a common algorithm to match
// wildcard patterns with any number of nodes without recursion.
func (m *matcher) nodes(ns1, ns2 nodeList, partial bool) ast.Node {
	ns1len, ns2len := ns1.len(), ns2.len()
	if ns1len == 0 {
		if ns2len == 0 {
			return ns2
		}
		return nil
	}
	// We need to keep a copy of m.values so that we can restart
	// with a different "any of" match while discarding any matches
	// we found while trying it.
	var oldMatches map[string]ast.Node
	backupMatches := func() {
		oldMatches = make(map[string]ast.Node, len(m.values))
		for k, v := range m.values {
			oldMatches[k] = v
		}
	}
	backupMatches()

	partialStart, partialEnd := 0, ns2len
	i1, i2 := 0, 0
	next1, next2 := 0, 0
	wildName := ""
	wildStart := 0

	// wouldMatch returns whether the current wildcard - if any -
	// matches the nodes we are currently trying it on.
	wouldMatch := func() bool {
		switch wildName {
		case "", "_":
			return true
		}
		list := ns2.slice(wildStart, i2)
		// check that it matches any nodes found elsewhere
		prev, ok := m.values[wildName]
		if ok && !m.node(prev, list) {
			return false
		}
		m.values[wildName] = list
		return true
	}
	for i1 < ns1len || i2 < ns2len {
		if i1 < ns1len {
			n1 := ns1.at(i1)
			id := fromWildNode(n1)
			info := m.info(id)
			if info.any {
				// keep track of where this wildcard
				// started (if info.name == wildName,
				// we're trying the same wildcard
				// matching one more node)
				if info.name != wildName {
					wildStart = i2
					wildName = info.name
				}
				// try to match zero or more at i2,
				// restarting at i2+1 if it fails
				next1 = i1
				next2 = i2 + 1
				backupMatches()
				i1++
				continue
			}
			if partial && i1 == 0 {
				// let "b; c" match "a; b; c"
				// (simulates a $*_ at the beginning)
				next2 = i2 + 1
				partialStart = i2
				backupMatches()
			}
			if i2 < ns2len && wouldMatch() && m.node(n1, ns2.at(i2)) {
				wildName = ""
				// ordinary match
				i1++
				i2++
				continue
			}
		}
		if partial && i1 == ns1len && wildName == "" {
			partialEnd = i2
			break // let "b; c" match "b; c; d"
		}
		// mismatch, try to restart
		if 0 < next2 && next2 <= ns2len && (i1 != next1 || i2 != next2) {
			i1 = next1
			i2 = next2
			m.values = oldMatches
			continue
		}
		return nil
	}
	if !wouldMatch() {
		return nil
	}
	return ns2.slice(partialStart, partialEnd)
}

func (m *matcher) exprs(exprs1, exprs2 []ast.Expr) bool {
	return m.nodes(exprList(exprs1), exprList(exprs2), false) != nil
}

func (m *matcher) idents(ids1, ids2 []*ast.Ident) bool {
	return m.nodes(identList(ids1), identList(ids2), false) != nil
}

func initExprList(init ast.Stmt, expr ast.Expr, post ast.Stmt) stmtList {
	var stmts []ast.Stmt
	if init != nil {
		stmts = append(stmts, init)
	}
	if expr != nil {
		stmts = append(stmts, &ast.ExprStmt{X: expr})
	}
	if post != nil {
		stmts = append(stmts, post)
	}
	return stmtList(stmts)
}

func (m *matcher) cases(stmts1, stmts2 []ast.Stmt) bool {
	for _, stmt := range stmts2 {
		switch stmt.(type) {
		case *ast.CaseClause, *ast.CommClause:
		default:
			return false
		}
	}
	var left []*ast.Ident
	for _, stmt := range stmts1 {
		var expr ast.Expr
		var bstmt ast.Stmt
		switch x := stmt.(type) {
		case *ast.CaseClause:
			if len(x.List) != 1 || len(x.Body) != 1 {
				return false
			}
			expr, bstmt = x.List[0], x.Body[0]
		case *ast.CommClause:
			if x.Comm == nil || len(x.Body) != 1 {
				return false
			}
			expr, bstmt = x.Comm.(*ast.ExprStmt).X, x.Body[0]
		default:
			return false
		}
		xs, ok := bstmt.(*ast.ExprStmt)
		if !ok {
			return false
		}
		bodyIdent, ok := xs.X.(*ast.Ident)
		if !ok || bodyIdent.Name != "gogrep_body" {
			return false
		}
		id, ok := expr.(*ast.Ident)
		if !ok || !isWildName(id.Name) {
			return false
		}
		left = append(left, id)
	}
	return m.nodes(identList(left), stmtList(stmts2), false) != nil
}

func (m *matcher) stmts(stmts1, stmts2 []ast.Stmt) bool {
	return m.nodes(stmtList(stmts1), stmtList(stmts2), false) != nil
}

func (m *matcher) specs(specs1, specs2 []ast.Spec) bool {
	return m.nodes(specList(specs1), specList(specs2), false) != nil
}

func (m *matcher) fields(fields1, fields2 *ast.FieldList) bool {
	if fields1 == nil || fields2 == nil {
		return fields1 == fields2
	}
	if len(fields1.List) != len(fields2.List) {
		return false
	}
	for i, f1 := range fields1.List {
		if !m.node(f1, fields2.List[i]) {
			return false
		}
	}
	return true
}

func fromWildNode(node ast.Node) int {
	switch x := node.(type) {
	case *ast.Ident:
		return fromWildName(x.Name)
	case *ast.ExprStmt:
		return fromWildNode(x.X)
	}
	return -1
}

func nodeLists(n ast.Node) []nodeList {
	var lists []nodeList
	addList := func(list nodeList) {
		if list.len() > 0 {
			lists = append(lists, list)
		}
	}
	switch x := n.(type) {
	case *ast.CompositeLit:
		addList(exprList(x.Elts))
	case *ast.CallExpr:
		addList(exprList(x.Args))
	case *ast.AssignStmt:
		addList(exprList(x.Lhs))
		addList(exprList(x.Rhs))
	case *ast.ReturnStmt:
		addList(exprList(x.Results))
	case *ast.ValueSpec:
		addList(exprList(x.Values))
	case *ast.BlockStmt:
		addList(stmtList(x.List))
	case *ast.CaseClause:
		addList(exprList(x.List))
		addList(stmtList(x.Body))
	case *ast.CommClause:
		addList(stmtList(x.Body))
	}
	return lists
}

type exprList []ast.Expr
type identList []*ast.Ident
type stmtList []ast.Stmt
type specList []ast.Spec

func (l exprList) len() int  { return len(l) }
func (l identList) len() int { return len(l) }
func (l stmtList) len() int  { return len(l) }
func (l specList) len() int  { return len(l) }

func (l exprList) at(i int) ast.Node  { return l[i] }
func (l identList) at(i int) ast.Node { return l[i] }
func (l stmtList) at(i int) ast.Node  { return l[i] }
func (l specList) at(i int) ast.Node  { return l[i] }

func (l exprList) slice(i, j int) nodeList  { return l[i:j] }
func (l identList) slice(i, j int) nodeList { return l[i:j] }
func (l stmtList) slice(i, j int) nodeList  { return l[i:j] }
func (l specList) slice(i, j int) nodeList  { return l[i:j] }

func (l exprList) Pos() token.Pos  { return l[0].Pos() }
func (l identList) Pos() token.Pos { return l[0].Pos() }
func (l stmtList) Pos() token.Pos  { return l[0].Pos() }
func (l specList) Pos() token.Pos  { return l[0].Pos() }

func (l exprList) End() token.Pos  { return l[len(l)-1].End() }
func (l identList) End() token.Pos { return l[len(l)-1].End() }
func (l stmtList) End() token.Pos  { return l[len(l)-1].End() }
func (l specList) End() token.Pos  { return l[len(l)-1].End() }
