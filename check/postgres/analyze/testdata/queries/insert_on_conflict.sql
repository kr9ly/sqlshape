INSERT INTO users (email, name) VALUES ($1, $2) ON CONFLICT (email) DO UPDATE SET name = excluded.name, updated_at = now() RETURNING id, email, name
