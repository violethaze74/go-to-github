// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package loader

import (
	"bytes"
	"cmd/internal/bio"
	"cmd/internal/goobj2"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/sys"
	"cmd/link/internal/sym"
	"debug/elf"
	"fmt"
	"log"
	"math/bits"
	"os"
	"sort"
	"strconv"
	"strings"
)

var _ = fmt.Print

// Sym encapsulates a global symbol index, used to identify a specific
// Go symbol. The 0-valued Sym is corresponds to an invalid symbol.
type Sym int

// Relocs encapsulates the set of relocations on a given symbol; an
// instance of this type is returned by the Loader Relocs() method.
type Relocs struct {
	rs []goobj2.Reloc2

	li int      // local index of symbol whose relocs we're examining
	r  *oReader // object reader for containing package
	l  *Loader  // loader
}

// Reloc contains the payload for a specific relocation.
// TODO: replace this with sym.Reloc, once we change the
// relocation target from "*sym.Symbol" to "loader.Sym" in sym.Reloc.
type Reloc struct {
	Off  int32            // offset to rewrite
	Size uint8            // number of bytes to rewrite: 0, 1, 2, or 4
	Type objabi.RelocType // the relocation type
	Add  int64            // addend
	Sym  Sym              // global index of symbol the reloc addresses
}

// Reloc2 holds a "handle" to access a relocation record from an
// object file.
type Reloc2 struct {
	*goobj2.Reloc2
	r *oReader
	l *Loader

	// External reloc types may not fit into a uint8 which the Go object file uses.
	// Store it here, instead of in the byte of goobj2.Reloc2.
	// For Go symbols this will always be 0.
	// goobj2.Reloc2.Type() + typ is always the right type, for both Go and external
	// symbols.
	typ objabi.RelocType
}

func (rel Reloc2) Type() objabi.RelocType { return objabi.RelocType(rel.Reloc2.Type()) + rel.typ }
func (rel Reloc2) Sym() Sym               { return rel.l.resolve(rel.r, rel.Reloc2.Sym()) }
func (rel Reloc2) SetSym(s Sym)           { rel.Reloc2.SetSym(goobj2.SymRef{PkgIdx: 0, SymIdx: uint32(s)}) }

func (rel Reloc2) SetType(t objabi.RelocType) {
	if t != objabi.RelocType(uint8(t)) {
		panic("SetType: type doesn't fit into Reloc2")
	}
	rel.Reloc2.SetType(uint8(t))
}

// Aux2 holds a "handle" to access an aux symbol record from an
// object file.
type Aux2 struct {
	*goobj2.Aux2
	r *oReader
	l *Loader
}

func (a Aux2) Sym() Sym { return a.l.resolve(a.r, a.Aux2.Sym()) }

// oReader is a wrapper type of obj.Reader, along with some
// extra information.
// TODO: rename to objReader once the old one is gone?
type oReader struct {
	*goobj2.Reader
	unit      *sym.CompilationUnit
	version   int    // version of static symbol
	flags     uint32 // read from object file
	pkgprefix string
	syms      []Sym  // Sym's global index, indexed by local index
	ndef      int    // cache goobj2.Reader.NSym()
	objidx    uint32 // index of this reader in the objs slice
}

type objIdx struct {
	r *oReader
	i Sym // start index
}

// objSym represents a symbol in an object file. It is a tuple of
// the object and the symbol's local index.
// For external symbols, r is l.extReader, s is its index into the
// payload array.
// {nil, 0} represents the nil symbol.
type objSym struct {
	r *oReader
	s int // local index
}

type nameVer struct {
	name string
	v    int
}

type Bitmap []uint32

// set the i-th bit.
func (bm Bitmap) Set(i Sym) {
	n, r := uint(i)/32, uint(i)%32
	bm[n] |= 1 << r
}

// unset the i-th bit.
func (bm Bitmap) Unset(i Sym) {
	n, r := uint(i)/32, uint(i)%32
	bm[n] &^= (1 << r)
}

// whether the i-th bit is set.
func (bm Bitmap) Has(i Sym) bool {
	n, r := uint(i)/32, uint(i)%32
	return bm[n]&(1<<r) != 0
}

// return current length of bitmap in bits.
func (bm Bitmap) Len() int {
	return len(bm) * 32
}
func MakeBitmap(n int) Bitmap {
	return make(Bitmap, (n+31)/32)
}

// growBitmap insures that the specified bitmap has enough capacity,
// reallocating (doubling the size) if needed.
func growBitmap(reqLen int, b Bitmap) Bitmap {
	curLen := b.Len()
	if reqLen > curLen {
		b = append(b, MakeBitmap(reqLen+1-curLen)...)
	}
	return b
}

// A Loader loads new object files and resolves indexed symbol references.
//
// Notes on the layout of global symbol index space:
//
// - Go object files are read before host object files; each Go object
//   read adds its defined package symbols to the global index space.
//   Nonpackage symbols are not yet added.
//
// - In loader.LoadNonpkgSyms, add non-package defined symbols and
//   references in all object files to the global index space.
//
// - Host object file loading happens; the host object loader does a
//   name/version lookup for each symbol it finds; this can wind up
//   extending the external symbol index space range. The host object
//   loader stores symbol payloads in loader.payloads using SymbolBuilder.
//
// - For now, in loader.LoadFull we convert all symbols (Go + external)
//   to sym.Symbols.
//
// - At some point (when the wayfront is pushed through all of the
//   linker), all external symbols will be payload-based, and we can
//   get rid of the loader.Syms array.
//
// - Each symbol gets a unique global index. For duplicated and
//   overwriting/overwritten symbols, the second (or later) appearance
//   of the symbol gets the same global index as the first appearance.
type Loader struct {
	start       map[*oReader]Sym // map from object file to its start index
	objs        []objIdx         // sorted by start index (i.e. objIdx.i)
	extStart    Sym              // from this index on, the symbols are externally defined
	builtinSyms []Sym            // global index of builtin symbols

	objSyms []objSym // global index mapping to local index

	symsByName    [2]map[string]Sym // map symbol name to index, two maps are for ABI0 and ABIInternal
	extStaticSyms map[nameVer]Sym   // externally defined static symbols, keyed by name

	extReader    *oReader // a dummy oReader, for external symbols
	payloadBatch []extSymPayload
	payloads     []*extSymPayload // contents of linker-materialized external syms
	values       []int64          // symbol values, indexed by global sym index

	itablink map[Sym]struct{} // itablink[j] defined if j is go.itablink.*

	objByPkg map[string]*oReader // map package path to its Go object reader

	Syms     []*sym.Symbol // indexed symbols. XXX we still make sym.Symbol for now.
	symBatch []sym.Symbol  // batch of symbols.

	anonVersion int // most recently assigned ext static sym pseudo-version

	// Bitmaps and other side structures used to store data used to store
	// symbol flags/attributes; these are to be accessed via the
	// corresponding loader "AttrXXX" and "SetAttrXXX" methods. Please
	// visit the comments on these methods for more details on the
	// semantics / interpretation of the specific flags or attribute.
	attrReachable        Bitmap // reachable symbols, indexed by global index
	attrOnList           Bitmap // "on list" symbols, indexed by global index
	attrLocal            Bitmap // "local" symbols, indexed by global index
	attrNotInSymbolTable Bitmap // "not in symtab" symbols, indexed by glob idx
	attrVisibilityHidden Bitmap // hidden symbols, indexed by ext sym index
	attrDuplicateOK      Bitmap // dupOK symbols, indexed by ext sym index
	attrShared           Bitmap // shared symbols, indexed by ext sym index
	attrExternal         Bitmap // external symbols, indexed by ext sym index

	attrReadOnly         map[Sym]bool     // readonly data for this sym
	attrTopFrame         map[Sym]struct{} // top frame symbols
	attrSpecial          map[Sym]struct{} // "special" frame symbols
	attrCgoExportDynamic map[Sym]struct{} // "cgo_export_dynamic" symbols
	attrCgoExportStatic  map[Sym]struct{} // "cgo_export_static" symbols

	// Outer and Sub relations for symbols.
	// TODO: figure out whether it's more efficient to just have these
	// as fields on extSymPayload (note that this won't be a viable
	// strategy if somewhere in the linker we set sub/outer for a
	// non-external sym).
	outer map[Sym]Sym
	sub   map[Sym]Sym

	align map[Sym]int32 // stores alignment for symbols

	dynimplib  map[Sym]string      // stores Dynimplib symbol attribute
	dynimpvers map[Sym]string      // stores Dynimpvers symbol attribute
	localentry map[Sym]uint8       // stores Localentry symbol attribute
	extname    map[Sym]string      // stores Extname symbol attribute
	elfType    map[Sym]elf.SymType // stores elf type symbol property
	symFile    map[Sym]string      // stores file for shlib-derived syms
	plt        map[Sym]int32       // stores dynimport for pe objects
	got        map[Sym]int32       // stores got for pe objects
	dynid      map[Sym]int32       // stores Dynid for symbol

	// Used to implement field tracking; created during deadcode if
	// field tracking is enabled. Reachparent[K] contains the index of
	// the symbol that triggered the marking of symbol K as live.
	Reachparent []Sym

	relocBatch []sym.Reloc // for bulk allocation of relocations

	flags uint32

	strictDupMsgs int // number of strict-dup warning/errors, when FlagStrictDups is enabled

	elfsetstring elfsetstringFunc

	SymLookup func(name string, ver int) *sym.Symbol
}

const (
	pkgDef = iota
	nonPkgDef
	nonPkgRef
)

type elfsetstringFunc func(s *sym.Symbol, str string, off int)

// extSymPayload holds the payload (data + relocations) for linker-synthesized
// external symbols (note that symbol value is stored in a separate slice).
type extSymPayload struct {
	name     string // TODO: would this be better as offset into str table?
	size     int64
	ver      int
	kind     sym.SymKind
	objidx   uint32 // index of original object if sym made by cloneToExternal
	gotype   Sym    // Gotype (0 if not present)
	relocs   []goobj2.Reloc2
	reltypes []objabi.RelocType // relocation types
	data     []byte
}

const (
	// Loader.flags
	FlagStrictDups = 1 << iota
)

func NewLoader(flags uint32, elfsetstring elfsetstringFunc) *Loader {
	nbuiltin := goobj2.NBuiltin()
	return &Loader{
		start:                make(map[*oReader]Sym),
		objs:                 []objIdx{{}}, // reserve index 0 for nil symbol
		objSyms:              []objSym{{}}, // reserve index 0 for nil symbol
		extReader:            &oReader{},
		symsByName:           [2]map[string]Sym{make(map[string]Sym, 100000), make(map[string]Sym, 50000)}, // preallocate ~2MB for ABI0 and ~1MB for ABI1 symbols
		objByPkg:             make(map[string]*oReader),
		outer:                make(map[Sym]Sym),
		sub:                  make(map[Sym]Sym),
		align:                make(map[Sym]int32),
		dynimplib:            make(map[Sym]string),
		dynimpvers:           make(map[Sym]string),
		localentry:           make(map[Sym]uint8),
		extname:              make(map[Sym]string),
		attrReadOnly:         make(map[Sym]bool),
		elfType:              make(map[Sym]elf.SymType),
		symFile:              make(map[Sym]string),
		plt:                  make(map[Sym]int32),
		got:                  make(map[Sym]int32),
		dynid:                make(map[Sym]int32),
		attrTopFrame:         make(map[Sym]struct{}),
		attrSpecial:          make(map[Sym]struct{}),
		attrCgoExportDynamic: make(map[Sym]struct{}),
		attrCgoExportStatic:  make(map[Sym]struct{}),
		itablink:             make(map[Sym]struct{}),
		extStaticSyms:        make(map[nameVer]Sym),
		builtinSyms:          make([]Sym, nbuiltin),
		flags:                flags,
		elfsetstring:         elfsetstring,
	}
}

