// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
)

func (check *Checker) reportAltDecl(obj Object) {
	if pos := obj.Pos(); pos.IsValid() {
		// We use "other" rather than "previous" here because
		// the first declaration seen may not be textually
		// earlier in the source.
		check.errorf(obj, _DuplicateDecl, "\tother declaration of %s", obj.Name()) // secondary error, \t indented
	}
}

func (check *Checker) declare(scope *Scope, id *ast.Ident, obj Object, pos token.Pos) {
	// spec: "The blank identifier, represented by the underscore
	// character _, may be used in a declaration like any other
	// identifier but the declaration does not introduce a new
	// binding."
	if obj.Name() != "_" {
		if alt := scope.Insert(obj); alt != nil {
			check.errorf(obj, _DuplicateDecl, "%s redeclared in this block", obj.Name())
			check.reportAltDecl(alt)
			return
		}
		obj.setScopePos(pos)
	}
	if id != nil {
		check.recordDef(id, obj)
	}
}

// pathString returns a string of the form a->b-> ... ->g for a path [a, b, ... g].
func pathString(path []Object) string {
	var s string
	for i, p := range path {
		if i > 0 {
			s += "->"
		}
		s += p.Name()
	}
	return s
}

// objDecl type-checks the declaration of obj in its respective (file) context.
// For the meaning of def, see Checker.definedType, in typexpr.go.
func (check *Checker) objDecl(obj Object, def *Named) {
	if trace && obj.Type() == nil {
		if check.indent == 0 {
			fmt.Println() // empty line between top-level objects for readability
		}
		check.trace(obj.Pos(), "-- checking %s (%s, objPath = %s)", obj, obj.color(), pathString(check.objPath))
		check.indent++
		defer func() {
			check.indent--
			check.trace(obj.Pos(), "=> %s (%s)", obj, obj.color())
		}()
	}

	// Checking the declaration of obj means inferring its type
	// (and possibly its value, for constants).
	// An object's type (and thus the object) may be in one of
	// three states which are expressed by colors:
	//
	// - an object whose type is not yet known is painted white (initial color)
	// - an object whose type is in the process of being inferred is painted grey
	// - an object whose type is fully inferred is painted black
	//
	// During type inference, an object's color changes from white to grey
	// to black (pre-declared objects are painted black from the start).
	// A black object (i.e., its type) can only depend on (refer to) other black
	// ones. White and grey objects may depend on white and black objects.
	// A dependency on a grey object indicates a cycle which may or may not be
	// valid.
	//
	// When objects turn grey, they are pushed on the object path (a stack);
	// they are popped again when they turn black. Thus, if a grey object (a
	// cycle) is encountered, it is on the object path, and all the objects
	// it depends on are the remaining objects on that path. Color encoding
	// is such that the color value of a grey object indicates the index of
	// that object in the object path.

	// During type-checking, white objects may be assigned a type without
	// traversing through objDecl; e.g., when initializing constants and
	// variables. Update the colors of those objects here (rather than
	// everywhere where we set the type) to satisfy the color invariants.
	if obj.color() == white && obj.Type() != nil {
		obj.setColor(black)
		return
	}

	switch obj.color() {
	case white:
		assert(obj.Type() == nil)
		// All color values other than white and black are considered grey.
		// Because black and white are < grey, all values >= grey are grey.
		// Use those values to encode the object's index into the object path.
		obj.setColor(grey + color(check.push(obj)))
		defer func() {
			check.pop().setColor(black)
		}()

	case black:
		assert(obj.Type() != nil)
		return

	default:
		// Color values other than white or black are considered grey.
		fallthrough

	case grey:
		// We have a cycle.
		// In the existing code, this is marked by a non-nil type
		// for the object except for constants and variables whose
		// type may be non-nil (known), or nil if it depends on the
		// not-yet known initialization value.
		// In the former case, set the type to Typ[Invalid] because
		// we have an initialization cycle. The cycle error will be
		// reported later, when determining initialization order.
		// TODO(gri) Report cycle here and simplify initialization
		// order code.
		switch obj := obj.(type) {
		case *Const:
			if check.cycle(obj) || obj.typ == nil {
				obj.typ = Typ[Invalid]
			}

		case *Var:
			if check.cycle(obj) || obj.typ == nil {
				obj.typ = Typ[Invalid]
			}

		case *TypeName:
			if check.cycle(obj) {
				// break cycle
				// (without this, calling underlying()
				// below may lead to an endless loop
				// if we have a cycle for a defined
				// (*Named) type)
				obj.typ = Typ[Invalid]
			}

		case *Func:
			if check.cycle(obj) {
				// Don't set obj.typ to Typ[Invalid] here
				// because plenty of code type-asserts that
				// functions have a *Signature type. Grey
				// functions have their type set to an empty
				// signature which makes it impossible to
				// initialize a variable with the function.
			}

		default:
			unreachable()
		}
		assert(obj.Type() != nil)
		return
	}

	d := check.objMap[obj]
	if d == nil {
		check.dump("%v: %s should have been declared", obj.Pos(), obj)
		unreachable()
	}

	// save/restore current context and setup object context
	defer func(ctxt context) {
		check.context = ctxt
	}(check.context)
	check.context = context{
		scope: d.file,
	}

	// Const and var declarations must not have initialization
	// cycles. We track them by remembering the current declaration
	// in check.decl. Initialization expressions depending on other
	// consts, vars, or functions, add dependencies to the current
	// check.decl.
	switch obj := obj.(type) {
	case *Const:
		check.decl = d // new package-level const decl
		check.constDecl(obj, d.vtyp, d.init, d.inherited)
	case *Var:
		check.decl = d // new package-level var decl
		check.varDecl(obj, d.lhs, d.vtyp, d.init)
	case *TypeName:
		// invalid recursive types are detected via path
		check.typeDecl(obj, d.tdecl, def)
		check.collectMethods(obj) // methods can only be added to top-level types
	case *Func:
		// functions may be recursive - no need to track dependencies
		check.funcDecl(obj, d)
	default:
		unreachable()
	}
}

