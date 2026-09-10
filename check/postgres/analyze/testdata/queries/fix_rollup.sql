SELECT u.role, o.status, count(*) FROM orders o JOIN users u ON u.id = o.user_id GROUP BY ROLLUP (u.role, o.status) HAVING count(*) > $1 ORDER BY 1, 2
