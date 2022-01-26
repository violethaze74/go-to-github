// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements type-checking of identifiers and type expressions.

package types2

import (
	"cmd/compile/internal/syntax"
	"fmt"
	"go/constant"
	"strings"
)

// ident type-checks identifier e and initializes x with the value or type of e.
// If an error occurred, x.mode is set to invalid.
// For the meaning of def, see Checker.definedType, below.
// If wantType is set, the identifier e is expected to denote a type.
//
func (check *Checker) ident(x *operand, e *syntax.Name, def *Named, wantType bool) {
	x.mode = invalid
	x.expr = e

	// Note that we cannot use check.lookup here because the returned scope
	// may be different from obj.Parent(). See also Scope.LookupParent doc.
	scope, obj := check.scope.LookupParent(e.Value, check.pos)
	switch obj {
	case nil:
		if e.Value == "_" {
			check.error(e, "cannot use _ as value or type")
		} else {
			if check.conf.CompilerErrorMessages {
				check.errorf(e, "undefined: %s", e.Value)
			} else {
				check.errorf(e, "undeclared name: %s", e.Value)
			}
		}
		return
	case universeAny, universeComparable:
		// complain if necessary but keep going
		if !check.allowVersion(check.pkg, 1, 18) {
			check.softErrorf(e, "undeclared name: %s (requires version go1.18 or later)", e.Value)
		} else if obj == universeAny {
			// If we allow "any" for general use, this if-statement can be removed (issue #33232).
			check.softErrorf(e, "cannot use any outside constraint position")
		}
	}
	check.recordUse(e, obj)

	// Type-check the object.
	// Only call Checker.objDecl if the object doesn't have a type yet
	// (in which case we must actually determine it) or the object is a
	// TypeName and we also want a type (in which case we might detect
	// a cycle which needs to be reported). Otherwise we can skip the
	// call and avoid a possible cycle error in favor of the more
	// informative "not a type/value" error that this function's caller
	// will issue (see issue #25790).
	typ := obj.Type()
	if _, gotType := obj.(*TypeName); typ == nil || gotType && wantType {
		check.objDecl(obj, def)
		typ = obj.Type() // type must have been assigned by Checker.objDecl
	}
	assert(typ != nil)

	// The object may have been dot-imported.
	// If so, mark the respective package as used.
	// (This code is only needed for dot-imports. Without them,
	// we only have to mark variables, see *Var case below).
	if pkgName := check.dotImportMap[dotImportKey{scope, obj.Name()}]; pkgName != nil {
		pkgName.used = true
	}

	switch obj := obj.(type) {
	case *PkgName:
		check.errorf(e, "use of package %s not in selector", obj.name)
		return

	case *Const:
		check.addDeclDep(obj)
		if typ == Typ[Invalid] {
			return
		}
		if obj == universeIota {
			if check.iota == nil {
				check.error(e, "cannot use iota outside constant declaration")
				return
			}
			x.val = check.iota
		} else {
			x.val = obj.val
		}
		assert(x.val != nil)
		x.mode = constant_

	case *TypeName:
		x.mode = typexpr

	case *Var:
		// It's ok to mark non-local variables, but ignore variables
		// from other packages to avoid potential race conditions with
		// dot-imported variables.
		if obj.pkg == check.pkg {
			obj.used = true
		}
		check.addDeclDep(obj)
		if typ == Typ[Invalid] {
			return
		}
		x.mode = variable

	case *Func:
		check.addDeclDep(obj)
		x.mode = value

	case *Builtin:
		x.id = obj.id
		x.mode = builtin

	case *Nil:
		x.mode = nilvalue

	default:
		unreachable()
	}

	x.typ = typ
}

// typ type-checks the type expression e and returns its type, or Typ[Invalid].
// The type must not be an (uninstantiated) generic type.
func (check *Checker) typ(e syntax.Expr) Type {
	return check.definedType(e, nil)
}

// varType type-checks the type expression e and returns its type, or Typ[Invalid].
// The type must not be an (uninstantiated) generic type and it must be ordinary
// (see ordinaryType).
func (check *Checker) varType(e syntax.Expr) Type {
	typ := check.definedType(e, nil)
	check.ordinaryType(syntax.StartPos(e), typ)
	return typ
}

// ordinaryType reports an error if typ is an interface type containing
// type lists or is (or embeds) the predeclared type comparable.
func (check *Checker) ordinaryType(pos syntax.Pos, typ Type) {
	// We don't want to call under() (via asInterface) or complete interfaces while we
	// are in the middle of type-checking parameter declarations that might belong to
	// interface methods. Delay this check to the end of type-checking.
	check.later(func() {
		if t := asInterface(typ); t != nil {
			tset := computeInterfaceTypeSet(check, pos, t) // TODO(gri) is this the correct position?
			if tset.IsConstraint() {
				if tset.comparable {
					check.softErrorf(pos, "interface is (or embeds) comparable")
				} else {
					check.softErrorf(pos, "interface contains type constraints")
				}
			}
		}
	})
}

