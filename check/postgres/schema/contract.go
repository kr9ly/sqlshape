package schema

import (
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// Contract is the schema as the contracts (x/obligation) see it: relations by the names the
// facts use, their columns, directives and foreign keys, and what a view column passes
// through.
func (s *Schema) Contract() obligation.Schema { return contractSchema{s} }

// ByFullName resolves the schema-qualified-unless-public name the facts use as Leaf.Table.
func (s *Schema) ByFullName(name string) *Relation {
	sch, n, ok := strings.Cut(name, ".")
	if !ok {
		return s.Relation("", name)
	}
	if rel := s.Relation(sch, n); rel != nil {
		return rel
	}
	return s.Relation("", name)
}

type contractSchema struct{ s *Schema }

func (c contractSchema) Relations() []obligation.Relation {
	out := make([]obligation.Relation, 0, len(c.s.Relations))
	for _, r := range c.s.Relations {
		out = append(out, contractRel{c.s, r})
	}
	return out
}

func (c contractSchema) Relation(name string) obligation.Relation {
	if r := c.s.ByFullName(name); r != nil {
		return contractRel{c.s, r}
	}
	return nil
}

type contractRel struct {
	s *Schema
	r *Relation
}

func (c contractRel) Name() string     { return c.r.Name }
func (c contractRel) FullName() string { return c.r.FullName() }

func (c contractRel) Kind() facts.RelKind {
	switch c.r.Kind {
	case View:
		return facts.View
	case MatView:
		return facts.MatView
	}
	return facts.Table
}

// HasColumn: a table's column, or a view's frozen output column.
func (c contractRel) HasColumn(col string) bool {
	if c.r.Column(col) != nil {
		return true
	}
	for _, vc := range c.r.Frozen {
		if vc.Name == col {
			return true
		}
	}
	return false
}

func (c contractRel) Directives() []string { return c.r.Directives }

// ForeignKeys lists the REFERENCES constraints; a key with no column list references the
// parent's primary key, spelled out here.
func (c contractRel) ForeignKeys() []obligation.ForeignKey {
	var out []obligation.ForeignKey
	for _, con := range c.r.Constraints {
		if con.Kind != ForeignKey {
			continue
		}
		fk := obligation.ForeignKey{Columns: con.Columns, RefTable: con.RefTable, RefColumns: con.RefColumns}
		if len(fk.RefColumns) == 0 {
			if parent := c.s.ByFullName(con.RefTable); parent != nil {
				for _, pc := range parent.Constraints {
					if pc.Kind == PrimaryKey {
						fk.RefColumns = pc.Columns
						break
					}
				}
			}
		}
		out = append(out, fk)
	}
	return out
}

// ViewSource is the base column a view's output column passes through unchanged.
func (c contractRel) ViewSource(col string) (string, string, bool) {
	if c.r.Kind != View && c.r.Kind != MatView {
		return "", "", false
	}
	for i := range c.r.Frozen {
		vc := &c.r.Frozen[i]
		if vc.Name != col {
			continue
		}
		switch {
		case vc.SrcRel != nil:
			return vc.SrcRel.FullName(), vc.SrcColumn, true
		case vc.SrcTable != "":
			return vc.SrcTable, vc.SrcColumn, true
		}
		return "", "", false
	}
	return "", "", false
}

func (c contractRel) ForceRowSecurity() bool { return c.r.ForceRowSecurity }
