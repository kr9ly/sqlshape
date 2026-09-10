-- sqlshape: mysql 8.4
CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  email VARCHAR(255),
  active TINYINT(1) NOT NULL DEFAULT 1,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  note TEXT,
  CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users (id)
);

-- sqlshape: require pinned(tenant_id)
CREATE TABLE tenant_notes (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  body TEXT NOT NULL,
  UNIQUE KEY tenant_notes_tenant_body (tenant_id, body(64))
);

-- sqlshape: waive tenant_notes pinned(tenant_id)
CREATE VIEW all_notes AS SELECT id, tenant_id, body FROM tenant_notes;

CREATE TABLE tickets (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  status ENUM('open', 'closed') NOT NULL,
  CONSTRAINT fk_tickets_user FOREIGN KEY (user_id) REFERENCES users (id)
);
