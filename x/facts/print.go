package facts

import (
	"fmt"
	"strings"
)

// String renders the facts as indented text, one item per line, for tests and for the
// audit output of an entry point. The form is stable: scopes nest by two spaces.
func (f *Facts) String() string {
	if f == nil {
		return "<nil>\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", f.Kind)
	if f.Top != nil {
		f.Top.write(&b, "  ")
	}
	for _, w := range f.Writes {
		fmt.Fprintf(&b, "  write %s %s", w.Kind, w.Table)
		if len(w.Assigned) > 0 {
			parts := make([]string, len(w.Assigned))
			for i, c := range w.Assigned {
				parts[i] = c
				if i < len(w.Values) {
					parts[i] += "=" + w.Values[i].String()
				}
			}
			fmt.Fprintf(&b, " set %s", strings.Join(parts, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *Scope) String() string { var b strings.Builder; s.write(&b, "  "); return b.String() }

func (s *Scope) write(b *strings.Builder, ind string) {
	if s.Returning {
		fmt.Fprintf(b, "%sreturning\n", ind)
	}
	for i, l := range s.Leaves {
		fmt.Fprintf(b, "%sleaf %d %s", ind, i, l.Kind)
		if l.Table != "" {
			fmt.Fprintf(b, " %s", l.Table)
		}
		if l.Alias != "" && l.Alias != l.Table && !strings.HasSuffix(l.Table, "."+l.Alias) {
			fmt.Fprintf(b, " as %s", l.Alias)
		}
		if l.Role == Target {
			b.WriteString(" target")
		}
		if len(l.Waived) > 0 {
			fmt.Fprintf(b, " waived %s", strings.Join(l.Waived, ","))
		}
		fmt.Fprintf(b, " @%d\n", l.Position)
		if l.Body != nil && (l.Kind == View || l.Kind == MatView) {
			l.Body.write(b, ind+"    ") // a derived leaf's body is written among the Children
		}
	}
	for _, p := range s.Preds {
		fmt.Fprintf(b, "%spred %s", ind, p)
		if p.Restricts != nil {
			fmt.Fprintf(b, " restricts %v", p.Restricts)
		}
		if p.Origin != FromStatement {
			fmt.Fprintf(b, " from %s", p.Origin)
		}
		b.WriteString("\n")
		if p.Op == Exists && p.Sub != nil {
			p.Sub.write(b, ind+"    ")
		}
	}
	if len(s.Fixed) > 0 {
		fmt.Fprintf(b, "%sfixed %s\n", ind, refs(s.Fixed))
	}
	for _, e := range s.Edges {
		fmt.Fprintf(b, "%sedge %s -> %s\n", ind, e.From, e.To)
	}
	if len(s.NotNull) > 0 {
		fmt.Fprintf(b, "%snotnull %s\n", ind, refs(s.NotNull))
	}
	for _, c := range s.Children {
		fmt.Fprintf(b, "%sscope\n", ind)
		c.write(b, ind+"  ")
	}
}

func refs(rs []ColRef) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}
	return strings.Join(parts, " ")
}

func (r ColRef) String() string { return fmt.Sprintf("%d.%s", r.Leaf, r.Column) }

func (p Pred) String() string {
	switch p.Op {
	case Eq:
		return p.Col.String() + " = " + p.Term.String()
	case IsNull:
		return p.Col.String() + " IS NULL"
	case IsNotNull:
		return p.Col.String() + " IS NOT NULL"
	case Exists:
		return "exists"
	case In:
		parts := make([]string, len(p.Terms))
		for i, t := range p.Terms {
			parts[i] = t.String()
		}
		return p.Col.String() + " IN (" + strings.Join(parts, ", ") + ")"
	case Contains:
		return p.Col.String() + " @> " + p.Term.String()
	}
	return fmt.Sprintf("opaque %q cols %s", p.Text, refs(p.Cols))
}

func (t Term) String() string {
	switch t.Kind {
	case Param:
		return fmt.Sprintf("$%d", t.Param)
	case Const:
		return "const " + t.Const
	case Column:
		return t.Col.String()
	case Outer:
		return "outer " + t.Col.String()
	case Expr:
		return fmt.Sprintf("expr %q", t.Text)
	}
	return fmt.Sprintf("known %q", t.Text)
}

func (k StmtKind) String() string {
	switch k {
	case Select:
		return "select"
	case Insert:
		return "insert"
	case Update:
		return "update"
	case Delete:
		return "delete"
	case Merge:
		return "merge"
	case Call:
		return "call"
	}
	return "none"
}

func (k RelKind) String() string {
	switch k {
	case Table:
		return "table"
	case View:
		return "view"
	case MatView:
		return "matview"
	case CTE:
		return "cte"
	case Function:
		return "function"
	}
	return "derived"
}

func (o Origin) String() string {
	switch o {
	case FromStatement:
		return "statement"
	case FromView:
		return "view"
	case FromPolicy:
		return "policy"
	case FromForeignKey:
		return "fk"
	}
	return "?"
}
