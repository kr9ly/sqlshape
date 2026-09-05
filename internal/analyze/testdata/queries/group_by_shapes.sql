SELECT o.id, o.total, count(*) AS n, lower(o.note) AS ln, o.total * 2 AS t2, u.email, 1 AS one, (SELECT max(id) FROM users) AS mx
  FROM orders o JOIN users u ON u.id = o.user_id
 GROUP BY o.id, lower(o.note), u.id
HAVING sum(o.total) > 0 AND lower(o.note) <> 'x'
 ORDER BY n, o.total, 3