// Add object file r, return the start index.
func (l *Loader) addObj(pkg string, r *oReader) Sym {
	if _, ok := l.start[r]; ok {
		panic("already added")
	}
	pkg = objabi.PathToPrefix(pkg) // the object file contains escaped package path
	if _, ok := l.objByPkg[pkg]; !ok {
		l.objByPkg[pkg] = r
	}
	i := Sym(len(l.objSyms))
	l.start[r] = i
	l.objs = append(l.objs, objIdx{r, i})
	return i
}

// Add a symbol from an object file, return the global index and whether it is added.
// If the symbol already exist, it returns the index of that symbol.
func (l *Loader) AddSym(name string, ver int, r *oReader, li int, kind int, dupok bool, typ sym.SymKind) (Sym, bool) {
	if l.extStart != 0 {
		panic("AddSym called after external symbol is created")
	}
	i := Sym(len(l.objSyms))
	addToGlobal := func() {
		l.objSyms = append(l.objSyms, objSym{r, li})
	}
	if name == "" {
		addToGlobal()
		return i, true // unnamed aux symbol
	}
	if ver == r.version {
		// Static symbol. Add its global index but don't
		// add to name lookup table, as it cannot be
		// referenced by name.
		addToGlobal()
		return i, true
	}
	if kind == pkgDef {
		// Defined package symbols cannot be dup to each other.
		// We load all the package symbols first, so we don't need
		// to check dup here.
		// We still add it to the lookup table, as it may still be
		// referenced by name (e.g. through linkname).
		l.symsByName[ver][name] = i
		addToGlobal()
		return i, true
	}

	// Non-package (named) symbol. Check if it already exists.
	oldi, existed := l.symsByName[ver][name]
	if !existed {
		l.symsByName[ver][name] = i
		addToGlobal()
		return i, true
	}
	// symbol already exists
	if dupok {
		if l.flags&FlagStrictDups != 0 {
			l.checkdup(name, r, li, oldi)
		}
		return oldi, false
	}
	oldr, oldli := l.toLocal(oldi)
	oldsym := oldr.Sym2(oldli)
	if oldsym.Dupok() {
		return oldi, false
	}
	overwrite := r.DataSize(li) != 0
	if overwrite {
		// new symbol overwrites old symbol.
		oldtyp := sym.AbiSymKindToSymKind[objabi.SymKind(oldsym.Type())]
		if !(oldtyp.IsData() && oldr.DataSize(oldli) == 0) {
			log.Fatalf("duplicated definition of symbol " + name)
		}
		l.objSyms[oldi] = objSym{r, li}
	} else {
		// old symbol overwrites new symbol.
		if !typ.IsData() { // only allow overwriting data symbol
			log.Fatalf("duplicated definition of symbol " + name)
		}
	}
	return oldi, true
}

// newExtSym creates a new external sym with the specified
// name/version.
func (l *Loader) newExtSym(name string, ver int) Sym {
	i := Sym(len(l.objSyms))
	if l.extStart == 0 {
		l.extStart = i
	}
	l.growSyms(int(i))
	pi := l.newPayload(name, ver)
	l.objSyms = append(l.objSyms, objSym{l.extReader, int(pi)})
	l.extReader.syms = append(l.extReader.syms, i)
	return i
}

// LookupOrCreateSym looks up the symbol with the specified name/version,
// returning its Sym index if found. If the lookup fails, a new external
// Sym will be created, entered into the lookup tables, and returned.
func (l *Loader) LookupOrCreateSym(name string, ver int) Sym {
	i := l.Lookup(name, ver)
	if i != 0 {
		return i
	}
	i = l.newExtSym(name, ver)
	static := ver >= sym.SymVerStatic || ver < 0
	if static {
		l.extStaticSyms[nameVer{name, ver}] = i
	} else {
		l.symsByName[ver][name] = i
	}
	return i
}

func (l *Loader) IsExternal(i Sym) bool {
	r, _ := l.toLocal(i)
	return l.isExtReader(r)
}

func (l *Loader) isExtReader(r *oReader) bool {
	return r == l.extReader
}

// For external symbol, return its index in the payloads array.
// XXX result is actually not a global index. We (ab)use the Sym type
// so we don't need conversion for accessing bitmaps.
func (l *Loader) extIndex(i Sym) Sym {
	_, li := l.toLocal(i)
	return Sym(li)
}

// Get a new payload for external symbol, return its index in
// the payloads array.
func (l *Loader) newPayload(name string, ver int) int {
	pi := len(l.payloads)
	pp := l.allocPayload()
	pp.name = name
	pp.ver = ver
	l.payloads = append(l.payloads, pp)
	l.growExtAttrBitmaps()
	return pi
}

// getPayload returns a pointer to the extSymPayload struct for an
// external symbol if the symbol has a payload. Will panic if the
// symbol in question is bogus (zero or not an external sym).
func (l *Loader) getPayload(i Sym) *extSymPayload {
	if !l.IsExternal(i) {
		panic(fmt.Sprintf("bogus symbol index %d in getPayload", i))
	}
	pi := l.extIndex(i)
	return l.payloads[pi]
}

// allocPayload allocates a new payload.
func (l *Loader) allocPayload() *extSymPayload {
	batch := l.payloadBatch
	if len(batch) == 0 {
		batch = make([]extSymPayload, 1000)
	}
	p := &batch[0]
	l.payloadBatch = batch[1:]
	return p
}

func (ms *extSymPayload) Grow(siz int64) {
	if int64(int(siz)) != siz {
		log.Fatalf("symgrow size %d too long", siz)
	}
	if int64(len(ms.data)) >= siz {
		return
	}
	if cap(ms.data) < int(siz) {
		cl := len(ms.data)
		ms.data = append(ms.data, make([]byte, int(siz)+1-cl)...)
		ms.data = ms.data[0:cl]
	}
	ms.data = ms.data[:siz]
}

// Ensure Syms slice has enough space.
func (l *Loader) growSyms(i int) {
	n := len(l.Syms)
	if n > i {
		return
	}
	l.Syms = append(l.Syms, make([]*sym.Symbol, i+1-n)...)
	l.growValues(int(i) + 1)
	l.growAttrBitmaps(int(i) + 1)
}

// Convert a local index to a global index.
func (l *Loader) toGlobal(r *oReader, i int) Sym {
	return r.syms[i]
}

// Convert a global index to a local index.
func (l *Loader) toLocal(i Sym) (*oReader, int) {
	return l.objSyms[i].r, int(l.objSyms[i].s)
}

// Resolve a local symbol reference. Return global index.
func (l *Loader) resolve(r *oReader, s goobj2.SymRef) Sym {
	var rr *oReader
	switch p := s.PkgIdx; p {
	case goobj2.PkgIdxInvalid:
		// {0, X} with non-zero X is never a valid sym reference from a Go object.
		// We steal this space for symbol references from external objects.
		// In this case, X is just the global index.
		if l.isExtReader(r) {
			return Sym(s.SymIdx)
		}
		if s.SymIdx != 0 {
			panic("bad sym ref")
		}
		return 0
	case goobj2.PkgIdxNone:
		i := int(s.SymIdx) + r.ndef
		return r.syms[i]
	case goobj2.PkgIdxBuiltin:
		return l.builtinSyms[s.SymIdx]
	case goobj2.PkgIdxSelf:
		rr = r
	default:
		pkg := r.Pkg(int(p))
		var ok bool
		rr, ok = l.objByPkg[pkg]
		if !ok {
			log.Fatalf("reference of nonexisted package %s, from %v", pkg, r.unit.Lib)
		}
	}
	return l.toGlobal(rr, int(s.SymIdx))
}

// Look up a symbol by name, return global index, or 0 if not found.
// This is more like Syms.ROLookup than Lookup -- it doesn't create
// new symbol.
func (l *Loader) Lookup(name string, ver int) Sym {
	if ver >= sym.SymVerStatic || ver < 0 {
		return l.extStaticSyms[nameVer{name, ver}]
	}
	return l.symsByName[ver][name]
}

// Check that duplicate symbols have same contents.
func (l *Loader) checkdup(name string, r *oReader, li int, dup Sym) {
	p := r.Data(li)
	rdup, ldup := l.toLocal(dup)
	pdup := rdup.Data(ldup)
	if bytes.Equal(p, pdup) {
		return
	}
	reason := "same length but different contents"
	if len(p) != len(pdup) {
		reason = fmt.Sprintf("new length %d != old length %d", len(p), len(pdup))
	}
	fmt.Fprintf(os.Stderr, "cmd/link: while reading object for '%v': duplicate symbol '%s', previous def at '%v', with mismatched payload: %s\n", r.unit.Lib, name, rdup.unit.Lib, reason)

	// For the moment, whitelist DWARF subprogram DIEs for
	// auto-generated wrapper functions. What seems to happen
	// here is that we get different line numbers on formal
	// params; I am guessing that the pos is being inherited
	// from the spot where the wrapper is needed.
	whitelist := strings.HasPrefix(name, "go.info.go.interface") ||
		strings.HasPrefix(name, "go.info.go.builtin") ||
		strings.HasPrefix(name, "go.debuglines")
	if !whitelist {
		l.strictDupMsgs++
	}
}

func (l *Loader) NStrictDupMsgs() int { return l.strictDupMsgs }

// Number of total symbols.
func (l *Loader) NSym() int {
	return len(l.objSyms)
}

// Number of defined Go symbols.
func (l *Loader) NDef() int {
	return int(l.extStart)
}

// Returns the raw (unpatched) name of the i-th symbol.
func (l *Loader) RawSymName(i Sym) string {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		return pp.name
	}
	r, li := l.toLocal(i)
	return r.Sym2(li).Name(r.Reader)
}

// Returns the (patched) name of the i-th symbol.
func (l *Loader) SymName(i Sym) string {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		return pp.name
	}
	r, li := l.toLocal(i)
	return strings.Replace(r.Sym2(li).Name(r.Reader), "\"\".", r.pkgprefix, -1)
}

// Returns the version of the i-th symbol.
func (l *Loader) SymVersion(i Sym) int {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		return pp.ver
	}
	r, li := l.toLocal(i)
	return int(abiToVer(r.Sym2(li).ABI(), r.version))
}

// Returns the type of the i-th symbol.
func (l *Loader) SymType(i Sym) sym.SymKind {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		if pp != nil {
			return pp.kind
		}
		return 0
	}
	r, li := l.toLocal(i)
	return sym.AbiSymKindToSymKind[objabi.SymKind(r.Sym2(li).Type())]
}

// Returns the attributes of the i-th symbol.
func (l *Loader) SymAttr(i Sym) uint8 {
	if l.IsExternal(i) {
		// TODO: do something? External symbols have different representation of attributes. For now, ReflectMethod is the only thing matters and it cannot be set by external symbol.
		return 0
	}
	r, li := l.toLocal(i)
	return r.Sym2(li).Flag()
}

// AttrReachable returns true for symbols that are transitively
// referenced from the entry points. Unreachable symbols are not
// written to the output.
func (l *Loader) AttrReachable(i Sym) bool {
	return l.attrReachable.Has(i)
}

// SetAttrReachable sets the reachability property for a symbol (see
// AttrReachable).
func (l *Loader) SetAttrReachable(i Sym, v bool) {
	if v {
		l.attrReachable.Set(i)
	} else {
		l.attrReachable.Unset(i)
	}
}

// AttrOnList returns true for symbols that are on some list (such as
// the list of all text symbols, or one of the lists of data symbols)
// and is consulted to avoid bugs where a symbol is put on a list
// twice.
func (l *Loader) AttrOnList(i Sym) bool {
	return l.attrOnList.Has(i)
}

// SetAttrOnList sets the "on list" property for a symbol (see
// AttrOnList).
func (l *Loader) SetAttrOnList(i Sym, v bool) {
	if v {
		l.attrOnList.Set(i)
	} else {
		l.attrOnList.Unset(i)
	}
}

