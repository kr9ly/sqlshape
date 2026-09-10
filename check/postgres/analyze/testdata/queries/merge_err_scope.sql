MERGE INTO orders o USING users u ON o.user_id = u.id
WHEN NOT MATCHED THEN INSERT (user_id, total) VALUES (u.id, o.total)
