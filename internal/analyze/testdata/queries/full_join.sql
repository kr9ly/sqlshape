SELECT u.id, o.id AS oid, o.total FROM users u FULL JOIN orders o ON o.user_id = u.id
