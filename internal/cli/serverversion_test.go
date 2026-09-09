package cli

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/pgparse"
)

func TestVersionMismatch(t *testing.T) {
	for _, c := range []struct {
		declared pgparse.Version
		server   string
		want     string // "" = no warning; else a substring
	}{
		{pgparse.PG17, "17.5", ""},
		{pgparse.PG17, "17.5 (Debian 17.5-1.pgdg120+1)", ""},
		{pgparse.PG18, "18beta1", ""},
		{0, "17.9", ""}, // the zero Version is Default
		{pgparse.PG18, "17.5", "runs PostgreSQL 17.5 but schema.sql declares 18"},
		{pgparse.PG17, "18.3", "declare `-- sqlshape: postgres 18`"},
		{pgparse.PG17, "garbage", ""},
	} {
		got := versionMismatch(c.declared, c.server)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%d vs %q: %q", int(c.declared), c.server, got)
		}
	}
}