// cycle checks if the cycle starting with obj is valid and
// reports an error if it is not.
func (check *Checker) cycle(obj Object) (isCycle bool) {
	// The object map contains the package scope objects and the non-interface methods.
	if debug {
		info := check.objMap[obj]
		inObjMap := info != nil && (info.fdecl == nil || info.fdecl.Recv == nil) // exclude methods
		isPkgObj := obj.Parent() == check.pkg.scope
		if isPkgObj != inObjMap {
			check.dump("%v: inconsistent object map for %s (isPkgObj = %v, inObjMap = %v)", obj.Pos(), obj, isPkgObj, inObjMap)
			unreachable()
		}
	}

	// Count cycle objects.
	assert(obj.color() >= grey)
	start := obj.color() - grey // index of obj in objPath
	cycle := check.objPath[start:]
	nval := 0 // number of (constant or variable) values in the cycle
	ndef := 0 // number of type definitions in the cycle
	for _, obj := range cycle {
		switch obj := obj.(type) {
		case *Const, *Var:
			nval++
		case *TypeName:
			// Determine if the type name is an alias or not. For
			// package-level objects, use the object map which
			// provides syntactic information (which doesn't rely
			// on the order in which the objects are set up). For
			// local objects, we can rely on the order, so use
			// the object's predicate.
			// TODO(gri) It would be less fragile to always access
			// the syntactic information. We should consider storing
			// this information explicitly in the object.
			var alias bool
			if d := check.objMap[obj]; d != nil {
				alias = d.tdecl.Assign.IsValid() // package-level object
			} else {
				alias = obj.IsAlias() // function local object
			}
			if !alias {
				ndef++
			}
		case *Func:
			// ignored for now
		default:
			unreachable()
		}
	}

	if trace {
		check.trace(obj.Pos(), "## cycle detected: objPath = %s->%s (len = %d)", pathString(cycle), obj.Name(), len(cycle))
		check.trace(obj.Pos(), "## cycle contains: %d values, %d type definitions", nval, ndef)
		defer func() {
			if isCycle {
				check.trace(obj.Pos(), "=> error: cycle is invalid")
			}
		}()
	}

	// A cycle involving only constants and variables is invalid but we
	// ignore them here because they are reported via the initialization
	// cycle check.
	if nval == len(cycle) {
		return false
	}

	// A cycle involving only types (and possibly functions) must have at least
	// one type definition to be permitted: If there is no type definition, we
	// have a sequence of alias type names which will expand ad infinitum.
	if nval == 0 && ndef > 0 {
		return false // cycle is permitted
	}

	check.cycleError(cycle)

	return true
}

