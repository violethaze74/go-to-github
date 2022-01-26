// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of index/slice expressions.

package types

import (
	"go/ast"
	"go/constant"
	"go/internal/typeparams"
)

// If e is a valid function instantiation, indexExpr returns true.
// In that case x represents the uninstantiated function value and
// it is the caller's responsibility to instantiate the function.
func (check *Checker) indexExpr(x *operand, e *typeparams.IndexExpr) (isFuncInst bool) {
	check.exprOrType(x, e.X, true)
	// x may be generic

	switch x.mode {
	case invalid:
		check.use(e.Indices...)
		return false

	case typexpr:
		// type instantiation
		x.mode = invalid
		// TODO(gri) here we re-evaluate e.X - try to avoid this
		x.typ = check.varType(e.Orig)
		if x.typ != Typ[Invalid] {
			x.mode = typexpr
		}
		return false

	case value:
		if sig := asSignature(x.typ); sig != nil && sig.TypeParams().Len() > 0 {
			// function instantiation
			return true
		}
	}

	// x should not be generic at this point, but be safe and check
	check.nonGeneric(x)
	if x.mode == invalid {
		return false
	}

	valid := false
	length := int64(-1) // valid if >= 0
	switch typ := under(x.typ).(type) {
	case *Basic:
		if isString(typ) {
			valid = true
			if x.mode == constant_ {
				length = int64(len(constant.StringVal(x.val)))
			}
			// an indexed string always yields a byte value
			// (not a constant) even if the string and the
			// index are constant
			x.mode = value
			x.typ = universeByte // use 'byte' name
		}

	case *Array:
		valid = true
		length = typ.len
		if x.mode != variable {
			x.mode = value
		}
		x.typ = typ.elem

	case *Pointer:
		if typ := asArray(typ.base); typ != nil {
			valid = true
			length = typ.len
			x.mode = variable
			x.typ = typ.elem
		}

	case *Slice:
		valid = true
		x.mode = variable
		x.typ = typ.elem

	case *Map:
		index := check.singleIndex(e)
		if index == nil {
			x.mode = invalid
			return false
		}
		var key operand
		check.expr(&key, index)
		check.assignment(&key, typ.key, "map index")
		// ok to continue even if indexing failed - map element type is known
		x.mode = mapindex
		x.typ = typ.elem
		x.expr = e.Orig
		return false

	case *TypeParam:
		// TODO(gri) report detailed failure cause for better error messages
		var tkey, telem Type // tkey != nil if we have maps
		if typ.underIs(func(u Type) bool {
			var key, elem Type
			alen := int64(-1) // valid if >= 0
			switch t := u.(type) {
			case *Basic:
				if !isString(t) {
					return false
				}
				elem = universeByte
			case *Array:
				elem = t.elem
				alen = t.len
			case *Pointer:
				a, _ := under(t.base).(*Array)
				if a == nil {
					return false
				}
				elem = a.elem
				alen = a.len
			case *Slice:
				elem = t.elem
			case *Map:
				key = t.key
				elem = t.elem
			default:
				return false
			}
			assert(elem != nil)
			if telem == nil {
				// first type
				tkey, telem = key, elem
				length = alen
			} else {
				// all map keys must be identical (incl. all nil)
				if !Identical(key, tkey) {
					return false
				}
				// all element types must be identical
				if !Identical(elem, telem) {
					return false
				}
				tkey, telem = key, elem
				// track the minimal length for arrays
				if alen >= 0 && alen < length {
					length = alen
				}
			}
			return true
		}) {
			// For maps, the index expression must be assignable to the map key type.
			if tkey != nil {
				index := check.singleIndex(e)
				if index == nil {
					x.mode = invalid
					return false
				}
				var key operand
				check.expr(&key, index)
				check.assignment(&key, tkey, "map index")
				// ok to continue even if indexing failed - map element type is known
				x.mode = mapindex
				x.typ = telem
				x.expr = e
				return false
			}

			// no maps
			valid = true
			x.mode = variable
			x.typ = telem
		}
	}

	if !valid {
		check.invalidOp(x, _NonIndexableOperand, "cannot index %s", x)
		x.mode = invalid
		return false
	}

	index := check.singleIndex(e)
	if index == nil {
		x.mode = invalid
		return false
	}

	// In pathological (invalid) cases (e.g.: type T1 [][[]T1{}[0][0]]T0)
	// the element type may be accessed before it's set. Make sure we have
	// a valid type.
	if x.typ == nil {
		x.typ = Typ[Invalid]
	}

	check.index(index, length)
	return false
}

