MERGE INTO orders o USING users u ON o.user_id = u.id
WHEN NOT MATCHED THEN INSERT (user_id, nope) VALUES (u.id, 1)