// AttrLocal returns true for symbols that are only visible within the
// module (executable or shared library) being linked. This attribute
// is applied to thunks and certain other linker-generated symbols.
func (l *Loader) AttrLocal(i Sym) bool {
	return l.attrLocal.Has(i)
}

// SetAttrLocal the "local" property for a symbol (see AttrLocal above).
func (l *Loader) SetAttrLocal(i Sym, v bool) {
	if v {
		l.attrLocal.Set(i)
	} else {
		l.attrLocal.Unset(i)
	}
}

// AttrNotInSymbolTable returns true for symbols that should not be
// added to the symbol table of the final generated load module.
func (l *Loader) AttrNotInSymbolTable(i Sym) bool {
	return l.attrNotInSymbolTable.Has(i)
}

// SetAttrNotInSymbolTable the "not in symtab" property for a symbol
// (see AttrNotInSymbolTable above).
func (l *Loader) SetAttrNotInSymbolTable(i Sym, v bool) {
	if v {
		l.attrNotInSymbolTable.Set(i)
	} else {
		l.attrNotInSymbolTable.Unset(i)
	}
}

// AttrVisibilityHidden symbols returns true for ELF symbols with
// visibility set to STV_HIDDEN. They become local symbols in
// the final executable. Only relevant when internally linking
// on an ELF platform.
func (l *Loader) AttrVisibilityHidden(i Sym) bool {
	if !l.IsExternal(i) {
		return false
	}
	return l.attrVisibilityHidden.Has(l.extIndex(i))
}

// SetAttrVisibilityHidden sets the "hidden visibility" property for a
// symbol (see AttrVisibilityHidden).
func (l *Loader) SetAttrVisibilityHidden(i Sym, v bool) {
	if !l.IsExternal(i) {
		panic("tried to set visibility attr on non-external symbol")
	}
	if v {
		l.attrVisibilityHidden.Set(l.extIndex(i))
	} else {
		l.attrVisibilityHidden.Unset(l.extIndex(i))
	}
}

// AttrDuplicateOK returns true for a symbol that can be present in
// multiple object files.
func (l *Loader) AttrDuplicateOK(i Sym) bool {
	if !l.IsExternal(i) {
		// TODO: if this path winds up being taken frequently, it
		// might make more sense to copy the flag value out of the object
		// into a larger bitmap during preload.
		r, li := l.toLocal(i)
		return r.Sym2(li).Dupok()
	}
	return l.attrDuplicateOK.Has(l.extIndex(i))
}

// SetAttrDuplicateOK sets the "duplicate OK" property for an external
// symbol (see AttrDuplicateOK).
func (l *Loader) SetAttrDuplicateOK(i Sym, v bool) {
	if !l.IsExternal(i) {
		panic("tried to set dupok attr on non-external symbol")
	}
	if v {
		l.attrDuplicateOK.Set(l.extIndex(i))
	} else {
		l.attrDuplicateOK.Unset(l.extIndex(i))
	}
}

// AttrShared returns true for symbols compiled with the -shared option.
func (l *Loader) AttrShared(i Sym) bool {
	if !l.IsExternal(i) {
		// TODO: if this path winds up being taken frequently, it
		// might make more sense to copy the flag value out of the
		// object into a larger bitmap during preload.
		r, _ := l.toLocal(i)
		return (r.Flags() & goobj2.ObjFlagShared) != 0
	}
	return l.attrShared.Has(l.extIndex(i))
}

// SetAttrShared sets the "shared" property for an external
// symbol (see AttrShared).
func (l *Loader) SetAttrShared(i Sym, v bool) {
	if !l.IsExternal(i) {
		panic(fmt.Sprintf("tried to set shared attr on non-external symbol %d %s", i, l.SymName(i)))
	}
	if v {
		l.attrShared.Set(l.extIndex(i))
	} else {
		l.attrShared.Unset(l.extIndex(i))
	}
}

// AttrExternal returns true for function symbols loaded from host
// object files.
func (l *Loader) AttrExternal(i Sym) bool {
	if !l.IsExternal(i) {
		return false
	}
	return l.attrExternal.Has(l.extIndex(i))
}

// SetAttrExternal sets the "external" property for an host object
// symbol (see AttrExternal).
func (l *Loader) SetAttrExternal(i Sym, v bool) {
	if !l.IsExternal(i) {
		panic(fmt.Sprintf("tried to set external attr on non-external symbol %q", l.RawSymName(i)))
	}
	if v {
		l.attrExternal.Set(l.extIndex(i))
	} else {
		l.attrExternal.Unset(l.extIndex(i))
	}
}

// AttrTopFrame returns true for a function symbol that is an entry
// point, meaning that unwinders should stop when they hit this
// function.
func (l *Loader) AttrTopFrame(i Sym) bool {
	_, ok := l.attrTopFrame[i]
	return ok
}

// SetAttrTopFrame sets the "top frame" property for a symbol (see
// AttrTopFrame).
func (l *Loader) SetAttrTopFrame(i Sym, v bool) {
	if v {
		l.attrTopFrame[i] = struct{}{}
	} else {
		delete(l.attrTopFrame, i)
	}
}

// AttrSpecial returns true for a symbols that do not have their
// address (i.e. Value) computed by the usual mechanism of
// data.go:dodata() & data.go:address().
func (l *Loader) AttrSpecial(i Sym) bool {
	_, ok := l.attrSpecial[i]
	return ok
}

// SetAttrSpecial sets the "special" property for a symbol (see
// AttrSpecial).
func (l *Loader) SetAttrSpecial(i Sym, v bool) {
	if v {
		l.attrSpecial[i] = struct{}{}
	} else {
		delete(l.attrSpecial, i)
	}
}

// AttrCgoExportDynamic returns true for a symbol that has been
// specially marked via the "cgo_export_dynamic" compiler directive
// written by cgo (in response to //export directives in the source).
func (l *Loader) AttrCgoExportDynamic(i Sym) bool {
	_, ok := l.attrCgoExportDynamic[i]
	return ok
}

// SetAttrCgoExportDynamic sets the "cgo_export_dynamic" for a symbol
// (see AttrCgoExportDynamic).
func (l *Loader) SetAttrCgoExportDynamic(i Sym, v bool) {
	if v {
		l.attrCgoExportDynamic[i] = struct{}{}
	} else {
		delete(l.attrCgoExportDynamic, i)
	}
}

// AttrCgoExportStatic returns true for a symbol that has been
// specially marked via the "cgo_export_static" directive
// written by cgo.
func (l *Loader) AttrCgoExportStatic(i Sym) bool {
	_, ok := l.attrCgoExportStatic[i]
	return ok
}

// SetAttrCgoExportStatic sets the "cgo_export_static" for a symbol
// (see AttrCgoExportStatic).
func (l *Loader) SetAttrCgoExportStatic(i Sym, v bool) {
	if v {
		l.attrCgoExportStatic[i] = struct{}{}
	} else {
		delete(l.attrCgoExportStatic, i)
	}
}

func (l *Loader) AttrCgoExport(i Sym) bool {
	return l.AttrCgoExportDynamic(i) || l.AttrCgoExportStatic(i)
}

// AttrReadOnly returns true for a symbol whose underlying data
// is stored via a read-only mmap.
func (l *Loader) AttrReadOnly(i Sym) bool {
	if v, ok := l.attrReadOnly[i]; ok {
		return v
	}
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		if pp.objidx != 0 {
			return l.objs[pp.objidx].r.ReadOnly()
		}
		return false
	}
	r, _ := l.toLocal(i)
	return r.ReadOnly()
}

// SetAttrReadOnly sets the "data is read only" property for a symbol
// (see AttrReadOnly).
func (l *Loader) SetAttrReadOnly(i Sym, v bool) {
	l.attrReadOnly[i] = v
}

// AttrSubSymbol returns true for symbols that are listed as a
// sub-symbol of some other outer symbol. The sub/outer mechanism is
// used when loading host objects (sections from the host object
// become regular linker symbols and symbols go on the Sub list of
// their section) and for constructing the global offset table when
// internally linking a dynamic executable.
func (l *Loader) AttrSubSymbol(i Sym) bool {
	// we don't explicitly store this attribute any more -- return
	// a value based on the sub-symbol setting.
	return l.OuterSym(i) != 0
}

// AttrContainer returns true for symbols that are listed as a
// sub-symbol of some other outer symbol. The sub/outer mechanism is
// used when loading host objects (sections from the host object
// become regular linker symbols and symbols go on the Sub list of
// their section) and for constructing the global offset table when
// internally linking a dynamic executable.
func (l *Loader) AttrContainer(i Sym) bool {
	// we don't explicitly store this attribute any more -- return
	// a value based on the sub-symbol setting.
	return l.SubSym(i) != 0
}

// Note that we don't have SetAttrSubSymbol' or 'SetAttrContainer' methods
// in the loader; clients should just use methods like PrependSub
// to establish these relationships

// Returns whether the i-th symbol has ReflectMethod attribute set.
func (l *Loader) IsReflectMethod(i Sym) bool {
	return l.SymAttr(i)&goobj2.SymFlagReflectMethod != 0
}

// Returns whether the i-th symbol is nosplit.
func (l *Loader) IsNoSplit(i Sym) bool {
	return l.SymAttr(i)&goobj2.SymFlagNoSplit != 0
}

// Returns whether this is a Go type symbol.
func (l *Loader) IsGoType(i Sym) bool {
	return l.SymAttr(i)&goobj2.SymFlagGoType != 0
}

// Returns whether this is a "go.itablink.*" symbol.
func (l *Loader) IsItabLink(i Sym) bool {
	if _, ok := l.itablink[i]; ok {
		return true
	}
	return false
}

// growValues grows the slice used to store symbol values.
func (l *Loader) growValues(reqLen int) {
	curLen := len(l.values)
	if reqLen > curLen {
		l.values = append(l.values, make([]int64, reqLen+1-curLen)...)
	}
}

// SymValue returns the value of the i-th symbol. i is global index.
func (l *Loader) SymValue(i Sym) int64 {
	return l.values[i]
}

// SetSymValue sets the value of the i-th symbol. i is global index.
func (l *Loader) SetSymValue(i Sym, val int64) {
	l.values[i] = val
}

// Returns the symbol content of the i-th symbol. i is global index.
func (l *Loader) Data(i Sym) []byte {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		if pp != nil {
			return pp.data
		}
		return nil
	}
	r, li := l.toLocal(i)
	return r.Data(li)
}

// SymAlign returns the alignment for a symbol.
func (l *Loader) SymAlign(i Sym) int32 {
	// If an alignment has been recorded, return that.
	if align, ok := l.align[i]; ok {
		return align
	}
	// TODO: would it make sense to return an arch-specific
	// alignment depending on section type? E.g. STEXT => 32,
	// SDATA => 1, etc?
	return 0
}

// SetSymAlign sets the alignment for a symbol.
func (l *Loader) SetSymAlign(i Sym, align int32) {
	// reject bad synbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetSymAlign")
	}
	// Reject nonsense alignments.
	// TODO: do we need this?
	if align < 0 {
		panic("bad alignment value")
	}
	if align == 0 {
		delete(l.align, i)
	} else {
		// Alignment should be a power of 2.
		if bits.OnesCount32(uint32(align)) != 1 {
			panic("bad alignment value")
		}
		l.align[i] = align
	}
}

// SymDynImplib returns the "dynimplib" attribute for the specified
// symbol, making up a portion of the info for a symbol specified
// on a "cgo_import_dynamic" compiler directive.
func (l *Loader) SymDynimplib(i Sym) string {
	return l.dynimplib[i]
}

// SetSymDynimplib sets the "dynimplib" attribute for a symbol.
func (l *Loader) SetSymDynimplib(i Sym, value string) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetDynimplib")
	}
	if value == "" {
		delete(l.dynimplib, i)
	} else {
		l.dynimplib[i] = value
	}
}

