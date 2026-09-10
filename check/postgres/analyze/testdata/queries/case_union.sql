SELECT CASE WHEN total > 100 THEN 'big' ELSE 'small' END AS size, total::int AS t FROM orders
UNION ALL
SELECT 'none', 0
