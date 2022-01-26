// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"internal/race"
	"math/rand"
	"sort"
	"sync"
	"time"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
	"cmd/internal/sys"
)

// cmpstackvarlt reports whether the stack variable a sorts before b.
//
// Sort the list of stack variables. Autos after anything else,
// within autos, unused after used, within used, things with
// pointers first, zeroed things first, and then decreasing size.
// Because autos are laid out in decreasing addresses
// on the stack, pointers first, zeroed things first and decreasing size
// really means, in memory, things with pointers needing zeroing at
// the top of the stack and increasing in size.
// Non-autos sort on offset.
func cmpstackvarlt(a, b *ir.Name) bool {
	if (a.Class == ir.PAUTO) != (b.Class == ir.PAUTO) {
		return b.Class == ir.PAUTO
	}

	if a.Class != ir.PAUTO {
		return a.FrameOffset() < b.FrameOffset()
	}

	if a.Used() != b.Used() {
		return a.Used()
	}

	ap := a.Type().HasPointers()
	bp := b.Type().HasPointers()
	if ap != bp {
		return ap
	}

	ap = a.Needzero()
	bp = b.Needzero()
	if ap != bp {
		return ap
	}

	if a.Type().Width != b.Type().Width {
		return a.Type().Width > b.Type().Width
	}

	return a.Sym().Name < b.Sym().Name
}

// byStackvar implements sort.Interface for []*Node using cmpstackvarlt.
type byStackVar []*ir.Name

func (s byStackVar) Len() int           { return len(s) }
func (s byStackVar) Less(i, j int) bool { return cmpstackvarlt(s[i], s[j]) }
func (s byStackVar) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func (s *ssafn) AllocFrame(f *ssa.Func) {
	s.stksize = 0
	s.stkptrsize = 0
	fn := s.curfn

	// Mark the PAUTO's unused.
	for _, ln := range fn.Dcl {
		if ln.Class == ir.PAUTO {
			ln.SetUsed(false)
		}
	}

	for _, l := range f.RegAlloc {
		if ls, ok := l.(ssa.LocalSlot); ok {
			ls.N.SetUsed(true)
		}
	}

	scratchUsed := false
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if n, ok := v.Aux.(*ir.Name); ok {
				switch n.Class {
				case ir.PPARAM, ir.PPARAMOUT:
					// Don't modify RegFP; it is a global.
					if n != ir.RegFP {
						n.SetUsed(true)
					}
				case ir.PAUTO:
					n.SetUsed(true)
				}
			}
			if !scratchUsed {
				scratchUsed = v.Op.UsesScratch()
			}

		}
	}

	if f.Config.NeedsFpScratch && scratchUsed {
		s.scratchFpMem = typecheck.TempAt(src.NoXPos, s.curfn, types.Types[types.TUINT64])
	}

	sort.Sort(byStackVar(fn.Dcl))

	// Reassign stack offsets of the locals that are used.
	lastHasPtr := false
	for i, n := range fn.Dcl {
		if n.Op() != ir.ONAME || n.Class != ir.PAUTO {
			continue
		}
		if !n.Used() {
			fn.Dcl = fn.Dcl[:i]
			break
		}

		types.CalcSize(n.Type())
		w := n.Type().Width
		if w >= types.MaxWidth || w < 0 {
			base.Fatalf("bad width")
		}
		if w == 0 && lastHasPtr {
			// Pad between a pointer-containing object and a zero-sized object.
			// This prevents a pointer to the zero-sized object from being interpreted
			// as a pointer to the pointer-containing object (and causing it
			// to be scanned when it shouldn't be). See issue 24993.
			w = 1
		}
		s.stksize += w
		s.stksize = types.Rnd(s.stksize, int64(n.Type().Align))
		if n.Type().HasPointers() {
			s.stkptrsize = s.stksize
			lastHasPtr = true
		} else {
			lastHasPtr = false
		}
		if Arch.LinkArch.InFamily(sys.MIPS, sys.MIPS64, sys.ARM, sys.ARM64, sys.PPC64, sys.S390X) {
			s.stksize = types.Rnd(s.stksize, int64(types.PtrSize))
		}
		n.SetFrameOffset(-s.stksize)
	}

	s.stksize = types.Rnd(s.stksize, int64(types.RegSize))
	s.stkptrsize = types.Rnd(s.stkptrsize, int64(types.RegSize))
}

const maxStackSize = 1 << 30