// SymDynimpvers returns the "dynimpvers" attribute for the specified
// symbol, making up a portion of the info for a symbol specified
// on a "cgo_import_dynamic" compiler directive.
func (l *Loader) SymDynimpvers(i Sym) string {
	return l.dynimpvers[i]
}

// SetSymDynimpvers sets the "dynimpvers" attribute for a symbol.
func (l *Loader) SetSymDynimpvers(i Sym, value string) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetDynimpvers")
	}
	if value == "" {
		delete(l.dynimpvers, i)
	} else {
		l.dynimpvers[i] = value
	}
}

// SymExtname returns the "extname" value for the specified
// symbol.
func (l *Loader) SymExtname(i Sym) string {
	if s, ok := l.extname[i]; ok {
		return s
	}
	return l.SymName(i)
}

// SetSymExtname sets the  "extname" attribute for a symbol.
func (l *Loader) SetSymExtname(i Sym, value string) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetExtname")
	}
	if value == "" {
		delete(l.extname, i)
	} else {
		l.extname[i] = value
	}
}

// SymElfType returns the previously recorded ELF type for a symbol
// (used only for symbols read from shared libraries by ldshlibsyms).
// It is not set for symbols defined by the packages being linked or
// by symbols read by ldelf (and so is left as elf.STT_NOTYPE).
func (l *Loader) SymElfType(i Sym) elf.SymType {
	if et, ok := l.elfType[i]; ok {
		return et
	}
	return elf.STT_NOTYPE
}

// SetSymElfType sets the elf type attribute for a symbol.
func (l *Loader) SetSymElfType(i Sym, et elf.SymType) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetSymElfType")
	}
	if et == elf.STT_NOTYPE {
		delete(l.elfType, i)
	} else {
		l.elfType[i] = et
	}
}

// SymPlt returns the plt value for pe symbols.
func (l *Loader) SymPlt(s Sym) int32 {
	if v, ok := l.plt[s]; ok {
		return v
	}
	return -1
}

// SetPlt sets the plt value for pe symbols.
func (l *Loader) SetPlt(i Sym, v int32) {
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol for SetPlt")
	}
	if v == -1 {
		delete(l.plt, i)
	} else {
		l.plt[i] = v
	}
}

// SymGot returns the got value for pe symbols.
func (l *Loader) SymGot(s Sym) int32 {
	if v, ok := l.got[s]; ok {
		return v
	}
	return -1
}

// SetGot sets the got value for pe symbols.
func (l *Loader) SetGot(i Sym, v int32) {
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol for SetGot")
	}
	if v == -1 {
		delete(l.got, i)
	} else {
		l.got[i] = v
	}
}

// SymDynid returns the "dynid" property for the specified symbol.
func (l *Loader) SymDynid(i Sym) int32 {
	if s, ok := l.dynid[i]; ok {
		return s
	}
	return -1
}

// SetSymDynid sets the "dynid" property for a symbol.
func (l *Loader) SetSymDynid(i Sym, val int32) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetSymDynid")
	}
	if val == -1 {
		delete(l.dynid, i)
	} else {
		l.dynid[i] = val
	}
}

// SymGoType returns the 'Gotype' property for a given symbol (set by
// the Go compiler for variable symbols). This version relies on
// reading aux symbols for the target sym -- it could be that a faster
// approach would be to check for gotype during preload and copy the
// results in to a map (might want to try this at some point and see
// if it helps speed things up).
func (l *Loader) SymGoType(i Sym) Sym {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		return pp.gotype
	}
	r, li := l.toLocal(i)
	auxs := r.Auxs2(li)
	for j := range auxs {
		a := &auxs[j]
		switch a.Type() {
		case goobj2.AuxGotype:
			return l.resolve(r, a.Sym())
		}
	}
	return 0
}

// SymUnit returns the compilation unit for a given symbol (which will
// typically be nil for external or linker-manufactured symbols).
func (l *Loader) SymUnit(i Sym) *sym.CompilationUnit {
	if l.IsExternal(i) {
		pp := l.getPayload(i)
		if pp.objidx != 0 {
			r := l.objs[pp.objidx].r
			return r.unit
		}
		return nil
	}
	r, _ := l.toLocal(i)
	return r.unit
}

// SymFile returns the file for a symbol, which is normally the
// package the symbol came from (for regular compiler-generated Go
// symbols), but in the case of building with "-linkshared" (when a
// symbol is read from a a shared library), will hold the library
// name.
func (l *Loader) SymFile(i Sym) string {
	if l.IsExternal(i) {
		if f, ok := l.symFile[i]; ok {
			return f
		}
		pp := l.getPayload(i)
		if pp.objidx != 0 {
			r := l.objs[pp.objidx].r
			return r.unit.Lib.File
		}
		return ""
	}
	r, _ := l.toLocal(i)
	return r.unit.Lib.File
}

// SetSymFile sets the file attribute for a symbol. This is
// needed mainly for external symbols, specifically those imported
// from shared libraries.
func (l *Loader) SetSymFile(i Sym, file string) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetSymFile")
	}
	if !l.IsExternal(i) {
		panic("can't set file for non-external sym")
	}
	l.symFile[i] = file
}

// SymLocalentry returns the "local entry" value for the specified
// symbol.
func (l *Loader) SymLocalentry(i Sym) uint8 {
	return l.localentry[i]
}

// SetSymLocalentry sets the "local entry" attribute for a symbol.
func (l *Loader) SetSymLocalentry(i Sym, value uint8) {
	// reject bad symbols
	if i >= Sym(len(l.objSyms)) || i == 0 {
		panic("bad symbol index in SetSymLocalentry")
	}
	if value == 0 {
		delete(l.localentry, i)
	} else {
		l.localentry[i] = value
	}
}

// Returns the number of aux symbols given a global index.
func (l *Loader) NAux(i Sym) int {
	if l.IsExternal(i) {
		return 0
	}
	r, li := l.toLocal(i)
	return r.NAux(li)
}

// Returns the "handle" to the j-th aux symbol of the i-th symbol.
func (l *Loader) Aux2(i Sym, j int) Aux2 {
	if l.IsExternal(i) {
		return Aux2{}
	}
	r, li := l.toLocal(i)
	if j >= r.NAux(li) {
		return Aux2{}
	}
	return Aux2{r.Aux2(li, j), r, l}
}

// GetFuncDwarfAuxSyms collects and returns the auxiliary DWARF
// symbols associated with a given function symbol.  Prior to the
// introduction of the loader, this was done purely using name
// lookups, e.f. for function with name XYZ we would then look up
// go.info.XYZ, etc.
// FIXME: once all of dwarfgen is converted over to the loader,
// it would save some space to make these aux symbols nameless.
func (l *Loader) GetFuncDwarfAuxSyms(fnSymIdx Sym) (auxDwarfInfo, auxDwarfLoc, auxDwarfRanges, auxDwarfLines Sym) {
	if l.SymType(fnSymIdx) != sym.STEXT {
		log.Fatalf("error: non-function sym %d/%s t=%s passed to GetFuncDwarfAuxSyms", fnSymIdx, l.SymName(fnSymIdx), l.SymType(fnSymIdx).String())
	}
	if l.IsExternal(fnSymIdx) {
		// Current expectation is that any external function will
		// not have auxsyms.
		return
	}
	r, li := l.toLocal(fnSymIdx)
	auxs := r.Auxs2(li)
	for i := range auxs {
		a := &auxs[i]
		switch a.Type() {
		case goobj2.AuxDwarfInfo:
			auxDwarfInfo = l.resolve(r, a.Sym())
			if l.SymType(auxDwarfInfo) != sym.SDWARFINFO {
				panic("aux dwarf info sym with wrong type")
			}
		case goobj2.AuxDwarfLoc:
			auxDwarfLoc = l.resolve(r, a.Sym())
			if l.SymType(auxDwarfLoc) != sym.SDWARFLOC {
				panic("aux dwarf loc sym with wrong type")
			}
		case goobj2.AuxDwarfRanges:
			auxDwarfRanges = l.resolve(r, a.Sym())
			if l.SymType(auxDwarfRanges) != sym.SDWARFRANGE {
				panic("aux dwarf ranges sym with wrong type")
			}
		case goobj2.AuxDwarfLines:
			auxDwarfLines = l.resolve(r, a.Sym())
			if l.SymType(auxDwarfLines) != sym.SDWARFLINES {
				panic("aux dwarf lines sym with wrong type")
			}
		}
	}
	return
}

// PrependSub prepends 'sub' onto the sub list for outer symbol 'outer'.
// Will panic if 'sub' already has an outer sym or sub sym.
// FIXME: should this be instead a method on SymbolBuilder?
func (l *Loader) PrependSub(outer Sym, sub Sym) {
	// NB: this presupposes that an outer sym can't be a sub symbol of
	// some other outer-outer sym (I'm assuming this is true, but I
	// haven't tested exhaustively).
	if l.OuterSym(outer) != 0 {
		panic("outer has outer itself")
	}
	if l.SubSym(sub) != 0 {
		panic("sub set for subsym")
	}
	if l.OuterSym(sub) != 0 {
		panic("outer already set for subsym")
	}
	l.sub[sub] = l.sub[outer]
	l.sub[outer] = sub
	l.outer[sub] = outer
}

// OuterSym gets the outer symbol for host object loaded symbols.
func (l *Loader) OuterSym(i Sym) Sym {
	// FIXME: add check for isExternal?
	return l.outer[i]
}

// SubSym gets the subsymbol for host object loaded symbols.
func (l *Loader) SubSym(i Sym) Sym {
	// NB: note -- no check for l.isExternal(), since I am pretty sure
	// that later phases in the linker set subsym for "type." syms
	return l.sub[i]
}

// Initialize Reachable bitmap and its siblings for running deadcode pass.
func (l *Loader) InitReachable() {
	l.growAttrBitmaps(l.NSym() + 1)
}

type symWithVal struct {
	s Sym
	v int64
}
type bySymValue []symWithVal

func (s bySymValue) Len() int           { return len(s) }
func (s bySymValue) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s bySymValue) Less(i, j int) bool { return s[i].v < s[j].v }

// SortSub walks through the sub-symbols for 's' and sorts them
// in place by increasing value. Return value is the new
// sub symbol for the specified outer symbol.
func (l *Loader) SortSub(s Sym) Sym {

	if s == 0 || l.sub[s] == 0 {
		return s
	}

	// Sort symbols using a slice first. Use a stable sort on the off
	// chance that there's more than once symbol with the same value,
	// so as to preserve reproducible builds.
	sl := []symWithVal{}
	for ss := l.sub[s]; ss != 0; ss = l.sub[ss] {
		sl = append(sl, symWithVal{s: ss, v: l.SymValue(ss)})
	}
	sort.Stable(bySymValue(sl))

	// Then apply any changes needed to the sub map.
	ns := Sym(0)
	for i := len(sl) - 1; i >= 0; i-- {
		s := sl[i].s
		l.sub[s] = ns
		ns = s
	}

	// Update sub for outer symbol, then return
	l.sub[s] = sl[0].s
	return sl[0].s
}

// Insure that reachable bitmap and its siblings have enough size.
func (l *Loader) growAttrBitmaps(reqLen int) {
	if reqLen > l.attrReachable.Len() {
		// These are indexed by global symbol
		l.attrReachable = growBitmap(reqLen, l.attrReachable)
		l.attrOnList = growBitmap(reqLen, l.attrOnList)
		l.attrLocal = growBitmap(reqLen, l.attrLocal)
		l.attrNotInSymbolTable = growBitmap(reqLen, l.attrNotInSymbolTable)
	}
	l.growExtAttrBitmaps()
}

