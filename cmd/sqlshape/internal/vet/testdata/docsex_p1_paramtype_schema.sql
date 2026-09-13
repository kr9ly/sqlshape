-- sqlshape: postgres 17
-- "A parameter's type follows where it is used".
CREATE TABLE users (
    id    uuid PRIMARY KEY,
    email text NOT NULL
);
