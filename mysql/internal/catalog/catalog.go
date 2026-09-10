// Package catalog is what MySQL knows about its built-in functions, read out of the server
// source by parsegen: the native function registry (name, argument count, evaluating
// class), and for every Item class its base, the family that fixes its result kind, and
// the declarative statements of its resolve_type(). The analyzer interprets the facts; this
// package only holds and indexes them.
package catalog

import "strings"

// Function is one entry of the native function registry.
type Function struct {
	Name     string // upper case
	Class    string // the Item class that evaluates it
	Min, Max int    // argument count; Max -1 for unbounded
	Odd      bool
	Even     bool
	Internal bool   // only from system views
	Factory  string // the special instantiator, when any
}

// Item is what the server declares about an Item class.
type Item struct {
	Base            string
	Family          string
	InheritsResolve string
	Facts           []string
}

// Lookup returns the registry entry for name (any case), or nil.
func Lookup(name string) *Function {
	u := strings.ToUpper(name)
	for i := range Functions {
		if Functions[i].Name == u {
			return &Functions[i]
		}
	}
	return nil
}

// Accepts reports whether the function takes n arguments.
func (f *Function) Accepts(n int) bool {
	if n < f.Min || (f.Max >= 0 && n > f.Max) {
		return false
	}
	if f.Odd && n%2 == 0 || f.Even && n%2 == 1 {
		return false
	}
	return true
}

// FamilyOf walks the class hierarchy to the family of class, or "".
func FamilyOf(class string) string {
	seen := map[string]bool{}
	for cur := class; cur != "" && !seen[cur]; {
		seen[cur] = true
		it, ok := Items[cur]
		if !ok {
			return ""
		}
		if it.Family != "" {
			return it.Family
		}
		cur = it.Base
	}
	return ""
}

// Merge is the type two values aggregate to, by MySQL's field_types_merge_rules: the
// type of a CASE / IF / COALESCE / GREATEST over values of types a and b (enum_field_types
// names without the MYSQL_TYPE_ prefix: "LONGLONG", "NEWDECIMAL", "VARCHAR"). "" when
// either is not a known type.
func Merge(a, b string) string {
	i, j := fieldIndex(a), fieldIndex(b)
	if i < 0 || j < 0 {
		return ""
	}
	return MergeRules[i][j]
}

// ResultKind is the Item_result of a type: "INT_RESULT", "DECIMAL_RESULT", "REAL_RESULT"
// or "STRING_RESULT"; "" when unknown.
func ResultKind(fieldType string) string {
	i := fieldIndex(fieldType)
	if i < 0 {
		return ""
	}
	return ResultKinds[i]
}

func fieldIndex(t string) int {
	for i, ft := range FieldTypes {
		if ft == t {
			return i
		}
	}
	return -1
}