func (l *Loader) growExtAttrBitmaps() {
	// These are indexed by external symbol index (e.g. l.extIndex(i))
	extReqLen := len(l.payloads)
	if extReqLen > l.attrVisibilityHidden.Len() {
		l.attrVisibilityHidden = growBitmap(extReqLen, l.attrVisibilityHidden)
		l.attrDuplicateOK = growBitmap(extReqLen, l.attrDuplicateOK)
		l.attrShared = growBitmap(extReqLen, l.attrShared)
		l.attrExternal = growBitmap(extReqLen, l.attrExternal)
	}
}

func (relocs *Relocs) Count() int { return len(relocs.rs) }

// At2 returns the j-th reloc for a global symbol.
func (relocs *Relocs) At2(j int) Reloc2 {
	if relocs.l.isExtReader(relocs.r) {
		pp := relocs.l.payloads[relocs.li]
		return Reloc2{&relocs.rs[j], relocs.r, relocs.l, pp.reltypes[j]}
	}
	return Reloc2{&relocs.rs[j], relocs.r, relocs.l, 0}
}

// Relocs returns a Relocs object for the given global sym.
func (l *Loader) Relocs(i Sym) Relocs {
	r, li := l.toLocal(i)
	if r == nil {
		panic(fmt.Sprintf("trying to get oreader for invalid sym %d\n\n", i))
	}
	return l.relocs(r, li)
}

// Relocs returns a Relocs object given a local sym index and reader.
func (l *Loader) relocs(r *oReader, li int) Relocs {
	var rs []goobj2.Reloc2
	if l.isExtReader(r) {
		pp := l.payloads[li]
		rs = pp.relocs
	} else {
		rs = r.Relocs2(li)
	}
	return Relocs{
		rs: rs,
		li: li,
		r:  r,
		l:  l,
	}
}

// RelocByOff implements sort.Interface for sorting relocations by offset.

type RelocByOff []Reloc

func (x RelocByOff) Len() int           { return len(x) }
func (x RelocByOff) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
func (x RelocByOff) Less(i, j int) bool { return x[i].Off < x[j].Off }

// FuncInfo provides hooks to access goobj2.FuncInfo in the objects.
type FuncInfo struct {
	l    *Loader
	r    *oReader
	data []byte
}

func (fi *FuncInfo) Valid() bool { return fi.r != nil }

func (fi *FuncInfo) Locals() int {
	return int((*goobj2.FuncInfo)(nil).ReadLocals(fi.data))
}

func (fi *FuncInfo) Pcsp() []byte {
	pcsp, end := (*goobj2.FuncInfo)(nil).ReadPcsp(fi.data)
	return fi.r.BytesAt(fi.r.PcdataBase()+pcsp, int(end-pcsp))
}

// TODO: more accessors.

func (l *Loader) FuncInfo(i Sym) FuncInfo {
	if l.IsExternal(i) {
		return FuncInfo{}
	}
	r, li := l.toLocal(i)
	auxs := r.Auxs2(li)
	for j := range auxs {
		a := &auxs[j]
		if a.Type() == goobj2.AuxFuncInfo {
			b := r.Data(int(a.Sym().SymIdx))
			return FuncInfo{l, r, b}
		}
	}
	return FuncInfo{}
}

// Preload a package: add autolibs, add defined package symbols to the symbol table.
// Does not add non-package symbols yet, which will be done in LoadNonpkgSyms.
// Does not read symbol data.
func (l *Loader) Preload(syms *sym.Symbols, f *bio.Reader, lib *sym.Library, unit *sym.CompilationUnit, length int64, flags int) {
	roObject, readonly, err := f.Slice(uint64(length))
	if err != nil {
		log.Fatal("cannot read object file:", err)
	}
	r := goobj2.NewReaderFromBytes(roObject, readonly)
	if r == nil {
		if len(roObject) >= 8 && bytes.Equal(roObject[:8], []byte("\x00go114ld")) {
			log.Fatalf("found object file %s in old format, but -go115newobj is true\nset -go115newobj consistently in all -gcflags, -asmflags, and -ldflags", f.File().Name())
		}
		panic("cannot read object file")
	}
	localSymVersion := syms.IncVersion()
	pkgprefix := objabi.PathToPrefix(lib.Pkg) + "."
	ndef := r.NSym()
	nnonpkgdef := r.NNonpkgdef()
	or := &oReader{r, unit, localSymVersion, r.Flags(), pkgprefix, make([]Sym, ndef+nnonpkgdef+r.NNonpkgref()), ndef, uint32(len(l.objs))}

	// Autolib
	lib.ImportStrings = append(lib.ImportStrings, r.Autolib()...)

	// DWARF file table
	nfile := r.NDwarfFile()
	unit.DWARFFileTable = make([]string, nfile)
	for i := range unit.DWARFFileTable {
		unit.DWARFFileTable[i] = r.DwarfFile(i)
	}

	l.addObj(lib.Pkg, or)
	l.preloadSyms(or, pkgDef)

	// The caller expects us consuming all the data
	f.MustSeek(length, os.SEEK_CUR)
}

// Preload symbols of given kind from an object.
func (l *Loader) preloadSyms(r *oReader, kind int) {
	ndef := r.NSym()
	nnonpkgdef := r.NNonpkgdef()
	var start, end int
	switch kind {
	case pkgDef:
		start = 0
		end = ndef
	case nonPkgDef:
		start = ndef
		end = ndef + nnonpkgdef
	default:
		panic("preloadSyms: bad kind")
	}
	l.growSyms(len(l.objSyms) + end - start)
	l.growAttrBitmaps(len(l.objSyms) + end - start)
	for i := start; i < end; i++ {
		osym := r.Sym2(i)
		name := strings.Replace(osym.Name(r.Reader), "\"\".", r.pkgprefix, -1)
		v := abiToVer(osym.ABI(), r.version)
		dupok := osym.Dupok()
		gi, added := l.AddSym(name, v, r, i, kind, dupok, sym.AbiSymKindToSymKind[objabi.SymKind(osym.Type())])
		r.syms[i] = gi
		if !added {
			continue
		}
		if osym.TopFrame() {
			l.SetAttrTopFrame(gi, true)
		}
		if osym.Local() {
			l.SetAttrLocal(gi, true)
		}
		if strings.HasPrefix(name, "go.itablink.") {
			l.itablink[gi] = struct{}{}
		}
		if strings.HasPrefix(name, "runtime.") {
			if bi := goobj2.BuiltinIdx(name, v); bi != -1 {
				// This is a definition of a builtin symbol. Record where it is.
				l.builtinSyms[bi] = gi
			}
		}
		if strings.HasPrefix(name, "go.string.") ||
			strings.HasPrefix(name, "gclocals·") ||
			strings.HasPrefix(name, "runtime.gcbits.") {
			l.SetAttrNotInSymbolTable(gi, true)
		}
		if a := osym.Align(); a != 0 {
			l.SetSymAlign(gi, int32(a))
		}
	}
}

// Add non-package symbols and references to external symbols (which are always
// named).
func (l *Loader) LoadNonpkgSyms(syms *sym.Symbols) {
	for _, o := range l.objs[1:] {
		l.preloadSyms(o.r, nonPkgDef)
	}
	for _, o := range l.objs[1:] {
		loadObjRefs(l, o.r, syms)
	}
}

func loadObjRefs(l *Loader, r *oReader, syms *sym.Symbols) {
	ndef := r.NSym() + r.NNonpkgdef()
	for i, n := 0, r.NNonpkgref(); i < n; i++ {
		osym := r.Sym2(ndef + i)
		name := strings.Replace(osym.Name(r.Reader), "\"\".", r.pkgprefix, -1)
		v := abiToVer(osym.ABI(), r.version)
		r.syms[ndef+i] = l.LookupOrCreateSym(name, v)
		if osym.Local() {
			gi := r.syms[ndef+i]
			l.SetAttrLocal(gi, true)
		}
	}
}

func abiToVer(abi uint16, localSymVersion int) int {
	var v int
	if abi == goobj2.SymABIstatic {
		// Static
		v = localSymVersion
	} else if abiver := sym.ABIToVersion(obj.ABI(abi)); abiver != -1 {
		// Note that data symbols are "ABI0", which maps to version 0.
		v = abiver
	} else {
		log.Fatalf("invalid symbol ABI: %d", abi)
	}
	return v
}

func preprocess(arch *sys.Arch, s *sym.Symbol) {
	if s.Name != "" && s.Name[0] == '$' && len(s.Name) > 5 && s.Type == 0 && len(s.P) == 0 {
		x, err := strconv.ParseUint(s.Name[5:], 16, 64)
		if err != nil {
			log.Panicf("failed to parse $-symbol %s: %v", s.Name, err)
		}
		s.Type = sym.SRODATA
		s.Attr |= sym.AttrLocal
		switch s.Name[:5] {
		case "$f32.":
			if uint64(uint32(x)) != x {
				log.Panicf("$-symbol %s too large: %d", s.Name, x)
			}
			s.AddUint32(arch, uint32(x))
		case "$f64.", "$i64.":
			s.AddUint64(arch, x)
		default:
			log.Panicf("unrecognized $-symbol: %s", s.Name)
		}
	}
}

// Load full contents.
func (l *Loader) LoadFull(arch *sys.Arch, syms *sym.Symbols) {
	// create all Symbols first.
	l.growSyms(l.NSym())

	nr := 0 // total number of sym.Reloc's we'll need
	for _, o := range l.objs[1:] {
		nr += loadObjSyms(l, syms, o.r)
	}

	// Make a first pass through the external symbols, making
	// sure that each external symbol has a non-nil entry in
	// l.Syms (note that relocations and symbol content will
	// be copied in a later loop).
	toConvert := make([]Sym, 0, len(l.payloads))
	for _, i := range l.extReader.syms {
		sname := l.RawSymName(i)
		if !l.attrReachable.Has(i) && !strings.HasPrefix(sname, "gofile..") { // XXX file symbols are used but not marked
			continue
		}
		pp := l.getPayload(i)
		nr += len(pp.relocs)
		// create and install the sym.Symbol here so that l.Syms will
		// be fully populated when we do relocation processing and
		// outer/sub processing below. Note that once we do this,
		// we'll need to get at the payload for a symbol with direct
		// reference to l.payloads[] as opposed to calling l.getPayload().
		s := l.allocSym(sname, 0)
		l.installSym(i, s)
		toConvert = append(toConvert, i)
	}

	// allocate a single large slab of relocations for all live symbols
	l.relocBatch = make([]sym.Reloc, nr)

	// convert payload-based external symbols into sym.Symbol-based
	for _, i := range toConvert {

		// Copy kind/size/value etc.
		pp := l.payloads[l.extIndex(i)]
		s := l.Syms[i]
		s.Version = int16(pp.ver)
		s.Type = pp.kind
		s.Size = pp.size
		if pp.gotype != 0 {
			s.Gotype = l.Syms[pp.gotype]
		}
		if f, ok := l.symFile[i]; ok {
			s.File = f
		} else if pp.objidx != 0 {
			s.File = l.objs[pp.objidx].r.unit.Lib.File
		}

		// Copy relocations
		batch := l.relocBatch
		s.R = batch[:len(pp.relocs):len(pp.relocs)]
		l.relocBatch = batch[len(pp.relocs):]
		relocs := l.Relocs(i)
		l.convertRelocations(&relocs, s, false)

		// Copy data
		s.P = pp.data

		// Transfer over attributes.
		l.migrateAttributes(i, s)

		// Preprocess symbol. May set 'AttrLocal'.
		preprocess(arch, s)
	}

	// load contents of defined symbols
	for _, o := range l.objs[1:] {
		loadObjFull(l, o.r)
	}

	// Note: resolution of ABI aliases is now also handled in
	// loader.convertRelocations, so once the host object loaders move
	// completely to loader.Sym, we can remove the code below.

	// Resolve ABI aliases for external symbols. This is only
	// needed for internal cgo linking.
	// (The old code does this in deadcode, but deadcode2 doesn't
	// do this.)
	for _, i := range l.extReader.syms {
		if s := l.Syms[i]; s != nil && s.Attr.Reachable() {
			for ri := range s.R {
				r := &s.R[ri]
				if r.Sym != nil && r.Sym.Type == sym.SABIALIAS {
					r.Sym = r.Sym.R[0].Sym
				}
			}
		}
	}
}

