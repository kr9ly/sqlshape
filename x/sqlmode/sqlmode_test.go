package sqlmode

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
		str  string
	}{
		{"", 0, ""},
		{"ansi_quotes, PIPES_AS_CONCAT", ANSIQuotes | PipesAsConcat, "PIPES_AS_CONCAT,ANSI_QUOTES"},
		{"ANSI", ANSI | RealAsFloat | PipesAsConcat | ANSIQuotes | IgnoreSpace | OnlyFullGroupBy,
			"REAL_AS_FLOAT,PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ONLY_FULL_GROUP_BY,ANSI"},
		{"TRADITIONAL", Traditional | StrictTransTables | StrictAllTables | NoZeroInDate | NoZeroDate | ErrorForDivisionByZero | NoEngineSubstitution,
			"STRICT_TRANS_TABLES,STRICT_ALL_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,TRADITIONAL,NO_ENGINE_SUBSTITUTION"},
		{"ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION", Default,
			"ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"},
		{"TIME_TRUNCATE_FRACTIONAL", TimeTruncateFractional, "TIME_TRUNCATE_FRACTIONAL"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %#x, want %#x", c.in, uint64(got), uint64(c.want))
		}
		if got.String() != c.str {
			t.Errorf("%q: String() = %q, want %q", c.in, got.String(), c.str)
		}
	}
	if _, err := Parse("STRICT"); err == nil {
		t.Error("STRICT: want an error")
	}
	if !Default.Strict() || (Default &^ StrictTransTables).Strict() || !StrictAllTables.Strict() {
		t.Error("Strict")
	}
	// the bit values are the server's (system_variables.h)
	if NoBackslashEscapes != 0x40000*4 || StrictTransTables != 1<<21 || StrictAllTables != 1<<22 || Traditional != 1<<27 || NoEngineSubstitution != 1<<30 {
		t.Errorf("bits: %#x %#x %#x %#x %#x", uint64(NoBackslashEscapes), uint64(StrictTransTables), uint64(StrictAllTables), uint64(Traditional), uint64(NoEngineSubstitution))
	}
}
