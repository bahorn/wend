// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Indexed package import.
// See cmd/compile/internal/gc/iexport.go for the export data format.

package gcimporter

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"io"
	"math/big"
	"sort"
)

type intReader struct {
	*bufio.Reader
	path string
}

func (r *intReader) int64() int64 {
	i, err := binary.ReadVarint(r.Reader)
	if err != nil {
		errorf("import %q: read varint error: %v", r.path, err)
	}
	return i
}

func (r *intReader) uint64() uint64 {
	i, err := binary.ReadUvarint(r.Reader)
	if err != nil {
		errorf("import %q: read varint error: %v", r.path, err)
	}
	return i
}

// Keep this in sync with constants in iexport.go.
const (
	iexportVersionGo1_11 = 0
	iexportVersionPosCol = 1
	// TODO: before release, change this back to 2.
	iexportVersionGenerics = iexportVersionPosCol

	iexportVersionCurrent = iexportVersionGenerics
)

type ident struct {
	pkg  string
	name string
}

const predeclReserved = 32

type itag uint64

const (
	// Types
	definedType itag = iota
	pointerType
	sliceType
	arrayType
	chanType
	mapType
	signatureType
	structType
	interfaceType
	typeParamType
	instType
	unionType
)

// iImportData imports a package from the serialized package data
// and returns the number of bytes consumed and a reference to the package.
// If the export data version is not recognized or the format is otherwise
// compromised, an error is returned.
func iImportData(fset *token.FileSet, imports map[string]*types.Package, dataReader *bufio.Reader, path string) (pkg *types.Package, err error) {
	const currentVersion = iexportVersionCurrent
	version := int64(-1)
	defer func() {
		if e := recover(); e != nil {
			if version > currentVersion {
				err = fmt.Errorf("cannot import %q (%v), export data is newer version - update tool", path, e)
			} else {
				err = fmt.Errorf("cannot import %q (%v), possibly version skew - reinstall package", path, e)
			}
		}
	}()

	r := &intReader{dataReader, path}

	version = int64(r.uint64())
	switch version {
	case /* iexportVersionGenerics, */ iexportVersionPosCol, iexportVersionGo1_11:
	default:
		if version > iexportVersionGenerics {
			errorf("unstable iexport format version %d, just rebuild compiler and std library", version)
		} else {
			errorf("unknown iexport format version %d", version)
		}
	}

	sLen := int64(r.uint64())
	dLen := int64(r.uint64())

	data := make([]byte, sLen+dLen)
	if _, err := io.ReadFull(r, data); err != nil {
		errorf("cannot read %d bytes of stringData and declData: %s", len(data), err)
	}
	stringData := data[:sLen]
	declData := data[sLen:]

	p := iimporter{
		exportVersion: version,
		ipath:         path,
		version:       int(version),

		stringData:  stringData,
		stringCache: make(map[uint64]string),
		pkgCache:    make(map[uint64]*types.Package),

		declData: declData,
		pkgIndex: make(map[*types.Package]map[string]uint64),
		typCache: make(map[uint64]types.Type),
		// Separate map for typeparams, keyed by their package and unique
		// name (name with subscript).
		tparamIndex: make(map[ident]types.Type),

		fake: fakeFileSet{
			fset:  fset,
			files: make(map[string]*token.File),
		},
	}

	for i, pt := range predeclared {
		p.typCache[uint64(i)] = pt
	}

	pkgList := make([]*types.Package, r.uint64())
	for i := range pkgList {
		pkgPathOff := r.uint64()
		pkgPath := p.stringAt(pkgPathOff)
		pkgName := p.stringAt(r.uint64())
		_ = r.uint64() // package height; unused by go/types

		if pkgPath == "" {
			pkgPath = path
		}
		pkg := imports[pkgPath]
		if pkg == nil {
			pkg = types.NewPackage(pkgPath, pkgName)
			imports[pkgPath] = pkg
		} else if pkg.Name() != pkgName {
			errorf("conflicting names %s and %s for package %q", pkg.Name(), pkgName, path)
		}

		p.pkgCache[pkgPathOff] = pkg

		nameIndex := make(map[string]uint64)
		for nSyms := r.uint64(); nSyms > 0; nSyms-- {
			name := p.stringAt(r.uint64())
			nameIndex[name] = r.uint64()
		}

		p.pkgIndex[pkg] = nameIndex
		pkgList[i] = pkg
	}

	localpkg := pkgList[0]

	names := make([]string, 0, len(p.pkgIndex[localpkg]))
	for name := range p.pkgIndex[localpkg] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p.doDecl(localpkg, name)
	}

	for _, typ := range p.interfaceList {
		typ.Complete()
	}

	// record all referenced packages as imports
	list := append(([]*types.Package)(nil), pkgList[1:]...)
	sort.Sort(byPath(list))
	localpkg.SetImports(list)

	// package was imported completely and without errors
	localpkg.MarkComplete()
	return localpkg, nil
}

