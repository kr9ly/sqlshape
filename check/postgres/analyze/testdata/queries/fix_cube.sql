SELECT user_id, status, max(created_at) FROM orders GROUP BY CUBE (user_id, status)
