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
