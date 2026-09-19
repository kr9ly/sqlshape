CREATE TEMP TABLE hot AS SELECT id, total FROM orders WHERE total > $1
