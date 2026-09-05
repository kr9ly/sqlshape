package vet

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Interpretation sharing: a Go named type that meets a DB nominal type — an enum,
// a domain, or the identity of a key column (PK, or FK-derived) — is bound to it by
// use. Bindings are compared for consistency, enum label sets are diffed against the
// type's constants, and conversions / switches over bound types are checked.

// ConstSetFact records the string constants declared for a named string type.
type ConstSetFact struct{ Values []string }

func (*ConstSetFact) AFact()           {}
func (f *ConstSetFact) String() string { return "consts " + strings.Join(f.Values, ",") }

// BindingFact records what a Go type was bound to in the package that declares it or uses it.
type BindingFact struct {
	Kind   byte // 'e' enum, 'd' domain, 'k' key identity
	Key    string
	Labels []string // enum labels, for downstream conversion / switch checks
}

func (*BindingFact) AFact()           {}
func (f *BindingFact) String() string { return fmt.Sprintf("bound %c %s", f.Kind, f.Key) }

func init() {
	Analyzer.FactTypes = append(Analyzer.FactTypes, (*ConstSetFact)(nil), (*BindingFact)(nil))
}

type binding struct {
	kind   byte
	key    string
	labels []string
	pos    token.Pos
}

// nominal describes the DB-side nominal type a value carries, if any.
type nominal struct {
	kind   byte
	key    string
	labels []string
}

func (n nominal) String() string {
	switch n.kind {
	case 'e':
		return "enum " + n.key
	case 'd':
		return "domain " + n.key
	}
	return "key " + n.key
}

// nominalOf classifies a PG type + provenance.
func (c *checker) nominalOf(pg schema.TypeRef, src *analyze.Source) (nominal, bool) {
	t := c.s.Types.ByOID(pg.OID)
	if t == nil {
		return nominal{}, false
	}
	if t.Elem != 0 && strings.HasPrefix(t.Name, "_") {
		t = c.s.Types.ByOID(t.Elem)
		if t == nil {
			return nominal{}, false
		}
	}
	switch t.Kind {
	case 'e':
		return nominal{kind: 'e', key: t.Name, labels: c.s.Types.Enums[t.OID]}, true
	case 'd':
		return nominal{kind: 'd', key: t.Name}, true
	}
	if src != nil {
		if id, ok := c.identity(src.Table, src.Column, 0); ok {
			return nominal{kind: 'k', key: id}, true
		}
	}
	return nominal{}, false
}

// identity follows single-column PK / FK structure to the root key column.
func (c *checker) identity(table, column string, depth int) (string, bool) {
	if depth > 10 {
		return "", false
	}
	rel := c.relByFullName(table)
	if rel == nil {
		return "", false
	}
	for _, con := range rel.Constraints {
		if con.Kind == schema.ForeignKey && len(con.Columns) == 1 && con.Columns[0] == column {
			refCol := ""
			if len(con.RefColumns) == 1 {
				refCol = con.RefColumns[0]
			} else if ref := c.relByFullName(con.RefTable); ref != nil {
				for _, rc := range ref.Constraints {
					if rc.Kind == schema.PrimaryKey && len(rc.Columns) == 1 {
						refCol = rc.Columns[0]
					}
				}
			}
			if refCol != "" {
				if id, ok := c.identity(con.RefTable, refCol, depth+1); ok {
					return id, true
				}
			}
		}
	}
	for _, con := range rel.Constraints {
		if con.Kind == schema.PrimaryKey && len(con.Columns) == 1 && con.Columns[0] == column {
			return table + "." + column, true
		}
	}
	return "", false
}

func (c *checker) relByFullName(name string) *schema.Relation {
	for _, r := range c.s.Relations {
		if r.FullName() == name {
			return r
		}
	}
	return nil
}

// namedOf strips pointers / slices / nullable wrappers and returns the Go named type, if any.
func namedOf(t types.Type) *types.Named {
	for {
		switch u := t.(type) {
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		case *types.Named:
			if _, isBasic := u.Underlying().(*types.Basic); isBasic {
				return u
			}
			return nil
		default:
			return nil
		}
	}
}

// meet records that Go type gt carries a value of PG type pg (from src). Returns the
// established binding's key mismatch, if any, as a diagnostic message.
func (c *checker) meet(gt types.Type, pg schema.TypeRef, src *analyze.Source, pos token.Pos, what string) {
	n, ok := c.nominalOf(pg, src)
	named := namedOf(gt)
	if !ok {
		return
	}
	if named == nil {
		if c.strict {
			c.pass.Reportf(pos, "sqlshape: %s carries %s as a plain %s; declare a named type to have it checked", what, n, gt)
		}
		return
	}
	obj := named.Obj()
	b, seen := c.bindings[obj]
	if !seen {
		// a binding exported by another package wins
		var imported BindingFact
		if c.pass.ImportObjectFact(obj, &imported) {
			b = &binding{kind: imported.Kind, key: imported.Key, labels: imported.Labels}
		} else {
			b = &binding{kind: n.kind, key: n.key, labels: n.labels, pos: pos}
		}
		c.bindings[obj] = b
	}
	if b.kind != n.kind || b.key != n.key {
		c.pass.Reportf(pos, "sqlshape: %s is %s, which stands for %s elsewhere, but here meets %s", what, named, nominal{kind: b.kind, key: b.key}, n)
	}
}

