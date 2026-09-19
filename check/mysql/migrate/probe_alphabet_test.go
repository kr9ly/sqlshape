package migrate

// alphabetKnownUnreached is diff.Alphabet's entries the gate (seed 1, 200 pairs) does not
// need to hit, each with why -- the two reasons TestMigrateProbe's gate accepts as an
// escape from adding generator vocabulary (see the assertion at the end of
// TestMigrateProbe): the planner writes no DDL for it (a note or a problem, measured in
// migrate.go), or this probe's real server can never produce a pair carrying it in the
// first place.
var alphabetKnownUnreached = map[string]string{}

// directedKnownUnusable is mutations directed coverage cannot ever apply completely alone
// (see TestMigrateProbe's own directed-coverage loop), each with why: every one of these
// edits a partitioning this package's generate() never produces on a fresh schema (only
// another mutation earlier in the same recipe -- "partition table by range" or "partition
// table by hash" -- creates one), so a schema fresh enough for these to have a candidate
// straight from generate() does not exist to draw. The random pairs above still reach every
// one of them in combination (a "partition table by ..." step ahead of it in the same
// recipe), which is what the alphabet coverage this file's TestMigrateProbe gate cares about
// actually depends on -- this map only accepts the standalone case as impossible instead of
// papering over a mutation the probe genuinely cannot exercise at all.
var directedKnownUnusable = map[string]string{
	"add partition":                               "needs a table already partitioned by RANGE with no MAXVALUE tail yet",
	"drop partition":                              "needs a table already partitioned by RANGE with a tail an earlier step of the same recipe added",
	"remove partitioning":                         "needs a table already partitioned (RANGE, HASH or LIST)",
	"reorganize partition boundary":               "needs a table already partitioned by RANGE with no MAXVALUE tail yet",
	"reorganize partition insert before maxvalue": "needs a table already partitioned by RANGE with a MAXVALUE tail",
	"change hash partition count":                 "needs a table already partitioned by HASH",
	"add list partition":                          "needs a table already partitioned by LIST",
	"drop list partition":                         "needs a table already partitioned by LIST with a partition an earlier step of the same recipe added",
	"move list partition value":                   "needs a table already partitioned by LIST",
	"change key partition count":                  "needs a table already partitioned by KEY",
	"toggle key algorithm":                        "needs a table already partitioned by KEY",
	"toggle partitioning linear":                  "needs a table already partitioned by HASH or KEY",
	"change subpartition count":                   "needs a table already partitioned with a SUBPARTITION BY clause",
	"remove subpartitioning":                      "needs a table already partitioned with a SUBPARTITION BY clause",
}
