SELECT id, row_number() OVER (PARTITION BY user_id ORDER BY created_at) AS rn, sum(total) OVER (PARTITION BY user_id) AS running, count(*) FILTER (WHERE status = 'paid') OVER () AS paid FROM orders
