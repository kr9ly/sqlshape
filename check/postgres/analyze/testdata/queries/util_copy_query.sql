COPY (SELECT id, total FROM orders WHERE total > 100) TO STDOUT WITH (FORMAT csv)
