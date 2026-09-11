-- sqlshape: postgres 17
-- Adversarial-testing lane "typefit". Own schema so it does not depend on schema.sql
-- other lanes may also be touching.
CREATE TYPE item AS (
    sku text,
    qty integer
);

CREATE TABLE t (
    id    bigint PRIMARY KEY,
    tags  integer[],
    items item[],
    span  interval
);
