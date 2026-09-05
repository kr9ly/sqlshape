MERGE INTO orders o
USING (SELECT $1::bigint AS user_id, $2::numeric AS total, $3::text AS note) s ON o.user_id = s.user_id AND o.note = s.note
WHEN MATCHED AND o.total < s.total THEN UPDATE SET total = s.total, status = 'paid'
WHEN MATCHED THEN DELETE
WHEN NOT MATCHED THEN INSERT (user_id, total, note) VALUES (s.user_id, s.total, s.note)
