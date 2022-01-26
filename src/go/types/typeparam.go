// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"go/token"
	"sync/atomic"
)

// Note: This is a uint32 rather than a uint64 because the
// respective 64 bit atomic instructions are not available
// on all platforms.
var lastID uint32

// nextID returns a value increasing monotonically by 1 with
// each call, starting with 1. It may be called concurrently.
func nextID() uint64 { return uint64(atomic.AddUint32(&lastID, 1)) }

// A TypeParam represents a type parameter type.
type TypeParam struct {
	check *Checker  // for lazy type bound completion
	id    uint64    // unique id, for debugging only
	obj   *TypeName // corresponding type name
	index int       // type parameter index in source order, starting at 0
	// TODO(rfindley): this could also be Typ[Invalid]. Verify that this is handled correctly.
	bound Type // *Named or *Interface; underlying type is always *Interface
}

// NewTypeParam returns a new TypeParam. Type parameters may be set on a Named
// or Signature type by calling SetTypeParams. Setting a type parameter on more
// than one type will result in a panic.
//
// The constraint argument can be nil, and set later via SetConstraint.
func NewTypeParam(obj *TypeName, constraint Type) *TypeParam {
	return (*Checker)(nil).newTypeParam(obj, constraint)
}

func (check *Checker) newTypeParam(obj *TypeName, constraint Type) *TypeParam {
	// Always increment lastID, even if it is not used.
	id := nextID()
	if check != nil {
		check.nextID++
		id = check.nextID
	}
	typ := &TypeParam{check: check, id: id, obj: obj, index: -1, bound: constraint}
	if obj.typ == nil {
		obj.typ = typ
	}
	return typ
}

// TODO(rfindley): remove or export these placeholder APIs.

// Index returns the index of the type param within its param list.
func (t *TypeParam) _Index() int {
	return t.index
}

// SetId sets the unique id of a type param. Should only be used for type params
// in imported generic types.
func (t *TypeParam) _SetId(id uint64) {
	t.id = id
}

// Obj returns the type name for t.
func (t *TypeParam) Obj() *TypeName { return t.obj }

// Constraint returns the type constraint specified for t.
func (t *TypeParam) Constraint() Type {
	// compute the type set if possible (we may not have an interface)
	if iface, _ := under(t.bound).(*Interface); iface != nil {
		// use the type bound position if we have one
		pos := token.NoPos
		if n, _ := t.bound.(*Named); n != nil {
			pos = n.obj.pos
		}
		computeInterfaceTypeSet(t.check, pos, iface)
	}
	return t.bound
}

// SetConstraint sets the type constraint for t.
func (t *TypeParam) SetConstraint(bound Type) {
	if bound == nil {
		panic("nil constraint")
	}
	t.bound = bound
}

func (t *TypeParam) Underlying() Type { return t }
func (t *TypeParam) String() string   { return TypeString(t, nil) }

// ----------------------------------------------------------------------------
// Implementation

// iface returns the constraint interface of t.
func (t *TypeParam) iface() *Interface {
	if iface, _ := under(t.Constraint()).(*Interface); iface != nil {
		return iface
	}
	return &emptyInterface
}

// structuralType returns the structural type of the type parameter's constraint; or nil.
func (t *TypeParam) structuralType() Type {
	return t.iface().typeSet().structuralType()
}

func (t *TypeParam) is(f func(*term) bool) bool {
	return t.iface().typeSet().is(f)
}

func (t *TypeParam) underIs(f func(Type) bool) bool {
	return t.iface().typeSet().underIs(f)
}
