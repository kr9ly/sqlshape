package mysqlast

import (
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
)

// Hand-written alternatives for CREATE EVENT's schedule and options. event_tail itself is
// ActDefault (the server's action only fills the LEX in the children), so the generic fold
// gives Node event_tail(if_not_exists, sp_name, ev_schedule, on_completion, status,
// comment, body) once these children stop being *Unsupported: the schedule's own
// alternatives write into a Event_parse_data object parsegen cannot read, and the option
// keywords have no readable payload at all.
func init() {
	// ev_schedule_time: `AT expr` (a one-time event) or `EVERY expr interval [STARTS expr]
	// [ENDS expr]` (a recurring one) -> Node ev_schedule{at, every, interval, starts, ends},
	// the fields the form does not have nil.
	register("ev_schedule_time", "AT_SYM expr", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "ev_schedule", Names: []string{"at", "every", "interval", "starts", "ends"},
			Args: []Value{kids[1], nil, nil, nil, nil}, Start: n.Start, End: n.End}, nil
	})
	register("ev_schedule_time", "EVERY_SYM expr interval ev_starts ev_ends", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "ev_schedule", Names: []string{"at", "every", "interval", "starts", "ends"},
			Args: []Value{nil, kids[1], kids[2], kids[3], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	// ev_starts / ev_ends: the expression alone (the empty alternative folds to nil).
	register("ev_starts", "STARTS_SYM expr", pass(2))
	register("ev_ends", "ENDS_SYM expr", pass(2))
	// opt_ev_status: the status keyword as text; the empty alternative stays the grammar's
	// own "0" (ENABLE, the server's default).
	register("opt_ev_status", "ENABLE_SYM", constant("ENABLE"))
	register("opt_ev_status", "DISABLE_SYM", constant("DISABLE"))
	register("opt_ev_status", "DISABLE_SYM ON_SYM SLAVE", constant("DISABLE ON REPLICA"))
	register("opt_ev_status", "DISABLE_SYM ON_SYM REPLICA_SYM", constant("DISABLE ON REPLICA"))
	// ev_on_completion: PRESERVE / NOT PRESERVE as text; opt_ev_on_completion's empty
	// alternative stays "0" (NOT PRESERVE, the server's default).
	register("ev_on_completion", "ON_SYM COMPLETION_SYM PRESERVE_SYM", constant("PRESERVE"))
	register("ev_on_completion", "ON_SYM COMPLETION_SYM NOT_SYM PRESERVE_SYM", constant("NOT PRESERVE"))
	// opt_ev_comment: the string literal alone.
	register("opt_ev_comment", "COMMENT_SYM TEXT_STRING_sys", pass(2))
}

// ALTER EVENT: alter_event_stmt is ActDefault (Node alter_event_stmt(definer, sp_name,
// schedule_completion, rename_to, status, comment, body)); its own children below carry
// only a "1" in the grammar's shapes, so they are folded here to the data they hold.
func init() {
	// ev_alter_on_schedule_completion -> Node ev_alter_schedule{schedule, completion}, the
	// one not written nil; the empty alternative stays the grammar's "0".
	register("ev_alter_on_schedule_completion", "ON_SYM SCHEDULE_SYM ev_schedule_time", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "ev_alter_schedule", Names: []string{"schedule", "completion"}, Args: []Value{kids[2], nil}, Start: n.Start, End: n.End}, nil
	})
	register("ev_alter_on_schedule_completion", "ev_on_completion", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "ev_alter_schedule", Names: []string{"schedule", "completion"}, Args: []Value{nil, kids[0]}, Start: n.Start, End: n.End}, nil
	})
	register("ev_alter_on_schedule_completion", "ON_SYM SCHEDULE_SYM ev_schedule_time ev_on_completion", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "ev_alter_schedule", Names: []string{"schedule", "completion"}, Args: []Value{kids[2], kids[3]}, Start: n.Start, End: n.End}, nil
	})
	// opt_ev_rename_to: the new sp_name alone; opt_ev_sql_stmt: the new body alone.
	register("opt_ev_rename_to", "RENAME TO_SYM sp_name", pass(3))
	register("opt_ev_sql_stmt", "DO_SYM ev_sql_stmt", pass(2))
}
