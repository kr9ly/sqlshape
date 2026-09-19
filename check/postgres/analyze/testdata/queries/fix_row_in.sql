SELECT o.id FROM orders o WHERE (o.user_id, o.note) IN (SELECT u.id, u.name FROM users u) AND (o.id, o.total) NOT IN (SELECT $1::bigint, $2::numeric)
