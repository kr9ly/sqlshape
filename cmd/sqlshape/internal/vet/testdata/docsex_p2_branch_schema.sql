-- sqlshape: postgres 17
-- "Every branch must be provable": same users shape as "Fix a unique key by equality"
-- (docsex_oneproof_schema.sql) -- duplicated here rather than shared, per this
-- milestone's file-ownership rule.
CREATE TABLE users (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email text UNIQUE NOT NULL,
    name  text
);
