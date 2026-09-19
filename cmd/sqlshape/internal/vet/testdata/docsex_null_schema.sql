-- sqlshape: postgres 17
-- "A column that may be NULL needs a field that can hold NULL".
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deleted_at timestamptz
);
