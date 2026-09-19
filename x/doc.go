// Package x holds nothing itself: its subdirectories are the parts of the checker that
// every module of the repository shares and that have no dependencies of their own —
// expand (the template's finite expansion), dialect (the schema's dialect declaration and
// the analyzer registry), facts (the statement facts the obligations are judged on). They
// are exported because Go's internal rule stops at a module's major-version suffix
// (check/postgres/v2 cannot import sqlshape/v2/internal), and they are tool-facing: no
// compatibility promise beyond what cmd/sqlshape and the check modules need.
package x