type iimporter struct {
	exportVersion int64
	ipath         string
	version       int

	stringData  []byte
	stringCache map[uint64]string
	pkgCache    map[uint64]*types.Package

	declData    []byte
	pkgIndex    map[*types.Package]map[string]uint64
	typCache    map[uint64]types.Type
	tparamIndex map[ident]types.Type

	fake          fakeFileSet
	interfaceList []*types.Interface
}

func (p *iimporter) doDecl(pkg *types.Package, name string) {
	// See if we've already imported this declaration.
	if obj := pkg.Scope().Lookup(name); obj != nil {
		return
	}

	off, ok := p.pkgIndex[pkg][name]
	if !ok {
		errorf("%v.%v not in index", pkg, name)
	}

	r := &importReader{p: p, currPkg: pkg}
	r.declReader.Reset(p.declData[off:])

	r.obj(name)
}

func (p *iimporter) stringAt(off uint64) string {
	if s, ok := p.stringCache[off]; ok {
		return s
	}

	slen, n := binary.Uvarint(p.stringData[off:])
	if n <= 0 {
		errorf("varint failed")
	}
	spos := off + uint64(n)
	s := string(p.stringData[spos : spos+slen])
	p.stringCache[off] = s
	return s
}

func (p *iimporter) pkgAt(off uint64) *types.Package {
	if pkg, ok := p.pkgCache[off]; ok {
		return pkg
	}
	path := p.stringAt(off)
	errorf("missing package %q in %q", path, p.ipath)
	return nil
}

func (p *iimporter) typAt(off uint64, base *types.Named) types.Type {
	if t, ok := p.typCache[off]; ok && (base == nil || !isInterface(t)) {
		return t
	}

	if off < predeclReserved {
		errorf("predeclared type missing from cache: %v", off)
	}

	r := &importReader{p: p}
	r.declReader.Reset(p.declData[off-predeclReserved:])
	t := r.doType(base)

	if base == nil || !isInterface(t) {
		p.typCache[off] = t
	}
	return t
}

type importReader struct {
	p          *iimporter
	declReader bytes.Reader
	currPkg    *types.Package
	prevFile   string
	prevLine   int64
	prevColumn int64
}

