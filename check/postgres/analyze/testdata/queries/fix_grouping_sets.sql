SELECT user_id, status, count(*) AS n, sum(total) AS t, GROUPING(user_id, status) AS g
FROM orders GROUP BY GROUPING SETS ((user_id), (status), ())
