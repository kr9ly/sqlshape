package analyze

import (
	"os"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestAnalyzeFunction checks SQL function bodies against their signatures (what PG does
// at CREATE with check_function_bodies). Bad bodies cannot live in the oracle schema, so
// they are appended here.
func TestAnalyzeFunction(t *testing.T) {
	base, _ := os.ReadFile("testdata/schema.sql")
	cases := []struct {
		def  string
		want string // substring of the error, "" for OK
		rels string // relations referenced, comma-separated
	}{
		{def: "CREATE FUNCTION f1(p_user bigint) RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM orders WHERE user_id = p_user $$", rels: "orders"},
		{def: "CREATE FUNCTION f2(p_user bigint) RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM orders WHERE user_id = $1 $$", rels: "orders"},
		// a column of the same name wins over the parameter: uid is orders.uid (uuid) here
		{def: "CREATE FUNCTION f3(uid bigint) RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM orders WHERE user_id = uid $$", want: "operator does not exist: bigint = uuid"},
		// assignment casts are allowed (bigint → text), other mismatches are not
		{def: "CREATE FUNCTION f4(p_user bigint) RETURNS text LANGUAGE sql AS $$ SELECT count(*) FROM orders WHERE user_id = p_user $$", rels: "orders"},
		{def: "CREATE FUNCTION f4b(p_user bigint) RETURNS date LANGUAGE sql AS $$ SELECT count(*) FROM orders WHERE user_id = p_user $$", want: "returns bigint instead of date"},
		{def: "CREATE FUNCTION f5(p_user bigint) RETURNS bigint LANGUAGE sql AS $$ SELECT id, total FROM orders WHERE user_id = p_user $$", want: "returns 2 columns, 1 expected"},
		{def: "CREATE FUNCTION f6(p_user bigint) RETURNS bigint LANGUAGE sql AS $$ UPDATE orders SET status = 'paid' WHERE user_id = p_user $$", want: "must be SELECT or INSERT/UPDATE/DELETE RETURNING"},
		{def: "CREATE FUNCTION f7(p_user bigint) RETURNS void LANGUAGE sql AS $$ UPDATE orders SET status = 'paid' WHERE user_id = p_user $$", rels: "orders"},
		{def: "CREATE FUNCTION f8(p_min numeric) RETURNS TABLE (user_id bigint, total numeric) LANGUAGE sql AS $$ SELECT user_id, sum(total) FROM orders WHERE total > p_min GROUP BY user_id $$", rels: "orders"},
		{def: "CREATE FUNCTION f9(p_min numeric) RETURNS TABLE (user_id bigint, total numeric) LANGUAGE sql AS $$ SELECT user_id FROM orders WHERE total > p_min $$", want: "returns 1 columns, 2 expected"},
		{def: "CREATE FUNCTION f10(p_id bigint) RETURNS orders LANGUAGE sql AS $$ SELECT * FROM orders WHERE id = p_id $$", rels: "orders"},
		{def: "CREATE FUNCTION f11(p_id bigint) RETURNS SETOF bigint LANGUAGE sql AS $$ SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id WHERE u.id = p_id $$", rels: "orders,users"},
		{def: "CREATE FUNCTION f12(p_id bigint) RETURNS bigint LANGUAGE sql AS $$ SELECT nope FROM orders $$", want: `column "nope" does not exist`},
		{def: "CREATE FUNCTION f13(p_id bigint) RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT count(*) FROM orders WHERE user_id = p_id; END", rels: "orders"},
		{def: "CREATE FUNCTION f14(p_id bigint) RETURNS bigint LANGUAGE sql BEGIN ATOMIC RETURN p_id + 1; END"},
		{def: "CREATE FUNCTION f15(p_id bigint) RETURNS bigint LANGUAGE sql BEGIN ATOMIC RETURN 'x'; END", want: "returns text instead of bigint"},
		{def: "CREATE FUNCTION f16(p_id bigint) RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$"},
	}
	for _, c := range cases {
		s, err := schema.Load(string(base) + "\n" + c.def + ";")
		if err != nil {
			t.Fatalf("%s: %v", c.def, err)
		}
		fn := s.Functions[len(s.Functions)-1]
		r, err := AnalyzeFunction(s, fn)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", fn.Name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want error containing %q, got %v", fn.Name, c.want, err)
		}
		if err == nil && c.rels != "" {
			var names []string
			for _, ref := range r.Relations {
				names = append(names, ref.Name)
			}
			if got := strings.Join(names, ","); got != c.rels {
				t.Errorf("%s: relations %q, want %q", fn.Name, got, c.rels)
			}
		}
	}
}
