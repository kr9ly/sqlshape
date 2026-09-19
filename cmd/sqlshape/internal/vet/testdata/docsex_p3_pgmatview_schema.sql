-- sqlshape: postgres 17
-- postgres.md "The runtime: pgx", the MatView example.
CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    total  numeric(12,2) NOT NULL
);

CREATE MATERIALIZED VIEW order_stats AS
SELECT count(*) AS n, sum(total) AS total FROM orders;