type typeInfo uint

// validType verifies that the given type does not "expand" infinitely
// producing a cycle in the type graph. Cycles are detected by marking
// defined types.
// (Cycles involving alias types, as in "type A = [10]A" are detected
// earlier, via the objDecl cycle detection mechanism.)
func (check *Checker) validType(typ Type, path []Object) typeInfo {
	const (
		unknown typeInfo = iota
		marked
		valid
		invalid
	)

	switch t := typ.(type) {
	case *Array:
		return check.validType(t.elem, path)

	case *Struct:
		for _, f := range t.fields {
			if check.validType(f.typ, path) == invalid {
				return invalid
			}
		}

	case *Interface:
		for _, etyp := range t.embeddeds {
			if check.validType(etyp, path) == invalid {
				return invalid
			}
		}

	case *Named:
		t.expand(check.env)
		// don't touch the type if it is from a different package or the Universe scope
		// (doing so would lead to a race condition - was issue #35049)
		if t.obj.pkg != check.pkg {
			return valid
		}

		// don't report a 2nd error if we already know the type is invalid
		// (e.g., if a cycle was detected earlier, via under).
		if t.underlying == Typ[Invalid] {
			t.info = invalid
			return invalid
		}

		switch t.info {
		case unknown:
			t.info = marked
			t.info = check.validType(t.fromRHS, append(path, t.obj)) // only types of current package added to path
		case marked:
			// cycle detected
			for i, tn := range path {
				if t.obj.pkg != check.pkg {
					panic("type cycle via package-external type")
				}
				if tn == t.obj {
					check.cycleError(path[i:])
					t.info = invalid
					return t.info
				}
			}
			panic("cycle start not found")
		}
		return t.info
	}

	return valid
}

// cycleError reports a declaration cycle starting with
// the object in cycle that is "first" in the source.
func (check *Checker) cycleError(cycle []Object) {
	// TODO(gri) Should we start with the last (rather than the first) object in the cycle
	//           since that is the earliest point in the source where we start seeing the
	//           cycle? That would be more consistent with other error messages.
	i := firstInSrc(cycle)
	obj := cycle[i]
	check.errorf(obj, _InvalidDeclCycle, "illegal cycle in declaration of %s", obj.Name())
	for range cycle {
		check.errorf(obj, _InvalidDeclCycle, "\t%s refers to", obj.Name()) // secondary error, \t indented
		i++
		if i >= len(cycle) {
			i = 0
		}
		obj = cycle[i]
	}
	check.errorf(obj, _InvalidDeclCycle, "\t%s", obj.Name())
}

// firstInSrc reports the index of the object with the "smallest"
// source position in path. path must not be empty.
func firstInSrc(path []Object) int {
	fst, pos := 0, path[0].Pos()
	for i, t := range path[1:] {
		if t.Pos() < pos {
			fst, pos = i+1, t.Pos()
		}
	}
	return fst
}

type (
	decl interface {
		node() ast.Node
	}

	importDecl struct{ spec *ast.ImportSpec }
	constDecl  struct {
		spec      *ast.ValueSpec
		iota      int
		typ       ast.Expr
		init      []ast.Expr
		inherited bool
	}
	varDecl  struct{ spec *ast.ValueSpec }
	typeDecl struct{ spec *ast.TypeSpec }
	funcDecl struct{ decl *ast.FuncDecl }
)

