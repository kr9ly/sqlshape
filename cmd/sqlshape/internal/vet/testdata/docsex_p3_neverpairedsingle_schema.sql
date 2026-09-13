-- sqlshape: postgres 17
-- "Append-only tables, paired writes, single-row deletes (`never`, `paired`, `single`)":
-- ledger (never), orders (paired(outbox), single), outbox as the pairing target.
-- sqlshape: require never on update, delete
CREATE TABLE ledger (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    amount numeric(12,2) NOT NULL
);

-- sqlshape: require paired(outbox) on insert
-- sqlshape: require single on delete
CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status text NOT NULL
);

CREATE TABLE outbox (
    id      bigint PRIMARY KEY,
    payload text NOT NULL
);