func (r *importReader) obj(name string) {
	tag := r.byte()
	pos := r.pos()

	switch tag {
	case 'A':
		typ := r.typ()

		r.declare(types.NewTypeName(pos, r.currPkg, name, typ))

	case 'C':
		typ, val := r.value()

		r.declare(types.NewConst(pos, r.currPkg, name, typ, val))

	case 'F', 'G':
		var tparams []*types.TypeParam
		if tag == 'G' {
			tparams = r.tparamList()
		}
		sig := r.signature(nil)
		sig.SetTParams(tparams)
		r.declare(types.NewFunc(pos, r.currPkg, name, sig))

	case 'T', 'U':
		var tparams []*types.TypeParam
		if tag == 'U' {
			tparams = r.tparamList()
		}
		// Types can be recursive. We need to setup a stub
		// declaration before recursing.
		obj := types.NewTypeName(pos, r.currPkg, name, nil)
		named := types.NewNamed(obj, nil, nil)
		named.SetTParams(tparams)
		r.declare(obj)

		underlying := r.p.typAt(r.uint64(), named).Underlying()
		named.SetUnderlying(underlying)

		if !isInterface(underlying) {
			for n := r.uint64(); n > 0; n-- {
				mpos := r.pos()
				mname := r.ident()
				recv := r.param()
				msig := r.signature(recv)

				// If the receiver has any targs, set those as the
				// rparams of the method (since those are the
				// typeparams being used in the method sig/body).
				targs := baseType(msig.Recv().Type()).TArgs()
				if targs.Len() > 0 {
					rparams := make([]*types.TypeParam, targs.Len())
					for i := range rparams {
						rparams[i], _ = targs.At(i).(*types.TypeParam)
					}
					msig.SetRParams(rparams)
				}

				named.AddMethod(types.NewFunc(mpos, r.currPkg, mname, msig))
			}
		}

	case 'P':
		// We need to "declare" a typeparam in order to have a name that
		// can be referenced recursively (if needed) in the type param's
		// bound.
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected type param type")
		}
		name0, sub := parseSubscript(name)
		tn := types.NewTypeName(pos, r.currPkg, name0, nil)
		t := types.NewTypeParam(tn, nil)
		if sub == 0 {
			errorf("missing subscript")
		}

		// TODO(rfindley): can we use a different, stable ID?
		// t.SetId(sub)

		// To handle recursive references to the typeparam within its
		// bound, save the partial type in tparamIndex before reading the bounds.
		id := ident{r.currPkg.Name(), name}
		r.p.tparamIndex[id] = t

		t.SetConstraint(r.typ())

	case 'V':
		typ := r.typ()

		r.declare(types.NewVar(pos, r.currPkg, name, typ))

	default:
		errorf("unexpected tag: %v", tag)
	}
}

func (r *importReader) declare(obj types.Object) {
	obj.Pkg().Scope().Insert(obj)
}

func (r *importReader) value() (typ types.Type, val constant.Value) {
	typ = r.typ()

	switch b := typ.Underlying().(*types.Basic); b.Info() & types.IsConstType {
	case types.IsBoolean:
		val = constant.MakeBool(r.bool())

	case types.IsString:
		val = constant.MakeString(r.string())

	case types.IsInteger:
		var x big.Int
		r.mpint(&x, b)
		val = constant.Make(&x)

	case types.IsFloat:
		val = r.mpfloat(b)

	case types.IsComplex:
		re := r.mpfloat(b)
		im := r.mpfloat(b)
		val = constant.BinaryOp(re, token.ADD, constant.MakeImag(im))

	default:
		errorf("unexpected type %v", typ) // panics
		panic("unreachable")
	}

	return
}

func intSize(b *types.Basic) (signed bool, maxBytes uint) {
	if (b.Info() & types.IsUntyped) != 0 {
		return true, 64
	}

	switch b.Kind() {
	case types.Float32, types.Complex64:
		return true, 3
	case types.Float64, types.Complex128:
		return true, 7
	}

	signed = (b.Info() & types.IsUnsigned) == 0
	switch b.Kind() {
	case types.Int8, types.Uint8:
		maxBytes = 1
	case types.Int16, types.Uint16:
		maxBytes = 2
	case types.Int32, types.Uint32:
		maxBytes = 4
	default:
		maxBytes = 8
	}

	return
}

func (r *importReader) mpint(x *big.Int, typ *types.Basic) {
	signed, maxBytes := intSize(typ)

	maxSmall := 256 - maxBytes
	if signed {
		maxSmall = 256 - 2*maxBytes
	}
	if maxBytes == 1 {
		maxSmall = 256
	}

	n, _ := r.declReader.ReadByte()
	if uint(n) < maxSmall {
		v := int64(n)
		if signed {
			v >>= 1
			if n&1 != 0 {
				v = ^v
			}
		}
		x.SetInt64(v)
		return
	}

	v := -n
	if signed {
		v = -(n &^ 1) >> 1
	}
	if v < 1 || uint(v) > maxBytes {
		errorf("weird decoding: %v, %v => %v", n, signed, v)
	}
	b := make([]byte, v)
	io.ReadFull(&r.declReader, b)
	x.SetBytes(b)
	if signed && n&1 != 0 {
		x.Neg(x)
	}
}

