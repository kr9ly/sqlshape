MERGE INTO orders o USING users u ON o.user_id = u.id
WHEN MATCHED THEN UPDATE SET note = u.name
WHEN NOT MATCHED THEN DO NOTHING
RETURNING merge_action() AS act, o.id, u.email
