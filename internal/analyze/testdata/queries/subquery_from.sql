SELECT s.uid, s.n, s.total FROM (SELECT user_id AS uid, count(*) AS n, sum(total) AS total FROM orders GROUP BY user_id) s WHERE s.n > $1