func (d importDecl) node() ast.Node { return d.spec }
func (d constDecl) node() ast.Node  { return d.spec }
func (d varDecl) node() ast.Node    { return d.spec }
func (d typeDecl) node() ast.Node   { return d.spec }
func (d funcDecl) node() ast.Node   { return d.decl }

func (check *Checker) walkDecls(decls []ast.Decl, f func(decl)) {
	for _, d := range decls {
		check.walkDecl(d, f)
	}
}

func (check *Checker) walkDecl(d ast.Decl, f func(decl)) {
	switch d := d.(type) {
	case *ast.BadDecl:
		// ignore
	case *ast.GenDecl:
		var last *ast.ValueSpec // last ValueSpec with type or init exprs seen
		for iota, s := range d.Specs {
			switch s := s.(type) {
			case *ast.ImportSpec:
				f(importDecl{s})
			case *ast.ValueSpec:
				switch d.Tok {
				case token.CONST:
					// determine which initialization expressions to use
					inherited := true
					switch {
					case s.Type != nil || len(s.Values) > 0:
						last = s
						inherited = false
					case last == nil:
						last = new(ast.ValueSpec) // make sure last exists
						inherited = false
					}
					check.arityMatch(s, last)
					f(constDecl{spec: s, iota: iota, typ: last.Type, init: last.Values, inherited: inherited})
				case token.VAR:
					check.arityMatch(s, nil)
					f(varDecl{s})
				default:
					check.invalidAST(s, "invalid token %s", d.Tok)
				}
			case *ast.TypeSpec:
				f(typeDecl{s})
			default:
				check.invalidAST(s, "unknown ast.Spec node %T", s)
			}
		}
	case *ast.FuncDecl:
		f(funcDecl{d})
	default:
		check.invalidAST(d, "unknown ast.Decl node %T", d)
	}
}

func (check *Checker) constDecl(obj *Const, typ, init ast.Expr, inherited bool) {
	assert(obj.typ == nil)

	// use the correct value of iota
	defer func(iota constant.Value, errpos positioner) {
		check.iota = iota
		check.errpos = errpos
	}(check.iota, check.errpos)
	check.iota = obj.val
	check.errpos = nil

	// provide valid constant value under all circumstances
	obj.val = constant.MakeUnknown()

	// determine type, if any
	if typ != nil {
		t := check.typ(typ)
		if !isConstType(t) {
			// don't report an error if the type is an invalid C (defined) type
			// (issue #22090)
			if under(t) != Typ[Invalid] {
				check.errorf(typ, _InvalidConstType, "invalid constant type %s", t)
			}
			obj.typ = Typ[Invalid]
			return
		}
		obj.typ = t
	}

	// check initialization
	var x operand
	if init != nil {
		if inherited {
			// The initialization expression is inherited from a previous
			// constant declaration, and (error) positions refer to that
			// expression and not the current constant declaration. Use
			// the constant identifier position for any errors during
			// init expression evaluation since that is all we have
			// (see issues #42991, #42992).
			check.errpos = atPos(obj.pos)
		}
		check.expr(&x, init)
	}
	check.initConst(obj, &x)
}

func (check *Checker) varDecl(obj *Var, lhs []*Var, typ, init ast.Expr) {
	assert(obj.typ == nil)

	// determine type, if any
	if typ != nil {
		obj.typ = check.varType(typ)
		// We cannot spread the type to all lhs variables if there
		// are more than one since that would mark them as checked
		// (see Checker.objDecl) and the assignment of init exprs,
		// if any, would not be checked.
		//
		// TODO(gri) If we have no init expr, we should distribute
		// a given type otherwise we need to re-evalate the type
		// expr for each lhs variable, leading to duplicate work.
	}

	// check initialization
	if init == nil {
		if typ == nil {
			// error reported before by arityMatch
			obj.typ = Typ[Invalid]
		}
		return
	}

	if lhs == nil || len(lhs) == 1 {
		assert(lhs == nil || lhs[0] == obj)
		var x operand
		check.expr(&x, init)
		check.initVar(obj, &x, "variable declaration")
		return
	}

	if debug {
		// obj must be one of lhs
		found := false
		for _, lhs := range lhs {
			if obj == lhs {
				found = true
				break
			}
		}
		if !found {
			panic("inconsistent lhs")
		}
	}

	// We have multiple variables on the lhs and one init expr.
	// Make sure all variables have been given the same type if
	// one was specified, otherwise they assume the type of the
	// init expression values (was issue #15755).
	if typ != nil {
		for _, lhs := range lhs {
			lhs.typ = obj.typ
		}
	}

	check.initVars(lhs, []ast.Expr{init}, token.NoPos)
}

