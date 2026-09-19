-- sqlshape: testdb 1
-- a stub dialect (dialect_test.go): the loader ignores the DDL and answers from a fixed table
CREATE TABLE things (id, name, price, note);
