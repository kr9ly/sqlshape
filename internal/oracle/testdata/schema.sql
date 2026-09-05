CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');

CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       varchar(100),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id),
    status     order_status NOT NULL DEFAULT 'pending',
    total      numeric(12,2) NOT NULL,
    note       text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE VIEW order_summary AS
SELECT o.id, o.status, o.total, u.email
  FROM orders o
  JOIN users u ON u.id = o.user_id;
