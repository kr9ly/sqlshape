package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2"
)

const schema = `-- sqlshape: mysql 8.4
CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  email VARCHAR(255),
  role ENUM('admin', 'member') NOT NULL DEFAULT 'member',
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY users_email_key (email)
);
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  note TEXT,
  CONSTRAINT orders_total_check CHECK (total >= 0),
  CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users (id)
);
`

type Role string

func (r Role) Known() bool { return r == "admin" || r == "member" }

type User struct {
	ID        uint64
	Name      string
	Email     *string
	Role      Role
	CreatedAt time.Time
}

type Order struct {
	ID     uint64
	UserID uint64
	Total  string
	Note   *string
}

var (
	insertUser = sqlshape.Query[struct{}, struct{ Name, Email string }](`INSERT INTO users (name, email) VALUES ({{.Name}}, {{.Email}})`)
	insertRole = sqlshape.Query[struct{}, struct {
		Name string
		Role Role
	}](`INSERT INTO users (name, role) VALUES ({{.Name}}, {{.Role}})`)
	userByEmail = sqlshape.One[User, struct{ Email string }](`SELECT id, name, email, role, created_at FROM users WHERE email = {{.Email}}`)
	users       = sqlshape.Query[User, struct{ Name *string }](`SELECT id, name, email, role, created_at FROM users WHERE TRUE {{if .Name}} AND name = {{.Name}} {{end}} ORDER BY id`)
	names       = sqlshape.Query[string, struct{}](`SELECT name FROM users ORDER BY id`)
	countUsers  = sqlshape.One[int64, struct{}](`SELECT COUNT(*) FROM users`)
	twice       = sqlshape.Query[uint64, struct{ ID uint64 }](`SELECT id FROM users WHERE id = {{.ID}} OR id + 1 = {{.ID}} ORDER BY id`)
	rename      = sqlshape.One[struct{}, struct {
		ID   uint64
		Name string
	}](`UPDATE users SET name = {{.Name}} WHERE id = {{.ID}}`)
	insertOrder = sqlshape.Query[struct{}, struct {
		UserID uint64
		Total  string
	}](`INSERT INTO orders (user_id, total) VALUES ({{.UserID}}, {{.Total}})`)
	insertNoName = sqlshape.Query[struct{}, struct{ Email string }](`INSERT INTO users (name, email) VALUES (NULL, {{.Email}})`)
	badRow       = sqlshape.Query[struct{ Nope string }, struct{}](`SELECT name FROM users`)
)

func start(t *testing.T) (context.Context, *mysqltest.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, schema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

func TestRuntime(t *testing.T) {
	ctx, srv := start(t)
	db := srv.Conn()

	if _, err := mysql.Exec(ctx, db, insertUser, struct{ Name, Email string }{"alice", "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mysql.Exec(ctx, db, insertRole, struct {
		Name string
		Role Role
	}{"bob", "admin"}); err != nil {
		t.Fatalf("named string parameter: %v", err)
	}

	// One: Get / Find, NULL into a pointer, enum into a Labelled type, DATETIME into time.Time
	u, err := mysql.Get(ctx, db, userByEmail, struct{ Email string }{"alice@example.com"})
	if err != nil || u.Name != "alice" || u.Email == nil || *u.Email != "alice@example.com" || u.Role != "member" || u.CreatedAt.IsZero() {
		t.Fatalf("Get: %+v %v", u, err)
	}
	if _, ok, err := mysql.Find(ctx, db, userByEmail, struct{ Email string }{"nobody"}); err != nil || ok {
		t.Fatalf("Find absent: ok=%v err=%v", ok, err)
	}
	if _, err := mysql.Get(ctx, db, userByEmail, struct{ Email string }{"nobody"}); !mysql.IsNoRows(err) {
		t.Fatalf("Get absent: %v", err)
	}

	// Collect with a branch, Run streamed, First, scalar R
	all, err := mysql.Collect(ctx, db, users, struct{ Name *string }{})
	if err != nil || len(all) != 2 || all[1].Email != nil {
		t.Fatalf("Collect: %d %v", len(all), err)
	}
	bob := "bob"
	some, err := mysql.Collect(ctx, db, users, struct{ Name *string }{&bob})
	if err != nil || len(some) != 1 || some[0].Role != "admin" {
		t.Fatalf("Collect branch: %+v %v", some, err)
	}
	n := 0
	for _, err := range mysql.Run(ctx, db, users, struct{ Name *string }{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		break // early exit closes the rows
	}
	if first, err := mysql.First(ctx, db, names, struct{}{}); err != nil || first != "alice" {
		t.Fatalf("First scalar: %q %v", first, err)
	}
	if c, err := mysql.Get(ctx, db, countUsers, struct{}{}); err != nil || c != 2 {
		t.Fatalf("count: %d %v", c, err)
	}

	// the same $n twice is sent twice
	ids, err := mysql.Collect(ctx, db, twice, struct{ ID uint64 }{2})
	if err != nil || len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("twice: %v %v", ids, err)
	}

	// ExecOne: one row, none, and the proof failing
	if _, err := mysql.ExecOne(ctx, db, rename, struct {
		ID   uint64
		Name string
	}{1, "alicia"}); err != nil {
		t.Fatalf("ExecOne: %v", err)
	}
	if _, err := mysql.ExecOne(ctx, db, rename, struct {
		ID   uint64
		Name string
	}{99, "x"}); !mysql.IsNoRows(err) {
		t.Fatalf("ExecOne none: %v", err)
	}

	// constraint violations under the schema's names
	if _, err := mysql.Exec(ctx, db, insertUser, struct{ Name, Email string }{"dup", "alice@example.com"}); !mysql.Violates(err, "users_email_key") {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := mysql.Exec(ctx, db, insertOrder, struct {
		UserID uint64
		Total  string
	}{99, "1.00"}); !mysql.Violates(err, "fk_orders_user") {
		t.Fatalf("foreign key: %v", err)
	}
	if _, err := mysql.Exec(ctx, db, insertOrder, struct {
		UserID uint64
		Total  string
	}{1, "-1.00"}); !mysql.Violates(err, "orders_total_check") {
		t.Fatalf("check: %v", err)
	}
	if _, err := mysql.Exec(ctx, db, insertNoName, struct{ Email string }{"n@example.com"}); !mysql.Violates(err, "name") {
		t.Fatalf("not null: %v", err)
	}

	// a result column without a field is the mapper's error, not the driver's
	if _, err := mysql.Collect(ctx, db, badRow, struct{}{}); err == nil || !strings.Contains(err.Error(), `result column "name" has no field`) {
		t.Fatalf("bad row: %v", err)
	}

	// an unknown enum label is rejected
	if _, err := db.ExecContext(ctx, "ALTER TABLE users MODIFY role ENUM('admin','member','guest') NOT NULL DEFAULT 'member'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE users SET role = 'guest' WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	var ule *sqlshape.UnknownLabelError
	if _, err := mysql.Collect(ctx, db, users, struct{ Name *string }{}); !errors.As(err, &ule) || ule.Value != "guest" {
		t.Fatalf("unknown label: %v", err)
	}
}
