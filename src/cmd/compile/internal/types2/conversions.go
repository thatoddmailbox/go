// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of conversions.

package types2

import (
	"go/constant"
	"unicode"
)

// Conversion type-checks the conversion T(x).
// The result is in x.
func (check *Checker) conversion(x *operand, T Type) {
	constArg := x.mode == constant_

	var ok bool
	switch {
	case constArg && isConstType(T):
		// constant conversion
		switch t := asBasic(T); {
		case representableConst(x.val, check, t, &x.val):
			ok = true
		case isInteger(x.typ) && isString(t):
			codepoint := unicode.ReplacementChar
			if i, ok := constant.Uint64Val(x.val); ok && i <= unicode.MaxRune {
				codepoint = rune(i)
			}
			x.val = constant.MakeString(string(codepoint))
			ok = true
		}
	case x.convertibleTo(check, T):
		// non-constant conversion
		x.mode = value
		ok = true
	}

	if !ok {
		if x.mode != invalid {
			check.errorf(x, "cannot convert %s to %s", x, T)
			x.mode = invalid
		}
		return
	}

	// The conversion argument types are final. For untyped values the
	// conversion provides the type, per the spec: "A constant may be
	// given a type explicitly by a constant declaration or conversion,...".
	if isUntyped(x.typ) {
		final := T
		// - For conversions to interfaces, except for untyped nil arguments,
		//   use the argument's default type.
		// - For conversions of untyped constants to non-constant types, also
		//   use the default type (e.g., []byte("foo") should report string
		//   not []byte as type for the constant "foo").
		// - For integer to string conversions, keep the argument type.
		//   (See also the TODO below.)
		if x.typ == Typ[UntypedNil] {
			// ok
		} else if IsInterface(T) || constArg && !isConstType(T) {
			final = Default(x.typ)
		} else if isInteger(x.typ) && isString(T) {
			final = x.typ
		}
		check.updateExprType(x.expr, final, true)
	}

	x.typ = T
}

// TODO(gri) convertibleTo checks if T(x) is valid. It assumes that the type
// of x is fully known, but that's not the case for say string(1<<s + 1.0):
// Here, the type of 1<<s + 1.0 will be UntypedFloat which will lead to the
// (correct!) refusal of the conversion. But the reported error is essentially
// "cannot convert untyped float value to string", yet the correct error (per
// the spec) is that we cannot shift a floating-point value: 1 in 1<<s should
// be converted to UntypedFloat because of the addition of 1.0. Fixing this
// is tricky because we'd have to run updateExprType on the argument first.
// (Issue #21982.)

// convertibleTo reports whether T(x) is valid.
// The check parameter may be nil if convertibleTo is invoked through an
// exported API call, i.e., when all methods have been type-checked.
func (x *operand) convertibleTo(check *Checker, T Type) bool {
	// "x is assignable to T"
	if ok, _ := x.assignableTo(check, T, nil); ok {
		return true
	}

	// TODO(gri) consider passing under(x.typ), under(T) into convertibleToImpl (optimization)
	Vp, _ := under(x.typ).(*TypeParam)
	Tp, _ := under(T).(*TypeParam)

	// generic cases
	// (generic operands cannot be constants, so we can ignore x.val)
	switch {
	case Vp != nil && Tp != nil:
		x := *x // don't modify outer x
		return Vp.underIs(func(V Type) bool {
			x.typ = V
			return Tp.underIs(func(T Type) bool {
				return x.convertibleToImpl(check, T)
			})
		})
	case Vp != nil:
		x := *x // don't modify outer x
		return Vp.underIs(func(V Type) bool {
			x.typ = V
			return x.convertibleToImpl(check, T)
		})
	case Tp != nil:
		return Tp.underIs(func(T Type) bool {
			return x.convertibleToImpl(check, T)
		})
	}

	// non-generic case
	return x.convertibleToImpl(check, T)
}

// convertibleToImpl should only be called by convertibleTo
func (x *operand) convertibleToImpl(check *Checker, T Type) bool {
	// "x's type and T have identical underlying types if tags are ignored"
	V := x.typ
	Vu := under(V)
	Tu := under(T)
	if IdenticalIgnoreTags(Vu, Tu) {
		return true
	}

	// "x's type and T are unnamed pointer types and their pointer base types
	// have identical underlying types if tags are ignored"
	if V, ok := V.(*Pointer); ok {
		if T, ok := T.(*Pointer); ok {
			if IdenticalIgnoreTags(under(V.base), under(T.base)) {
				return true
			}
		}
	}

	// "x's type and T are both integer or floating point types"
	if isIntegerOrFloat(V) && isIntegerOrFloat(T) {
		return true
	}

	// "x's type and T are both complex types"
	if isComplex(V) && isComplex(T) {
		return true
	}

	// "x is an integer or a slice of bytes or runes and T is a string type"
	if (isInteger(V) || isBytesOrRunes(Vu)) && isString(T) {
		return true
	}

	// "x is a string and T is a slice of bytes or runes"
	if isString(V) && isBytesOrRunes(Tu) {
		return true
	}

	// package unsafe:
	// "any pointer or value of underlying type uintptr can be converted into a unsafe.Pointer"
	if (isPointer(Vu) || isUintptr(Vu)) && isUnsafePointer(T) {
		return true
	}
	// "and vice versa"
	if isUnsafePointer(V) && (isPointer(Tu) || isUintptr(Tu)) {
		return true
	}

	// "x is a slice, T is a pointer-to-array type,
	// and the slice and array types have identical element types."
	if s := asSlice(V); s != nil {
		if p := asPointer(T); p != nil {
			if a := asArray(p.Elem()); a != nil {
				if Identical(s.Elem(), a.Elem()) {
					if check == nil || check.allowVersion(check.pkg, 1, 17) {
						return true
					}
					// check != nil
					if check.conf.CompilerErrorMessages {
						check.error(x, "conversion of slices to array pointers only supported as of -lang=go1.17")
					} else {
						check.error(x, "conversion of slices to array pointers requires go1.17 or later")
					}
					x.mode = invalid // avoid follow-up error
				}
			}
		}
	}

	return false
}

func isUintptr(typ Type) bool {
	t := asBasic(typ)
	return t != nil && t.kind == Uintptr
}

func isUnsafePointer(typ Type) bool {
	// TODO(gri): Is this asBasic(typ) instead of typ.(*Basic) correct?
	//            (The former calls under(), while the latter doesn't.)
	//            The spec does not say so, but gc claims it is. See also
	//            issue 6326.
	t := asBasic(typ)
	return t != nil && t.kind == UnsafePointer
}

func isPointer(typ Type) bool {
	return asPointer(typ) != nil
}

func isBytesOrRunes(typ Type) bool {
	if s := asSlice(typ); s != nil {
		t := asBasic(s.elem)
		return t != nil && (t.kind == Byte || t.kind == Rune)
	}
	return false
}
