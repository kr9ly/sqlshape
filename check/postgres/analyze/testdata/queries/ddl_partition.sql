INSERT INTO events (ts, kind) VALUES ($1, $2) RETURNING id, ts, kind
