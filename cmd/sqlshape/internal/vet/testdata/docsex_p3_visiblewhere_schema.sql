-- sqlshape: postgres 17
-- "Every read carries the visibility predicate (`visible where`)": memos, completed with
-- the columns the example's SELECT and its predicate need (the doc's fence has only
-- `CREATE TABLE memos (...)`).
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL,
    body       text NOT NULL,
    deleted_at timestamptz
);
