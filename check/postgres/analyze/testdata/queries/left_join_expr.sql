SELECT u.id, u.name, count(o.id) AS order_count, coalesce(sum(o.total), 0) AS total, max(o.created_at)
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id
 GROUP BY u.id, u.name
 ORDER BY u.id
 LIMIT $1
