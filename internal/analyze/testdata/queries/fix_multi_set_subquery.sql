UPDATE orders o SET (note, total) = (SELECT u.name, u.balance FROM users u WHERE u.id = o.user_id) WHERE o.id = $1