// anyType type-checks the type expression e and returns its type, or Typ[Invalid].
// The type may be generic or instantiated.
func (check *Checker) anyType(e syntax.Expr) Type {
	typ := check.typInternal(e, nil)
	assert(isTyped(typ))
	check.recordTypeAndValue(e, typexpr, typ, nil)
	return typ
}

// definedType is like typ but also accepts a type name def.
// If def != nil, e is the type specification for the defined type def, declared
// in a type declaration, and def.underlying will be set to the type of e before
// any components of e are type-checked.
//
func (check *Checker) definedType(e syntax.Expr, def *Named) Type {
	typ := check.typInternal(e, def)
	assert(isTyped(typ))
	if isGeneric(typ) {
		check.errorf(e, "cannot use generic type %s without instantiation", typ)
		typ = Typ[Invalid]
	}
	check.recordTypeAndValue(e, typexpr, typ, nil)
	return typ
}

// genericType is like typ but the type must be an (uninstantiated) generic type.
func (check *Checker) genericType(e syntax.Expr, reportErr bool) Type {
	typ := check.typInternal(e, nil)
	assert(isTyped(typ))
	if typ != Typ[Invalid] && !isGeneric(typ) {
		if reportErr {
			check.errorf(e, "%s is not a generic type", typ)
		}
		typ = Typ[Invalid]
	}
	// TODO(gri) what is the correct call below?
	check.recordTypeAndValue(e, typexpr, typ, nil)
	return typ
}

// goTypeName returns the Go type name for typ and
// removes any occurrences of "types2." from that name.
func goTypeName(typ Type) string {
	return strings.Replace(fmt.Sprintf("%T", typ), "types2.", "", -1) // strings.ReplaceAll is not available in Go 1.4
}

// typInternal drives type checking of types.
// Must only be called by definedType or genericType.
//
func (check *Checker) typInternal(e0 syntax.Expr, def *Named) (T Type) {
	if check.conf.Trace {
		check.trace(e0.Pos(), "type %s", e0)
		check.indent++
		defer func() {
			check.indent--
			var under Type
			if T != nil {
				// Calling under() here may lead to endless instantiations.
				// Test case: type T[P any] *T[P]
				// TODO(gri) investigate if that's a bug or to be expected
				// (see also analogous comment in Checker.instantiate).
				under = safeUnderlying(T)
			}
			if T == under {
				check.trace(e0.Pos(), "=> %s // %s", T, goTypeName(T))
			} else {
				check.trace(e0.Pos(), "=> %s (under = %s) // %s", T, under, goTypeName(T))
			}
		}()
	}

	switch e := e0.(type) {
	case *syntax.BadExpr:
		// ignore - error reported before

	case *syntax.Name:
		var x operand
		check.ident(&x, e, def, true)

		switch x.mode {
		case typexpr:
			typ := x.typ
			def.setUnderlying(typ)
			return typ
		case invalid:
			// ignore - error reported before
		case novalue:
			check.errorf(&x, "%s used as type", &x)
		default:
			check.errorf(&x, "%s is not a type", &x)
		}

	case *syntax.SelectorExpr:
		var x operand
		check.selector(&x, e)

		switch x.mode {
		case typexpr:
			typ := x.typ
			def.setUnderlying(typ)
			return typ
		case invalid:
			// ignore - error reported before
		case novalue:
			check.errorf(&x, "%s used as type", &x)
		default:
			check.errorf(&x, "%s is not a type", &x)
		}

	case *syntax.IndexExpr:
		if !check.allowVersion(check.pkg, 1, 18) {
			check.softErrorf(e.Pos(), "type instantiation requires go1.18 or later")
		}
		return check.instantiatedType(e.X, unpackExpr(e.Index), def)

	case *syntax.ParenExpr:
		// Generic types must be instantiated before they can be used in any form.
		// Consequently, generic types cannot be parenthesized.
		return check.definedType(e.X, def)

	case *syntax.ArrayType:
		typ := new(Array)
		def.setUnderlying(typ)
		if e.Len != nil {
			typ.len = check.arrayLength(e.Len)
		} else {
			// [...]array
			check.error(e, "invalid use of [...] array (outside a composite literal)")
			typ.len = -1
		}
		typ.elem = check.varType(e.Elem)
		if typ.len >= 0 {
			return typ
		}
		// report error if we encountered [...]

	case *syntax.SliceType:
		typ := new(Slice)
		def.setUnderlying(typ)
		typ.elem = check.varType(e.Elem)
		return typ

	case *syntax.DotsType:
		// dots are handled explicitly where they are legal
		// (array composite literals and parameter lists)
		check.error(e, "invalid use of '...'")
		check.use(e.Elem)

	case *syntax.StructType:
		typ := new(Struct)
		def.setUnderlying(typ)
		check.structType(typ, e)
		return typ

	case *syntax.Operation:
		if e.Op == syntax.Mul && e.Y == nil {
			typ := new(Pointer)
			def.setUnderlying(typ)
			typ.base = check.varType(e.X)
			return typ
		}

		check.errorf(e0, "%s is not a type", e0)
		check.use(e0)

	case *syntax.FuncType:
		typ := new(Signature)
		def.setUnderlying(typ)
		check.funcType(typ, nil, nil, e)
		return typ

	case *syntax.InterfaceType:
		typ := new(Interface)
		def.setUnderlying(typ)
		if def != nil {
			typ.obj = def.obj
		}
		check.interfaceType(typ, e, def)
		return typ

	case *syntax.MapType:
		typ := new(Map)
		def.setUnderlying(typ)

		typ.key = check.varType(e.Key)
		typ.elem = check.varType(e.Value)

		// spec: "The comparison operators == and != must be fully defined
		// for operands of the key type; thus the key type must not be a
		// function, map, or slice."
		//
		// Delay this check because it requires fully setup types;
		// it is safe to continue in any case (was issue 6667).
		check.later(func() {
			if !Comparable(typ.key) {
				var why string
				if asTypeParam(typ.key) != nil {
					why = " (missing comparable constraint)"
				}
				check.errorf(e.Key, "invalid map key type %s%s", typ.key, why)
			}
		})

		return typ

	case *syntax.ChanType:
		typ := new(Chan)
		def.setUnderlying(typ)

		dir := SendRecv
		switch e.Dir {
		case 0:
			// nothing to do
		case syntax.SendOnly:
			dir = SendOnly
		case syntax.RecvOnly:
			dir = RecvOnly
		default:
			check.errorf(e, invalidAST+"unknown channel direction %d", e.Dir)
			// ok to continue
		}

		typ.dir = dir
		typ.elem = check.varType(e.Elem)
		return typ

	default:
		check.errorf(e0, "%s is not a type", e0)
		check.use(e0)
	}

	typ := Typ[Invalid]
	def.setUnderlying(typ)
	return typ
}

