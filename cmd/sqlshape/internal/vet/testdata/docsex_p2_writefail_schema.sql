-- sqlshape: postgres 17
-- "Declare the constraints a write can violate": a customers table with the unique email
-- the doc's INSERT can violate.
CREATE TABLE customers (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email text UNIQUE NOT NULL,
    name  text NOT NULL
);
