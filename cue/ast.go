// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cue

import (
	"fmt"
	"strconv"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/token"
)

// insertFile inserts the given file at the root of the instance.
//
// The contents will be merged (unified) with any pre-existing value. In this
// case an error may be reported, but only if the merge failed at the top-level.
// Other errors will be recorded at the respective values in the tree.
//
// There should be no unresolved identifiers in file, meaning the Node field
// of all identifiers should be set to a non-nil value.
func (inst *Instance) insertFile(f *ast.File) error {
	// TODO: insert by converting to value first so that the trim command can
	// also remove top-level fields.
	// First process single file.
	v := newVisitor(inst.index, inst.inst, inst.rootStruct, inst.scope)
	v.astState.astMap[f] = inst.rootStruct
	result := v.walk(f)
	if isBottom(result) {
		return result.(*bottom)
	}

	for _, c := range v.comprehensions {
		inst.rootValue = mkBin(v.ctx(), token.NoPos, opUnify, inst.rootValue, c)
	}

	return nil
}

type astVisitor struct {
	*astState
	object         *structLit
	comprehensions []*structComprehension

	inSelector int
}

func (v *astVisitor) ctx() *context {
	return v.astState.ctx
}

type astState struct {
	ctx *context
	*index
	inst *build.Instance

	litParser   *litParser
	resolveRoot *structLit

	// make unique per level to avoid reuse of structs being an issue.
	astMap map[ast.Node]scope
}

func (s *astState) mapScope(n ast.Node) (m scope) {
	if m = s.astMap[n]; m == nil {
		m = newStruct(newNode(n))
		s.astMap[n] = m
	}
	return m
}

func (s *astState) setScope(n ast.Node, v scope) {
	if m, ok := s.astMap[n]; ok && m != v {
		panic("already defined")
	}
	s.astMap[n] = v
}

func newVisitor(idx *index, inst *build.Instance, obj, resolveRoot *structLit) *astVisitor {
	ctx := idx.newContext()
	v := &astVisitor{
		object: obj,
	}
	v.astState = &astState{
		ctx:         ctx,
		index:       ctx.index,
		inst:        inst,
		litParser:   &litParser{ctx: ctx},
		resolveRoot: resolveRoot,
		astMap:      map[ast.Node]scope{},
	}
	return v
}

func (v *astVisitor) error(n ast.Node, args ...interface{}) value {
	return v.mkErr(newNode(n), args...)
}

func (v *astVisitor) resolve(n *ast.Ident) value {
	ctx := v.ctx()
	label := v.label(n.Name, true)
	if r := v.resolveRoot; r != nil {
		if value, _ := r.lookup(v.ctx(), label); value != nil {
			return &selectorExpr{newExpr(n),
				&nodeRef{baseValue: newExpr(n), node: r}, label}
		}
		if v.inSelector > 0 {
			if p := getBuiltinShorthandPkg(ctx, n.Name); p != nil {
				return &nodeRef{baseValue: newExpr(n), node: p}
			}
		}
	}
	return nil
}

func (v *astVisitor) loadImport(imp *ast.ImportSpec) evaluated {
	ctx := v.ctx()
	val := lookupBuiltinPkg(ctx, imp)
	if !isBottom(val) {
		return val
	}
	path, err := strconv.Unquote(imp.Path.Value)
	if err != nil {
		return ctx.mkErr(newNode(imp), "illformed import spec")
	}
	bimp := v.inst.LookupImport(path)
	if bimp == nil {
		return ctx.mkErr(newNode(imp), "package %q not found", path)
	}
	impInst := v.index.loadInstance(bimp)
	return impInst.rootValue.evalPartial(ctx)
}

