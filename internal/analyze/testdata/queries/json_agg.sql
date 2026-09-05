SELECT user_id, JSON_ARRAYAGG(id ORDER BY id) AS ids, JSON_OBJECTAGG(note: total ABSENT ON NULL RETURNING jsonb) AS by_note, JSON_ARRAYAGG(id) FILTER (WHERE total > $1) AS big
FROM orders GROUP BY user_id
