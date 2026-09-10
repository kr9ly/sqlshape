package migrate

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/diff"
	"github.com/kr9ly/sqlshape/check/postgres/schema"
)

// seeds brings the content of every seeded table of the target to its declared rows: the
// declared rows are merged into the table by the seed's key (a row that differs in a
// declared column is updated, a missing row inserted), and rows the declaration does not
// list are deleted unless the seed is additive. Columns the declaration leaves out are
// untouched. A table on its own gets one MERGE that does all three; seeded tables tied by
// foreign keys are merged parents first and then pruned children first, so a new child row
// finds its parent and a parent row goes after its children. A delete some other table's
// foreign key still blocks fails at apply time, which is the right place to learn a value
// is in use.
func (p *planner) seeds() {
	_, toOrder := relations(p.to)
	var seeded []*schema.Relation
	for _, r := range toOrder {
		if r.Kind == schema.Table && r.Seed != nil && len(r.Seed.Rows) > 0 {
			seeded = append(seeded, r)
		}
	}
	var changed []*schema.Relation
	for _, r := range fkOrder(seeded) { // children first
		if f := p.fromOf(r); f != nil && !p.recreated[r.FullName()] && !seedDiffers(f, r) {
			continue
		}
		changed = append(changed, r)
	}
	tied := fkTied(seeded)
	for i := len(changed) - 1; i >= 0; i-- { // parents first
		r := changed[i]
		p.emit("%s", mergeText(p.to, r, !tied[r.FullName()]))
	}
	for _, r := range changed { // children first
		if tied[r.FullName()] && !r.Seed.Additive {
			p.emit("%s", pruneText(p.to, r))
		}
	}
}

// fkTied marks the tables that reference, or are referenced by, another of rels.
func fkTied(rels []*schema.Relation) map[string]bool {
	names := map[string]bool{}
	for _, r := range rels {
		names[r.FullName()] = true
	}
	tied := map[string]bool{}
	for _, r := range rels {
		for _, c := range r.Constraints {
			if ref := strings.TrimPrefix(c.RefTable, "public."); c.Kind == schema.ForeignKey && names[ref] && ref != r.FullName() {
				tied[r.FullName()], tied[ref] = true, true
			}
		}
	}
	return tied
}

// seedDiffers: the rows of from, seen through to's declaration, differ from to's.
func seedDiffers(from, to *schema.Relation) bool { return len(diff.RowChanges(from, to)) > 0 }

// mergeText renders the MERGE that makes r's content its declared rows; prune adds the
// delete arm for rows the declaration does not list (never for an additive seed).
func mergeText(s *schema.Schema, r *schema.Relation, prune bool) string {
	sd := r.Seed
	key := map[string]bool{}
	for _, k := range sd.Key {
		key[k] = true
	}
	var rows []string
	for _, row := range sd.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = schema.Deparse(v) + "::" + typeText(s, r.Column(sd.Columns[i]))
		}
		rows = append(rows, "  ("+strings.Join(vals, ", ")+")")
	}
	var on, set, tcols, scols, tvals, svals []string
	for _, c := range sd.Columns {
		tcols = append(tcols, q(c))
		scols = append(scols, "s."+q(c))
		if key[c] {
			on = append(on, "t."+q(c)+" = s."+q(c))
		} else {
			set = append(set, q(c)+" = s."+q(c))
			tvals = append(tvals, "t."+q(c))
			svals = append(svals, "s."+q(c))
		}
	}
	var b strings.Builder
	b.WriteString("MERGE INTO " + qrel(r) + " AS t\n")
	b.WriteString("USING (VALUES\n" + strings.Join(rows, ",\n") + "\n) AS s(" + strings.Join(tcols, ", ") + ")\n")
	b.WriteString("ON " + strings.Join(on, " AND "))
	if len(set) > 0 {
		b.WriteString("\nWHEN MATCHED AND (" + strings.Join(tvals, ", ") + ") IS DISTINCT FROM (" + strings.Join(svals, ", ") + ") THEN UPDATE SET " + strings.Join(set, ", "))
	}
	b.WriteString("\nWHEN NOT MATCHED THEN INSERT (" + strings.Join(tcols, ", ") + ") VALUES (" + strings.Join(scols, ", ") + ")")
	if prune && !sd.Additive {
		b.WriteString("\nWHEN NOT MATCHED BY SOURCE THEN DELETE")
	}
	return b.String()
}

// pruneText renders the DELETE of the rows r's declaration does not list.
func pruneText(s *schema.Schema, r *schema.Relation) string {
	sd := r.Seed
	var keys, rows []string
	for _, k := range sd.Key {
		keys = append(keys, q(k))
	}
	for _, row := range sd.Rows {
		var vals []string
		for _, k := range sd.Key {
			i := seedIndex(sd, k)
			vals = append(vals, schema.Deparse(row[i])+"::"+typeText(s, r.Column(k)))
		}
		rows = append(rows, "("+strings.Join(vals, ", ")+")")
	}
	return "DELETE FROM " + qrel(r) + " WHERE (" + strings.Join(keys, ", ") + ") NOT IN (VALUES\n  " + strings.Join(rows, ",\n  ") + "\n)"
}

func seedIndex(sd *schema.Seed, col string) int {
	for i, c := range sd.Columns {
		if c == col {
			return i
		}
	}
	return -1
}