func (r *importReader) mpfloat(typ *types.Basic) constant.Value {
	var mant big.Int
	r.mpint(&mant, typ)
	var f big.Float
	f.SetInt(&mant)
	if f.Sign() != 0 {
		f.SetMantExp(&f, int(r.int64()))
	}
	return constant.Make(&f)
}

func (r *importReader) ident() string {
	return r.string()
}

func (r *importReader) qualifiedIdent() (*types.Package, string) {
	name := r.string()
	pkg := r.pkg()
	return pkg, name
}

func (r *importReader) pos() token.Pos {
	if r.p.version >= 1 {
		r.posv1()
	} else {
		r.posv0()
	}

	if r.prevFile == "" && r.prevLine == 0 && r.prevColumn == 0 {
		return token.NoPos
	}
	return r.p.fake.pos(r.prevFile, int(r.prevLine), int(r.prevColumn))
}

func (r *importReader) posv0() {
	delta := r.int64()
	if delta != deltaNewFile {
		r.prevLine += delta
	} else if l := r.int64(); l == -1 {
		r.prevLine += deltaNewFile
	} else {
		r.prevFile = r.string()
		r.prevLine = l
	}
}

func (r *importReader) posv1() {
	delta := r.int64()
	r.prevColumn += delta >> 1
	if delta&1 != 0 {
		delta = r.int64()
		r.prevLine += delta >> 1
		if delta&1 != 0 {
			r.prevFile = r.string()
		}
	}
}

func (r *importReader) typ() types.Type {
	return r.p.typAt(r.uint64(), nil)
}

func isInterface(t types.Type) bool {
	_, ok := t.(*types.Interface)
	return ok
}

func (r *importReader) pkg() *types.Package { return r.p.pkgAt(r.uint64()) }
func (r *importReader) string() string      { return r.p.stringAt(r.uint64()) }

func (r *importReader) doType(base *types.Named) types.Type {
	switch k := r.kind(); k {
	default:
		errorf("unexpected kind tag in %q: %v", r.p.ipath, k)
		return nil

	case definedType:
		pkg, name := r.qualifiedIdent()
		r.p.doDecl(pkg, name)
		return pkg.Scope().Lookup(name).(*types.TypeName).Type()
	case pointerType:
		return types.NewPointer(r.typ())
	case sliceType:
		return types.NewSlice(r.typ())
	case arrayType:
		n := r.uint64()
		return types.NewArray(r.typ(), int64(n))
	case chanType:
		dir := chanDir(int(r.uint64()))
		return types.NewChan(dir, r.typ())
	case mapType:
		return types.NewMap(r.typ(), r.typ())
	case signatureType:
		r.currPkg = r.pkg()
		return r.signature(nil)

	case structType:
		r.currPkg = r.pkg()

		fields := make([]*types.Var, r.uint64())
		tags := make([]string, len(fields))
		for i := range fields {
			fpos := r.pos()
			fname := r.ident()
			ftyp := r.typ()
			emb := r.bool()
			tag := r.string()

			fields[i] = types.NewField(fpos, r.currPkg, fname, ftyp, emb)
			tags[i] = tag
		}
		return types.NewStruct(fields, tags)

	case interfaceType:
		r.currPkg = r.pkg()

		embeddeds := make([]types.Type, r.uint64())
		for i := range embeddeds {
			_ = r.pos()
			embeddeds[i] = r.typ()
		}

		methods := make([]*types.Func, r.uint64())
		for i := range methods {
			mpos := r.pos()
			mname := r.ident()

			// TODO(mdempsky): Matches bimport.go, but I
			// don't agree with this.
			var recv *types.Var
			if base != nil {
				recv = types.NewVar(token.NoPos, r.currPkg, "", base)
			}

			msig := r.signature(recv)
			methods[i] = types.NewFunc(mpos, r.currPkg, mname, msig)
		}

		typ := types.NewInterfaceType(methods, embeddeds)
		r.p.interfaceList = append(r.p.interfaceList, typ)
		return typ

	case typeParamType:
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected type param type")
		}
		pkg, name := r.qualifiedIdent()
		id := ident{pkg.Name(), name}
		if t, ok := r.p.tparamIndex[id]; ok {
			// We're already in the process of importing this typeparam.
			return t
		}
		// Otherwise, import the definition of the typeparam now.
		r.p.doDecl(pkg, name)
		return r.p.tparamIndex[id]

	case instType:
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected instantiation type")
		}
		// pos does not matter for instances: they are positioned on the original
		// type.
		_ = r.pos()
		len := r.uint64()
		targs := make([]types.Type, len)
		for i := range targs {
			targs[i] = r.typ()
		}
		baseType := r.typ()
		// The imported instantiated type doesn't include any methods, so
		// we must always use the methods of the base (orig) type.
		// TODO provide a non-nil *Checker
		t, _ := types.Instantiate(nil, baseType, targs, false)
		return t

	case unionType:
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected instantiation type")
		}
		terms := make([]*types.Term, r.uint64())
		for i := range terms {
			terms[i] = types.NewTerm(r.bool(), r.typ())
		}
		return types.NewUnion(terms)
	}
}