// isImportedConstraint reports whether typ is an imported type constraint.
func (check *Checker) isImportedConstraint(typ Type) bool {
	named, _ := typ.(*Named)
	if named == nil || named.obj.pkg == check.pkg || named.obj.pkg == nil {
		return false
	}
	u, _ := named.under().(*Interface)
	return u != nil && u.IsConstraint()
}

func (check *Checker) typeDecl(obj *TypeName, tdecl *ast.TypeSpec, def *Named) {
	assert(obj.typ == nil)

	var rhs Type
	check.later(func() {
		check.validType(obj.typ, nil)
		// If typ is local, an error was already reported where typ is specified/defined.
		if check.isImportedConstraint(rhs) && !check.allowVersion(check.pkg, 1, 18) {
			check.errorf(tdecl.Type, _Todo, "using type constraint %s requires go1.18 or later", rhs)
		}
	})

	alias := tdecl.Assign.IsValid()
	if alias && tdecl.TParams.NumFields() != 0 {
		// The parser will ensure this but we may still get an invalid AST.
		// Complain and continue as regular type definition.
		check.error(atPos(tdecl.Assign), 0, "generic type cannot be alias")
		alias = false
	}

	// alias declaration
	if alias {
		if !check.allowVersion(check.pkg, 1, 9) {
			check.errorf(atPos(tdecl.Assign), _BadDecl, "type aliases requires go1.9 or later")
		}

		obj.typ = Typ[Invalid]
		rhs = check.varType(tdecl.Type)
		obj.typ = rhs
		return
	}

	// type definition or generic type declaration
	named := check.newNamed(obj, nil, nil, nil, nil)
	def.setUnderlying(named)

	if tdecl.TParams != nil {
		check.openScope(tdecl, "type parameters")
		defer check.closeScope()
		named.tparams = check.collectTypeParams(tdecl.TParams)
	}

	// determine underlying type of named
	rhs = check.definedType(tdecl.Type, named)
	assert(rhs != nil)
	named.fromRHS = rhs

	// The underlying type of named may be itself a named type that is
	// incomplete:
	//
	//	type (
	//		A B
	//		B *C
	//		C A
	//	)
	//
	// The type of C is the (named) type of A which is incomplete,
	// and which has as its underlying type the named type B.
	// Determine the (final, unnamed) underlying type by resolving
	// any forward chain.
	// TODO(gri) Investigate if we can just use named.fromRHS here
	//           and rely on lazy computation of the underlying type.
	named.underlying = under(named)

	// If the RHS is a type parameter, it must be from this type declaration.
	if tpar, _ := named.underlying.(*TypeParam); tpar != nil && tparamIndex(named.TParams().list(), tpar) < 0 {
		check.errorf(tdecl.Type, _Todo, "cannot use function type parameter %s as RHS in type declaration", tpar)
		named.underlying = Typ[Invalid]
	}
}

func (check *Checker) collectTypeParams(list *ast.FieldList) *TParamList {
	var tparams []*TypeParam
	// Declare type parameters up-front, with empty interface as type bound.
	// The scope of type parameters starts at the beginning of the type parameter
	// list (so we can have mutually recursive parameterized interfaces).
	for _, f := range list.List {
		tparams = check.declareTypeParams(tparams, f.Names)
	}

	index := 0
	var bound Type
	for _, f := range list.List {
		if f.Type == nil {
			goto next
		}
		bound = check.boundType(f.Type)
		for i := range f.Names {
			tparams[index+i].bound = bound
		}

	next:
		index += len(f.Names)
	}

	return bindTParams(tparams)
}