// ResolveABIAlias given a symbol returns the ABI alias target of that
// symbol. If the sym in question is not an alias, the sym itself is
// returned.
func (l *Loader) ResolveABIAlias(s Sym) Sym {
	if l.SymType(s) != sym.SABIALIAS {
		return s
	}
	relocs := l.Relocs(s)
	target := relocs.At2(0).Sym()
	if l.SymType(target) == sym.SABIALIAS {
		panic(fmt.Sprintf("ABI alias %s references another ABI alias %s", l.SymName(s), l.SymName(target)))
	}
	return target
}

// PropagateSymbolChangesBackToLoader is a temporary shim function
// that copies over a given sym.Symbol into the equivalent representation
// in the loader world. The intent is to enable converting a given
// linker phase/pass from dealing with sym.Symbol's to a modernized
// pass that works with loader.Sym, in cases where the "loader.Sym
// wavefront" has not yet reached the pass in question. For such work
// the recipe is to first call PropagateSymbolChangesBackToLoader(),
// then exexute the pass working with the loader, then call
// PropagateLoaderChangesToSymbols to copy the changes made by the
// pass back to the sym.Symbol world.
func (l *Loader) PropagateSymbolChangesBackToLoader() {

	// For the moment we only copy symbol values, and we don't touch
	// any new sym.Symbols created since loadlibfull() was run. This
	// seems to be what's needed for DWARF gen.
	for i := Sym(1); i < Sym(len(l.objSyms)); i++ {
		s := l.Syms[i]
		if s != nil {
			if s.Value != l.SymValue(i) {
				l.SetSymValue(i, s.Value)
			}
		}
	}
}

// PropagateLoaderChangesToSymbols is a temporary shim function that
// takes a list of loader.Sym symbols and works to copy their contents
// and attributes over to a corresponding sym.Symbol. The parameter
// anonVerReplacement specifies a version number for any new anonymous
// symbols encountered on the list, when creating sym.Symbols for them
// (or zero if we don't expect to encounter any new anon symbols). See
// the PropagateSymbolChangesBackToLoader header comment for more
// info.
//
// WARNING: this function is brittle and depends heavily on loader
// implementation. A key problem with doing this is that as things
// stand at the moment, some sym.Symbol contents/attributes are
// populated only when converting from loader.Sym to sym.Symbol in
// loadlibfull, meaning we may wipe out some information when copying
// back.

func (l *Loader) PropagateLoaderChangesToSymbols(toconvert []Sym, anonVerReplacement int) []*sym.Symbol {

	result := []*sym.Symbol{}
	relocfixup := []Sym{}

	// Note: this loop needs to allow for the possibility that we may
	// see "new" symbols on the 'toconvert' list that come from object
	// files (for example, DWARF location lists), as opposed to just
	// newly manufactured symbols (ex: DWARF section symbols such as
	// ".debug_info").  This means that we have to be careful not to
	// stomp on sym.Symbol attributes/content that was set up in
	// in loadlibfull().

	// Also note that in order for the relocation fixup to work, we
	// have to do this in two passes -- one pass to create the symbols,
	// and then a second fix up the relocations once all necessary
	// sym.Symbols are created.

	// First pass, symbol creation and symbol data fixup.
	for _, cand := range toconvert {

		sn := l.SymName(cand)
		sv := l.SymVersion(cand)
		st := l.SymType(cand)
		if sv < 0 {
			if anonVerReplacement == 0 {
				panic("expected valid anon version replacement")
			}
			sv = anonVerReplacement
		}

		s := l.Syms[cand]

		isnew := false
		if sn == "" {
			// Don't install anonymous symbols in the lookup tab.
			if s == nil {
				s = l.allocSym(sn, sv)
				l.installSym(cand, s)
			}
			isnew = true
		} else {
			if s != nil {
				// Already have a symbol for this -- it must be
				// something that was previously processed by
				// loadObjFull. Note that the symbol in question may
				// or may not be in the name lookup map.
			} else {
				isnew = true
				s = l.SymLookup(sn, sv)
			}
		}
		result = append(result, s)

		// Always copy these from new to old.
		s.Value = l.SymValue(cand)
		s.Type = st

		// If the data for a symbol has increased in size, make sure
		// we bring the new content across.
		relfix := isnew
		if isnew || len(l.Data(cand)) > len(s.P) {
			s.P = l.Data(cand)
			s.Size = int64(len(s.P))
			relfix = true
		}

		// For 'new' symbols, copy other content (such as Gotype,
		// sym file, relocations, etc).
		if isnew {
			if gt := l.SymGoType(cand); gt != 0 {
				s.Gotype = l.Syms[gt]
			}
			if f, ok := l.symFile[cand]; ok {
				s.File = f
			} else {
				r, _ := l.toLocal(cand)
				if r != nil && r != l.extReader {
					s.File = l.SymFile(cand)
				}
			}
		}

		if relfix {
			relocfixup = append(relocfixup, cand)
		}

		// If new symbol, call a helper to migrate attributes.
		// Otherwise touch only not-in-symbol-table, since there are
		// some attrs that are only set up at the point where we
		// convert loader.Sym to sym.Symbol.
		if isnew {
			l.migrateAttributes(cand, s)
		} else {
			if l.AttrNotInSymbolTable(cand) {
				s.Attr.Set(sym.AttrNotInSymbolTable, true)
			}
		}
	}

	// Second pass to fix up relocations.
	for _, cand := range relocfixup {
		s := l.Syms[cand]
		relocs := l.Relocs(cand)
		if len(s.R) != relocs.Count() {
			s.R = make([]sym.Reloc, relocs.Count())
		}
		l.convertRelocations(&relocs, s, true)
	}

	return result
}

// ExtractSymbols grabs the symbols out of the loader for work that hasn't been
// ported to the new symbol type.
func (l *Loader) ExtractSymbols(syms *sym.Symbols, rp map[*sym.Symbol]*sym.Symbol) {
	// Add symbols to the ctxt.Syms lookup table. This explicitly skips things
	// created via loader.Create (marked with versions less than zero), since
	// if we tried to add these we'd wind up with collisions. We do, however,
	// add these symbols to the list of global symbols so that other future
	// steps (like pclntab generation) can find these symbols if neceassary.
	// Along the way, update the version from the negative anon version to
	// something larger than sym.SymVerStatic (needed so that
	// sym.symbol.IsFileLocal() works properly).
	anonVerReplacement := syms.IncVersion()
	for _, s := range l.Syms {
		if s == nil {
			continue
		}
		syms.Allsym = append(syms.Allsym, s) // XXX still add to Allsym for now, as there are code looping through Allsym
		if s.Version < 0 {
			s.Version = int16(anonVerReplacement)
		}
	}

	for i, s := range l.Reachparent {
		if i == 0 {
			continue
		}
		rp[l.Syms[i]] = l.Syms[s]
	}

	// Provide lookup functions for sym.Symbols.
	l.SymLookup = func(name string, ver int) *sym.Symbol {
		i := l.LookupOrCreateSym(name, ver)
		if s := l.Syms[i]; s != nil {
			return s
		}
		s := l.allocSym(name, ver)
		l.installSym(i, s)
		syms.Allsym = append(syms.Allsym, s) // XXX see above
		return s
	}
	syms.Lookup = l.SymLookup
	syms.ROLookup = func(name string, ver int) *sym.Symbol {
		i := l.Lookup(name, ver)
		return l.Syms[i]
	}
	syms.Newsym = func(name string, ver int) *sym.Symbol {
		i := l.newExtSym(name, ver)
		s := l.allocSym(name, ver)
		l.installSym(i, s)
		syms.Allsym = append(syms.Allsym, s) // XXX see above
		return s
	}
}

// allocSym allocates a new symbol backing.
func (l *Loader) allocSym(name string, version int) *sym.Symbol {
	batch := l.symBatch
	if len(batch) == 0 {
		batch = make([]sym.Symbol, 1000)
	}
	s := &batch[0]
	l.symBatch = batch[1:]

	s.Dynid = -1
	s.Name = name
	s.Version = int16(version)

	return s
}

// installSym sets the underlying sym.Symbol for the specified sym index.
func (l *Loader) installSym(i Sym, s *sym.Symbol) {
	if s == nil {
		panic("installSym nil symbol")
	}
	if l.Syms[i] != nil {
		panic("sym already present in installSym")
	}
	l.Syms[i] = s
}

// addNewSym adds a new sym.Symbol to the i-th index in the list of symbols.
func (l *Loader) addNewSym(i Sym, name string, ver int, unit *sym.CompilationUnit, t sym.SymKind) *sym.Symbol {
	s := l.allocSym(name, ver)
	if s.Type != 0 && s.Type != sym.SXREF {
		fmt.Println("symbol already processed:", unit.Lib, i, s)
		panic("symbol already processed")
	}
	if t == sym.SBSS && (s.Type == sym.SRODATA || s.Type == sym.SNOPTRBSS) {
		t = s.Type
	}
	s.Type = t
	s.Unit = unit
	l.growSyms(int(i))
	l.installSym(i, s)
	return s
}

// loadObjSyms creates sym.Symbol objects for the live Syms in the
// object corresponding to object reader "r". Return value is the
// number of sym.Reloc entries required for all the new symbols.
func loadObjSyms(l *Loader, syms *sym.Symbols, r *oReader) int {
	nr := 0
	for i, n := 0, r.NSym()+r.NNonpkgdef(); i < n; i++ {
		gi := r.syms[i]
		if r2, i2 := l.toLocal(gi); r2 != r || i2 != i {
			continue // come from a different object
		}
		osym := r.Sym2(i)
		name := strings.Replace(osym.Name(r.Reader), "\"\".", r.pkgprefix, -1)
		t := sym.AbiSymKindToSymKind[objabi.SymKind(osym.Type())]
		// NB: for the test below, we can skip most anonymous symbols
		// since they will never be turned into sym.Symbols (ex:
		// funcdata), however DWARF subprogram DIE symbols (which are
		// nameless) will eventually need to be turned into
		// sym.Symbols (with relocations), so the simplest thing to do
		// is include them as part of this loop.
		if name == "" && t != sym.SDWARFINFO {
			continue
		}
		ver := abiToVer(osym.ABI(), r.version)
		if t == sym.SXREF {
			log.Fatalf("bad sxref")
		}
		if t == 0 {
			log.Fatalf("missing type for %s in %s", name, r.unit.Lib)
		}
		if !l.attrReachable.Has(gi) && name != "runtime.addmoduledata" && name != "runtime.lastmoduledatap" {
			// No need to load unreachable symbols.
			// XXX reference to runtime.addmoduledata may be generated later by the linker in plugin mode.
			continue
		}

		s := l.addNewSym(gi, name, ver, r.unit, t)
		l.migrateAttributes(gi, s)
		nr += r.NReloc(i)
	}
	return nr
}

// funcInfoSym records the sym.Symbol for a function, along with a copy
// of the corresponding goobj2.Sym and the index of its FuncInfo aux sym.
// We use this to delay populating FuncInfo until we can batch-allocate
// slices for their sub-objects.
type funcInfoSym struct {
	s    *sym.Symbol  // sym.Symbol for a live function
	osym *goobj2.Sym2 // object file symbol data for that function
	isym int          // global symbol index of FuncInfo aux sym for func
}

// funcAllocInfo records totals/counts for all functions in an objfile;
// used to help with bulk allocation of sym.Symbol sub-objects.
type funcAllocInfo struct {
	symPtr  uint32 // number of *sym.Symbol's needed in file slices
	inlCall uint32 // number of sym.InlinedCall's needed in inltree slices
	pcData  uint32 // number of sym.Pcdata's needed in pdata slices
	fdOff   uint32 // number of int64's needed in all Funcdataoff slices
}