func (r *importReader) kind() itag {
	return itag(r.uint64())
}

func (r *importReader) signature(recv *types.Var) *types.Signature {
	params := r.paramList()
	results := r.paramList()
	variadic := params.Len() > 0 && r.bool()
	return types.NewSignature(recv, params, results, variadic)
}

func (r *importReader) tparamList() []*types.TypeParam {
	n := r.uint64()
	if n == 0 {
		return nil
	}
	xs := make([]*types.TypeParam, n)
	for i := range xs {
		xs[i], _ = r.typ().(*types.TypeParam)
	}
	return xs
}

func (r *importReader) paramList() *types.Tuple {
	xs := make([]*types.Var, r.uint64())
	for i := range xs {
		xs[i] = r.param()
	}
	return types.NewTuple(xs...)
}

func (r *importReader) param() *types.Var {
	pos := r.pos()
	name := r.ident()
	typ := r.typ()
	return types.NewParam(pos, r.currPkg, name, typ)
}

func (r *importReader) bool() bool {
	return r.uint64() != 0
}

func (r *importReader) int64() int64 {
	n, err := binary.ReadVarint(&r.declReader)
	if err != nil {
		errorf("readVarint: %v", err)
	}
	return n
}

func (r *importReader) uint64() uint64 {
	n, err := binary.ReadUvarint(&r.declReader)
	if err != nil {
		errorf("readUvarint: %v", err)
	}
	return n
}

func (r *importReader) byte() byte {
	x, err := r.declReader.ReadByte()
	if err != nil {
		errorf("declReader.ReadByte: %v", err)
	}
	return x
}

func baseType(typ types.Type) *types.Named {
	// pointer receivers are never types.Named types
	if p, _ := typ.(*types.Pointer); p != nil {
		typ = p.Elem()
	}
	// receiver base types are always (possibly generic) types.Named types
	n, _ := typ.(*types.Named)
	return n
}

func parseSubscript(name string) (string, uint64) {
	// Extract the subscript value from the type param name. We export
	// and import the subscript value, so that all type params have
	// unique names.
	sub := uint64(0)
	startsub := -1
	for i, r := range name {
		if '₀' <= r && r < '₀'+10 {
			if startsub == -1 {
				startsub = i
			}
			sub = sub*10 + uint64(r-'₀')
		}
	}
	if startsub >= 0 {
		name = name[:startsub]
	}
	return name, sub
}
