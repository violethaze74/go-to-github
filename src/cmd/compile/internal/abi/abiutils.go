// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package abi

import (
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"fmt"
	"sync"
)

//......................................................................
//
// Public/exported bits of the ABI utilities.
//

// ABIParamResultInfo stores the results of processing a given
// function type to compute stack layout and register assignments. For
// each input and output parameter we capture whether the param was
// register-assigned (and to which register(s)) or the stack offset
// for the param if is not going to be passed in registers according
// to the rules in the Go internal ABI specification (1.17).
type ABIParamResultInfo struct {
	inparams          []ABIParamAssignment // Includes receiver for method calls.  Does NOT include hidden closure pointer.
	outparams         []ABIParamAssignment
	offsetToSpillArea int64
	spillAreaSize     int64
	config            *ABIConfig // to enable String() method
}

func (a *ABIParamResultInfo) InParams() []ABIParamAssignment {
	return a.inparams
}

func (a *ABIParamResultInfo) OutParams() []ABIParamAssignment {
	return a.outparams
}

func (a *ABIParamResultInfo) InParam(i int) ABIParamAssignment {
	return a.inparams[i]
}

func (a *ABIParamResultInfo) OutParam(i int) ABIParamAssignment {
	return a.outparams[i]
}

func (a *ABIParamResultInfo) SpillAreaOffset() int64 {
	return a.offsetToSpillArea
}

func (a *ABIParamResultInfo) SpillAreaSize() int64 {
	return a.spillAreaSize
}

// RegIndex stores the index into the set of machine registers used by
// the ABI on a specific architecture for parameter passing.  RegIndex
// values 0 through N-1 (where N is the number of integer registers
// used for param passing according to the ABI rules) describe integer
// registers; values N through M (where M is the number of floating
// point registers used).  Thus if the ABI says there are 5 integer
// registers and 7 floating point registers, then RegIndex value of 4
// indicates the 5th integer register, and a RegIndex value of 11
// indicates the 7th floating point register.
type RegIndex uint8

// ABIParamAssignment holds information about how a specific param or
// result will be passed: in registers (in which case 'Registers' is
// populated) or on the stack (in which case 'Offset' is set to a
// non-negative stack offset. The values in 'Registers' are indices (as
// described above), not architected registers.
type ABIParamAssignment struct {
	Type      *types.Type
	Registers []RegIndex
	offset    int32
}

// Offset returns the stack offset for addressing the parameter that "a" describes.
// This will panic if "a" describes a register-allocated parameter.
func (a *ABIParamAssignment) Offset() int32 {
	if len(a.Registers) > 0 {
		panic("Register allocated parameters have no offset")
	}
	return a.offset
}

// SpillOffset returns the offset *within the spill area* for the parameter that "a" describes.
// Registers will be spilled here; if a memory home is needed (for a pointer method e.g.)
// then that will be the address.
// This will panic if "a" describes a stack-allocated parameter.
func (a *ABIParamAssignment) SpillOffset() int32 {
	if len(a.Registers) == 0 {
		panic("Stack-allocated parameters have no spill offset")
	}
	return a.offset
}

// RegAmounts holds a specified number of integer/float registers.
type RegAmounts struct {
	intRegs   int
	floatRegs int
}

// ABIConfig captures the number of registers made available
// by the ABI rules for parameter passing and result returning.
type ABIConfig struct {
	// Do we need anything more than this?
	regAmounts       RegAmounts
	regsForTypeCache map[*types.Type]int
}

// NewABIConfig returns a new ABI configuration for an architecture with
// iRegsCount integer/pointer registers and fRegsCount floating point registers.
func NewABIConfig(iRegsCount, fRegsCount int) *ABIConfig {
	return &ABIConfig{regAmounts: RegAmounts{iRegsCount, fRegsCount}, regsForTypeCache: make(map[*types.Type]int)}
}

