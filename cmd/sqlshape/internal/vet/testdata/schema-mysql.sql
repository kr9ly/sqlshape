-- sqlshape: mysql 8.4

CREATE TABLE logs (
  id   INT NOT NULL PRIMARY KEY,
  note VARCHAR(10),
  CONSTRAINT c CHECK (id > 0) NOT ENFORCED
) ENGINE=MyISAM;

CREATE TABLE orders (
  id        BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  total     INT NOT NULL
);

-- sqlshape: require pinned(tenant_id)
CREATE VIEW v_orders AS SELECT id, tenant_id, total FROM orders;
