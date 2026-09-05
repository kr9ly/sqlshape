SELECT o.user_id, count(*), o.note FROM orders o GROUP BY o.user_id HAVING sum(o.total) > 10 ORDER BY o.total
