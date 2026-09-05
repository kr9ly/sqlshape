SELECT o.id, o.status, o.total, u.email
  FROM orders o
  JOIN users u ON u.id = o.user_id
 WHERE o.status = $1 AND o.user_id = ANY($2)