// cloneToExternal takes the existing object file symbol (symIdx)
// and creates a new external symbol payload that is a clone with
// respect to name, version, type, relocations, etc. The idea here
// is that if the linker decides it wants to update the contents of
// a symbol originally discovered as part of an object file, it's
// easier to do this if we make the updates to an external symbol
// payload.
// XXX maybe rename? makeExtPayload?
func (l *Loader) cloneToExternal(symIdx Sym) {
	if l.IsExternal(symIdx) {
		panic("sym is already external, no need for clone")
	}
	l.growSyms(int(symIdx))

	// Read the particulars from object.
	r, li := l.toLocal(symIdx)
	osym := r.Sym2(li)
	sname := strings.Replace(osym.Name(r.Reader), "\"\".", r.pkgprefix, -1)
	sver := abiToVer(osym.ABI(), r.version)
	skind := sym.AbiSymKindToSymKind[objabi.SymKind(osym.Type())]

	// Create new symbol, update version and kind.
	pi := l.newPayload(sname, sver)
	pp := l.payloads[pi]
	pp.kind = skind
	pp.ver = sver
	pp.size = int64(osym.Siz())
	pp.objidx = r.objidx

	// If this is a def, then copy the guts. We expect this case
	// to be very rare (one case it may come up is with -X).
	if li < (r.NSym() + r.NNonpkgdef()) {

		// Copy relocations
		relocs := l.Relocs(symIdx)
		pp.relocs = make([]goobj2.Reloc2, relocs.Count())
		pp.reltypes = make([]objabi.RelocType, relocs.Count())
		for i := range pp.relocs {
			// Copy the relocs slice.
			// Convert local reference to global reference.
			rel := relocs.At2(i)
			pp.relocs[i].Set(rel.Off(), rel.Siz(), 0, rel.Add(), goobj2.SymRef{PkgIdx: 0, SymIdx: uint32(rel.Sym())})
			pp.reltypes[i] = rel.Type()
		}

		// Copy data
		pp.data = r.Data(li)
	}

	// If we're overriding a data symbol, collect the associated
	// Gotype, so as to propagate it to the new symbol.
	auxs := r.Auxs2(li)
	for j := range auxs {
		a := &auxs[j]
		switch a.Type() {
		case goobj2.AuxGotype:
			pp.gotype = l.resolve(r, a.Sym())
		default:
			log.Fatalf("internal error: cloneToExternal applied to %s symbol %s with non-gotype aux data %d", skind.String(), sname, a.Type())
		}
	}

	// Install new payload to global index space.
	// (This needs to happen at the end, as the accessors above
	// need to access the old symbol content.)
	l.objSyms[symIdx] = objSym{l.extReader, pi}
	l.extReader.syms = append(l.extReader.syms, symIdx)
}

// Copy the payload of symbol src to dst. Both src and dst must be external
// symbols.
// The intended use case is that when building/linking against a shared library,
// where we do symbol name mangling, the Go object file may have reference to
// the original symbol name whereas the shared library provides a symbol with
// the mangled name. When we do mangling, we copy payload of mangled to original.
func (l *Loader) CopySym(src, dst Sym) {
	if !l.IsExternal(dst) {
		panic("dst is not external") //l.newExtSym(l.SymName(dst), l.SymVersion(dst))
	}
	if !l.IsExternal(src) {
		panic("src is not external") //l.cloneToExternal(src)
	}
	l.payloads[l.extIndex(dst)] = l.payloads[l.extIndex(src)]
	l.SetSymFile(dst, l.SymFile(src))
	// TODO: other attributes?
}

// CopyAttributes copies over all of the attributes of symbol 'src' to
// symbol 'dst'.
func (l *Loader) CopyAttributes(src Sym, dst Sym) {
	l.SetAttrReachable(dst, l.AttrReachable(src))
	l.SetAttrOnList(dst, l.AttrOnList(src))
	l.SetAttrLocal(dst, l.AttrLocal(src))
	l.SetAttrNotInSymbolTable(dst, l.AttrNotInSymbolTable(src))
	if l.IsExternal(dst) {
		l.SetAttrVisibilityHidden(dst, l.AttrVisibilityHidden(src))
		l.SetAttrDuplicateOK(dst, l.AttrDuplicateOK(src))
		l.SetAttrShared(dst, l.AttrShared(src))
		l.SetAttrExternal(dst, l.AttrExternal(src))
	} else {
		// Some attributes are modifiable only for external symbols.
		// In such cases, don't try to transfer over the attribute
		// from the source even if there is a clash. This comes up
		// when copying attributes from a dupOK ABI wrapper symbol to
		// the real target symbol (which may not be marked dupOK).
	}
	l.SetAttrTopFrame(dst, l.AttrTopFrame(src))
	l.SetAttrSpecial(dst, l.AttrSpecial(src))
	l.SetAttrCgoExportDynamic(dst, l.AttrCgoExportDynamic(src))
	l.SetAttrCgoExportStatic(dst, l.AttrCgoExportStatic(src))
	l.SetAttrReadOnly(dst, l.AttrReadOnly(src))
}

// migrateAttributes copies over all of the attributes of symbol 'src' to
// sym.Symbol 'dst'.
func (l *Loader) migrateAttributes(src Sym, dst *sym.Symbol) {
	dst.Value = l.SymValue(src)
	dst.Align = l.SymAlign(src)

	dst.Attr.Set(sym.AttrReachable, l.AttrReachable(src))
	dst.Attr.Set(sym.AttrOnList, l.AttrOnList(src))
	dst.Attr.Set(sym.AttrLocal, l.AttrLocal(src))
	dst.Attr.Set(sym.AttrNotInSymbolTable, l.AttrNotInSymbolTable(src))
	dst.Attr.Set(sym.AttrNoSplit, l.IsNoSplit(src))
	dst.Attr.Set(sym.AttrVisibilityHidden, l.AttrVisibilityHidden(src))
	dst.Attr.Set(sym.AttrDuplicateOK, l.AttrDuplicateOK(src))
	dst.Attr.Set(sym.AttrShared, l.AttrShared(src))
	dst.Attr.Set(sym.AttrExternal, l.AttrExternal(src))
	dst.Attr.Set(sym.AttrTopFrame, l.AttrTopFrame(src))
	dst.Attr.Set(sym.AttrSpecial, l.AttrSpecial(src))
	dst.Attr.Set(sym.AttrCgoExportDynamic, l.AttrCgoExportDynamic(src))
	dst.Attr.Set(sym.AttrCgoExportStatic, l.AttrCgoExportStatic(src))
	dst.Attr.Set(sym.AttrReadOnly, l.AttrReadOnly(src))

	// Convert outer/sub relationships
	if outer, ok := l.outer[src]; ok {
		dst.Outer = l.Syms[outer]
	}
	if sub, ok := l.sub[src]; ok {
		dst.Sub = l.Syms[sub]
	}

	// Set sub-symbol attribute. FIXME: would be better to do away
	// with this and just use l.OuterSymbol() != 0 elsewhere within
	// the linker.
	dst.Attr.Set(sym.AttrSubSymbol, dst.Outer != nil)

	// Copy over dynimplib, dynimpvers, extname.
	if name, ok := l.extname[src]; ok {
		dst.SetExtname(name)
	}
	if l.SymDynimplib(src) != "" {
		dst.SetDynimplib(l.SymDynimplib(src))
	}
	if l.SymDynimpvers(src) != "" {
		dst.SetDynimpvers(l.SymDynimpvers(src))
	}

	// Copy ELF type if set.
	if et, ok := l.elfType[src]; ok {
		dst.SetElfType(et)
	}

	// Copy pe objects values if set.
	if plt, ok := l.plt[src]; ok {
		dst.SetPlt(plt)
	}
	if got, ok := l.got[src]; ok {
		dst.SetGot(got)
	}

	// Copy dynid
	if dynid, ok := l.dynid[src]; ok {
		dst.Dynid = dynid
	}
}

// CreateExtSym creates a new external symbol with the specified name
// without adding it to any lookup tables, returning a Sym index for it.
func (l *Loader) CreateExtSym(name string, ver int) Sym {
	return l.newExtSym(name, ver)
}

// CreateStaticSym creates a new static symbol with the specified name
// without adding it to any lookup tables, returning a Sym index for it.
func (l *Loader) CreateStaticSym(name string) Sym {
	// Assign a new unique negative version -- this is to mark the
	// symbol so that it can be skipped when ExtractSymbols is adding
	// ext syms to the sym.Symbols hash.
	l.anonVersion--
	return l.newExtSym(name, l.anonVersion)
}

