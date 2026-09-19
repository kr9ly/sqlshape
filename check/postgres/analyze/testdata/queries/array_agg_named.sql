SELECT u.id, array_agg(o.id ORDER BY o.id) AS ids, array_agg(o.total) AS totals, string_agg(o.note, ', ') AS notes FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id