func (check *Checker) sliceExpr(x *operand, e *ast.SliceExpr) {
	check.expr(x, e.X)
	if x.mode == invalid {
		check.use(e.Low, e.High, e.Max)
		return
	}

	valid := false
	length := int64(-1) // valid if >= 0
	switch typ := under(x.typ).(type) {
	case *Basic:
		if isString(typ) {
			if e.Slice3 {
				check.invalidOp(x, _InvalidSliceExpr, "3-index slice of string")
				x.mode = invalid
				return
			}
			valid = true
			if x.mode == constant_ {
				length = int64(len(constant.StringVal(x.val)))
			}
			// spec: "For untyped string operands the result
			// is a non-constant value of type string."
			if typ.kind == UntypedString {
				x.typ = Typ[String]
			}
		}

	case *Array:
		valid = true
		length = typ.len
		if x.mode != variable {
			check.invalidOp(x, _NonSliceableOperand, "cannot slice %s (value not addressable)", x)
			x.mode = invalid
			return
		}
		x.typ = &Slice{elem: typ.elem}

	case *Pointer:
		if typ := asArray(typ.base); typ != nil {
			valid = true
			length = typ.len
			x.typ = &Slice{elem: typ.elem}
		}

	case *Slice:
		valid = true
		// x.typ doesn't change

	case *TypeParam:
		check.errorf(x, _Todo, "generic slice expressions not yet implemented")
		x.mode = invalid
		return
	}

	if !valid {
		check.invalidOp(x, _NonSliceableOperand, "cannot slice %s", x)
		x.mode = invalid
		return
	}

	x.mode = value

	// spec: "Only the first index may be omitted; it defaults to 0."
	if e.Slice3 && (e.High == nil || e.Max == nil) {
		check.invalidAST(inNode(e, e.Rbrack), "2nd and 3rd index required in 3-index slice")
		x.mode = invalid
		return
	}

	// check indices
	var ind [3]int64
	for i, expr := range []ast.Expr{e.Low, e.High, e.Max} {
		x := int64(-1)
		switch {
		case expr != nil:
			// The "capacity" is only known statically for strings, arrays,
			// and pointers to arrays, and it is the same as the length for
			// those types.
			max := int64(-1)
			if length >= 0 {
				max = length + 1
			}
			if _, v := check.index(expr, max); v >= 0 {
				x = v
			}
		case i == 0:
			// default is 0 for the first index
			x = 0
		case length >= 0:
			// default is length (== capacity) otherwise
			x = length
		}
		ind[i] = x
	}

	// constant indices must be in range
	// (check.index already checks that existing indices >= 0)
L:
	for i, x := range ind[:len(ind)-1] {
		if x > 0 {
			for _, y := range ind[i+1:] {
				if y >= 0 && x > y {
					check.errorf(inNode(e, e.Rbrack), _SwappedSliceIndices, "swapped slice indices: %d > %d", x, y)
					break L // only report one error, ok to continue
				}
			}
		}
	}
}

// singleIndex returns the (single) index from the index expression e.
// If the index is missing, or if there are multiple indices, an error
// is reported and the result is nil.
func (check *Checker) singleIndex(expr *typeparams.IndexExpr) ast.Expr {
	if len(expr.Indices) == 0 {
		check.invalidAST(expr.Orig, "index expression %v with 0 indices", expr)
		return nil
	}
	if len(expr.Indices) > 1 {
		// TODO(rFindley) should this get a distinct error code?
		check.invalidOp(expr.Indices[1], _InvalidIndex, "more than one index")
	}
	return expr.Indices[0]
}

// index checks an index expression for validity.
// If max >= 0, it is the upper bound for index.
// If the result typ is != Typ[Invalid], index is valid and typ is its (possibly named) integer type.
// If the result val >= 0, index is valid and val is its constant int value.
func (check *Checker) index(index ast.Expr, max int64) (typ Type, val int64) {
	typ = Typ[Invalid]
	val = -1

	var x operand
	check.expr(&x, index)
	if !check.isValidIndex(&x, _InvalidIndex, "index", false) {
		return
	}

	if x.mode != constant_ {
		return x.typ, -1
	}

	if x.val.Kind() == constant.Unknown {
		return
	}

	v, ok := constant.Int64Val(x.val)
	assert(ok)
	if max >= 0 && v >= max {
		check.invalidArg(&x, _InvalidIndex, "index %s is out of bounds", &x)
		return
	}

	// 0 <= v [ && v < max ]
	return x.typ, v
}

func (check *Checker) isValidIndex(x *operand, code errorCode, what string, allowNegative bool) bool {
	if x.mode == invalid {
		return false
	}

	// spec: "a constant index that is untyped is given type int"
	check.convertUntyped(x, Typ[Int])
	if x.mode == invalid {
		return false
	}

	// spec: "the index x must be of integer type or an untyped constant"
	if !isInteger(x.typ) {
		check.invalidArg(x, code, "%s %s must be integer", what, x)
		return false
	}

	if x.mode == constant_ {
		// spec: "a constant index must be non-negative ..."
		if !allowNegative && constant.Sign(x.val) < 0 {
			check.invalidArg(x, code, "%s %s must not be negative", what, x)
			return false
		}

		// spec: "... and representable by a value of type int"
		if !representableConst(x.val, check, Typ[Int], &x.val) {
			check.invalidArg(x, code, "%s %s overflows int", what, x)
			return false
		}
	}

	return true
}

// indexElts checks the elements (elts) of an array or slice composite literal
// against the literal's element type (typ), and the element indices against
// the literal length if known (length >= 0). It returns the length of the
// literal (maximum index value + 1).
//
func (check *Checker) indexedElts(elts []ast.Expr, typ Type, length int64) int64 {
	visited := make(map[int64]bool, len(elts))
	var index, max int64
	for _, e := range elts {
		// determine and check index
		validIndex := false
		eval := e
		if kv, _ := e.(*ast.KeyValueExpr); kv != nil {
			if typ, i := check.index(kv.Key, length); typ != Typ[Invalid] {
				if i >= 0 {
					index = i
					validIndex = true
				} else {
					check.errorf(e, _InvalidLitIndex, "index %s must be integer constant", kv.Key)
				}
			}
			eval = kv.Value
		} else if length >= 0 && index >= length {
			check.errorf(e, _OversizeArrayLit, "index %d is out of bounds (>= %d)", index, length)
		} else {
			validIndex = true
		}

		// if we have a valid index, check for duplicate entries
		if validIndex {
			if visited[index] {
				check.errorf(e, _DuplicateLitKey, "duplicate index %d in array or slice literal", index)
			}
			visited[index] = true
		}
		index++
		if index > max {
			max = index
		}

		// check element against composite literal element type
		var x operand
		check.exprWithHint(&x, eval, typ)
		check.assignment(&x, typ, "array or slice literal")
	}
	return max
}
