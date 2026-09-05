DELETE FROM orders o USING users u WHERE u.id = o.user_id AND u.email = $1 AND o.created_at < $2 RETURNING o.id, u.email