// NumParamRegs returns the number of parameter registers used for a given type,
// without regard for the number available.
func (a *ABIConfig) NumParamRegs(t *types.Type) int {
	if n, ok := a.regsForTypeCache[t]; ok {
		return n
	}

	if t.IsScalar() || t.IsPtrShaped() {
		var n int
		if t.IsComplex() {
			n = 2
		} else {
			n = (int(t.Size()) + types.RegSize - 1) / types.RegSize
		}
		a.regsForTypeCache[t] = n
		return n
	}
	typ := t.Kind()
	n := 0
	switch typ {
	case types.TARRAY:
		n = a.NumParamRegs(t.Elem()) * int(t.NumElem())
	case types.TSTRUCT:
		for _, f := range t.FieldSlice() {
			n += a.NumParamRegs(f.Type)
		}
	case types.TSLICE:
		n = a.NumParamRegs(synthSlice)
	case types.TSTRING:
		n = a.NumParamRegs(synthString)
	case types.TINTER:
		n = a.NumParamRegs(synthIface)
	}
	a.regsForTypeCache[t] = n
	return n
}

// ABIAnalyze takes a function type 't' and an ABI rules description
// 'config' and analyzes the function to determine how its parameters
// and results will be passed (in registers or on the stack), returning
// an ABIParamResultInfo object that holds the results of the analysis.
func (config *ABIConfig) ABIAnalyze(t *types.Type) ABIParamResultInfo {
	setup()
	s := assignState{
		rTotal: config.regAmounts,
	}
	result := ABIParamResultInfo{config: config}

	// Receiver
	ft := t.FuncType()
	if t.NumRecvs() != 0 {
		rfsl := ft.Receiver.FieldSlice()
		result.inparams = append(result.inparams,
			s.assignParamOrReturn(rfsl[0].Type, false))
	}

	// Inputs
	ifsl := ft.Params.FieldSlice()
	for _, f := range ifsl {
		result.inparams = append(result.inparams,
			s.assignParamOrReturn(f.Type, false))
	}
	s.stackOffset = types.Rnd(s.stackOffset, int64(types.RegSize))

	// Outputs
	s.rUsed = RegAmounts{}
	ofsl := ft.Results.FieldSlice()
	for _, f := range ofsl {
		result.outparams = append(result.outparams, s.assignParamOrReturn(f.Type, true))
	}
	// The spill area is at a register-aligned offset and its size is rounded up to a register alignment.
	// TODO in theory could align offset only to minimum required by spilled data types.
	result.offsetToSpillArea = alignTo(s.stackOffset, types.RegSize)
	result.spillAreaSize = alignTo(s.spillOffset, types.RegSize)

	return result
}

//......................................................................
//
// Non-public portions.

// regString produces a human-readable version of a RegIndex.
func (c *RegAmounts) regString(r RegIndex) string {
	if int(r) < c.intRegs {
		return fmt.Sprintf("I%d", int(r))
	} else if int(r) < c.intRegs+c.floatRegs {
		return fmt.Sprintf("F%d", int(r)-c.intRegs)
	}
	return fmt.Sprintf("<?>%d", r)
}

// toString method renders an ABIParamAssignment in human-readable
// form, suitable for debugging or unit testing.
func (ri *ABIParamAssignment) toString(config *ABIConfig) string {
	regs := "R{"
	offname := "spilloffset" // offset is for spill for register(s)
	if len(ri.Registers) == 0 {
		offname = "offset" // offset is for memory arg
	}
	for _, r := range ri.Registers {
		regs += " " + config.regAmounts.regString(r)
	}
	return fmt.Sprintf("%s } %s: %d typ: %v", regs, offname, ri.offset, ri.Type)
}

// toString method renders an ABIParamResultInfo in human-readable
// form, suitable for debugging or unit testing.
func (ri *ABIParamResultInfo) String() string {
	res := ""
	for k, p := range ri.inparams {
		res += fmt.Sprintf("IN %d: %s\n", k, p.toString(ri.config))
	}
	for k, r := range ri.outparams {
		res += fmt.Sprintf("OUT %d: %s\n", k, r.toString(ri.config))
	}
	res += fmt.Sprintf("offsetToSpillArea: %d spillAreaSize: %d",
		ri.offsetToSpillArea, ri.spillAreaSize)
	return res
}