// exportConstSets publishes the string constants of each named string type declared here.
func (c *checker) exportConstSets() map[*types.TypeName][]string {
	local := map[*types.TypeName][]string{}
	for _, obj := range c.pass.TypesInfo.Defs {
		cn, ok := obj.(*types.Const)
		if !ok || cn.Val().Kind() != constant.String {
			continue
		}
		named, ok := cn.Type().(*types.Named)
		if !ok || named.Obj().Pkg() != c.pass.Pkg {
			continue
		}
		local[named.Obj()] = append(local[named.Obj()], constant.StringVal(cn.Val()))
	}
	for tn, vals := range local {
		sort.Strings(vals)
		c.pass.ExportObjectFact(tn, &ConstSetFact{Values: vals})
	}
	return local
}

// finishBindings runs the whole-package checks once every call site has been visited.
func (c *checker) finishBindings() {
	local := c.exportConstSets()
	// enum label sets vs constants
	for tn, b := range c.bindings {
		if tn.Pkg() == c.pass.Pkg {
			c.pass.ExportObjectFact(tn, &BindingFact{Kind: b.kind, Key: b.key, Labels: b.labels})
		}
		if b.kind != 'e' {
			continue
		}
		consts, have := local[tn]
		if !have {
			var f ConstSetFact
			if c.pass.ImportObjectFact(tn, &f) {
				consts, have = f.Values, true
			}
		}
		if !have {
			continue // untyped usage (e.g. string literals only); nothing to diff
		}
		at := b.pos
		if at == token.NoPos {
			at = tn.Pos()
		}
		cs := map[string]bool{}
		for _, v := range consts {
			cs[v] = true
		}
		ls := map[string]bool{}
		for _, l := range b.labels {
			ls[l] = true
		}
		for _, l := range b.labels {
			if !cs[l] {
				c.pass.Reportf(at, "sqlshape: enum %s has label %q but %s has no constant for it", b.key, l, tn.Name())
			}
		}
		for _, v := range consts {
			if !ls[v] {
				c.pass.Reportf(at, "sqlshape: %s has constant %q which is not a label of enum %s", tn.Name(), v, b.key)
			}
		}
	}
	c.checkConversionsAndSwitches()
}

// enumLabels returns the labels a Go type is bound to (locally or via fact), if it is an enum type.
func (c *checker) enumLabels(t types.Type) (*types.Named, []string, string, bool) {
	named, ok := t.(*types.Named)
	if !ok {
		return nil, nil, "", false
	}
	if b, ok := c.bindings[named.Obj()]; ok && b.kind == 'e' {
		return named, b.labels, b.key, true
	}
	var f BindingFact
	if c.pass.ImportObjectFact(named.Obj(), &f) && f.Kind == 'e' {
		return named, f.Labels, f.Key, true
	}
	return nil, nil, "", false
}

// checkConversionsAndSwitches flags T("literal") outside the label set and non-exhaustive switches.
func (c *checker) checkConversionsAndSwitches() {
	for _, f := range c.pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if len(v.Args) != 1 {
					return true
				}
				tv, ok := c.pass.TypesInfo.Types[v.Fun]
				if !ok || !tv.IsType() {
					return true
				}
				named, labels, key, ok := c.enumLabels(tv.Type)
				if !ok {
					return true
				}
				av, ok := c.pass.TypesInfo.Types[v.Args[0]]
				if !ok || av.Value == nil || av.Value.Kind() != constant.String {
					return true
				}
				lit := constant.StringVal(av.Value)
				for _, l := range labels {
					if l == lit {
						return true
					}
				}
				c.pass.Reportf(v.Pos(), "sqlshape: %s(%q) is not a label of enum %s", named.Obj().Name(), lit, key)
			case *ast.SwitchStmt:
				if v.Tag == nil {
					return true
				}
				tv, ok := c.pass.TypesInfo.Types[v.Tag]
				if !ok {
					return true
				}
				named, labels, key, ok := c.enumLabels(tv.Type)
				if !ok {
					return true
				}
				covered := map[string]bool{}
				for _, st := range v.Body.List {
					cc := st.(*ast.CaseClause)
					if cc.List == nil {
						return true // default: exhaustive by construction
					}
					for _, e := range cc.List {
						if ev, ok := c.pass.TypesInfo.Types[e]; ok && ev.Value != nil && ev.Value.Kind() == constant.String {
							covered[constant.StringVal(ev.Value)] = true
						}
					}
				}
				var missing []string
				for _, l := range labels {
					if !covered[l] {
						missing = append(missing, l)
					}
				}
				if len(missing) > 0 {
					c.pass.Reportf(v.Pos(), "sqlshape: switch on %s does not handle enum %s labels: %s", named.Obj().Name(), key, strings.Join(missing, ", "))
				}
			}
			return true
		})
	}
}
