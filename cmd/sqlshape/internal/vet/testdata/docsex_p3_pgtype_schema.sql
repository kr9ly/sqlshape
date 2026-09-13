-- sqlshape: postgres 17
-- postgres.md "The Go type table": a domain over numeric, bound to a Go type via
-- `// sqlshape: type money_amount` on the Go type (below, in the test package).
CREATE DOMAIN money_amount AS numeric(12,2);

CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    amount money_amount NOT NULL
);
