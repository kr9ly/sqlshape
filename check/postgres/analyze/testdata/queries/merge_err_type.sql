MERGE INTO orders o USING users u ON o.user_id = u.id
WHEN MATCHED THEN UPDATE SET total = u.name
