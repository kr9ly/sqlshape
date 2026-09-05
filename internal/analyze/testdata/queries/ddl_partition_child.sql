SELECT id, ts, kind FROM events_2024 WHERE kind = $1 UNION ALL SELECT id, ts, kind FROM events_default
