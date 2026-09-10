package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysql/internal/oracle"
	"github.com/kr9ly/sqlshape/mysql/internal/schema"
)

// TestOracle checks the analyzer's answers against a real mysqld: every statement of
// analyzeCases and errorCases is described by the server, and the result columns' names,
// types and nullability (for the columns the analyzer types) and the error numbers must
// agree. Skipped without a mysqld on PATH (nix-shell -p mysql84).
func TestOracle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, testSchema)
	if errors.Is(err, oracle.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	s := load(t)
	untyped := 0
	for _, c := range analyzeCases {
		d, err := o.Describe(ctx, c.sql)
		if err != nil {
			t.Errorf("%s: the server rejects it: %v", c.sql, err)
			continue
		}
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: the analyzer rejects what the server accepts: %v", c.sql, err)
			continue
		}
		if len(r.Columns) != len(d.Columns) {
			t.Errorf("%s: %d columns, the server says %d", c.sql, len(r.Columns), len(d.Columns))
			continue
		}
		for i, col := range r.Columns {
			oc := d.Columns[i]
			if col.Name != oc.Name {
				t.Errorf("%s: column %d named %q, the server says %q", c.sql, i+1, col.Name, oc.Name)
			}
			if !col.Known {
				untyped++
				continue
			}
			if got, want := typeKey(col.Type), oracleKey(oc); got != want {
				t.Errorf("%s: column %q is %s, the server says %s", c.sql, col.Name, got, want)
			}
			if col.Nullable != oc.Nullable {
				t.Errorf("%s: column %q nullable=%v, the server says %v", c.sql, col.Name, col.Nullable, oc.Nullable)
			}
		}
	}
	for _, c := range errorCases {
		_, err := o.Describe(ctx, c.sql)
		var oe *oracle.Error
		if !errors.As(err, &oe) {
			t.Errorf("%s: the server accepts what the analyzer rejects with %d (%v)", c.sql, c.code, err)
			continue
		}
		if oe.Number != c.code {
			t.Errorf("%s: the analyzer says %d, the server %d (%s)", c.sql, c.code, oe.Number, oe.Message)
		}
	}
	t.Logf("server %s: %d statements agree; %d columns the analyzer leaves untyped", o.Version, len(analyzeCases), untyped)
}

// typeKey reduces a type to what the wire metadata can confirm: the type name and its
// signedness (lengths and the TEXT sizes are not compared).
func typeKey(t schema.Type) string {
	name := t.Name
	switch name {
	case "tinytext", "mediumtext", "longtext":
		name = "text"
	case "tinyblob", "mediumblob", "longblob":
		name = "blob"
	}
	if t.Unsigned {
		return name + " unsigned"
	}
	return name
}

// oracleKey is the driver's DatabaseTypeName in typeKey's spelling.
func oracleKey(c oracle.Column) string {
	name := strings.ToLower(c.Type)
	unsigned := strings.HasPrefix(name, "unsigned ")
	name = strings.TrimPrefix(name, "unsigned ")
	switch name {
	case "tinytext", "mediumtext", "longtext":
		name = "text"
	case "tinyblob", "mediumblob", "longblob":
		name = "blob"
	}
	if unsigned {
		return name + " unsigned"
	}
	return name
}