func loadObjFull(l *Loader, r *oReader) {
	lib := r.unit.Lib
	resolveSymRef := func(s goobj2.SymRef) *sym.Symbol {
		i := l.resolve(r, s)
		return l.Syms[i]
	}

	funcs := []funcInfoSym{}
	fdsyms := []*sym.Symbol{}
	var funcAllocCounts funcAllocInfo
	pcdataBase := r.PcdataBase()
	for i, n := 0, r.NSym()+r.NNonpkgdef(); i < n; i++ {
		// A symbol may be a dup or overwritten. In this case, its
		// content will actually be provided by a different object
		// (to which its global index points). Skip those symbols.
		gi := l.toGlobal(r, i)
		var isdup bool
		if r2, i2 := l.toLocal(gi); r2 != r || i2 != i {
			isdup = true
		}

		osym := r.Sym2(i)
		dupok := osym.Dupok()
		if dupok && isdup {
			if l.attrReachable.Has(gi) {
				// A dupok symbol is resolved to another package. We still need
				// to record its presence in the current package, as the trampoline
				// pass expects packages are laid out in dependency order.
				s := l.Syms[gi]
				if s.Type == sym.STEXT {
					lib.DupTextSyms = append(lib.DupTextSyms, s)
				}
			}
			continue
		}

		if isdup {
			continue // come from a different object
		}
		s := l.Syms[gi]
		if s == nil {
			continue
		}

		local := osym.Local()
		makeTypelink := osym.Typelink()
		size := osym.Siz()

		// Symbol data
		s.P = r.Data(i)
		s.Attr.Set(sym.AttrReadOnly, r.ReadOnly())

		// Relocs
		relocs := l.relocs(r, i)
		batch := l.relocBatch
		s.R = batch[:relocs.Count():relocs.Count()]
		l.relocBatch = batch[relocs.Count():]
		l.convertRelocations(&relocs, s, false)

		// Aux symbol info
		isym := -1
		auxs := r.Auxs2(i)
		for j := range auxs {
			a := &auxs[j]
			switch a.Type() {
			case goobj2.AuxGotype:
				typ := resolveSymRef(a.Sym())
				if typ != nil {
					s.Gotype = typ
				}
			case goobj2.AuxFuncdata:
				fdsyms = append(fdsyms, resolveSymRef(a.Sym()))
			case goobj2.AuxFuncInfo:
				if a.Sym().PkgIdx != goobj2.PkgIdxSelf {
					panic("funcinfo symbol not defined in current package")
				}
				isym = int(a.Sym().SymIdx)
			case goobj2.AuxDwarfInfo, goobj2.AuxDwarfLoc, goobj2.AuxDwarfRanges, goobj2.AuxDwarfLines:
				// ignored for now
			default:
				panic("unknown aux type")
			}
		}

		s.File = r.pkgprefix[:len(r.pkgprefix)-1]
		if dupok {
			s.Attr |= sym.AttrDuplicateOK
		}
		if s.Size < int64(size) {
			s.Size = int64(size)
		}
		s.Attr.Set(sym.AttrLocal, local)
		s.Attr.Set(sym.AttrMakeTypelink, makeTypelink)

		if s.Type != sym.STEXT {
			continue
		}

		if isym == -1 {
			continue
		}

		// Record function sym and associated info for additional
		// processing in the loop below.
		fwis := funcInfoSym{s: s, isym: isym, osym: osym}
		funcs = append(funcs, fwis)

		// Read the goobj2.FuncInfo for this text symbol so that we can
		// collect allocation counts. We'll read it again in the loop
		// below.
		b := r.Data(isym)
		info := goobj2.FuncInfo{}
		info.Read(b)
		funcAllocCounts.symPtr += uint32(len(info.File))
		funcAllocCounts.pcData += uint32(len(info.Pcdata))
		funcAllocCounts.inlCall += uint32(len(info.InlTree))
		funcAllocCounts.fdOff += uint32(len(info.Funcdataoff))
	}

	// At this point we can do batch allocation of the sym.FuncInfo's,
	// along with the slices of sub-objects they use.
	fiBatch := make([]sym.FuncInfo, len(funcs))
	inlCallBatch := make([]sym.InlinedCall, funcAllocCounts.inlCall)
	symPtrBatch := make([]*sym.Symbol, funcAllocCounts.symPtr)
	pcDataBatch := make([]sym.Pcdata, funcAllocCounts.pcData)
	fdOffBatch := make([]int64, funcAllocCounts.fdOff)

	// Populate FuncInfo contents for func symbols.
	for fi := 0; fi < len(funcs); fi++ {
		s := funcs[fi].s
		isym := funcs[fi].isym
		osym := funcs[fi].osym

		s.FuncInfo = &fiBatch[0]
		fiBatch = fiBatch[1:]

		b := r.Data(isym)
		info := goobj2.FuncInfo{}
		info.Read(b)

		if osym.NoSplit() {
			s.Attr |= sym.AttrNoSplit
		}
		if osym.ReflectMethod() {
			s.Attr |= sym.AttrReflectMethod
		}
		if r.Flags()&goobj2.ObjFlagShared != 0 {
			s.Attr |= sym.AttrShared
		}
		if osym.TopFrame() {
			s.Attr |= sym.AttrTopFrame
		}

		pc := s.FuncInfo

		if len(info.Funcdataoff) != 0 {
			nfd := len(info.Funcdataoff)
			pc.Funcdata = fdsyms[:nfd:nfd]
			fdsyms = fdsyms[nfd:]
		}

		info.Pcdata = append(info.Pcdata, info.PcdataEnd) // for the ease of knowing where it ends
		pc.Args = int32(info.Args)
		pc.Locals = int32(info.Locals)

		npc := len(info.Pcdata) - 1 // -1 as we appended one above
		pc.Pcdata = pcDataBatch[:npc:npc]
		pcDataBatch = pcDataBatch[npc:]

		nfd := len(info.Funcdataoff)
		pc.Funcdataoff = fdOffBatch[:nfd:nfd]
		fdOffBatch = fdOffBatch[nfd:]

		nsp := len(info.File)
		pc.File = symPtrBatch[:nsp:nsp]
		symPtrBatch = symPtrBatch[nsp:]

		nic := len(info.InlTree)
		pc.InlTree = inlCallBatch[:nic:nic]
		inlCallBatch = inlCallBatch[nic:]

		pc.Pcsp.P = r.BytesAt(pcdataBase+info.Pcsp, int(info.Pcfile-info.Pcsp))
		pc.Pcfile.P = r.BytesAt(pcdataBase+info.Pcfile, int(info.Pcline-info.Pcfile))
		pc.Pcline.P = r.BytesAt(pcdataBase+info.Pcline, int(info.Pcinline-info.Pcline))
		pc.Pcinline.P = r.BytesAt(pcdataBase+info.Pcinline, int(info.Pcdata[0]-info.Pcinline))
		for k := range pc.Pcdata {
			pc.Pcdata[k].P = r.BytesAt(pcdataBase+info.Pcdata[k], int(info.Pcdata[k+1]-info.Pcdata[k]))
		}
		for k := range pc.Funcdataoff {
			pc.Funcdataoff[k] = int64(info.Funcdataoff[k])
		}
		for k := range pc.File {
			pc.File[k] = resolveSymRef(info.File[k])
		}
		for k := range pc.InlTree {
			inl := &info.InlTree[k]
			pc.InlTree[k] = sym.InlinedCall{
				Parent:   inl.Parent,
				File:     resolveSymRef(inl.File),
				Line:     inl.Line,
				Func:     l.SymName(l.resolve(r, inl.Func)),
				ParentPC: inl.ParentPC,
			}
		}

		dupok := osym.Dupok()
		if !dupok {
			if s.Attr.OnList() {
				log.Fatalf("symbol %s listed multiple times", s.Name)
			}
			s.Attr.Set(sym.AttrOnList, true)
			lib.Textp = append(lib.Textp, s)
		} else {
			// there may be a dup in another package
			// put into a temp list and add to text later
			lib.DupTextSyms = append(lib.DupTextSyms, s)
		}
	}
}

// convertRelocations takes a vector of loader.Reloc relocations and
// translates them into an equivalent set of sym.Reloc relocations on
// the symbol "dst", performing fixups along the way for ABI aliases,
// etc. It is assumed that the caller has pre-allocated the dst symbol
// relocations slice. If 'strict' is set, then this method will
// panic if it finds a relocation targeting a nil symbol.
func (l *Loader) convertRelocations(src *Relocs, dst *sym.Symbol, strict bool) {
	for j := range dst.R {
		r := src.At2(j)
		rs := r.Sym()
		sz := r.Siz()
		rt := r.Type()
		if rt == objabi.R_METHODOFF {
			if l.attrReachable.Has(rs) {
				rt = objabi.R_ADDROFF
			} else {
				sz = 0
				rs = 0
			}
		}
		if rt == objabi.R_WEAKADDROFF && !l.attrReachable.Has(rs) {
			rs = 0
			sz = 0
		}
		if rs != 0 && l.Syms[rs] != nil && l.Syms[rs].Type == sym.SABIALIAS {
			rsrelocs := l.Relocs(rs)
			rs = rsrelocs.At2(0).Sym()
		}
		if strict && rs != 0 && l.Syms[rs] == nil && rt != objabi.R_USETYPE {
			panic("nil reloc target in convertRelocations")
		}
		dst.R[j] = sym.Reloc{
			Off:  r.Off(),
			Siz:  sz,
			Type: rt,
			Add:  r.Add(),
			Sym:  l.Syms[rs],
		}
	}
}

// UndefinedRelocTargets iterates through the global symbol index
// space, looking for symbols with relocations targeting undefined
// references. The linker's loadlib method uses this to determine if
// there are unresolved references to functions in system libraries
// (for example, libgcc.a), presumably due to CGO code. Return
// value is a list of loader.Sym's corresponding to the undefined
// cross-refs. The "limit" param controls the maximum number of
// results returned; if "limit" is -1, then all undefs are returned.
func (l *Loader) UndefinedRelocTargets(limit int) []Sym {
	result := []Sym{}
	for si := Sym(1); si < Sym(len(l.objSyms)); si++ {
		relocs := l.Relocs(si)
		for ri := 0; ri < relocs.Count(); ri++ {
			r := relocs.At2(ri)
			rs := r.Sym()
			if rs != 0 && l.SymType(rs) == sym.SXREF && l.RawSymName(rs) != ".got" {
				result = append(result, rs)
				if limit != -1 && len(result) >= limit {
					break
				}
			}
		}
	}
	return result
}

// AssignTextSymbolOrder populates the Textp2 slices within each
// library and compilation unit, insuring that packages are laid down
// in dependency order (internal first, then everything else). Return value
// is a slice of all text syms.
func (l *Loader) AssignTextSymbolOrder(libs []*sym.Library, intlibs []bool, extsyms []Sym) []Sym {

	// Library Textp2 lists should be empty at this point.
	for _, lib := range libs {
		if len(lib.Textp2) != 0 {
			panic("expected empty Textp2 slice for library")
		}
		if len(lib.DupTextSyms2) != 0 {
			panic("expected empty DupTextSyms2 slice for library")
		}
	}

	// Used to record which dupok symbol we've assigned to a unit.
	// Can't use the onlist attribute here because it will need to
	// clear for the later assignment of the sym.Symbol to a unit.
	// NB: we can convert to using onList once we no longer have to
	// call the regular addToTextp.
	assignedToUnit := MakeBitmap(l.NSym() + 1)

	// Start off textp2 with reachable external syms.
	textp2 := []Sym{}
	for _, sym := range extsyms {
		if !l.attrReachable.Has(sym) {
			continue
		}
		textp2 = append(textp2, sym)
	}

	// Walk through all text symbols from Go object files and append
	// them to their corresponding library's textp2 list.
	for _, o := range l.objs[1:] {
		r := o.r
		lib := r.unit.Lib
		for i, n := 0, r.NSym()+r.NNonpkgdef(); i < n; i++ {
			gi := l.toGlobal(r, i)
			if !l.attrReachable.Has(gi) {
				continue
			}
			osym := r.Sym2(i)
			st := sym.AbiSymKindToSymKind[objabi.SymKind(osym.Type())]
			if st != sym.STEXT {
				continue
			}
			dupok := osym.Dupok()
			if r2, i2 := l.toLocal(gi); r2 != r || i2 != i {
				// A dupok text symbol is resolved to another package.
				// We still need to record its presence in the current
				// package, as the trampoline pass expects packages
				// are laid out in dependency order.
				lib.DupTextSyms2 = append(lib.DupTextSyms2, sym.LoaderSym(gi))
				continue // symbol in different object
			}
			if dupok {
				lib.DupTextSyms2 = append(lib.DupTextSyms2, sym.LoaderSym(gi))
			}

			lib.Textp2 = append(lib.Textp2, sym.LoaderSym(gi))
		}
	}

	// Now redo the assignment of text symbols to libs/units.
	for _, doInternal := range [2]bool{true, false} {
		for idx, lib := range libs {
			if intlibs[idx] != doInternal {
				continue
			}
			libtextp2 := []sym.LoaderSym{}
			lists := [2][]sym.LoaderSym{lib.Textp2, lib.DupTextSyms2}
			for _, list := range lists {
				for _, s := range list {
					sym := Sym(s)
					if l.attrReachable.Has(sym) && !assignedToUnit.Has(sym) {
						libtextp2 = append(libtextp2, s)
						textp2 = append(textp2, sym)
						unit := l.SymUnit(sym)
						if unit != nil {
							unit.Textp2 = append(unit.Textp2, s)
							assignedToUnit.Set(sym)
						}
					}
				}
			}
			lib.Textp2 = libtextp2
		}
	}

	return textp2
}

// For debugging.
func (l *Loader) Dump() {
	fmt.Println("objs")
	for _, obj := range l.objs {
		if obj.r != nil {
			fmt.Println(obj.i, obj.r.unit.Lib)
		}
	}
	fmt.Println("extStart:", l.extStart)
	fmt.Println("Nsyms:", len(l.objSyms))
	fmt.Println("syms")
	for i := Sym(1); i < Sym(len(l.objSyms)); i++ {
		pi := interface{}("")
		if l.IsExternal(i) {
			pi = fmt.Sprintf("<ext %d>", l.extIndex(i))
		}
		var s *sym.Symbol
		if int(i) < len(l.Syms) {
			s = l.Syms[i]
		}
		if s != nil {
			fmt.Println(i, s, s.Type, pi)
		} else {
			fmt.Println(i, l.SymName(i), "<not loaded>", pi)
		}
	}
	fmt.Println("symsByName")
	for name, i := range l.symsByName[0] {
		fmt.Println(i, name, 0)
	}
	for name, i := range l.symsByName[1] {
		fmt.Println(i, name, 1)
	}
	fmt.Println("payloads:")
	for i := range l.payloads {
		pp := l.payloads[i]
		fmt.Println(i, pp.name, pp.ver, pp.kind)
	}
}
