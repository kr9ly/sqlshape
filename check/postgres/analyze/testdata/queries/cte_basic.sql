WITH big AS (SELECT id, total FROM orders WHERE total > $1), cnt AS (SELECT count(*) AS n FROM big) SELECT big.id, big.total, cnt.n FROM big, cnt
