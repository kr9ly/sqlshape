// Package sqlmode is MySQL's sql_mode as 8.4 defines it: the names, their bits, the
// combination modes and the server's default (sql/system_variables.h and the
// sql_mode_names / expand_sql_mode of sql/sys_vars.cc). The MySQL analyzer reads a schema's
// declared mode through it, and so do mysqltest (to start the server with it) and the MySQL
// runtime (to compare a connection's @@sql_mode with it); it lives in the root module so
// that all three read one table.
package sqlmode

import (
	"fmt"
	"strings"
)

// Mode is a set of sql_mode flags, with the server's bit values.
type Mode uint64

const (
	RealAsFloat            Mode = 1
	PipesAsConcat          Mode = 2
	ANSIQuotes             Mode = 4
	IgnoreSpace            Mode = 8
	OnlyFullGroupBy        Mode = 32
	NoUnsignedSubtraction  Mode = 64
	NoDirInCreate          Mode = 128
	ANSI                   Mode = 0x40000
	NoAutoValueOnZero      Mode = ANSI * 2
	NoBackslashEscapes     Mode = NoAutoValueOnZero * 2
	StrictTransTables      Mode = NoBackslashEscapes * 2
	StrictAllTables        Mode = StrictTransTables * 2
	NoZeroInDate           Mode = StrictAllTables * 2
	NoZeroDate             Mode = NoZeroInDate * 2
	AllowInvalidDates      Mode = NoZeroDate * 2
	ErrorForDivisionByZero Mode = AllowInvalidDates * 2
	Traditional            Mode = ErrorForDivisionByZero * 2
	HighNotPrecedence      Mode = 1 << 29
	NoEngineSubstitution   Mode = HighNotPrecedence * 2
	PadCharToFullLength    Mode = 1 << 31
	TimeTruncateFractional Mode = 1 << 32
)

// Default is the server's default sql_mode (8.4).
const Default = NoEngineSubstitution | OnlyFullGroupBy | StrictTransTables | NoZeroInDate | NoZeroDate | ErrorForDivisionByZero

// names is sql_mode_names in bit order; "" for an unused bit.
var names = []string{
	"REAL_AS_FLOAT", "PIPES_AS_CONCAT", "ANSI_QUOTES", "IGNORE_SPACE", "", "ONLY_FULL_GROUP_BY",
	"NO_UNSIGNED_SUBTRACTION", "NO_DIR_IN_CREATE", "", "", "", "", "", "", "", "", "", "",
	"ANSI", "NO_AUTO_VALUE_ON_ZERO", "NO_BACKSLASH_ESCAPES", "STRICT_TRANS_TABLES", "STRICT_ALL_TABLES",
	"NO_ZERO_IN_DATE", "NO_ZERO_DATE", "ALLOW_INVALID_DATES", "ERROR_FOR_DIVISION_BY_ZERO", "TRADITIONAL", "",
	"HIGH_NOT_PRECEDENCE", "NO_ENGINE_SUBSTITUTION", "PAD_CHAR_TO_FULL_LENGTH", "TIME_TRUNCATE_FRACTIONAL",
}

// Parse reads a sql_mode value as SET sql_mode does: names separated by commas, in any
// case, spaces around them ignored, the empty string allowed. The combination modes are
// expanded (Expand), as the server stores them.
func Parse(s string) (Mode, error) {
	var m Mode
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		bit, ok := lookup(f)
		if !ok {
			return 0, fmt.Errorf("sql_mode: %q is not a mode of MySQL 8.4", f)
		}
		m |= bit
	}
	return m.Expand(), nil
}

func lookup(name string) (Mode, bool) {
	for i, n := range names {
		if n != "" && strings.EqualFold(n, name) {
			return 1 << i, true
		}
	}
	return 0, false
}

// Expand adds what the combination modes stand for (expand_sql_mode): ANSI is REAL_AS_FLOAT,
// PIPES_AS_CONCAT, ANSI_QUOTES, IGNORE_SPACE and ONLY_FULL_GROUP_BY; TRADITIONAL is
// STRICT_TRANS_TABLES, STRICT_ALL_TABLES, NO_ZERO_IN_DATE, NO_ZERO_DATE,
// ERROR_FOR_DIVISION_BY_ZERO and NO_ENGINE_SUBSTITUTION. The combination flag stays set, as
// the server keeps it.
func (m Mode) Expand() Mode {
	if m&ANSI != 0 {
		m |= RealAsFloat | PipesAsConcat | ANSIQuotes | IgnoreSpace | OnlyFullGroupBy
	}
	if m&Traditional != 0 {
		m |= StrictTransTables | StrictAllTables | NoZeroInDate | NoZeroDate | ErrorForDivisionByZero | NoEngineSubstitution
	}
	return m
}

// Strict is THD::is_strict_mode: either strict flag.
func (m Mode) Strict() bool { return m&(StrictTransTables|StrictAllTables) != 0 }

// Has reports whether every flag of f is set.
func (m Mode) Has(f Mode) bool { return m&f == f }

// String spells the mode as @@sql_mode does: the set flags' names in bit order, comma-joined.
func (m Mode) String() string {
	var out []string
	for i, n := range names {
		if n != "" && m&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return strings.Join(out, ",")
}
