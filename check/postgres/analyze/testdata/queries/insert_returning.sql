INSERT INTO orders (user_id, status, total, note) VALUES ($1, $2, $3, $4) RETURNING id, created_at