// assignState holds intermediate state during the register assigning process
// for a given function signature.
type assignState struct {
	rTotal      RegAmounts // total reg amounts from ABI rules
	rUsed       RegAmounts // regs used by params completely assigned so far
	pUsed       RegAmounts // regs used by the current param (or pieces therein)
	stackOffset int64      // current stack offset
	spillOffset int64      // current spill offset
}

// align returns a rounded up to t's alignment
func align(a int64, t *types.Type) int64 {
	return alignTo(a, int(t.Align))
}

// alignTo returns a rounded up to t, where t must be 0 or a power of 2.
func alignTo(a int64, t int) int64 {
	if t == 0 {
		return a
	}
	return types.Rnd(a, int64(t))
}

// stackSlot returns a stack offset for a param or result of the
// specified type.
func (state *assignState) stackSlot(t *types.Type) int64 {
	rv := align(state.stackOffset, t)
	state.stackOffset = rv + t.Width
	return rv
}

// allocateRegs returns a set of register indices for a parameter or result
// that we've just determined to be register-assignable. The number of registers
// needed is assumed to be stored in state.pUsed.
func (state *assignState) allocateRegs() []RegIndex {
	regs := []RegIndex{}

	// integer
	for r := state.rUsed.intRegs; r < state.rUsed.intRegs+state.pUsed.intRegs; r++ {
		regs = append(regs, RegIndex(r))
	}
	state.rUsed.intRegs += state.pUsed.intRegs

	// floating
	for r := state.rUsed.floatRegs; r < state.rUsed.floatRegs+state.pUsed.floatRegs; r++ {
		regs = append(regs, RegIndex(r+state.rTotal.intRegs))
	}
	state.rUsed.floatRegs += state.pUsed.floatRegs

	return regs
}

// regAllocate creates a register ABIParamAssignment object for a param
// or result with the specified type, as a final step (this assumes
// that all of the safety/suitability analysis is complete).
func (state *assignState) regAllocate(t *types.Type, isReturn bool) ABIParamAssignment {
	spillLoc := int64(-1)
	if !isReturn {
		// Spill for register-resident t must be aligned for storage of a t.
		spillLoc = align(state.spillOffset, t)
		state.spillOffset = spillLoc + t.Size()
	}
	return ABIParamAssignment{
		Type:      t,
		Registers: state.allocateRegs(),
		offset:    int32(spillLoc),
	}
}

// stackAllocate creates a stack memory ABIParamAssignment object for
// a param or result with the specified type, as a final step (this
// assumes that all of the safety/suitability analysis is complete).
func (state *assignState) stackAllocate(t *types.Type) ABIParamAssignment {
	return ABIParamAssignment{
		Type:   t,
		offset: int32(state.stackSlot(t)),
	}
}

// intUsed returns the number of integer registers consumed
// at a given point within an assignment stage.
func (state *assignState) intUsed() int {
	return state.rUsed.intRegs + state.pUsed.intRegs
}

// floatUsed returns the number of floating point registers consumed at
// a given point within an assignment stage.
func (state *assignState) floatUsed() int {
	return state.rUsed.floatRegs + state.pUsed.floatRegs
}

// regassignIntegral examines a param/result of integral type 't' to
// determines whether it can be register-assigned. Returns TRUE if we
// can register allocate, FALSE otherwise (and updates state
// accordingly).
func (state *assignState) regassignIntegral(t *types.Type) bool {
	regsNeeded := int(types.Rnd(t.Width, int64(types.PtrSize)) / int64(types.PtrSize))
	if t.IsComplex() {
		regsNeeded = 2
	}

	// Floating point and complex.
	if t.IsFloat() || t.IsComplex() {
		if regsNeeded+state.floatUsed() > state.rTotal.floatRegs {
			// not enough regs
			return false
		}
		state.pUsed.floatRegs += regsNeeded
		return true
	}

	// Non-floating point
	if regsNeeded+state.intUsed() > state.rTotal.intRegs {
		// not enough regs
		return false
	}
	state.pUsed.intRegs += regsNeeded
	return true
}