// We probably don't need to call Walk.s
func (v *astVisitor) walk(astNode ast.Node) (value value) {
	switch n := astNode.(type) {
	case *ast.File:
		obj := v.object
		v1 := &astVisitor{
			astState: v.astState,
			object:   obj,
		}
		for _, e := range n.Decls {
			switch x := e.(type) {
			case *ast.EmitDecl:
				if v1.object.emit == nil {
					v1.object.emit = v1.walk(x.Expr)
				} else {
					v1.object.emit = mkBin(v.ctx(), token.NoPos, opUnify, v1.object.emit, v1.walk(x.Expr))
				}
			default:
				v1.walk(e)
			}
		}
		for _, c := range v1.comprehensions {
			v.comprehensions = append(v.comprehensions, c)
		}
		value = obj

	case *ast.ImportDecl:
		for _, s := range n.Specs {
			v.walk(s)
		}

	case *ast.ImportSpec:
		val := v.loadImport(n)
		if !isBottom(val) {
			v.setScope(n, val.(*structLit))
		}

	case *ast.StructLit:
		obj := v.mapScope(n).(*structLit)
		v1 := &astVisitor{
			astState: v.astState,
			object:   obj,
		}
		for _, e := range n.Elts {
			switch x := e.(type) {
			case *ast.EmitDecl:
				// Only allowed at top-level.
				v1.error(x, "emitting values is only allowed at top level")
			case *ast.Field, *ast.Alias:
				v1.walk(e)
			case *ast.ComprehensionDecl:
				v1.walk(x)
			}
		}
		value = obj
		for i, c := range v1.comprehensions {
			if i == 0 && obj.template == nil && len(obj.arcs) == 0 {
				value = c
				continue
			}
			value = mkBin(v.ctx(), token.NoPos, opUnify, value, c)
		}

	case *ast.ComprehensionDecl:
		yielder := &yield{baseValue: newExpr(n.Field.Value)}
		sc := &structComprehension{
			baseValue: newDecl(n),
			clauses:   wrapClauses(v, yielder, n.Clauses),
		}
		field := n.Field
		switch x := field.Label.(type) {
		case *ast.Interpolation:
			yielder.key = v.walk(x)
			yielder.value = v.walk(field.Value)

		case *ast.TemplateLabel:
			f := v.label(x.Ident.Name, true)

			sig := &params{}
			sig.add(f, &basicType{newNode(field.Label), stringKind})
			template := &lambdaExpr{newExpr(field.Value), sig, nil}

			v.setScope(field, template)
			template.value = v.walk(field.Value)
			yielder.value = template
			sc.isTemplate = true

		case *ast.BasicLit, *ast.Ident:
			name, ok := ast.LabelName(x)
			if !ok {
				return v.error(x, "invalid field name: %v", x)
			}
			if name != "" {
				yielder.key = &stringLit{newNode(x), name}
				yielder.value = v.walk(field.Value)
			}

		default:
			panic("cue: unknown label type")
		}
		// yielder.key = v.walk(n.Field.Label)
		// yielder.value = v.walk(n.Field.Value)
		v.comprehensions = append(v.comprehensions, sc)

	case *ast.Field:
		switch x := n.Label.(type) {
		case *ast.Interpolation:
			yielder := &yield{baseValue: newNode(x)}
			sc := &structComprehension{
				baseValue: newDecl(n),
				clauses:   yielder,
			}
			yielder.key = v.walk(x)
			yielder.value = v.walk(n.Value)
			v.comprehensions = append(v.comprehensions, sc)

		case *ast.TemplateLabel:
			f := v.label(x.Ident.Name, true)

			sig := &params{}
			sig.add(f, &basicType{newNode(n.Label), stringKind})
			template := &lambdaExpr{newExpr(n.Value), sig, nil}

			v.setScope(n, template)
			template.value = v.walk(n.Value)

			if v.object.template == nil {
				v.object.template = template
			} else {
				v.object.template = mkBin(v.ctx(), token.NoPos, opUnify, v.object.template, template)
			}

		case *ast.BasicLit, *ast.Ident:
			f, ok := v.nodeLabel(x)
			if !ok {
				return v.error(n.Label, "invalid field name: %v", n.Label)
			}
			if f != 0 {
				v.object.insertValue(v.ctx(), f, v.walk(n.Value))
			}

		default:
			panic("cue: unknown label type")
		}

	case *ast.Alias:
		// parsed verbatim at reference.

	case *ast.LambdaExpr:
		sig := &params{}
		lambda := &lambdaExpr{newExpr(n), sig, nil}
		v.setScope(n, lambda)

		for _, p := range n.Params {
			f, _ := v.nodeLabel(p.Label)
			if p.Value != nil {
				sig.add(f, v.walk(p.Value))
			} else {
				src := &ast.Ident{NamePos: p.Pos(), Name: "_"}
				sig.add(f, &top{baseValue: newExpr(src)})
			}
		}
		lambda.value = v.walk(n.Expr)
		return lambda

	case *ast.ListComprehension:
		yielder := &yield{baseValue: newExpr(n.Expr)}
		lc := &listComprehension{
			newExpr(n),
			wrapClauses(v, yielder, n.Clauses),
		}
		// we don't support key for lists (yet?)
		yielder.value = v.walk(n.Expr)
		return lc

	// Expressions
	case *ast.Ident:
		if n.Node == nil {
			if value = v.resolve(n); value != nil {
				break
			}

			switch n.Name {
			case "_":
				return &top{newExpr(n)}
			case "string":
				return &basicType{newExpr(n), stringKind}
			case "bytes":
				return &basicType{newExpr(n), bytesKind}
			case "bool":
				return &basicType{newExpr(n), boolKind}
			case "int":
				return &basicType{newExpr(n), intKind}
			case "float":
				return &basicType{newExpr(n), floatKind}
			case "number":
				return &basicType{newExpr(n), numKind}
			case "duration":
				return &basicType{newExpr(n), durationKind}

			case "len":
				return lenBuiltin
			}
			if r, ok := predefinedRanges[n.Name]; ok {
				return r
			}

			value = v.error(n, "reference %q not found", n.Name)
			break
		}

		if a, ok := n.Node.(*ast.Alias); ok {
			value = v.walk(a.Expr)
			break
		}

		label := v.label(n.Name, true)
		if n.Scope != nil {
			n2 := v.mapScope(n.Scope)
			value = &nodeRef{baseValue: newExpr(n), node: n2}
			value = &selectorExpr{newExpr(n), value, label}
		} else {
			n2 := v.mapScope(n.Node)
			value = &nodeRef{baseValue: newExpr(n), node: n2}
		}

	case *ast.BottomLit:
		value = v.error(n, "from source")

	case *ast.BadDecl:
		// nothing to do

	case *ast.BadExpr:
		value = v.error(n, "invalid expression")

	case *ast.BasicLit:
		value = v.litParser.parse(n)

	case *ast.Interpolation:
		if len(n.Elts) == 0 {
			return v.error(n, "invalid interpolation")
		}
		first, ok1 := n.Elts[0].(*ast.BasicLit)
		last, ok2 := n.Elts[len(n.Elts)-1].(*ast.BasicLit)
		if !ok1 || !ok2 {
			return v.error(n, "invalid interpolation")
		}
		if len(n.Elts) == 1 {
			value = v.walk(n.Elts[0])
			break
		}
		lit := &interpolation{baseValue: newExpr(n), k: stringKind}
		value = lit
		quote, err := stringType(first.Value)
		if err != nil {
			return v.error(n, "invalid interpolation: %v", err)
		}
		if quote[0] == '\'' {
			return v.error(n, "interpolation not implemented for bytes: %v", err)
		}
		ws, err := wsPrefix(last.Value, quote)
		if err != nil {
			return v.error(n, "invalid interpolation: %v", err)
		}
		prefix := quote
		multi := len(quote) == 3
		p := v.litParser
		for i := 0; i < len(n.Elts); i += 2 {
			l, ok := n.Elts[i].(*ast.BasicLit)
			if !ok {
				return v.error(n, "invalid interpolation")
			}
			if err := p.init(l); err != nil {
				return v.error(n, "invalid interpolation: %v", err)
			}
			if i+1 < len(n.Elts) {
				x := p.parseString(prefix, `\(`, ws, multi, quote[0])
				lit.parts = append(lit.parts, x, v.walk(n.Elts[i+1]))
			} else {
				x := p.parseString(prefix, quote, ws, multi, quote[0])
				lit.parts = append(lit.parts, x)
			}
			prefix = ")"
		}

	case *ast.ListLit:
		list := &list{baseValue: newExpr(n)}
		for _, e := range n.Elts {
			list.a = append(list.a, v.walk(e))
		}
		list.initLit()
		if n.Ellipsis != token.NoPos || n.Type != nil {
			list.len = &rangeLit{list.baseValue, list.len, &top{list.baseValue}}
			if n.Type != nil {
				list.typ = v.walk(n.Type)
			}
		}
		value = list

	case *ast.ParenExpr:
		value = v.walk(n.X)

	case *ast.SelectorExpr:
		v.inSelector++
		value = &selectorExpr{
			newExpr(n),
			v.walk(n.X),
			v.label(n.Sel.Name, true),
		}
		v.inSelector--

	case *ast.IndexExpr:
		value = &indexExpr{newExpr(n), v.walk(n.X), v.walk(n.Index)}

	case *ast.SliceExpr:
		slice := &sliceExpr{baseValue: newExpr(n), x: v.walk(n.X)}
		if n.Low != nil {
			slice.lo = v.walk(n.Low)
		}
		if n.High != nil {
			slice.hi = v.walk(n.High)
		}
		value = slice

	case *ast.CallExpr:
		call := &callExpr{baseValue: newExpr(n), x: v.walk(n.Fun)}
		for _, a := range n.Args {
			call.args = append(call.args, v.walk(a))
		}
		value = call

	case *ast.UnaryExpr:
		value = &unaryExpr{
			newExpr(n),
			tokenMap[n.Op],
			v.walk(n.X),
		}

	case *ast.BinaryExpr:
		switch n.Op {
		case token.DISJUNCTION:
			value = makeDisjunction(v.ctx(), n, v.walk(n.X), v.walk(n.Y))
		case token.RANGE:
			value = &rangeLit{
				newExpr(n),
				v.walk(n.X), // from
				v.walk(n.Y), // to
			}
		default:
			value = &binaryExpr{
				newExpr(n),
				tokenMap[n.Op], // op
				v.walk(n.X),    // left
				v.walk(n.Y),    // right
			}
		}

	// nothing to do
	// case *syntax.EmitDecl:
	default:
		// TODO: unhandled node.
		// value = ctx.mkErr(n, "unknown node type %T", n)
		panic(fmt.Sprintf("unimplemented %T", n))

	}
	return value
}

func wrapClauses(v *astVisitor, y yielder, clauses []ast.Clause) yielder {
	for _, c := range clauses {
		if n, ok := c.(*ast.ForClause); ok {
			params := &params{}
			fn := &lambdaExpr{newExpr(n.Source), params, nil}
			v.setScope(n, fn)
		}
	}
	for i := len(clauses) - 1; i >= 0; i-- {
		switch n := clauses[i].(type) {
		case *ast.ForClause:
			fn := v.mapScope(n).(*lambdaExpr)
			fn.value = y

			key := "_"
			if n.Key != nil {
				key = n.Key.Name
			}
			f := v.label(key, true)
			fn.add(f, &basicType{newExpr(n.Key), stringKind | intKind})

			f = v.label(n.Value.Name, true)
			fn.add(f, &top{})

			y = &feed{newExpr(n.Source), v.walk(n.Source), fn}

		case *ast.IfClause:
			y = &guard{newExpr(n.Condition), v.walk(n.Condition), y}
		}
	}
	return y
}