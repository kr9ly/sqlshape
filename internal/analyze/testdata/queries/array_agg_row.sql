SELECT u.id, array_agg(row(o.id, o.total)) AS orders, array_agg(o.status) AS statuses
  FROM users u JOIN orders o ON o.user_id = u.id
 GROUP BY u.id
