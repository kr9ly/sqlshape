// Package consumers indexes which Go statements depend on which relation columns: the
// side of a schema change a diff tool cannot see. The vet analyzer fills one Index per
// package (its analysis result); a migration reads the merged index to list what a DROP
// or a type change reaches, and to confirm nobody reads a column before it goes.
package consumers

import (
	"fmt"
	"go/token"
	"sort"
	"strings"
)

// Site is one place in Go source that depends on a column: the position of the reference
// inside the SQL template, and the declaration holding the statement.
type Site struct {
	Pos token.Position
	// Owner is the package path and the declaration the statement belongs to
	// ("app/orders.listOrders", "app/orders.(*Repo).Find"); the package path alone when
	// the statement sits outside any declaration.
	Owner string
}

func (s Site) String() string {
	if s.Owner == "" {
		return s.Pos.String()
	}
	return s.Pos.String() + " (" + s.Owner + ")"
}

// Index maps relations and their columns to the sites depending on them.
type Index struct {
	// columns: "table.column" → sites; relations: "table" → sites (a statement that
	// references the relation at all). Tables are schema-qualified unless public.
	columns   map[string][]Site
	relations map[string][]Site
}

// New returns an empty Index.
func New() *Index {
	return &Index{columns: map[string][]Site{}, relations: map[string][]Site{}}
}

// AddColumn records that site depends on table.column (and on table).
func (x *Index) AddColumn(table, column string, site Site) {
	x.columns[table+"."+column] = addSite(x.columns[table+"."+column], site)
	x.AddRelation(table, site)
}

// AddRelation records that site references table.
func (x *Index) AddRelation(table string, site Site) {
	x.relations[table] = addSite(x.relations[table], site)
}

func addSite(sites []Site, s Site) []Site {
	for _, o := range sites {
		if o == s {
			return sites
		}
	}
	return append(sites, s)
}

// Merge adds every site of o.
func (x *Index) Merge(o *Index) {
	for k, sites := range o.columns {
		for _, s := range sites {
			x.columns[k] = addSite(x.columns[k], s)
		}
	}
	for k, sites := range o.relations {
		for _, s := range sites {
			x.relations[k] = addSite(x.relations[k], s)
		}
	}
}

// Column lists the sites depending on table.column, in source order.
func (x *Index) Column(table, column string) []Site {
	return sorted(x.columns[table+"."+column])
}

// Relation lists the sites referencing table, in source order.
func (x *Index) Relation(table string) []Site {
	return sorted(x.relations[table])
}

// Columns lists the "table.column" keys with at least one site, sorted.
func (x *Index) Columns() []string {
	keys := make([]string, 0, len(x.columns))
	for k := range x.columns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Relations lists the tables with at least one site, sorted.
func (x *Index) Relations() []string {
	keys := make([]string, 0, len(x.relations))
	for k := range x.relations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Len is the number of indexed columns.
func (x *Index) Len() int { return len(x.columns) }

func sorted(sites []Site) []Site {
	out := append([]Site(nil), sites...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Pos, out[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return out
}

// String renders the index as "table.column" lines with their sites indented.
func (x *Index) String() string {
	var b strings.Builder
	for _, k := range x.Columns() {
		fmt.Fprintln(&b, k)
		for _, s := range sorted(x.columns[k]) {
			fmt.Fprintf(&b, "    %s\n", s)
		}
	}
	return b.String()
}
