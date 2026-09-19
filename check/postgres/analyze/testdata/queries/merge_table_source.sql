MERGE INTO order_stats_copy c USING order_stats s ON c.user_id = s.user_id
WHEN MATCHED THEN UPDATE SET n = s.n, total = s.total
WHEN NOT MATCHED BY SOURCE THEN DELETE
WHEN NOT MATCHED THEN INSERT VALUES (s.user_id, s.n, s.total)
