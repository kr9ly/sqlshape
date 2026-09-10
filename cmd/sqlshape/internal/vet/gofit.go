package vet

import (
	"go/types"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// The Go side of a dialect type: dialect.Type spells, in GoSpelling, the Go types that
// carry a value; this file matches a go/types type against those spellings. Nothing here
// knows a database: what fits is the dialect's table, how a spelling reads is this grammar.

// fitType decides whether Go type t carries a value of dt, as a result column (param
// false) or as a parameter (param true), under the dialect's traits.
func fitType(dt dialect.Type, t types.Type, param bool, tr dialect.Traits) fit {
	inner, nullable := unwrapNullable(t)
	if inner == nil {
		return fit{ok: true, nullable: true}
	}
	if n, ok := inner.(*types.Named); ok && n.Obj().Pkg() != nil && n.TypeArgs() == nil {
		for _, w := range tr.NullWrappers {
			if strings.HasSuffix(n.Obj().Pkg().Path(), w) {
				return fit{ok: true, nullable: true}
			}
		}
	}
	switch inner.Underlying().(type) {
	case *types.Slice, *types.Map:
		nullable = true
	}
	if dt.Unknown() {
		return fit{ok: true, unknown: true, nullable: nullable}
	}
	// a Go type that decodes / encodes itself receives any column / encodes as any parameter
	if !param && implementsScanner(inner) {
		return fit{ok: true, nullable: true}
	}
	if param && implementsValuer(inner) {
		return fit{ok: true, nullable: nullable}
	}
	f := fitValue(dt, inner, param, tr)
	f.nullable = nullable
	return f
}

// fitValue matches the value itself (nullability already stripped) against the dialect's
// list for the direction.
func fitValue(dt dialect.Type, t types.Type, param bool, tr dialect.Traits) fit {
	if dt.Unknown() {
		return fit{ok: true, unknown: true}
	}
	if param && tr.TextParams {
		if k, ok := basicKind(t); ok && k == types.String {
			return fit{ok: true}
		}
	}
	list := dt.Result
	if param {
		list = dt.Param
	}
	for _, g := range list {
		if ok, lossy := matchSpelling(g.Go, dt, t, param, tr); ok {
			if lossy != "" {
				return fit{ok: true, lossy: lossy}
			}
			return fit{ok: true, lossy: g.Lossy}
		}
	}
	return fit{}
}

// matchSpelling reads one GoSpelling against t. For a compound spelling ([]$elem, a
// generic over $elem) the element's fit is Elem's, and its lossiness is carried up.
func matchSpelling(spell string, dt dialect.Type, t types.Type, param bool, tr dialect.Traits) (ok bool, lossy string) {
	switch {
	case spell == "struct":
		_, isStruct := t.Underlying().(*types.Struct)
		return isStruct, ""
	case spell == "json":
		if k, isBasic := basicKind(t); isBasic && k != types.String {
			return false, ""
		}
		return true, ""
	case spell == "[]byte":
		return isByteSlice(t), ""
	case strings.HasPrefix(spell, "map["):
		return matchMap(spell, t), ""
	case strings.HasSuffix(spell, "$elem") && (strings.HasPrefix(spell, "[]") || strings.HasPrefix(spell, "[")):
		// []$elem or [N]$elem
		var et types.Type
		switch u := t.Underlying().(type) {
		case *types.Slice:
			if !strings.HasPrefix(spell, "[]") {
				return false, ""
			}
			et = u.Elem()
		case *types.Array:
			n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(spell, "["), "]$elem"), 10, 64)
			if strings.HasPrefix(spell, "[]") {
				et = u.Elem()
			} else if err != nil || n != u.Len() {
				return false, ""
			} else {
				et = u.Elem()
			}
		default:
			return false, ""
		}
		if dt.Elem == nil {
			return false, ""
		}
		ef := fitType(*dt.Elem, et, param, tr)
		if ef.unknown {
			return true, ""
		}
		return ef.ok, ef.lossy
	case strings.HasSuffix(spell, "[$elem]"):
		// pkg.Name[$elem]
		n, isNamed := t.(*types.Named)
		if !isNamed || !sameNamed(n, strings.TrimSuffix(spell, "[$elem]")) {
			return false, ""
		}
		args := n.TypeArgs()
		if args == nil || args.Len() != 1 || dt.Elem == nil {
			return false, ""
		}
		ef := fitType(*dt.Elem, args.At(0), param, tr)
		return ef.ok, ef.lossy
	case strings.HasPrefix(spell, "[") && strings.HasSuffix(spell, "]byte"):
		arr, isArr := t.Underlying().(*types.Array)
		if !isArr {
			return false, ""
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(spell, "["), "]byte"), 10, 64)
		if err != nil || n != arr.Len() {
			return false, ""
		}
		k, isBasic := basicKind(arr.Elem())
		return isBasic && (k == types.Byte || k == types.Uint8), ""
	}
	if strings.ContainsAny(spell, "./") {
		n, isNamed := t.(*types.Named)
		return isNamed && sameNamed(n, spell), ""
	}
	// a basic type, by its own or its underlying spelling (a named string carries text)
	b, isBasic := t.Underlying().(*types.Basic)
	if !isBasic {
		return false, ""
	}
	return b.Name() == spell || (spell == "byte" && b.Kind() == types.Uint8), ""
}

// sameNamed: the named type's package path and name spell "path.Name" (the path matched
// by suffix, so a vendored or forked module still fits).
func sameNamed(n *types.Named, spell string) bool {
	if n.Obj().Pkg() == nil {
		return false
	}
	i := strings.LastIndexByte(spell, '.')
	if i < 0 {
		return false
	}
	path, name := spell[:i], spell[i+1:]
	return n.Obj().Name() == name && (n.Obj().Pkg().Path() == path || strings.HasSuffix(n.Obj().Pkg().Path(), "/"+path) || strings.HasSuffix(n.Obj().Pkg().Path(), path))
}

// matchMap reads map[K]V with K, V basic or *basic.
func matchMap(spell string, t types.Type) bool {
	m, ok := t.Underlying().(*types.Map)
	if !ok {
		return false
	}
	rest := strings.TrimPrefix(spell, "map[")
	k, v, ok := strings.Cut(rest, "]")
	if !ok {
		return false
	}
	kb, isBasic := basicKind(m.Key())
	if !isBasic || types.Typ[kb].Name() != k {
		return false
	}
	vt := m.Elem()
	if strings.HasPrefix(v, "*") {
		p, isPtr := vt.(*types.Pointer)
		if !isPtr {
			return false
		}
		vt, v = p.Elem(), v[1:]
	} else if _, isPtr := vt.(*types.Pointer); isPtr {
		return false
	}
	vb, isBasic := basicKind(vt)
	return isBasic && types.Typ[vb].Name() == v
}
