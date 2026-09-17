package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

const geomSchema = `-- sqlshape: mysql 8.4
CREATE TABLE geo (
  id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  g GEOMETRY,
  p POINT,
  ls LINESTRING,
  pg POLYGON NOT NULL,
  gc GEOMETRYCOLLECTION,
  mp MULTIPOINT
);
`

// geomCases are statements over the spatial types the checker judges (geom.go), each pinned
// against mysqld: want is the error number the server raises, 0 when it runs.
var geomCases = []struct {
	sql  string
	want int
}{
	// WKT: what create_from_wkt accepts
	{"SELECT ST_GeomFromText('POINT(1 1)')", 0},
	{"SELECT ST_GeomFromText('POINT(1e2 -3.5)')", 0},
	{"SELECT ST_GeomFromText('POINT(.5 1)')", 0},
	{"SELECT ST_GeomFromText(' point ( 1 1 ) ')", 0},
	{"SELECT ST_GeomFromText('POINT(1 . 1)')", 3037},
	{"SELECT ST_GeomFromText('POINT(1,1)')", 3037},
	{"SELECT ST_GeomFromText('POINT(1 1) x')", 3037},
	{"SELECT ST_GeomFromText('')", 3037},
	{"SELECT ST_GeomFromText('LINESTRING(0 0)')", 3037},
	{"SELECT ST_GeomFromText('LINESTRING(0 0,1 1)')", 0},
	{"SELECT ST_GeomFromText('LINESTRING(0 0,1 1,)')", 3037},
	{"SELECT ST_GeomFromText('POLYGON((0 0, 1 1))')", 3037},
	{"SELECT ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 1))')", 3037},
	{"SELECT ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))')", 0},
	{"SELECT ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0),(0 0,1 1,1 0,0 0))')", 0},
	{"SELECT ST_GeomFromText('MULTIPOINT()')", 3037},
	{"SELECT ST_GeomFromText('MULTIPOINT(0 0, (1 1))')", 3037},
	{"SELECT ST_GeomFromText('MULTIPOINT((0 0), 1 1)')", 3037},
	{"SELECT ST_GeomFromText('MULTIPOINT((0 0),(1 1))')", 0},
	{"SELECT ST_GeomFromText('MULTIPOINT(0 0, 1 1)')", 0},
	{"SELECT ST_GeomFromText('MULTILINESTRING((9 8))')", 3037},
	{"SELECT ST_GeomFromText('MULTILINESTRING((9 8,1 1))')", 0},
	{"SELECT ST_GeomFromText('MULTIPOLYGON(((0 0,5 5)))')", 3037},
	{"SELECT ST_GeomFromText('MULTIPOLYGON(((0 0,1 1,1 0,0 0)))')", 0},
	{"SELECT ST_GeomFromText('GEOMETRYCOLLECTION()')", 0},
	{"SELECT ST_GeomFromText('GEOMETRYCOLLECTION EMPTY')", 0},
	{"SELECT ST_GeomFromText('GEOMCOLLECTION(POINT(1 1))')", 3037},
	{"SELECT ST_GeomFromText('GeometryCollection(())')", 3037},
	{"SELECT ST_GeomFromText('GeometryCollection(POLYGON((0 0)))')", 3037},
	{"SELECT ST_GeomFromText('GeometryCollection(point(0 0),)')", 3037},
	{"SELECT ST_GeomFromText('GeometryCollection(,point(0 0))')", 3037},
	{"SELECT ST_GeomFromText('GeometryCollection((point(0 0)))')", 3037},
	{"SELECT ST_GeomFromText('GeometryCollection(GeometryCollection(point(0 0)))')", 0},
	{"SELECT ST_GeomFromText('CIRCLE(0 0)')", 3037},
	// the typed readers
	{"SELECT ST_PointFromText('POINT(1 1)')", 0},
	{"SELECT ST_PointFromText('LINESTRING(0 0,1 1)')", 3516},
	{"SELECT ST_LineFromText('LINESTRING(0 0,1 1)')", 0},
	{"SELECT ST_PolyFromText('POLYGON((1 1, 10 1, 10 20, 1 20, 1 1), (5 5))')", 3037},
	{"SELECT ST_PolygonFromText('POLYGON((0 0,1 1,1 0,0 0))')", 0},
	{"SELECT ST_MPointFromText('MULTIPOINT(0 0)')", 0},
	{"SELECT ST_MPointFromText('POINT(0 0)')", 3516},
	{"SELECT ST_GeomCollFromText('MULTIPOINT(0 0)')", 0},
	{"SELECT ST_GeomCollFromText('POINT(0 0)')", 3516},
	// SRID
	{"SELECT ST_GeomFromText('POINT(1 1)', 0)", 0},
	{"SELECT ST_GeomFromText('POINT(1 1)', 4326)", 0},
	{"SELECT ST_GeomFromText('POINT(1 1)', -1)", 1690},
	{"SELECT ST_GeomFromText('POINT(1 1)', 4294967295)", 3548},
	{"SELECT ST_GeomFromText('POINT(1 1)', 4294967296)", 1690},
	{"SELECT ST_PolyFromWKB(0x0101000000000000000000F03F000000000000F03F, -1)", 1690},
	// WKB: what create_from_wkb accepts, either byte order, no trailing bytes
	{"SELECT ST_GeomFromWKB(0x0101000000000000000000F03F000000000000F03F)", 0},
	{"SELECT ST_GeomFromWKB(0x00000000013FF00000000000003FF0000000000000)", 0},
	{"SELECT ST_GeomFromWKB(0x0101000000000000000000F03F000000000000F03F11)", 3037},
	{"SELECT ST_GeomFromWKB(0x0101000000000000000000F03F)", 3037},
	{"SELECT ST_GeomFromWKB(0x020100000000000000000000000000000000000000)", 3037},
	{"SELECT ST_GeomFromWKB(0x0102000000010000000000000000000000000000000000000000)", 3037},
	{"SELECT ST_GeomFromWKB(0x010200000002000000000000000000000000000000000000000000000000000000000000000000F03F)", 0},
	{"SELECT ST_GeomFromWKB(0x01070000000100000002010000000000000000000000)", 3037},
	{"SELECT ST_GeomFromWKB(0x010700000000000000)", 0},
	{"SELECT ST_GeomFromWKB(0x010400000000000000)", 0},
	{"SELECT ST_GeomFromWKB(0x010300000000000000)", 3037},
	{"SELECT ST_GeomFromWKB(UNHEX('0101000000000000000000F03F000000000000F03F'))", 0},
	{"SELECT ST_GeomFromWKB(UNHEX('0101000000000000000000F03F'))", 3037},
	{"SELECT ST_PointFromWKB(0x010200000002000000000000000000000000000000000000000000000000000000000000000000F03F)", 3516},
	{"SELECT ST_GeomFromWKB(ST_GeomFromText('POINT(1 1)'))", 3037},
	// the constructors
	{"SELECT POINT(1, 1)", 0},
	{"SELECT LINESTRING(POINT(0,0))", 3037},
	{"SELECT LINESTRING(POINT(0,0), POINT(1,1))", 0},
	{"SELECT POLYGON(LINESTRING(POINT(0,0), POINT(1,1), POINT(1,0), POINT(0,0)))", 0},
	{"SELECT POLYGON(LINESTRING(POINT(0,0), POINT(1,1), POINT(1,0)))", 3037},
	{"SELECT POLYGON(LINESTRING(POINT(0,0), POINT(1,1), POINT(1,0), POINT(0,1)))", 3037},
	{"SELECT POLYGON(POINT(1,1))", 1210},
	{"SELECT MULTIPOINT(LINESTRING(POINT(0,0), POINT(1,1)))", 1210},
	{"SELECT MULTIPOINT(POINT(1,1), POINT(2,2))", 0},
	{"SELECT MULTILINESTRING(POINT(1,1))", 1210},
	{"SELECT MULTIPOLYGON(POINT(1,1))", 1210},
	{"SELECT MULTIPOLYGON(POLYGON(LINESTRING(POINT(0,0), POINT(1,1), POINT(1,0), POINT(0,0))))", 0},
	{"SELECT GEOMETRYCOLLECTION(POINT(1,1), LINESTRING(POINT(0,0), POINT(1,1)))", 0},
	{"SELECT GEOMETRYCOLLECTION()", 0},
	{"SELECT LINESTRING(ST_GeomFromText('POINT(0 0)'), ST_GeomFromText('POINT(1 1)'))", 0},
	{"SELECT LINESTRING(ST_GeomFromText('LINESTRING(0 0,1 1)'), POINT(1,1))", 1210},
	// functions that reject a geometry argument
	{"SELECT SIN(POINT(1,1))", 1210},
	{"SELECT POINT(1,1) + 1", 1210},
	{"SELECT POINT(1,1) | 1", 1210},
	{"SELECT -POINT(1,1)", 1210},
	{"SELECT POINT(1,1) = POINT(1,1)", 0},
	{"SELECT POINT(1,1) BETWEEN 1 AND 2", 1210},
	{"SELECT ABS(g) FROM geo", 1210},
	{"SELECT LENGTH(g) FROM geo", 0},
	{"SELECT ST_AsText(POINT(1,1))", 0},
	// stores into a spatial column: the internal format of the column's type
	{"INSERT INTO geo (p, pg) VALUES (POINT(1,1), ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'))", 0},
	{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), LINESTRING(POINT(0,0),POINT(1,1)))", 1416},
	{"INSERT INTO geo (pg, g) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), LINESTRING(POINT(0,0),POINT(1,1)))", 0},
	{"INSERT INTO geo (pg, gc) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), MULTIPOINT(POINT(1,1)))", 0},
	{"INSERT INTO geo (pg, gc) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), POINT(1,1))", 1416},
	{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), ST_GeomFromText('LINESTRING(0 0,1 1)'))", 1416},
	{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), ST_PointFromText('POINT(1 1)'))", 0},
	{"INSERT INTO geo (pg) VALUES (POINT(1,1))", 1416},
	{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), 'x')", 1416},
	{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), 1)", 1416},
	{"INSERT INTO geo (pg, g) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), 0x000000000101000000000000000000F03F000000000000F03F)", 0},
	{"INSERT INTO geo (pg, g) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('000000000101000000000000000000F03F000000000000F03F'))", 0},
	{"INSERT INTO geo (pg, g) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('0000000001010000000000000000F03F'))", 1416},
	{"INSERT INTO geo (pg, g) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('000000000101000000000000000000F03F000000000000F03F00'))", 1416},
	{"INSERT INTO geo (pg, g) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('0000000000000000013FF00000000000003FF0000000000000'))", 1416},
	{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('00000000010200000002000000000000000000000000000000000000000000000000000000000000000000F03F'))", 1416},
	{"INSERT INTO geo (pg, ls) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('00000000010200000002000000000000000000000000000000000000000000000000000000000000000000F03F'))", 0},
	{"INSERT INTO geo (pg, gc) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('000000000104000000010000000101000000000000000000F03F000000000000F03F'))", 0},
	{"INSERT INTO geo (pg, mp) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), UNHEX('00000000010400000000000000'))", 1416},
	{"INSERT IGNORE INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), 'x')", 1416},
	{"UPDATE geo SET p = LINESTRING(POINT(0,0),POINT(1,1)) WHERE id = 1", 1416},
	{"UPDATE geo SET p = POINT(0,0) WHERE id = 1", 0},
	{"INSERT INTO geo (pg, p) SELECT pg, p FROM geo WHERE id = 1", 0},
}

