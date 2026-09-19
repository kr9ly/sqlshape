-- sqlshape: postgres 17
-- "A package references only its schemas (`-schemas`, PostgreSQL)": c_private, outside
-- the -schemas=a_api,b_private set the test applies.
CREATE SCHEMA c_private;
CREATE TABLE c_private.orders (id bigint PRIMARY KEY);
