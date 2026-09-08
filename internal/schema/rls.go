package schema

import (
	"strings"

	"github.com/kr9ly/sqlshape/internal/pgparse"
)

// Row-level security. A table's policies are part of its definition: the loader keeps
// them (with the ENABLE / FORCE ROW LEVEL SECURITY flags), the analyzer type-checks their
// predicates against the table, the diff and the migration carry them, and the checker
// points out the ways they fail to apply (policies on a table whose row security is off,
// SECURITY DEFINER functions the owner's privileges take past them).

// Policy is one CREATE POLICY.
type Policy struct {
	Name string
	// Command is the command the policy applies to: all, select, insert, update, delete.
	Command string
	// Permissive (the default) policies are ORed; restrictive ones ANDed on top.
	Permissive bool
	// Roles the policy applies to; empty (PUBLIC) means every role.
	Roles []string
	// Using is the USING predicate (rows visible / modifiable), WithCheck the WITH CHECK
	// predicate (rows a write may produce). Either may be nil.
	Using, WithCheck Expr
	// Definition is the text of the CREATE POLICY statement.
	Definition string
}

// Policy finds a policy by name.
func (r *Relation) Policy(name string) *Policy {
	for _, p := range r.Policies {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (s *Schema) createPolicy(st *pgparse.CreatePolicyStmt, loc int32) {
	rel := s.findRelation(s.rangeVar(st.Table))
	if rel == nil {
		sch, name := s.rangeVar(st.Table)
		s.problem(loc, "CREATE POLICY %s: relation %s.%s does not exist", st.PolicyName, sch, name)
		return
	}
	if rel.Kind != Table {
		s.problem(loc, "CREATE POLICY %s: %s is not a table", st.PolicyName, rel.FullName())
		return
	}
	if rel.Policy(st.PolicyName) != nil {
		s.problem(loc, "CREATE POLICY %s: policy %q for table %s already exists", st.PolicyName, st.PolicyName, rel.FullName())
		return
	}
	p := &Policy{Name: st.PolicyName, Command: strings.ToLower(st.CmdName), Permissive: st.Permissive, Using: st.Qual, WithCheck: st.WithCheck, Definition: s.stmtText}
	if p.Command == "" {
		p.Command = "all"
	}
	for _, r := range st.Roles {
		if rs := r.GetRoleSpec(); rs != nil {
			if rs.Roletype == pgparse.RoleSpecType_ROLESPEC_PUBLIC {
				p.Roles = nil
				break
			}
			p.Roles = append(p.Roles, rs.Rolename)
		}
	}
	rel.Policies = append(rel.Policies, p)
}

func (s *Schema) alterPolicy(st *pgparse.AlterPolicyStmt, loc int32) {
	rel := s.findRelation(s.rangeVar(st.Table))
	var p *Policy
	if rel != nil {
		p = rel.Policy(st.PolicyName)
	}
	if p == nil {
		s.problem(loc, "ALTER POLICY %s: no such policy", st.PolicyName)
		return
	}
	if st.Qual != nil {
		p.Using = st.Qual
	}
	if st.WithCheck != nil {
		p.WithCheck = st.WithCheck
	}
	if len(st.Roles) > 0 {
		p.Roles = nil
		for _, r := range st.Roles {
			if rs := r.GetRoleSpec(); rs != nil && rs.Roletype != pgparse.RoleSpecType_ROLESPEC_PUBLIC {
				p.Roles = append(p.Roles, rs.Rolename)
			}
		}
	}
	// the definition is now the CREATE plus this ALTER: keep both for the migration
	p.Definition += ";\n" + s.stmtText
}

// dropPolicy removes a policy named by a DROP POLICY object list [table..., name].
func (s *Schema) dropPolicy(items []string, missingOk bool, loc int32) {
	if len(items) < 2 {
		return
	}
	name := items[len(items)-1]
	sch, tname := qualified(items[:len(items)-1])
	rel := s.findRelation(sch, tname)
	if rel == nil || rel.Policy(name) == nil {
		if !missingOk {
			s.problem(loc, "DROP POLICY %s: no such policy on %s", name, tname)
		}
		return
	}
	var kept []*Policy
	for _, p := range rel.Policies {
		if p.Name != name {
			kept = append(kept, p)
		}
	}
	rel.Policies = kept
}

// renamePolicy handles ALTER POLICY ... ON table RENAME TO.
func (s *Schema) renamePolicy(st *pgparse.RenameStmt, loc int32) {
	rel := s.findRelation(s.rangeVar(st.Relation))
	if rel == nil || rel.Policy(st.Subname) == nil {
		s.problem(loc, "ALTER POLICY %s RENAME: no such policy", st.Subname)
		return
	}
	rel.Policy(st.Subname).Name = st.Newname
}