// typeOrNil type-checks the type expression (or nil value) e
// and returns the type of e, or nil. If e is a type, it must
// not be an (uninstantiated) generic type.
// If e is neither a type nor nil, typeOrNil returns Typ[Invalid].
// TODO(gri) should we also disallow non-var types?
func (check *Checker) typOrNil(e syntax.Expr) Type {
	var x operand
	check.rawExpr(&x, e, nil)
	switch x.mode {
	case invalid:
		// ignore - error reported before
	case novalue:
		check.errorf(&x, "%s used as type", &x)
	case typexpr:
		check.instantiatedOperand(&x)
		return x.typ
	case nilvalue:
		return nil
	default:
		check.errorf(&x, "%s is not a type", &x)
	}
	return Typ[Invalid]
}

func (check *Checker) instantiatedType(x syntax.Expr, targsx []syntax.Expr, def *Named) Type {
	gtyp := check.genericType(x, true)
	if gtyp == Typ[Invalid] {
		return gtyp // error already reported
	}
	base, _ := gtyp.(*Named)
	if base == nil {
		panic(fmt.Sprintf("%v: cannot instantiate %v", x.Pos(), gtyp))
	}

	// evaluate arguments
	targs := check.typeList(targsx)
	if targs == nil {
		def.setUnderlying(Typ[Invalid]) // avoid later errors due to lazy instantiation
		return Typ[Invalid]
	}

	// determine argument positions
	posList := make([]syntax.Pos, len(targs))
	for i, arg := range targsx {
		posList[i] = syntax.StartPos(arg)
	}

	typ := check.instantiate(x.Pos(), base, targs, posList)
	def.setUnderlying(typ)

	// make sure we check instantiation works at least once
	// and that the resulting type is valid
	check.later(func() {
		check.validType(typ, nil)
	})

	return typ
}

// arrayLength type-checks the array length expression e
// and returns the constant length >= 0, or a value < 0
// to indicate an error (and thus an unknown length).
func (check *Checker) arrayLength(e syntax.Expr) int64 {
	var x operand
	check.expr(&x, e)
	if x.mode != constant_ {
		if x.mode != invalid {
			check.errorf(&x, "array length %s must be constant", &x)
		}
		return -1
	}
	if isUntyped(x.typ) || isInteger(x.typ) {
		if val := constant.ToInt(x.val); val.Kind() == constant.Int {
			if representableConst(val, check, Typ[Int], nil) {
				if n, ok := constant.Int64Val(val); ok && n >= 0 {
					return n
				}
				check.errorf(&x, "invalid array length %s", &x)
				return -1
			}
		}
	}
	check.errorf(&x, "array length %s must be integer", &x)
	return -1
}

// typeList provides the list of types corresponding to the incoming expression list.
// If an error occurred, the result is nil, but all list elements were type-checked.
func (check *Checker) typeList(list []syntax.Expr) []Type {
	res := make([]Type, len(list)) // res != nil even if len(list) == 0
	for i, x := range list {
		t := check.varType(x)
		if t == Typ[Invalid] {
			res = nil
		}
		if res != nil {
			res[i] = t
		}
	}
	return res
}
