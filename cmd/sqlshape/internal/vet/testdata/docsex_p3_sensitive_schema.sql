-- sqlshape: postgres 17
-- "Labelled columns are read only where allowed (`sensitive`, `may read`)": orders with
-- pii columns, order_contacts a view that masks phone and passes email through.
-- sqlshape: sensitive pii: email, phone
CREATE TABLE orders (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email text NOT NULL,
    phone text NOT NULL
);

-- sqlshape: context billing: may read pii
CREATE VIEW order_contacts AS
SELECT id, email, left(phone, 3) || '***' AS phone_masked FROM orders;
