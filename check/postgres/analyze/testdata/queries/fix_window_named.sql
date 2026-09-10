SELECT id, sum(total) OVER w AS running, rank() OVER w AS rk, dense_rank() OVER (ORDER BY total) AS dr
FROM orders WINDOW w AS (PARTITION BY user_id ORDER BY total)