// regassignArray processes an array type (or array component within some
// other enclosing type) to determine if it can be register assigned.
// Returns TRUE if we can register allocate, FALSE otherwise.
func (state *assignState) regassignArray(t *types.Type) bool {

	nel := t.NumElem()
	if nel == 0 {
		return true
	}
	if nel > 1 {
		// Not an array of length 1: stack assign
		return false
	}
	// Visit element
	return state.regassign(t.Elem())
}

// regassignStruct processes a struct type (or struct component within
// some other enclosing type) to determine if it can be register
// assigned. Returns TRUE if we can register allocate, FALSE otherwise.
func (state *assignState) regassignStruct(t *types.Type) bool {
	for _, field := range t.FieldSlice() {
		if !state.regassign(field.Type) {
			return false
		}
	}
	return true
}

// synthOnce ensures that we only create the synth* fake types once.
var synthOnce sync.Once

// synthSlice, synthString, and syncIface are synthesized struct types
// meant to capture the underlying implementations of string/slice/interface.
var synthSlice *types.Type
var synthString *types.Type
var synthIface *types.Type

// setup performs setup for the register assignment utilities, manufacturing
// a small set of synthesized types that we'll need along the way.
func setup() {
	synthOnce.Do(func() {
		fname := types.BuiltinPkg.Lookup
		nxp := src.NoXPos
		unsp := types.Types[types.TUNSAFEPTR]
		ui := types.Types[types.TUINTPTR]
		synthSlice = types.NewStruct(types.NoPkg, []*types.Field{
			types.NewField(nxp, fname("ptr"), unsp),
			types.NewField(nxp, fname("len"), ui),
			types.NewField(nxp, fname("cap"), ui),
		})
		synthString = types.NewStruct(types.NoPkg, []*types.Field{
			types.NewField(nxp, fname("data"), unsp),
			types.NewField(nxp, fname("len"), ui),
		})
		synthIface = types.NewStruct(types.NoPkg, []*types.Field{
			types.NewField(nxp, fname("f1"), unsp),
			types.NewField(nxp, fname("f2"), unsp),
		})
	})
}

// regassign examines a given param type (or component within some
// composite) to determine if it can be register assigned.  Returns
// TRUE if we can register allocate, FALSE otherwise.
func (state *assignState) regassign(pt *types.Type) bool {
	typ := pt.Kind()
	if pt.IsScalar() || pt.IsPtrShaped() {
		return state.regassignIntegral(pt)
	}
	switch typ {
	case types.TARRAY:
		return state.regassignArray(pt)
	case types.TSTRUCT:
		return state.regassignStruct(pt)
	case types.TSLICE:
		return state.regassignStruct(synthSlice)
	case types.TSTRING:
		return state.regassignStruct(synthString)
	case types.TINTER:
		return state.regassignStruct(synthIface)
	default:
		panic("not expected")
	}
}

// assignParamOrReturn processes a given receiver, param, or result
// of type 'pt' to determine whether it can be register assigned.
// The result of the analysis is recorded in the result
// ABIParamResultInfo held in 'state'.
func (state *assignState) assignParamOrReturn(pt *types.Type, isReturn bool) ABIParamAssignment {
	state.pUsed = RegAmounts{}
	if pt.Width == types.BADWIDTH {
		panic("should never happen")
	} else if pt.Width == 0 {
		return state.stackAllocate(pt)
	} else if state.regassign(pt) {
		return state.regAllocate(pt, isReturn)
	} else {
		return state.stackAllocate(pt)
	}
}