func (check *Checker) declareTypeParams(tparams []*TypeParam, names []*ast.Ident) []*TypeParam {
	for _, name := range names {
		tname := NewTypeName(name.Pos(), check.pkg, name.Name, nil)
		tpar := check.newTypeParam(tname, &emptyInterface)       // assigns type to tpar as a side-effect
		check.declare(check.scope, name, tname, check.scope.pos) // TODO(gri) check scope position
		tparams = append(tparams, tpar)
	}

	if trace && len(names) > 0 {
		check.trace(names[0].Pos(), "type params = %v", tparams[len(tparams)-len(names):])
	}

	return tparams
}

// boundType type-checks the type expression e and returns its type, or Typ[Invalid].
// The type must be an interface, including the predeclared type "any".
func (check *Checker) boundType(e ast.Expr) Type {
	// The predeclared identifier "any" is visible only as a type bound in a type parameter list.
	// If we allow "any" for general use, this if-statement can be removed (issue #33232).
	if name, _ := unparen(e).(*ast.Ident); name != nil && name.Name == "any" && check.lookup("any") == universeAny {
		return universeAny.Type()
	}

	bound := check.typ(e)
	check.later(func() {
		u := under(bound)
		if _, ok := u.(*Interface); !ok && u != Typ[Invalid] {
			check.errorf(e, _Todo, "%s is not an interface", bound)
		}
	})
	return bound
}

func (check *Checker) collectMethods(obj *TypeName) {
	// get associated methods
	// (Checker.collectObjects only collects methods with non-blank names;
	// Checker.resolveBaseTypeName ensures that obj is not an alias name
	// if it has attached methods.)
	methods := check.methods[obj]
	if methods == nil {
		return
	}
	delete(check.methods, obj)
	assert(!check.objMap[obj].tdecl.Assign.IsValid()) // don't use TypeName.IsAlias (requires fully set up object)

	// use an objset to check for name conflicts
	var mset objset

	// spec: "If the base type is a struct type, the non-blank method
	// and field names must be distinct."
	base := asNamed(obj.typ) // shouldn't fail but be conservative
	if base != nil {
		u := safeUnderlying(base) // base should be expanded, but use safeUnderlying to be conservative
		if t, _ := u.(*Struct); t != nil {
			for _, fld := range t.fields {
				if fld.name != "_" {
					assert(mset.insert(fld) == nil)
				}
			}
		}

		// Checker.Files may be called multiple times; additional package files
		// may add methods to already type-checked types. Add pre-existing methods
		// so that we can detect redeclarations.
		for _, m := range base.methods {
			assert(m.name != "_")
			assert(mset.insert(m) == nil)
		}
	}

	// add valid methods
	for _, m := range methods {
		// spec: "For a base type, the non-blank names of methods bound
		// to it must be unique."
		assert(m.name != "_")
		if alt := mset.insert(m); alt != nil {
			switch alt.(type) {
			case *Var:
				check.errorf(m, _DuplicateFieldAndMethod, "field and method with the same name %s", m.name)
			case *Func:
				check.errorf(m, _DuplicateMethod, "method %s already declared for %s", m.name, obj)
			default:
				unreachable()
			}
			check.reportAltDecl(alt)
			continue
		}

		if base != nil {
			base.load() // TODO(mdempsky): Probably unnecessary.
			base.methods = append(base.methods, m)
		}
	}
}

