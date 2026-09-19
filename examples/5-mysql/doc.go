// Package mysqlexample is the MySQL counterpart of 1-tables: the same declarations
// (sqlshape.Query / One) judged by the MySQL analyzer because schema.sql declares
// `-- sqlshape: mysql 8.4`, and run through the database/sql runtime
// github.com/kr9ly/sqlshape/mysql/v2. The tests boot a real mysqld with mysqltest and skip
// when none is on PATH.
//
// What differs from PostgreSQL is only what MySQL itself does differently: `?`
// placeholders on the wire (the templates still write {{.X}}), DECIMAL received as its text,
// an ENUM column as a string type the application can bind (Status), and the constraint
// names MySQL reports (the UNIQUE key's name, the FOREIGN KEY's CONSTRAINT name). One is proved
// from the schema's keys as on PostgreSQL (the customer's UNIQUE email, the order's primary key).
package mysqlexample