// Compile builds an SSA backend function,
// uses it to generate a plist,
// and flushes that plist to machine code.
// worker indicates which of the backend workers is doing the processing.
func Compile(fn *ir.Func, worker int) {
	f := buildssa(fn, worker)
	// Note: check arg size to fix issue 25507.
	if f.Frontend().(*ssafn).stksize >= maxStackSize || fn.Type().ArgWidth() >= maxStackSize {
		largeStackFramesMu.Lock()
		largeStackFrames = append(largeStackFrames, largeStack{locals: f.Frontend().(*ssafn).stksize, args: fn.Type().ArgWidth(), pos: fn.Pos()})
		largeStackFramesMu.Unlock()
		return
	}
	pp := objw.NewProgs(fn, worker)
	defer pp.Free()
	genssa(f, pp)
	// Check frame size again.
	// The check above included only the space needed for local variables.
	// After genssa, the space needed includes local variables and the callee arg region.
	// We must do this check prior to calling pp.Flush.
	// If there are any oversized stack frames,
	// the assembler may emit inscrutable complaints about invalid instructions.
	if pp.Text.To.Offset >= maxStackSize {
		largeStackFramesMu.Lock()
		locals := f.Frontend().(*ssafn).stksize
		largeStackFrames = append(largeStackFrames, largeStack{locals: locals, args: fn.Type().ArgWidth(), callee: pp.Text.To.Offset - locals, pos: fn.Pos()})
		largeStackFramesMu.Unlock()
		return
	}

	pp.Flush() // assemble, fill in boilerplate, etc.
	// fieldtrack must be called after pp.Flush. See issue 20014.
	fieldtrack(pp.Text.From.Sym, fn.FieldTrack)
}

func init() {
	if race.Enabled {
		rand.Seed(time.Now().UnixNano())
	}
}

// StackOffset returns the stack location of a LocalSlot relative to the
// stack pointer, suitable for use in a DWARF location entry. This has nothing
// to do with its offset in the user variable.
func StackOffset(slot ssa.LocalSlot) int32 {
	n := slot.N
	var off int64
	switch n.Class {
	case ir.PAUTO:
		off = n.FrameOffset()
		if base.Ctxt.FixedFrameSize() == 0 {
			off -= int64(types.PtrSize)
		}
		if objabi.Framepointer_enabled || objabi.GOARCH == "arm64" {
			// There is a word space for FP on ARM64 even if the frame pointer is disabled
			off -= int64(types.PtrSize)
		}
	case ir.PPARAM, ir.PPARAMOUT:
		off = n.FrameOffset() + base.Ctxt.FixedFrameSize()
	}
	return int32(off + slot.Off)
}

// fieldtrack adds R_USEFIELD relocations to fnsym to record any
// struct fields that it used.
func fieldtrack(fnsym *obj.LSym, tracked map[*obj.LSym]struct{}) {
	if fnsym == nil {
		return
	}
	if objabi.Fieldtrack_enabled == 0 || len(tracked) == 0 {
		return
	}

	trackSyms := make([]*obj.LSym, 0, len(tracked))
	for sym := range tracked {
		trackSyms = append(trackSyms, sym)
	}
	sort.Slice(trackSyms, func(i, j int) bool { return trackSyms[i].Name < trackSyms[j].Name })
	for _, sym := range trackSyms {
		r := obj.Addrel(fnsym)
		r.Sym = sym
		r.Type = objabi.R_USEFIELD
	}
}

// largeStack is info about a function whose stack frame is too large (rare).
type largeStack struct {
	locals int64
	args   int64
	callee int64
	pos    src.XPos
}

var (
	largeStackFramesMu sync.Mutex // protects largeStackFrames
	largeStackFrames   []largeStack
)

func CheckLargeStacks() {
	// Check whether any of the functions we have compiled have gigantic stack frames.
	sort.Slice(largeStackFrames, func(i, j int) bool {
		return largeStackFrames[i].pos.Before(largeStackFrames[j].pos)
	})
	for _, large := range largeStackFrames {
		if large.callee != 0 {
			base.ErrorfAt(large.pos, "stack frame too large (>1GB): %d MB locals + %d MB args + %d MB callee", large.locals>>20, large.args>>20, large.callee>>20)
		} else {
			base.ErrorfAt(large.pos, "stack frame too large (>1GB): %d MB locals + %d MB args", large.locals>>20, large.args>>20)
		}
	}
}