func (check *Checker) funcDecl(obj *Func, decl *declInfo) {
	assert(obj.typ == nil)

	// func declarations cannot use iota
	assert(check.iota == nil)

	sig := new(Signature)
	obj.typ = sig // guard against cycles

	// Avoid cycle error when referring to method while type-checking the signature.
	// This avoids a nuisance in the best case (non-parameterized receiver type) and
	// since the method is not a type, we get an error. If we have a parameterized
	// receiver type, instantiating the receiver type leads to the instantiation of
	// its methods, and we don't want a cycle error in that case.
	// TODO(gri) review if this is correct and/or whether we still need this?
	saved := obj.color_
	obj.color_ = black
	fdecl := decl.fdecl
	check.funcType(sig, fdecl.Recv, fdecl.Type)
	obj.color_ = saved

	if fdecl.Type.TParams.NumFields() > 0 && fdecl.Body == nil {
		check.softErrorf(fdecl.Name, _Todo, "parameterized function is missing function body")
	}

	// function body must be type-checked after global declarations
	// (functions implemented elsewhere have no body)
	if !check.conf.IgnoreFuncBodies && fdecl.Body != nil {
		check.later(func() {
			check.funcBody(decl, obj.name, sig, fdecl.Body, nil)
		})
	}
}

func (check *Checker) declStmt(d ast.Decl) {
	pkg := check.pkg

	check.walkDecl(d, func(d decl) {
		switch d := d.(type) {
		case constDecl:
			top := len(check.delayed)

			// declare all constants
			lhs := make([]*Const, len(d.spec.Names))
			for i, name := range d.spec.Names {
				obj := NewConst(name.Pos(), pkg, name.Name, nil, constant.MakeInt64(int64(d.iota)))
				lhs[i] = obj

				var init ast.Expr
				if i < len(d.init) {
					init = d.init[i]
				}

				check.constDecl(obj, d.typ, init, d.inherited)
			}

			// process function literals in init expressions before scope changes
			check.processDelayed(top)

			// spec: "The scope of a constant or variable identifier declared
			// inside a function begins at the end of the ConstSpec or VarSpec
			// (ShortVarDecl for short variable declarations) and ends at the
			// end of the innermost containing block."
			scopePos := d.spec.End()
			for i, name := range d.spec.Names {
				check.declare(check.scope, name, lhs[i], scopePos)
			}

		case varDecl:
			top := len(check.delayed)

			lhs0 := make([]*Var, len(d.spec.Names))
			for i, name := range d.spec.Names {
				lhs0[i] = NewVar(name.Pos(), pkg, name.Name, nil)
			}

			// initialize all variables
			for i, obj := range lhs0 {
				var lhs []*Var
				var init ast.Expr
				switch len(d.spec.Values) {
				case len(d.spec.Names):
					// lhs and rhs match
					init = d.spec.Values[i]
				case 1:
					// rhs is expected to be a multi-valued expression
					lhs = lhs0
					init = d.spec.Values[0]
				default:
					if i < len(d.spec.Values) {
						init = d.spec.Values[i]
					}
				}
				check.varDecl(obj, lhs, d.spec.Type, init)
				if len(d.spec.Values) == 1 {
					// If we have a single lhs variable we are done either way.
					// If we have a single rhs expression, it must be a multi-
					// valued expression, in which case handling the first lhs
					// variable will cause all lhs variables to have a type
					// assigned, and we are done as well.
					if debug {
						for _, obj := range lhs0 {
							assert(obj.typ != nil)
						}
					}
					break
				}
			}

			// process function literals in init expressions before scope changes
			check.processDelayed(top)

			// declare all variables
			// (only at this point are the variable scopes (parents) set)
			scopePos := d.spec.End() // see constant declarations
			for i, name := range d.spec.Names {
				// see constant declarations
				check.declare(check.scope, name, lhs0[i], scopePos)
			}

		case typeDecl:
			obj := NewTypeName(d.spec.Name.Pos(), pkg, d.spec.Name.Name, nil)
			// spec: "The scope of a type identifier declared inside a function
			// begins at the identifier in the TypeSpec and ends at the end of
			// the innermost containing block."
			scopePos := d.spec.Name.Pos()
			check.declare(check.scope, d.spec.Name, obj, scopePos)
			// mark and unmark type before calling typeDecl; its type is still nil (see Checker.objDecl)
			obj.setColor(grey + color(check.push(obj)))
			check.typeDecl(obj, d.spec, nil)
			check.pop().setColor(black)
		default:
			check.invalidAST(d.node(), "unknown ast.Decl node %T", d.node())
		}
	})
}
