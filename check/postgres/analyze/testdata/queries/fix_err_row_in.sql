SELECT o.id FROM orders o WHERE (o.user_id, o.note) IN (SELECT u.id FROM users u)
