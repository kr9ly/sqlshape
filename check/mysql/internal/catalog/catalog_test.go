package catalog

import "testing"

func TestRegistry(t *testing.T) {
	if len(Functions) < 340 {
		t.Fatalf("only %d functions", len(Functions))
	}
	abs := Lookup("abs")
	if abs == nil || abs.Class != "Item_func_abs" || !abs.Accepts(1) || abs.Accepts(2) {
		t.Errorf("ABS: %+v", abs)
	}
	concat := Lookup("CONCAT")
	if concat == nil || concat.Max != -1 || !concat.Accepts(9) || concat.Accepts(0) {
		t.Errorf("CONCAT: %+v", concat)
	}
	round := Lookup("ROUND")
	if round == nil || round.Class != "Item_func_round" || round.Min != 1 || round.Max != 2 || round.Factory != "Round_instantiator" {
		t.Errorf("ROUND: %+v", round)
	}
	if st := Lookup("ST_STARTPOINT"); st == nil || st.Class != "Item_func_spatial_decomp" || st.Min != 1 {
		t.Errorf("ST_STARTPOINT: %+v", st)
	}
	if f := FamilyOf("Item_func_char_length"); f != "Item_int_func" {
		t.Errorf("CHAR_LENGTH family %q", f)
	}
	if f := FamilyOf("Item_func_concat"); f != "Item_str_func" {
		t.Errorf("CONCAT family %q", f)
	}
	if facts := Items["Item_func_round"].Facts; len(facts) == 0 {
		t.Error("ROUND has no resolve_type facts")
	}
	if Lookup("no_such_function") != nil {
		t.Error("unknown function found")
	}
}
