SELECT user_id, note, count(*) FROM orders GROUP BY GROUPING SETS ((user_id), (status))
