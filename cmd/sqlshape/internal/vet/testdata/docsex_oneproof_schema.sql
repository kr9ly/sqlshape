-- sqlshape: postgres 17
-- "Fix a unique key by equality" (the `One` proof).
CREATE TABLE users (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email text UNIQUE NOT NULL,
    name  text
);