// TestGeometry checks the checker's verdict on each case, without a server, and the
// violations it predicts for the data-decided stores.
func TestGeometry(t *testing.T) {
	s, err := schema.Load(geomSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range geomCases {
		_, err := Analyze(s, c.sql)
		want := c.want
		if want == 3548 {
			want = 0 // the spatial reference system's existence is not read
		}
		if got := errCode(err); got != want {
			t.Errorf("%s: checker says %d (%v), want %d", c.sql, got, err, want)
		}
	}
	// a nullable LINESTRING column into a POINT column: every non-NULL value fails
	r, err := Analyze(s, "INSERT INTO geo (pg, p) SELECT pg, ls FROM geo")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range r.Violations {
		if v.Code == 1416 && v.Key() == "1416" {
			found = true
		}
	}
	if !found {
		t.Errorf("INSERT ... SELECT ls into p: predicted %v, want a 1416 violation", r.Violations)
	}
	// the types the constructors and readers carry
	cases := []struct{ sql, want string }{
		{"SELECT POINT(1,1)", "point"},
		{"SELECT LINESTRING(POINT(0,0),POINT(1,1))", "linestring"},
		{"SELECT ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))')", "polygon"},
		{"SELECT ST_PointFromText(ST_AsText(g)) FROM geo", "point"},
		{"SELECT ST_GeomCollFromText(ST_AsText(g)) FROM geo", "geometry"},
		{"SELECT ST_GeomFromWKB(0x0101000000000000000000F03F000000000000F03F)", "point"},
		{"SELECT ST_Buffer(POINT(1,1), 1)", "geometry"},
		{"SELECT g FROM geo", "geometry"},
		{"SELECT p FROM geo", "point"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil || len(r.Columns) != 1 || !r.Columns[0].Known {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if got := r.Columns[0].Type.Name; got != c.want {
			t.Errorf("%s: typed %s, want %s", c.sql, got, c.want)
		}
	}
}

// TestGeometryServer runs each case on mysqld and requires the server's error number to be
// the checker's (3548, the unknown spatial reference system, is the server's alone). Skipped
// without a mysqld on PATH (nix-shell -p mysql84).
func TestGeometryServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, geomSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO geo (id, pg, p, ls) VALUES (1, ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), POINT(1,1), LINESTRING(POINT(0,0),POINT(1,1)))"); err != nil {
		t.Fatal(err)
	}
	run := func(sql string) int {
		_, err := conn.ExecContext(ctx, sql)
		if err == nil {
			return 0
		}
		var me *driver.MySQLError
		if !errors.As(err, &me) {
			t.Errorf("%s: %v", sql, err)
			return -1
		}
		return int(me.Number)
	}
	for _, c := range geomCases {
		if got := run(c.sql); got != c.want && got >= 0 {
			t.Errorf("%s: the server says %d, the checker %d", c.sql, got, c.want)
		}
	}
	// the data-decided store: every non-NULL LINESTRING fails, a NULL passes
	if got := run("INSERT INTO geo (pg, p) SELECT pg, ls FROM geo WHERE id = 1"); got != 1416 {
		t.Errorf("INSERT ... SELECT ls into p over a row: the server says %d, want 1416", got)
	}
	if got := run("INSERT INTO geo (pg, p) SELECT pg, NULL FROM geo WHERE id = 1"); got != 0 {
		t.Errorf("INSERT ... SELECT NULL into p: the server says %d, want 0", got)
	}
	// the geometry errors do not depend on the sql_mode
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode = ''"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"INSERT INTO geo (pg, p) VALUES (ST_GeomFromText('POLYGON((0 0,1 1,1 0,0 0))'), 'x')", "INSERT INTO geo (pg) VALUES (POINT(1,1))"} {
		if got := run(sql); got != 1416 {
			t.Errorf("%s without strict mode: the server says %d, want 1416", sql, got)
		}
	}
	lenient, err := schema.Load(strings.Replace(geomSchema, "-- sqlshape: mysql 8.4\n", "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = ''\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(lenient, "INSERT INTO geo (pg) VALUES (POINT(1,1))"); errCode(err) != 1416 {
		t.Errorf("the checker without strict mode: %v, want 1416", err)
	}
}
